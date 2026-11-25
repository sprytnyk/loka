// Package transfer holds what Loka sends, receives and shares: the state of
// each transfer, the items chosen, and the streams that carry them.
package transfer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Status is a step in a transfer's life.
type Status string

const (
	Pairing   Status = "pairing"   // between Lokas, until both know the pairing code
	Waiting   Status = "waiting"   // offered; the receiver has not answered
	Accepted  Status = "accepted"  // the receiver chose a destination
	Sending   Status = "sending"   // bytes are leaving this device
	Receiving Status = "receiving" // bytes are arriving on this device
	Complete  Status = "complete"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
	Shared    Status = "shared"  // a download link is active
	Stopped   Status = "stopped" // a download link was stopped
)

// Kind says what a transfer carries.
type Kind string

const (
	Files Kind = ""     // files and directories, as a tar stream
	Text  Kind = "text" // text to copy, which never touches the disk
)

// MaxText is the most bytes of text one transfer or link carries.
const MaxText = 1 << 20

// Direction says whether a transfer leaves or arrives on this device.
type Direction string

const (
	Incoming Direction = "incoming"
	Outgoing Direction = "outgoing"
)

// Transfer is one send, one receive or one download link.
//
// The fields from ID to shared are set before the transfer is added to a
// Registry and never change after, so any goroutine may read them. Done
// is written only by the goroutine that moves the bytes. Status, error,
// destination and text change only through the methods below, under the
// transfer's own lock.
type Transfer struct {
	ID        string
	Direction Direction
	Kind      Kind
	Peer      string   // the other device's name or address, or "Link"
	Items     []string // the top-level names, for display
	Size      int64    // bytes of file content

	Secret string // proves a remote sender owns an incoming transfer
	// Who sent an incoming transfer: a browser, which shows no certificate,
	// or a Loka with this ID and certificate fingerprint.
	Browser     bool
	DeviceID    string
	Fingerprint string
	// For the pairing code of an incoming transfer from a Loka: what the
	// sender committed to, and this side's nonce.
	Commitment string
	Nonce      string
	Token      string       // the download link token, only for links
	shared     []sharedItem // what a link serves, only for links

	Done atomic.Int64 // bytes of file content moved so far

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	status      Status
	err         string
	newDevice   bool   // the other Loka has not been remembered yet
	code        string // the pairing code, once both nonces are known
	destination string
	// The text of a text transfer: to send, to serve as a link, or
	// received and not yet read. It is dropped once it is no longer needed.
	text    string
	unread  bool
	speed   speedometer
	started time.Time // when bytes started moving
	ended   time.Time // when the transfer completed
}

// New makes a transfer that starts at status.
func New(direction Direction, peer string, items []string, size int64, status Status) *Transfer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Transfer{
		ID:        RandomToken(16),
		Direction: direction,
		Peer:      peer,
		Items:     items,
		Size:      size,
		ctx:       ctx,
		cancel:    cancel,
		status:    status,
	}
}

// NewText makes a transfer of text that starts at status.
func NewText(direction Direction, peer, text string, status Status) *Transfer {
	t := New(direction, peer, nil, int64(len(text)), status)
	t.Kind, t.text = Text, text
	return t
}

// NewTextLink makes a link that shows text.
func NewTextLink(text string) *Transfer {
	t := NewText(Outgoing, "Link", text, Shared)
	t.Token = RandomToken(24)
	return t
}

// Text returns the text to send or serve, or the text received and not
// yet read.
func (t *Transfer) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text
}

// ReceiveText keeps the text until it is read and reports whether it
// completed the transfer before cancellation.
func (t *Transfer) ReceiveText(text string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != Receiving {
		return false
	}
	if t.ctx.Err() != nil {
		t.status = Cancelled
		return false
	}
	t.text, t.unread, t.status = text, true, Complete
	return true
}

// Unread reports whether received text waits to be read.
func (t *Transfer) Unread() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unread
}

// Dismiss drops received text once it was read.
func (t *Transfer) Dismiss() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.text, t.unread = "", false
}

// dropSentText drops text this device no longer needs to send or serve,
// once the transfer has ended. Callers hold the lock.
func (t *Transfer) dropSentText() {
	if t.Direction == Outgoing && t.status.Final() {
		t.text = ""
	}
}

// Context ends when the transfer is stopped.
func (t *Transfer) Context() context.Context { return t.ctx }

// Status returns the current step.
func (t *Transfer) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// Destination returns the directory an accepted incoming transfer goes to.
func (t *Transfer) Destination() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.destination
}

// Move changes the status to `to` only if it is one of `from`, and reports
// whether it did. Every actor goes through here, so two of them can never
// both win the same step.
func (t *Transfer) Move(to Status, from ...Status) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !slices.Contains(from, t.status) {
		return false
	}
	t.status = to
	if to == Sending || to == Receiving {
		t.started = time.Now()
		t.speed = speedometer{} // start measuring as the bytes start
		t.speed.read(t.Done.Load(), t.started)
	}
	return true
}

// Accept records the destination of a waiting incoming transfer.
func (t *Transfer) Accept(destination string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != Waiting {
		return false
	}
	t.status = Accepted
	t.destination = destination
	return true
}

// Stop cancels the transfer. A transfer that is not moving bytes stops at
// once; one that is notices the cancelled context and calls Finish.
func (t *Transfer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancel()
	switch t.status {
	case Pairing, Waiting, Accepted:
		t.status = Cancelled
	case Shared:
		t.status = Stopped
	}
	t.dropSentText()
}

// Finish records how a transfer that was moving bytes ended. It does nothing
// when the transfer has already ended.
func (t *Transfer) Finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status.Final() {
		return
	}
	switch {
	case err == nil:
		t.status = Complete
		t.ended = time.Now()
	case t.ctx.Err() != nil || errors.Is(err, context.Canceled):
		t.status = Cancelled
	default:
		t.status = Failed
		t.err = err.Error()
	}
	t.dropSentText()
}

// Expire fails the transfer with err if it is still at status.
func (t *Transfer) Expire(status Status, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != status {
		return
	}
	t.cancel()
	t.status, t.err = Failed, err.Error()
	t.dropSentText()
}

// Final reports whether a transfer at this status has ended.
func (s Status) Final() bool {
	switch s {
	case Complete, Failed, Cancelled, Stopped:
		return true
	}
	return false
}

// MarkNewDevice notes that the other Loka has not been remembered yet. A
// sender learns it while the transfer is already shown.
func (t *Transfer) MarkNewDevice() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.newDevice = true
}

// NewDevice reports whether the other Loka had not been remembered.
func (t *Transfer) NewDevice() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.newDevice
}

// Pair records the pairing code of a transfer between Lokas and moves it
// on to waiting for the receiver. It reports false when the transfer was
// no longer pairing.
func (t *Transfer) Pair(code string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != Pairing {
		return false
	}
	t.code, t.status = code, Waiting
	return true
}

// View is what the page sees of a transfer.
type View struct {
	ID        string    `json:"id"`
	Direction Direction `json:"direction"`
	Kind      Kind      `json:"kind,omitempty"`
	Name      string    `json:"name"`
	Items     []string  `json:"items"`
	Size      int64     `json:"size"`
	Status    Status    `json:"status"`
	Progress  int       `json:"progress"`
	Error     string    `json:"error,omitempty"`
	Token     string    `json:"token,omitempty"`
	SpeedMbps *float64  `json:"speed_mbps,omitempty"` // while bytes move, then the average once complete
	Browser   bool      `json:"browser,omitempty"`
	NewDevice bool      `json:"new_device,omitempty"`
	Code      string    `json:"code,omitempty"`    // the pairing code, between Lokas
	Unread    bool      `json:"unread,omitempty"`  // received text waits to be read
	Preview   string    `json:"preview,omitempty"` // the start of text this device sends or serves
}

// previewSize is how many bytes of outgoing text a view shows.
const previewSize = 120

// View returns what the page shows now. While bytes move, it includes the
// current speed, which reading the view measures. Once the transfer
// completes, it includes the average speed. Text has neither.
func (t *Transfer) View() View {
	t.mu.Lock()
	status, err, newDevice, code, unread := t.status, t.err, t.newDevice, t.code, t.unread
	var preview string
	if t.Direction == Outgoing {
		preview = cutToBytes(t.text, previewSize)
	}
	var speed *float64
	switch {
	case t.Kind == Text: // too small for a speed to mean anything
	case status == Sending || status == Receiving:
		mbps := t.speed.read(t.Done.Load(), time.Now())
		speed = &mbps
	case status == Complete && !t.started.IsZero() && t.ended.After(t.started):
		mbps := float64(t.Done.Load()) * 8 / t.ended.Sub(t.started).Seconds() / 1e6
		speed = &mbps
	}
	t.mu.Unlock()
	return View{
		ID:        t.ID,
		Direction: t.Direction,
		Kind:      t.Kind,
		Name:      t.Peer,
		Items:     t.Items,
		Size:      t.Size,
		Status:    status,
		Progress:  progress(t.Done.Load(), t.Size, status),
		Error:     err,
		Token:     t.Token,
		SpeedMbps: speed,
		Browser:   t.Browser,
		NewDevice: newDevice,
		Code:      code,
		Unread:    unread,
		Preview:   preview,
	}
}

// speedEvery is the shortest time a speed is measured over.
const speedEvery = 500 * time.Millisecond

// speedometer measures the speed between looks at the byte count, so the
// copy loops only add to a counter. Each measurement covers at least
// speedEvery, and one that saw no new bytes reads zero.
type speedometer struct {
	bytes int64
	at    time.Time
	mbps  float64
}

func (s *speedometer) read(bytes int64, now time.Time) float64 {
	if s.at.IsZero() {
		s.bytes, s.at = bytes, now
		return 0
	}
	if elapsed := now.Sub(s.at); elapsed >= speedEvery {
		s.mbps = float64(bytes-s.bytes) * 8 / elapsed.Seconds() / 1e6
		s.bytes, s.at = bytes, now
	}
	return s.mbps
}

func progress(done, size int64, status Status) int {
	switch {
	case status == Complete || status == Shared:
		return 100
	case size <= 0:
		return 0
	}
	return int(min(done, size) * 100 / size)
}

// MaxEnded is how many ended transfers a Registry keeps. Older ones are
// dropped, so offers that keep arriving can't fill the memory.
const MaxEnded = 100

// Registry holds the transfers still under way and the latest that ended.
// Its lock guards only the lookup tables, never a transfer's own state.
type Registry struct {
	mu      sync.Mutex
	ordered []*Transfer
	byID    map[string]*Transfer
	byToken map[string]*Transfer
}

// Add records a new transfer.
func (r *Registry) Add(t *Transfer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = map[string]*Transfer{}
		r.byToken = map[string]*Transfer{}
	}
	r.ordered = append(r.ordered, t)
	r.byID[t.ID] = t
	if t.Token != "" {
		r.byToken[t.Token] = t
	}
	r.dropEnded()
}

// dropEnded drops the oldest ended transfers beyond MaxEnded.
func (r *Registry) dropEnded() {
	ended := 0
	for _, t := range r.ordered {
		if droppable(t) {
			ended++
		}
	}
	if ended <= MaxEnded {
		return
	}
	r.ordered = slices.DeleteFunc(r.ordered, func(t *Transfer) bool {
		if ended <= MaxEnded || !droppable(t) {
			return false
		}
		ended--
		delete(r.byID, t.ID)
		if t.Token != "" {
			delete(r.byToken, t.Token)
		}
		return true
	})
}

// droppable reports whether a transfer has ended and holds no text
// waiting to be read.
func droppable(t *Transfer) bool {
	return t.Status().Final() && !t.Unread()
}

// Get returns the transfer with this ID, or nil.
func (r *Registry) Get(id string) *Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

// Link returns the download link with this token, or nil.
func (r *Registry) Link(token string) *Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byToken[token]
}

// All returns every transfer, oldest first.
func (r *Registry) All() []*Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ordered)
}

// RandomToken returns n random bytes as URL-safe text.
func RandomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

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
// is written only by the goroutine that moves the bytes. Status, error and
// destination change only through the methods below, under the transfer's
// own lock.
type Transfer struct {
	ID        string
	Direction Direction
	Peer      string   // the other device's name or address, or "Link"
	Items     []string // the top-level names, for display
	Size      int64    // bytes of file content

	Secret string       // proves a remote sender owns an incoming transfer
	Token  string       // the download link token, only for links
	shared []sharedItem // what a link serves, only for links

	Done atomic.Int64 // bytes of file content moved so far

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	status      Status
	err         string
	destination string
	speed       speedometer
}

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
		t.speed = speedometer{} // start measuring as the bytes start
		t.speed.read(t.Done.Load(), time.Now())
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
// once; one that is notices the cancelled context and calls finish.
func (t *Transfer) Stop() {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.status {
	case Waiting, Accepted:
		t.status = Cancelled
	case Shared:
		t.status = Stopped
	}
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
	case t.ctx.Err() != nil || errors.Is(err, context.Canceled):
		t.status = Cancelled
	default:
		t.status = Failed
		t.err = err.Error()
	}
}

func (s Status) Final() bool {
	switch s {
	case Complete, Failed, Cancelled, Stopped:
		return true
	}
	return false
}

// View is what the page sees of a transfer.
type View struct {
	ID        string    `json:"id"`
	Direction Direction `json:"direction"`
	Name      string    `json:"name"`
	Items     []string  `json:"items"`
	Size      int64     `json:"size"`
	Status    Status    `json:"status"`
	Progress  int       `json:"progress"`
	Error     string    `json:"error,omitempty"`
	Token     string    `json:"token,omitempty"`
	SpeedMbps *float64  `json:"speed_mbps,omitempty"` // only while bytes move
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

func (t *Transfer) View() View {
	t.mu.Lock()
	status, err := t.status, t.err
	var speed *float64
	if status == Sending || status == Receiving {
		mbps := t.speed.read(t.Done.Load(), time.Now())
		speed = &mbps
	}
	t.mu.Unlock()
	return View{
		ID:        t.ID,
		Direction: t.Direction,
		Name:      t.Peer,
		Items:     t.Items,
		Size:      t.Size,
		Status:    status,
		Progress:  progress(t.Done.Load(), t.Size, status),
		Error:     err,
		Token:     t.Token,
		SpeedMbps: speed,
	}
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

// Registry holds every transfer made since Loka started. Its lock guards
// only the lookup tables, never a transfer's own state.
type Registry struct {
	mu      sync.Mutex
	ordered []*Transfer
	byID    map[string]*Transfer
	byToken map[string]*Transfer
}

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
}

func (r *Registry) Get(id string) *Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *Registry) Link(token string) *Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byToken[token]
}

func (r *Registry) All() []*Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Transfer(nil), r.ordered...)
}

// RandomToken returns n random bytes as URL-safe text.
func RandomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
)

// Status is a step in a transfer's life.
type Status string

const (
	statusWaiting   Status = "waiting"   // offered; the receiver has not answered
	statusAccepted  Status = "accepted"  // the receiver chose a destination
	statusSending   Status = "sending"   // bytes are leaving this device
	statusReceiving Status = "receiving" // bytes are arriving on this device
	statusComplete  Status = "complete"
	statusFailed    Status = "failed"
	statusCancelled Status = "cancelled"
	statusShared    Status = "shared"  // a download link is active
	statusStopped   Status = "stopped" // a download link was stopped
)

// Direction says whether a transfer leaves or arrives on this device.
type Direction string

const (
	incoming Direction = "incoming"
	outgoing Direction = "outgoing"
)

// Transfer is one send, one receive or one download link.
//
// The exported fields never change after newTransfer returns, so any
// goroutine may read them. Progress is written only by the goroutine that
// moves the bytes. Status, error and destination change only through the
// methods below, under the transfer's own lock.
type Transfer struct {
	ID        string
	Direction Direction
	Peer      string   // the other device's name or address, or "Link"
	Items     []string // the top-level names, for display
	Size      int64    // bytes of file content

	secret string       // proves a remote sender owns an incoming transfer
	token  string       // the download link token, only for links
	shared []sharedItem // what a link serves, only for links

	ctx    context.Context
	cancel context.CancelFunc
	done   atomic.Int64 // bytes of file content moved so far

	mu          sync.Mutex
	status      Status
	err         string
	destination string
}

func newTransfer(direction Direction, peer string, items []string, size int64, status Status) *Transfer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Transfer{
		ID:        randomToken(16),
		Direction: direction,
		Peer:      peer,
		Items:     items,
		Size:      size,
		ctx:       ctx,
		cancel:    cancel,
		status:    status,
	}
}

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

// move changes the status to `to` only if it is one of `from`, and reports
// whether it did. Every actor goes through here, so two of them can never
// both win the same step.
func (t *Transfer) move(to Status, from ...Status) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !slices.Contains(from, t.status) {
		return false
	}
	t.status = to
	return true
}

// accept records the destination of a waiting incoming transfer.
func (t *Transfer) accept(destination string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != statusWaiting {
		return false
	}
	t.status = statusAccepted
	t.destination = destination
	return true
}

// stop cancels the transfer. A transfer that is not moving bytes stops at
// once; one that is notices the cancelled context and calls finish.
func (t *Transfer) stop() {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.status {
	case statusWaiting, statusAccepted:
		t.status = statusCancelled
	case statusShared:
		t.status = statusStopped
	}
}

// finish records how a transfer that was moving bytes ended. It does nothing
// when the transfer has already ended.
func (t *Transfer) finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status.final() {
		return
	}
	switch {
	case err == nil:
		t.status = statusComplete
	case t.ctx.Err() != nil || errors.Is(err, context.Canceled):
		t.status = statusCancelled
	default:
		t.status = statusFailed
		t.err = err.Error()
	}
}

func (s Status) final() bool {
	switch s {
	case statusComplete, statusFailed, statusCancelled, statusStopped:
		return true
	}
	return false
}

// transferView is what the page sees of a transfer.
type transferView struct {
	ID        string    `json:"id"`
	Direction Direction `json:"direction"`
	Name      string    `json:"name"`
	Items     []string  `json:"items"`
	Size      int64     `json:"size"`
	Status    Status    `json:"status"`
	Progress  int       `json:"progress"`
	Error     string    `json:"error,omitempty"`
	Token     string    `json:"token,omitempty"`
}

func (t *Transfer) view() transferView {
	t.mu.Lock()
	status, err := t.status, t.err
	t.mu.Unlock()
	return transferView{
		ID:        t.ID,
		Direction: t.Direction,
		Name:      t.Peer,
		Items:     t.Items,
		Size:      t.Size,
		Status:    status,
		Progress:  progress(t.done.Load(), t.Size, status),
		Error:     err,
		Token:     t.token,
	}
}

func progress(done, size int64, status Status) int {
	switch {
	case status == statusComplete || status == statusShared:
		return 100
	case size <= 0:
		return 0
	}
	return int(min(done, size) * 100 / size)
}

// registry holds every transfer made since Loka started. Its lock guards
// only the lookup tables, never a transfer's own state.
type registry struct {
	mu      sync.Mutex
	ordered []*Transfer
	byID    map[string]*Transfer
	byToken map[string]*Transfer
}

func (r *registry) add(t *Transfer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = map[string]*Transfer{}
		r.byToken = map[string]*Transfer{}
	}
	r.ordered = append(r.ordered, t)
	r.byID[t.ID] = t
	if t.token != "" {
		r.byToken[t.token] = t
	}
}

func (r *registry) get(id string) *Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *registry) link(token string) *Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byToken[token]
}

func (r *registry) all() []*Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Transfer(nil), r.ordered...)
}

// randomToken returns n random bytes as URL-safe text.
func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

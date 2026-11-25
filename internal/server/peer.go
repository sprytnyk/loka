package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

// pollEvery is how often a sender asks whether the receiver has answered.
var pollEvery = time.Second

// SendTo offers entries to the device at host and port, waits for it to
// accept, then streams them, all over TLS. It returns why the transfer
// ended, if not complete. The receiver is remembered once it accepts.
// While it waits, t shows the pairing code, which the receiver shows too.
//
// id is the device picked from the list of devices, or empty for an
// address typed in. Another device answering there is refused. A receiver
// never remembered before marks the transfer as to a new device.
func SendTo(t *transfer.Transfer, entries []transfer.Entry, store *trust.Store, from, host string, port int, id string) error {
	base := "https://" + net.JoinHostPort(host, strconv.Itoa(port))
	receiver := &pinned{store: store, want: id}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = store.ClientConfig(receiver.check)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	var reply offerReply
	nonce := trust.Nonce()
	o := offer{Name: from, Kind: t.Kind, Items: t.Items[:min(len(t.Items), transfer.MaxOfferItems)], Size: t.Size, Commitment: trust.Commit(nonce)}
	if err := call(t.Context(), client, http.MethodPost, base+"/peer/offer", "", o, &reply); err != nil {
		if t.Kind == transfer.Text && err.Error() == errInvalidOffer.Error() {
			return errOldReceiver
		}
		return err
	}
	newReceiver := receiver.isNew()
	if newReceiver {
		t.MarkNewDevice()
	}
	device := receiver.device(cmp.Or(truncate(reply.Name, 255), host))
	tellReceiver := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		call(ctx, client, http.MethodPost, base+"/peer/cancel/"+reply.ID, reply.Secret, nil, nil)
	})
	// Tell the receiver when this side cancels, before dropping the stream,
	// so it shows the transfer as cancelled rather than failed.
	uploadCtx, stopUpload := context.WithCancel(context.Background())
	defer stopUpload()
	stop := context.AfterFunc(t.Context(), func() {
		tellReceiver()
		stopUpload()
	})
	defer stop()

	if !t.Pair(trust.Code(store.Fingerprint(), device.Fingerprint, nonce, reply.Nonce)) {
		return context.Canceled
	}
	reveal := revealRequest{Nonce: nonce, ReceiverKnown: t.Kind == transfer.Text && !newReceiver}
	if err := call(t.Context(), client, http.MethodPost, base+"/peer/reveal/"+reply.ID, reply.Secret, reveal, nil); err != nil {
		tellReceiver()
		return err
	}
	if err := waitForAnswer(t.Context(), client, base+"/peer/status/"+reply.ID, reply.Secret); err != nil {
		// Otherwise the receiver would show the offer as waiting forever.
		tellReceiver()
		return err
	}
	// Remembering is a convenience: a failure only means the receiver
	// counts as new next time.
	store.Remember(device)
	if !t.Move(transfer.Sending, transfer.Waiting) {
		return context.Canceled
	}
	// Nothing tells the sender when the receiver went away without a
	// word, such as when its lid closed, so a transfer that stops moving
	// counts as interrupted.
	var stalled atomic.Bool
	stopWatch := transfer.WatchStall(&t.Done, func() {
		stalled.Store(true)
		stopUpload()
	})
	defer stopWatch()
	write := func(ctx context.Context, w io.Writer) error {
		if t.Kind != transfer.Text {
			return transfer.WriteTar(ctx, w, entries, &t.Done)
		}
		n, err := io.WriteString(w, t.Text())
		t.Done.Add(int64(n))
		return err
	}
	err := upload(uploadCtx, client, base+"/peer/upload/"+reply.ID, reply.Secret, write)
	if err != nil && stalled.Load() {
		return transfer.ErrInterrupted
	}
	return err
}

// errOldReceiver is what a Loka from before text answers to text.
var errOldReceiver = errors.New("The receiver's Loka can't receive text. Update it there")

// pinned is the receiver as its first connection showed it. Every later
// connection of the same transfer must show the same certificate.
type pinned struct {
	store *trust.Store
	want  string // the device picked, if any

	mu              sync.Mutex
	id, fingerprint string
	standing        trust.Standing
}

// errNotPicked is another device answering at the address of the one
// picked: one posing as it, or one that took its address.
var errNotPicked = errors.New("Another device answered at this address, not the one you picked. It may be posing as it")

func (p *pinned) check(id, fingerprint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fingerprint != "" {
		if id != p.id || fingerprint != p.fingerprint {
			return errors.New("The other device's certificate changed during the transfer")
		}
		return nil
	}
	if p.want != "" && id != p.want {
		return errNotPicked
	}
	standing, known, err := p.store.Check(id, fingerprint)
	if err != nil {
		return err
	}
	if standing == trust.Changed {
		return &trust.ChangedError{Device: known}
	}
	p.id, p.fingerprint, p.standing = id, fingerprint, standing
	return nil
}

// isNew reports whether the receiver had not been remembered before.
func (p *pinned) isNew() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fingerprint != "" && p.standing == trust.New
}

func (p *pinned) device(name string) trust.Device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return trust.Device{ID: p.id, Name: name, Fingerprint: p.fingerprint}
}

// waitForAnswer polls the receiver until it accepts or turns the transfer
// down.
func waitForAnswer(ctx context.Context, client *http.Client, url, secret string) error {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		var reply statusReply
		if err := call(ctx, client, http.MethodGet, url, secret, nil, &reply); err != nil {
			return err
		}
		switch {
		case reply.Status == transfer.Accepted:
			return nil
		case reply.Status == transfer.Waiting:
		case reply.Error != "":
			return errors.New(truncate(reply.Error, 255))
		default:
			return errDeclined
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// upload streams to the receiver what write writes.
func upload(ctx context.Context, client *http.Client, url, secret string, write func(context.Context, io.Writer) error) error {
	pr, pw := io.Pipe()
	defer pr.Close()
	writeResult := make(chan error, 1)
	go func() {
		err := write(ctx, pw)
		writeResult <- err
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Transfer-Secret", secret)
	resp, err := client.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		if writeErr := <-writeResult; writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
			return writeErr // a local problem explains the failure better
		}
		// The receiver was reached for the offer, so a broken connection
		// now means it dropped, unless it can't be reached again.
		if transfer.Interrupted(err) && ctx.Err() == nil {
			return transfer.ErrInterrupted
		}
		return connectionError(ctx, err)
	}
	defer resp.Body.Close()
	if err := replyError(resp); err != nil {
		return err
	}
	return <-writeResult
}

// call makes a small JSON request to another device.
func call(ctx context.Context, client *http.Client, method, url, secret string, body, reply any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Transfer-Secret", secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return connectionError(ctx, err)
	}
	defer resp.Body.Close()
	if err := replyError(resp); err != nil {
		return err
	}
	if reply == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(reply)
}

// replyError turns an error reply from another device into an error.
func replyError(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	var reply struct {
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply)
	if reply.Error == "" {
		return fmt.Errorf("The other device answered %s", resp.Status)
	}
	return errors.New(reply.Error)
}

// connectionError explains a failed request without the request details.
func connectionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if changed, ok := errors.AsType[*trust.ChangedError](err); ok {
		return changed
	}
	if errors.Is(err, errNotPicked) {
		return errNotPicked
	}
	// An answer that isn't TLS, so not Loka. Go's client
	// reports a plain HTTP answer only in words.
	_, notTLS := errors.AsType[tls.RecordHeaderError](err)
	if notTLS || strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
		return errors.New("Could not connect securely to the other device. Check that it runs Loka")
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}
	return fmt.Errorf("Could not reach the other device: %w", err)
}

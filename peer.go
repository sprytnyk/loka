package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// pollEvery is how often a sender asks whether the receiver has answered.
var pollEvery = time.Second

// sendTo offers entries to the device at host and port, waits for it to
// accept, then streams them. It returns why the transfer ended, if not
// complete.
func sendTo(t *Transfer, entries []entry, from, host string, port int) error {
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	var reply offerReply
	o := offer{Name: from, Items: t.Items[:min(len(t.Items), maxOfferItems)], Size: t.Size}
	if err := call(t.ctx, http.MethodPost, base+"/peer/offer", "", o, &reply); err != nil {
		return err
	}
	tellReceiver := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		call(ctx, http.MethodPost, base+"/peer/cancel/"+reply.ID, reply.Secret, nil, nil)
	})
	// Tell the receiver when this side cancels, before dropping the stream,
	// so it shows the transfer as cancelled rather than failed.
	uploadCtx, stopUpload := context.WithCancel(context.Background())
	defer stopUpload()
	stop := context.AfterFunc(t.ctx, func() {
		tellReceiver()
		stopUpload()
	})
	defer stop()

	if err := waitForAnswer(t.ctx, base+"/peer/status/"+reply.ID, reply.Secret); err != nil {
		// Otherwise the receiver would show the offer as waiting forever.
		tellReceiver()
		return err
	}
	if !t.move(statusSending, statusWaiting) {
		return context.Canceled
	}
	return upload(uploadCtx, base+"/peer/upload/"+reply.ID, reply.Secret, entries, &t.done)
}

// waitForAnswer polls the receiver until it accepts or turns the transfer
// down.
func waitForAnswer(ctx context.Context, url, secret string) error {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		var reply struct {
			Status Status `json:"status"`
		}
		if err := call(ctx, http.MethodGet, url, secret, nil, &reply); err != nil {
			return err
		}
		switch reply.Status {
		case statusAccepted:
			return nil
		case statusWaiting:
		default:
			return errors.New("The receiver declined or cancelled")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// upload streams the entries to the receiver.
func upload(ctx context.Context, url, secret string, entries []entry, done *atomic.Int64) error {
	pr, pw := io.Pipe()
	defer pr.Close()
	writeErr := make(chan error, 1)
	go func() {
		err := writeTar(ctx, pw, entries, done)
		writeErr <- err
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Transfer-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		if werr := <-writeErr; werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
			return werr // a local problem explains the failure better
		}
		return connectionError(ctx, err)
	}
	defer resp.Body.Close()
	if err := replyError(resp); err != nil {
		return err
	}
	return <-writeErr
}

// call makes a small JSON request to another device.
func call(ctx context.Context, method, url, secret string, body, reply any) error {
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
	resp, err := http.DefaultClient.Do(req)
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
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}
	return fmt.Errorf("Could not reach the other device: %w", err)
}

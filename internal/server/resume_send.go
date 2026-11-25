package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

type resumeTarget struct {
	ID, Secret string
	Device     trust.Device
	ChunkBytes int
}

var (
	errResumeOffset = errors.New("The receiver's saved progress doesn't match this transfer")
	errChunkIdle    = errors.New("The upload timed out")
)

const minChunkSize = 16 << 10

// Send starts an outgoing transfer from the local page or terminal.
func (s *Server) Send(t *transfer.Transfer, entries []transfer.Entry, host string, port int, id string) {
	s.Transfers.Add(t)
	r := s.resume
	if r == nil || t.Kind == transfer.Text {
		go func() { t.Finish(SendTo(t, entries, s.Trust, s.name, host, port, id)) }()
		return
	}
	r.runWorker(func() {
		save := func(target resumeTarget) error {
			files, err := transfer.Snapshot(entries)
			if err != nil {
				return err
			}
			saved := savedSend{ID: t.ID, Peer: t.Peer, Host: host, Port: port, Items: t.Items, Size: t.Size, Files: files, Target: target}
			for _, file := range files {
				if strings.HasPrefix(file.Path, s.temp+string(filepath.Separator)) {
					saved.DropDir = s.temp
					break
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if err := r.save("out-", t.ID, saved); err != nil {
				return err
			}
			r.outgoing[t.ID] = saved
			return nil
		}
		s.finishSend(t, sendTo(r.ctx, t, entries, s.Trust, s.name, host, port, id, save))
	})
}

func (s *Server) restoreSend(record savedSend) {
	r := s.resume
	t := transfer.New(transfer.Outgoing, record.Peer, record.Items, record.Size, transfer.Accepted)
	t.ID = record.ID
	t.Move(transfer.Sending, transfer.Accepted)
	s.Transfers.Add(t)
	r.runWorker(func() {
		ctx, cancel := context.WithCancel(r.ctx)
		defer cancel()
		stop := context.AfterFunc(t.Context(), cancel)
		defer stop()
		base := "https://" + net.JoinHostPort(record.Host, strconv.Itoa(record.Port))
		target := record.Target
		peer := &pinned{store: s.Trust, id: target.Device.ID, fingerprint: target.Device.Fingerprint}
		client := transferClient(s.Trust, peer)
		defer client.CloseIdleConnections()
		err := errResumeOffset
		if target.ChunkBytes > 0 && target.ChunkBytes <= chunkBytes {
			err = sendArchive(ctx, t, client, base, target, func() ([]transfer.Entry, error) {
				return transfer.Restore(record.Files)
			})
		}
		if t.Context().Err() != nil || err != nil && ctx.Err() == nil && target.ID != "" {
			call(context.Background(), client, http.MethodPost, base+"/peer/cancel/"+target.ID, target.Secret, nil, nil)
		}
		s.finishSend(t, err)
	})
}

func (s *Server) finishSend(t *transfer.Transfer, err error) {
	r := s.resume
	if err == nil || r.ctx.Err() == nil || t.Context().Err() != nil {
		r.mu.Lock()
		if saved, ok := r.outgoing[t.ID]; ok {
			if err := os.Remove(r.path("out-", t.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("Could not remove a finished send: %v", err)
			} else {
				delete(r.outgoing, t.ID)
				if saved.DropDir != s.temp {
					s.cleanDrop(saved.DropDir)
				}
			}
		}
		r.mu.Unlock()
	}
	t.Finish(err)
}

func sendArchive(ctx context.Context, t *transfer.Transfer, client *http.Client, base string, target resumeTarget, loadEntries func() ([]transfer.Entry, error)) error {
	sender := chunkSender{
		t: t, client: client, url: base + "/peer/upload/" + target.ID, secret: target.Secret, limit: target.ChunkBytes,
	}
	state, err := sender.retry(ctx, http.MethodGet, 0, nil)
	if err != nil {
		return err
	}
	if state.Status == transfer.Complete {
		t.Done.Store(t.Size)
		return nil
	}
	if state.Offset < 0 {
		return errResumeOffset
	}
	if state.ReadyToSave {
		return sender.finish(ctx, state.Offset)
	}
	entries, err := loadEntries()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	written := make(chan struct{})
	defer func() {
		reader.Close()
		<-written
	}()
	go func() {
		defer close(written)
		writer.CloseWithError(transfer.WriteTar(ctx, writer, entries, &t.Done))
	}()
	// Regenerate the same archive locally up to the receiver's saved offset.
	// Only the bytes after that offset go across the network.
	h := sha256.New()
	if _, err := io.CopyN(h, reader, state.Offset); err != nil {
		if errors.Is(err, io.EOF) {
			return errResumeOffset
		}
		return err
	}
	if state.Offset > 0 && hex.EncodeToString(h.Sum(nil)) != state.Prefix {
		return errResumeOffset
	}
	offset := state.Offset
	buffer := make([]byte, target.ChunkBytes)
	for {
		n, readErr := io.ReadFull(reader, buffer)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if n > 0 {
			if _, err := sender.retry(ctx, http.MethodPatch, offset, buffer[:n]); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr != nil {
			return sender.finish(ctx, offset)
		}
	}
}

type chunkSender struct {
	t           *transfer.Transfer
	client      *http.Client
	url, secret string
	limit       int
}

func (s *chunkSender) finish(ctx context.Context, offset int64) error {
	reply, err := s.retry(ctx, http.MethodPatch, offset, nil)
	if err != nil {
		return err
	}
	if reply.Status != transfer.Complete || reply.Offset != offset {
		return errResumeOffset
	}
	s.t.Done.Store(s.t.Size)
	return nil
}

func (s *chunkSender) retry(ctx context.Context, method string, offset int64, data []byte) (chunkReply, error) {
	ctx, cancel := context.WithTimeout(ctx, resumeFor)
	defer cancel()
	t := s.t
	defer t.Reconnecting.Store(false)
	defer t.SetPendingError(nil)
	delay := time.Second
	for {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return chunkReply{}, transfer.ErrInterrupted
			}
			return chunkReply{}, ctx.Err()
		}
		part := data[:min(len(data), s.limit)]
		reply, retry, err := requestChunk(ctx, s.client, s.url, s.secret, method, offset, part)
		if !retry {
			if err != nil || len(data) == 0 {
				return reply, err
			}
			// A larger attempt may have reached the receiver before its
			// reply was lost. A shorter retry can confirm those bytes too.
			confirmed := reply.Offset - offset
			if confirmed < int64(len(part)) || confirmed > int64(len(data)) {
				return chunkReply{}, errResumeOffset
			}
			if confirmed == int64(len(data)) {
				return reply, nil
			}
			offset = reply.Offset
			data = data[confirmed:]
			t.Reconnecting.Store(false)
			t.SetPendingError(nil)
			delay = time.Second
			continue
		}
		if errors.Is(err, errChunkIdle) && len(part) > minChunkSize {
			s.limit = max(minChunkSize, len(part)/2)
		}
		waitingToSave := errors.Is(err, errSavePending)
		t.Reconnecting.Store(!waitingToSave)
		if waitingToSave {
			t.SetPendingError(errSavePending)
		} else {
			t.SetPendingError(nil)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		delay = min(2*delay, 5*time.Second)
	}
}

func requestChunk(ctx context.Context, client *http.Client, url, secret, method string, offset int64, data []byte) (chunkReply, bool, error) {
	ctx, activity, stop := watchChunkActivity(ctx)
	defer stop()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { activity() }})
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
	if err != nil {
		return chunkReply{}, false, err
	}
	req.Header.Set("X-Transfer-Secret", secret)
	if method == http.MethodPatch {
		req.Header.Set(offsetHeader, strconv.FormatInt(offset, 10))
	}
	if len(data) > 0 {
		req.Body = chunkRequestBody{req.Body, activity}
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			return chunkReply{}, true, errChunkIdle
		}
		return chunkReply{}, retryConnection(err), connectionError(ctx, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var reply chunkReply
		err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply)
		if err != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			return chunkReply{}, true, errChunkIdle
		}
		return reply, retryConnection(err), err
	case http.StatusRequestTimeout:
		return chunkReply{}, true, errChunkIdle
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return chunkReply{}, true, transfer.ErrInterrupted
	case http.StatusInsufficientStorage:
		return chunkReply{}, true, errSavePending
	default:
		err := replyError(resp)
		if err == nil {
			err = errResumeOffset
		}
		return chunkReply{}, false, err
	}
}

type chunkRequestBody struct {
	io.ReadCloser
	activity func()
}

func (b chunkRequestBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if n > 0 {
		b.activity()
	}
	return n, err
}

// A chunk may take longer than the timeout if its bytes are still moving.
func watchChunkActivity(parent context.Context) (context.Context, func(), func()) {
	ctx, cancel := context.WithCancelCause(parent)
	start := time.Now()
	var last atomic.Int64
	activity := func() { last.Store(time.Since(start).Nanoseconds()) }
	timer := time.NewTimer(chunkTimeout)
	go func() {
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				idle := time.Since(start) - time.Duration(last.Load())
				if idle >= chunkTimeout {
					cancel(context.DeadlineExceeded)
					return
				}
				timer.Reset(chunkTimeout - idle)
			}
		}
	}()
	return ctx, activity, func() { cancel(nil) }
}

func retryConnection(err error) bool {
	_, networkError := errors.AsType[*net.OpError](err)
	return networkError || transfer.Interrupted(err) || errors.Is(err, io.EOF) ||
		errors.Is(err, context.DeadlineExceeded)
}

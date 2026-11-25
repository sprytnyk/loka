//go:build linux

package server

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/transfer"
)

type pacedChunkTransport struct{ next http.RoundTripper }

func (p pacedChunkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Body = &pacedChunkBody{ReadCloser: req.Body, ctx: req.Context()}
	return p.next.RoundTrip(req)
}

type pacedChunkBody struct {
	io.ReadCloser
	ctx context.Context
}

func (p *pacedChunkBody) Read(b []byte) (int, error) {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
		return 0, p.ctx.Err()
	case <-timer.C:
		return p.ReadCloser.Read(b[:min(len(b), 16<<10)])
	}
}

type countedChunkBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (b countedChunkBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func newSavedUploadForTest(t *testing.T, size int64) (*Server, *transfer.Transfer, *savedUpload, string) {
	t.Helper()
	s := New("receiver", "test")
	s.Trust = openTrust(t)
	if err := s.Resume(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	in := transfer.New(transfer.Incoming, "sender", []string{"file"}, size, transfer.Waiting)
	in.Resumable = true
	in.Browser = true
	in.Secret = transfer.RandomToken(24)
	s.Transfers.Add(in)
	dest := t.TempDir()
	if !s.Accept(in, dest) {
		t.Fatal("accept failed")
	}
	value, ok := s.resume.incoming.Load(in.ID)
	if !ok {
		t.Fatal("missing saved offer")
	}
	t.Cleanup(s.Close)
	return s, in, value.(*savedUpload), dest
}

func TestChunkCompletesWhileBytesKeepMoving(t *testing.T) {
	receiver, in, _, _ := newSavedUploadForTest(t, chunkBytes)
	var received atomic.Int64
	base := serve(t, receiver, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				r.Body = countedChunkBody{ReadCloser: r.Body, read: &received}
			}
			handler.ServeHTTP(w, r)
		})
	})
	client := browser(receiver)
	client.Transport = pacedChunkTransport{next: client.Transport}
	start := time.Now()
	_, retry, err := requestChunk(t.Context(), client, secure(base)+"/peer/upload/"+in.ID, in.Secret, http.MethodPatch, 0, make([]byte, chunkBytes))
	t.Logf("paced PATCH received %d of %d bytes in %s; retry=%v, confirmed=%d, error=%v", received.Load(), chunkBytes, time.Since(start).Round(time.Millisecond), retry, in.Done.Load(), err)
	if retry || err != nil || received.Load() != chunkBytes || in.Done.Load() != chunkBytes {
		t.Fatal("steady progress should keep the chunk alive")
	}
}

func TestChunkTimeoutRetriesSmallerAndKeepsConfirmedBytes(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "timed out before saving"
		if committed {
			name = "timed out after saving"
		}
		t.Run(name, func(t *testing.T) {
			receiver, in, _, _ := newSavedUploadForTest(t, chunkBytes)
			var attempts, secondSize atomic.Int64
			base := serve(t, receiver, func(handler http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch attempts.Add(1) {
					case 1:
						if committed {
							handler.ServeHTTP(discardReply{w}, r)
						} else {
							io.Copy(io.Discard, r.Body)
						}
						w.WriteHeader(http.StatusRequestTimeout)
						return
					case 2:
						secondSize.Store(r.ContentLength)
					}
					handler.ServeHTTP(w, r)
				})
			})
			sender := chunkSender{t: transfer.New(transfer.Outgoing, "receiver", nil, chunkBytes, transfer.Sending),
				client: browser(receiver), url: secure(base) + "/peer/upload/" + in.ID, secret: in.Secret, limit: chunkBytes}
			reply, err := sender.retry(t.Context(), http.MethodPatch, 0, make([]byte, chunkBytes))
			if err != nil || reply.Offset != chunkBytes || in.Done.Load() != chunkBytes || secondSize.Load() != chunkBytes/2 {
				t.Fatalf("offset=%d, confirmed=%d, retry size=%d, err=%v", reply.Offset, in.Done.Load(), secondSize.Load(), err)
			}
			wantAttempts := int64(3)
			if committed {
				wantAttempts = 2
			}
			if attempts.Load() != wantAttempts {
				t.Fatalf("expected %d requests, got %d", wantAttempts, attempts.Load())
			}
		})
	}
}

func TestExtractionWriteErrorKeepsConfirmedArchive(t *testing.T) {
	const size = 256 << 10
	s, in, uploaded, destination := newSavedUploadForTest(t, size)
	client := browser(s)
	base := serve(t, s, nil)
	url := secure(base) + "/peer/upload/" + in.ID
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	if err := w.WriteHeader(&tar.Header{Name: "file", Size: size, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.resume.appendUpload(uploaded, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if uploaded.Offset != int64(archive.Len()) {
		t.Fatal("archive was not fully confirmed")
	}
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	limit := original
	limit.Cur = 64 << 10
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	var retry bool
	var attempt error
	func() {
		defer func() {
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
				t.Fatal(err)
			}
		}()
		_, retry, attempt = requestChunk(t.Context(), client, url, in.Secret, http.MethodPatch, int64(archive.Len()), nil)
	}()
	if !retry || !errors.Is(attempt, errSavePending) || !strings.Contains(in.View().Error, syscall.EFBIG.Error()) {
		t.Fatalf("expected a retryable extraction write error, got %v, retry=%v, receiver=%q", attempt, retry, in.View().Error)
	}
	_, archived := os.Stat(uploaded.archive())
	visible, err := os.ReadDir(destination)
	t.Logf("confirmed %d archive bytes, injected extraction write error, receiver=%s, archive removed=%v, visible=%d", archive.Len(), in.Status(), os.IsNotExist(archived), len(visible))
	if err != nil || in.Status() != transfer.Receiving || archived != nil || len(visible) != 1 {
		t.Fatalf("expected a retryable transfer with its archive intact: %v", err)
	}
	reply, retry, err := requestChunk(t.Context(), client, url, in.Secret, http.MethodPatch, int64(archive.Len()), nil)
	if err != nil || retry || reply.Status != transfer.Complete {
		t.Fatalf("retry final save: %+v, %v, retry=%v", reply, err, retry)
	}
	got, err := os.ReadFile(filepath.Join(destination, "file"))
	if err != nil || len(got) != size {
		t.Fatalf("retry did not publish the whole file: %d bytes, %v", len(got), err)
	}
}

func TestRestartFinishesInterruptedPublication(t *testing.T) {
	s, in, uploaded, destination := newSavedUploadForTest(t, 2)
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for _, name := range []string{"a", "b"} {
		if err := w.WriteHeader(&tar.Header{Name: name, Size: 1, Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.resume.appendUpload(uploaded, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "a"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.preparePublication(uploaded); err != nil {
		t.Fatal(err)
	}
	next := uploaded.uploadRecord
	next.Move = transfer.PublishMove{Index: 0, Target: "a (1)"}
	if err := s.resume.checkpoint(uploaded, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(uploaded.stagedFiles(), "a"), filepath.Join(destination, next.Move.Target)); err != nil {
		t.Fatal(err)
	}
	// Stop after the first rename and before the checkpoint for the next one.
	s.Close()
	reopened := New("receiver", "test")
	reopened.Trust = s.Trust
	if err := reopened.Resume(t.Context(), s.resume.dir); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result := waitForEnd(t, reopened.Transfers.Get(in.ID))
	visible, err := os.ReadDir(destination)
	_, archiveErr := os.Stat(uploaded.archive())
	t.Logf("reopened between top-level moves: status=%s, files=%v, archive removed=%v", result.Status, visible, os.IsNotExist(archiveErr))
	if err != nil || len(visible) != 3 || result.Status != transfer.Complete || !os.IsNotExist(archiveErr) {
		t.Fatalf("expected each file to appear once: %v, %v, %+v", visible, err, result)
	}
	for name, want := range map[string]string{"a": "existing", "a (1)": "a", "b": "b"} {
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(got) != want {
			t.Fatalf("published %s: %q, %v", name, got, err)
		}
	}
}

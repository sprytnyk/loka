package server

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sprytnyk/loka/internal/transfer"
)

func TestTransferResumesAfterBothAppsRestart(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	path := filepath.Join(source, "file.bin")
	data := bytes.Repeat([]byte("0123456789abcdef"), chunkBytes/8+17)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, items, size, err := transfer.Collect([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	senderDir, receiverDir := t.TempDir(), t.TempDir()
	senderTrust, receiverTrust := openTrust(t), openTrust(t)
	var backend atomic.Pointer[Server]
	var connected atomic.Bool
	var receivedAfterRestart atomic.Int64
	receivedAfterRestart.Store(-1)
	connected.Store(true)
	newReceiver := func() *Server {
		s := New("receiver", "test")
		s.Trust = receiverTrust
		if err := s.Resume(t.Context(), receiverDir); err != nil {
			t.Fatal(err)
		}
		backend.Store(s)
		return s
	}
	receiver := newReceiver()
	var restarted atomic.Bool
	base := serve(t, receiver, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/peer/upload/") && r.Method == http.MethodPatch {
				offset, _ := strconv.ParseInt(r.Header.Get(offsetHeader), 10, 64)
				if !connected.Load() || !restarted.Load() && offset > 0 {
					panic(http.ErrAbortHandler)
				}
				if restarted.Load() {
					receivedAfterRestart.CompareAndSwap(-1, offset)
				}
			}
			backend.Load().Handler().ServeHTTP(w, r)
		})
	})
	host, port := hostPort(t, base)
	newSender := func() *Server {
		s := New("sender", "test")
		s.Trust = senderTrust
		if err := s.Resume(t.Context(), senderDir); err != nil {
			t.Fatal(err)
		}
		return s
	}
	sender := newSender()
	out := transfer.New(transfer.Outgoing, host, items, size, transfer.Pairing)
	sender.Send(out, entries, host, port, "")
	in := waitForIncoming(t, receiver)
	if !receiver.Accept(in, destination) {
		t.Fatal("could not accept transfer")
	}
	waitFor(t, "the first chunk to be saved", func() bool { return in.Done.Load() >= chunkBytes })
	if _, err := os.Stat(filepath.Join(destination, "file.bin")); !os.IsNotExist(err) {
		t.Fatalf("unfinished file visible: %v", err)
	}
	connected.Store(false)
	sender.Close()
	receiver.Close()

	receiver = newReceiver()
	defer receiver.Close()
	in = receiver.Transfers.Get(in.ID)
	if in == nil || in.Status() != transfer.Receiving || in.Done.Load() != chunkBytes {
		t.Fatalf("receiver did not restore the saved chunk: %v", in)
	}
	restarted.Store(true)
	connected.Store(true)
	sender = newSender()
	defer sender.Close()
	out = sender.Transfers.Get(out.ID)
	if out == nil {
		t.Fatal("sender did not restore the transfer")
	}
	if v := waitForEnd(t, out); v.Status != transfer.Complete {
		t.Fatalf("sender ended %+v", v)
	}
	if v := waitForEnd(t, in); v.Status != transfer.Complete {
		t.Fatalf("receiver ended %+v", v)
	}
	if got := receivedAfterRestart.Load(); got != chunkBytes {
		t.Fatalf("first offset after restarting was %d, want %d", got, chunkBytes)
	}
	got, err := os.ReadFile(filepath.Join(destination, "file.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("received file differs: %v", err)
	}
	files, err := os.ReadDir(destination)
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected destination contents: %v, %v", files, err)
	}
}

func TestTransferRetriesLostChunkAndFinishReplies(t *testing.T) {
	receiver := New("receiver", "test")
	receiver.Trust = openTrust(t)
	if err := receiver.Resume(t.Context(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	var lostChunk, lostFinish atomic.Bool
	base := serve(t, receiver, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/peer/upload/") {
				if r.ContentLength > 0 && !lostChunk.Swap(true) || r.ContentLength == 0 && !lostFinish.Swap(true) {
					handler.ServeHTTP(discardReply{w}, r)
					panic(http.ErrAbortHandler)
				}
			}
			handler.ServeHTTP(w, r)
		})
	})
	source, destination := t.TempDir(), t.TempDir()
	path := filepath.Join(source, "file.txt")
	const data = "A chunk whose confirmation was lost."
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port := hostPort(t, base)
	out := startSending(t, "sender", []string{path}, host, port)
	in := waitForIncoming(t, receiver)
	if !receiver.Accept(in, destination) {
		t.Fatal("could not accept transfer")
	}
	if v := waitForEnd(t, out); v.Status != transfer.Complete {
		t.Fatalf("sender ended %+v", v)
	}
	if !lostChunk.Load() || !lostFinish.Load() || in.Status() != transfer.Complete {
		t.Fatal("test did not lose both replies and complete")
	}
	got, err := os.ReadFile(filepath.Join(destination, "file.txt"))
	if err != nil || string(got) != data {
		t.Fatalf("received %q: %v", got, err)
	}
	files, err := os.ReadDir(destination)
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected destination contents: %v, %v", files, err)
	}
}

type discardReply struct{ http.ResponseWriter }

func (w discardReply) Unwrap() http.ResponseWriter  { return w.ResponseWriter }
func (discardReply) WriteHeader(int)                {}
func (discardReply) Write(data []byte) (int, error) { return io.Discard.Write(data) }

func TestRestartChecksReceiverBeforeSource(t *testing.T) {
	const (
		finished     = "finished upload with missing source"
		awaitingSave = "upload awaiting save with missing source"
		needsSource  = "unfinished upload with changed source"
	)
	for _, receiverState := range []string{finished, awaitingSave, needsSource} {
		t.Run(receiverState, func(t *testing.T) {
			receiver := New("receiver", "test")
			receiver.Trust = openTrust(t)
			if err := receiver.Resume(t.Context(), t.TempDir()); err != nil {
				t.Fatal(err)
			}
			defer receiver.Close()
			var droppedFinish, restarted, checkedReceiver, resentData atomic.Bool
			base := serve(t, receiver, func(handler http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasPrefix(r.URL.Path, "/peer/upload/") {
						handler.ServeHTTP(w, r)
						return
					}
					if restarted.Load() {
						switch {
						case r.Method == http.MethodGet:
							checkedReceiver.Store(true)
						case r.Method == http.MethodPatch && r.ContentLength > 0:
							resentData.Store(true)
						}
					} else if r.Method == http.MethodPatch && r.ContentLength == 0 {
						if receiverState == finished {
							handler.ServeHTTP(discardReply{w}, r)
						}
						droppedFinish.Store(true)
						panic(http.ErrAbortHandler)
					}
					handler.ServeHTTP(w, r)
				})
			})
			host, port := hostPort(t, base)
			path, destination := filepath.Join(t.TempDir(), "source"), t.TempDir()
			const data = "source file contents"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			entries, items, size, err := transfer.Collect([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			senderDir, senderTrust := t.TempDir(), openTrust(t)
			openSender := func() *Server {
				s := New("sender", "test")
				s.Trust = senderTrust
				if err := s.Resume(t.Context(), senderDir); err != nil {
					t.Fatal(err)
				}
				return s
			}
			sender := openSender()
			out := transfer.New(transfer.Outgoing, host, items, size, transfer.Pairing)
			sender.Send(out, entries, host, port, "")
			in := waitForIncoming(t, receiver)
			if !receiver.Accept(in, destination) {
				t.Fatal("could not accept transfer")
			}
			waitFor(t, "the final request to lose its reply", droppedFinish.Load)
			sender.Close()
			if receiverState == awaitingSave {
				value, ok := receiver.resume.incoming.Load(in.ID)
				if !ok {
					t.Fatal("receiver did not save the archive")
				}
				u := value.(*savedUpload)
				// Save the state left when the receiver accepts the final
				// request but cannot move the files into place yet.
				u.mu.Lock()
				next := u.uploadRecord
				next.Ready = true
				err := receiver.resume.checkpoint(u, next)
				u.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			if receiverState == needsSource {
				if err := os.WriteFile(path, []byte("new contents"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			restarted.Store(true)
			sender = openSender()
			defer sender.Close()
			out = sender.Transfers.Get(out.ID)
			if out == nil {
				t.Fatal("sender did not restore the transfer")
			}
			v := waitForEnd(t, out)
			if !checkedReceiver.Load() {
				t.Fatal("restarted sender did not ask the receiver for its status")
			}
			if resentData.Load() {
				t.Fatal("restarted sender resent archive data")
			}
			if receiverState == needsSource {
				if v.Status != transfer.Failed || !strings.Contains(v.Error, "changed") {
					t.Fatalf("unfinished upload with changed source ended %+v", v)
				}
				return
			}
			if v.Status != transfer.Complete || v.Progress != 100 {
				t.Fatalf("upload with missing source ended %+v", v)
			}
			got, err := os.ReadFile(filepath.Join(destination, "source"))
			if err != nil || string(got) != data {
				t.Fatalf("received %q: %v", got, err)
			}
		})
	}
}

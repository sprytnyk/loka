package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sprytnyk/loka/internal/transfer"
)

// Keep partial uploads and completion receipts long enough for either device
// to be closed and reopened. Only activity on this transfer renews its time.
const resumeFor = 24 * time.Hour

type resumeStore struct {
	dir  string
	ctx  context.Context
	stop context.CancelFunc
	lock *os.File

	mu       sync.Mutex
	closed   bool
	workers  sync.WaitGroup
	incoming sync.Map // transfer ID to *savedUpload
	outgoing map[string]savedSend
}

type savedSend struct {
	ID, Peer, Host, DropDir string
	Items                   []string
	Size                    int64
	Port                    int
	Files                   []transfer.SourceFile
	Target                  resumeTarget
}

type uploadRecord struct {
	ID, Secret, Peer, DeviceID, Fingerprint string
	Items                                   []string
	Size, Offset                            int64
	Checksum                                []byte
	Browser                                 bool
	Destination, Stage                      string
	Status                                  transfer.Status
	Error                                   string
	Ready                                   bool // the sender asked the receiver to save the archive
	Saving                                  bool
	Move                                    transfer.PublishMove
}

type savedUpload struct {
	mu sync.Mutex
	t  *transfer.Transfer
	uploadRecord
}

// Resume opens the saved transfers for this instance and restarts its sends.
// The caller closes the server even if serving or sending fails.
func (s *Server) Resume(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := lockResume(lock); err != nil {
		lock.Close()
		return fmt.Errorf("could not open saved transfers: %w", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		lock.Close()
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	r := &resumeStore{dir: dir, ctx: ctx, stop: stop, lock: lock, outgoing: make(map[string]savedSend)}
	s.resume = r
	s.temp = filepath.Join(dir, "drop-"+transfer.RandomToken(9))
	var sends []savedSend
	for _, file := range files {
		name := file.Name()
		if id, ok := resumeID(name, "in-"); ok {
			var record uploadRecord
			if err := readSaved(filepath.Join(dir, name), &record); err != nil || record.ID != id {
				log.Printf("Could not read a saved incoming transfer: %v", err)
				continue
			}
			s.restoreUpload(record)
		}
		if id, ok := resumeID(name, "out-"); ok {
			var record savedSend
			if err := readSaved(filepath.Join(dir, name), &record); err != nil || record.ID != id {
				log.Printf("Could not read a saved outgoing transfer: %v", err)
				continue
			}
			r.outgoing[id] = record
			sends = append(sends, record)
		}
	}
	for _, record := range sends {
		s.restoreSend(record)
	}
	r.runWorker(s.maintainUploads)
	return nil
}

func (s *Server) maintainUploads() {
	r := s.resume
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-timer.C:
			r.incoming.Range(func(key, value any) bool {
				u := value.(*savedUpload)
				if !u.mu.TryLock() {
					return true
				}
				defer u.mu.Unlock()
				expired := r.expired("in-", u.ID)
				if u.Saving || u.Ready && !expired {
					s.publishUpload(u)
					return true
				}
				if !expired {
					return true
				}
				if !u.Status.Final() {
					u.t.Finish(transfer.ErrInterrupted)
					u.Status, u.Error = transfer.Failed, transfer.ErrInterrupted.Error()
				}
				if os.RemoveAll(u.Stage) == nil && os.Remove(r.path("in-", u.ID)) == nil {
					r.incoming.Delete(key)
				}
				return true
			})
		}
	}
}

func resumeID(name, prefix string) (string, bool) {
	if len(name) != len(prefix)+22+len(".json") || name[:len(prefix)] != prefix || name[len(name)-5:] != ".json" {
		return "", false
	}
	id := name[len(prefix) : len(name)-5]
	b, err := base64.RawURLEncoding.DecodeString(id)
	return id, err == nil && len(b) == 16
}

func readSaved(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(value)
}

func (r *resumeStore) save(prefix, id string, value any) error {
	return writeSaved(r.path(prefix, id), value)
}

func writeSaved(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".transfer-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (r *resumeStore) path(prefix, id string) string {
	return filepath.Join(r.dir, prefix+id+".json")
}

func (r *resumeStore) expired(prefix, id string) bool {
	info, err := os.Stat(r.path(prefix, id))
	return err == nil && time.Since(info.ModTime()) > resumeFor
}

func (r *resumeStore) runWorker(run func()) {
	if r.startWork() {
		go func() {
			defer r.workers.Done()
			run()
		}()
	}
}

func (r *resumeStore) startWork() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.workers.Add(1)
	return true
}

func (s *Server) closeResume() {
	r := s.resume
	r.mu.Lock()
	r.closed = true
	r.stop()
	r.mu.Unlock()
	r.workers.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	s.cleanDrop(s.temp)
	r.lock.Close()
}

// cleanDrop removes a run's copied browser files once all sends are done.
// The caller holds the store mutex. The current run can still select its files.
func (s *Server) cleanDrop(dir string) {
	if dir == "" || filepath.Dir(dir) != s.resume.dir {
		return
	}
	for _, saved := range s.resume.outgoing {
		if saved.DropDir == dir {
			return
		}
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("Could not remove temporary upload files: %v", err)
	}
}

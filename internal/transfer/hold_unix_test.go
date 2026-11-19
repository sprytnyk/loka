//go:build unix

package transfer

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestOpeningNeverWaitsOnANamedPipe puts a named pipe where a chosen file
// was looked at. Opening a pipe waits for a writer, which would hang the
// send before the offer is made.
func TestOpeningNeverWaitsOnANamedPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	seen, err := lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		h := newHolder()
		defer h.release()
		_, err := h.open(path, seen)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errReplaced) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening waited on the named pipe")
	}
}

// TestOpenForReadingOpensANamedPipeAtOnce checks the open itself, which
// openChosen relies on when a pipe takes the path's place after its checks.
func TestOpenForReadingOpensANamedPipeAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openForReading(path)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the open waited for a writer")
	}
}

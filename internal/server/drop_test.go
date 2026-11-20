package server

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/treetest"
)

// cutAfter passes connections on to target. Once more than limit bytes
// went through one toward target, it resets both ends, as when the other
// device's program ends. Or, with silent, it stops passing anything on in
// either direction, as when the network between two devices goes away.
func cutAfter(t *testing.T, target string, limit int64, silent bool) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var cuts []func()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, cut := range cuts {
			cut()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			from, err := ln.Accept()
			if err != nil {
				return
			}
			to, err := net.Dial("tcp", target)
			if err != nil {
				from.Close()
				continue
			}
			cut := sync.OnceFunc(func() {
				for _, c := range []net.Conn{from, to} {
					c.(*net.TCPConn).SetLinger(0)
					c.Close()
				}
			})
			mu.Lock()
			cuts = append(cuts, cut)
			mu.Unlock()
			var gone atomic.Bool
			wg.Go(func() {
				io.Copy(unlessGone{from, &gone}, to)
				if !silent {
					cut()
				}
			})
			wg.Go(func() {
				io.CopyN(to, from, limit)
				if silent {
					gone.Store(true)
				} else {
					cut()
				}
			})
		}
	})
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// unlessGone writes to w until gone, then drops what it is given.
type unlessGone struct {
	w    io.Writer
	gone *atomic.Bool
}

func (u unlessGone) Write(b []byte) (int, error) {
	if u.gone.Load() {
		return len(b), nil
	}
	return u.w.Write(b)
}

// TestDroppedConnectionEndsBothSides drops the connection while a file is
// sent, with no cancel from either side, and checks both sides fail and
// nothing is left in the destination.
func TestDroppedConnectionEndsBothSides(t *testing.T) {
	receiver, host, port, _ := startRecorded(t)
	cutHost, cutPort := cutAfter(t, net.JoinHostPort(host, strconv.Itoa(port)), 256<<10, false)
	source := t.TempDir()
	treetest.Write(t, source, map[string]string{"dir/file": strings.Repeat("x", 4<<20)})
	out := sendFrom(t, openTrust(t), []string{filepath.Join(source, "dir")}, cutHost, cutPort)
	in := waitForIncomingStatus(t, receiver, transfer.Waiting)
	destination := t.TempDir()
	if !receiver.Accept(in, destination) {
		t.Fatal("couldn't accept")
	}

	sent, received := waitForEnd(t, out), waitForEnd(t, in)
	for side, v := range map[string]transfer.View{"sender": sent, "receiver": received} {
		if v.Status != transfer.Failed || v.Error != transfer.ErrInterrupted.Error() {
			t.Errorf("the %s ended %s, %q", side, v.Status, v.Error)
		}
	}
	if in.Done.Load() == 0 {
		t.Error("the connection dropped before the file started")
	}
	waitFor(t, "the destination to empty", func() bool {
		entries, _ := os.ReadDir(destination)
		return len(entries) == 0
	})
}

// TestSilentConnectionEndsBothSides stops the connection while a file is
// sent, with nothing to tell either side, and checks both give up once no
// bytes move.
func TestSilentConnectionEndsBothSides(t *testing.T) {
	defer func(d time.Duration) { transfer.StallTimeout = d }(transfer.StallTimeout)
	transfer.StallTimeout = 300 * time.Millisecond
	receiver, host, port, _ := startRecorded(t)
	cutHost, cutPort := cutAfter(t, net.JoinHostPort(host, strconv.Itoa(port)), 256<<10, true)
	source := t.TempDir()
	// Bigger than what the connection's buffers hold, so the sender stalls.
	treetest.Write(t, source, map[string]string{"file": strings.Repeat("x", 64<<20)})
	out := sendFrom(t, openTrust(t), []string{filepath.Join(source, "file")}, cutHost, cutPort)
	in := waitForIncomingStatus(t, receiver, transfer.Waiting)
	destination := t.TempDir()
	if !receiver.Accept(in, destination) {
		t.Fatal("couldn't accept")
	}

	for side, tr := range map[string]*transfer.Transfer{"sender": out, "receiver": in} {
		if v := waitForEnd(t, tr); v.Status != transfer.Failed || v.Error != transfer.ErrInterrupted.Error() {
			t.Errorf("the %s ended %s, %q", side, v.Status, v.Error)
		}
	}
	if done := out.Done.Load(); done == 0 || done == out.Size {
		t.Errorf("the sender sent %d of %d bytes", done, out.Size)
	}
	waitFor(t, "the destination to empty", func() bool {
		entries, _ := os.ReadDir(destination)
		return len(entries) == 0
	})
}

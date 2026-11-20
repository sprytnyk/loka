package server

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/treetest"
)

// cutAfter passes connections on to target. Once more than limit bytes
// went through one toward target, it resets both ends, as when the other
// device's program ends.
func cutAfter(t *testing.T, target string, limit int64) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		ln.Close()
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
			wg.Go(func() {
				io.Copy(from, to)
				cut()
			})
			wg.Go(func() {
				io.CopyN(to, from, limit)
				cut()
			})
		}
	})
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// TestDroppedConnectionEndsBothSides drops the connection while a file is
// sent, with no cancel from either side, and checks both sides fail and
// nothing is left in the destination.
func TestDroppedConnectionEndsBothSides(t *testing.T) {
	receiver, host, port, _ := startRecorded(t)
	cutHost, cutPort := cutAfter(t, net.JoinHostPort(host, strconv.Itoa(port)), 256<<10)
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

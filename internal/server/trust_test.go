package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/treetest"
	"github.com/sprytnyk/loka/internal/trust"
)

// recorder keeps every byte a listener's connections receive.
type recorder struct {
	net.Listener
	mu  sync.Mutex
	got bytes.Buffer
}

func (r *recorder) Accept() (net.Conn, error) {
	c, err := r.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &recordedConn{c, r}, nil
}

func (r *recorder) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.got.Bytes())
}

type recordedConn struct {
	net.Conn
	r *recorder
}

func (c *recordedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.r.mu.Lock()
	c.r.got.Write(b[:n])
	c.r.mu.Unlock()
	return n, err
}

// startRecorded runs a receiver as Loka does and records what reaches it.
func startRecorded(t *testing.T) (*Server, string, int, *recorder) {
	t.Helper()
	s := New("receiver", "test")
	s.Trust = openTrust(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, rec) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	return s, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port, rec
}

// sendFrom sends paths from a Loka with this identity, as /api/send does
// for an address typed in.
func sendFrom(t *testing.T, store *trust.Store, paths []string, host string, port int) *transfer.Transfer {
	t.Helper()
	return sendPicked(t, store, paths, host, port, "")
}

// sendPicked sends paths as /api/send does for the device with this ID,
// picked from the list.
func sendPicked(t *testing.T, store *trust.Store, paths []string, host string, port int, id string) *transfer.Transfer {
	t.Helper()
	entries, items, size, err := transfer.Collect(paths)
	if err != nil {
		t.Fatal(err)
	}
	out := transfer.New(transfer.Outgoing, host, items, size, transfer.Pairing)
	go func() { out.Finish(SendTo(out, entries, store, "laptop", host, port, id)) }()
	return out
}

// TestTransfersBetweenLokasAreEncrypted sends a file twice between two
// Lokas and checks that neither its contents nor its name crossed the
// wire in the clear, that the first transfer marks the sender as new, and
// that both sides remember each other once it is accepted.
func TestTransfersBetweenLokasAreEncrypted(t *testing.T) {
	receiver, host, port, rec := startRecorded(t)
	sender := openTrust(t)
	source, destination := t.TempDir(), t.TempDir()
	secret := make([]byte, 64<<10)
	rand.Read(secret)
	name := "secret-plans-Ω.txt"
	os.WriteFile(filepath.Join(source, name), secret, 0o644)

	for round, wantNew := range []bool{true, false} {
		out := sendFrom(t, sender, []string{filepath.Join(source, name)}, host, port)
		in := waitForIncomingStatus(t, receiver, transfer.Waiting)
		if v := in.View(); v.NewDevice != wantNew || v.Browser || v.Name != "laptop" {
			t.Fatalf("round %d: offer %+v, want new %v", round, v, wantNew)
		}
		receiver.Accept(in, destination)
		if v := waitForEnd(t, out); v.Status != transfer.Complete || v.NewDevice != wantNew {
			t.Fatalf("round %d: sender ended %+v, want new %v", round, v, wantNew)
		}
	}
	wire := rec.bytes()
	if len(wire) < len(secret) {
		t.Fatalf("recorded only %d bytes", len(wire))
	}
	for _, clear := range [][]byte{secret[:32], []byte(name), []byte("/peer/offer"), []byte("X-Transfer-Secret")} {
		if bytes.Contains(wire, clear) {
			t.Fatalf("%q crossed the wire in the clear", clear[:min(len(clear), 20)])
		}
	}
	got, _ := os.ReadFile(filepath.Join(destination, name))
	if !bytes.Equal(got, secret) {
		t.Fatal("the received file differs")
	}
	if standing, _, _ := sender.Check(receiver.Trust.ID, receiver.Trust.Fingerprint()); standing != trust.Known {
		t.Fatalf("sender's view of the receiver: %v", standing)
	}
}

// waitForIncomingStatus returns the newest incoming transfer at status.
func waitForIncomingStatus(t *testing.T, s *Server, status transfer.Status) *transfer.Transfer {
	t.Helper()
	var found *transfer.Transfer
	waitFor(t, "an incoming transfer "+string(status), func() bool {
		for _, tr := range s.Transfers.All() {
			if tr.Direction == transfer.Incoming && tr.Status() == status {
				found = tr
			}
		}
		return found != nil
	})
	return found
}

// TestReceiverRefusesASenderWithANewCertificate has the receiver remember
// the sender's ID with another certificate, as when another device poses
// as it, and checks both sides refuse, once on the receiver however often
// it tries, until the receiver forgets it.
func TestReceiverRefusesASenderWithANewCertificate(t *testing.T) {
	receiver, host, port, _ := startRecorded(t)
	sender := openTrust(t)
	receiver.Trust.Remember(trust.Device{ID: sender.ID, Name: "old laptop", Fingerprint: "an older certificate"})
	file := filepath.Join(t.TempDir(), "file")
	treetest.Write(t, filepath.Dir(file), map[string]string{"file": "abc"})

	out := sendFrom(t, sender, []string{file}, host, port)
	v := waitForEnd(t, out)
	if v.Status != transfer.Failed || !strings.Contains(v.Error, "different certificate") {
		t.Fatalf("sender ended %+v", v)
	}
	refused := waitForIncomingStatus(t, receiver, transfer.Failed).View()
	// The name it gave, laptop, is not proven, so the refusal names the
	// device it claims to be.
	if refused.Name != "old laptop" || !strings.HasPrefix(refused.Error, "old laptop showed a different certificate") || strings.Contains(refused.Error, "forget") {
		t.Fatalf("receiver shows %+v", refused)
	}
	waitForEnd(t, sendFrom(t, sender, []string{file}, host, port))
	if n := len(receiver.Transfers.All()); n != 1 {
		t.Fatalf("receiver shows %d refusals, want 1", n)
	}

	// As loka forget does.
	if _, err := receiver.Trust.Forget(func(d trust.Device) bool { return d.ID == sender.ID }); err != nil {
		t.Fatal(err)
	}
	sendFrom(t, sender, []string{file}, host, port)
	if in := waitForIncomingStatus(t, receiver, transfer.Waiting); !in.NewDevice() {
		t.Fatal("a forgotten device should count as new")
	}
}

// TestSenderRefusesAReceiverWithANewCertificate is the same from the
// sending side: the transfer fails before anything is offered.
func TestSenderRefusesAReceiverWithANewCertificate(t *testing.T) {
	receiver, host, port, _ := startRecorded(t)
	sender := openTrust(t)
	sender.Remember(trust.Device{ID: receiver.Trust.ID, Name: "desktop", Fingerprint: "an older certificate"})
	file := filepath.Join(t.TempDir(), "file")
	treetest.Write(t, filepath.Dir(file), map[string]string{"file": "abc"})

	v := waitForEnd(t, sendFrom(t, sender, []string{file}, host, port))
	if v.Status != transfer.Failed || !strings.HasPrefix(v.Error, "desktop showed a different certificate") {
		t.Fatalf("sender ended %+v", v)
	}
	if len(receiver.Transfers.All()) != 0 {
		t.Fatal("the receiver got an offer")
	}
}

// TestSenderRefusesAnotherDeviceThanPicked picks the remembered receiver
// from the list, has a device with another identity answer at its address,
// and checks the sender refuses before offering anything.
func TestSenderRefusesAnotherDeviceThanPicked(t *testing.T) {
	picked := openTrust(t)
	other, host, port, _ := startRecorded(t)
	sender := openTrust(t)
	sender.Remember(trust.Device{ID: picked.ID, Name: "desktop", Fingerprint: picked.Fingerprint()})
	file := filepath.Join(t.TempDir(), "file")
	treetest.Write(t, filepath.Dir(file), map[string]string{"file": "abc"})

	v := waitForEnd(t, sendPicked(t, sender, []string{file}, host, port, picked.ID))
	if v.Status != transfer.Failed || v.Error != errNotPicked.Error() {
		t.Fatalf("sender ended %+v", v)
	}
	if n := len(other.Transfers.All()); n != 0 {
		t.Fatalf("the other device got %d offers", n)
	}
	if devices, _ := sender.Devices(); len(devices) != 1 {
		t.Fatalf("sender remembers %+v", devices)
	}

	// Picking it from the list works when it is the one answering.
	out := sendPicked(t, sender, []string{file}, host, port, other.Trust.ID)
	other.Accept(waitForIncomingStatus(t, other, transfer.Waiting), t.TempDir())
	if v := waitForEnd(t, out); v.Status != transfer.Complete {
		t.Fatalf("sender ended %+v", v)
	}
}

// TestBrowserOffersAreLabelled checks an offer over TLS without a
// certificate, as a phone's browser makes, is labelled by address, whatever
// name it gives, and never remembered.
func TestBrowserOffersAreLabelled(t *testing.T) {
	receiver, base := startServer(t)
	var reply offerReply
	if err := call(t.Context(), browser(receiver), http.MethodPost, secure(base)+"/peer/offer", "", offer{Name: "posing as laptop", Items: []string{"f"}, Size: 1}, &reply); err != nil {
		t.Fatal(err)
	}
	in := receiver.Transfers.Get(reply.ID)
	if v := in.View(); !v.Browser || v.Name != "127.0.0.1" || v.NewDevice {
		t.Fatalf("offer %+v", v)
	}
	receiver.Accept(in, t.TempDir())
	if devices, _ := receiver.Trust.Devices(); len(devices) != 0 {
		t.Fatalf("remembered %+v", devices)
	}
}

// TestPlainHTTPOffersAreRefused has Loka 0.1, which sends without TLS,
// offer a file, and checks it is told to update and nothing is shown.
func TestPlainHTTPOffersAreRefused(t *testing.T) {
	receiver, base := startServer(t)
	for _, host := range []string{"", "192.0.2.10:45873"} { // this device, and another
		req, _ := http.NewRequest(http.MethodPost, base+"/peer/offer", strings.NewReader(`{"name":"old","items":["f"],"size":1}`))
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		err = replyError(resp) // as Loka 0.1 reads it
		resp.Body.Close()
		if err == nil || err.Error() != "This device runs a newer Loka. Update Loka to send to it." {
			t.Fatalf("host %q: got %v", host, err)
		}
	}
	if n := len(receiver.Transfers.All()); n != 0 {
		t.Fatalf("%d offers shown", n)
	}
}

// TestOldLokaGetsAClearError has a plain HTTP server, as Loka 0.1 runs,
// receive a TLS offer.
func TestOldLokaGetsAClearError(t *testing.T) {
	old := New("old", "0.1.0")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: old.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	file := filepath.Join(t.TempDir(), "file")
	treetest.Write(t, filepath.Dir(file), map[string]string{"file": "abc"})
	v := waitForEnd(t, sendFrom(t, openTrust(t), []string{file}, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port))
	if v.Status != transfer.Failed || !strings.Contains(v.Error, "Could not connect securely") {
		t.Fatalf("got %+v", v)
	}
}

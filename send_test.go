package main

import (
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests should not wait a second between polls.
func init() { pollEvery = 20 * time.Millisecond }

// hostPort splits a test server URL for /api/send.
func hostPort(t *testing.T, raw string) (string, int) {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(u.Host)
	n, _ := strconv.Atoi(port)
	return host, n
}

// TestSendBetweenTwoServers sends a nested folder and a file from one real
// server to another over loopback, through the same API the page uses, and
// compares the checksums of every file on both sides.
func TestSendBetweenTwoServers(t *testing.T) {
	sender, senderURL := startServer(t)
	receiver, receiverURL := startServer(t)
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")

	tree := map[string]string{
		"album/Збірка – 音楽 – 🎵 é 100% #1.mp3": "unicode",
		"album/nested/deeper/track.flac":       "deep",
		"album/empty/":                         "",
		"album/.hidden":                        "dot",
		"file.txt":                             "new",
	}
	if runtime.GOOS != "windows" {
		tree["album/Set: What’s next?.mp3"] = "punctuation"
	}
	writeTree(t, source, tree)
	big := make([]byte, 3<<20+17)
	rand.Read(big)
	os.WriteFile(filepath.Join(source, "album", "big.bin"), big, 0o644)
	writeTree(t, destination, map[string]string{"file.txt": "old"})

	host, port := hostPort(t, receiverURL)
	var sent struct{ ID string }
	paths := []string{filepath.Join(source, "album"), filepath.Join(source, "file.txt")}
	if code := api(t, sender, senderURL, "/api/send", map[string]any{"paths": paths, "host": host, "port": port}, &sent); code != http.StatusOK {
		t.Fatalf("send answered %d", code)
	}

	in := waitForIncoming(t, receiver)
	if in.Peer != "receiver" || strings.Join(in.Items, ",") != "album,file.txt" {
		t.Fatalf("offer shows %q %v", in.Peer, in.Items)
	}
	if code := api(t, receiver, receiverURL, "/api/accept", map[string]string{"id": in.ID, "destination": destination}, nil); code != http.StatusOK {
		t.Fatalf("accept answered %d", code)
	}

	out := sender.transfers.get(sent.ID)
	if v := waitForEnd(t, out); v.Status != statusComplete || v.Progress != 100 {
		t.Fatalf("sender ended %+v", v)
	}
	if v := waitForEnd(t, in); v.Status != statusComplete || v.Progress != 100 {
		t.Fatalf("receiver ended %+v", v)
	}

	want := readTree(t, source)
	want["file (1).txt"] = want["file.txt"]
	got := readTree(t, destination)
	if old, _ := os.ReadFile(filepath.Join(destination, "file.txt")); string(old) != "old" {
		t.Fatalf("existing file was overwritten: %q", old)
	}
	want["file.txt"] = got["file.txt"]
	sameTree(t, got, want)

	var st state
	api(t, receiver, receiverURL, "/api/state", nil, &st)
	if st.Destination != destination {
		t.Fatalf("destination not remembered: %q", st.Destination)
	}
}

func TestSeveralIncomingAtOnce(t *testing.T) {
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	root := t.TempDir()
	destination := filepath.Join(root, "destination")
	writeTree(t, root, map[string]string{"a/report.txt": "first", "b/report.txt": "second", "destination/": ""})

	var outs []*Transfer
	for _, dir := range []string{"a", "b"} {
		entries, items, size, err := collect([]string{filepath.Join(root, dir, "report.txt")})
		if err != nil {
			t.Fatal(err)
		}
		out := newTransfer(outgoing, host, items, size, statusWaiting)
		outs = append(outs, out)
		go func() { out.finish(sendTo(out, entries, dir, host, port)) }()
	}
	waitFor(t, "both offers", func() bool { return len(receiver.transfers.all()) == 2 })
	for _, in := range receiver.transfers.all() {
		api(t, receiver, receiverURL, "/api/accept", map[string]string{"id": in.ID, "destination": destination}, nil)
	}
	for _, out := range outs {
		if v := waitForEnd(t, out); v.Status != statusComplete {
			t.Fatalf("a sender ended %+v", v)
		}
	}
	got := map[string]bool{}
	for _, name := range []string{"report.txt", "report (1).txt"} {
		data, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		got[string(data)] = true
	}
	if !got["first"] || !got["second"] {
		t.Fatalf("one transfer replaced the other: %v", got)
	}
}

func TestDeclineEndsBothSides(t *testing.T) {
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"file": "abc"})
	entries, items, size, _ := collect([]string{filepath.Join(root, "file")})
	out := newTransfer(outgoing, host, items, size, statusWaiting)
	go func() { out.finish(sendTo(out, entries, "sender", host, port)) }()

	in := waitForIncoming(t, receiver)
	api(t, receiver, receiverURL, "/api/cancel", map[string]string{"id": in.ID}, nil)
	if v := waitForEnd(t, out); v.Status != statusFailed || !strings.Contains(v.Error, "declined") {
		t.Fatalf("sender ended %+v", v)
	}
	if got := in.Status(); got != statusCancelled {
		t.Fatalf("receiver status %q", got)
	}
}

func TestSenderCancelEndsBothSides(t *testing.T) {
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"file": "abc"})
	entries, items, size, _ := collect([]string{filepath.Join(root, "file")})
	out := newTransfer(outgoing, host, items, size, statusWaiting)
	go func() { out.finish(sendTo(out, entries, "sender", host, port)) }()

	in := waitForIncoming(t, receiver)
	out.stop()
	if v := waitForEnd(t, out); v.Status != statusCancelled {
		t.Fatalf("sender ended %+v", v)
	}
	waitFor(t, "the receiver to see the cancel", func() bool { return in.Status() == statusCancelled })
}

// TestSenderFailureEndsTheOffer has the receiver's first status reply fail,
// as a dropped connection would, and checks that the receiver stops
// waiting instead of showing the offer forever.
func TestSenderFailureEndsTheOffer(t *testing.T) {
	receiver := newServer("receiver", "test")
	var failed atomic.Bool
	handler := receiver.handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/peer/status/") && !failed.Swap(true) {
			http.Error(w, `{"error":"lost"}`, http.StatusBadGateway)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	host, port := hostPort(t, ts.URL)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"file": "abc"})
	entries, items, size, _ := collect([]string{filepath.Join(root, "file")})
	out := newTransfer(outgoing, host, items, size, statusWaiting)
	go func() { out.finish(sendTo(out, entries, "sender", host, port)) }()

	in := waitForIncoming(t, receiver)
	if v := waitForEnd(t, out); v.Status != statusFailed {
		t.Fatalf("sender ended %+v", v)
	}
	waitFor(t, "the receiver to stop waiting", func() bool { return in.Status() == statusCancelled })
}

func TestPeerRoutesNeedTheSecret(t *testing.T) {
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	var reply offerReply
	if err := call(t.Context(), http.MethodPost, base+"/peer/offer", "", offer{Name: "x", Items: []string{"f"}, Size: 1}, &reply); err != nil {
		t.Fatal(err)
	}
	if err := call(t.Context(), http.MethodPost, base+"/peer/cancel/"+reply.ID, "wrong", nil, nil); err == nil {
		t.Fatal("cancel worked without the secret")
	}
	if got := receiver.transfers.get(reply.ID).Status(); got != statusWaiting {
		t.Fatalf("status %q", got)
	}
}

func TestDestinationKeepsSpaces(t *testing.T) {
	spaced := filepath.Join(t.TempDir(), " spaced ")
	if err := os.Mkdir(spaced, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := existingDir(spaced); err != nil || got != spaced {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"", "  ", spaced + "x"} {
		if _, err := existingDir(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestOffersMustNameItemsAndASize(t *testing.T) {
	receiver, receiverURL := startServer(t)
	for _, o := range []offer{{Name: "x", Size: 1}, {Name: "x", Items: []string{"f"}, Size: -1}} {
		if err := call(t.Context(), http.MethodPost, receiverURL+"/peer/offer", "", o, nil); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	if n := len(receiver.transfers.all()); n != 0 {
		t.Fatalf("%d offers shown", n)
	}
}

func TestPageAPIIsOnlyForThisDevicesPage(t *testing.T) {
	s, base := startServer(t)
	for _, c := range []struct {
		name, host, key string
	}{
		{"no key", "", ""},
		{"wrong key", "", "wrong"},
		{"another site pointing at 127.0.0.1", "evil.example:45873", s.key},
	} {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/state", nil)
		if c.host != "" {
			req.Host = c.host
		}
		req.Header.Set("X-Loka-Key", c.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: answered %d", c.name, resp.StatusCode)
		}
	}
	if code := api(t, s, base, "/api/state", nil, nil); code != http.StatusOK {
		t.Fatalf("this device's page got %d", code)
	}
}

// TestReceiverCancelReachesALiveSender cancels on the receiving side while
// a real sender is still streaming, and checks the sender is told why.
func TestReceiverCancelReachesALiveSender(t *testing.T) {
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	root, destination := t.TempDir(), t.TempDir()
	big := make([]byte, 256<<20)
	os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644)
	entries, items, size, _ := collect([]string{filepath.Join(root, "big.bin")})
	out := newTransfer(outgoing, host, items, size, statusWaiting)
	go func() { out.finish(sendTo(out, entries, "sender", host, port)) }()

	in := waitForIncoming(t, receiver)
	api(t, receiver, receiverURL, "/api/accept", map[string]string{"id": in.ID, "destination": destination}, nil)
	waitFor(t, "bytes to arrive", func() bool { return in.done.Load() > 0 })
	api(t, receiver, receiverURL, "/api/cancel", map[string]string{"id": in.ID}, nil)
	v := waitForEnd(t, out)
	if in.done.Load() == size {
		t.Skip("the transfer finished before the cancel")
	}
	if v.Status != statusFailed || !strings.Contains(v.Error, "cancelled") {
		t.Fatalf("sender ended %+v", v)
	}
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatalf("a cancelled transfer left %v behind", entries)
	}
}

// TestReceiverCancelDuringUpload stalls the stream halfway, cancels on the
// receiving side, and checks that nothing is left in the destination.
func TestReceiverCancelDuringUpload(t *testing.T) {
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	destination := t.TempDir()
	data, size := makeTar(tarItem{"file", strings.Repeat("x", 4096)})

	var reply offerReply
	if err := call(t.Context(), http.MethodPost, base+"/peer/offer", "", offer{Name: "s", Items: []string{"file"}, Size: size}, &reply); err != nil {
		t.Fatal(err)
	}
	api(t, receiver, receiverURL, "/api/accept", map[string]string{"id": reply.ID, "destination": destination}, nil)

	pr, pw := io.Pipe()
	go pw.Write(data[:1024]) // the header and part of the file, then nothing
	req, _ := http.NewRequest(http.MethodPost, base+"/peer/upload/"+reply.ID, pr)
	req.Header.Set("X-Transfer-Secret", reply.Secret)
	answer := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			err = replyError(resp)
			resp.Body.Close()
		}
		answer <- err
	}()

	in := receiver.transfers.get(reply.ID)
	waitFor(t, "bytes to arrive", func() bool { return in.done.Load() > 0 })
	api(t, receiver, receiverURL, "/api/cancel", map[string]string{"id": reply.ID}, nil)
	go pw.Write([]byte("x")) // wake the stalled read
	if err := <-answer; err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("sender was told %v", err)
	}
	pw.Close()
	if got := in.Status(); got != statusCancelled {
		t.Fatalf("status %q", got)
	}
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatalf("a cancelled transfer left %v behind", entries)
	}
}

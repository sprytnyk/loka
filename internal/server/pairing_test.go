package server

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/treetest"
	"github.com/sprytnyk/loka/internal/trust"
)

// lokaClient makes requests as a Loka with this identity, trusting any
// receiver.
func lokaClient(store *trust.Store) *http.Client {
	config := store.ClientConfig(func(id, fingerprint string) error { return nil })
	return &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
}

// offerFrom offers a file from a Loka that commits to nonce.
func offerFrom(t *testing.T, client *http.Client, base, nonce string) offerReply {
	t.Helper()
	var reply offerReply
	o := offer{Name: "laptop", Items: []string{"f"}, Size: 1, Commitment: trust.Commit(nonce)}
	if err := call(t.Context(), client, http.MethodPost, secure(base)+"/peer/offer", "", o, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// reveal reveals nonce for the offer in reply.
func reveal(t *testing.T, client *http.Client, base string, reply offerReply, nonce string) error {
	t.Helper()
	body := map[string]string{"nonce": nonce}
	return call(t.Context(), client, http.MethodPost, secure(base)+"/peer/reveal/"+reply.ID, reply.Secret, body, nil)
}

func oneFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	treetest.Write(t, dir, map[string]string{"file": "abc"})
	return filepath.Join(dir, "file")
}

// TestBothSidesShowTheSameCode sends between two Lokas and checks that the
// offer waits for an answer only once both sides show one code.
func TestBothSidesShowTheSameCode(t *testing.T) {
	receiver, host, port, _ := startRecorded(t)
	out := sendFrom(t, openTrust(t), []string{oneFile(t)}, host, port)
	in := waitForIncomingStatus(t, receiver, transfer.Waiting)
	got, want := out.View().Code, in.View().Code
	if !regexp.MustCompile(`^\d{3} \d{3}$`).MatchString(want) || got != want {
		t.Fatalf("sender shows %q, receiver %q", got, want)
	}
	receiver.Accept(in, t.TempDir())
	if v := waitForEnd(t, out); v.Status != transfer.Complete {
		t.Fatalf("sender ended %+v", v)
	}
}

// TestDeviceInTheMiddleChangesTheCode puts a device between sender and
// receiver that shows its own certificate to each and passes everything
// else on, and checks the two codes differ.
func TestDeviceInTheMiddleChangesTheCode(t *testing.T) {
	receiver, host, port, _ := startRecorded(t)
	middle := openTrust(t)
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "https", Host: net.JoinHostPort(host, strconv.Itoa(port))})
	proxy.Transport = lokaClient(middle).Transport
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, secure := network.Split(ln, middle.ServerConfig())
	srv := &http.Server{Handler: proxy}
	go srv.Serve(secure)
	t.Cleanup(func() { srv.Close() })

	out := sendFrom(t, openTrust(t), []string{oneFile(t)}, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	in := waitForIncomingStatus(t, receiver, transfer.Waiting)
	if got, other := out.View().Code, in.View().Code; got == "" || got == other {
		t.Fatalf("sender shows %q, receiver %q", got, other)
	}
	out.Stop()
}

// TestRevealMustMatchTheOffer has a Loka reveal another nonce than it
// committed to, and checks the offer can't be accepted and then stops.
func TestRevealMustMatchTheOffer(t *testing.T) {
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	reply := offerFrom(t, client, base, trust.Nonce())
	in := receiver.Transfers.Get(reply.ID)
	if in.Status() != transfer.Pairing || receiver.Accept(in, t.TempDir()) {
		t.Fatalf("an offer without a code could be accepted: %+v", in.View())
	}
	if err := reveal(t, client, base, reply, trust.Nonce()); err == nil || err.Error() != errCodeMismatch.Error() {
		t.Fatalf("reveal answered %v", err)
	}
	if v := in.View(); v.Status != transfer.Failed || v.Code != "" {
		t.Fatalf("offer %+v", v)
	}
}

// TestDeclineWhilePairing declines an offer before its sender reveals,
// and checks the sender hears it was declined.
func TestDeclineWhilePairing(t *testing.T) {
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	nonce := trust.Nonce()
	reply := offerFrom(t, client, base, nonce)
	receiver.Transfers.Get(reply.ID).Stop()
	if err := reveal(t, client, base, reply, nonce); err == nil || err.Error() != errDeclined.Error() {
		t.Fatalf("reveal answered %v", err)
	}
}

// TestOldSenderIsToldToUpdate has a Loka offer without a commitment, as
// 0.2.0 does, and checks it is told to update and nothing is shown.
func TestOldSenderIsToldToUpdate(t *testing.T) {
	receiver, base := startServer(t)
	o := offer{Name: "laptop", Items: []string{"f"}, Size: 1}
	err := call(t.Context(), lokaClient(openTrust(t)), http.MethodPost, secure(base)+"/peer/offer", "", o, nil)
	if err == nil || err.Error() != errOldLoka.Error() {
		t.Fatalf("offer answered %v", err)
	}
	if n := len(receiver.Transfers.All()); n != 0 {
		t.Fatalf("%d offers shown", n)
	}
}

// TestOldReceiverIsCancelled has the receiver answer without a nonce, as
// 0.2.0 does, and checks the sender stops and cancels the offer there.
func TestOldReceiverIsCancelled(t *testing.T) {
	receiver := New("receiver", "test")
	receiver.Trust = openTrust(t)
	withoutNonce := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/peer/offer" {
				h.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			var reply offerReply
			json.Unmarshal(rec.Body.Bytes(), &reply)
			reply.Nonce = ""
			var body bytes.Buffer
			json.NewEncoder(&body).Encode(reply)
			w.WriteHeader(rec.Code)
			w.Write(body.Bytes())
		})
	}
	host, port := hostPort(t, serve(t, receiver, withoutNonce))
	v := waitForEnd(t, sendFrom(t, openTrust(t), []string{oneFile(t)}, host, port))
	if v.Status != transfer.Failed || v.Error != "The receiver runs an older Loka. Update Loka on it" {
		t.Fatalf("sender ended %+v", v)
	}
	if in := waitForEnd(t, waitForIncoming(t, receiver)); in.Status != transfer.Cancelled {
		t.Fatalf("receiver shows %+v", in)
	}
}

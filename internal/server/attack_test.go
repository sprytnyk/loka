package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

// Attacks on pairing. Each test checks for the safe outcome, so a failure
// means the attack worked.

// TestBrowserNeedsReceiveToken offers from a browser without the receive
// token, and with wrong ones, and checks each is refused.
func TestBrowserNeedsReceiveToken(t *testing.T) {
	receiver, base := startServer(t)
	for _, token := range []string{"", transfer.RandomToken(18), receiver.ReceiveToken + "x"} {
		o := offer{Items: []string{"f"}, Size: 1, Token: token}
		err := call(t.Context(), browser(receiver), http.MethodPost, secure(base)+"/peer/offer", "", o, nil)
		if err == nil || err.Error() != errNeedsReceiveToken.Error() {
			t.Errorf("token %q answered %v", token, err)
		}
	}
	if n := len(receiver.Transfers.All()); n != 0 {
		t.Fatalf("the receiver keeps %d transfers", n)
	}
}

// TestPagesElsewhereHideReceiveToken checks the token is only on this
// device's own page, not on the page another device gets.
func TestPagesElsewhereHideReceiveToken(t *testing.T) {
	receiver, base := startServer(t)
	for _, url := range []string{base, secure(base)} {
		req, _ := http.NewRequest(http.MethodGet, url+"/", nil)
		req.Host = "192.0.2.10:45873" // as another device asks
		resp, err := browser(receiver).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), receiver.ReceiveToken) {
			t.Errorf("%s shows the receive token to another device", url)
		}
	}
}

// TestPairingFloodCantLockOutRememberedDevices offers from new Lokas that
// never reveal, then from browsers, and checks those together get only
// the places not kept for remembered devices, and a remembered device
// still gets through.
func TestPairingFloodCantLockOutRememberedDevices(t *testing.T) {
	receiver, base := startServer(t)
	for i := range pairingTries {
		if _, err := tryOffer(t, lokaClient(openTrust(t)), base, trust.Nonce()); err != nil {
			t.Fatalf("new Loka offer %d: %v", i, err)
		}
	}
	o := offer{Items: []string{"f"}, Size: 1, Token: receiver.ReceiveToken}
	room := maxOffers - keptForKnown - pairingTries // places left for browsers
	for i := range maxOffers - pairingTries {
		err := call(t.Context(), browser(receiver), http.MethodPost, secure(base)+"/peer/offer", "", o, nil)
		switch {
		case i < room && err != nil:
			t.Fatalf("browser offer %d: %v", i, err)
		case i >= room && (err == nil || err.Error() != errTooManyOffers.Error()):
			t.Fatalf("browser offer %d, past the %d places for unknown senders, answered %v", i, maxOffers-keptForKnown, err)
		}
	}
	friend := rememberedClient(t, receiver, "desktop")
	if _, err := tryOffer(t, friend, base, trust.Nonce()); err != nil {
		t.Errorf("a remembered device was locked out: %v", err)
	}
}

// TestParallelOffersCantBeatTheTryLimit sends many offers from new Lokas
// at once and checks no more than the tries get through.
func TestParallelOffersCantBeatTheTryLimit(t *testing.T) {
	_, base := startServer(t)
	var wg sync.WaitGroup
	var ok atomic.Int32
	clients := make([]*http.Client, 40)
	for i := range clients {
		clients[i] = lokaClient(openTrust(t))
	}
	for _, c := range clients {
		wg.Go(func() {
			if _, err := tryOffer(t, c, base, trust.Nonce()); err == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if n := ok.Load(); n > pairingTries {
		t.Fatalf("%d offers got through, the limit is %d", n, pairingTries)
	}
}

// TestRevealTwiceKeepsTheCode reveals a second time and checks the code
// and status don't change.
func TestRevealTwiceKeepsTheCode(t *testing.T) {
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	nonce := trust.Nonce()
	reply := offerFrom(t, client, base, nonce)
	if err := reveal(t, client, base, reply, nonce); err != nil {
		t.Fatal(err)
	}
	in := receiver.Transfers.Get(reply.ID)
	code := in.View().Code
	if err := reveal(t, client, base, reply, nonce); err == nil {
		t.Error("a second reveal was taken")
	}
	if err := reveal(t, client, base, reply, trust.Nonce()); err == nil {
		t.Error("a second reveal with another nonce was taken")
	}
	if v := in.View(); v.Code != code {
		t.Fatalf("code went from %q to %q", code, v.Code)
	}
}

// TestRevealNeedsTheSecret reveals with a wrong secret and checks the
// offer stays in pairing.
func TestRevealNeedsTheSecret(t *testing.T) {
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	nonce := trust.Nonce()
	reply := offerFrom(t, client, base, nonce)
	wrong := reply
	wrong.Secret = transfer.RandomToken(24)
	if err := reveal(t, client, base, wrong, nonce); err == nil {
		t.Fatal("a reveal with a wrong secret was taken")
	}
	if s := receiver.Transfers.Get(reply.ID).Status(); s != transfer.Pairing {
		t.Fatalf("status %s", s)
	}
}

// TestRevealFromAnotherLoka reveals with the right secret but from a Loka
// that didn't make the offer. The code is bound to the offer's
// certificate, so this must not change whose code it is.
func TestRevealFromAnotherLoka(t *testing.T) {
	receiver, base := startServer(t)
	sender := openTrust(t)
	nonce := trust.Nonce()
	reply := offerFrom(t, lokaClient(sender), base, nonce)
	if err := reveal(t, lokaClient(openTrust(t)), base, reply, nonce); err != nil {
		t.Fatal(err)
	}
	got := receiver.Transfers.Get(reply.ID).View().Code
	want := trust.Code(sender.Fingerprint(), receiver.Trust.Fingerprint(), nonce, reply.Nonce)
	if got != want {
		t.Fatalf("code %q, want the offering Loka's %q", got, want)
	}
}

// TestRevealAfterTimeout reveals after the pairing time ran out and
// checks the offer stays failed.
func TestRevealAfterTimeout(t *testing.T) {
	defer func(d time.Duration) { pairingTimeout = d }(pairingTimeout)
	pairingTimeout = 50 * time.Millisecond
	receiver, base := startServer(t)
	client := lokaClient(openTrust(t))
	nonce := trust.Nonce()
	reply := offerFrom(t, client, base, nonce)
	in := receiver.Transfers.Get(reply.ID)
	waitFor(t, "the offer to expire", func() bool { return in.Status() == transfer.Failed })
	if err := reveal(t, client, base, reply, nonce); err == nil {
		t.Error("a late reveal was taken")
	}
	if v := in.View(); v.Status != transfer.Failed || v.Code != "" || receiver.Accept(in, t.TempDir()) {
		t.Fatalf("offer %+v", v)
	}
}

// TestMiddlePosesAsBrowser puts a device between two Lokas. To the sender
// it is the receiver. To the receiver it is a browser, which shows no
// pairing code. It answers the sender's reveal itself and passes
// everything else on. It never saw the phone link, so the receiver must
// refuse it.
func TestMiddlePosesAsBrowser(t *testing.T) {
	receiver, host, port, _ := startRecorded(t)
	middle := openTrust(t)
	target := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, strconv.Itoa(port))}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = browser(receiver).Transport
	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	mux.HandleFunc("POST /peer/reveal/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, tlsLn := network.Split(ln, middle.ServerConfig())
	srv := &http.Server{Handler: mux}
	go srv.Serve(tlsLn)
	t.Cleanup(func() { srv.Close() })

	out := sendFrom(t, openTrust(t), []string{oneFile(t)}, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	waitFor(t, "the sender to end or the receiver to show an offer", func() bool {
		return out.Status().Final() || len(receiver.Transfers.All()) > 0
	})
	if n := len(receiver.Transfers.All()); n != 0 {
		out.Stop()
		t.Fatalf("the receiver shows %d offers from the device in the middle", n)
	}
	// Only the missing receive token should stop it, not some other failure.
	if v := out.View(); v.Status != transfer.Failed || v.Error != errNeedsReceiveToken.Error() {
		t.Errorf("sender ended %s, %q; want it refused for lacking the receive token", v.Status, v.Error)
	}
}

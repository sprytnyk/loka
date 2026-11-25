package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

// phoneText sends text the way the phone page does: an offer, then the
// text once the offer is accepted, which for text from a browser is at
// once. It returns the upload's error.
func phoneText(t *testing.T, receiver *Server, base, text string) error {
	t.Helper()
	client := browser(receiver)
	var reply offerReply
	o := offer{Kind: transfer.Text, Size: int64(len(text)), Token: receiver.ReceiveToken}
	if err := call(t.Context(), client, http.MethodPost, secure(base)+"/peer/offer", "", o, &reply); err != nil {
		return err
	}
	var status statusReply
	if err := call(t.Context(), client, http.MethodGet, secure(base)+"/peer/status/"+reply.ID, reply.Secret, nil, &status); err != nil {
		return err
	}
	if status.Status != transfer.Accepted {
		t.Fatalf("text from a browser is %q, not accepted", status.Status)
	}
	write := func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, text)
		return err
	}
	return upload(t.Context(), client, secure(base)+"/peer/upload/"+reply.ID, reply.Secret, write)
}

// readTextOf reads received text through the page API, and returns the
// HTTP status.
func readTextOf(t *testing.T, s *Server, base, id string) (string, int) {
	t.Helper()
	var reply struct{ Text string }
	code := api(t, s, base, "/api/text", map[string]string{"id": id}, &reply)
	return reply.Text, code
}

// sendText sends text from a Loka with identity store to the server at
// base, and returns the outgoing transfer.
func sendText(t *testing.T, store *trust.Store, base, text string) *transfer.Transfer {
	t.Helper()
	host, port := hostPort(t, base)
	out := transfer.NewText(transfer.Outgoing, host, text, transfer.Pairing)
	go func() { out.Finish(SendTo(out, nil, store, "laptop", host, port, "")) }()
	return out
}

func rememberTextPeer(t *testing.T, store, peer *trust.Store, name string) {
	t.Helper()
	if err := store.Remember(trust.Device{ID: peer.ID, Name: name, Fingerprint: peer.Fingerprint()}); err != nil {
		t.Fatal(err)
	}
}

// TestPhoneSendsText checks text from a phone shows on the page without
// being accepted, stays until read, and is gone once closed.
func TestPhoneSendsText(t *testing.T) {
	receiver, base := startServer(t)
	text := "\uFEFF  Wi-Fi: κωδικός 🔑\r\nline two\rline three  \n"
	if err := phoneText(t, receiver, base, text); err != nil {
		t.Fatal(err)
	}
	in := receiver.Transfers.All()[0]
	if v := in.View(); v.Status != transfer.Complete || !v.Unread || v.Kind != transfer.Text || v.Preview != "" {
		t.Fatalf("received text shows %+v", v)
	}
	if got, code := readTextOf(t, receiver, base, in.ID); code != http.StatusOK || got != text {
		t.Fatalf("the page reads %q (%d)", got, code)
	}
	if code := api(t, receiver, base, "/api/dismiss", map[string]string{"id": in.ID}, nil); code != http.StatusOK {
		t.Fatalf("dismiss answered %d", code)
	}
	if got, code := readTextOf(t, receiver, base, in.ID); code != http.StatusGone || got != "" {
		t.Fatalf("closed text still reads %q (%d)", got, code)
	}
	if in.Text() != "" {
		t.Fatal("closed text stays in memory")
	}
}

// TestTextFromRememberedLokaNeedsNoAnswer sends text between two Lokas
// that know each other, and checks it arrives without the person
// accepting it.
func TestTextFromRememberedLokaNeedsNoAnswer(t *testing.T) {
	receiver, base := startServer(t)
	store := openTrust(t)
	rememberTextPeer(t, receiver.Trust, store, "laptop")
	rememberTextPeer(t, store, receiver.Trust, "receiver")
	out := sendText(t, store, base, "hello")
	if v := waitForEnd(t, out); v.Status != transfer.Complete {
		t.Fatalf("sender ended %+v", v)
	}
	if out.Text() != "" {
		t.Fatal("the sender keeps sent text in memory")
	}
	in := waitForIncoming(t, receiver)
	if got, _ := readTextOf(t, receiver, base, in.ID); got != "hello" {
		t.Fatalf("received %q", got)
	}
}

func TestTextWithOneSidedTrustWaitsForAccept(t *testing.T) {
	for _, side := range []string{"sender", "receiver"} {
		t.Run(side+" remembers", func(t *testing.T) {
			receiver, base := startServer(t)
			sender := openTrust(t)
			if side == "sender" {
				rememberTextPeer(t, sender, receiver.Trust, "receiver")
			} else {
				rememberTextPeer(t, receiver.Trust, sender, "laptop")
			}
			out := sendText(t, sender, base, "hello")
			t.Cleanup(out.Stop)
			in := waitForIncomingStatus(t, receiver, transfer.Waiting)
			if got, want := out.View().Code, in.View().Code; got == "" || got != want {
				t.Fatalf("sender shows %q, receiver shows %q", got, want)
			}
			if _, code := readTextOf(t, receiver, base, in.ID); code != http.StatusGone {
				t.Fatalf("reading before acceptance answered %d", code)
			}
			if code := api(t, receiver, base, "/api/accept", map[string]string{"id": in.ID}, nil); code != http.StatusOK {
				t.Fatalf("accept answered %d", code)
			}
			if v := waitForEnd(t, out); v.Status != transfer.Complete {
				t.Fatalf("sender ended %+v", v)
			}
			if got, _ := readTextOf(t, receiver, base, in.ID); got != "hello" {
				t.Fatalf("received %q", got)
			}
		})
	}
}

// TestTextFromNewLokaWaitsForAccept checks a Loka not seen before must be
// accepted, with its pairing code, before its text shows, and that
// accepting needs no destination.
func TestTextFromNewLokaWaitsForAccept(t *testing.T) {
	receiver, base := startServer(t)
	out := sendText(t, openTrust(t), base, "hello")
	in := waitForIncoming(t, receiver)
	if v := in.View(); v.Status != transfer.Waiting || !v.NewDevice || v.Code == "" {
		t.Fatalf("text from a new Loka shows %+v", v)
	}
	if code := api(t, receiver, base, "/api/accept", map[string]string{"id": in.ID}, nil); code != http.StatusOK {
		t.Fatalf("accept answered %d", code)
	}
	if v := waitForEnd(t, out); v.Status != transfer.Complete {
		t.Fatalf("sender ended %+v", v)
	}
	if got, _ := readTextOf(t, receiver, base, in.ID); got != "hello" {
		t.Fatalf("received %q", got)
	}
	var st state
	api(t, receiver, base, "/api/state", nil, &st)
	if st.Destination != "" {
		t.Fatalf("text set the destination to %q", st.Destination)
	}
}

// TestTextBetweenPages sends text from one page to another, through the
// API the page uses.
func TestTextBetweenPages(t *testing.T) {
	sender, senderURL := startServer(t)
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	var sent struct{ ID string }
	if code := api(t, sender, senderURL, "/api/send", map[string]any{"text": "from the page", "host": host, "port": port}, &sent); code != http.StatusOK {
		t.Fatalf("send answered %d", code)
	}
	out := sender.Transfers.Get(sent.ID)
	if v := out.View(); v.Kind != transfer.Text || v.Preview != "from the page" {
		t.Fatalf("the sender shows %+v", v)
	}
	in := waitForIncomingStatus(t, receiver, transfer.Waiting)
	if code := api(t, receiver, receiverURL, "/api/accept", map[string]string{"id": in.ID}, nil); code != http.StatusOK {
		t.Fatalf("accept answered %d", code)
	}
	if v := waitForEnd(t, out); v.Status != transfer.Complete {
		t.Fatalf("sender ended %+v", v)
	}
	if got, _ := readTextOf(t, receiver, receiverURL, in.ID); got != "from the page" {
		t.Fatalf("received %q", got)
	}
}

// TestTextLink shares text by link, reads it as the phone page does, and
// checks it stops with the link.
func TestTextLink(t *testing.T) {
	s, base := startServer(t)
	var shared struct{ ID, Token string }
	text := "\uFEFF<b>not html</b>\r\n extra space  \r"
	if code := api(t, s, base, "/api/share", map[string]string{"text": text}, &shared); code != http.StatusOK {
		t.Fatalf("share answered %d", code)
	}
	resp, err := browser(s).Get(secure(base) + "/download/" + shared.Token)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != text || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("the link serves %q as %q", body, resp.Header.Get("Content-Type"))
	}
	api(t, s, base, "/api/cancel", map[string]string{"id": shared.ID}, nil)
	if s.Transfers.Get(shared.ID).Text() != "" {
		t.Fatal("a stopped link keeps its text in memory")
	}
	resp, err = browser(s).Get(secure(base) + "/download/" + shared.Token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a stopped link answered %d", resp.StatusCode)
	}
}

type cancelAtEOF struct {
	io.Reader
	cancel func()
}

func (r cancelAtEOF) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.cancel()
	}
	return n, err
}

func TestCancelTextAtEndOfUpload(t *testing.T) {
	in := transfer.New(transfer.Incoming, "phone", nil, 5, transfer.Receiving)
	in.Kind = transfer.Text
	body := cancelAtEOF{Reader: strings.NewReader("hello"), cancel: in.Stop}
	req := httptest.NewRequest(http.MethodPost, "/peer/upload/test", body)
	resp := httptest.NewRecorder()
	receiveText(resp, req, in)
	if v := in.View(); resp.Code != http.StatusConflict || v.Status != transfer.Cancelled || v.Unread || in.Text() != "" {
		t.Fatalf("cancelled text answered %d and shows %+v", resp.Code, v)
	}
}

// TestTextLimits checks text past the limit, empty text and text that
// names items are refused.
func TestTextLimits(t *testing.T) {
	s, base := startServer(t)
	long := strings.Repeat("a", transfer.MaxText+1)
	for _, path := range []string{"/api/send", "/api/share"} {
		var reply struct{ Error string }
		api(t, s, base, path, map[string]any{"text": long, "host": "192.0.2.10"}, &reply)
		if reply.Error != errTextTooLong.Error() {
			t.Errorf("%s answered %q", path, reply.Error)
		}
	}
	var full struct{ ID string }
	if code := api(t, s, base, "/api/share", map[string]string{"text": strings.Repeat("\n", transfer.MaxText)}, &full); code != http.StatusOK {
		t.Fatalf("sharing text at the limit answered %d", code)
	}
	api(t, s, base, "/api/cancel", map[string]string{"id": full.ID}, nil)
	s.Transfers = transfer.Registry{}
	for _, o := range []offer{
		{Kind: transfer.Text, Size: transfer.MaxText + 1},
		{Kind: transfer.Text, Size: 0},
		{Kind: transfer.Text, Size: 1, Items: []string{"f"}},
		{Kind: "video", Size: 1, Items: []string{"f"}},
	} {
		o.Token = s.ReceiveToken
		err := call(t.Context(), browser(s), http.MethodPost, secure(base)+"/peer/offer", "", o, nil)
		if err == nil {
			t.Errorf("offer %+v was taken", o)
		}
	}
	if n := len(s.Transfers.All()); n != 0 {
		t.Fatalf("the receiver keeps %d transfers", n)
	}
}

// TestTextMustMatchItsOffer sends more and other text than offered.
func TestTextMustMatchItsOffer(t *testing.T) {
	receiver, base := startServer(t)
	client := browser(receiver)
	for _, body := range []string{"longer than offered", "\xff\xfe"} {
		var reply offerReply
		o := offer{Kind: transfer.Text, Size: 2, Token: receiver.ReceiveToken}
		if err := call(t.Context(), client, http.MethodPost, secure(base)+"/peer/offer", "", o, &reply); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, secure(base)+"/peer/upload/"+reply.ID, bytes.NewReader([]byte(body)))
		req.Header.Set("X-Transfer-Secret", reply.Secret)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		in := receiver.Transfers.Get(reply.ID)
		if v := in.View(); resp.StatusCode != http.StatusBadRequest || v.Status != transfer.Failed || v.Unread {
			t.Errorf("%q answered %d and shows %+v", body, resp.StatusCode, v)
		}
	}
}

// TestUnreadTextTakesAnOfferPlace checks text left unread counts as
// waiting, so text that keeps arriving can't fill the memory.
func TestUnreadTextTakesAnOfferPlace(t *testing.T) {
	receiver, base := startServer(t)
	for i := range maxOffers - keptForKnown {
		if err := phoneText(t, receiver, base, "spam"); err != nil {
			t.Fatalf("text %d: %v", i, err)
		}
	}
	if err := phoneText(t, receiver, base, "spam"); err == nil || err.Error() != errTooManyOffers.Error() {
		t.Fatalf("text past the limit answered %v", err)
	}
	first := receiver.Transfers.All()[0]
	api(t, receiver, base, "/api/dismiss", map[string]string{"id": first.ID}, nil)
	if err := phoneText(t, receiver, base, "after reading one"); err != nil {
		t.Fatalf("text after reading one: %v", err)
	}
}

// TestTerminalRefusesText checks loka receive, which can't show text,
// says so to the sender.
func TestTerminalRefusesText(t *testing.T) {
	receiver, base := startServer(t)
	receiver.NoText = true
	out := sendText(t, openTrust(t), base, "hello")
	if v := waitForEnd(t, out); v.Status != transfer.Failed || v.Error != errNoText.Error() {
		t.Fatalf("sender ended %+v", v)
	}
}

// TestOldReceiverRefusesText checks the sender explains what a Loka from
// before text answers.
func TestOldReceiverRefusesText(t *testing.T) {
	receiver := New("receiver", "test")
	receiver.Trust = openTrust(t)
	base := serve(t, receiver, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/peer/offer" {
				writeError(w, http.StatusBadRequest, errInvalidOffer)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	out := sendText(t, openTrust(t), base, "hello")
	if v := waitForEnd(t, out); v.Status != transfer.Failed || v.Error != errOldReceiver.Error() {
		t.Fatalf("sender ended %+v", v)
	}
}

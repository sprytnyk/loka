// Package server answers the Loka page on this device and other devices on
// the network, and sends to other devices.
package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sprytnyk/loka/internal/desktop"
	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

// Server answers the page on this device and other devices on the network.
type Server struct {
	name      string // shown to other devices
	version   string
	key       string // proves a request comes from this device's page
	localName func(string) string
	pick      func(kind desktop.Kind) ([]string, error)
	temp      string // holds files dropped on the page, made when needed

	// ReceiveToken is in the phone link, after #r=. A browser's offer
	// must carry it, so only someone shown the link can offer from one.
	ReceiveToken string

	Transfers   transfer.Registry
	Trust       *trust.Store           // this device's certificates
	OnlyLinks   bool                   // serve only download links, as loka share does
	NoText      bool                   // refuse text, which nothing here can show
	Devices     *network.Discovery     // nil when not looking for devices
	destination atomic.Pointer[string] // the last accepted destination

	offerMu  sync.Mutex // makes counting and adding an offer one step
	pairings pairingLimits
}

// New makes a server that names this device name to other devices.
func New(name, version string) *Server {
	return &Server{
		name:         name,
		version:      version,
		key:          transfer.RandomToken(24),
		ReceiveToken: transfer.RandomToken(18),
		localName:    transfer.LocalName,
		pick:         desktop.Pick,
		temp:         filepath.Join(os.TempDir(), "loka-"+transfer.RandomToken(9)),
	}
}

// Close removes the files dropped on the page.
func (s *Server) Close() {
	os.RemoveAll(s.temp)
}

// Serve answers on ln until ctx is done, over TLS and plain HTTP on the
// same port.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(quietHandshakes{os.Stderr}, "", log.LstdFlags),
		ConnState:         network.ConnState,
		ConnContext:       network.ConnContext,
	}
	plain, secure := network.Split(ln, s.Trust.ServerConfig())
	listeners := []net.Listener{plain, secure}
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		srv.Close()
	})
	defer stop()
	ended := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() { ended <- srv.Serve(l) }()
	}
	var first error
	for range listeners {
		if err := <-ended; !errors.Is(err, http.ErrServerClosed) && first == nil {
			first = err
			srv.Close() // stop the other listener too
		}
	}
	return first
}

// quietHandshakes drops log lines about failed TLS handshakes. A browser
// that doesn't trust this device yet makes one on each visit, as the page
// checks whether it can move to HTTPS.
type quietHandshakes struct{ w io.Writer }

func (q quietHandshakes) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}
	return q.w.Write(p)
}

// CAPath is where browsers download this device's certificate authority.
const CAPath = "/loka-ca.crt"

// Handler answers the page, its API, other Lokas and browsers. Browsers
// on other devices must use HTTPS: over plain HTTP they get only the page
// that moves them to it and the certificate authority to install. With
// OnlyLinks, as for loka share, nobody can accept a transfer, so only the
// links are served.
func (s *Server) Handler() http.Handler {
	plain := http.NewServeMux()
	plain.HandleFunc("GET /{$}", s.index)
	plain.HandleFunc("GET "+CAPath, s.caCertificate)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET "+CAPath, s.caCertificate)
	mux.HandleFunc("GET /download/{token}", s.download)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil && !fromThisDevice(r) {
			plain.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	if s.OnlyLinks {
		return handler
	}
	plain.HandleFunc("POST /peer/offer", s.offer) // to tell an old Loka why it can't send
	mux.HandleFunc("POST /peer/offer", s.offer)
	mux.HandleFunc("POST /peer/reveal/{id}", s.peer(s.peerReveal))
	mux.HandleFunc("GET /peer/status/{id}", s.peer(s.peerStatus))
	mux.HandleFunc("POST /peer/cancel/{id}", s.peer(s.peerCancel))
	mux.HandleFunc("POST /peer/upload/{id}", s.peer(s.peerUpload))

	mux.HandleFunc("GET /api/state", s.local(s.state))
	mux.HandleFunc("POST /api/pick", s.local(s.pickPaths))
	mux.HandleFunc("POST /api/upload", s.local(s.upload))
	mux.HandleFunc("POST /api/send", s.local(s.send))
	mux.HandleFunc("POST /api/accept", s.local(s.accept))
	mux.HandleFunc("POST /api/cancel", s.local(s.cancel))
	mux.HandleFunc("POST /api/text", s.local(s.readText))
	mux.HandleFunc("POST /api/dismiss", s.local(s.dismiss))
	mux.HandleFunc("POST /api/share", s.local(s.share))
	mux.HandleFunc("POST /api/qr", s.local(s.qrCode))
	return handler
}

// fromThisDevice reports whether a request comes from this device, under a
// local host name. Checking the host name stops other web sites from
// reaching Loka through a DNS name that points at 127.0.0.1.
func fromThisDevice(r *http.Request) bool {
	client, _, _ := net.SplitHostPort(r.RemoteAddr)
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	ip := net.ParseIP(client)
	return ip != nil && ip.IsLoopback() && (host == "localhost" || net.ParseIP(strings.Trim(host, "[]")).IsLoopback())
}

// local lets through only requests from this device's page.
func (s *Server) local(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Loka-Key")
		if !fromThisDevice(r) || subtle.ConstantTimeCompare([]byte(key), []byte(s.key)) != 1 {
			writeError(w, http.StatusForbidden, errors.New("Local access only"))
			return
		}
		h(w, r)
	}
}

// peer finds the incoming transfer named in the path and lets through only
// the device that offered it.
func (s *Server) peer(h func(http.ResponseWriter, *http.Request, *transfer.Transfer)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := s.Transfers.Get(r.PathValue("id"))
		secret := r.Header.Get("X-Transfer-Secret")
		if t == nil || t.Direction != transfer.Incoming || subtle.ConstantTimeCompare([]byte(secret), []byte(t.Secret)) != 1 {
			writeError(w, http.StatusNotFound, errors.New("Not found"))
			return
		}
		h(w, r, t)
	}
}

// offer is what a sender tells the receiver before anything is sent.
type offer struct {
	Name       string        `json:"name"`
	Kind       transfer.Kind `json:"kind,omitempty"` // text, or files when empty
	Items      []string      `json:"items"`          // none for text
	Size       int64         `json:"size"`
	Commitment string        `json:"commitment,omitempty"` // to the sender's nonce, from a Loka
	Token      string        `json:"token,omitempty"`      // the receive token, from a browser
}

type offerReply struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
	Name   string `json:"name"`            // the receiver's, so the sender can remember it
	Nonce  string `json:"nonce,omitempty"` // the receiver's, to a Loka
}

// errNeedsHTTPS refuses an offer over plain HTTP, which nothing would
// encrypt.
var errNeedsHTTPS = errors.New("Loka takes offers only over HTTPS")

// errDeclined is how a sender learns that the person at the receiver
// declined or cancelled, whichever request it was making.
var errDeclined = errors.New("The receiver declined or cancelled")

// errNeedsReceiveToken refuses a browser's offer without the receive
// token. Without it, a device between two Lokas could pose as a browser,
// which shows no pairing code.
var errNeedsReceiveToken = errors.New("To send from a browser, open the link or QR code the receiver shows")

// errTextTooLong refuses text past transfer.MaxText.
var errTextTooLong = errors.New("The text is longer than 1 MB. Send it as a file instead")

// errNoText refuses text where nothing can show it.
var errNoText = errors.New("The receiver runs loka receive, which can't show text. Run loka there instead")

// errTextLate ends a text transfer whose sender didn't send the text after
// it was accepted.
var errTextLate = errors.New("The sender didn't send the text in time")

// errTooManyOffers refuses an offer while many others wait for an answer.
var errTooManyOffers = errors.New("The receiver has too many offers waiting. Try again later")

// errPairingLimit refuses a Loka while the pairing budget is spent.
var errPairingLimit = errors.New("The receiver refuses Lokas it hasn't seen before for a minute, as several didn't finish pairing. Try again later")

// errPairingLimitKnown refuses a remembered Loka while its own pairing
// budget is spent.
var errPairingLimitKnown = errors.New("The receiver refuses this device for a minute, as it started pairing several times and didn't finish. Try again later")

// errPairingTimeout ends an offer whose sender didn't reveal its nonce.
var errPairingTimeout = errors.New("The sender didn't finish pairing in time")

// errNoAnswer ends an offer nobody accepted or declined in time.
var errNoAnswer = errors.New("Nobody answered the offer within a minute")

// errCodeMismatch ends a transfer whose sender revealed a nonce other
// than the one it committed to.
var errCodeMismatch = errors.New("The pairing code didn't check out, so the transfer stopped")

func (s *Server) offer(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		writeError(w, http.StatusUpgradeRequired, errNeedsHTTPS)
		return
	}
	var o offer
	if err := readJSON(w, r, &o); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.checkOffer(o); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	items := o.Items[:min(len(o.Items), transfer.MaxOfferItems)]
	for i := range items {
		items[i] = truncate(items[i], 255)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if len(r.TLS.PeerCertificates) == 0 {
		// A browser: it can't show which device it is.
		if subtle.ConstantTimeCompare([]byte(o.Token), []byte(s.ReceiveToken)) != 1 {
			writeError(w, http.StatusForbidden, errNeedsReceiveToken)
			return
		}
		// Text needs no answer: it waits on the page until read, and
		// the phone link already says who may send.
		status := transfer.Waiting
		if o.Kind == transfer.Text {
			status = transfer.Accepted
		}
		t := transfer.New(transfer.Incoming, host, items, o.Size, status)
		t.Kind, t.Browser = o.Kind, true
		s.addOffer(w, t)
		return
	}
	if len(o.Commitment) != trust.CommitmentSize {
		writeError(w, http.StatusBadRequest, errInvalidOffer)
		return
	}
	t := transfer.New(transfer.Incoming, cmp.Or(truncate(o.Name, 255), host), items, o.Size, transfer.Pairing)
	t.Kind = o.Kind
	t.DeviceID, t.Fingerprint = trust.Identify(r.TLS.PeerCertificates[0])
	t.Commitment, t.Nonce = o.Commitment, trust.Nonce()
	standing, known, err := s.Trust.Check(t.DeviceID, t.Fingerprint)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if standing == trust.Changed {
		// Show the refusal under the name the device is remembered by: the
		// name it gave is not proven. Show it once per device, as anyone
		// can make one.
		s.refuseChanged(t, known)
		writeError(w, http.StatusForbidden, errors.New("The receiver remembers this device with a different certificate and refused the transfer"))
		return
	}
	if standing == trust.New {
		t.MarkNewDevice()
	}
	s.addOffer(w, t)
}

// errInvalidOffer refuses an offer that doesn't say what it carries. A
// Loka before text answers it to an offer of text.
var errInvalidOffer = errors.New("Invalid offer")

// checkOffer refuses an offer that doesn't say what it carries, and text
// this device can't take.
func (s *Server) checkOffer(o offer) error {
	switch o.Kind {
	case transfer.Files:
		if o.Size < 0 || len(o.Items) == 0 {
			return errInvalidOffer
		}
	case transfer.Text:
		switch {
		case len(o.Items) != 0 || o.Size <= 0:
			return errInvalidOffer
		case o.Size > transfer.MaxText:
			return errTextTooLong
		case s.NoText:
			return errNoText
		}
	default:
		return errInvalidOffer
	}
	return nil
}

// refuseChanged shows that a device posing as a remembered one was
// refused, unless that is already shown.
func (s *Server) refuseChanged(t *transfer.Transfer, known trust.Device) {
	refusal := &trust.ChangedError{Device: known}
	s.offerMu.Lock()
	defer s.offerMu.Unlock()
	for _, other := range s.Transfers.All() {
		if other.Direction == transfer.Incoming && other.DeviceID == known.ID && other.Status() == transfer.Failed && other.View().Error == refusal.Error() {
			return
		}
	}
	t.Peer = known.Name
	s.Transfers.Add(t)
	t.Finish(refusal)
}

// maxOffers is how many incoming offers may wait at once, in pairing or
// for the person's answer.
const maxOffers = 16

// keptForKnown is how many of those places only remembered Lokas may
// take. Anyone can offer as a new Loka, and anyone with the phone link from
// a browser, so a flood of those can't lock out a remembered device.
const keptForKnown = 4

// waitsHere reports whether an incoming transfer takes one of those
// places: an offer in pairing or waiting for the person, text waiting for
// its sender, or text waiting to be read.
func waitsHere(t *transfer.Transfer) bool {
	if t.Direction != transfer.Incoming {
		return false
	}
	switch t.Status() {
	case transfer.Pairing, transfer.Waiting:
		return true
	case transfer.Accepted, transfer.Receiving:
		return t.Kind == transfer.Text
	}
	return t.Unread()
}

// fromUnknown reports whether t comes from a browser or a new Loka, which
// may not take the places kept for remembered Lokas.
func fromUnknown(t *transfer.Transfer) bool {
	return t.Browser || t.NewDevice()
}

// answerTimeout is how long an offer waits for the person to answer.
// Anyone on the network can offer from a browser, so offers left waiting
// would soon block all others.
var answerTimeout = time.Minute

// waitForPerson fails t if nobody answers it in time.
func waitForPerson(t *transfer.Transfer) {
	time.AfterFunc(answerTimeout, func() { t.Expire(transfer.Waiting, errNoAnswer) })
}

// waitForText fails accepted text that its sender doesn't send in time.
func waitForText(t *transfer.Transfer) {
	time.AfterFunc(answerTimeout, func() { t.Expire(transfer.Accepted, errTextLate) })
}

// pairingTimeout is how long a Loka has to reveal its nonce after the
// receiver answers its offer. A Loka reveals at once.
var pairingTimeout = 10 * time.Second

// addOffer adds an incoming offer, unless too many others wait. An offer
// from a Loka spends a pairing try and fails if its sender doesn't reveal
// in time. Any offer fails if nobody answers it in time.
func (s *Server) addOffer(w http.ResponseWriter, t *transfer.Transfer) {
	s.offerMu.Lock()
	defer s.offerMu.Unlock()
	waiting, unknown := 0, 0
	for _, other := range s.Transfers.All() {
		if waitsHere(other) {
			waiting++
			if fromUnknown(other) {
				unknown++
			}
		}
	}
	if waiting >= maxOffers || fromUnknown(t) && unknown >= maxOffers-keptForKnown {
		writeError(w, http.StatusServiceUnavailable, errTooManyOffers)
		return
	}
	switch t.Status() {
	case transfer.Pairing:
		if !s.pairings.take(t, time.Now()) {
			err := errPairingLimitKnown
			if t.NewDevice() {
				err = errPairingLimit
			}
			writeError(w, http.StatusTooManyRequests, err)
			return
		}
		time.AfterFunc(pairingTimeout, func() { t.Expire(transfer.Pairing, errPairingTimeout) })
	case transfer.Accepted:
		waitForText(t)
	default:
		waitForPerson(t)
	}
	t.Secret = transfer.RandomToken(24)
	s.Transfers.Add(t)
	writeJSON(w, http.StatusOK, offerReply{t.ID, t.Secret, s.name, t.Nonce})
}

// Pairing limits. A device in the middle can work out the receiver's
// code from each answer to an offer, and reveal only the offer whose code
// matches the sender's. So each offer from a Loka spends a try, and only
// the person accepting it gives the try back. Tries also come back slowly
// on their own. New devices share one budget, as anyone can make a new
// one. Each remembered device has its own, so a flood of offers from
// others can't hold it off, and it can't keep trying either.
const pairingTries = 10

var pairingRefill = 30 * time.Second

// warnFor is how long the page warns after a budget ran out.
const warnFor = 10 * time.Minute

type pairingLimits struct {
	mu      sync.Mutex
	budgets map[string]*pairingBudget // by device ID, "" for new devices
}

type pairingBudget struct {
	used      int       // tries spent and not yet back
	since     time.Time // when the last try came back on its own
	exhausted time.Time // when an offer last found no try left
}

// budgetFor returns the key of the budget an offer from t spends.
func budgetFor(t *transfer.Transfer) string {
	if t.NewDevice() {
		return ""
	}
	return t.DeviceID
}

func (l *pairingLimits) take(t *transfer.Transfer, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := budgetFor(t)
	b := l.budgets[key]
	if b == nil {
		if l.budgets == nil {
			l.budgets = map[string]*pairingBudget{}
		}
		b = &pairingBudget{}
		l.budgets[key] = b
	}
	return b.take(now)
}

func (l *pairingLimits) giveBack(t *transfer.Transfer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.budgets[budgetFor(t)]; b != nil {
		b.used = max(b.used-1, 0)
	}
}

func (b *pairingBudget) take(now time.Time) bool {
	for b.used > 0 && now.Sub(b.since) >= pairingRefill {
		b.used--
		b.since = b.since.Add(pairingRefill)
	}
	if b.used >= pairingTries {
		b.exhausted = now
		return false
	}
	if b.used == 0 {
		b.since = now
	}
	b.used++
	return true
}

// exhausted returns the key of a budget that ran out lately, the new
// devices' one first, and whether there is one.
func (l *pairingLimits) exhausted(now time.Time) (key string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.budgets {
		if !b.exhausted.IsZero() && now.Sub(b.exhausted) < warnFor && (!ok || k < key) {
			key, ok = k, true
		}
	}
	return key, ok
}

// errPairingWarning tells the person why new Lokas are refused for a
// while.
var errPairingWarning = errors.New("Several Lokas started pairing here and didn't finish. Another device may be trying to sit between two Lokas, so Lokas not seen before wait for a minute")

// pairingWarning tells the person why a remembered device is refused for
// a while.
func pairingWarning(name string) string {
	return fmt.Sprintf("%s started pairing here several times and didn't finish. It may be trying to sit between two Lokas, so it waits for a minute", name)
}

// Warning is what the person should know about this device, if anything.
func (s *Server) Warning() string {
	key, ok := s.pairings.exhausted(time.Now())
	if !ok {
		return ""
	}
	if key == "" {
		return errPairingWarning.Error()
	}
	devices, _ := s.Trust.Devices()
	for _, d := range devices {
		if d.ID == key {
			return pairingWarning(d.Name)
		}
	}
	return pairingWarning("A remembered device")
}

type revealRequest struct {
	Nonce         string `json:"nonce"`
	ReceiverKnown bool   `json:"receiver_known,omitempty"`
}

// peerReveal takes the nonce a Loka committed to in its offer, which
// makes the pairing code. Text can skip the prompt when both Lokas
// remember each other.
func (s *Server) peerReveal(w http.ResponseWriter, r *http.Request, t *transfer.Transfer) {
	var req revealRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if trust.Commit(req.Nonce) != t.Commitment {
		t.Finish(errCodeMismatch)
		writeError(w, http.StatusBadRequest, errCodeMismatch)
		return
	}
	if !t.Pair(trust.Code(t.Fingerprint, s.Trust.Fingerprint(), req.Nonce, t.Nonce)) {
		// The person declined while the sender was about to reveal.
		writeError(w, http.StatusConflict, errDeclined)
		return
	}
	if t.Kind == transfer.Text && req.ReceiverKnown && !t.NewDevice() {
		s.Accept(t, "")
	} else {
		waitForPerson(t)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// statusReply tells a sender where its offer stands, and why it failed.
type statusReply struct {
	Status transfer.Status `json:"status"`
	Error  string          `json:"error,omitempty"`
}

func (s *Server) peerStatus(w http.ResponseWriter, _ *http.Request, t *transfer.Transfer) {
	v := t.View()
	writeJSON(w, http.StatusOK, statusReply{v.Status, v.Error})
}

func (s *Server) peerCancel(w http.ResponseWriter, _ *http.Request, t *transfer.Transfer) {
	t.Stop()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) peerUpload(w http.ResponseWriter, r *http.Request, t *transfer.Transfer) {
	// When the upload stops early, answer at once and drop the connection,
	// rather than first reading the rest of the stream.
	w.Header().Set("Connection", "close")
	if !t.Move(transfer.Receiving, transfer.Accepted) {
		writeError(w, http.StatusConflict, errors.New("The transfer is not accepted"))
		return
	}
	network.Prove(r.Context())
	if t.Kind == transfer.Text {
		receiveText(w, r, t)
		return
	}
	// A cancel must also end a read that is waiting for bytes, as when the
	// sender has stalled.
	rc := http.NewResponseController(w)
	stop := context.AfterFunc(t.Context(), func() { rc.SetReadDeadline(time.Now()) })
	defer stop()
	// A sender that went away without a word must end it too.
	var stalled atomic.Bool
	stopWatch := transfer.WatchStall(&t.Done, func() {
		stalled.Store(true)
		rc.SetReadDeadline(time.Now())
	})
	defer stopWatch()
	err := transfer.Receive(t.Context(), r.Body, t.Destination(), t.Size, &t.Done, s.localName)
	if err != nil && stalled.Load() {
		err = transfer.ErrInterrupted
	}
	t.Finish(err)
	switch {
	case t.Context().Err() != nil:
		writeError(w, http.StatusConflict, errReceiverCancelled)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": transfer.Complete, "speed_mbps": t.View().SpeedMbps})
	}
}

// textTimeout is how long a sender has to send its text. Text is small,
// so a sender that takes longer has stalled.
var textTimeout = time.Minute

// errReceiverCancelled tells a sender the receiver cancelled while bytes
// were arriving.
var errReceiverCancelled = errors.New("The receiver cancelled the transfer")

// errTextMismatch ends a text transfer whose text isn't what was offered.
var errTextMismatch = errors.New("The text didn't match what the sender offered")

// receiveText keeps the text that arrives for t in memory, never on disk,
// until the person reads it.
func receiveText(w http.ResponseWriter, r *http.Request, t *transfer.Transfer) {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(textTimeout))
	stop := context.AfterFunc(t.Context(), func() { rc.SetReadDeadline(time.Now()) })
	defer stop()
	data, err := io.ReadAll(io.LimitReader(r.Body, t.Size+1))
	t.Done.Store(min(int64(len(data)), t.Size))
	switch {
	case err != nil:
		err = transfer.ErrInterrupted
	case int64(len(data)) != t.Size || !utf8.Valid(data):
		err = errTextMismatch
	}
	if err != nil {
		t.Finish(err)
		if t.Context().Err() != nil {
			writeError(w, http.StatusConflict, errReceiverCancelled)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !t.ReceiveText(string(data)) {
		writeError(w, http.StatusConflict, errReceiverCancelled)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": transfer.Complete})
}

// state is everything the page shows.
type state struct {
	Name           string          `json:"name"`
	Peers          []network.Peer  `json:"peers"`
	Transfers      []transfer.View `json:"transfers"`
	Addresses      []string        `json:"addresses"`
	Destination    string          `json:"destination"`
	DiscoveryError string          `json:"discovery_error"`
	Warnings       []string        `json:"warnings,omitempty"`
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	st := state{Name: s.name, Peers: []network.Peer{}, Transfers: []transfer.View{}, Addresses: network.LocalAddresses()}
	if warning := s.Warning(); warning != "" {
		st.Warnings = append(st.Warnings, warning)
	}
	st.Warnings = append(st.Warnings, s.Trust.AddressProblems(st.Addresses)...)
	if s.Devices != nil {
		peers, problem := s.Devices.List()
		st.DiscoveryError = problem
		if peers != nil {
			st.Peers = peers
		}
	}
	for _, t := range s.Transfers.All() {
		st.Transfers = append(st.Transfers, t.View())
	}
	if d := s.destination.Load(); d != nil {
		st.Destination = *d
	}
	if st.Addresses == nil {
		st.Addresses = []string{}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
		Text  string   `json:"text"` // sent instead of paths when not empty
		Host  string   `json:"host"`
		Port  int      `json:"port"`
		ID    string   `json:"id"` // the device picked from the list, if any
	}
	if err := readJSONUpTo(w, r, &req, maxTextRequest); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Host == "" {
		writeError(w, http.StatusBadRequest, errors.New("Enter the device address"))
		return
	}
	t, entries, err := outgoing(req.Host, req.Paths, req.Text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.Transfers.Add(t)
	go func() {
		t.Finish(SendTo(t, entries, s.Trust, s.name, req.Host, cmp.Or(req.Port, network.DefaultPort), req.ID))
	}()
	writeJSON(w, http.StatusOK, map[string]string{"id": t.ID})
}

// outgoing makes a transfer to host: of text when there is any, and of
// the paths otherwise.
func outgoing(host string, paths []string, text string) (*transfer.Transfer, []transfer.Entry, error) {
	if text != "" {
		if len(text) > transfer.MaxText {
			return nil, nil, errTextTooLong
		}
		return transfer.NewText(transfer.Outgoing, host, text, transfer.Pairing), nil, nil
	}
	entries, items, size, err := transfer.Collect(paths)
	if err != nil {
		return nil, nil, err
	}
	return transfer.New(transfer.Outgoing, host, items, size, transfer.Pairing), entries, nil
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		Destination string `json:"destination"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.Transfers.Get(req.ID)
	if t == nil || t.Direction != transfer.Incoming {
		writeError(w, http.StatusNotFound, errors.New("Not found"))
		return
	}
	var destination string
	if t.Kind != transfer.Text {
		var err error
		if destination, err = ExistingDir(req.Destination); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if !s.Accept(t, destination) {
		writeError(w, http.StatusConflict, errors.New("The transfer is no longer waiting"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Accept accepts a waiting incoming transfer into destination and offers
// that destination for the next one. Text has no destination, and must
// then arrive in time. Accept remembers a sending Loka, so a later change
// of its certificate is caught. It reports false when the transfer was no
// longer waiting.
func (s *Server) Accept(t *transfer.Transfer, destination string) bool {
	if !t.Accept(destination) {
		return false
	}
	if t.Kind == transfer.Text {
		waitForText(t)
	} else {
		s.destination.Store(&destination)
	}
	if t.DeviceID != "" {
		s.pairings.giveBack(t)
		// A failure only means the sender counts as new next time.
		s.Trust.Remember(trust.Device{ID: t.DeviceID, Name: t.Peer, Fingerprint: t.Fingerprint})
	}
	return true
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.Transfers.Get(req.ID)
	if t == nil {
		writeError(w, http.StatusNotFound, errors.New("Not found"))
		return
	}
	t.Stop()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// receivedText finds the incoming text named in a request from the page.
func (s *Server) receivedText(w http.ResponseWriter, r *http.Request) *transfer.Transfer {
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil
	}
	t := s.Transfers.Get(req.ID)
	if t == nil || t.Direction != transfer.Incoming || t.Kind != transfer.Text {
		writeError(w, http.StatusNotFound, errors.New("Not found"))
		return nil
	}
	return t
}

// readText gives the page text that arrived and wasn't read yet.
func (s *Server) readText(w http.ResponseWriter, r *http.Request) {
	t := s.receivedText(w, r)
	if t == nil {
		return
	}
	text := t.Text()
	if text == "" {
		writeError(w, http.StatusGone, errors.New("The text was already closed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

// dismiss drops text the person has read.
func (s *Server) dismiss(w http.ResponseWriter, r *http.Request) {
	if t := s.receivedText(w, r); t != nil {
		t.Dismiss()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ExistingDir expands a leading ~ and returns the absolute path of an
// existing directory.
func ExistingDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("The destination must be an existing directory")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", errors.New("The destination must be an existing directory")
	}
	return abs, nil
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return readJSONUpTo(w, r, v, 1<<20)
}

// maxTextRequest is how big a request from the page that carries text may
// be. JSON may write a byte of text as six.
const maxTextRequest = 6*transfer.MaxText + 1<<16

// readJSONUpTo reads a JSON request of at most limit bytes.
func readJSONUpTo(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("Invalid request: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// truncate shortens s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

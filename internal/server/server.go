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
	"sync/atomic"
	"time"

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

	Transfers   transfer.Registry
	Trust       *trust.Store           // this device's certificates
	OnlyLinks   bool                   // serve only download links, as loka share does
	Devices     *network.Discovery     // nil when not looking for devices
	destination atomic.Pointer[string] // the last accepted destination
}

// New makes a server that names this device name to other devices.
func New(name, version string) *Server {
	return &Server{
		name:      name,
		version:   version,
		key:       transfer.RandomToken(24),
		localName: transfer.LocalName,
		pick:      desktop.Pick,
		temp:      filepath.Join(os.TempDir(), "loka-"+transfer.RandomToken(9)),
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
	mux.HandleFunc("POST /api/share", s.local(s.share))
	mux.HandleFunc("POST /api/qr", s.local(s.qrCode))
	mux.HandleFunc("POST /api/forget", s.local(s.forget))
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
	Name       string   `json:"name"`
	Items      []string `json:"items"`
	Size       int64    `json:"size"`
	Commitment string   `json:"commitment,omitempty"` // to the sender's nonce, from a Loka
}

type offerReply struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
	Name   string `json:"name"`            // the receiver's, so the sender can remember it
	Nonce  string `json:"nonce,omitempty"` // the receiver's, to a Loka
}

// errOldLoka is the reply to a Loka too old to send here. Loka 0.1 sends
// without TLS, and 0.2.0 without a pairing code. Both show the message.
var errOldLoka = errors.New("This device runs a newer Loka. Update Loka to send to it.")

// errDeclined is how a sender learns that the person at the receiver
// declined or cancelled, whichever request it was making.
var errDeclined = errors.New("The receiver declined or cancelled")

// errCodeMismatch ends a transfer whose sender revealed a nonce other
// than the one it committed to.
var errCodeMismatch = errors.New("The pairing code didn't check out, so the transfer stopped")

func (s *Server) offer(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		writeError(w, http.StatusUpgradeRequired, errOldLoka)
		return
	}
	var o offer
	if err := readJSON(w, r, &o); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if o.Size < 0 || len(o.Items) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("Invalid offer"))
		return
	}
	items := o.Items[:min(len(o.Items), transfer.MaxOfferItems)]
	for i := range items {
		items[i] = truncate(items[i], 255)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if len(r.TLS.PeerCertificates) == 0 {
		// A browser: it can't show which device it is.
		t := transfer.New(transfer.Incoming, host, items, o.Size, transfer.Waiting)
		t.Browser = true
		s.addOffer(w, t)
		return
	}
	if o.Commitment == "" {
		writeError(w, http.StatusUpgradeRequired, errOldLoka)
		return
	}
	t := transfer.New(transfer.Incoming, cmp.Or(truncate(o.Name, 255), host), items, o.Size, transfer.Pairing)
	t.DeviceID, t.Fingerprint = trust.Identify(r.TLS.PeerCertificates[0])
	t.Commitment, t.Nonce = o.Commitment, trust.Nonce()
	standing, known, err := s.Trust.Check(t.DeviceID, t.Fingerprint)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if standing == trust.Changed {
		// Show the refusal here, where the device can be forgotten, under
		// the name it is remembered by: the name it gave is not proven.
		t.Peer = known.Name
		s.Transfers.Add(t)
		t.Finish(&trust.ChangedError{Device: known})
		writeError(w, http.StatusForbidden, errors.New("The receiver remembers this device with a different certificate and refused the transfer"))
		return
	}
	if standing == trust.New {
		t.MarkNewDevice()
	}
	s.addOffer(w, t)
}

func (s *Server) addOffer(w http.ResponseWriter, t *transfer.Transfer) {
	t.Secret = transfer.RandomToken(24)
	s.Transfers.Add(t)
	writeJSON(w, http.StatusOK, offerReply{t.ID, t.Secret, s.name, t.Nonce})
}

// peerReveal takes the nonce a Loka committed to in its offer, which
// makes the pairing code. Only then does the offer wait for an answer.
func (s *Server) peerReveal(w http.ResponseWriter, r *http.Request, t *transfer.Transfer) {
	var req struct {
		Nonce string `json:"nonce"`
	}
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
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) peerStatus(w http.ResponseWriter, _ *http.Request, t *transfer.Transfer) {
	writeJSON(w, http.StatusOK, map[string]transfer.Status{"status": t.Status()})
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
	// A cancel must also end a read that is waiting for bytes, as when the
	// sender has stalled.
	rc := http.NewResponseController(w)
	stop := context.AfterFunc(t.Context(), func() { rc.SetReadDeadline(time.Now()) })
	defer stop()
	err := transfer.Receive(t.Context(), r.Body, t.Destination(), t.Size, &t.Done, s.localName)
	t.Finish(err)
	switch {
	case t.Context().Err() != nil:
		writeError(w, http.StatusConflict, errors.New("The receiver cancelled the transfer"))
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, map[string]transfer.Status{"status": transfer.Complete})
	}
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
		Host  string   `json:"host"`
		Port  int      `json:"port"`
		ID    string   `json:"id"` // the device picked from the list, if any
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Host == "" {
		writeError(w, http.StatusBadRequest, errors.New("Enter the device address"))
		return
	}
	entries, items, size, err := transfer.Collect(req.Paths)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := transfer.New(transfer.Outgoing, req.Host, items, size, transfer.Pairing)
	s.Transfers.Add(t)
	go func() {
		t.Finish(SendTo(t, entries, s.Trust, s.name, req.Host, cmp.Or(req.Port, network.DefaultPort), req.ID))
	}()
	writeJSON(w, http.StatusOK, map[string]string{"id": t.ID})
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
	destination, err := ExistingDir(req.Destination)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.Accept(t, destination) {
		writeError(w, http.StatusConflict, errors.New("The transfer is no longer waiting"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Accept accepts a waiting incoming transfer into destination and offers
// that destination for the next one. It remembers a sending Loka, so a
// later change of its certificate is caught. It reports false when the
// transfer was no longer waiting.
func (s *Server) Accept(t *transfer.Transfer, destination string) bool {
	if !t.Accept(destination) {
		return false
	}
	s.destination.Store(&destination)
	if t.DeviceID != "" {
		// A failure only means the sender counts as new next time.
		s.Trust.Remember(trust.Device{ID: t.DeviceID, Name: t.Peer, Fingerprint: t.Fingerprint})
	}
	return true
}

// forget drops a remembered device, so its new certificate is trusted
// next time.
func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.Trust.Forget(func(d trust.Device) bool { return d.ID == req.ID }); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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

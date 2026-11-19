package main

import (
	"cmp"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/sprytnyk/loka/internal/desktop"
	"github.com/sprytnyk/loka/internal/network"
)

// server answers the page on this device and other devices on the network.
type server struct {
	name      string // shown to other devices
	version   string
	key       string // proves a request comes from this device's page
	localName func(string) string
	pick      func(kind desktop.Kind) ([]string, error)
	temp      string // holds files dropped on the page, made when needed

	transfers   registry
	devices     *network.Discovery     // nil when not looking for devices
	destination atomic.Pointer[string] // the last accepted destination
}

func newServer(name, version string) *server {
	return &server{
		name:      name,
		version:   version,
		key:       randomToken(24),
		localName: localName,
		pick:      desktop.Pick,
		temp:      filepath.Join(os.TempDir(), "loka-"+randomToken(9)),
	}
}

// close removes the files dropped on the page.
func (s *server) close() {
	os.RemoveAll(s.temp)
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /peer/offer", s.offer)
	mux.HandleFunc("GET /peer/status/{id}", s.peer(s.peerStatus))
	mux.HandleFunc("POST /peer/cancel/{id}", s.peer(s.peerCancel))
	mux.HandleFunc("POST /peer/upload/{id}", s.peer(s.peerUpload))

	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/state", s.local(s.state))
	mux.HandleFunc("POST /api/pick", s.local(s.pickPaths))
	mux.HandleFunc("POST /api/upload", s.local(s.upload))
	mux.HandleFunc("POST /api/send", s.local(s.send))
	mux.HandleFunc("POST /api/accept", s.local(s.accept))
	mux.HandleFunc("POST /api/cancel", s.local(s.cancel))
	mux.HandleFunc("POST /api/share", s.local(s.share))
	mux.HandleFunc("GET /download/{token}", s.download)
	return mux
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
func (s *server) local(h http.HandlerFunc) http.HandlerFunc {
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
func (s *server) peer(h func(http.ResponseWriter, *http.Request, *Transfer)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := s.transfers.get(r.PathValue("id"))
		secret := r.Header.Get("X-Transfer-Secret")
		if t == nil || t.Direction != incoming || subtle.ConstantTimeCompare([]byte(secret), []byte(t.secret)) != 1 {
			writeError(w, http.StatusNotFound, errors.New("Not found"))
			return
		}
		h(w, r, t)
	}
}

// offer is what a sender tells the receiver before anything is sent.
type offer struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
	Size  int64    `json:"size"`
}

type offerReply struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

func (s *server) offer(w http.ResponseWriter, r *http.Request) {
	var o offer
	if err := readJSON(w, r, &o); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if o.Size < 0 || len(o.Items) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("Invalid offer"))
		return
	}
	items := o.Items[:min(len(o.Items), maxOfferItems)]
	for i := range items {
		items[i] = truncate(items[i], 255)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	t := newTransfer(incoming, cmp.Or(truncate(o.Name, 255), host), items, o.Size, statusWaiting)
	t.secret = randomToken(24)
	s.transfers.add(t)
	writeJSON(w, http.StatusOK, offerReply{t.ID, t.secret})
}

func (s *server) peerStatus(w http.ResponseWriter, _ *http.Request, t *Transfer) {
	writeJSON(w, http.StatusOK, map[string]Status{"status": t.Status()})
}

func (s *server) peerCancel(w http.ResponseWriter, _ *http.Request, t *Transfer) {
	t.stop()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) peerUpload(w http.ResponseWriter, r *http.Request, t *Transfer) {
	// When the upload stops early, answer at once and drop the connection,
	// rather than first reading the rest of the stream.
	w.Header().Set("Connection", "close")
	if !t.move(statusReceiving, statusAccepted) {
		writeError(w, http.StatusConflict, errors.New("The transfer is not accepted"))
		return
	}
	err := receive(t.ctx, r.Body, t.Destination(), t.Size, &t.done, s.localName)
	t.finish(err)
	switch {
	case t.ctx.Err() != nil:
		writeError(w, http.StatusConflict, errors.New("The receiver cancelled the transfer"))
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, map[string]Status{"status": statusComplete})
	}
}

// state is everything the page shows.
type state struct {
	Name           string         `json:"name"`
	Peers          []network.Peer `json:"peers"`
	Transfers      []transferView `json:"transfers"`
	Addresses      []string       `json:"addresses"`
	Destination    string         `json:"destination"`
	DiscoveryError string         `json:"discovery_error"`
}

func (s *server) state(w http.ResponseWriter, _ *http.Request) {
	st := state{Name: s.name, Peers: []network.Peer{}, Transfers: []transferView{}, Addresses: network.LocalAddresses()}
	if s.devices != nil {
		peers, problem := s.devices.List()
		st.DiscoveryError = problem
		if peers != nil {
			st.Peers = peers
		}
	}
	for _, t := range s.transfers.all() {
		st.Transfers = append(st.Transfers, t.view())
	}
	if d := s.destination.Load(); d != nil {
		st.Destination = *d
	}
	if st.Addresses == nil {
		st.Addresses = []string{}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
		Host  string   `json:"host"`
		Port  int      `json:"port"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Host == "" {
		writeError(w, http.StatusBadRequest, errors.New("Enter the device address"))
		return
	}
	entries, items, size, err := collect(req.Paths)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := newTransfer(outgoing, req.Host, items, size, statusWaiting)
	s.transfers.add(t)
	go func() { t.finish(sendTo(t, entries, s.name, req.Host, cmp.Or(req.Port, network.DefaultPort))) }()
	writeJSON(w, http.StatusOK, map[string]string{"id": t.ID})
}

func (s *server) accept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		Destination string `json:"destination"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.transfers.get(req.ID)
	if t == nil || t.Direction != incoming {
		writeError(w, http.StatusNotFound, errors.New("Not found"))
		return
	}
	destination, err := existingDir(req.Destination)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !t.accept(destination) {
		writeError(w, http.StatusConflict, errors.New("The transfer is no longer waiting"))
		return
	}
	s.destination.Store(&destination)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.transfers.get(req.ID)
	if t == nil {
		writeError(w, http.StatusNotFound, errors.New("Not found"))
		return
	}
	t.stop()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// existingDir expands a leading ~ and returns the absolute path of an
// existing directory.
func existingDir(path string) (string, error) {
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
	if info, err := os.Stat(abs); path == "" || err != nil || !info.IsDir() {
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

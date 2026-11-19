// Package server answers the Loka page on this device and other devices on
// the network, and sends to other devices.
package server

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
	"github.com/sprytnyk/loka/internal/transfer"
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
	Devices     *network.Discovery     // nil when not looking for devices
	destination atomic.Pointer[string] // the last accepted destination
}

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

func (s *Server) Handler() http.Handler {
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
	Name  string   `json:"name"`
	Items []string `json:"items"`
	Size  int64    `json:"size"`
}

type offerReply struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

func (s *Server) offer(w http.ResponseWriter, r *http.Request) {
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
	t := transfer.New(transfer.Incoming, cmp.Or(truncate(o.Name, 255), host), items, o.Size, transfer.Waiting)
	t.Secret = transfer.RandomToken(24)
	s.Transfers.Add(t)
	writeJSON(w, http.StatusOK, offerReply{t.ID, t.Secret})
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
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	st := state{Name: s.name, Peers: []network.Peer{}, Transfers: []transfer.View{}, Addresses: network.LocalAddresses()}
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
	t := transfer.New(transfer.Outgoing, req.Host, items, size, transfer.Waiting)
	s.Transfers.Add(t)
	go func() { t.Finish(SendTo(t, entries, s.name, req.Host, cmp.Or(req.Port, network.DefaultPort))) }()
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
// that destination for the next one. It reports false when the transfer
// was no longer waiting.
func (s *Server) Accept(t *transfer.Transfer, destination string) bool {
	if !t.Accept(destination) {
		return false
	}
	s.destination.Store(&destination)
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

package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/sprytnyk/loka/internal/desktop"
	"github.com/sprytnyk/loka/internal/qr"
	"github.com/sprytnyk/loka/internal/transfer"
)

//go:embed index.html
var page string

// index serves the page. This device gets it with the key it uses for the
// API. Other devices, such as phones, get its upload view over HTTPS,
// which sends the way another Loka does and needs no key. Over plain HTTP,
// and on a server that only shares links, they get the view that checks
// whether the browser trusts this device and moves it to HTTPS.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	label := s.version
	if label != "" && label[0] >= '0' && label[0] <= '9' {
		label = "v" + label
	}
	key, mode := "", "phone"
	local := fromThisDevice(r)
	switch {
	case s.OnlyLinks || r.TLS == nil && !local:
		mode = "check"
	case local:
		key, mode = s.key, "desktop"
	}
	name, _ := json.Marshal(s.name) // escapes <, > and &, so it is safe in a script
	body := strings.NewReplacer(
		"__LOKA_KEY__", key,
		"__LOKA_MODE__", mode,
		"__LOKA_NAME__", string(name),
		"__LOKA_VERSION__", label,
		"__LOKA_CA_FINGERPRINT__", s.Trust.CAFingerprint(),
	).Replace(page)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, body)
}

// caCertificate serves this device's certificate authority, in the form
// phones offer to install.
func (s *Server) caCertificate(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", attachment(CAPath[1:]))
	w.Write(s.Trust.CA())
}

// pickPaths opens the system's picker for files, a directory or a
// destination and returns the chosen paths, or none when the person
// cancels.
func (s *Server) pickPaths(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind desktop.Kind `json:"kind"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Kind != desktop.Files && req.Kind != desktop.Directory && req.Kind != desktop.Destination {
		writeError(w, http.StatusBadRequest, errors.New("Choose files, a directory or a destination"))
		return
	}
	paths, err := s.pick(req.Kind)
	if err != nil {
		if !errors.Is(err, desktop.ErrNoPicker) {
			err = fmt.Errorf("%w (%v)", desktop.ErrNoPicker, err)
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if paths == nil {
		paths = []string{}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"paths": paths})
}

// upload stores one file or directory dropped on the page. The browser
// does not tell the page where dropped files live, so they are copied
// into this run's temporary directory, grouped by session. The reply names
// the top-level item to send.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	session := r.Header.Get("X-Upload-Session")
	name, err := url.PathUnescape(r.Header.Get("X-File-Path"))
	parts, perr := transfer.SplitName(name)
	if err != nil || perr != nil || session == "" || strings.Trim(session, "0123456789abcdefABCDEF") != "" {
		writeError(w, http.StatusBadRequest, errors.New("Invalid upload path"))
		return
	}
	for _, part := range parts {
		if s.localName(part) != part {
			writeError(w, http.StatusBadRequest, errors.New("Invalid upload path"))
			return
		}
	}
	root := filepath.Join(s.temp, "uploads", session)
	target := filepath.Join(append([]string{root}, parts...)...)
	if r.Header.Get("X-Entry-Type") == "directory" {
		err = os.MkdirAll(target, 0o700)
	} else {
		err = store(target, r.Body)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": filepath.Join(root, parts[0])})
}

// store writes a new file, refusing to replace one.
func store(target string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(target)
		return err
	}
	return f.Close()
}

// qrCode draws text, a link, as a QR code for the page.
func (s *Server) qrCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	code, err := qr.Encode(req.Text)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("The link is too long for a QR code"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"svg": code.SVG()})
}

package main

import (
	_ "embed"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

//go:embed index.html
var page string

// errNoPicker means this system has no way to show a file picker.
var errNoPicker = errors.New("The file picker is unavailable here. Enter a path or drop files instead")

// index serves the page, with the key it uses for the API, only to this
// device.
func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if !fromThisDevice(r) {
		writeError(w, http.StatusForbidden, errors.New("Local access only"))
		return
	}
	label := s.version
	if label != "" && label[0] >= '0' && label[0] <= '9' {
		label = "v" + label
	}
	body := strings.NewReplacer("__LOKA_KEY__", s.key, "__LOKA_VERSION__", label).Replace(page)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, body)
}

// pickPaths opens the system's picker for files or for a folder and
// returns the chosen paths, or none when the person cancels.
func (s *server) pickPaths(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	paths, err := s.pick(req.Kind == "folder")
	if err != nil {
		if !errors.Is(err, errNoPicker) {
			err = errors.Join(errNoPicker, err)
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
func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	session := r.Header.Get("X-Upload-Session")
	name, err := url.PathUnescape(r.Header.Get("X-File-Path"))
	parts, perr := splitName(name)
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
		err = s.store(target, r.Body)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": filepath.Join(root, parts[0])})
}

// store writes a new file, refusing to replace one.
func (s *server) store(target string, body io.Reader) error {
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

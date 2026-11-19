package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

// shareArchiveName is the download name when a link serves several items.
const shareArchiveName = "loka-share.zip"

// newLink makes a download link for the chosen paths.
func newLink(paths []string) (*Transfer, error) {
	entries, items, size, err := collect(paths)
	if err != nil {
		return nil, err
	}
	t := newTransfer(outgoing, "Link", items, size, statusShared)
	t.token = randomToken(24)
	t.entries = entries
	return t, nil
}

func (s *server) share(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t, err := newLink(req.Paths)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.transfers.add(t)
	writeJSON(w, http.StatusOK, map[string]string{"id": t.ID, "token": t.token})
}

// download serves a link: a single file as itself, so browsers can resume
// it, and anything else as a ZIP archive built while it downloads.
func (s *server) download(w http.ResponseWriter, r *http.Request) {
	t := s.transfers.link(r.PathValue("token"))
	if t == nil || t.Status() != statusShared {
		writeError(w, http.StatusNotFound, errors.New("This link has expired"))
		return
	}
	// Stopping the link also stops downloads in progress.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(t.ctx, cancel)()

	if len(t.entries) == 1 && !t.entries[0].dir {
		serveFile(w, r.WithContext(ctx), t.entries[0])
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachment(shareArchiveName))
	if err := writeZip(ctx, w, t.entries); err != nil {
		// Abort the response, so the download fails instead of ending
		// with a broken archive that looks complete.
		panic(http.ErrAbortHandler)
	}
}

func serveFile(w http.ResponseWriter, r *http.Request, e entry) {
	f, err := os.Open(e.src)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("The shared file is no longer available"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, errors.New("The shared file is no longer available"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", attachment(e.name))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// writeZip writes entries as a ZIP archive without compression, the way
// the files are on disk.
func writeZip(ctx context.Context, w io.Writer, entries []entry) error {
	zw := zip.NewWriter(w)
	var done atomic.Int64
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Store, Modified: e.mtime}
		if e.dir {
			hdr.Name += "/"
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if !e.dir {
			if err := copyFile(ctx, fw, e, &done); err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// attachment returns a Content-Disposition value that downloads as name,
// encoded so any name survives (RFC 6266 and RFC 8187).
func attachment(name string) string {
	var b strings.Builder
	for _, c := range []byte(name) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return "attachment; filename*=UTF-8''" + b.String()
}

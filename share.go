package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// shareArchiveName is the download name when a link serves several items.
const shareArchiveName = "loka-share.zip"

// errLinkGone means a shared item was removed or replaced.
var errLinkGone = errors.New("The shared items are no longer available")

// sharedItem is a path chosen for a link and what it was when the link was
// made, so a download can tell when something else took its place.
type sharedItem struct {
	path string
	info os.FileInfo
}

// sameAs reports whether info is still the item that was shared.
func (it sharedItem) sameAs(info os.FileInfo) bool {
	return os.SameFile(it.info, info) && info.Mode().Type() == it.info.Mode().Type()
}

// newLink makes a download link for the chosen paths.
func newLink(paths []string) (*Transfer, error) {
	_, items, size, err := collect(paths)
	if err != nil {
		return nil, err
	}
	t := newTransfer(outgoing, "Link", items, size, statusShared)
	t.token = randomToken(24)
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		t.shared = append(t.shared, sharedItem{filepath.Clean(p), info})
	}
	return t, nil
}

// sharedEntries lists what a link serves now, so each download has the
// current contents of the shared directories.
func sharedEntries(shared []sharedItem) ([]entry, error) {
	paths := make([]string, len(shared))
	for i, it := range shared {
		info, err := os.Lstat(it.path)
		if err != nil || !it.sameAs(info) {
			return nil, errLinkGone
		}
		paths[i] = it.path
	}
	entries, _, _, err := collect(paths)
	if err != nil {
		return nil, errLinkGone
	}
	return entries, nil
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

	if len(t.shared) == 1 && !t.shared[0].info.IsDir() {
		serveFile(w, r.WithContext(ctx), t.shared[0])
		return
	}
	entries, err := sharedEntries(t.shared)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachment(shareArchiveName))
	if err := writeZip(ctx, w, entries); err != nil {
		// Abort the response, so the download fails instead of ending
		// with a broken archive that looks complete.
		panic(http.ErrAbortHandler)
	}
}

// serveFile serves the shared file as it is now, and only while it is
// still the file that was shared.
func serveFile(w http.ResponseWriter, r *http.Request, it sharedItem) {
	f, err := os.Open(it.path)
	if err != nil {
		writeError(w, http.StatusNotFound, errLinkGone)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !it.sameAs(info) {
		writeError(w, http.StatusNotFound, errLinkGone)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", attachment(filepath.Base(it.path)))
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

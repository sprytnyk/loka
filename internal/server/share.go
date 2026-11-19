package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sprytnyk/loka/internal/transfer"
)

// shareArchiveName is the download name when a link serves several items.
const shareArchiveName = "loka-share.zip"

func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t, err := transfer.NewLink(req.Paths)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.Transfers.Add(t)
	writeJSON(w, http.StatusOK, map[string]string{"id": t.ID, "token": t.Token})
}

// download serves a link: a single file as itself, so browsers can resume
// it, and anything else as a ZIP archive built while it downloads.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	t := s.Transfers.Link(r.PathValue("token"))
	if t == nil || t.Status() != transfer.Shared {
		writeError(w, http.StatusNotFound, errors.New("This link has expired"))
		return
	}
	// Stopping the link also stops downloads in progress.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(t.Context(), cancel)()

	if t.SharesOneFile() {
		serveFile(w, r.WithContext(ctx), t)
		return
	}
	entries, err := t.LinkEntries()
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachment(shareArchiveName))
	if err := transfer.WriteZip(ctx, w, entries); err != nil {
		// Abort the response, so the download fails instead of ending
		// with a broken archive that looks complete.
		panic(http.ErrAbortHandler)
	}
}

// serveFile serves the file a link shares, as it is now, and only while it
// is still the file that was shared.
func serveFile(w http.ResponseWriter, r *http.Request, t *transfer.Transfer) {
	f, info, err := t.OpenSharedFile()
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", attachment(info.Name()))
	http.ServeContent(w, r, "", info.ModTime(), f)
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

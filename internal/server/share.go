package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

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
	defer stopWriting(w, t.Context(), cancel)()

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

// stopWriting calls cancel when ctx ends, and also ends any write to w
// then, so a download whose client has stopped reading can't keep the
// handler waiting. Calling the returned func, before the handler returns,
// stops it from touching w afterwards, when the connection may already be
// serving another request.
func stopWriting(w http.ResponseWriter, ctx context.Context, cancel func()) (stop func()) {
	rc := http.NewResponseController(w)
	var mu sync.Mutex
	done := false
	stopAfter := context.AfterFunc(ctx, func() {
		cancel()
		mu.Lock()
		defer mu.Unlock()
		if !done {
			rc.SetWriteDeadline(time.Now())
		}
	})
	return func() {
		stopAfter()
		mu.Lock()
		defer mu.Unlock()
		done = true
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
	http.ServeContent(w, r, "", info.ModTime(), stoppableFile{r.Context(), f})
}

// stoppableFile reads a file until ctx ends. Being a plain reader rather
// than an *os.File also keeps the kernel from sending the file in one go,
// which would not notice the stop.
type stoppableFile struct {
	ctx context.Context
	f   *os.File
}

func (s stoppableFile) Read(b []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	return s.f.Read(b)
}

func (s stoppableFile) Seek(offset int64, whence int) (int64, error) {
	return s.f.Seek(offset, whence)
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

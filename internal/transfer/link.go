package transfer

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

// errLinkGone means a shared item was removed or replaced.
var errLinkGone = errors.New("The shared items are no longer available")

// ErrLinkBusy tells whoever downloads a link that a shared file is open
// for writing, without showing where it is on the sharing device. It
// lasts only until the file is closed.
var ErrLinkBusy = errors.New("A shared file is open for editing on the sharing device. Try again later")

// sharedItem is a path chosen for a link and what it was when the link was
// made, so a download can tell when something else took its place.
type sharedItem struct {
	path string
	info os.FileInfo
}

// NewLink makes a download link for the chosen paths.
func NewLink(paths []string) (*Transfer, error) {
	_, items, size, err := Collect(paths)
	if err != nil {
		return nil, err
	}
	h := newHolder()
	var shared []sharedItem
	for _, p := range paths {
		p = filepath.Clean(p)
		info, err := lstat(p)
		if err == nil {
			info, err = h.open(p, info)
		}
		if err != nil {
			h.release()
			return nil, fmt.Errorf("Cannot share %s: %w", p, err)
		}
		shared = append(shared, sharedItem{p, info})
	}
	t := New(Outgoing, "Link", items, size, Shared)
	t.Token = RandomToken(24)
	t.shared = shared
	// Keep the shared items open until the link stops.
	context.AfterFunc(t.Context(), h.release)
	return t, nil
}

// SharesOneFile reports whether a link shares a single file, which is
// served as itself rather than in a ZIP archive.
func (t *Transfer) SharesOneFile() bool {
	return len(t.shared) == 1 && !t.shared[0].info.IsDir()
}

// OpenSharedFile opens the file a link shares, as it is now, and only while
// it is still the file that was shared.
func (t *Transfer) OpenSharedFile() (*os.File, os.FileInfo, error) {
	it := t.shared[0]
	f, info, err := openChosen(it.path, it.info)
	if errors.Is(err, errInUse) {
		return nil, nil, ErrLinkBusy
	}
	if err != nil {
		return nil, nil, errLinkGone
	}
	return f, info, nil
}

// LinkEntries lists what a link serves now, so each download has the
// current contents of the shared directories.
func (t *Transfer) LinkEntries() ([]Entry, error) {
	paths := make([]string, len(t.shared))
	for i, it := range t.shared {
		info, err := os.Lstat(it.path)
		if err != nil || !sameFile(it.info, info) {
			return nil, errLinkGone
		}
		paths[i] = it.path
	}
	entries, _, _, err := Collect(paths)
	if err != nil {
		return nil, errLinkGone
	}
	if anyInUse(entries) {
		return nil, ErrLinkBusy
	}
	return entries, nil
}

// anyInUse reports whether another program has one of the files open for
// writing, so reading it would fail. A ZIP download checks first, while it
// can still answer with a message instead of breaking off. A program can
// still open a file after the check, which breaks the download off.
func anyInUse(entries []Entry) bool {
	if !blocksWriters {
		return false
	}
	for _, e := range entries {
		if e.dir {
			continue
		}
		f, err := openForReading(e.src)
		if errors.Is(err, errInUse) {
			return true
		}
		if err == nil {
			f.Close()
		}
	}
	return false
}

// WriteZip writes entries as a ZIP archive without compression, the way
// the files are on disk.
func WriteZip(ctx context.Context, w io.Writer, entries []Entry) error {
	zw := zip.NewWriter(w)
	var done atomic.Int64
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Store, Modified: e.info.ModTime()}
		if e.dir {
			hdr.Name += "/"
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if e.dir {
			continue
		}
		if err := copyFile(ctx, fw, e, &done); err != nil {
			return err
		}
	}
	return zw.Close()
}

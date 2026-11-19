package transfer

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
)

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

// NewLink makes a download link for the chosen paths.
func NewLink(paths []string) (*Transfer, error) {
	_, items, size, err := Collect(paths)
	if err != nil {
		return nil, err
	}
	t := New(Outgoing, "Link", items, size, Shared)
	t.Token = RandomToken(24)
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		t.shared = append(t.shared, sharedItem{filepath.Clean(p), info})
	}
	if err := t.holdShared(); err != nil {
		return nil, err
	}
	return t, nil
}

// holdShared keeps the shared items open until the link stops. Unix
// systems can give a deleted file's inode number to the next new file, but
// not while the old one is open, so a file written in place of a shared
// one can't pass for it. Windows file IDs change on reuse, so nothing is
// held there.
func (t *Transfer) holdShared() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	var held []*os.File
	release := func() {
		for _, f := range held {
			f.Close()
		}
	}
	for _, it := range t.shared {
		f, err := os.Open(it.path)
		if err != nil {
			release()
			return err
		}
		held = append(held, f)
		if info, err := f.Stat(); err != nil || !it.sameAs(info) {
			release()
			return errLinkGone
		}
	}
	context.AfterFunc(t.Context(), release)
	return nil
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
	f, err := os.Open(it.path)
	if err != nil {
		return nil, nil, errLinkGone
	}
	info, err := f.Stat()
	if err != nil || !it.sameAs(info) {
		f.Close()
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
		if err != nil || !it.sameAs(info) {
			return nil, errLinkGone
		}
		paths[i] = it.path
	}
	entries, _, _, err := Collect(paths)
	if err != nil {
		return nil, errLinkGone
	}
	return entries, nil
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

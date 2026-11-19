package transfer

import (
	"fmt"
	"os"
	"sync/atomic"
)

// CollectHeld is Collect that also keeps the files open until release is
// called. Each file's size and identity are read from the open file, so
// nothing can take a file's place between choosing it and sending it.
func CollectHeld(paths []string) (entries []Entry, items []string, size int64, release func(), err error) {
	h := newHolder()
	entries, items, size, err = collect(paths, h)
	if err != nil {
		h.release()
		return nil, nil, 0, nil, err
	}
	return entries, items, size, h.release, nil
}

// holder keeps chosen files open. Unix systems can give a deleted file's
// inode number to the next new file, but not while the old one is open, so
// a file written in place of a held one can't pass for it. Windows file IDs
// change on reuse, so nothing is held there.
type holder struct {
	budget int
	files  []*os.File
}

// heldFiles counts the files all holders keep open, so transfers waiting
// at the same time share one budget.
var heldFiles atomic.Int64

func newHolder() *holder {
	limit := openFileLimit()
	// Leave a quarter of the limit, at most 256, for everything else.
	return &holder{budget: limit - min(limit/4, 256)}
}

// open opens path, which must still be what seen describes, and keeps it
// open. It returns the open file's own info, so what is recorded and what
// is held are the same file.
func (h *holder) open(path string, seen os.FileInfo) (os.FileInfo, error) {
	if !idsCanBeReused {
		return seen, nil
	}
	if heldFiles.Add(1) > int64(h.budget) {
		heldFiles.Add(-1)
		return nil, fmt.Errorf("Loka can keep only %d files open on this system, counting those other transfers and links hold. Choose fewer files", h.budget)
	}
	f, info, err := openChosen(path, seen)
	if err != nil {
		heldFiles.Add(-1)
		return nil, err
	}
	h.files = append(h.files, f)
	return info, nil
}

// release closes the held files.
func (h *holder) release() {
	for _, f := range h.files {
		f.Close()
	}
	heldFiles.Add(-int64(len(h.files)))
	h.files = nil
}

// openChosen opens path only while it is still what seen describes, and
// returns what the open file is. It checks before opening too, so a
// special file put in its place is never opened.
func openChosen(path string, seen os.FileInfo) (*os.File, os.FileInfo, error) {
	if info, err := os.Lstat(path); err != nil || !sameFile(seen, info) {
		return nil, nil, fmt.Errorf("%s %w", path, errReplaced)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !sameFile(seen, info) {
		f.Close()
		return nil, nil, fmt.Errorf("%s %w", path, errReplaced)
	}
	return f, info, nil
}

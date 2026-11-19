package transfer

import (
	"fmt"
	"os"
	"sync/atomic"
)

// holder keeps the items a link shares open. Unix systems can give a
// deleted file's inode number to the next new file, but not while the old
// one is open, so a file written in place of a held one can't pass for it.
// Links serve their items as they are now, edits included, so the change
// time that sends check can't tell a link's file apart from a new one.
// Only the chosen items are held, not what is inside directories. Nothing
// is held on Windows, where NTFS file IDs change on reuse; FAT drives can
// reuse them, so a link there still trusts the ID.
type holder struct {
	budget int
	files  []*os.File
}

// heldFiles counts the files all holders keep open, so links share one
// budget.
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
	if !holdsFiles {
		return seen, nil
	}
	if heldFiles.Add(1) > int64(h.budget) {
		heldFiles.Add(-1)
		return nil, fmt.Errorf("Loka can keep only %d items open on this system, counting those other links hold. Share fewer items, or share the directory that holds them", h.budget)
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

// openChosen opens path only while it is still what seen describes, a
// file or a directory, and returns what the open file is. It checks before
// opening too, and opens with openForReading, so a special file put in its
// place is never read and never hangs the open.
func openChosen(path string, seen os.FileInfo) (*os.File, os.FileInfo, error) {
	if !seen.Mode().IsRegular() && !seen.IsDir() {
		return nil, nil, fmt.Errorf("%s %w", path, errReplaced)
	}
	if info, err := os.Lstat(path); err != nil || !sameFile(seen, info) {
		return nil, nil, fmt.Errorf("%s %w", path, errReplaced)
	}
	f, err := openForReading(path)
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

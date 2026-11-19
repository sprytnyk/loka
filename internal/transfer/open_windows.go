package transfer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openForReading opens path so other programs can still read or delete
// the file while it is open, but not write to it. A program that keeps a
// file open can write to it without moving its last write time until it
// closes it, so the times alone can't show an edit made then. If another
// program has the file open for writing already, the open fails with
// errInUse.
func openForReading(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, fmt.Errorf("%s %w", path, errInUse)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// blocksWriters is whether openForReading keeps other programs from
// writing to a file while it is open.
const blocksWriters = true

// longPath lets Windows open a path longer than its usual limit of 260
// characters, the way os.Open does. A short relative path counts in full,
// since Windows adds the working directory to it.
func longPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil || len(abs) < 248 {
		return path
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}

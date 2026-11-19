package transfer

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from to to, failing with fs.ErrExist when to
// exists, in one step that no other process can slip between.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return renameIfFree(from, to) // the file system cannot do it in one step
	}
	return err
}

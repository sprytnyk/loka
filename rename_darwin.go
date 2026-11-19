package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from to to, failing with fs.ErrExist when to
// exists, in one step that no other process can slip between.
func renameNoReplace(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return renameIfFree(from, to) // the file system cannot do it in one step
	}
	return err
}

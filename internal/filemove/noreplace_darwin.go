package filemove

import (
	"errors"

	"golang.org/x/sys/unix"
)

func NoReplace(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return ErrUnsupported
	}
	return err
}

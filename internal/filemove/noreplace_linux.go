package filemove

import (
	"errors"

	"golang.org/x/sys/unix"
)

func NoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return ErrUnsupported
	}
	return err
}

//go:build !windows

package trust

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile waits until no other process holds f's lock, then takes it.
func lockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			return err
		}
	}
}

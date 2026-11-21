package trust

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile waits until no other process holds f's lock, then takes it.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped))
}

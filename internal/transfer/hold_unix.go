//go:build unix

package transfer

import (
	"math"
	"os"
	"syscall"
)

const holdsFiles = true

// openFileLimit is how many files this process may have open. A variable
// so tests can lower it.
var openFileLimit = func() int {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil || lim.Cur > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(lim.Cur)
}

// openForReading opens path for reading. It doesn't follow a symbolic
// link or wait for a writer on a named pipe, in case one has taken the
// path's place. Reads of regular files ignore O_NONBLOCK.
func openForReading(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

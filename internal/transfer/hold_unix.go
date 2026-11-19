//go:build unix

package transfer

import (
	"math"
	"syscall"
)

const idsCanBeReused = true

// openFileLimit is how many files this process may have open. A variable
// so tests can lower it.
var openFileLimit = func() int {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil || lim.Cur > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(lim.Cur)
}

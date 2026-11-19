//go:build darwin || freebsd || netbsd

package transfer

import (
	"os"
	"syscall"
)

// changeStamp is the file's change time, which moves on any change to
// its data or metadata, chmod and touch included, and can't be set back.
func changeStamp(info os.FileInfo) stamp {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return stamp{}
	}
	return stamp{int64(st.Ctimespec.Sec), int64(st.Ctimespec.Nsec)}
}

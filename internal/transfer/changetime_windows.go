package transfer

import (
	"os"
	"syscall"
)

// changeStamp is the file's last write and creation times. Windows keeps
// a change time too, but file info read by path doesn't include it. Writing
// moves the last write time, and a new file gets a new creation time,
// unless it takes a deleted file's name within seconds, when Windows can
// give it the old one's.
func changeStamp(info os.FileInfo) stamp {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return stamp{}
	}
	return stamp{d.LastWriteTime.Nanoseconds(), d.CreationTime.Nanoseconds()}
}

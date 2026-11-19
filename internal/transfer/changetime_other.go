//go:build !unix && !windows

package transfer

import "os"

// changeStamp is the file's modification time, the only time these
// systems report.
func changeStamp(info os.FileInfo) stamp {
	return stamp{info.ModTime().UnixNano()}
}

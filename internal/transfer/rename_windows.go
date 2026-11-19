package transfer

import "golang.org/x/sys/windows"

// renameNoReplace renames from to to, failing with fs.ErrExist when to
// exists. Without MOVEFILE_REPLACE_EXISTING, Windows refuses to replace.
func renameNoReplace(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, 0)
}

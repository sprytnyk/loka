package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f is a console that shows colour codes. Older
// consoles leave that off, so it turns it on where it can.
func isTerminal(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	const colorCodes = windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	return mode&colorCodes != 0 || windows.SetConsoleMode(h, mode|colorCodes) == nil
}

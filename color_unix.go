//go:build !windows

package main

import "os"

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func isTerminalInput(f *os.File) bool { return isTerminal(f) }

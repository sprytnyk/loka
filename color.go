package main

import (
	"io"
	"os"
)

// The colours Python's argparse gives help and errors, as ANSI codes.
const (
	headingColor    = "1;34" // bold blue: "usage:" and section names
	progColor       = "1;35" // bold magenta: the program name
	commandColor    = "1;32" // bold green: commands
	optionColor     = "1;36" // bold cyan: options
	argColor        = "1;33" // bold yellow: option values
	defaultColor    = "33"   // yellow: default values
	errorLabelColor = "1;35" // bold magenta: "error:"
	errorColor      = "35"   // magenta: the error message
)

// painter colours text, or leaves it plain when colour is off.
type painter bool

func (p painter) paint(code, s string) string {
	if !p {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// colorFor decides whether text written to w gets colour, by the rules
// Python uses: NO_COLOR turns it off, FORCE_COLOR turns it on, and
// otherwise only a terminal gets colour, unless TERM is dumb.
func colorFor(w io.Writer) painter {
	switch {
	case os.Getenv("NO_COLOR") != "":
		return false
	case os.Getenv("FORCE_COLOR") != "":
		return true
	case os.Getenv("TERM") == "dumb":
		return false
	}
	f, ok := w.(*os.File)
	return painter(ok && isTerminal(f))
}

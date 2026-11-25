package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

type command struct{ name, help string }

// commands are the commands loka knows, in the order help lists them.
var commands = []command{
	{"serve", "open the page in your browser (the default)"},
	{"send", "send files, directories or text to another device"},
	{"receive", "wait for transfers and answer them in the terminal"},
	{"devices", "list devices running Loka on the same network"},
	{"share", "print a download link for paths: loka share PATH..."},
	{"forget", "forget a remembered device: loka forget NAME|ID"},
}

func isCommand(name string) bool {
	return slices.ContainsFunc(commands, func(c command) bool { return c.name == name })
}

// optionArgs names the value each option takes, for help.
var optionArgs = map[string]string{"to": "ADDRESS", "address": "ADDRESS", "port": "PORT", "peer-port": "PORT"}

func usageLine(p painter) string {
	return p.paint(headingColor, "usage:") + " " + p.paint(progColor, "loka") + " " +
		p.paint("32", "[command]") + " " + p.paint("36", "[options]") + " " + p.paint("32", "[paths...]")
}

// printHelp writes the full help, in colour when w shows it.
func printHelp(w io.Writer, fs *flag.FlagSet) {
	p := colorFor(w)
	type row struct{ left, plain, help string }
	var commandRows, optionRows []row
	for _, c := range commands {
		commandRows = append(commandRows, row{p.paint(commandColor, c.name), c.name, c.help})
	}
	optionRows = append(optionRows, row{p.paint(optionColor, "-h") + ", " + p.paint(optionColor, "--help"), "-h, --help", "show this help and exit"})
	fs.VisitAll(func(f *flag.Flag) {
		r := row{p.paint(optionColor, "--"+f.Name), "--" + f.Name, f.Usage}
		if arg := optionArgs[f.Name]; arg != "" {
			r.left += " " + p.paint(argColor, arg)
			r.plain += " " + arg
		}
		if f.DefValue != "" && f.DefValue != "false" {
			r.help += " (default: " + p.paint(defaultColor, f.DefValue) + ")"
		}
		optionRows = append(optionRows, r)
	})
	width := 0
	for _, r := range slices.Concat(commandRows, optionRows) {
		width = max(width, len(r.plain))
	}

	var b strings.Builder
	b.WriteString(usageLine(p) + "\n\nShare files, directories and text with computers and phones on the same local network.\n")
	for _, section := range []struct {
		name string
		rows []row
	}{{"commands:", commandRows}, {"options:", optionRows}} {
		b.WriteString("\n" + p.paint(headingColor, section.name) + "\n")
		for _, r := range section.rows {
			fmt.Fprintf(&b, "  %s%s  %s\n", r.left, strings.Repeat(" ", width-len(r.plain)), r.help)
		}
	}
	io.WriteString(w, b.String())
}

// printError writes an error the way argparse does, in colour when w shows
// it.
func printError(w io.Writer, msg string) {
	p := colorFor(w)
	fmt.Fprintf(w, "loka: %s %s\n", p.paint(errorLabelColor, "error:"), p.paint(errorColor, msg))
}

// printUsageError writes the short usage line and then the error.
func printUsageError(w io.Writer, msg string) {
	fmt.Fprintln(w, usageLine(colorFor(w)))
	printError(w, msg)
}

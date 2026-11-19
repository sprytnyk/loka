package main

import (
	"errors"
	"os/exec"
	"strings"
)

// pickWithSystem shows the Finder picker through AppleScript.
func pickWithSystem(folder bool) ([]string, error) {
	script := []string{
		`set picked to choose file with prompt "Choose files" with multiple selections allowed`,
		`set out to ""`,
		`repeat with f in picked`,
		`set out to out & POSIX path of f & linefeed`,
		`end repeat`,
		`return out`,
	}
	if folder {
		script = []string{`return POSIX path of (choose folder with prompt "Choose a directory")`}
	}
	var args []string
	for _, line := range script {
		args = append(args, "-e", line)
	}
	out, err := exec.Command("osascript", args...).Output()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && strings.Contains(string(exitErr.Stderr), "-128") {
		return nil, nil // cancelled
	}
	if err != nil {
		return nil, err
	}
	paths := lines(string(out))
	for i, p := range paths {
		if len(p) > 1 {
			paths[i] = strings.TrimSuffix(p, "/") // folders end with a slash
		}
	}
	return paths, nil
}

// openBrowser opens url in the default web browser.
func openBrowser(url string) error {
	return start("open", url)
}

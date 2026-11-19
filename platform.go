package main

import (
	"os/exec"
	"strings"
)

// lines splits picker output into paths, one per line.
func lines(out string) []string {
	var paths []string
	for line := range strings.Lines(out) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// start runs a command without waiting for it.
func start(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

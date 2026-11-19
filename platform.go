package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// pickKind says what a picker chooses.
type pickKind string

const (
	pickFiles       pickKind = "files"
	pickDirectory   pickKind = "directory"
	pickDestination pickKind = "destination"
)

// title is the picker window's title.
func (k pickKind) title() string {
	switch k {
	case pickFiles:
		return "Choose files"
	case pickDirectory:
		return "Choose a directory"
	}
	return "Choose a destination"
}

// splitPaths splits picker output on sep. Only the final newline is
// dropped, since a path can start or end with spaces.
func splitPaths(out, sep string) ([]string, error) {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil, nil
	}
	return fullPaths(strings.Split(out, sep))
}

// decodePaths reads picker output written as a JSON list.
func decodePaths(out []byte) ([]string, error) {
	var paths []string
	if err := json.Unmarshal(out, &paths); err != nil {
		return nil, fmt.Errorf("The file picker answered %q", out)
	}
	return fullPaths(paths)
}

// fullPaths makes sure a picker returned only full paths.
func fullPaths(paths []string) ([]string, error) {
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("The file picker returned %q, which is not a full path", p)
		}
	}
	return paths, nil
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

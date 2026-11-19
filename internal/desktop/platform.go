// Package desktop shows the system's file pickers and opens the browser,
// through the tools each system has.
package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoPicker means this system has no way to show a file picker.
var ErrNoPicker = errors.New("The desktop file picker is unavailable here")

// Kind says what a picker chooses.
type Kind string

const (
	Files       Kind = "files"
	Directory   Kind = "directory"
	Destination Kind = "destination"
)

// title is the picker window's title.
func (k Kind) title() string {
	switch k {
	case Files:
		return "Choose files"
	case Directory:
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

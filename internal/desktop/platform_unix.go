//go:build !darwin && !windows

package desktop

import (
	"errors"
	"os"
	"os/exec"
)

// Pick shows a desktop file picker through zenity or kdialog.
func Pick(kind Kind) ([]string, error) {
	// Without a display, the tools fail the same way as when the person
	// cancels.
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, ErrNoPicker
	}
	// zenity joins several paths with this separator, which file names
	// do not use. kdialog can only put them on separate lines.
	const unitSeparator = "\x1f"
	zenity := []string{"zenity", "--file-selection", "--title=" + kind.title()}
	kdialog := []string{"kdialog", "--title", kind.title()}
	if kind == Files {
		zenity = append(zenity, "--multiple", "--separator="+unitSeparator)
		kdialog = append(kdialog, "--getopenfilename", ".", "--multiple", "--separate-output")
	} else {
		zenity = append(zenity, "--directory")
		kdialog = append(kdialog, "--getexistingdirectory")
	}
	for _, tool := range []struct {
		command []string
		sep     string
	}{{zenity, unitSeparator}, {kdialog, "\n"}} {
		if _, err := exec.LookPath(tool.command[0]); err != nil {
			continue
		}
		out, err := exec.Command(tool.command[0], tool.command[1:]...).Output()
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return nil, nil // cancelled
		}
		if err != nil {
			continue
		}
		return splitPaths(string(out), tool.sep)
	}
	return nil, ErrNoPicker
}

// OpenBrowser opens url in the desktop's web browser.
func OpenBrowser(url string) error {
	return start("xdg-open", url)
}

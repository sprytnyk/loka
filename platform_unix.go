//go:build !darwin && !windows

package main

import (
	"errors"
	"os/exec"
)

// pickWithSystem shows a desktop file picker through zenity or kdialog.
func pickWithSystem(folder bool) ([]string, error) {
	zenity := []string{"zenity", "--file-selection", "--title=Choose files", "--multiple", "--separator=\n"}
	kdialog := []string{"kdialog", "--getopenfilename", ".", "--multiple", "--separate-output"}
	if folder {
		zenity = []string{"zenity", "--file-selection", "--directory", "--title=Choose a directory"}
		kdialog = []string{"kdialog", "--getexistingdirectory"}
	}
	for _, command := range [][]string{zenity, kdialog} {
		if _, err := exec.LookPath(command[0]); err != nil {
			continue
		}
		out, err := exec.Command(command[0], command[1:]...).Output()
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return nil, nil // cancelled
		}
		if err != nil {
			continue
		}
		return lines(string(out)), nil
	}
	return nil, errNoPicker
}

// openBrowser opens url in the desktop's web browser.
func openBrowser(url string) error {
	return start("xdg-open", url)
}

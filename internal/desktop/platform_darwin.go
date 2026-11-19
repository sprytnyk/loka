package desktop

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// Pick shows the Finder picker through JavaScript for
// Automation. It brings the picker to the front and answers in JSON, so
// any file name survives.
func Pick(kind Kind) ([]string, error) {
	prompt := strconv.Quote(kind.title())
	choose := `app.chooseFile({withPrompt: ` + prompt + `, multipleSelectionsAllowed: true})`
	if kind != Files {
		choose = `[app.chooseFolder({withPrompt: ` + prompt + `})]`
	}
	script := `const app = Application.currentApplication();
app.includeStandardAdditions = true;
app.activate();
JSON.stringify(` + choose + `.map(p => p.toString()));`
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).Output()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && strings.Contains(string(exitErr.Stderr), "-128") {
		return nil, nil // cancelled
	}
	if err != nil {
		return nil, err
	}
	paths, err := decodePaths(out)
	for i, p := range paths {
		if len(p) > 1 {
			paths[i] = strings.TrimSuffix(p, "/")
		}
	}
	return paths, err
}

// OpenBrowser opens url in the default web browser.
func OpenBrowser(url string) error {
	return start("open", url)
}

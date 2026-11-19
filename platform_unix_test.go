//go:build !darwin && !windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeTool puts a shell script named name first on PATH.
func fakeTool(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestLinuxPickerUsesZenity(t *testing.T) {
	fakeTool(t, "zenity", `for a in "$@"; do [ "$a" = --directory ] && { echo "/tmp/Мої файли"; exit 0; }; done
printf '/tmp/a b.txt\n/tmp/c.txt\n'`)
	if got, err := pickWithSystem(true); err != nil || !slices.Equal(got, []string{"/tmp/Мої файли"}) {
		t.Fatalf("folder: %v %v", got, err)
	}
	if got, err := pickWithSystem(false); err != nil || !slices.Equal(got, []string{"/tmp/a b.txt", "/tmp/c.txt"}) {
		t.Fatalf("files: %v %v", got, err)
	}
}

func TestLinuxPickerCancelAndFallback(t *testing.T) {
	fakeTool(t, "zenity", "exit 1")
	if got, err := pickWithSystem(true); err != nil || got != nil {
		t.Fatalf("cancel returned %v %v", got, err)
	}
	fakeTool(t, "kdialog", "echo /tmp/from-kdialog")
	if got, err := pickWithSystem(true); err != nil || !slices.Equal(got, []string{"/tmp/from-kdialog"}) {
		t.Fatalf("kdialog returned %v %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := pickWithSystem(true); err != errNoPicker {
		t.Fatalf("no picker returned %v", err)
	}
}

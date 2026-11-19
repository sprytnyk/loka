//go:build !darwin && !windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeTool puts a shell script named name first on PATH, with a display
// to show it on.
func fakeTool(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
}

func TestLinuxPickerUsesZenity(t *testing.T) {
	fakeTool(t, "zenity", `for a in "$@"; do [ "$a" = --directory ] && { echo "/tmp/Мої файли"; exit 0; }; done
printf '/tmp/a|b.txt\037/tmp/line\nbreak\037/tmp/space \n'`)
	for _, kind := range []pickKind{pickDirectory, pickDestination} {
		if got, err := pickWithSystem(kind); err != nil || !slices.Equal(got, []string{"/tmp/Мої файли"}) {
			t.Fatalf("%s: %q %v", kind, got, err)
		}
	}
	want := []string{"/tmp/a|b.txt", "/tmp/line\nbreak", "/tmp/space "}
	if got, err := pickWithSystem(pickFiles); err != nil || !slices.Equal(got, want) {
		t.Fatalf("files: %q %v", got, err)
	}
}

func TestLinuxPickerCancelAndFallback(t *testing.T) {
	fakeTool(t, "zenity", "exit 1")
	if got, err := pickWithSystem(pickDirectory); err != nil || got != nil {
		t.Fatalf("cancel returned %v %v", got, err)
	}
	fakeTool(t, "kdialog", "echo /tmp/from-kdialog")
	if got, err := pickWithSystem(pickDirectory); err != nil || !slices.Equal(got, []string{"/tmp/from-kdialog"}) {
		t.Fatalf("kdialog returned %v %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := pickWithSystem(pickDirectory); err != errNoPicker {
		t.Fatalf("no picker returned %v", err)
	}
}

func TestLinuxPickerNeedsADisplay(t *testing.T) {
	fakeTool(t, "zenity", "exit 1")
	t.Setenv("DISPLAY", "")
	if _, err := pickWithSystem(pickFiles); err != errNoPicker {
		t.Fatalf("returned %v", err)
	}
}

func TestLinuxPickerRefusesPartialPaths(t *testing.T) {
	fakeTool(t, "zenity", "echo relative/path")
	if got, err := pickWithSystem(pickFiles); err == nil {
		t.Fatalf("returned %q", got)
	}
}

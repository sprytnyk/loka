//go:build !darwin && !windows

package desktop

import (
	"errors"
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
	for _, kind := range []Kind{Directory, Destination} {
		if got, err := Pick(kind); err != nil || !slices.Equal(got, []string{"/tmp/Мої файли"}) {
			t.Fatalf("%s: %q %v", kind, got, err)
		}
	}
	want := []string{"/tmp/a|b.txt", "/tmp/line\nbreak", "/tmp/space "}
	if got, err := Pick(Files); err != nil || !slices.Equal(got, want) {
		t.Fatalf("files: %q %v", got, err)
	}
}

func TestLinuxPickerCancelAndFallback(t *testing.T) {
	fakeTool(t, "zenity", "exit 1")
	if got, err := Pick(Directory); err != nil || got != nil {
		t.Fatalf("cancel returned %v %v", got, err)
	}
	fakeTool(t, "kdialog", "echo /tmp/from-kdialog")
	if got, err := Pick(Directory); err != nil || !slices.Equal(got, []string{"/tmp/from-kdialog"}) {
		t.Fatalf("kdialog returned %v %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Pick(Directory); !errors.Is(err, ErrNoPicker) {
		t.Fatalf("no picker returned %v", err)
	}
}

func TestLinuxPickerNeedsADisplay(t *testing.T) {
	fakeTool(t, "zenity", "exit 1")
	t.Setenv("DISPLAY", "")
	if _, err := Pick(Files); !errors.Is(err, ErrNoPicker) {
		t.Fatalf("returned %v", err)
	}
}

func TestLinuxPickerRefusesPartialPaths(t *testing.T) {
	fakeTool(t, "zenity", "echo relative/path")
	if got, err := Pick(Files); err == nil {
		t.Fatalf("returned %q", got)
	}
}

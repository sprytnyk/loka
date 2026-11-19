package transfer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// collectOne writes a file and chooses it for sending.
func collectOne(t *testing.T, dir string) (string, []Entry) {
	t.Helper()
	path := filepath.Join(dir, "chosen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _, _, err := Collect([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return path, entries
}

// TestSendingRefusesAFileOpenForWriting keeps a chosen file open for
// writing, the way an editor or a database can. Windows may not move the
// file's last write time until then, so Loka must refuse rather than read.
func TestSendingRefusesAFileOpenForWriting(t *testing.T) {
	path, entries := collectOne(t, t.TempDir())
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var done atomic.Int64
	if err := WriteTar(context.Background(), io.Discard, entries, &done); !errors.Is(err, errInUse) {
		t.Fatalf("got %v", err)
	}
}

// TestSendingAFileOpenForReading checks that a program that only reads the
// file doesn't stop it being sent.
func TestSendingAFileOpenForReading(t *testing.T) {
	path, entries := collectOne(t, t.TempDir())
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var done atomic.Int64
	if err := WriteTar(context.Background(), io.Discard, entries, &done); err != nil {
		t.Fatal(err)
	}
}

// TestSendingARelativePathThatIsLongInFull chooses a file by a relative
// path shorter than Windows' usual limit of 260 characters, from a working
// directory that makes the full path longer. The working directory itself
// stays short, since Windows can't change into a longer one.
func TestSendingARelativePathThatIsLongInFull(t *testing.T) {
	base := t.TempDir()
	rel := filepath.Join(strings.Repeat("a", 50), strings.Repeat("b", 50), strings.Repeat("c", 50), strings.Repeat("d", 50))
	collectOne(t, filepath.Join(base, rel))
	rel = filepath.Join(rel, "chosen")
	if full := len(filepath.Join(base, rel)); len(rel) >= 248 || full < 260 {
		t.Fatalf("the relative path has %d characters and the full one %d", len(rel), full)
	}
	t.Chdir(base)
	entries, _, _, err := Collect([]string{rel})
	if err != nil {
		t.Fatal(err)
	}
	var done atomic.Int64
	if err := WriteTar(context.Background(), io.Discard, entries, &done); err != nil {
		t.Fatal(err)
	}
}

// TestLinksAnswerWhileAFileIsOpenForWriting downloads links whose file, or
// a file in whose directory, is open for writing. Both must answer with a
// message to try again before any of the download is sent.
func TestLinksAnswerWhileAFileIsOpenForWriting(t *testing.T) {
	root := t.TempDir()
	file, _ := collectOne(t, filepath.Join(root, "dir"))
	f, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	one, err := NewLink([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	defer one.Stop()
	if _, _, err := one.OpenSharedFile(); !errors.Is(err, ErrLinkBusy) {
		t.Fatalf("single file link: got %v", err)
	}
	dir, err := NewLink([]string{filepath.Join(root, "dir")})
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Stop()
	if _, err := dir.LinkEntries(); !errors.Is(err, ErrLinkBusy) {
		t.Fatalf("directory link: got %v", err)
	}
}

// TestSendingAFileWithALongPath sends a file whose path is longer than
// Windows' usual limit of 260 characters.
func TestSendingAFileWithALongPath(t *testing.T) {
	dir := t.TempDir()
	for range 6 {
		dir = filepath.Join(dir, strings.Repeat("d", 50))
	}
	_, entries := collectOne(t, dir)
	var done atomic.Int64
	if err := WriteTar(context.Background(), io.Discard, entries, &done); err != nil {
		t.Fatal(err)
	}
}

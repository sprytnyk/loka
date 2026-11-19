package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sprytnyk/loka/internal/treetest"
)

type tarItem struct{ name, body string }

// makeTar builds a transfer stream; names ending in "/" are directories.
func makeTar(items ...tarItem) ([]byte, int64) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var size int64
	for _, it := range items {
		hdr := &tar.Header{Name: it.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(it.body))}
		if strings.HasSuffix(it.name, "/") {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		tw.WriteHeader(hdr)
		tw.Write([]byte(it.body))
		size += hdr.Size
	}
	tw.Close()
	return buf.Bytes(), size
}

func receiveTar(t *testing.T, destination string, nameFor func(string) string, items ...tarItem) error {
	t.Helper()
	data, size := makeTar(items...)
	var done atomic.Int64
	return Receive(context.Background(), bytes.NewReader(data), destination, size, &done, nameFor)
}

func identity(s string) string { return s }

func TestReceiveRefusesPathsOutsideTheDestination(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "destination")
	os.Mkdir(destination, 0o755)
	for _, name := range []string{"../escape", "a/../../escape", "/abs", "a//b", `a\b`, "./x"} {
		if err := receiveTar(t, destination, identity, tarItem{name, "bad"}); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "escape")); err == nil {
		t.Fatal("a file escaped the destination")
	}
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatalf("a refused transfer left %v behind", entries)
	}
}

func TestReceiveRefusesLinksAndWrongSizes(t *testing.T) {
	destination := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	var done atomic.Int64
	if err := Receive(context.Background(), &buf, destination, 0, &done, identity); err == nil {
		t.Error("a symbolic link was accepted")
	}

	data, size := makeTar(tarItem{"file", "abcdef"})
	if err := Receive(context.Background(), bytes.NewReader(data), destination, size-1, &done, identity); err == nil {
		t.Error("more data than offered was accepted")
	}
	if err := Receive(context.Background(), bytes.NewReader(data), destination, size+1, &done, identity); err == nil {
		t.Error("less data than offered was accepted")
	}
	if err := Receive(context.Background(), bytes.NewReader(data[:515]), destination, size, &done, identity); err == nil {
		t.Error("a cut-off stream was accepted")
	}
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatalf("refused transfers left %v behind", entries)
	}
}

func TestReceiveKeepsExistingItems(t *testing.T) {
	destination := t.TempDir()
	treetest.Write(t, destination, map[string]string{"source/old": "old"})
	err := receiveTar(t, destination, identity, tarItem{"source/", ""}, tarItem{"source/empty/", ""}, tarItem{"source/file", "new"})
	if err != nil {
		t.Fatal(err)
	}
	treetest.Same(t, treetest.Read(t, destination), map[string]string{
		"source/":           "dir",
		"source/old":        treetest.Read(t, destination)["source/old"],
		"source (1)/":       "dir",
		"source (1)/empty/": "dir",
		"source (1)/file":   treetest.Read(t, destination)["source (1)/file"],
	})
}

func TestReceiveKeepsBrokenSymlinksAtTheSameName(t *testing.T) {
	destination := t.TempDir()
	links := []string{"report.txt", "report (1).txt"}
	for _, link := range links {
		if err := os.Symlink("missing.txt", filepath.Join(destination, link)); err != nil {
			t.Skip("cannot create symbolic links:", err)
		}
	}
	if err := receiveTar(t, destination, identity, tarItem{"report.txt", "incoming"}); err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if target, err := os.Readlink(filepath.Join(destination, link)); err != nil || target != "missing.txt" {
			t.Errorf("%s now points to %q, %v", link, target, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(destination, "report (2).txt")); string(data) != "incoming" {
		t.Fatalf("got %q", data)
	}
}

func TestReceiveAdaptsWindowsNames(t *testing.T) {
	destination := t.TempDir()
	err := receiveTar(t, destination, windowsName,
		tarItem{"Set: live?/Track?.mp3", "renamed"},
		tarItem{"Set: live?/Track_.mp3", "unchanged name, numbered"},
		tarItem{"Set_ live_/CON.txt", "reserved"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"Set_ live_/Track_.mp3":     "renamed",
		"Set_ live_/Track_ (1).mp3": "unchanged name, numbered",
		"Set_ live_ (1)/_CON.txt":   "reserved",
	} {
		if data, err := os.ReadFile(filepath.Join(destination, path)); err != nil || string(data) != want {
			t.Errorf("%s: got %q, %v", path, data, err)
		}
	}
}

func TestWindowsName(t *testing.T) {
	for in, want := range map[string]string{
		"plain.txt":     "plain.txt",
		"a:b?c*d.mp3":   "a_b_c_d.mp3",
		"trailing. . ":  "trailing",
		"...":           "_",
		"CON":           "_CON",
		"con.txt":       "_con.txt",
		"Com1.tar.gz":   "_Com1.tar.gz",
		"COM10":         "COM10",
		"LPT¹":          "_LPT¹",
		"CONSOLE":       "CONSOLE",
		"tab\there":     "tab_here",
		"Set: live?":    "Set_ live_",
		"Set_ What’s ?": "Set_ What’s _",
	} {
		if got := windowsName(in); got != want {
			t.Errorf("windowsName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollectNamesItemsApart(t *testing.T) {
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{
		"first/report.txt/empty/": "",
		"second/report.txt":       "second",
		"third/report (1).txt":    "third",
	})
	entries, items, size, err := Collect([]string{
		filepath.Join(root, "first", "report.txt"),
		filepath.Join(root, "second", "report.txt"),
		filepath.Join(root, "third", "report (1).txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"report.txt", "report (2).txt", "report (1).txt"}; !slices.Equal(items, want) {
		t.Fatalf("items %v, want %v", items, want)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
	}
	if want := []string{"report.txt", "report.txt/empty", "report (2).txt", "report (1).txt"}; !slices.Equal(names, want) {
		t.Fatalf("entries %v, want %v", names, want)
	}
	if size != int64(len("second")+len("third")) {
		t.Fatalf("size %d", size)
	}
}

func TestCollectLeavesOutLinksInsideDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need extra rights on Windows")
	}
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"dir/file": "x", "elsewhere/secret": "s"})
	os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(root, "dir", "linked-dir"))
	os.Symlink(filepath.Join(root, "elsewhere", "secret"), filepath.Join(root, "dir", "linked-file"))
	os.Symlink(filepath.Join(root, "dir", "file"), filepath.Join(root, "top-link"))

	entries, _, _, err := Collect([]string{filepath.Join(root, "dir")})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.name, "linked") {
			t.Errorf("%s was included", e.name)
		}
	}
	if _, _, _, err := Collect([]string{filepath.Join(root, "top-link")}); err == nil {
		t.Error("a chosen symbolic link was accepted")
	}
	if _, _, _, err := Collect([]string{filepath.Join(root, "missing")}); err == nil {
		t.Error("a missing path was accepted")
	}
}

func TestSendingFailsWhenAFileChanges(t *testing.T) {
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"grows": "abc", "shrinks": "abcdef"})
	for name, change := range map[string]string{"grows": "abcdef", "shrinks": "a"} {
		entries, _, _, err := Collect([]string{filepath.Join(root, name)})
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, name), []byte(change), 0o644)
		var done atomic.Int64
		var buf bytes.Buffer
		if err := WriteTar(context.Background(), &buf, entries, &done); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestReceiveStopsWhenCancelled(t *testing.T) {
	destination := t.TempDir()
	data, size := makeTar(tarItem{"file", "abc"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var done atomic.Int64
	err := Receive(ctx, bytes.NewReader(data), destination, size, &done, identity)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestReceiveMovesNothingWhenOneItemCannotMove(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("relies on Linux's 255-byte name limit")
	}
	destination := t.TempDir()
	long := strings.Repeat("b", 252) // "bbb… (1)" is too long a name
	treetest.Write(t, destination, map[string]string{long: "old"})
	before := treetest.Read(t, destination)
	err := receiveTar(t, destination, identity, tarItem{"a", "new"}, tarItem{long, "new"})
	if err == nil {
		t.Fatal("receive succeeded")
	}
	treetest.Same(t, treetest.Read(t, destination), before)
}

// cancelAtEnd cancels once its last byte is read, as if a cancel arrived
// with the last bytes.
type cancelAtEnd struct {
	r      *bytes.Reader
	cancel context.CancelFunc
}

func (c cancelAtEnd) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if c.r.Len() == 0 {
		c.cancel()
	}
	return n, err
}

func TestReceiveMovesNothingWhenCancelledAtTheEnd(t *testing.T) {
	destination := t.TempDir()
	data, size := makeTar(tarItem{"file", "abc"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var done atomic.Int64
	err := Receive(ctx, cancelAtEnd{bytes.NewReader(data), cancel}, destination, size, &done, identity)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	treetest.Same(t, treetest.Read(t, destination), map[string]string{})
}

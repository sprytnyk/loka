package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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
	got := treetest.Read(t, destination)
	treetest.Same(t, got, map[string]string{
		"source/":           "dir",
		"source/old":        got["source/old"],
		"source (1)/":       "dir",
		"source (1)/empty/": "dir",
		"source (1)/file":   got["source (1)/file"],
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
	staging, destination := t.TempDir(), t.TempDir()
	treetest.Write(t, staging, map[string]string{"a": "new"})
	treetest.Write(t, destination, map[string]string{"old": "old"})
	before := treetest.Read(t, destination)
	// "b" is missing from staging, so it cannot move after "a" did.
	staged := map[string]string{"a": filepath.Join(staging, "a"), "b": filepath.Join(staging, "b")}
	if err := publish(staged, []string{"a", "b"}, destination); err == nil {
		t.Fatal("publish succeeded")
	}
	treetest.Same(t, treetest.Read(t, destination), before)
	if _, err := os.Lstat(staged["a"]); err != nil {
		t.Fatalf("a was not moved back: %v", err)
	}
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

func TestSendingRefusesAReplacedFile(t *testing.T) {
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"dir/chosen": "abc", "other": "xyz"})
	entries, _, _, err := Collect([]string{filepath.Join(root, "dir")})
	if err != nil {
		t.Fatal(err)
	}
	// Replace it in one step with a file of the same size, the way an
	// editor saves.
	if err := os.Rename(filepath.Join(root, "other"), filepath.Join(root, "dir", "chosen")); err != nil {
		t.Fatal(err)
	}
	var done atomic.Int64
	var buf bytes.Buffer
	if err := WriteTar(context.Background(), &buf, entries, &done); !errors.Is(err, errReplaced) {
		t.Fatalf("got %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("xyz")) {
		t.Fatal("the replacement was sent")
	}
}

// waitPastChangeTime lets the clock move past the times just given to a
// file, which some systems keep to a few milliseconds.
func waitPastChangeTime() { time.Sleep(50 * time.Millisecond) }

// TestSendingRefusesADeletedAndRecreatedFile deletes a chosen file and
// writes a new one in its place, again and again. Unix systems often give
// the new file the old one's inode number, so only its later change time
// tells it apart from the chosen file.
func TestSendingRefusesADeletedAndRecreatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chosen")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitPastChangeTime()
	entries, _, _, err := Collect([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 256 {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(path); err == nil && os.SameFile(entries[0].info, info) {
			t.Logf("the new file took the chosen file's identity after %d tries", i+1)
			break
		}
	}
	var done atomic.Int64
	var buf bytes.Buffer
	if err := WriteTar(context.Background(), &buf, entries, &done); !errors.Is(err, errReplaced) {
		t.Fatalf("got %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("new")) {
		t.Fatal("the replacement was sent")
	}
}

// TestHoldingRecordsTheHeldFile replaces a file between looking at it and
// opening it, with the new file taking the old one's identity. What is
// recorded must then describe the file that is held, not the one looked at.
func TestHoldingRecordsTheHeldFile(t *testing.T) {
	if !holdsFiles {
		t.Skip("nothing is held on this system")
	}
	path := filepath.Join(t.TempDir(), "chosen")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen, err := lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	reused := false
	for range 256 {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("longer"), 0o600); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(path); err == nil && os.SameFile(seen, info) {
			reused = true
			break
		}
	}
	if !reused {
		t.Skip("the filesystem did not reuse the identity")
	}
	h := newHolder()
	defer h.release()
	info, err := h.open(path, seen)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len("longer")) {
		t.Fatalf("recorded %d bytes for the held file", info.Size())
	}
}

// TestLinksShareOneBudget checks that even a low open file limit leaves
// room to share an item, and that links share the room.
func TestLinksShareOneBudget(t *testing.T) {
	if !holdsFiles {
		t.Skip("nothing is held on this system")
	}
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"a": "1", "b": "2", "c": "3"})
	defer func(limit func() int) { openFileLimit = limit }(openFileLimit)
	hold := func(names ...string) (*holder, error) {
		h := newHolder()
		for _, name := range names {
			path := filepath.Join(root, name)
			info, err := lstat(path)
			if err == nil {
				_, err = h.open(path, info)
			}
			if err != nil {
				h.release()
				return nil, err
			}
		}
		return h, nil
	}

	openFileLimit = func() int { return 256 }
	h, err := hold("a")
	if err != nil {
		t.Fatal(err)
	}
	h.release()

	openFileLimit = func() int { return 4 } // room for 3
	first, err := hold("a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold("a"); err == nil || !strings.Contains(err.Error(), "only 3 items") {
		t.Fatalf("got %v", err)
	}
	first.release()
	second, err := hold("a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	second.release()
	if n := heldFiles.Load(); n != 0 {
		t.Fatalf("%d files still counted as held", n)
	}
}

// TestSendingAFolderKeepsNoFilesOpen sends a folder of more files than the
// open file limit allows, which works because nothing is held open while
// the transfer waits.
func TestSendingAFolderKeepsNoFilesOpen(t *testing.T) {
	root := t.TempDir()
	tree := map[string]string{}
	for i := range 64 {
		tree[fmt.Sprintf("dir/%d", i)] = fmt.Sprint(i)
	}
	treetest.Write(t, root, tree)
	defer func(limit func() int) { openFileLimit = limit }(openFileLimit)
	openFileLimit = func() int { return 4 }

	entries, _, _, err := Collect([]string{filepath.Join(root, "dir")})
	if err != nil {
		t.Fatal(err)
	}
	var done atomic.Int64
	if err := WriteTar(context.Background(), io.Discard, entries, &done); err != nil {
		t.Fatal(err)
	}
	if n := heldFiles.Load(); n != 0 {
		t.Fatalf("%d files held", n)
	}
}

// TestSendingRefusesAFileChangedAfterChoosing changes a chosen file
// without changing its identity or size. Its stamp moves, the same as a new
// file in its place would get, so it is refused.
func TestSendingRefusesAFileChangedAfterChoosing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(path string) error
	}{
		{"edit", func(path string) error {
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, err = f.WriteString("xyz")
			return errors.Join(err, f.Close())
		}},
		{"chmod", func(path string) error {
			if runtime.GOOS == "windows" {
				t.Skip("Windows doesn't record permission changes in the times Loka reads")
			}
			return os.Chmod(path, 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chosen")
			if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
				t.Fatal(err)
			}
			waitPastChangeTime()
			entries, _, _, err := Collect([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			waitPastChangeTime()
			if err := tc.change(path); err != nil {
				t.Fatal(err)
			}
			var done atomic.Int64
			if err := WriteTar(context.Background(), io.Discard, entries, &done); !errors.Is(err, errReplaced) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// editWhileSent edits path in place, at the same size, once the stream
// is past the file's tar header and into its bytes. On Windows the edit
// must be refused, which blocked records.
type editWhileSent struct {
	t       *testing.T
	path    string
	written int
	edited  bool
	blocked bool
}

func (w *editWhileSent) Write(b []byte) (int, error) {
	w.written += len(b)
	if !w.edited && w.written > 512 {
		w.edited = true
		waitPastChangeTime()
		f, err := os.OpenFile(w.path, os.O_WRONLY, 0)
		if err != nil && runtime.GOOS == "windows" {
			w.blocked = true
			return len(b), nil
		}
		if err != nil {
			w.t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("new"), 0); err != nil {
			w.t.Fatal(err)
		}
		f.Close()
	}
	return len(b), nil
}

// TestSendingRefusesAFileEditedWhileSent edits a file at the same size
// while it is being read, so the count of bytes alone can't tell. Windows
// doesn't let the edit happen at all.
func TestSendingRefusesAFileEditedWhileSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chosen")
	if err := os.WriteFile(path, []byte("old"+strings.Repeat(".", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	waitPastChangeTime()
	entries, _, _, err := Collect([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	var done atomic.Int64
	w := &editWhileSent{t: t, path: path}
	err = WriteTar(context.Background(), w, entries, &done)
	if w.blocked {
		if err != nil {
			t.Fatalf("the edit was blocked, yet sending failed: %v", err)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Fatal("another program could write to the file while it was sent")
	}
	if err == nil || !strings.Contains(err.Error(), "changed while it was being sent") {
		t.Fatalf("got %v", err)
	}
}

func TestNumberedFitsTheNameLimit(t *testing.T) {
	for _, tc := range []struct{ name, suffix string }{
		{strings.Repeat("a", 251) + ".txt", " (1).txt"},
		{strings.Repeat("я", 125) + ".txt", " (1).txt"},
		{strings.Repeat("a", 255), " (1)"},
		{"a." + strings.Repeat("e", 253), " (1)"}, // the extension alone is too long to keep
	} {
		got := numbered(tc.name, 1)
		if len(got) > maxNameBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, tc.suffix) {
			t.Errorf("numbered(%q) = %q (%d bytes)", tc.name, got, len(got))
		}
	}
}

// TestSendingTwoItemsWithTheSameLongName sends two files whose shared name
// is as long as names can be, so the second one's numbered name must be
// shortened to fit.
func TestSendingTwoItemsWithTheSameLongName(t *testing.T) {
	root, destination := t.TempDir(), t.TempDir()
	long := strings.Repeat("a", 251) + ".txt"
	treetest.Write(t, root, map[string]string{"one/" + long: "1", "two/" + long: "2"})
	entries, items, size, err := Collect([]string{filepath.Join(root, "one", long), filepath.Join(root, "two", long)})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	var done atomic.Int64
	if err := WriteTar(context.Background(), &buf, entries, &done); err != nil {
		t.Fatal(err)
	}
	if err := Receive(context.Background(), &buf, destination, size, &done, identity); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"1", "2"} {
		if got, err := os.ReadFile(filepath.Join(destination, items[i])); err != nil || string(got) != want {
			t.Errorf("%s: got %q, %v", items[i], got, err)
		}
	}
	if found, _ := os.ReadDir(destination); len(found) != 2 {
		t.Errorf("destination has %d items, want 2", len(found))
	}
}

func TestReceiveFailsOnANameTooLong(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("relies on Linux's 255-byte name limit")
	}
	destination := t.TempDir()
	result := make(chan error, 1)
	go func() { result <- receiveTar(t, destination, identity, tarItem{strings.Repeat("a", 300), "x"}) }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("receive succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receive did not end")
	}
	treetest.Same(t, treetest.Read(t, destination), map[string]string{})
}

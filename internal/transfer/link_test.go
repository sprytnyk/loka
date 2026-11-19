package transfer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/treetest"
)

// openFiles counts this process's open files, where /proc lists them.
func openFiles(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("needs /proc/self/fd")
	}
	return len(fds)
}

func TestLinkLetsGoOfItsItemsWhenStopped(t *testing.T) {
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"dir/a": "a", "file": "f"})
	before := openFiles(t)
	link, err := NewLink([]string{filepath.Join(root, "dir"), filepath.Join(root, "file")})
	if err != nil {
		t.Fatal(err)
	}
	if held := openFiles(t) - before; held != 2 {
		t.Fatalf("link holds %d files, want 2", held)
	}
	link.Stop()
	for deadline := time.Now().Add(time.Second); openFiles(t) != before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("still holding %d files after Stop", openFiles(t)-before)
		}
	}
}

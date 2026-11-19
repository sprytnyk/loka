package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sprytnyk/loka/internal/treetest"
)

// TestLinksAnswerConflictWhileAFileIsOpenForWriting downloads a link to a
// file, and one to its directory, while the file is open for writing. Both
// answer 409 and must not be cached, so a retry after the file is closed
// gets the file.
func TestLinksAnswerConflictWhileAFileIsOpenForWriting(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"dir/file": "abc"})
	path := filepath.Join(root, "dir", "file")
	one := addLink(t, s, path)
	dir := addLink(t, s, filepath.Join(root, "dir"))

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{one.Token, dir.Token} {
		resp := get(t, base+"/download/"+token)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusConflict || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(string(body), "Try again later") {
			t.Fatalf("answered %d, Cache-Control %q: %s", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
		}
	}
	f.Close()
	for _, token := range []string{one.Token, dir.Token} {
		if resp := get(t, base+"/download/"+token); resp.StatusCode != http.StatusOK {
			t.Fatalf("after the file closed, answered %d", resp.StatusCode)
		}
	}
}

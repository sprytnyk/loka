package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/treetest"
)

func get(t *testing.T, url string, header ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestLinkServesManyUnicodeFilesAsZip shares a directory of 1,200 files
// with Unicode names, downloads the link, and compares every file.
func TestLinkServesManyUnicodeFilesAsZip(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	tree := map[string]string{"music/empty/": ""}
	for i := range 1200 {
		tree[fmt.Sprintf("music/Збірка – 音楽 – 🎵 é 100%% #%d.mp3", i)] = fmt.Sprint(i)
	}
	if runtime.GOOS != "windows" {
		tree["music/Set: What’s next?.mp3"] = "punctuation"
	}
	treetest.Write(t, root, tree)

	var link struct{ ID, Token string }
	paths := []string{filepath.Join(root, "music"), filepath.Join(root, "music", "empty")}
	if code := api(t, s, base, "/api/share", map[string]any{"paths": paths}, &link); code != http.StatusOK {
		t.Fatalf("share answered %d", code)
	}
	resp := get(t, base+"/download/"+link.Token)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), shareArchiveName) {
		t.Fatalf("download answered %d %q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	data, _ := io.ReadAll(resp.Body)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			got[f.Name] = "dir"
			continue
		}
		rc, _ := f.Open()
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		sum := sha256.Sum256(content)
		got[f.Name] = hex.EncodeToString(sum[:])
	}
	want := treetest.Read(t, root)
	want["empty/"] = "dir" // the second chosen path
	treetest.Same(t, got, want)
}

func TestLinkServesASingleFileAsItself(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"Звіт 100%.txt": "hello world"})
	link, err := transfer.NewLink([]string{filepath.Join(root, "Звіт 100%.txt")})
	if err != nil {
		t.Fatal(err)
	}
	s.transfers.Add(link)

	resp := get(t, base+"/download/"+link.Token)
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello world" {
		t.Fatalf("got %q", body)
	}
	if got := resp.Header.Get("Content-Disposition"); got != "attachment; filename*=UTF-8''%D0%97%D0%B2%D1%96%D1%82%20100%25.txt" {
		t.Fatalf("Content-Disposition %q", got)
	}
	resp = get(t, base+"/download/"+link.Token, "Range", "bytes=6-")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(body) != "world" {
		t.Fatalf("resume answered %d %q", resp.StatusCode, body)
	}
}

// zipNames downloads a ZIP link and returns the names inside it.
func zipNames(t *testing.T, url string) []string {
	t.Helper()
	resp := get(t, url)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("link answered %d %s", resp.StatusCode, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func TestLinkServesCurrentContents(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"dir/a": "a", "file": "old"})
	files, _ := transfer.NewLink([]string{filepath.Join(root, "file")})
	dir, _ := transfer.NewLink([]string{filepath.Join(root, "dir")})
	s.transfers.Add(files)
	s.transfers.Add(dir)

	treetest.Write(t, root, map[string]string{"dir/b": "b"})
	if got := zipNames(t, base+"/download/"+dir.Token); !slices.Equal(got, []string{"dir/", "dir/a", "dir/b"}) {
		t.Fatalf("zip has %q", got)
	}
	f, err := os.OpenFile(filepath.Join(root, "file"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("edited")
	f.Close()
	if body, _ := io.ReadAll(get(t, base+"/download/"+files.Token).Body); string(body) != "edited" {
		t.Fatalf("got %q", body)
	}
}

func TestLinkRefusesReplacedItems(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"dir/a": "a", "file": "old"})
	file, _ := transfer.NewLink([]string{filepath.Join(root, "file")})
	dir, _ := transfer.NewLink([]string{filepath.Join(root, "dir")})
	s.transfers.Add(file)
	s.transfers.Add(dir)

	// Write the replacements first and move them into place, the way
	// editors save, so they cannot reuse the old items' inode numbers.
	treetest.Write(t, root, map[string]string{"new dir/a": "other", "new file": "other"})
	for _, name := range []string{"file", "dir"} {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(root, "new "+name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, link := range []*transfer.Transfer{file, dir} {
		resp := get(t, base+"/download/"+link.Token)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no longer available") {
			t.Fatalf("replaced %v answered %d %s", link.Items, resp.StatusCode, body)
		}
	}
}

func TestStoppedLinkExpires(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	treetest.Write(t, root, map[string]string{"file": "x"})
	link, _ := transfer.NewLink([]string{filepath.Join(root, "file")})
	s.transfers.Add(link)
	api(t, s, base, "/api/cancel", map[string]string{"id": link.ID}, nil)
	if resp := get(t, base+"/download/"+link.Token); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stopped link answered %d", resp.StatusCode)
	}
	if resp := get(t, base+"/download/unknown"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown link answered %d", resp.StatusCode)
	}
	if v := link.View(); v.Status != transfer.Stopped {
		t.Fatalf("status %q", v.Status)
	}
}

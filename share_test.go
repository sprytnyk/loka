package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	writeTree(t, root, tree)

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
	want := readTree(t, root)
	want["empty/"] = "dir" // the second chosen path
	sameTree(t, got, want)
}

func TestLinkServesASingleFileAsItself(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"Звіт 100%.txt": "hello world"})
	link, err := newLink([]string{filepath.Join(root, "Звіт 100%.txt")})
	if err != nil {
		t.Fatal(err)
	}
	s.transfers.add(link)

	resp := get(t, base+"/download/"+link.token)
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello world" {
		t.Fatalf("got %q", body)
	}
	if got := resp.Header.Get("Content-Disposition"); got != "attachment; filename*=UTF-8''%D0%97%D0%B2%D1%96%D1%82%20100%25.txt" {
		t.Fatalf("Content-Disposition %q", got)
	}
	resp = get(t, base+"/download/"+link.token, "Range", "bytes=6-")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(body) != "world" {
		t.Fatalf("resume answered %d %q", resp.StatusCode, body)
	}
}

func TestStoppedLinkExpires(t *testing.T) {
	s, base := startServer(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"file": "x"})
	link, _ := newLink([]string{filepath.Join(root, "file")})
	s.transfers.add(link)
	api(t, s, base, "/api/cancel", map[string]string{"id": link.ID}, nil)
	if resp := get(t, base+"/download/"+link.token); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stopped link answered %d", resp.StatusCode)
	}
	if resp := get(t, base+"/download/unknown"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown link answered %d", resp.StatusCode)
	}
	if v := link.view(); v.Status != statusStopped {
		t.Fatalf("status %q", v.Status)
	}
}

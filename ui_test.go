package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDecodePaths(t *testing.T) {
	a := filepath.Join(t.TempDir(), "line\nbreak ")
	out, _ := json.Marshal([]string{a})
	if got, err := decodePaths(append(out, "\r\n"...)); err != nil || !slices.Equal(got, []string{a}) {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err := decodePaths([]byte("[]")); err != nil || len(got) != 0 {
		t.Fatalf("empty answer gave %q %v", got, err)
	}
	for _, bad := range []string{`["relative"]`, "not json"} {
		if _, err := decodePaths([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestPageCarriesKeyAndVersion(t *testing.T) {
	s, base := startServer(t)
	s.version = "1.2.3"
	resp := get(t, base+"/")
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"const key='" + s.key + "'", ">v1.2.3<", "Preparing ${item.label}…", " · ${transfer.progress}%"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	req.Host = "evil.example"
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("another site got the page: %d", resp.StatusCode)
	}
}

func uploadItem(t *testing.T, s *server, base, session, path, body string, dir bool) (string, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/upload", bytes.NewBufferString(body))
	req.Header.Set("X-Loka-Key", s.key)
	req.Header.Set("X-Upload-Session", session)
	req.Header.Set("X-File-Path", url.PathEscape(path))
	if dir {
		req.Header.Set("X-Entry-Type", "directory")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply struct{ Path string }
	json.NewDecoder(resp.Body).Decode(&reply)
	return reply.Path, resp.StatusCode
}

func TestUploadKeepsEmptyDirectoriesAndDoesNotOverwrite(t *testing.T) {
	s, base := startServer(t)
	t.Cleanup(s.close)
	staged, code := uploadItem(t, s, base, "abc123", "parent/empty", "", true)
	if code != http.StatusOK {
		t.Fatalf("directory upload answered %d", code)
	}
	if info, err := os.Stat(filepath.Join(staged, "empty")); err != nil || !info.IsDir() {
		t.Fatal("the empty directory is missing")
	}
	name := "parent/Збірка – 🎵 é 100% #1.mp3"
	if _, code := uploadItem(t, s, base, "abc123", name, "first", false); code != http.StatusOK {
		t.Fatalf("file upload answered %d", code)
	}
	if _, code := uploadItem(t, s, base, "abc123", name, "second", false); code != http.StatusBadRequest {
		t.Fatalf("second upload of the same file answered %d", code)
	}
	if data, _ := os.ReadFile(filepath.Join(staged, "Збірка – 🎵 é 100% #1.mp3")); string(data) != "first" {
		t.Fatalf("got %q", data)
	}
	for _, bad := range []string{"../escape", "a/../../b", "/abs"} {
		if _, code := uploadItem(t, s, base, "abc123", bad, "x", false); code != http.StatusBadRequest {
			t.Errorf("%q answered %d", bad, code)
		}
	}
	if _, code := uploadItem(t, s, base, "../x", "file", "x", false); code != http.StatusBadRequest {
		t.Errorf("a bad session answered %d", code)
	}
	s.close()
	if _, err := os.Stat(s.temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("closing kept the uploaded files")
	}
}

func TestPickReturnsChosenPaths(t *testing.T) {
	s, base := startServer(t)
	var asked []pickKind
	s.pick = func(kind pickKind) ([]string, error) {
		asked = append(asked, kind)
		if kind == pickDirectory {
			return []string{"/home/me/Music"}, nil
		}
		return nil, nil
	}
	var reply struct{ Paths []string }
	api(t, s, base, "/api/pick", map[string]string{"kind": "directory"}, &reply)
	if !slices.Equal(reply.Paths, []string{"/home/me/Music"}) {
		t.Fatalf("directory pick returned %v", reply.Paths)
	}
	for _, kind := range []string{"files", "destination"} {
		api(t, s, base, "/api/pick", map[string]string{"kind": kind}, &reply)
		if reply.Paths == nil || len(reply.Paths) != 0 {
			t.Fatalf("a cancelled pick returned %#v", reply.Paths)
		}
	}
	var failure struct{ Error string }
	if code := api(t, s, base, "/api/pick", map[string]string{"kind": "folder"}, &failure); code != http.StatusBadRequest {
		t.Fatalf("an unknown kind answered %d", code)
	}
	s.pick = func(pickKind) ([]string, error) { return nil, errNoPicker }
	if code := api(t, s, base, "/api/pick", map[string]string{"kind": "files"}, &failure); code != http.StatusBadRequest || !strings.Contains(failure.Error, "Enter a path") {
		t.Fatalf("missing picker answered %d %q", code, failure.Error)
	}
	if !slices.Equal(asked, []pickKind{pickDirectory, pickFiles, pickDestination}) {
		t.Fatalf("asked for %v", asked)
	}
}

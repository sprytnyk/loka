package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sprytnyk/loka/internal/desktop"
)

func TestPageCarriesKeyAndVersion(t *testing.T) {
	s, base := startServer(t)
	s.version = "1.2.3"
	resp := get(t, base+"/")
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"const key = '" + s.key + "'", ">v1.2.3<", "Copying {name}…", "Надіслати на пристрій", " · ${transfer.progress}%"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(page, "const mode = 'desktop'") {
		t.Error("this device got the phone view")
	}
	// Another site, or another device, gets the upload view without the key.
	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	req.Host = "evil.example"
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	page = string(body)
	if resp.StatusCode != http.StatusOK || strings.Contains(page, s.key) || !strings.Contains(page, "const mode = 'phone'") {
		t.Fatalf("another site got %d, key shown %v", resp.StatusCode, strings.Contains(page, s.key))
	}
}

// uploadItem uploads one dropped file, or a directory when dir is true, the
// way the page does. It returns the path the server answers and the status.
func uploadItem(t *testing.T, s *Server, base, session, path, body string, dir bool) (string, int) {
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
	t.Cleanup(s.Close)
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
	s.Close()
	if _, err := os.Stat(s.temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("closing kept the uploaded files")
	}
}

func TestPickReturnsChosenPaths(t *testing.T) {
	s, base := startServer(t)
	var asked []desktop.Kind
	s.pick = func(kind desktop.Kind) ([]string, error) {
		asked = append(asked, kind)
		if kind == desktop.Directory {
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
	s.pick = func(desktop.Kind) ([]string, error) { return nil, desktop.ErrNoPicker }
	if code := api(t, s, base, "/api/pick", map[string]string{"kind": "files"}, &failure); code != http.StatusBadRequest || !strings.Contains(failure.Error, "unavailable") {
		t.Fatalf("missing picker answered %d %q", code, failure.Error)
	}
	if !slices.Equal(asked, []desktop.Kind{desktop.Directory, desktop.Files, desktop.Destination}) {
		t.Fatalf("asked for %v", asked)
	}
}

func TestQRCodeForALink(t *testing.T) {
	s, base := startServer(t)
	var reply struct{ SVG, Error string }
	if code := api(t, s, base, "/api/qr", map[string]string{"text": "http://192.0.2.10:45873/"}, &reply); code != http.StatusOK || !strings.HasPrefix(reply.SVG, "<svg") {
		t.Fatalf("got %d %+v", code, reply)
	}
	if code := api(t, s, base, "/api/qr", map[string]string{"text": strings.Repeat("x", 300)}, &reply); code != http.StatusBadRequest || reply.Error == "" {
		t.Fatalf("too long: got %d %+v", code, reply)
	}
}

// TestWithoutAnIdentityOnlyLinksAreServed checks a server like loka share's,
// which can't accept transfers, offers neither the page nor the phone view.
func TestWithoutAnIdentityOnlyLinksAreServed(t *testing.T) {
	ts := httptest.NewServer(New("share", "test").Handler())
	t.Cleanup(ts.Close)
	for _, path := range []string{"/", "/peer/offer"} {
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d", path, resp.StatusCode)
		}
	}
}

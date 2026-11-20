package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sprytnyk/loka/internal/desktop"
	"github.com/sprytnyk/loka/internal/transfer"
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
	for _, want := range []string{"const key = '" + s.key + "'", ">v1.2.3<", "const caFingerprint = '" + s.Trust.CAFingerprint() + "'", "Copying {name}…", "Надіслати на пристрій", " · ${transfer.progress}%"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(page, "const mode = 'desktop'") {
		t.Error("this device got the phone view")
	}
	// Another site or device gets, without the key, the view that moves
	// it to HTTPS, and over HTTPS the upload view.
	for _, c := range []struct {
		client *http.Client
		url    string
		mode   string
	}{
		{http.DefaultClient, base + "/", "check"},
		{browser(s), secure(base) + "/", "phone"},
	} {
		req, _ := http.NewRequest(http.MethodGet, c.url, nil)
		req.Host = "evil.example"
		resp, err := c.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		page = string(body)
		if resp.StatusCode != http.StatusOK || strings.Contains(page, s.key) || !strings.Contains(page, "const mode = '"+c.mode+"'") {
			t.Fatalf("%s: another site got %d, key shown %v", c.url, resp.StatusCode, strings.Contains(page, s.key))
		}
	}
}

// TestPlainHTTPGivesOtherDevicesOnlyTheWayToHTTPS checks that over plain
// HTTP another device gets the page that moves it to HTTPS and the
// certificate authority, and nothing else.
func TestPlainHTTPGivesOtherDevicesOnlyTheWayToHTTPS(t *testing.T) {
	s, base := startServer(t)
	link, err := transfer.NewLink([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.Transfers.Add(link)
	fetch := func(path string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Host = "192.0.2.10:45873"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	if resp, body := fetch("/"); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "const mode = 'check'") {
		t.Fatalf("the page answered %d", resp.StatusCode)
	}
	resp, body := fetch(CAPath)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, s.Trust.CA()) || resp.Header.Get("Content-Type") != "application/x-x509-ca-cert" {
		t.Fatalf("the certificate answered %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, path := range []string{"/download/" + link.Token, "/api/state", "/peer/status/x"} {
		if resp, _ := fetch(path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %d", path, resp.StatusCode)
		}
	}
	resp, err = browser(s).Get(secure(base) + "/download/" + link.Token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("over HTTPS the link answered %d", resp.StatusCode)
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

// TestShareOnlyServesLinks checks a server like loka share's, which can't
// accept transfers, offers neither the page nor the phone view.
func TestShareOnlyServesLinks(t *testing.T) {
	s := New("share", "test")
	s.Trust = openTrust(t)
	s.OnlyLinks = true
	base := serve(t, s, nil)
	for _, url := range []string{base, secure(base)} {
		resp, err := browser(s).Post(url+"/peer/offer", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d", url, resp.StatusCode)
		}
		resp, err = browser(s).Get(url + "/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "const mode = 'check'") {
			t.Errorf("%s gave another view", url)
		}
	}
}

// TestCheckPageSaysWhenTheCertificateCantHelp serves the check page at an
// address Loka's authority can't vouch for, and checks it says so.
func TestCheckPageSaysWhenTheCertificateCantHelp(t *testing.T) {
	s := New("desktop", "test")
	s.Trust = openTrust(t)
	for _, c := range []struct {
		local string
		want  string
	}{{"192.168.1.20:45873", "const vouches = true;"}, {"203.0.113.5:45873", "const vouches = false;"}} {
		r := httptest.NewRequest(http.MethodGet, "http://"+c.local+"/", nil)
		r.RemoteAddr = "192.168.1.30:50000"
		local, _ := net.ResolveTCPAddr("tcp", c.local)
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		body := w.Body.String()
		if !strings.Contains(body, c.want) || strings.Contains(body, "__LOKA_") {
			t.Errorf("%s: page lacks %q or keeps a placeholder", c.local, c.want)
		}
	}
}

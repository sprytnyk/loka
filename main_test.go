package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// output is a buffer the command writes to while the test reads it.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// wait returns the first match of pattern in the output, waiting for it.
func (o *output) wait(t *testing.T, pattern string) []string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var match []string
	waitFor(t, "output matching "+pattern, func() bool {
		match = re.FindStringSubmatch(o.String())
		return match != nil
	})
	return match
}

// runLoka runs the command line in the background and returns its output,
// a way to stop it, and its exit code once stopped.
func runLoka(t *testing.T, stdin io.Reader, args ...string) (*output, context.CancelFunc, <-chan int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := &output{}
	code := make(chan int, 1)
	go func() { code <- run(ctx, args, stdin, out, out) }()
	t.Cleanup(cancel)
	return out, cancel, code
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		args    []string
		command string
		paths   []string
		to      string
		code    int
	}{
		{nil, "serve", nil, "", -1},
		{[]string{"send", "a", "--to", "h", "b"}, "send", []string{"a", "b"}, "h", -1},
		{[]string{"--to=h", "send", "a"}, "send", []string{"a"}, "h", -1},
		{[]string{"send", "--to", "h", "--", "-dash", "--to"}, "send", []string{"-dash", "--to"}, "h", -1},
		{[]string{"share", "x", "--port", "0"}, "share", []string{"x"}, "", -1},
		{[]string{"--version"}, "", nil, "", -1},
		{[]string{"-h"}, "", nil, "", 0},
		{[]string{"file.txt"}, "", nil, "", 2},
		{[]string{"send", "a"}, "", nil, "", 2},
		{[]string{"share"}, "", nil, "", 2},
		{[]string{"devices", "x"}, "", nil, "", 2},
		{[]string{"--bogus"}, "", nil, "", 2},
	} {
		o, code := parse(c.args, io.Discard)
		if code != c.code || code == -1 && (o.command != c.command || !slices.Equal(o.paths, c.paths) || o.to != c.to) {
			t.Errorf("%q: got %q %q to=%q code %d", c.args, o.command, o.paths, o.to, code)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, nil, &out, &out); code != 0 || out.String() != "loka dev\n" {
		t.Fatalf("got %d %q", code, out.String())
	}
}

func TestShareCommandServesUntilStopped(t *testing.T) {
	file := filepath.Join(t.TempDir(), "shared.txt")
	os.WriteFile(file, []byte("hello"), 0o644)
	out, stop, code := runLoka(t, nil, "share", "--address", "127.0.0.1", "--port", "0", file)
	link := out.wait(t, `http://127\.0\.0\.1:\d+/download/\S+`)[0]
	resp := get(t, link)
	if body, _ := io.ReadAll(resp.Body); string(body) != "hello" {
		t.Fatalf("got %q", body)
	}
	stop()
	if c := <-code; c != 0 {
		t.Fatalf("exit code %d, output %q", c, out.String())
	}
}

func TestServeCommandServesThePage(t *testing.T) {
	out, stop, code := runLoka(t, nil, "--port", "0", "--no-browser")
	url := out.wait(t, `running at (http://127\.0\.0\.1:\d+/)`)[1]
	resp := get(t, url)
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>") {
		t.Fatalf("page answered %d", resp.StatusCode)
	}
	stop()
	if c := <-code; c != 0 {
		t.Fatalf("exit code %d, output %q", c, out.String())
	}
}

func TestSendCommand(t *testing.T) {
	receiver, receiverURL := startServer(t)
	host, port := hostPort(t, receiverURL)
	source, destination := t.TempDir(), t.TempDir()
	writeTree(t, source, map[string]string{"dir/a.txt": "a", "dir/sub/b.txt": "b"})

	out, _, code := runLoka(t, nil, "send", filepath.Join(source, "dir"), "--to", host, "--peer-port", strconv.Itoa(port))
	in := waitForIncoming(t, receiver)
	api(t, receiver, receiverURL, "/api/accept", map[string]string{"id": in.ID, "destination": destination}, nil)
	if c := <-code; c != 0 {
		t.Fatalf("exit code %d, output %q", c, out.String())
	}
	if !strings.Contains(out.String(), "complete: 100%") {
		t.Fatalf("output %q", out.String())
	}
	sameTree(t, readTree(t, destination), readTree(t, source))
}

func TestReceiveCommand(t *testing.T) {
	stdin, answer := io.Pipe()
	defer answer.Close()
	out, stop, code := runLoka(t, stdin, "receive", "--port", "0")
	port, _ := strconv.Atoi(out.wait(t, `on port (\d+)`)[1])

	source, destination := t.TempDir(), t.TempDir()
	writeTree(t, source, map[string]string{"song.mp3": "la la"})
	entries, items, size, _ := collect([]string{filepath.Join(source, "song.mp3")})
	sent := newTransfer(outgoing, "127.0.0.1", items, size, statusWaiting)
	go func() { sent.finish(sendTo(sent, entries, "laptop", "127.0.0.1", port)) }()

	out.wait(t, `laptop wants to send: song\.mp3\nDestination directory`)
	io.WriteString(answer, filepath.Join(destination, "missing")+"\n")
	out.wait(t, `must be an existing directory\nDestination directory`)
	io.WriteString(answer, destination+"\n")
	if v := waitForEnd(t, sent); v.Status != statusComplete {
		t.Fatalf("sender ended %+v", v)
	}
	if data, _ := os.ReadFile(filepath.Join(destination, "song.mp3")); string(data) != "la la" {
		t.Fatalf("got %q", data)
	}
	stop()
	if c := <-code; c != 0 {
		t.Fatalf("exit code %d, output %q", c, out.String())
	}
}

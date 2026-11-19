package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/treetest"
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
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if match := re.FindStringSubmatch(o.String()); match != nil {
			return match
		}
	}
	t.Fatalf("timed out waiting for output matching %s, got %q", pattern, o.String())
	return nil
}

// get fetches url and returns the response, closed when the test ends.
func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
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
		{[]string{"--port", "65536"}, "", nil, "", 2},
		{[]string{"send", "a", "--to", "h", "--peer-port", "0"}, "", nil, "", 2},
	} {
		o, code := parse(c.args, io.Discard, io.Discard)
		if code != c.code || code == -1 && (o.command != c.command || !slices.Equal(o.paths, c.paths) || o.to != c.to) {
			t.Errorf("%q: got %q %q to=%q code %d", c.args, o.command, o.paths, o.to, code)
		}
	}
}

func TestTakenPortSuggestsAFreeOne(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	t.Setenv("FORCE_COLOR", "")
	var out bytes.Buffer
	code := run(context.Background(), []string{"--no-browser", "--port", port}, nil, &out, &out)
	want := "loka: error: port " + port + " is already in use. Stop the other program, or run with --port 0 to pick a free port\n"
	if code != 1 || out.String() != want {
		t.Fatalf("got %d %q", code, out.String())
	}
}

// TestHelpAndErrorColours checks the rules Python's argparse uses: colour
// only on a terminal or with FORCE_COLOR, and never with NO_COLOR.
func TestHelpAndErrorColours(t *testing.T) {
	for _, c := range []struct {
		noColor, forceColor string
		want                bool
	}{
		{"", "", false}, // a buffer is not a terminal
		{"", "1", true},
		{"1", "1", false},
	} {
		t.Setenv("NO_COLOR", c.noColor)
		t.Setenv("FORCE_COLOR", c.forceColor)
		var help, usageErr bytes.Buffer
		run(context.Background(), []string{"-h"}, nil, &help, &help)
		run(context.Background(), []string{"bogus"}, nil, &usageErr, &usageErr)
		colored := strings.Contains(help.String(), "\x1b[1;34musage:\x1b[0m") &&
			strings.Contains(help.String(), "\x1b[1;36m--port\x1b[0m \x1b[1;33mPORT\x1b[0m") &&
			strings.Contains(usageErr.String(), "loka: \x1b[1;35merror:\x1b[0m \x1b[35munknown command \"bogus\"\x1b[0m")
		plain := !strings.Contains(help.String()+usageErr.String(), "\x1b[")
		if c.want && !colored || !c.want && !plain {
			t.Errorf("NO_COLOR=%q FORCE_COLOR=%q: help %q, error %q", c.noColor, c.forceColor, help.String(), usageErr.String())
		}
	}
}

// TestHelpGoesToStdout keeps help on standard output, where scripts and
// pagers expect it, and usage errors on standard error.
func TestHelpGoesToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, nil, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "usage:") || stderr.Len() != 0 {
		t.Fatalf("got %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"--bogus"}, nil, &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("got %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
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

// TestSendAndReceiveCommands sends a directory from loka send to loka
// receive, answering the receiver's prompt the way a person would.
func TestSendAndReceiveCommands(t *testing.T) {
	stdin, answer := io.Pipe()
	defer answer.Close()
	received, stopReceiving, receiveCode := runLoka(t, stdin, "receive", "--port", "0")
	port := received.wait(t, `on port (\d+)`)[1]

	source, destination := t.TempDir(), t.TempDir()
	treetest.Write(t, source, map[string]string{"dir/a.txt": "a", "dir/sub/b.txt": "b"})
	sent, _, sendCode := runLoka(t, nil, "send", filepath.Join(source, "dir"), "--to", "127.0.0.1", "--peer-port", port)

	received.wait(t, `wants to send: dir\nDestination directory`)
	io.WriteString(answer, filepath.Join(destination, "missing")+"\n")
	received.wait(t, `must be an existing directory\nDestination directory`)
	io.WriteString(answer, destination+"\n")
	if c := <-sendCode; c != 0 || !strings.Contains(sent.String(), "complete: 100%") {
		t.Fatalf("send exited %d, output %q", c, sent.String())
	}
	treetest.Same(t, treetest.Read(t, destination), treetest.Read(t, source))

	stopReceiving()
	if c := <-receiveCode; c != 0 {
		t.Fatalf("receive exited %d, output %q", c, received.String())
	}
}

// TestReceiveStopsAfterAccepting stops loka receive after it accepted an
// offer whose sender never starts uploading.
func TestReceiveStopsAfterAccepting(t *testing.T) {
	stdin, answer := io.Pipe()
	defer answer.Close()
	received, stopReceiving, receiveCode := runLoka(t, stdin, "receive", "--port", "0")
	port := received.wait(t, `on port (\d+)`)[1]

	offer := strings.NewReader(`{"name":"stalled","items":["x"],"size":1}`)
	resp, err := http.Post("http://127.0.0.1:"+port+"/peer/offer", "application/json", offer)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	received.wait(t, `wants to send: x\nDestination directory`)
	io.WriteString(answer, t.TempDir()+"\n")
	received.wait(t, `accepted: 0%`)

	stopReceiving()
	select {
	case c := <-receiveCode:
		if c != 0 {
			t.Fatalf("receive exited %d, output %q", c, received.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("receive kept running after it was stopped, output %q", received.String())
	}
}

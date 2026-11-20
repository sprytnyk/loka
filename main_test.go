package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/server"
	"github.com/sprytnyk/loka/internal/treetest"
	"github.com/sprytnyk/loka/internal/trust"
)

// TestMain keeps the tests' identities out of the real settings directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loka-test-")
	if err != nil {
		panic(err)
	}
	configDir = func() (string, error) { return dir, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

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
		{[]string{"forget", "my laptop"}, "forget", []string{"my laptop"}, "", -1},
		{[]string{"forget"}, "", nil, "", 2},
		{[]string{"forget", "a", "b"}, "", nil, "", 2},
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
	// The browser link keeps its token after #, which browsers never send.
	m := out.wait(t, `(http://127\.0\.0\.1:\d+)/#d=(\S+)`)
	base, token := m[1], m[2]
	curl := out.wait(t, `curl -kOJ (https://127\.0\.0\.1:\d+/download/(\S+))`)
	if curl[2] != token {
		t.Fatalf("the curl line has another token: %q", curl[0])
	}
	// Another device gets nothing from the link over plain HTTP.
	req, _ := http.NewRequest(http.MethodGet, base+"/download/"+token, nil)
	req.Host = "192.0.2.10"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("plain HTTP download answered %v %v", resp, err)
	}
	resp, err := browser(t).Get(curl[1])
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
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

	code := received.wait(t, `wants to send: dir\nPairing code (\d{3} \d{3})\. Check that the sender shows the same\.\nDestination directory`)[1]
	// A line longer than any path is a wrong answer, not the last one.
	go io.WriteString(answer, strings.Repeat("x", 1<<20)+"\n")
	received.wait(t, `must be an existing directory\nDestination directory`)
	io.WriteString(answer, filepath.Join(destination, "missing")+"\n")
	received.wait(t, `must be an existing directory\nDestination directory \(empty to decline\): The destination must be an existing directory\nDestination directory`)
	io.WriteString(answer, destination+"\n")
	if c := <-sendCode; c != 0 || !strings.Contains(sent.String(), "complete: 100%") {
		t.Fatalf("send exited %d, output %q", c, sent.String())
	}
	if !strings.Contains(sent.String(), "Pairing code "+code+".") {
		t.Fatalf("the sender didn't show the receiver's code %s, output %q", code, sent.String())
	}
	treetest.Same(t, treetest.Read(t, destination), treetest.Read(t, source))

	stopReceiving()
	if c := <-receiveCode; c != 0 {
		t.Fatalf("receive exited %d, output %q", c, received.String())
	}
}

// browser is an HTTP client that trusts Loka's certificate authority, as a
// phone that installed it does.
func browser(t *testing.T) *http.Client {
	t.Helper()
	store, err := openTrust()
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(store.CA())
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
}

// TestReceiveStopsAfterAccepting stops loka receive after it accepted an
// offer whose sender never starts uploading.
func TestReceiveStopsAfterAccepting(t *testing.T) {
	stdin, answer := io.Pipe()
	defer answer.Close()
	received, stopReceiving, receiveCode := runLoka(t, stdin, "receive", "--port", "0")
	port := received.wait(t, `on port (\d+)`)[1]

	offer := strings.NewReader(`{"name":"stalled","items":["x"],"size":1}`)
	resp, err := browser(t).Post("https://127.0.0.1:"+port+"/peer/offer", "application/json", offer)
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

func TestForgetCommand(t *testing.T) {
	store, err := openTrust()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := store.Remember(trust.Device{ID: id, Name: "old laptop", Fingerprint: id}); err != nil {
			t.Fatal(err)
		}
	}
	// Names can repeat, so an ID forgets just that one device.
	var out bytes.Buffer
	if code := run(context.Background(), []string{"forget", "a"}, nil, &out, &out); code != 0 || out.String() != "Forgot old laptop.\n" {
		t.Fatalf("got %d %q", code, out.String())
	}
	devices, _ := store.Devices()
	ids := map[string]bool{}
	for _, d := range devices {
		ids[d.ID] = true
	}
	if ids["a"] || !ids["b"] {
		t.Fatalf("left %+v", devices)
	}
	store.Remember(trust.Device{ID: "a", Name: "old laptop", Fingerprint: "a"})
	out.Reset()
	if code := run(context.Background(), []string{"forget", "old laptop"}, nil, &out, &out); code != 0 || out.String() != "Forgot 2 devices named old laptop.\n" {
		t.Fatalf("got %d %q", code, out.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"forget", "old laptop"}, nil, &out, &out); code != 1 || !strings.Contains(out.String(), `no remembered device has the name or ID "old laptop"`) {
		t.Fatalf("got %d %q", code, out.String())
	}
}

// TestDevicesChecksRememberedCertificates runs a device and checks that
// loka devices labels it remembered only when its certificate matches.
func TestDevicesChecksRememberedCertificates(t *testing.T) {
	store, err := openTrust()
	if err != nil {
		t.Fatal(err)
	}
	other, err := trust.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := server.New("desk", "test")
	s.Trust = other
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()
	stop := sync.OnceFunc(func() { cancel(); <-served })
	t.Cleanup(stop)
	p := network.Peer{ID: other.ID, Name: "desk", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	t.Cleanup(func() { store.Forget(func(d trust.Device) bool { return d.ID == other.ID }) })

	store.Remember(trust.Device{ID: other.ID, Name: "desk", Fingerprint: other.Fingerprint()})
	if got := rememberedLabel(store, p); got != "  (remembered)" {
		t.Fatalf("matching certificate: %q", got)
	}
	posing := p
	posing.ID = "the ID of another remembered device"
	store.Remember(trust.Device{ID: posing.ID, Name: "laptop", Fingerprint: "its certificate"})
	if got := rememberedLabel(store, posing); !strings.Contains(got, "posing") {
		t.Fatalf("another device's ID: %q", got)
	}
	store.Forget(func(d trust.Device) bool { return d.ID == posing.ID })
	stop()
	if got := rememberedLabel(store, p); !strings.Contains(got, "could not check") {
		t.Fatalf("unreachable: %q", got)
	}
}

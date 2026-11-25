package trust

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIdentityLastsAndIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loka")
	first := open(t, dir)
	if again := open(t, dir); again.ID != first.ID || first.ID == "" {
		t.Fatalf("ID changed from %q to %q", first.ID, again.ID)
	}
	if other := open(t, t.TempDir()); other.ID == first.ID {
		t.Fatal("two devices got the same ID")
	}
	info, err := os.Stat(filepath.Join(dir, identityFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("identity readable by others: %v", mode)
	}
}

func TestConcurrentCertificateLoadsUseOneIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), identityFile)
	var creations atomic.Int32
	template := func() *x509.Certificate {
		creations.Add(1)
		// Give the other callers time to see that the identity is missing.
		time.Sleep(20 * time.Millisecond)
		return identity()
	}

	start := make(chan struct{})
	var fingerprints [16]string
	var wg sync.WaitGroup
	for i := range fingerprints {
		wg.Go(func() {
			<-start
			cert, err := load(path, "this device's identity", template)
			if err != nil {
				t.Errorf("could not load identity: %v", err)
				return
			}
			_, fingerprints[i] = Identify(cert.Leaf)
		})
	}
	close(start)
	wg.Wait()
	if got := creations.Load(); got != 1 {
		t.Errorf("prepared %d certificate templates, want 1", got)
	}
	for _, fingerprint := range fingerprints {
		if fingerprint == "" || fingerprint != fingerprints[0] {
			t.Fatal("concurrent callers did not load the same identity")
		}
	}
}

func TestCertificateCreationKeepsLegacyIdentity(t *testing.T) {
	legacyData, err := createIfMissing(filepath.Join(t.TempDir(), identityFile), identity)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := tls.X509KeyPair(legacyData, legacyData)
	if err != nil {
		t.Fatal(err)
	}
	_, want := Identify(legacy.Leaf)
	dir := t.TempDir()
	path := filepath.Join(dir, identityFile)
	cert, err := load(path, "this device's identity", func() *x509.Certificate {
		// An older process writes directly to the final path without the lock.
		if err := os.WriteFile(path, legacyData, 0o600); err != nil {
			t.Fatal(err)
		}
		return identity()
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, got := Identify(cert.Leaf); got != want {
		t.Error("startup did not use the identity already saved by the older process")
	}
	saved, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Fingerprint() != want {
		t.Error("startup replaced the identity saved by the older process")
	}
}

func TestTrustFirstThenRefuseAChange(t *testing.T) {
	s := open(t, t.TempDir())
	laptop := Device{ID: "laptop-id", Name: "laptop", Fingerprint: "aa"}
	if standing, _, err := s.Check(laptop.ID, laptop.Fingerprint); err != nil || standing != New {
		t.Fatalf("got %v %v, want New", standing, err)
	}
	if err := s.Remember(laptop); err != nil {
		t.Fatal(err)
	}
	if standing, _, _ := s.Check(laptop.ID, "aa"); standing != Known {
		t.Fatalf("got %v, want Known", standing)
	}
	standing, remembered, _ := s.Check(laptop.ID, "bb")
	if standing != Changed || remembered.Name != "laptop" {
		t.Fatalf("got %v %+v, want Changed laptop", standing, remembered)
	}
	if err := s.Remember(Device{ID: laptop.ID, Name: "laptop", Fingerprint: "bb"}); err == nil {
		t.Fatal("Remember replaced a certificate")
	}
	if err := s.Remember(Device{ID: laptop.ID, Name: "new name", Fingerprint: "aa"}); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := s.Check(laptop.ID, "aa"); d.Name != "new name" {
		t.Fatalf("name not updated: %+v", d)
	}
}

func TestForgetByNameDropsEveryMatch(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	for _, d := range []Device{{"1", "laptop", "a"}, {"2", "laptop", "b"}, {"3", "phone", "c"}} {
		if err := s.Remember(d); err != nil {
			t.Fatal(err)
		}
	}
	// Another process forgets; this one sees it at once.
	forgotten, err := open(t, dir).Forget(func(d Device) bool { return d.Name == "laptop" })
	if err != nil || len(forgotten) != 2 {
		t.Fatalf("forgot %v, %v", forgotten, err)
	}
	if standing, _, _ := s.Check("1", "a"); standing != New {
		t.Fatalf("got %v after forgetting, want New", standing)
	}
	left, _ := s.Devices()
	if len(left) != 1 || left[0].Name != "phone" {
		t.Fatalf("left %+v", left)
	}
}

func TestConcurrentRemembersKeepEveryDevice(t *testing.T) {
	s := open(t, t.TempDir())
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			if err := s.Remember(Device{ID: fmt.Sprint(i), Name: "d", Fingerprint: "f"}); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() { s.Check(fmt.Sprint(i), "f") })
	}
	wg.Wait()
	if devices, _ := s.Devices(); len(devices) != 40 {
		t.Fatalf("kept %d of 40 devices", len(devices))
	}
}

// TestSeparateProcessesKeepEveryDevice runs real Loka-like processes
// that each remember devices in one settings directory at once, so the
// lock must work between processes, not only between open files.
func TestSeparateProcessesKeepEveryDevice(t *testing.T) {
	const processes, each = 8, 15
	dir := t.TempDir()
	var wg sync.WaitGroup
	for p := range processes {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRememberInChild$")
		cmd.Env = append(os.Environ(), "LOKA_TEST_DIR="+dir, fmt.Sprintf("LOKA_TEST_PREFIX=%d-", p), fmt.Sprintf("LOKA_TEST_COUNT=%d", each))
		wg.Go(func() {
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("process %d: %v\n%s", p, err, out)
			}
		})
	}
	wg.Wait()
	if devices, _ := open(t, dir).Devices(); len(devices) != processes*each {
		t.Fatalf("kept %d of %d devices", len(devices), processes*each)
	}
}

// TestRememberInChild is the process TestSeparateProcessesKeepEveryDevice
// starts. On its own it does nothing.
func TestRememberInChild(t *testing.T) {
	dir := os.Getenv("LOKA_TEST_DIR")
	if dir == "" {
		t.Skip("run by TestSeparateProcessesKeepEveryDevice")
	}
	count, err := strconv.Atoi(os.Getenv("LOKA_TEST_COUNT"))
	if err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	for i := range count {
		if err := s.Remember(Device{ID: os.Getenv("LOKA_TEST_PREFIX") + fmt.Sprint(i), Name: "d", Fingerprint: "f"}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHandshakeShowsBothDevices connects a client and a server and checks
// each sees the other's ID, and that the client can refuse the server.
func TestHandshakeShowsBothDevices(t *testing.T) {
	server, client := open(t, t.TempDir()), open(t, t.TempDir())
	// Over TCP, not net.Pipe: the server writes session tickets after the
	// handshake, which an unbuffered pipe would block on.
	handshake := func(check func(id, fp string) error) (seenByServer string, err error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		done := make(chan string, 1)
		go func() {
			raw, err := ln.Accept()
			if err != nil {
				done <- ""
				return
			}
			defer raw.Close()
			conn := tls.Server(raw, server.ServerConfig())
			if conn.Handshake() != nil {
				done <- ""
				return
			}
			id, _ := Identify(conn.ConnectionState().PeerCertificates[0])
			done <- id
		}()
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		conn := tls.Client(raw, client.ClientConfig(check))
		err = conn.Handshake()
		if err == nil {
			conn.Read(make([]byte, 1)) // a TLS 1.3 server checks the client's certificate here
		}
		raw.Close()
		return <-done, err
	}
	var seenByClient string
	seen, err := handshake(func(id, _ string) error { seenByClient = id; return nil })
	if err != nil || seen != client.ID || seenByClient != server.ID {
		t.Fatalf("server saw %q, client saw %q, err %v", seen, seenByClient, err)
	}
	refusal := &ChangedError{Device{ID: server.ID, Name: "server"}}
	if _, err := handshake(func(string, string) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("got %v, want the refusal", err)
	}
}

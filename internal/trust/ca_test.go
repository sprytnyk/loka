package trust

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAuthorityLastsAndIsPrivate(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir)
	if again := open(t, dir); again.CAFingerprint() != first.CAFingerprint() {
		t.Fatal("the authority changed between runs")
	}
	if other := open(t, t.TempDir()); other.CAFingerprint() == first.CAFingerprint() {
		t.Fatal("two devices got the same authority")
	}
	info, err := os.Stat(filepath.Join(dir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("authority key readable by others: %v", mode)
	}
	ca, err := x509.ParseCertificate(first.CA())
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.MaxPathLenZero || !strings.HasPrefix(ca.Subject.CommonName, "Loka") {
		t.Fatalf("authority %+v", ca.Subject)
	}
	if years := time.Until(ca.NotAfter).Hours() / 24 / 365; years < 9.9 {
		t.Fatalf("authority lasts %.1f years", years)
	}
}

// pool trusts only s's authority, as a phone that installed it does.
func pool(s *Store) *x509.CertPool {
	p := x509.NewCertPool()
	ca, _ := x509.ParseCertificate(s.CA())
	p.AddCert(ca)
	return p
}

func TestBrowserCertificatesNameTheLocalAddress(t *testing.T) {
	s := open(t, t.TempDir())
	for _, addr := range []string{"192.168.1.20", "10.1.2.3", "172.20.0.5", "127.0.0.1", "::1", "fe80::1", "::ffff:192.168.1.20"} {
		ip := netip.MustParseAddr(addr)
		c, err := s.BrowserCertificate(ip)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Leaf.Verify(x509.VerifyOptions{DNSName: ip.Unmap().String(), Roots: pool(s)})
		if err != nil {
			t.Errorf("%s: %v", addr, err)
		}
		if days := c.Leaf.NotAfter.Sub(c.Leaf.NotBefore).Hours() / 24; days > 398 {
			t.Errorf("%s: lasts %.0f days, more than phones accept", addr, days)
		}
		if again, _ := s.BrowserCertificate(ip); again != c {
			t.Errorf("%s: made a new certificate for the same address", addr)
		}
	}
	other := open(t, t.TempDir())
	c, _ := other.BrowserCertificate(netip.MustParseAddr("192.168.1.20"))
	if _, err := c.Leaf.Verify(x509.VerifyOptions{DNSName: "192.168.1.20", Roots: pool(s)}); err == nil {
		t.Fatal("another device's certificate passed")
	}
}

// signed makes a certificate from s's authority, as someone holding its
// key could.
func signed(t *testing.T, s *Store, template *x509.Certificate) *x509.Certificate {
	t.Helper()
	template.SerialNumber, _ = serial()
	template.NotBefore, template.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	der, err := x509.CreateCertificate(rand.Reader, template, s.ca.Leaf, &s.leafKey.PublicKey, s.ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

// posing are certificates for web sites that the authority's key could
// sign if it leaked. None of them should pass.
func posing(t *testing.T, s *Store) map[string]*x509.Certificate {
	return map[string]*x509.Certificate{
		"example.com": signed(t, s, &x509.Certificate{Subject: pkix.Name{CommonName: "example.com"}, DNSNames: []string{"example.com"}}),
		"8.8.8.8":     signed(t, s, &x509.Certificate{Subject: pkix.Name{CommonName: "Loka"}, IPAddresses: []net.IP{net.ParseIP("8.8.8.8")}}),
		"localhost":   signed(t, s, &x509.Certificate{Subject: pkix.Name{CommonName: "Loka"}, DNSNames: []string{"localhost"}}),
	}
}

func TestAuthorityCannotPoseAsAWebSite(t *testing.T) {
	s := open(t, t.TempDir())
	for name, c := range posing(t, s) {
		_, err := c.Verify(x509.VerifyOptions{DNSName: name, Roots: pool(s)})
		if invalid, ok := errors.AsType[x509.CertificateInvalidError](err); !ok || invalid.Reason != x509.CANotAuthorizedForThisName {
			t.Errorf("%s: got %v, want a name constraint refusal", name, err)
		}
	}
}

// TestOpenSSLAgrees checks the same with another verifier, when openssl
// is installed.
func TestOpenSSLAgrees(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not installed")
	}
	s := open(t, t.TempDir())
	dir := t.TempDir()
	write := func(name string, der []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ca := write("ca.pem", s.CA())
	verify := func(cert, flag, name string) (string, error) {
		out, err := exec.Command("openssl", "verify", "-CAfile", ca, flag, name, cert).CombinedOutput()
		return string(out), err
	}
	local, _ := s.BrowserCertificate(netip.MustParseAddr("192.168.1.20"))
	if out, err := verify(write("local.pem", local.Leaf.Raw), "-verify_ip", "192.168.1.20"); err != nil {
		t.Fatalf("a local address failed: %s", out)
	}
	for name, c := range posing(t, s) {
		flag := "-verify_hostname"
		if net.ParseIP(name) != nil {
			flag = "-verify_ip"
		}
		if out, err := verify(write(name+".pem", c.Raw), flag, name); err == nil {
			t.Errorf("%s passed: %s", name, out)
		}
	}
}

// TestLokasAndBrowsersGetTheirOwnCertificates connects to the port as a
// Loka and as a browser that trusts the authority. The Loka gets the
// device's certificate and must show its own; the browser gets one from
// the authority and needs none.
func TestLokasAndBrowsersGetTheirOwnCertificates(t *testing.T) {
	server, client := open(t, t.TempDir()), open(t, t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				conn := tls.Server(raw, server.ServerConfig())
				if conn.Handshake() == nil {
					conn.Write([]byte("hi"))
				}
			}()
		}
	}()
	dial := func(config *tls.Config) (tls.ConnectionState, error) {
		conn, err := tls.Dial("tcp", ln.Addr().String(), config)
		if err != nil {
			return tls.ConnectionState{}, err
		}
		defer conn.Close()
		_, err = conn.Read(make([]byte, 2))
		return conn.ConnectionState(), err
	}

	var seen string
	state, err := dial(client.ClientConfig(func(id, _ string) error { seen = id; return nil }))
	if err != nil || seen != server.ID {
		t.Fatalf("as a Loka: saw %q, %v", seen, err)
	}
	if state.PeerCertificates[0].IsCA || state.Version != tls.VersionTLS13 {
		t.Fatal("a Loka got the wrong certificate")
	}

	state, err = dial(&tls.Config{RootCAs: pool(server)})
	if err != nil {
		t.Fatalf("as a browser: %v", err)
	}
	if id, _ := Identify(state.PeerCertificates[0]); id == server.ID {
		t.Fatal("a browser got the device's own certificate")
	}

	if _, err := dial(&tls.Config{RootCAs: pool(client)}); err == nil {
		t.Fatal("a browser trusting another authority connected")
	}
}

// TestVouchesAgreesWithVerifying checks that Loka says a phone can trust
// it at an address exactly when the browser certificate there verifies.
func TestVouchesAgreesWithVerifying(t *testing.T) {
	s := open(t, t.TempDir())
	for _, addr := range []string{"192.168.1.20", "100.64.1.5", "100.127.255.1", "100.128.0.1", "203.0.113.5", "8.8.8.8", "fe80::1", "2001:db8::1"} {
		ip := netip.MustParseAddr(addr)
		c, err := s.BrowserCertificate(ip)
		if err != nil {
			t.Fatal(err)
		}
		_, verr := c.Leaf.Verify(x509.VerifyOptions{Roots: pool(s), DNSName: addr})
		if s.Vouches(ip) != (verr == nil) || (s.AddressProblem(ip) == nil) != (verr == nil) {
			t.Errorf("%s: vouches %v, problem %v, but verifying gave %v", addr, s.Vouches(ip), s.AddressProblem(ip), verr)
		}
	}
}

// TestOlderAuthorityNeedsRemaking opens an authority made before shared
// addresses were allowed, and checks the problem says how to fix it.
func TestOlderAuthorityNeedsRemaking(t *testing.T) {
	dir := t.TempDir()
	saved := localRanges
	localRanges = slices.DeleteFunc(slices.Clone(localRanges), func(r string) bool { return r == "100.64.0.0/10" })
	open(t, dir)
	localRanges = saved
	s := open(t, dir)
	err := s.AddressProblem(netip.MustParseAddr("100.64.1.5"))
	if err == nil || !strings.Contains(err.Error(), "older") || !strings.Contains(err.Error(), filepath.Join(dir, caFile)) {
		t.Fatalf("got %v", err)
	}
	if err := s.AddressProblem(netip.MustParseAddr("203.0.113.5")); err == nil || !strings.Contains(err.Error(), "isn't a local network address") {
		t.Fatalf("got %v", err)
	}
}

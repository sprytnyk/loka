package trust

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"time"
)

// localRanges are the addresses the authority may vouch for: private,
// link-local and loopback ones, which no public web site uses.
var localRanges = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8",
	"fc00::/7", "fe80::/10", "::1/128",
}

// authority is this device's certificate authority. Its name constraints
// let it vouch only for local addresses, and for no domain name outside
// .invalid, which never resolves. So if its key leaked, it still could not
// pose as a web site.
func authority() *x509.Certificate {
	name := "Loka"
	if host, err := os.Hostname(); err == nil && host != "" {
		name += " on " + host
	}
	var ranges []*net.IPNet
	for _, r := range localRanges {
		_, n, _ := net.ParseCIDR(r)
		ranges = append(ranges, n)
	}
	return &x509.Certificate{
		Subject:                     pkix.Name{CommonName: name, Organization: []string{"Loka"}},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"invalid"},
		PermittedIPRanges:           ranges,
	}
}

// leafLifetime is how long a certificate for browsers lasts. Phones
// refuse longer ones, and a running Loka makes a new one when it is due.
const leafLifetime = 397 * 24 * time.Hour

// BrowserCertificate returns a certificate from this device's authority
// for one of its addresses, as browsers see it when they connect there.
// Making it for the address of each connection keeps it right when the
// device's addresses change.
func (s *Store) BrowserCertificate(addr netip.Addr) (*tls.Certificate, error) {
	addr = addr.Unmap()
	s.leafMu.Lock()
	defer s.leafMu.Unlock()
	if c, ok := s.leaves[addr]; ok && time.Until(c.Leaf.NotAfter) > 24*time.Hour {
		return c, nil
	}
	n, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	until := now.Add(leafLifetime)
	if until.After(s.ca.Leaf.NotAfter) {
		until = s.ca.Leaf.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: n,
		// Not the address: some programs read a common name with dots as a
		// domain name, which the authority may not vouch for.
		Subject:     pkix.Name{CommonName: "Loka"},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    until,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{addr.AsSlice()},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, s.ca.Leaf, &s.leafKey.PublicKey, s.ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: s.leafKey, Leaf: leaf}
	s.leaves[addr] = c
	return c, nil
}

// CA returns this device's authority certificate, for a phone to install.
func (s *Store) CA() []byte {
	return s.ca.Leaf.Raw
}

// CAFingerprint is the SHA-256 of the authority certificate, which phones
// show when installing it.
func (s *Store) CAFingerprint() string {
	sum := sha256.Sum256(s.ca.Leaf.Raw)
	return hex.EncodeToString(sum[:])
}

// Package trust holds this device's certificate and the devices it has
// sent to or received from before. Lokas talk to each other over TLS with
// these certificates. A device is trusted the first time, then remembered
// by its certificate, and refused if that certificate ever changes.
//
// Browsers can't pin a certificate, so this device also has its own
// certificate authority, which a phone can install once. It vouches for
// this device's local addresses only.
package trust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Device is another Loka this device remembers.
type Device struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"` // SHA-256 of its certificate
}

// Standing is what this device knows of another.
type Standing int

const (
	New     Standing = iota // never remembered
	Known                   // remembered with this certificate
	Changed                 // remembered with another certificate
)

// Store is this device's identity and the devices it remembers. The list
// of devices is read from disk on every check, so a device forgotten by
// another Loka process, such as loka forget, takes effect at once.
type Store struct {
	ID   string // this device, also its certificate's common name
	cert tls.Certificate
	ca   tls.Certificate // signs the certificates browsers see

	leafKey *ecdsa.PrivateKey // for every certificate browsers see in this run
	leafMu  sync.Mutex
	leaves  map[netip.Addr]*tls.Certificate // by the local address it names

	mu      sync.Mutex // serialises changes to the devices file in this process
	devices string     // the devices file
	lock    string     // locked while a process changes the devices file
	caPath  string     // the authority's file
}

const (
	identityFile = "identity.pem"
	caFile       = "ca.pem"
	devicesFile  = "devices.json"
	devicesLock  = "devices.lock"
)

// Open reads the identity and the certificate authority in dir, making
// them the first time.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("could not make the settings directory: %w", err)
	}
	cert, err := load(filepath.Join(dir, identityFile), "this device's identity", identity)
	if err != nil {
		return nil, err
	}
	ca, err := load(filepath.Join(dir, caFile), "this device's certificate authority", authority)
	if err != nil {
		return nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Store{
		ID:      cert.Leaf.Subject.CommonName,
		cert:    cert,
		ca:      ca,
		leafKey: leafKey,
		leaves:  map[netip.Addr]*tls.Certificate{},
		devices: filepath.Join(dir, devicesFile),
		lock:    filepath.Join(dir, devicesLock),
		caPath:  filepath.Join(dir, caFile),
	}, nil
}

// load reads the certificate and key at path, making them with template
// the first time.
func load(path, what string, template func() *x509.Certificate) (tls.Certificate, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = create(path, template())
	}
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("could not read %s in %s: %w", what, path, err)
	}
	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("could not read %s in %s: %w", what, path, err)
	}
	return cert, nil
}

// identity is this device's certificate for other Lokas. It names a new
// random ID. Peers pin the certificate itself and never check its dates.
func identity() *x509.Certificate {
	return &x509.Certificate{
		Subject:     pkix.Name{CommonName: rand.Text()},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
}

// create makes a key and a self-signed certificate from template, and
// saves both, readable only by this user, in one file at path.
func create(path string, template *x509.Certificate) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if template.SerialNumber, err = serial(); err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	data := slices.Concat(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return os.ReadFile(path) // another Loka made it first
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return data, nil
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// Identify returns the ID and fingerprint a peer's certificate shows.
func Identify(cert *x509.Certificate) (id, fingerprint string) {
	sum := sha256.Sum256(cert.Raw)
	return cert.Subject.CommonName, hex.EncodeToString(sum[:])
}

// Fingerprint is this device's certificate fingerprint, as other Lokas
// see it.
func (s *Store) Fingerprint() string {
	_, fp := Identify(s.cert.Leaf)
	return fp
}

// PeerName is the server name a Loka asks for when it connects to
// another. It tells Lokas from browsers before any certificate is sent:
// browsers send no name when they connect to an address.
const PeerName = "peer.loka.invalid"

// ServerConfig is for this device's port. Other Lokas get this device's
// certificate and must show theirs, which the handlers check. Browsers
// get a certificate from this device's authority.
func (s *Store) ServerConfig() *tls.Config {
	peers := &tls.Config{
		Certificates: []tls.Certificate{s.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
	browsers := &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			addr, err := netip.ParseAddrPort(hello.Conn.LocalAddr().String())
			if err != nil {
				return nil, err
			}
			return s.BrowserCertificate(addr.Addr())
		},
		MinVersion: tls.VersionTLS12,
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if hello.ServerName == PeerName {
				return peers, nil
			}
			return browsers, nil
		},
	}
}

// ClientConfig is for connecting to another device. check sees the other
// device's ID and fingerprint and can refuse it. The usual certificate
// checks are off because certificates here are self-signed; check pins
// them instead. The handshake still proves the other device holds its
// certificate's key.
func (s *Store) ClientConfig(check func(id, fingerprint string) error) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{s.cert},
		ServerName:         PeerName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the other device sent no certificate")
			}
			return check(Identify(cs.PeerCertificates[0]))
		},
	}
}

// Check says whether the device with this ID is new, known with this
// certificate, or known with another one. When it is not new, it also
// returns the device as remembered.
func (s *Store) Check(id, fingerprint string) (Standing, Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices, err := s.read()
	if err != nil {
		return New, Device{}, err
	}
	i := slices.IndexFunc(devices, func(d Device) bool { return d.ID == id })
	switch {
	case i < 0:
		return New, Device{}, nil
	case devices[i].Fingerprint == fingerprint:
		return Known, devices[i], nil
	default:
		return Changed, devices[i], nil
	}
}

// Remember saves a device, or its new name. It never replaces a device's
// certificate: that takes Forget first.
func (s *Store) Remember(d Device) error {
	unlock, err := s.lockDevices()
	if err != nil {
		return err
	}
	defer unlock()
	devices, err := s.read()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(devices, func(old Device) bool { return old.ID == d.ID })
	switch {
	case i < 0:
		devices = append(devices, d)
	case devices[i].Fingerprint != d.Fingerprint:
		return fmt.Errorf("%s is remembered with another certificate", devices[i].Name)
	case devices[i].Name == d.Name:
		return nil
	default:
		devices[i].Name = d.Name
	}
	return s.write(devices)
}

// Forget drops the devices that match and returns them.
func (s *Store) Forget(match func(Device) bool) ([]Device, error) {
	unlock, err := s.lockDevices()
	if err != nil {
		return nil, err
	}
	defer unlock()
	devices, err := s.read()
	if err != nil {
		return nil, err
	}
	var forgotten []Device
	devices = slices.DeleteFunc(devices, func(d Device) bool {
		if match(d) {
			forgotten = append(forgotten, d)
			return true
		}
		return false
	})
	if len(forgotten) == 0 {
		return nil, nil
	}
	return forgotten, s.write(devices)
}

// lockDevices keeps other goroutines and other Loka processes, such as
// loka send next to a running loka, from changing the devices file until
// unlock. Without it, two that read the file at once would each save
// their own change, and one would be lost.
func (s *Store) lockDevices() (unlock func(), err error) {
	s.mu.Lock()
	f, err := os.OpenFile(s.lock, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("could not lock the remembered devices: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		s.mu.Unlock()
		return nil, fmt.Errorf("could not lock the remembered devices: %w", err)
	}
	return func() {
		f.Close() // releases the lock
		s.mu.Unlock()
	}, nil
}

// Devices returns the remembered devices.
func (s *Store) Devices() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

func (s *Store) read() ([]Device, error) {
	data, err := os.ReadFile(s.devices)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read the remembered devices: %w", err)
	}
	var devices []Device
	if err := json.Unmarshal(data, &devices); err != nil {
		return nil, fmt.Errorf("could not read the remembered devices in %s: %w", s.devices, err)
	}
	return devices, nil
}

// write replaces the devices file in one step, so a reader never sees half
// of it.
func (s *Store) write(devices []Device) error {
	data, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.devices), ".devices-*")
	if err != nil {
		return fmt.Errorf("could not save the remembered devices: %w", err)
	}
	_, err = f.Write(append(data, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.devices)
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("could not save the remembered devices: %w", err)
	}
	return nil
}

// ChangedError is a remembered device showing a different certificate.
// A reinstalled Loka gets a new ID too, and shows as a new device, so this
// means another device is using the remembered one's ID. IDs are announced
// to the whole network, so anyone can do that. The error therefore never
// suggests forgetting the device.
type ChangedError struct {
	Device Device
}

func (e *ChangedError) Error() string {
	return fmt.Sprintf("%s showed a different certificate than before, so another device is probably posing as it. Loka refused it", e.Device.Name)
}

// Package network finds nearby Loka devices over multicast and lists this
// device's addresses.
package network

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultPort   = 45873
	discoveryAddr = "239.255.45.87"
	AnnounceEvery = 3 * time.Second
	peerTimeout   = 12 * time.Second
)

// Announcement is what each device multicasts every few seconds.
type Announcement struct {
	Service string `json:"service"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
}

// Peer is a nearby device found by discovery.
type Peer struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Host string    `json:"host"`
	Port int       `json:"port"`
	seen time.Time // when it last announced itself
}

// Discovery finds nearby devices. Only its listener writes peers and only
// its announcer writes announceErr, each under the discovery's own lock.
type Discovery struct {
	self Announcement
	port int // the UDP port to listen and announce on

	mu          sync.Mutex
	peers       map[string]Peer
	listenErr   string
	announceErr string
}

// New makes a Discovery that announces self.
func New(self Announcement) *Discovery {
	self.Service = "loka"
	return &Discovery{self: self, port: DefaultPort, peers: map[string]Peer{}}
}

// List returns the devices heard from recently, by name, and any problem
// that may hide devices.
func (d *Discovery) List() ([]Peer, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var peers []Peer
	for _, p := range d.peers {
		if time.Since(p.seen) < peerTimeout {
			peers = append(peers, p)
		}
	}
	slices.SortFunc(peers, func(a, b Peer) int {
		return strings.Compare(a.Name+"\x00"+a.ID, b.Name+"\x00"+b.ID)
	})
	return peers, cmp.Or(d.listenErr, d.announceErr)
}

// Listen records announcements from other devices until ctx is done.
func (d *Discovery) Listen(ctx context.Context) {
	conn, err := d.joinGroup()
	if err != nil {
		d.mu.Lock()
		d.listenErr = fmt.Sprintf("Discovery could not start: %v. Check your firewall and network connection.", err)
		d.mu.Unlock()
		return
	}
	d.serve(ctx, conn)
}

// serve reads announcements from conn until ctx is done.
func (d *Discovery) serve(ctx context.Context, conn net.PacketConn) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		host, _, _ := net.SplitHostPort(from.String())
		if p, ok := parseAnnouncement(buf[:n], host, d.self.ID); ok {
			d.mu.Lock()
			d.peers[p.ID] = p
			d.mu.Unlock()
		}
	}
}

// joinGroup listens on the discovery port and joins the multicast group on
// every interface that will let it.
func (d *Discovery) joinGroup() (net.PacketConn, error) {
	config := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return control(c, reuseAddr)
	}}
	conn, err := config.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", d.port))
	if err != nil {
		return nil, err
	}
	raw, err := conn.(*net.UDPConn).SyscallConn()
	if err != nil {
		conn.Close()
		return nil, err
	}
	group := ipv4(net.ParseIP(discoveryAddr))
	var joinErr error
	joined := false
	for _, addr := range multicastAddresses() {
		err := control(raw, func(fd uintptr) error { return joinGroup(fd, group, addr) })
		if err == nil {
			joined = true
		}
		joinErr = cmp.Or(joinErr, err)
	}
	if !joined {
		conn.Close()
		return nil, fmt.Errorf("could not join on any network interface: %w", joinErr)
	}
	return conn, nil
}

// Announce tells other devices about this one until ctx is done.
func (d *Discovery) Announce(ctx context.Context) {
	payload, _ := json.Marshal(d.self)
	group := &net.UDPAddr{IP: net.ParseIP(discoveryAddr), Port: d.port}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		d.setAnnounceErr(fmt.Sprintf("Could not start device announcements: %v", err))
		return
	}
	defer conn.Close()
	raw, err := conn.(*net.UDPConn).SyscallConn()
	if err != nil {
		d.setAnnounceErr(fmt.Sprintf("Could not start device announcements: %v", err))
		return
	}
	ticker := time.NewTicker(AnnounceEvery)
	defer ticker.Stop()
	for {
		sent := false
		for _, addr := range multicastAddresses() {
			err := control(raw, func(fd uintptr) error { return multicastInterface(fd, addr) })
			if err != nil {
				continue
			}
			if _, err := conn.WriteTo(payload, group); err == nil {
				sent = true
			}
		}
		if sent {
			d.setAnnounceErr("")
		} else {
			d.setAnnounceErr("Could not announce this device. Check your firewall and network connection.")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Discovery) setAnnounceErr(text string) {
	d.mu.Lock()
	d.announceErr = text
	d.mu.Unlock()
}

// parseAnnouncement checks a datagram and turns it into a peer. It ignores
// this device's own announcements.
func parseAnnouncement(data []byte, host, selfID string) (Peer, bool) {
	var a Announcement
	if json.Unmarshal(data, &a) != nil || a.Service != "loka" || a.ID == "" || a.ID == selfID ||
		a.Port <= 0 || a.Port > 65535 || host == "" {
		return Peer{}, false
	}
	return Peer{ID: a.ID, Name: truncate(a.Name, 255), Host: host, Port: a.Port, seen: time.Now()}, true
}

// multicastAddresses lists the interface addresses to join and announce on,
// or the "any" address when there are none.
func multicastAddresses() [][4]byte {
	found, _ := interfaceAddresses()
	var addrs [][4]byte
	for _, a := range found {
		if ip := net.ParseIP(a.ip); ip != nil && !ip.IsLoopback() {
			addrs = append(addrs, ipv4(ip))
		}
	}
	if len(addrs) == 0 {
		addrs = append(addrs, [4]byte{})
	}
	return addrs
}

func ipv4(ip net.IP) [4]byte {
	return [4]byte(ip.To4())
}

// control runs a socket option call on the raw socket.
func control(c syscall.RawConn, f func(fd uintptr) error) error {
	var opErr error
	if err := c.Control(func(fd uintptr) { opErr = f(fd) }); err != nil {
		return err
	}
	return opErr
}

// truncate shortens s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

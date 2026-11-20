package network

import (
	"cmp"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// localAddress is an IPv4 address on this device. lan is false when its
// interface looks like a VPN tunnel or a virtual bridge rather than the
// local network.
type localAddress struct {
	ip  string
	lan bool
}

// LocalAddresses lists this device's local-network IPv4 addresses, the
// best one for a link first. Other devices may be on any of them, so each
// gets its own link.
func LocalAddresses() []string {
	found, err := interfaceAddresses()
	if err != nil {
		return nil
	}
	return orderAddresses(found, routeAddress())
}

// routeAddress returns the address the system would send from to reach
// another network, or "" when there is no route. Nothing is sent.
func routeAddress() string {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// meshRange holds the addresses that Tailscale and similar VPNs give each
// of a person's devices. Unlike other VPN addresses, a phone on the same
// VPN can reach them from anywhere.
var meshRange = netip.MustParsePrefix("100.64.0.0/10")

// orderAddresses drops loopback addresses, and VPN and virtual ones unless
// there is nothing else, but keeps addresses in meshRange. It puts local
// network addresses first, the route address first among them, then the
// rest in text order.
func orderAddresses(found []localAddress, route string) []string {
	lan := map[string]bool{}
	for _, a := range found {
		if strings.HasPrefix(a.ip, "127.") || a.ip == "0.0.0.0" {
			continue
		}
		lan[a.ip] = lan[a.ip] || a.lan
	}
	ips := slices.Collect(maps.Keys(lan))
	if slices.ContainsFunc(ips, func(ip string) bool { return lan[ip] }) {
		ips = slices.DeleteFunc(ips, func(ip string) bool { return !lan[ip] && !inMesh(ip) })
	}
	slices.SortFunc(ips, func(a, b string) int {
		return cmp.Or(
			cmp.Compare(boolInt(!lan[a]), boolInt(!lan[b])),
			cmp.Compare(boolInt(a != route), boolInt(b != route)),
			strings.Compare(a, b),
		)
	})
	return ips
}

func inMesh(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	return err == nil && meshRange.Contains(addr)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

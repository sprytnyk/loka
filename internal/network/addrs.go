package network

import (
	"cmp"
	"maps"
	"net"
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

// orderAddresses drops loopback addresses, and VPN and virtual ones unless
// there is nothing else. It puts the route address first, then the rest in
// text order.
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
		ips = slices.DeleteFunc(ips, func(ip string) bool { return !lan[ip] })
	}
	slices.SortFunc(ips, func(a, b string) int {
		return cmp.Or(
			cmp.Compare(boolInt(a != route), boolInt(b != route)),
			strings.Compare(a, b),
		)
	})
	return ips
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

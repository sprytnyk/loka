//go:build !windows

package network

import (
	"net"
	"strings"
)

// interfaceAddresses lists the IPv4 addresses of interfaces that are up.
// Point-to-point links (most VPNs) and container or VM bridges do not count
// as the local network.
func interfaceAddresses() ([]localAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var found []localAddress
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		virtual := false
		for _, prefix := range []string{"docker", "br-", "virbr", "veth"} {
			virtual = virtual || strings.HasPrefix(iface.Name, prefix)
		}
		lan := iface.Flags&net.FlagPointToPoint == 0 && !virtual
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				found = append(found, localAddress{ipnet.IP.String(), lan})
			}
		}
	}
	return found, nil
}

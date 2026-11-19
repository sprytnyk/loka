package network

import (
	"errors"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// interfaceAddresses lists the IPv4 addresses of adapters that are up. Only
// wired and Wi-Fi adapters count as the local network, and not when their
// description names a VPN driver.
func interfaceAddresses() ([]localAddress, error) {
	size := uint32(15 * 1024)
	var first *windows.IpAdapterAddresses
	for {
		buf := make([]byte, size)
		first = (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET,
			windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER,
			0, first, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	var found []localAddress
	for a := first; a != nil; a = a.Next {
		if a.OperStatus != windows.IfOperStatusUp {
			continue
		}
		description := strings.ToLower(windows.UTF16PtrToString(a.Description))
		vpn := slices.ContainsFunc([]string{"openvpn", "tap-windows", "wireguard", "wintun"}, func(name string) bool {
			return strings.Contains(description, name)
		})
		wiredOrWiFi := a.IfType == windows.IF_TYPE_ETHERNET_CSMACD || a.IfType == windows.IF_TYPE_IEEE80211
		for u := a.FirstUnicastAddress; u != nil; u = u.Next {
			if ip := u.Address.IP().To4(); ip != nil {
				found = append(found, localAddress{ip.String(), wiredOrWiFi && !vpn})
			}
		}
	}
	return found, nil
}

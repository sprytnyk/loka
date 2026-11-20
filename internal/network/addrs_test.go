package network

import (
	"slices"
	"testing"
)

func TestOrderAddresses(t *testing.T) {
	const (
		lan    = "192.168.50.7"
		vpn    = "10.23.0.2"
		bridge = "172.17.0.1"
	)
	for _, c := range []struct {
		name  string
		found []localAddress
		route string
		want  []string
	}{
		{
			"route address beats a docker bridge",
			[]localAddress{{bridge, true}, {"192.168.1.10", true}},
			"192.168.1.10",
			[]string{"192.168.1.10", bridge},
		},
		{
			"text order without a default route",
			[]localAddress{{"192.168.1.10", true}, {bridge, true}},
			"",
			[]string{bridge, "192.168.1.10"},
		},
		{
			"vpn and bridge left out beside the local network",
			[]localAddress{{vpn, false}, {lan, true}, {bridge, false}},
			vpn,
			[]string{lan},
		},
		{
			"every local network, route first",
			[]localAddress{{"10.0.0.5", true}, {vpn, false}, {lan, true}},
			lan,
			[]string{lan, "10.0.0.5"},
		},
		{
			"vpn and bridge only",
			[]localAddress{{bridge, false}, {vpn, false}},
			vpn,
			[]string{vpn, bridge},
		},
		{
			"loopback and duplicates dropped",
			[]localAddress{{"127.0.0.1", true}, {lan, false}, {lan, true}, {"0.0.0.0", true}},
			"",
			[]string{lan},
		},
	} {
		if got := orderAddresses(c.found, c.route); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

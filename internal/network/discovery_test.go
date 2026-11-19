package network

import (
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"
)

// freeUDPPort returns a UDP port nothing listens on, so tests do not meet
// a real Loka on this machine.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("local sockets are blocked:", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// waitForPeers polls until d lists n peers or a few seconds pass.
func waitForPeers(d *Discovery, n int) []Peer {
	deadline := time.Now().Add(5 * time.Second)
	for {
		peers, _ := d.List()
		if len(peers) >= n || time.Now().After(deadline) {
			return peers
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestParseAnnouncement(t *testing.T) {
	valid := `{"service":"loka","id":"a","name":"desk","port":45873}`
	if p, ok := parseAnnouncement([]byte(valid), "192.0.2.10", "self"); !ok || p.Host != "192.0.2.10" || p.Name != "desk" {
		t.Fatalf("valid announcement rejected: %+v", p)
	}
	for _, bad := range []string{
		`{"service":"loka","id":"self","name":"me","port":45873}`,
		`{"service":"other","id":"a","name":"x","port":45873}`,
		`{"service":"loka","id":"a","name":"x","port":0}`,
		`{"service":"loka","id":"a","name":"x","port":70000}`,
		`{"service":"loka","id":"","name":"x","port":45873}`,
		`not json`,
	} {
		if _, ok := parseAnnouncement([]byte(bad), "192.0.2.10", "self"); ok {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestListenRecordsDatagrams(t *testing.T) {
	d := New(Announcement{ID: "self", Name: "me"})
	d.port = freeUDPPort(t)
	listener, err := d.joinGroup()
	if err != nil {
		t.Skip("multicast is unavailable:", err)
	}
	ctx := t.Context()
	go d.serve(ctx, listener)

	payload, _ := json.Marshal(Announcement{Service: "loka", ID: "other", Name: "laptop", Port: 1234})
	conn, err := net.Dial("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(d.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if peers := waitForPeers(d, 1); len(peers) != 1 || peers[0].Name != "laptop" || peers[0].Host != "127.0.0.1" || peers[0].Port != 1234 {
		t.Fatalf("got %+v", peers)
	}
}

// TestDevicesFindEachOther runs two devices on this machine over real
// multicast. Each must list the other and not itself.
func TestDevicesFindEachOther(t *testing.T) {
	port := freeUDPPort(t)
	ctx := t.Context()
	a := New(Announcement{ID: "a", Name: "alpha", Port: 1})
	b := New(Announcement{ID: "b", Name: "beta", Port: 2})
	for _, d := range []*Discovery{a, b} {
		d.port = port
		listener, err := d.joinGroup()
		if err != nil {
			t.Skip("multicast is unavailable:", err)
		}
		go d.serve(ctx, listener)
	}
	for _, d := range []*Discovery{a, b} {
		go d.Announce(ctx)
	}
	peersA, peersB := waitForPeers(a, 1), waitForPeers(b, 1)
	if len(peersA) != 1 || peersA[0].ID != "b" || len(peersB) != 1 || peersB[0].ID != "a" {
		t.Fatalf("alpha sees %+v, beta sees %+v", peersA, peersB)
	}
}

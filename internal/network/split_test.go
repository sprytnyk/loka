package network

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/trust"
)

// TestSplitServesHTTPAndTLSOnOnePort runs one HTTP server on both halves
// of a split port and checks each kind of client reaches the right one,
// even while another connection sends nothing.
func TestSplitServesHTTPAndTLSOnOnePort(t *testing.T) {
	server, err := trust.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := trust.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	plain, secure := Split(ln, server.ServerConfig())
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			id, _ := trust.Identify(r.TLS.PeerCertificates[0])
			io.WriteString(w, "secure "+id)
			return
		}
		io.WriteString(w, "plain")
	})}
	served := make(chan error, 2)
	go func() { served <- srv.Serve(plain) }()
	go func() { served <- srv.Serve(secure) }()

	silent, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()

	get := func(scheme string, c *http.Client) string {
		resp, err := c.Get(scheme + "://" + ln.Addr().String() + "/")
		if err != nil {
			t.Fatalf("%s: %v", scheme, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	if got := get("http", &http.Client{}); got != "plain" {
		t.Fatalf("http got %q", got)
	}
	tlsClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: client.ClientConfig(func(id, _ string) error { return nil }),
	}}
	if got := get("https", tlsClient); got != "secure "+client.ID {
		t.Fatalf("https got %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	for range 2 {
		if err := <-served; err != http.ErrServerClosed {
			t.Fatalf("Serve returned %v", err)
		}
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("the port is still open after shutdown")
	}
}

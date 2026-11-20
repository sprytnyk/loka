package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

// openTrust makes a device identity for a test.
func openTrust(t *testing.T) *trust.Store {
	t.Helper()
	store, err := trust.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// startServer runs a server on a loopback port, as Loka does, and returns
// it with its plain HTTP URL.
func startServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := New("receiver", "test")
	s.Trust = openTrust(t)
	return s, serve(t, s, nil)
}

// serve runs s on a loopback port until the test ends, through wrap when
// it is not nil, and returns the plain HTTP URL.
func serve(t *testing.T, s *Server, wrap func(http.Handler) http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	if wrap == nil {
		go func() { served <- s.Serve(ctx, ln) }()
	} else {
		srv := &http.Server{Handler: wrap(s.Handler())}
		plain, secure := network.Split(ln, s.Trust.ServerConfig())
		go srv.Serve(plain)
		go func() { served <- srv.Serve(secure) }()
		context.AfterFunc(ctx, func() { srv.Close() })
	}
	t.Cleanup(func() {
		cancel()
		<-served
	})
	return "http://" + ln.Addr().String()
}

// browser is an HTTP client that trusts s's certificate authority, as a
// phone that installed it does. It shows no certificate of its own.
func browser(s *Server) *http.Client {
	ca, _ := x509.ParseCertificate(s.Trust.CA())
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
}

// secure turns a server's plain HTTP URL into its HTTPS one.
func secure(base string) string {
	return "https" + strings.TrimPrefix(base, "http")
}

// api calls the page API of the server at url and decodes the reply into
// reply when it is not nil. It returns the HTTP status.
func api(t *testing.T, s *Server, url, path string, body, reply any) int {
	t.Helper()
	method := http.MethodGet
	var payload io.Reader
	if body != nil {
		method = http.MethodPost
		data, _ := json.Marshal(body)
		payload = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, url+path, payload)
	req.Header.Set("X-Loka-Key", s.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if reply != nil {
		json.NewDecoder(resp.Body).Decode(reply)
	}
	return resp.StatusCode
}

// waitFor polls until cond is true or a few seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForIncoming returns the first incoming transfer on s once it is
// past pairing, as the person sees it.
func waitForIncoming(t *testing.T, s *Server) *transfer.Transfer {
	t.Helper()
	var found *transfer.Transfer
	waitFor(t, "an incoming offer", func() bool {
		for _, tr := range s.Transfers.All() {
			if tr.Direction == transfer.Incoming && tr.Status() != transfer.Pairing {
				found = tr
				return true
			}
		}
		return false
	})
	return found
}

// waitForEnd waits for tr to end and returns how it ended.
func waitForEnd(t *testing.T, tr *transfer.Transfer) transfer.View {
	t.Helper()
	waitFor(t, "the transfer to end", func() bool { return tr.Status().Final() })
	return tr.View()
}

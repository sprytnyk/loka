package network

import (
	"context"
	"crypto/tls"
	"errors"
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
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve returned %v", err)
		}
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("the port is still open after shutdown")
	}
}

// failing fails Accept with each of errs in turn, then accepts as ln does.
type failing struct {
	net.Listener
	errs chan error
}

func (f failing) Accept() (net.Conn, error) {
	select {
	case err := <-f.errs:
		return nil, err
	default:
		return f.Listener.Accept()
	}
}

// outOfFiles is the error accept returns when the process has run out of
// file handles, such as while many connections are open at once.
type outOfFiles struct{}

func (outOfFiles) Error() string   { return "accept: too many open files" }
func (outOfFiles) Timeout() bool   { return false }
func (outOfFiles) Temporary() bool { return true }

// TestSplitOutlastsTemporaryErrors runs out of file handles twice before
// the first connection, and checks the port still answers once they are
// back.
func TestSplitOutlastsTemporaryErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := failing{ln, make(chan error, 2)}
	f.errs <- outOfFiles{}
	f.errs <- outOfFiles{}
	plain, _ := Split(f, nil)
	defer plain.Close()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			c.Write([]byte("G"))
			defer c.Close()
			time.Sleep(time.Second)
		}
	}()
	accepted := make(chan error, 1)
	go func() {
		c, err := plain.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("Accept returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the port stopped accepting after running out of file handles")
	}
}

// TestSplitStopsOnOtherErrors checks an error that won't pass ends both
// halves with that error, so the server stops and says why rather than
// retrying for ever.
func TestSplitStopsOnOtherErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	broken := errors.New("the network went away")
	f := failing{ln, make(chan error, 1)}
	f.errs <- broken
	plain, secure := Split(f, nil)
	for _, half := range []net.Listener{plain, secure} {
		if _, err := half.Accept(); !errors.Is(err, broken) {
			t.Fatalf("Accept returned %v, want %v", err, broken)
		}
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("the port is still open")
	}
}

// TestSplitClosesTheOldestSilentConnections floods the port with
// connections that send nothing, and checks only the newest few stay
// open, while a transfer that proved itself keeps going and a new request
// still gets an answer.
func TestSplitClosesTheOldestSilentConnections(t *testing.T) {
	defer func(m int, local func(net.Conn) bool) { maxUnproven, fromThisDevice = m, local }(maxUnproven, fromThisDevice)
	maxUnproven = 4
	fromThisDevice = func(net.Conn) bool { return false }

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := Split(ln, nil)
	inTransfer, release := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/transfer", func(w http.ResponseWriter, r *http.Request) {
		Prove(r.Context())
		close(inTransfer)
		<-release
		io.WriteString(w, "done")
	})
	srv := &http.Server{
		Handler:     mux,
		ConnState:   ConnState,
		ConnContext: ConnContext,
	}
	go srv.Serve(plain)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(path string) (string, error) {
		resp, err := client.Get("http://" + ln.Addr().String() + path)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}

	transferred := make(chan string, 1)
	go func() {
		body, err := get("/transfer")
		if err != nil {
			body = err.Error()
		}
		transferred <- body
	}()
	<-inTransfer

	var silent []net.Conn
	for range 10 {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		silent = append(silent, c)
	}
	closed := func(c net.Conn, wait time.Duration) bool {
		c.SetReadDeadline(time.Now().Add(wait))
		_, err := c.Read(make([]byte, 1))
		var ne net.Error
		return !errors.As(err, &ne) || !ne.Timeout()
	}
	for i, c := range silent[:6] {
		if !closed(c, 2*time.Second) {
			t.Errorf("silent connection %d is still open", i)
		}
	}
	for i, c := range silent[6:] {
		if closed(c, 200*time.Millisecond) {
			t.Errorf("silent connection %d was closed, but it's among the newest", 6+i)
		}
	}

	if body, err := get("/"); err != nil || body != "ok" {
		t.Fatalf("a new request got %q, %v", body, err)
	}
	close(release)
	if body := <-transferred; body != "done" {
		t.Fatalf("the transfer got %q", body)
	}
}

// TestUnwrapFindsTheCountedConnection checks Prove and ConnState find the
// connection Split counts inside what http.Server sees, TLS or not.
func TestUnwrapFindsTheCountedConnection(t *testing.T) {
	c := &tracked{}
	for _, seen := range []net.Conn{c, peekedConn{c, nil}, tls.Server(peekedConn{c, nil}, nil)} {
		if unwrap(seen) != c {
			t.Errorf("unwrap(%T) didn't find the connection", seen)
		}
	}
}

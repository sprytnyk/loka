package network

import (
	"bufio"
	"container/list"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// tlsHandshake is the first byte of every TLS connection. An HTTP request
// starts with a letter.
const tlsHandshake = 0x16

// firstByteTimeout is how long a new connection has to send its first byte.
const firstByteTimeout = 10 * time.Second

// maxUnproven is how many connections from other devices may be open
// while none of their requests has proven to be part of a transfer: new
// ones, ones in another request, and idle ones. Past it, the oldest is
// closed, so connections that send nothing can't use up the file handles
// a transfer needs. A device sends its request at once, so under a flood
// the oldest are the silent ones. A variable so tests can lower it.
var maxUnproven = 128

// fromThisDevice reports whether a connection comes from this device,
// which never counts against maxUnproven. A variable so tests on loopback
// can count their connections.
var fromThisDevice = func(c net.Conn) bool {
	a, ok := c.RemoteAddr().(*net.TCPAddr)
	return ok && a.IP.IsLoopback()
}

// Split shares one listening port between TLS and plain HTTP. It reads
// the first byte of each connection to tell them apart. Closing either
// listener closes ln and both.
func Split(ln net.Listener, config *tls.Config) (plain, secure net.Listener) {
	s := &splitter{ln: ln, closed: make(chan struct{})}
	p := &half{s: s, conns: make(chan net.Conn)}
	t := &half{s: s, conns: make(chan net.Conn)}
	go s.accept(p, t, config)
	return p, t
}

type splitter struct {
	ln        net.Listener
	closeOnce sync.Once
	closed    chan struct{}
	err       error // why accepting stopped, set before closed is closed

	mu       sync.Mutex
	unproven list.List // of *tracked, oldest first
}

// track counts c as unproven, as its newest connection. Past
// maxUnproven, it closes the oldest.
func (s *splitter) track(c *tracked) {
	if c.local {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.el != nil {
		s.unproven.MoveToBack(c.el)
		return
	}
	c.el = s.unproven.PushBack(c)
	if s.unproven.Len() > maxUnproven {
		oldest := s.unproven.Remove(s.unproven.Front()).(*tracked)
		oldest.el = nil
		oldest.Conn.Close()
	}
}

// untrack stops counting c.
func (s *splitter) untrack(c *tracked) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.el != nil {
		s.unproven.Remove(c.el)
		c.el = nil
	}
}

func (s *splitter) close(err error) {
	s.closeOnce.Do(func() {
		s.err = err
		close(s.closed)
		s.ln.Close()
	})
}

func (s *splitter) accept(plain, secure *half, config *tls.Config) {
	var wait time.Duration
	for {
		raw, err := s.ln.Accept()
		if ne, ok := err.(net.Error); ok && ne.Temporary() {
			// Such as running out of file handles: wait for some to close,
			// as http.Server does.
			wait = min(max(2*wait, 5*time.Millisecond), time.Second)
			select {
			case <-time.After(wait):
				continue
			case <-s.closed:
				return
			}
		}
		if err != nil {
			s.close(err)
			return
		}
		wait = 0
		conn := &tracked{Conn: raw, s: s, local: fromThisDevice(raw)}
		s.track(conn)
		go func() {
			conn.SetReadDeadline(time.Now().Add(firstByteTimeout))
			r := bufio.NewReader(conn)
			first, err := r.Peek(1)
			conn.SetReadDeadline(time.Time{})
			if err != nil {
				conn.Close()
				return
			}
			to, c := plain, net.Conn(peekedConn{conn, r})
			if first[0] == tlsHandshake {
				to, c = secure, tls.Server(c, config)
			}
			select {
			case to.conns <- c:
			case <-s.closed:
				conn.Close()
			}
		}()
	}
}

// tracked is a connection Split counts while it is unproven.
type tracked struct {
	net.Conn
	s     *splitter
	local bool          // from this device, so never counted
	el    *list.Element // in s.unproven while counted, guarded by s.mu
}

func (c *tracked) Close() error {
	c.s.untrack(c)
	return c.Conn.Close()
}

// ConnState counts again a connection Split handed out once it is idle,
// and stops counting it once closed. It is for http.Server.ConnState.
func ConnState(c net.Conn, state http.ConnState) {
	t := unwrap(c)
	if t == nil {
		return
	}
	switch state {
	case http.StateIdle:
		t.s.track(t)
	case http.StateClosed, http.StateHijacked:
		t.s.untrack(t)
	}
}

// connKey holds a request's connection in its context.
type connKey struct{}

// ConnContext keeps c in ctx, for Prove. It is for
// http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// Prove stops counting the connection of the request with ctx while that
// request is part of a transfer, so a flood can't close it. Once the
// request ends and the connection is idle, ConnState counts it again.
func Prove(ctx context.Context) {
	c, _ := ctx.Value(connKey{}).(net.Conn)
	if t := unwrap(c); t != nil {
		t.s.untrack(t)
	}
}

// unwrap finds the connection Split counts inside c, or nil.
func unwrap(c net.Conn) *tracked {
	for {
		switch v := c.(type) {
		case *tracked:
			return v
		case *tls.Conn:
			c = v.NetConn()
		case peekedConn:
			c = v.Conn
		default:
			return nil
		}
	}
}

// half is one of the two listeners Split returns.
type half struct {
	s     *splitter
	conns chan net.Conn
}

func (h *half) Accept() (net.Conn, error) {
	select {
	case c := <-h.conns:
		return c, nil
	case <-h.s.closed:
		return nil, h.s.err
	}
}

func (h *half) Close() error {
	h.s.close(net.ErrClosed)
	return nil
}

func (h *half) Addr() net.Addr { return h.s.ln.Addr() }

// peekedConn reads first what was peeked from the connection.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

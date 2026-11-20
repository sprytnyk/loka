package network

import (
	"bufio"
	"crypto/tls"
	"net"
	"sync"
	"time"
)

// tlsHandshake is the first byte of every TLS connection. An HTTP request
// starts with a letter.
const tlsHandshake = 0x16

// firstByteTimeout is how long a new connection has to send its first byte.
const firstByteTimeout = 10 * time.Second

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
}

func (s *splitter) close(err error) {
	s.closeOnce.Do(func() {
		s.err = err
		close(s.closed)
		s.ln.Close()
	})
}

func (s *splitter) accept(plain, secure *half, config *tls.Config) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.close(err)
			return
		}
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

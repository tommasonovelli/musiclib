package pgtest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Proxy is a TCP proxy between a test and the PostgreSQL server that loses
// the database at chosen points of the wire protocol, so that the handling
// of a lost connection and of an uncertain commit (DESIGN.md §6.4, §12.2)
// is tested against the real server instead of a mock.
//
// It understands just enough of the protocol (v3, no TLS) to find a COMMIT
// sent by the simple query protocol, which is how pgx sends it.
type Proxy struct {
	// URL reaches the database of NewProxy through the proxy.
	URL string

	target string
	ln     net.Listener

	mu     sync.Mutex
	action commitAction
	conns  map[*proxyConn]struct{}
	wg     sync.WaitGroup
}

type commitAction int

const (
	commitPass commitAction = iota
	// commitLoseAck forwards the next COMMIT, lets the server finish it,
	// then closes the connection before the client sees the answer: the
	// transaction is durable and the client cannot know it.
	commitLoseAck
	// commitCutBefore closes the connection instead of forwarding the next
	// COMMIT: the server rolls back, and the client cannot know it either.
	commitCutBefore
)

// NewProxy starts a proxy to the database at databaseURL (a URL returned by
// EmptyDB), stopped when the test ends.
func NewProxy(t testing.TB, databaseURL string) *Proxy {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("sslmode") != "disable" {
		t.Fatalf("the proxy needs sslmode=disable in %s", envURL)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{target: u.Host, ln: ln, conns: map[*proxyConn]struct{}{}}
	u.Host = ln.Addr().String()
	p.URL = u.String()
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

// LoseNextCommitAck makes the next COMMIT succeed on the server and its
// answer never reach the client.
func (p *Proxy) LoseNextCommitAck() { p.arm(commitLoseAck) }

// CutBeforeNextCommit makes the connection drop instead of delivering the
// next COMMIT to the server.
func (p *Proxy) CutBeforeNextCommit() { p.arm(commitCutBefore) }

// CutAll drops every open connection now.
func (p *Proxy) CutAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.close()
	}
}

func (p *Proxy) arm(a commitAction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.action = a
}

// takeAction returns the armed action and disarms it: it applies to one
// COMMIT only.
func (p *Proxy) takeAction() commitAction {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.action
	p.action = commitPass
	return a
}

func (p *Proxy) close() {
	_ = p.ln.Close()
	p.CutAll()
	p.wg.Wait()
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		c := &proxyConn{client: client, server: server}
		p.mu.Lock()
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(2)
		go func() {
			defer p.wg.Done()
			p.clientToServer(c)
			c.close()
		}()
		go func() {
			defer p.wg.Done()
			c.serverToClient()
			c.close()
			p.mu.Lock()
			delete(p.conns, c)
			p.mu.Unlock()
		}()
	}
}

type proxyConn struct {
	client, server net.Conn

	mu sync.Mutex
	// dropping: the server's answers are discarded until its next
	// ReadyForQuery, then the connection is closed.
	dropping bool
	once     sync.Once
}

func (c *proxyConn) close() {
	c.once.Do(func() {
		_ = c.client.Close()
		_ = c.server.Close()
	})
}

// clientToServer forwards the startup message, then one typed message at a
// time, acting on an armed COMMIT.
func (p *Proxy) clientToServer(c *proxyConn) {
	startup, err := readUntyped(c.client)
	if err != nil || writeAll(c.server, startup) != nil {
		return
	}
	for {
		msg, err := readTyped(c.client)
		if err != nil {
			return
		}
		if isCommit(msg) {
			switch p.takeAction() {
			case commitCutBefore:
				return
			case commitLoseAck:
				c.mu.Lock()
				c.dropping = true
				c.mu.Unlock()
			}
		}
		if writeAll(c.server, msg) != nil {
			return
		}
	}
}

// serverToClient forwards the server's messages, except while dropping.
func (c *proxyConn) serverToClient() {
	for {
		msg, err := readTyped(c.server)
		if err != nil {
			return
		}
		c.mu.Lock()
		dropping := c.dropping
		c.mu.Unlock()
		if dropping {
			if msg[0] == 'Z' { // ReadyForQuery: the COMMIT is over.
				return
			}
			continue
		}
		if writeAll(c.client, msg) != nil {
			return
		}
	}
}

// isCommit recognizes a simple Query message whose text is COMMIT.
func isCommit(msg []byte) bool {
	if msg[0] != 'Q' {
		return false
	}
	text := strings.TrimSpace(string(bytes.TrimRight(msg[5:], "\x00")))
	return strings.EqualFold(text, "commit")
}

// readUntyped reads a length-prefixed message without a type byte (the
// startup message).
func readUntyped(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < 4 || n > 1<<20 {
		return nil, errors.New("pgtest proxy: bad startup length")
	}
	msg := make([]byte, n)
	copy(msg, hdr[:])
	_, err := io.ReadFull(r, msg[4:])
	return msg, err
}

// readTyped reads one message: type byte, length, body.
func readTyped(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n > 1<<30 {
		return nil, errors.New("pgtest proxy: bad message length")
	}
	msg := make([]byte, 1+n)
	copy(msg, hdr[:])
	_, err := io.ReadFull(r, msg[5:])
	return msg, err
}

func writeAll(w io.Writer, b []byte) error {
	_, err := w.Write(b)
	return err
}

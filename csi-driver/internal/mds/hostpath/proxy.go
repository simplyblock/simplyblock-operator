// The proxy: on the host's network, it opens the connections the forwarder
// names and carries them. On the node's own addresses SPDK's interface-bound
// listeners accept them from the host and refuse them from a pod; on other
// nodes, the host reaches the storage data network where the pod network may
// not.

package hostpath

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// DefaultDialTimeout bounds the proxy's connect to a listener, and the
// forwarder's to the proxy. A local listener answers at once, so a connect that
// takes longer is one that will not complete.
const DefaultDialTimeout = 5 * time.Second

// requestTimeout bounds how long a peer may take to send its request line.
const requestTimeout = 10 * time.Second

// DialFunc opens a connection, as net.Dialer.DialContext does.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Proxy serves the protocol on the host's network.
type Proxy struct {
	// Addrs returns the addresses CONNECT may name and ADDRS lists. In the
	// binary it is LocalAddrs, read on every request so an address added to
	// the node is offered without a restart.
	Addrs func() ([]netip.Addr, error)
	// Ports returns the storage cluster's listener ports, which CONNECT may
	// name on an address that is not the node's own. Nil allows none.
	Ports func() ([]uint16, error)
	// Dial opens the connection to a listener.
	Dial DialFunc
	// DialTimeout bounds Dial, DefaultDialTimeout when zero.
	DialTimeout time.Duration
	// Log reports each refused or failed request.
	Log func(format string, args ...any)
}

// Serve accepts connections on ln until ctx ends or ln is closed.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	return acceptLoop(ctx, ln, p.handle)
}

func (p *Proxy) handle(ctx context.Context, conn net.Conn) {
	defer closeQuietly(conn)
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := readLine(r)
	if err != nil {
		p.logf("reading a request from %s: %v", conn.RemoteAddr(), err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	req, err := ParseRequest(line)
	if err != nil {
		p.refuse(conn, err)
		return
	}
	addrs, err := p.Addrs()
	if err != nil {
		p.refuse(conn, err)
		return
	}
	switch req.Verb {
	case VerbAddrs:
		words := make([]string, 0, len(addrs))
		for _, a := range addrs {
			words = append(words, a.String())
		}
		_, _ = io.WriteString(conn, okLine(words...))
	case VerbConnect:
		if err := p.allowed(addrs, req.Target); err != nil {
			p.refuse(conn, err)
			return
		}
		timeout := p.DialTimeout
		if timeout == 0 {
			timeout = DefaultDialTimeout
		}
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		target, err := p.Dial(dialCtx, "tcp4", req.Target.String())
		cancel()
		if err != nil {
			p.refuse(conn, fmt.Errorf("connecting to %s: %w", req.Target, err))
			return
		}
		defer closeQuietly(target)
		if _, err := io.WriteString(conn, okLine()); err != nil {
			return
		}
		// Whatever the forwarder sent after its request line is already in r.
		splice(ctx, &bufferedConn{Conn: conn, r: r}, target)
	}
}

// allowed says whether CONNECT may open target: any port on an address of the
// node's own, or a storage listener port on another unicast address. The
// listeners authorize every connect by host NQN, so a target the second rule
// admits lets nobody past the control plane's authorization.
func (p *Proxy) allowed(addrs []netip.Addr, target netip.AddrPort) error {
	if slices.Contains(addrs, target.Addr()) {
		return nil
	}
	if !target.Addr().IsGlobalUnicast() {
		return fmt.Errorf("%s is neither an address of this node nor a unicast one", target.Addr())
	}
	if p.Ports == nil {
		return fmt.Errorf("%s is not an address of this node", target.Addr())
	}
	ports, err := p.Ports()
	if err != nil {
		return fmt.Errorf("reading the storage listener ports: %w", err)
	}
	if !slices.Contains(ports, target.Port()) {
		return fmt.Errorf("%s is not an address of this node, and %d is no storage listener port",
			target.Addr(), target.Port())
	}
	return nil
}

func (p *Proxy) refuse(conn net.Conn, err error) {
	p.logf("refused %s: %v", conn.RemoteAddr(), err)
	_, _ = io.WriteString(conn, errLine(err))
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// acceptLoop hands every accepted connection to handle on its own goroutine,
// and closes ln when ctx ends, which is what ends Accept.
func acceptLoop(ctx context.Context, ln net.Listener, handle func(context.Context, net.Conn)) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("accepting on %s: %w", ln.Addr(), err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(ctx, conn)
		}()
	}
}

// bufferedConn reads through r, which holds what was read past a line.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// closeWriter is a connection whose sending half closes on its own, which is
// how an end of stream on one side reaches the other without cutting the
// replies still coming back.
type closeWriter interface {
	CloseWrite() error
}

// splice copies both ways until both directions have ended, or ctx ends. Each
// direction that ends closes the other side's sending half, so a peer that
// half-closes still gets its answer.
func splice(ctx context.Context, a, b net.Conn) {
	stop := context.AfterFunc(ctx, func() {
		_ = a.Close()
		_ = b.Close()
	})
	defer stop()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pipe(b, a)
	}()
	go func() {
		defer wg.Done()
		pipe(a, b)
	}()
	wg.Wait()
}

// pipe copies src to dst and then ends dst's sending half, or closes dst when
// it cannot be half-closed.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if cw, ok := unwrap(dst).(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = dst.Close()
}

func unwrap(c net.Conn) net.Conn {
	if b, ok := c.(*bufferedConn); ok {
		return b.Conn
	}
	return c
}

func closeQuietly(c io.Closer) { _ = c.Close() }

// The forwarder: in the metadata server pod, it accepts the guest's connections
// the runner redirected, reads where each was headed, and hands it to the proxy
// on the host.

package hostpath

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Forwarder serves the redirected connections.
type Forwarder struct {
	// Proxy is the proxy's host:port.
	Proxy string
	// Dial opens the connection to the proxy.
	Dial DialFunc
	// OriginalDst returns where a redirected connection was headed before the
	// redirect. In the runner it is OriginalDestination.
	OriginalDst func(net.Conn) (netip.AddrPort, error)
	// DialTimeout bounds the connect to the proxy and its answer,
	// DefaultDialTimeout when zero.
	DialTimeout time.Duration
	// Log reports each connection that could not be carried.
	Log func(format string, args ...any)
}

// Serve accepts connections on ln until ctx ends or ln is closed.
func (f *Forwarder) Serve(ctx context.Context, ln net.Listener) error {
	return acceptLoop(ctx, ln, f.handle)
}

func (f *Forwarder) handle(ctx context.Context, guest net.Conn) {
	defer closeQuietly(guest)
	target, err := f.OriginalDst(guest)
	if err != nil {
		f.logf("reading where %s was headed: %v", guest.RemoteAddr(), err)
		return
	}
	proxy, r, err := f.connect(ctx, target)
	if err != nil {
		f.logf("carrying %s to %s: %v", guest.RemoteAddr(), target, err)
		return
	}
	defer closeQuietly(proxy)
	splice(ctx, guest, &bufferedConn{Conn: proxy, r: r})
}

// connect opens the proxy connection for target and waits for its answer.
func (f *Forwarder) connect(ctx context.Context, target netip.AddrPort) (net.Conn, *bufio.Reader, error) {
	timeout := f.DialTimeout
	if timeout == 0 {
		timeout = DefaultDialTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := f.Dial(dialCtx, "tcp4", f.Proxy)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to the proxy at %s: %w", f.Proxy, err)
	}
	// The proxy answers only once its own connect is done, so the answer is
	// given the proxy's connect timeout and a margin.
	_ = conn.SetDeadline(time.Now().Add(2 * timeout))
	r := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, ConnectRequest(target)); err != nil {
		closeQuietly(conn)
		return nil, nil, fmt.Errorf("asking the proxy: %w", err)
	}
	line, err := readLine(r)
	if err != nil {
		closeQuietly(conn)
		return nil, nil, fmt.Errorf("reading the proxy's answer: %w", err)
	}
	if line != answerOK {
		closeQuietly(conn)
		return nil, nil, errors.New("the proxy answered " + strings.TrimSpace(line))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, r, nil
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

// QueryAddrs asks the proxy at proxy for its node's addresses.
func QueryAddrs(ctx context.Context, dial DialFunc, proxy string) ([]netip.Addr, error) {
	dialCtx, cancel := context.WithTimeout(ctx, DefaultDialTimeout)
	defer cancel()
	conn, err := dial(dialCtx, "tcp4", proxy)
	if err != nil {
		return nil, fmt.Errorf("connecting to the proxy at %s: %w", proxy, err)
	}
	defer closeQuietly(conn)
	_ = conn.SetDeadline(time.Now().Add(DefaultDialTimeout))
	if _, err := io.WriteString(conn, AddrsRequest()); err != nil {
		return nil, fmt.Errorf("asking the proxy at %s: %w", proxy, err)
	}
	line, err := readLine(bufio.NewReader(conn))
	if err != nil {
		return nil, fmt.Errorf("reading the proxy's answer: %w", err)
	}
	return ParseAddrsReply(line)
}

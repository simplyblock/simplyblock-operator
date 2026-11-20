// Tests for the guest's path to its own node: the one-line protocol, the
// addresses the proxy offers, the proxy and the forwarder against real loopback
// sockets, and the refresher that keeps the pod's redirects current.

package hostpath

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConnectRequestRoundTrips(t *testing.T) {
	target := netip.MustParseAddrPort("10.10.10.143:4430")
	req, err := ParseRequest(strings.TrimSuffix(ConnectRequest(target), "\n"))
	if err != nil {
		t.Fatalf("ParseRequest(ConnectRequest(%s)): %v", target, err)
	}
	if req.Verb != VerbConnect || req.Target != target {
		t.Errorf("ParseRequest = %+v, want CONNECT %s", req, target)
	}
	req, err = ParseRequest(strings.TrimSuffix(AddrsRequest(), "\n"))
	if err != nil || req.Verb != VerbAddrs {
		t.Errorf("ParseRequest(AddrsRequest()) = %+v, %v, want ADDRS", req, err)
	}
}

func TestParseRequestRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, line := range []string{
		"",
		"CONNECT 10.10.10.143:4430",            // no magic
		"SBMDS2 CONNECT 10.10.10.143:4430",     // another protocol version
		"SBMDS1 CONNECT",                       // no target
		"SBMDS1 CONNECT 10.10.10.143",          // no port
		"SBMDS1 CONNECT [fd00::1]:4430",        // IPv6
		"SBMDS1 CONNECT 10.10.10.143:4430 x",   // trailing field
		"SBMDS1 ADDRS 10.10.10.143",            // ADDRS takes nothing
		"SBMDS1 LISTEN 10.10.10.143:4430",      // unknown verb
		"SBMDS1 CONNECT ::ffff:10.0.0.1:4430",  // a mapped address is not an IPv4 one
		"SBMDS1 CONNECT 10.10.10.143:notaport", // not a number
	} {
		if req, err := ParseRequest(line); err == nil {
			t.Errorf("ParseRequest(%q) = %+v, want an error", line, req)
		}
	}
}

func TestParseAddrsReply(t *testing.T) {
	got, err := ParseAddrsReply("OK 10.10.10.143 192.168.10.143")
	if err != nil {
		t.Fatalf("ParseAddrsReply: %v", err)
	}
	want := []netip.Addr{netip.MustParseAddr("10.10.10.143"), netip.MustParseAddr("192.168.10.143")}
	if !slices.Equal(got, want) {
		t.Errorf("ParseAddrsReply = %v, want %v", got, want)
	}
	if got, err := ParseAddrsReply("OK"); err != nil || len(got) != 0 {
		t.Errorf("ParseAddrsReply(OK) = %v, %v, want no addresses", got, err)
	}
	for _, line := range []string{"ERR no addresses", "OK 10.0.0", "10.0.0.1"} {
		if _, err := ParseAddrsReply(line); err == nil {
			t.Errorf("ParseAddrsReply(%q) succeeded, want an error", line)
		}
	}
}

func TestUsableKeepsTheNodesRoutableIPv4Addresses(t *testing.T) {
	in := []netip.Addr{
		netip.MustParseAddr("192.168.10.143"),
		netip.MustParseAddr("127.0.0.1"),       // never a storage node's listener
		netip.MustParseAddr("169.254.100.1"),   // could be the guest's own bridge
		netip.MustParseAddr("fd00::143"),       // the guest connects over IPv4
		netip.MustParseAddr("10.10.10.143"),    //
		netip.MustParseAddr("::ffff:10.0.0.9"), // mapped: counts as its IPv4 address
		netip.MustParseAddr("10.10.10.143"),    // twice on two interfaces
	}
	got := Usable(in)
	want := []netip.Addr{
		netip.MustParseAddr("10.0.0.9"),
		netip.MustParseAddr("10.10.10.143"),
		netip.MustParseAddr("192.168.10.143"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("Usable = %v, want %v", got, want)
	}
}

// listen opens a loopback listener closed with the test.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func addrPort(t *testing.T, ln net.Listener) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("parse %s: %v", ln.Addr(), err)
	}
	return ap
}

// serve runs s in the background until the test ends.
func serve(t *testing.T, s func(context.Context, net.Listener) error, ln net.Listener) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		<-done
	})
}

// backend answers every connection with what it read, prefixed, once the peer
// has closed its sending half: a target that only answers a complete request
// proves half-closes are carried through.
func backend(t *testing.T) net.Listener {
	t.Helper()
	ln := listen(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				got, _ := io.ReadAll(conn)
				_, _ = conn.Write(append([]byte("echo:"), got...))
			}()
		}
	}()
	return ln
}

// dialer records the addresses it is asked to dial.
type dialer struct {
	mu    sync.Mutex
	dials []string
}

func (d *dialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, address)
	d.mu.Unlock()
	var netDialer net.Dialer
	return netDialer.DialContext(ctx, network, address)
}

func (d *dialer) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.dials)
}

// exchange sends request on a new connection to addr and returns the answer
// line and the connection.
func exchange(t *testing.T, addr netip.AddrPort, request string) (string, net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", addr.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the answer to %q: %v", request, err)
	}
	return strings.TrimSuffix(line, "\n"), conn, r
}

func newProxy(t *testing.T, addrs []netip.Addr, d *dialer) netip.AddrPort {
	t.Helper()
	ln := listen(t)
	p := &Proxy{
		Addrs: func() ([]netip.Addr, error) { return addrs, nil },
		Dial:  d.dial,
		Log:   t.Logf,
	}
	serve(t, p.Serve, ln)
	return addrPort(t, ln)
}

func TestProxyRelaysToAnAddressTheNodeHolds(t *testing.T) {
	target := addrPort(t, backend(t))
	proxy := newProxy(t, []netip.Addr{target.Addr()}, &dialer{})

	answer, conn, r := exchange(t, proxy, ConnectRequest(target))
	if answer != "OK" {
		t.Fatalf("CONNECT answered %q, want OK", answer)
	}
	if _, err := io.WriteString(conn, "icreq"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "echo:icreq" {
		t.Errorf("relayed %q, want echo:icreq", got)
	}
}

func TestProxyRefusesAnAddressTheNodeDoesNotHold(t *testing.T) {
	d := &dialer{}
	proxy := newProxy(t, []netip.Addr{netip.MustParseAddr("10.10.10.143")}, d)

	answer, _, r := exchange(t, proxy, ConnectRequest(netip.MustParseAddrPort("127.0.0.1:22")))
	if !strings.HasPrefix(answer, "ERR ") {
		t.Errorf("CONNECT to an address the node does not offer answered %q, want ERR", answer)
	}
	if rest, _ := io.ReadAll(r); len(rest) != 0 {
		t.Errorf("the proxy sent %q after ERR, want it to close", rest)
	}
	if dials := d.seen(); len(dials) != 0 {
		t.Errorf("the proxy dialed %v for a refused target", dials)
	}
}

func newPortsProxy(t *testing.T, addrs []netip.Addr, ports []uint16, d *dialer) netip.AddrPort {
	t.Helper()
	ln := listen(t)
	p := &Proxy{
		Addrs: func() ([]netip.Addr, error) { return addrs, nil },
		Ports: func() ([]uint16, error) { return ports, nil },
		Dial:  d.dial,
		Log:   t.Logf,
	}
	serve(t, p.Serve, ln)
	return addrPort(t, ln)
}

// Without its own attachment to the data network, the guest reaches every
// storage node through the proxy: on OVN-Kubernetes a pod's traffic to the data
// network leaves through the gateway bridge and never arrives. The proxy then
// opens connections to other nodes too, but only to the storage cluster's
// listener ports, so it is no relay into the host's network.
func TestProxyDialsAnotherNodeOnlyOnAStorageListenerPort(t *testing.T) {
	d := &dialer{}
	own := []netip.Addr{netip.MustParseAddr("10.10.10.245")}
	proxy := newPortsProxy(t, own, []uint16{4429, 4431}, d)

	for _, refused := range []string{"10.10.10.244:22", "127.0.0.1:4429", "169.254.169.254:4429", "224.0.0.1:4429"} {
		answer, _, _ := exchange(t, proxy, ConnectRequest(netip.MustParseAddrPort(refused)))
		if !strings.HasPrefix(answer, "ERR ") {
			t.Errorf("CONNECT %s answered %q, want ERR", refused, answer)
		}
	}
	if dials := d.seen(); len(dials) != 0 {
		t.Fatalf("the proxy dialed %v for refused targets", dials)
	}

	// Another node's storage listener is dialed. Nothing listens there in the
	// test, so the connect fails, but the proxy tried it.
	_, _, _ = exchange(t, proxy, ConnectRequest(netip.MustParseAddrPort("10.10.10.244:4429")))
	if dials := d.seen(); !slices.Contains(dials, "10.10.10.244:4429") {
		t.Errorf("the proxy dialed %v, want 10.10.10.244:4429", dials)
	}
}

func TestParsePortsReadsWhatTheOperatorWrites(t *testing.T) {
	got, err := ParsePorts("4429\n4431 4433\n")
	if err != nil || !slices.Equal(got, []uint16{4429, 4431, 4433}) {
		t.Errorf("ParsePorts = %v, %v, want [4429 4431 4433]", got, err)
	}
	for _, bad := range []string{"0", "65536", "lvol"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("ParsePorts(%q) accepted it", bad)
		}
	}
	if got, err := ReadPorts(t.TempDir() + "/absent"); err != nil || got != nil {
		t.Errorf("ReadPorts of a missing file = %v, %v, want no ports and no error", got, err)
	}
}

func TestProxyReportsAConnectThatFails(t *testing.T) {
	gone := listen(t)
	target := addrPort(t, gone)
	_ = gone.Close()
	proxy := newProxy(t, []netip.Addr{target.Addr()}, &dialer{})

	answer, _, _ := exchange(t, proxy, ConnectRequest(target))
	if !strings.HasPrefix(answer, "ERR ") {
		t.Errorf("CONNECT to a closed port answered %q, want ERR", answer)
	}
}

func TestProxyAnswersWithTheNodesAddresses(t *testing.T) {
	addrs := []netip.Addr{netip.MustParseAddr("10.10.10.143"), netip.MustParseAddr("192.168.10.143")}
	proxy := newProxy(t, addrs, &dialer{})

	got, err := QueryAddrs(context.Background(), (&dialer{}).dial, proxy.String())
	if err != nil {
		t.Fatalf("QueryAddrs: %v", err)
	}
	if !slices.Equal(got, addrs) {
		t.Errorf("QueryAddrs = %v, want %v", got, addrs)
	}
}

func TestProxyAnswersAMalformedRequestWithAnError(t *testing.T) {
	proxy := newProxy(t, nil, &dialer{})
	answer, _, _ := exchange(t, proxy, "GET / HTTP/1.1\n")
	if !strings.HasPrefix(answer, "ERR ") {
		t.Errorf("a malformed request was answered %q, want ERR", answer)
	}
}

func newForwarder(t *testing.T, proxy netip.AddrPort, original netip.AddrPort) netip.AddrPort {
	t.Helper()
	ln := listen(t)
	f := &Forwarder{
		Proxy:       proxy.String(),
		Dial:        (&dialer{}).dial,
		OriginalDst: func(net.Conn) (netip.AddrPort, error) { return original, nil },
		Log:         t.Logf,
	}
	serve(t, f.Serve, ln)
	return addrPort(t, ln)
}

func TestForwarderCarriesTheGuestsStreamToItsOriginalDestination(t *testing.T) {
	target := addrPort(t, backend(t))
	d := &dialer{}
	proxy := newProxy(t, []netip.Addr{target.Addr()}, d)
	forwarder := newForwarder(t, proxy, target)

	guest, err := net.DialTimeout("tcp4", forwarder.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the forwarder: %v", err)
	}
	defer func() { _ = guest.Close() }()
	_ = guest.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(guest, "icreq"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := guest.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(guest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "echo:icreq" {
		t.Errorf("the guest read %q, want echo:icreq", got)
	}
	if dials := d.seen(); !slices.Equal(dials, []string{target.String()}) {
		t.Errorf("the proxy dialed %v, want only the original destination %s", dials, target)
	}
}

func TestForwarderClosesTheGuestsConnectionWhenTheProxyRefuses(t *testing.T) {
	proxy := newProxy(t, []netip.Addr{netip.MustParseAddr("10.10.10.143")}, &dialer{})
	forwarder := newForwarder(t, proxy, netip.MustParseAddrPort("10.10.10.144:4430"))

	guest, err := net.DialTimeout("tcp4", forwarder.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the forwarder: %v", err)
	}
	defer func() { _ = guest.Close() }()
	_ = guest.SetDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(guest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the guest read %q from a refused connection, want nothing", got)
	}
}

func TestForwarderClosesTheGuestsConnectionWhenTheProxyIsUnreachable(t *testing.T) {
	gone := listen(t)
	proxy := addrPort(t, gone)
	_ = gone.Close()
	forwarder := newForwarder(t, proxy, netip.MustParseAddrPort("10.10.10.143:4430"))

	guest, err := net.DialTimeout("tcp4", forwarder.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the forwarder: %v", err)
	}
	defer func() { _ = guest.Close() }()
	_ = guest.SetDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(guest); err != nil || len(got) != 0 {
		t.Errorf("read %q, %v, want an empty, closed connection", got, err)
	}
}

func TestRefresherAppliesOnlyWhatChanged(t *testing.T) {
	a := netip.MustParseAddr("10.10.10.143")
	b := netip.MustParseAddr("192.168.10.143")
	answers := []struct {
		addrs []netip.Addr
		err   error
	}{
		{err: errors.New("proxy not up yet")},
		{addrs: []netip.Addr{a}},
		{addrs: []netip.Addr{a}},
		{err: errors.New("proxy restarting")},
		{addrs: []netip.Addr{a, b}},
		{addrs: []netip.Addr{}},
	}
	var applied [][]netip.Addr
	call := 0
	r := &Refresher{
		Query: func(context.Context) ([]netip.Addr, error) {
			next := answers[call]
			call++
			return next.addrs, next.err
		},
		Apply: func(_ context.Context, addrs []netip.Addr) error {
			applied = append(applied, slices.Clone(addrs))
			return nil
		},
		Log: t.Logf,
	}
	for range answers {
		r.Tick(context.Background())
	}
	want := [][]netip.Addr{{a}, {a, b}, {}}
	if !slices.EqualFunc(applied, want, slices.Equal[[]netip.Addr]) {
		t.Errorf("applied %v, want %v: once per change, and never on a failed query", applied, want)
	}
}

func TestRefresherRetriesAnApplyThatFailed(t *testing.T) {
	a := netip.MustParseAddr("10.10.10.143")
	attempts := 0
	r := &Refresher{
		Query: func(context.Context) ([]netip.Addr, error) { return []netip.Addr{a}, nil },
		Apply: func(context.Context, []netip.Addr) error {
			attempts++
			if attempts == 1 {
				return errors.New("xtables lock")
			}
			return nil
		},
		Log: t.Logf,
	}
	r.Tick(context.Background())
	r.Tick(context.Background())
	r.Tick(context.Background())
	if attempts != 2 {
		t.Errorf("Apply ran %d times, want 2: again after the failure, then not at all", attempts)
	}
}

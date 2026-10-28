// Tests for the host check service's wire layer: that a selector and a report
// survive the round trip, that a refusal is an answer and not a failed call,
// and that a request the node would refuse is refused on both ends. What the
// host does with it is tested in the netcheck package.

package netcheckrpc

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/netcheck"
)

// aHost is the node's side: the interfaces it shows and what a connection does.
type aHost struct {
	report     netcheck.Report
	readErr    error
	connectErr error
	selectors  []netcheck.Selector
	connected  []string
	timeouts   []time.Duration
}

func (h *aHost) read(sel netcheck.Selector) (netcheck.Report, error) {
	h.selectors = append(h.selectors, sel)
	return h.report, h.readErr
}

func (h *aHost) connect(ctx context.Context, address string, port int) error {
	h.connected = append(h.connected, address+":"+itoa(port))
	if deadline, ok := ctx.Deadline(); ok {
		h.timeouts = append(h.timeouts, time.Until(deadline))
	}
	return h.connectErr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for ; n > 0; n /= 10 {
		digits = string(rune('0'+n%10)) + digits
	}
	return digits
}

func serve(t *testing.T, h *aHost) *Client {
	t.Helper()
	srv, err := NewServer(h.read, h.connect)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	srv.Register(grpcServer)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return Remote(conn)
}

func TestASelectorAndTheReportSurviveTheRoundTrip(t *testing.T) {
	h := &aHost{report: netcheck.Report{Found: true, Addresses: []string{"fe80::1/64", "10.10.100.8/24"}}}
	got, err := serve(t, h).InterfaceAddresses(context.Background(), netcheck.Selector{Name: "ens6", PCIAddress: "0000:00:06.0"})
	if err != nil {
		t.Fatalf("InterfaceAddresses: %v", err)
	}
	if !got.Found || !slices.Equal(got.Addresses, h.report.Addresses) {
		t.Errorf("report = %+v, want %+v in host order", got, h.report)
	}
	if len(h.selectors) != 1 || h.selectors[0] != (netcheck.Selector{Name: "ens6", PCIAddress: "0000:00:06.0"}) {
		t.Errorf("node got %+v, want the selector intact", h.selectors)
	}
}

func TestAnAbsentInterfaceIsAnAnswerAndNotAFailure(t *testing.T) {
	got, err := serve(t, &aHost{}).InterfaceAddresses(context.Background(), netcheck.Selector{Name: "ens9"})
	if err != nil {
		t.Fatalf("InterfaceAddresses: %v, want the absence returned as an answer", err)
	}
	if got.Found || len(got.Addresses) != 0 {
		t.Errorf("report = %+v, want not found", got)
	}
}

func TestAReadThatFailsOnTheNodeIsAFailedCall(t *testing.T) {
	_, err := serve(t, &aHost{readErr: errors.New("sysfs unreadable")}).
		InterfaceAddresses(context.Background(), netcheck.Selector{Name: "ens6"})
	if err == nil {
		t.Fatal("InterfaceAddresses succeeded though the node could not read its interfaces")
	}
}

func TestAConnectionThatSucceedsComesBackConnected(t *testing.T) {
	h := &aHost{}
	got, err := serve(t, h).Connect(context.Background(), "10.10.100.200", 4426, 3*time.Second)
	if err != nil || !got.Connected || got.Detail != "" {
		t.Fatalf("Connect = %+v, %v, want connected with no detail", got, err)
	}
	if len(h.connected) != 1 || h.connected[0] != "10.10.100.200:4426" {
		t.Errorf("node connected to %v, want 10.10.100.200:4426", h.connected)
	}
}

func TestARefusedConnectionIsAnAnswerAndNotAFailedCall(t *testing.T) {
	got, err := serve(t, &aHost{connectErr: errors.New("connection refused")}).
		Connect(context.Background(), "10.10.100.200", 4426, time.Second)
	if err != nil {
		t.Fatalf("Connect: %v, want the refusal returned as an answer", err)
	}
	if got.Connected || got.Detail != "connection refused" {
		t.Errorf("result = %+v, want not connected with the reason", got)
	}
}

func TestTheNodeAppliesTheTimeoutTheCallerAsksFor(t *testing.T) {
	h := &aHost{}
	if _, err := serve(t, h).Connect(context.Background(), "10.10.100.200", 4426, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(h.timeouts) != 1 || h.timeouts[0] <= 0 || h.timeouts[0] > 2*time.Second {
		t.Errorf("the node's deadline = %v, want at most the 2s asked for", h.timeouts)
	}
}

func TestARequestTheNodeWouldRefuseIsRefusedBeforeItIsSent(t *testing.T) {
	h := &aHost{}
	c := serve(t, h)
	if _, err := c.InterfaceAddresses(context.Background(), netcheck.Selector{}); !errors.Is(err, netcheck.ErrInvalidSelector) {
		t.Errorf("an empty selector: err = %v, want ErrInvalidSelector", err)
	}
	for name, call := range map[string]func() error{
		"port zero": func() error { _, err := c.Connect(context.Background(), "10.10.100.200", 0, time.Second); return err },
		"port too big": func() error {
			_, err := c.Connect(context.Background(), "10.10.100.200", 70000, time.Second)
			return err
		},
		"not an address": func() error {
			_, err := c.Connect(context.Background(), "ten", 4426, time.Second)
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: Connect succeeded, want it refused", name)
		}
	}
	if len(h.selectors) != 0 || len(h.connected) != 0 {
		t.Errorf("the node saw %d reads and %d connections, want none sent", len(h.selectors), len(h.connected))
	}
}

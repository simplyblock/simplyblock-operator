package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// fakeSubs is a nvme.SubsystemResolver whose ByNQN is supplied per test.
type fakeSubs struct {
	byNQN func(ctx context.Context, nqn string) (nvme.Subsystem, error)
}

func (f fakeSubs) List(context.Context) ([]nvme.Subsystem, error) { return nil, nil }
func (f fakeSubs) ByNQN(ctx context.Context, nqn string) (nvme.Subsystem, error) {
	return f.byNQN(ctx, nqn)
}

func notFound() (nvme.Subsystem, error) {
	return nvme.Subsystem{}, fmt.Errorf("subsystem: %w", errs.ErrNotFound)
}

// ctrl is a controller fronting addr over TCP on the default service port.
func ctrl(id, addr, state string) nvme.Controller {
	return nvme.Controller{
		ID:        nvme.ControllerID(id),
		Transport: "tcp",
		State:     state,
		Address:   nvme.Address{TrAddr: addr, TrSvcID: strconv.Itoa(defaultTrSvcID)},
	}
}

// mustOptions renders a target's connect options, failing the test if the host
// identity cannot be resolved. Tests that care about that error call
// fabricsOptions directly.
func mustOptions(t *testing.T, tgt Target, hostNQN, hostID string) string {
	t.Helper()
	opts, err := fabricsOptions(tgt, hostNQN, hostID)
	if err != nil {
		t.Fatalf("fabricsOptions: %v", err)
	}
	return opts
}

// liveSub is a single-path subsystem whose controller fronts addr and is live.
func liveSub(nqn, addr string) (nvme.Subsystem, error) {
	return nvme.Subsystem{NQN: nqn, Controllers: []nvme.Controller{ctrl("nvme0", addr, "live")}}, nil
}

func TestOptions(t *testing.T) {
	clt := 0
	opts := mustOptions(t, Target{
		NQN:               "nqn.test:vol",
		Address:           "10.0.0.1",
		NrIOQueues:        3,
		ReconnectDelaySec: 2,
		KeepAliveTMOSec:   4,
		CtrlLossTMOSec:    &clt,
	}, "host-nqn", "host-id")
	want := "transport=tcp,traddr=10.0.0.1,trsvcid=4420,nqn=nqn.test:vol," +
		"hostnqn=host-nqn,hostid=host-id,nr_io_queues=3,reconnect_delay=2," +
		"keep_alive_tmo=4,ctrl_loss_tmo=0"
	if opts != want {
		t.Errorf("options =\n  %q\nwant\n  %q", opts, want)
	}
}

func TestOptions_TargetOverridesHostIdentity(t *testing.T) {
	opts := mustOptions(t,
		Target{NQN: "n", Address: "a", Port: 4438, HostNQN: "t-nqn", HostID: "t-id"}, "node-nqn", "node-id")
	want := "transport=tcp,traddr=a,trsvcid=4438,nqn=n,hostnqn=t-nqn,hostid=t-id"
	if opts != want {
		t.Errorf("options = %q, want %q", opts, want)
	}
}

// The kernel takes the DHCHAP secrets in the options line itself, and takes the
// host identity as a pair: a target naming its own host NQN keeps the node's
// hostid out of the line entirely.
func TestOptions_CarriesDHCHAPAuth(t *testing.T) {
	opts := mustOptions(t, Target{
		NQN: "n", Address: "a",
		HostNQN:          sbHostNQN,
		DHCHAPSecret:     "DHHC-1:00:host-secret:",
		DHCHAPCtrlSecret: "DHHC-1:00:ctrl-secret:",
	}, "node-nqn", "node-id")
	want := "transport=tcp,traddr=a,trsvcid=4420,nqn=n," +
		"hostnqn=" + sbHostNQN + ",hostid=" + sbHostID +
		",dhchap_secret=DHHC-1:00:host-secret:,dhchap_ctrl_secret=DHHC-1:00:ctrl-secret:"
	if opts != want {
		t.Errorf("options =\n  %q\nwant\n  %q", opts, want)
	}
}

func TestOptions_OmitsDHCHAPWhenUnset(t *testing.T) {
	opts := mustOptions(t, Target{NQN: "n", Address: "a"}, "", "")
	if strings.Contains(opts, "dhchap") {
		t.Errorf("options = %q, want no dhchap options for an ungated volume", opts)
	}
}

func TestConnect_WritesFabricsThenWaitsLive(t *testing.T) {
	connected := false
	var attached string
	c := &FabricsConnector{connector: connector{
		hostNQN: "h", hostID: "i", poll: time.Millisecond, pathTimeout: time.Second,
		subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			if !connected {
				return notFound()
			}
			return liveSub(nqn, "10.0.0.1")
		}},
		attach: func(_ context.Context, tgt Target) (string, error) {
			attached = tgt.Address
			connected = true
			return "instance=0,cntlid=1", nil
		}}}
	if err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if attached == "" {
		t.Error("connect was not written to the fabrics device")
	}
}

func TestConnect_IdempotentWhenPathAlreadyLive(t *testing.T) {
	called := false
	c := &FabricsConnector{connector: connector{
		poll: time.Millisecond, pathTimeout: time.Second,
		subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			return liveSub(nqn, "10.0.0.1")
		}},
		attach: func(context.Context, Target) (string, error) { called = true; return "", nil }}}
	if err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("connect wrote the fabrics device despite an already-live controller for this path")
	}
}

// A live controller at another address is a different path, not this one. A
// storage node whose IP changed must still be connected at its new address.
func TestConnect_ConnectsWhenOnlyAnotherPathIsLive(t *testing.T) {
	connected := false
	c := &FabricsConnector{connector: connector{
		poll: time.Millisecond, pathTimeout: time.Second,
		subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			s, _ := liveSub(nqn, "10.0.0.1")
			if connected {
				s.Controllers = append(s.Controllers, ctrl("nvme1", "10.0.0.2", "live"))
			}
			return s, nil
		}},
		attach: func(context.Context, Target) (string, error) { connected = true; return "", nil }}}
	if err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	if !connected {
		t.Error("connect did not attach the new path")
	}
}

func TestConnect_WriteErrorPropagates(t *testing.T) {
	c := &FabricsConnector{connector: connector{
		poll: time.Millisecond, pathTimeout: time.Second,
		subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return notFound() }},
		attach: func(context.Context, Target) (string, error) {
			return "", errors.New("connection refused")
		}}}
	err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "a"})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want to wrap the write error", err)
	}
}

func TestDisconnect_WritesDeleteControllerForEachPath(t *testing.T) {
	dir := t.TempDir()
	var paths []nvme.Controller
	for _, name := range []string{"nvme0", "nvme1"} { // multipath: two controllers
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, deleteControllerAttr), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, nvme.Controller{ID: nvme.ControllerID(name), SysfsPath: p})
	}
	// The real constructor, so this exercises the actual delete_controller write
	// rather than a stub standing in for it.
	c := NewFabricsConnector(fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
		return nvme.Subsystem{NQN: nqn, Controllers: paths}, nil
	}})

	if err := c.Disconnect(context.Background(), "nqn.x"); err != nil {
		t.Fatal(err)
	}
	for _, ctrl := range paths {
		b, err := os.ReadFile(filepath.Join(ctrl.SysfsPath, deleteControllerAttr))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "1" {
			t.Errorf("%s delete_controller = %q, want \"1\"", ctrl.ID, b)
		}
	}
}

func TestDisconnect_IdempotentWhenAbsent(t *testing.T) {
	c := &FabricsConnector{connector: connector{subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}}}}
	if err := c.Disconnect(context.Background(), "nqn.gone"); err != nil {
		t.Errorf("Disconnect of absent subsystem = %v, want nil", err)
	}
}

func TestIsConnected(t *testing.T) {
	ctx := context.Background()
	t.Run("live", func(t *testing.T) {
		c := &FabricsConnector{connector: connector{subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			return liveSub(nqn, "10.0.0.1")
		}}}}
		if ok, err := c.IsConnected(ctx, "n"); err != nil || !ok {
			t.Errorf("IsConnected = %v, %v; want true, nil", ok, err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		c := &FabricsConnector{connector: connector{subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
			return notFound()
		}}}}
		if ok, err := c.IsConnected(ctx, "n"); err != nil || ok {
			t.Errorf("IsConnected = %v, %v; want false, nil", ok, err)
		}
	})
	t.Run("present but not live", func(t *testing.T) {
		c := &FabricsConnector{connector: connector{subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			return nvme.Subsystem{NQN: nqn, Controllers: []nvme.Controller{{State: "connecting"}}}, nil
		}}}}
		if ok, err := c.IsConnected(ctx, "n"); err != nil || ok {
			t.Errorf("IsConnected = %v, %v; want false, nil", ok, err)
		}
	})
}

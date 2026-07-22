package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func liveSub(nqn string) (nvme.Subsystem, error) {
	return nvme.Subsystem{NQN: nqn, Controllers: []nvme.Controller{{ID: "nvme0", State: "live"}}}, nil
}

func TestOptions(t *testing.T) {
	clt := 0
	c := &FabricsConnector{hostNQN: "host-nqn", hostID: "host-id"}
	opts := c.options(Target{
		NQN:               "nqn.test:vol",
		Address:           "10.0.0.1",
		NrIOQueues:        3,
		ReconnectDelaySec: 2,
		KeepAliveTMOSec:   4,
		CtrlLossTMOSec:    &clt,
	})
	want := "transport=tcp,traddr=10.0.0.1,trsvcid=4420,nqn=nqn.test:vol," +
		"hostnqn=host-nqn,hostid=host-id,nr_io_queues=3,reconnect_delay=2," +
		"keep_alive_tmo=4,ctrl_loss_tmo=0"
	if opts != want {
		t.Errorf("options =\n  %q\nwant\n  %q", opts, want)
	}
}

func TestOptions_TargetOverridesHostIdentity(t *testing.T) {
	c := &FabricsConnector{hostNQN: "node-nqn", hostID: "node-id"}
	opts := c.options(Target{NQN: "n", Address: "a", Port: 4438, HostNQN: "t-nqn", HostID: "t-id"})
	want := "transport=tcp,traddr=a,trsvcid=4438,nqn=n,hostnqn=t-nqn,hostid=t-id"
	if opts != want {
		t.Errorf("options = %q, want %q", opts, want)
	}
}

func TestConnect_WritesFabricsThenWaitsLive(t *testing.T) {
	connected := false
	var gotOpts string
	c := &FabricsConnector{
		hostNQN: "h", hostID: "i", poll: time.Millisecond,
		subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			if !connected {
				return notFound()
			}
			return liveSub(nqn)
		}},
		connect: func(_ context.Context, opts string) (string, error) {
			gotOpts = opts
			connected = true
			return "instance=0,cntlid=1", nil
		},
	}
	if err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if gotOpts == "" {
		t.Error("connect was not written to the fabrics device")
	}
}

func TestConnect_IdempotentWhenAlreadyLive(t *testing.T) {
	called := false
	c := &FabricsConnector{
		poll: time.Millisecond,
		subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			return liveSub(nqn)
		}},
		connect: func(context.Context, string) (string, error) { called = true; return "", nil },
	}
	if err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "a"}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("connect wrote the fabrics device despite an already-live controller")
	}
}

func TestConnect_WriteErrorPropagates(t *testing.T) {
	c := &FabricsConnector{
		poll: time.Millisecond,
		subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return notFound() }},
		connect: func(context.Context, string) (string, error) {
			return "", errors.New("connection refused")
		},
	}
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
	c := &FabricsConnector{subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
		return nvme.Subsystem{NQN: nqn, Controllers: paths}, nil
	}}}

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
	c := &FabricsConnector{subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}}}
	if err := c.Disconnect(context.Background(), "nqn.gone"); err != nil {
		t.Errorf("Disconnect of absent subsystem = %v, want nil", err)
	}
}

func TestIsConnected(t *testing.T) {
	ctx := context.Background()
	t.Run("live", func(t *testing.T) {
		c := &FabricsConnector{subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			return liveSub(nqn)
		}}}
		if ok, err := c.IsConnected(ctx, "n"); err != nil || !ok {
			t.Errorf("IsConnected = %v, %v; want true, nil", ok, err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		c := &FabricsConnector{subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
			return notFound()
		}}}
		if ok, err := c.IsConnected(ctx, "n"); err != nil || ok {
			t.Errorf("IsConnected = %v, %v; want false, nil", ok, err)
		}
	})
	t.Run("present but not live", func(t *testing.T) {
		c := &FabricsConnector{subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
			return nvme.Subsystem{NQN: nqn, Controllers: []nvme.Controller{{State: "connecting"}}}, nil
		}}}
		if ok, err := c.IsConnected(ctx, "n"); err != nil || ok {
			t.Errorf("IsConnected = %v, %v; want false, nil", ok, err)
		}
	})
}

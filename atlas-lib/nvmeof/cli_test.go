package nvmeof

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/ptr"
)

// cliRun records the command lines a CLIConnector would run, and answers them
// from a script so no nvme-cli has to be present.
type cliRun struct {
	calls  [][]string
	out    map[string][]byte // keyed by the argument the reply belongs to
	err    map[string]error
	always error
}

func (r *cliRun) run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	key := ""
	for i, a := range args {
		if a == "-a" || a == "-d" {
			if i+1 < len(args) {
				key = args[i+1]
			}
		}
	}
	if r.always != nil {
		return r.out[key], r.always
	}
	return r.out[key], r.err[key]
}

func cliConnector(t *testing.T, run *cliRun, subs nvme.SubsystemResolver) *CLIConnector {
	t.Helper()
	c := NewCLIConnector(subs, WithPollInterval(time.Millisecond), WithPathTimeout(200*time.Millisecond))
	c.run = run.run
	// The constructor reads the node's identity; pin it so the rendering is the
	// test's and not the machine's.
	c.hostNQN, c.hostID = "", ""
	c.attach = c.connect
	return c
}

// flagValue pulls the value of a flag out of a rendered command line.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func argsOf(calls [][]string, verb string) []string {
	for _, c := range calls {
		if len(c) > 0 && c[0] == verb {
			return c
		}
	}
	return nil
}

// The two mechanisms must ask the kernel for the same thing, or a caller that
// switches over silently changes its connect parameters.
func TestCLIConnectArgs_MirrorTheFabricsOptions(t *testing.T) {
	clt, fiof := 60, 0
	tgt := Target{
		NQN: "nqn.test:vol", Address: "10.0.0.1", Port: 4438,
		HostNQN: "h-nqn", HostID: "h-id", HostIface: "eth1",
		NrIOQueues: 8, ReconnectDelaySec: 2, KeepAliveTMOSec: 5,
		CtrlLossTMOSec: &clt, FastIOFailTMOSec: &fiof, TLS: true,
	}
	c := cliConnector(t, &cliRun{}, fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}})

	args := strings.Join(c.connectArgs(tgt), " ")
	for _, want := range []string{
		"-t tcp", "-a 10.0.0.1", "-s 4438", "-n nqn.test:vol",
		"--hostnqn h-nqn", "--hostid h-id", "--host-iface eth1",
		"--nr-io-queues 8", "--reconnect-delay 2", "--keep-alive-tmo 5",
		"--ctrl-loss-tmo 60", "--fast_io_fail_tmo 0", "--tls",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("connect args missing %q\n  got: %s", want, args)
		}
	}
}

// A zero value means "leave the kernel default", exactly as the fabrics-device
// line omits it — not "send zero".
func TestCLIConnectArgs_OmitsUnsetTunables(t *testing.T) {
	c := cliConnector(t, &cliRun{}, fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}})
	args := strings.Join(c.connectArgs(Target{NQN: "n", Address: "a"}), " ")
	for _, absent := range []string{
		"--nr-io-queues", "--reconnect-delay", "--keep-alive-tmo",
		"--ctrl-loss-tmo", "--fast_io_fail_tmo", "--tls", "--host-iface",
	} {
		if strings.Contains(args, absent) {
			t.Errorf("connect args carry %q for an unset field\n  got: %s", absent, args)
		}
	}
	// Defaults still apply where the fabrics line has them.
	if !strings.Contains(args, "-t tcp") || !strings.Contains(args, "-s 4420") {
		t.Errorf("connect args = %s, want the tcp/4420 defaults", args)
	}
}

// "already connected" is the message at the heart of the bug this package
// exists for. At the controller level it is the truth — the path is there — so
// the connect has to succeed; whether that controller serves a namespace is
// Inspect's question, not this one.
func TestCLIConnect_AlreadyConnectedIsSuccess(t *testing.T) {
	run := &cliRun{
		out:    map[string][]byte{"10.0.0.1": []byte("Failed to write to /dev/nvme-fabrics: Operation already in progress\nalready connected\n")},
		err:    map[string]error{"10.0.0.1": errors.New("exit status 1")},
		always: nil,
	}
	connected := false
	c := cliConnector(t, run, fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
		if !connected {
			connected = true // the controller shows up on the next look
			return notFound()
		}
		return liveSub(nqn, "10.0.0.1")
	}})

	if err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "10.0.0.1"}); err != nil {
		t.Fatalf("Connect = %v, want success: a controller for this path exists", err)
	}
}

func TestCLIConnect_RealFailureSurfacesTheOutput(t *testing.T) {
	run := &cliRun{
		out: map[string][]byte{"10.0.0.1": []byte("failed to write to nvme-fabrics: Connection refused")},
		err: map[string]error{"10.0.0.1": errors.New("exit status 1")},
	}
	c := cliConnector(t, run, fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}})

	err := c.Connect(context.Background(), Target{NQN: "nqn.x", Address: "10.0.0.1"})
	if err == nil {
		t.Fatal("Connect = nil, want the connect failure")
	}
	if !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("err = %v, want nvme-cli's reason carried through", err)
	}
}

// `nvme disconnect -n` would take the whole subsystem, and on a shared one every
// co-tenant volume with it. Only -d releases a single path.
func TestCLIDisconnectController_UsesDeviceScopedDisconnect(t *testing.T) {
	dir := t.TempDir()
	ctrlDir := filepath.Join(dir, "nvme3")
	if err := os.MkdirAll(ctrlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := &cliRun{}
	c := cliConnector(t, run, fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}})

	err := c.DisconnectController(context.Background(),
		nvme.Controller{ID: "nvme3", SysfsPath: ctrlDir})
	if err != nil {
		t.Fatal(err)
	}
	got := argsOf(run.calls, "disconnect")
	if want := []string{"disconnect", "-d", "nvme3"}; !slices.Equal(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
	for _, c := range run.calls {
		if slices.Contains(c, "-n") {
			t.Errorf("ran a subsystem-scoped disconnect %v; it would take every co-tenant down", c)
		}
	}
}

// The caller holds a snapshot and the kernel can reap the controller in
// between, so a controller already gone is not a failed repair.
func TestCLIDisconnectController_IdempotentWhenAlreadyGone(t *testing.T) {
	run := &cliRun{}
	c := cliConnector(t, run, fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}})

	err := c.DisconnectController(context.Background(),
		nvme.Controller{ID: "nvme9", SysfsPath: filepath.Join(t.TempDir(), "gone")})
	if err != nil {
		t.Errorf("DisconnectController = %v, want nil for a controller already reaped", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("ran %v, want no command for a controller that is not there", run.calls)
	}
}

func TestControllerName(t *testing.T) {
	cases := []struct {
		ctrl nvme.Controller
		want string
	}{
		{nvme.Controller{ID: "nvme3"}, "nvme3"},
		{nvme.Controller{SysfsPath: "/sys/class/nvme/nvme7"}, "nvme7"},
		{nvme.Controller{}, ""},
	}
	for _, tc := range cases {
		if got := controllerName(tc.ctrl); got != tc.want {
			t.Errorf("controllerName(%+v) = %q, want %q", tc.ctrl, got, tc.want)
		}
	}
}

// The point of the shared base: CLIConnector gets priority order, idempotence
// and per-path isolation without reimplementing any of it.
func TestCLIConnectPaths_InheritsThePathHandling(t *testing.T) {
	var live []string
	run := &cliRun{
		out: map[string][]byte{"10.0.0.2": []byte("Connection refused")},
		err: map[string]error{"10.0.0.2": errors.New("exit status 1")},
	}
	c := cliConnector(t, run, fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
		if len(live) == 0 {
			return notFound()
		}
		s := nvme.Subsystem{NQN: nqn}
		for i, a := range live {
			s.Controllers = append(s.Controllers, ctrl("nvme"+string(rune('0'+i)), a, "live"))
		}
		return s, nil
	}})
	// A successful connect is what makes the controller appear.
	inner := c.run
	c.run = func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := inner(ctx, args...)
		if err == nil && len(args) > 0 && args[0] == "connect" {
			live = append(live, flagValue(args, "-a"))
		}
		return out, err
	}

	res, err := c.ConnectPaths(context.Background(), targets("10.0.0.1", "10.0.0.2", "10.0.0.3"))
	if err != nil {
		t.Fatalf("ConnectPaths = %v, want success: two paths came up", err)
	}

	var attempted []string
	for _, call := range run.calls {
		if call[0] == "connect" {
			attempted = append(attempted, flagValue(call, "-a"))
		}
	}
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if !slices.Equal(attempted, want) {
		t.Errorf("attach order = %v, want %v: priority order is the control plane's", attempted, want)
	}
	if res[1].Err == nil {
		t.Error("the refused path reports no reason")
	}
	if !res[0].Live || !res[2].Live {
		t.Errorf("results = %+v, want the reachable paths live despite the middle one failing", res)
	}
}

// Options are the shared ones and must reach the embedded base, or a caller
// switching connectors silently loses its timeouts.
func TestNewCLIConnector_AppliesSharedOptions(t *testing.T) {
	c := NewCLIConnector(nil, WithPathTimeout(7*time.Second), WithPollInterval(11*time.Millisecond))
	if c.pathTimeout != 7*time.Second {
		t.Errorf("pathTimeout = %v, want 7s", c.pathTimeout)
	}
	if c.pollInterval() != 11*time.Millisecond {
		t.Errorf("pollInterval = %v, want 11ms", c.pollInterval())
	}
	if c.attach == nil || c.deleteCtrl == nil {
		t.Error("the constructor left a mechanism primitive unset")
	}
}

// Nothing here should need the fabrics device; the whole point is a caller that
// cannot write it.
func TestCLIConnector_NeverRendersFabricsOptions(t *testing.T) {
	c := cliConnector(t, &cliRun{}, fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) {
		return notFound()
	}})
	args := c.connectArgs(Target{NQN: "n", Address: "a", CtrlLossTMOSec: ptr.To(1)})
	for _, a := range args {
		if strings.Contains(a, "transport=") || strings.Contains(a, "traddr=") {
			t.Errorf("arg %q is fabrics-device syntax, not an nvme-cli flag", a)
		}
	}
}

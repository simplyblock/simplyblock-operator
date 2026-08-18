package cluster

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The monitor is the only way to fault a host, and it is reached over a line
// protocol that a real QEMU speaks and this code has to speak back. Booting a
// virtual machine to find out whether the framing is right is a slow way to learn
// it, so the framing is tested against a stand-in that greets and prompts the way
// HMP does.
//
// What this cannot check is that QEMU accepts the commands. That belongs to a run
// with a real cluster; what belongs here is that a reply is read to its end, that
// a command's output is not confused with the banner or the prompt, and that a
// monitor which dies mid-command says so.

// fakeMonitor is a unix socket that answers like QEMU's human monitor.
type fakeMonitor struct {
	path string

	mu       sync.Mutex
	received []string

	// reply is what to answer with, keyed by command. A command with no entry is
	// answered with an empty line, as most HMP commands are.
	reply map[string]string

	// dieOn closes the connection instead of replying, the way `quit` does.
	dieOn string
}

func startFakeMonitor(t *testing.T, f *fakeMonitor) {
	t.Helper()

	// Not t.TempDir(): it puts the test's name in the path, and a unix socket path
	// has the same 104 bytes to live in that checkNameFits is about — the longer
	// test names here overrun it and bind fails with EINVAL.
	dir, err := os.MkdirTemp("", "sbm")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f.path = filepath.Join(dir, "m")

	l, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatalf("listen on %s: %v", f.path, err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
}

func (f *fakeMonitor) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// QEMU greets before it prompts, and the greeting must not be mistaken for a
	// command's output.
	_, _ = conn.Write([]byte("QEMU 9.1.2 monitor - type 'help' for more information\n(qemu) "))

	r := bufio.NewScanner(conn)
	for r.Scan() {
		cmd := strings.TrimSpace(r.Text())

		f.mu.Lock()
		f.received = append(f.received, cmd)
		f.mu.Unlock()

		if f.dieOn != "" && cmd == f.dieOn {
			return
		}
		if reply, ok := f.reply[cmd]; ok {
			_, _ = conn.Write([]byte(reply + "\n(qemu) "))
			continue
		}
		_, _ = conn.Write([]byte("\n(qemu) "))
	}
}

func (f *fakeMonitor) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

// dial connects to the fake as Cluster.Monitor would, reading the banner.
func dialFake(t *testing.T, f *fakeMonitor) *Monitor {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", f.path)
	if err != nil {
		t.Fatalf("dial fake monitor: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	m := &Monitor{conn: conn, node: "fake"}
	if _, err := m.read(ctx); err != nil {
		t.Fatalf("read banner: %v", err)
	}
	return m
}

func TestMonitor_ReadsTheBannerBeforeTheFirstReply(t *testing.T) {
	f := &fakeMonitor{reply: map[string]string{
		"info status": "VM status: running",
	}}
	startFakeMonitor(t, f)
	m := dialFake(t, f)

	out, err := m.Command(context.Background(), "info status")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	// The banner would be prepended to this if it had not been consumed.
	if out != "VM status: running" {
		t.Errorf("output = %q, want just the reply", out)
	}
}

func TestMonitor_SendsTheCommandAndKeepsRepliesSeparate(t *testing.T) {
	f := &fakeMonitor{reply: map[string]string{
		"stop":              "",
		"info status":       "VM status: paused",
		"set_link net0 off": "",
	}}
	startFakeMonitor(t, f)
	m := dialFake(t, f)

	ctx := context.Background()
	if _, err := m.Command(ctx, "stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	status, err := m.Command(ctx, "info status")
	if err != nil {
		t.Fatalf("info status: %v", err)
	}
	if status != "VM status: paused" {
		t.Errorf("status = %q; a reply leaked from the previous command", status)
	}
	if _, err := m.Command(ctx, "set_link net0 off"); err != nil {
		t.Fatalf("set_link: %v", err)
	}

	want := []string{"stop", "info status", "set_link net0 off"}
	if got := f.commands(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("monitor received %v, want %v", got, want)
	}
}

// HMP answers a bad command by printing an error and prompting again, so it
// arrives as output rather than as a transport failure. Reporting it as success
// would let a test that faults nothing pass.
func TestMonitor_ReportsARejectedCommand(t *testing.T) {
	f := &fakeMonitor{reply: map[string]string{
		"nonsense": "unknown command: 'nonsense'",
	}}
	startFakeMonitor(t, f)
	m := dialFake(t, f)

	if _, err := m.Command(context.Background(), "nonsense"); err == nil {
		t.Fatal("Command reported success for a command the monitor rejected")
	}
}

// `quit` makes QEMU exit, so the reply to it never comes. PowerOff treats that as
// success, and this pins which errors count as "the monitor went away" — the
// alternative being a power-off that always reports failure.
func TestMonitor_QuitLosesTheConnection(t *testing.T) {
	f := &fakeMonitor{dieOn: "quit"}
	startFakeMonitor(t, f)
	m := dialFake(t, f)

	_, err := m.Command(context.Background(), "quit")
	if err == nil {
		t.Fatal("want an error from a monitor that closed mid-command")
	}
	if !isClosed(err) {
		t.Errorf("isClosed(%v) = false; PowerOff would report this as a failure", err)
	}
}

func TestMonitorPath_IsDerivedFromTheNodeName(t *testing.T) {
	c := &Cluster{cfg: Config{Name: "sbi-1234"}}

	got := c.MonitorPath("sbi-1234-controlplane-1")
	want := filepath.Join(stateDir(), "sbi-1234", "sbi-1234-controlplane-1.monitor")
	if got != want {
		t.Errorf("MonitorPath = %q, want %q", got, want)
	}
}

func TestMonitor_AbsentSocketSaysWhereItLooked(t *testing.T) {
	c := &Cluster{cfg: Config{Name: "sbi-does-not-exist"}}

	_, err := c.Monitor(context.Background(), "sbi-does-not-exist-worker-1")
	if err == nil {
		t.Fatal("want an error for a cluster that is not running")
	}
	if !strings.Contains(err.Error(), ".monitor") {
		t.Errorf("error does not name the path it looked for: %v", err)
	}
}

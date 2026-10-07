package qmp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs/deferrers"
)

// fakeQEMU serves one QMP session on a Unix socket: the greeting, then one
// scripted reply per command, after any events queued for it.
type fakeQEMU struct {
	t        *testing.T
	path     string
	received chan map[string]any
	replies  map[string][]string // command -> lines written in answer, in order
}

func newFakeQEMU(t *testing.T) *fakeQEMU {
	t.Helper()
	// Not t.TempDir(): it embeds the test name, and a Unix socket path is
	// limited to 104 bytes on macOS and 108 on Linux.
	dir, err := os.MkdirTemp("", "qmp")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(deferrers.RunFn(func() error { return os.RemoveAll(dir) }))
	f := &fakeQEMU{
		t:        t,
		path:     filepath.Join(dir, "qmp.sock"),
		received: make(chan map[string]any, 16),
		replies: map[string][]string{
			"qmp_capabilities": {`{"return": {}}`},
		},
	}
	return f
}

func (f *fakeQEMU) serve() {
	f.t.Helper()
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		f.t.Fatalf("listen: %v", err)
	}
	f.t.Cleanup(deferrers.CloseFn(ln))
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer deferrers.Close(conn)
		w := bufio.NewWriter(conn)
		write := func(line string) {
			_, _ = w.WriteString(line + "\r\n")
			_ = w.Flush()
		}
		write(`{"QMP": {"version": {"qemu": {"major": 10, "minor": 1, "micro": 0}}, "capabilities": []}}`)
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var cmd map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
				write(`{"error": {"class": "GenericError", "desc": "bad json"}}`)
				continue
			}
			f.received <- cmd
			name, _ := cmd["execute"].(string)
			lines, ok := f.replies[name]
			if !ok {
				lines = []string{`{"error": {"class": "CommandNotFound", "desc": "The command ` + name + ` has not been found"}}`}
			}
			for _, line := range lines {
				write(line)
			}
		}
	}()
}

func dial(t *testing.T, f *fakeQEMU) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, f.path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(deferrers.CloseFn(c))
	return c
}

func next(t *testing.T, f *fakeQEMU) map[string]any {
	t.Helper()
	select {
	case cmd := <-f.received:
		return cmd
	case <-time.After(5 * time.Second):
		t.Fatal("fake QEMU received nothing")
		return nil
	}
}

// QEMU accepts no command until capabilities are negotiated, so Dial has to
// do it before handing the client out.
func TestDialNegotiatesCapabilities(t *testing.T) {
	f := newFakeQEMU(t)
	f.serve()
	dial(t, f)
	if cmd := next(t, f); cmd["execute"] != "qmp_capabilities" {
		t.Errorf("first command = %v, want qmp_capabilities", cmd)
	}
}

func TestSystemPowerdownSendsTheCommand(t *testing.T) {
	f := newFakeQEMU(t)
	f.replies["system_powerdown"] = []string{`{"return": {}}`}
	f.serve()
	c := dial(t, f)
	next(t, f) // capabilities

	if err := c.SystemPowerdown(context.Background()); err != nil {
		t.Fatalf("SystemPowerdown: %v", err)
	}
	if cmd := next(t, f); cmd["execute"] != "system_powerdown" {
		t.Errorf("command = %v, want system_powerdown", cmd)
	}
}

// Events arrive whenever QEMU has one, including between a command and its
// reply. Reading an event as the reply would report success for a command
// that has not been answered yet.
func TestEventsBeforeTheReplyAreSkipped(t *testing.T) {
	f := newFakeQEMU(t)
	f.replies["query-status"] = []string{
		`{"event": "POWERDOWN", "timestamp": {"seconds": 1, "microseconds": 0}}`,
		`{"return": {"status": "running", "running": true}}`,
	}
	f.serve()
	c := dial(t, f)

	out, err := c.Execute(context.Background(), "query-status", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var status struct{ Status string }
	if err := json.Unmarshal(out, &status); err != nil || status.Status != "running" {
		t.Errorf("result = %s, %v, want status running", out, err)
	}
}

func TestQEMUErrorIsReturned(t *testing.T) {
	f := newFakeQEMU(t)
	f.serve()
	c := dial(t, f)

	_, err := c.Execute(context.Background(), "no-such-command", nil)
	if err == nil || !strings.Contains(err.Error(), "CommandNotFound") {
		t.Errorf("err = %v, want QEMU's CommandNotFound", err)
	}
}

func TestArgumentsAreSent(t *testing.T) {
	f := newFakeQEMU(t)
	f.replies["human-monitor-command"] = []string{`{"return": ""}`}
	f.serve()
	c := dial(t, f)
	next(t, f) // capabilities

	args := map[string]string{"command-line": "info status"}
	if _, err := c.Execute(context.Background(), "human-monitor-command", args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cmd := next(t, f)
	got, _ := cmd["arguments"].(map[string]any)
	if got["command-line"] != "info status" {
		t.Errorf("arguments = %v, want %v", cmd["arguments"], args)
	}
}

func TestDialFailsWithoutQEMU(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Dial(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("Dial succeeded without a socket")
	}
}

package runner

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs/deferrers"
)

func TestCheckKVMNeedsAReadWriteDevice(t *testing.T) {
	dir := t.TempDir()
	usable := filepath.Join(dir, "kvm")
	if err := os.WriteFile(usable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckKVM(usable); err != nil {
		t.Errorf("CheckKVM(read-write) = %v, want nil", err)
	}
	if err := CheckKVM(filepath.Join(dir, "missing")); err == nil {
		t.Error("CheckKVM accepted a missing device")
	}
	if os.Geteuid() != 0 { // root opens a read-only file for writing anyway
		readOnly := filepath.Join(dir, "kvm-ro")
		if err := os.WriteFile(readOnly, nil, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := CheckKVM(readOnly); err == nil {
			t.Error("CheckKVM accepted a device it cannot open for writing")
		}
	}
}

func TestStartProcessRunsAndReportsTheExit(t *testing.T) {
	var out bytes.Buffer
	start := StartProcess("sh", []string{"-c", "echo booted; exit 3"}, &out, &out)
	proc, err := start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := proc.Wait(); err == nil {
		t.Error("Wait() = nil for exit 3")
	}
	if out.String() != "booted\n" {
		t.Errorf("output = %q, want the process's own", out.String())
	}
}

func TestStartedProcessCanBeKilled(t *testing.T) {
	proc, err := StartProcess("sleep", []string{"60"}, nil, nil)(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("killed process did not exit")
	}
}

func TestStartProcessFailsForAMissingBinary(t *testing.T) {
	if _, err := StartProcess("/nonexistent/qemu", nil, nil, nil)(context.Background()); err == nil {
		t.Error("started a binary that does not exist")
	}
}

func TestTCPProbeFollowsTheListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	probe := TCPProbe(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := probe(ctx); err != nil {
		t.Errorf("probe with a listener = %v, want nil", err)
	}
	deferrers.Close(ln)
	if err := probe(ctx); err == nil {
		t.Error("probe passed after the listener closed")
	}
}

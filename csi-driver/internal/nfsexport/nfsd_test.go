package nfsexport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/export"
)

// fakeExportAssembler is the inner delegate nfsdAssembler wraps.
type fakeExportAssembler struct {
	checked  int
	checkErr error
}

func (f *fakeExportAssembler) Create(context.Context, export.Spec) error { return nil }
func (f *fakeExportAssembler) Delete(context.Context, export.Spec) error { return nil }
func (f *fakeExportAssembler) Check(context.Context, export.Spec) error {
	f.checked++
	return f.checkErr
}

// validateThreadCount is checkNFSDThreads' decision, pulled out so it is
// testable without a real /proc/fs/nfsd, which a sandbox does not have.
func TestValidateThreadCount(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{"running", "8\n", false},
		{"zero threads", "0", true},
		{"negative reads as unhealthy, not a crash", "-1", true},
		{"not a number", "not-a-number", true},
		{"empty", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateThreadCount([]byte(c.data))
			if (err != nil) != c.wantErr {
				t.Errorf("validateThreadCount(%q) error = %v, wantErr %v", c.data, err, c.wantErr)
			}
		})
	}
}

// nfsd is checked before the export sitting on it: an export cannot be well
// served by a kernel NFS server with no threads running, whatever its own
// mount and export-table state says. A sandbox has no /proc/fs/nfsd, so this
// also pins that the failure is reported rather than panicking past it.
func TestCheckFailsClosedWithoutReachingInnerWhenNFSDsControlFileIsAbsent(t *testing.T) {
	inner := &fakeExportAssembler{}
	assembler := nfsdAssembler{inner: inner}

	err := assembler.Check(context.Background(), export.Spec{Path: "/var/lib/simplyblock/exports/x"})
	if err == nil {
		t.Fatal("Check passed with no nfsd control filesystem present")
	}
	if !strings.Contains(err.Error(), "nfsd") {
		t.Errorf("error = %q, want it to name nfsd as the cause", err.Error())
	}
	if inner.checked != 0 {
		t.Errorf("inner.Check was reached %d times; nfsd's own health should have refused first", inner.checked)
	}
}

// startFakeProcess launches a process whose kernel comm is name, by copying a
// real binary to a temp path under that name and executing it there.
// /proc/pid/comm reflects the executed file's own basename, not argv[0], so
// this is the one portable way to make a process idmapdRunning would actually
// find, without needing a real rpc.idmapd on the test machine.
func startFakeProcess(t *testing.T, name string) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary on PATH to fake a process with: %v", err)
	}
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatalf("reading %s: %v", sleep, err)
	}
	fake := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(fake, data, 0o755); err != nil {
		t.Fatalf("writing %s: %v", fake, err)
	}
	cmd := exec.Command(fake, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", fake, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// waitForProcessRunning polls processRunning(comm) until it reports want, or
// fails the test: Start() returns once the fork succeeds, not once exec() has
// replaced the image and the kernel has set comm, and killing a process is
// exactly as asynchronous from the caller's side.
func waitForProcessRunning(t *testing.T, comm string, want bool) {
	t.Helper()
	// processRunning reads /proc, which only Linux has; nfsd itself is
	// Linux-only in production, but a contributor's own machine is not.
	if runtime.GOOS != "linux" {
		t.Skipf("processRunning reads /proc, which %s does not have", runtime.GOOS)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		running, err := processRunning(comm)
		if err != nil {
			t.Fatalf("processRunning(%q): %v", comm, err)
		}
		if running == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processRunning(%q) still reports %v after 2s, want %v", comm, running, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The one signal available without a pidfile or shelling out to pgrep/ps:
// scanning /proc for a process whose comm matches.
func TestProcessRunningDetectsARealProcessByComm(t *testing.T) {
	waitForProcessRunning(t, "rpc.idmapd", false)
	startFakeProcess(t, "rpc.idmapd")
	waitForProcessRunning(t, "rpc.idmapd", true)
}

// ensureIdmapd starts rpc.idmapd exactly when processRunning says it is
// missing -- both directions matter: a start that never happens leaves the
// client hanging (§ package comment), and a start that always happens leaks
// one idmapd per reconcile, since EnsureNFSD runs before every assembly and
// idmapd itself does not refuse a second copy.
func TestEnsureIdmapdStartsItWhenNotRunning(t *testing.T) {
	waitForProcessRunning(t, "rpc.idmapd", false)

	var calls []string
	run := func(_ context.Context, name string, _ ...string) ([]byte, int, error) {
		calls = append(calls, name)
		return nil, 0, nil
	}
	if err := ensureIdmapd(context.Background(), run); err != nil {
		t.Fatalf("ensureIdmapd: %v", err)
	}
	if len(calls) != 1 || calls[0] != "rpc.idmapd" {
		t.Errorf("calls = %v, want exactly one call to rpc.idmapd", calls)
	}
}

func TestEnsureIdmapdSkipsStartingItWhenAlreadyRunning(t *testing.T) {
	startFakeProcess(t, "rpc.idmapd")
	waitForProcessRunning(t, "rpc.idmapd", true)

	run := func(_ context.Context, name string, _ ...string) ([]byte, int, error) {
		t.Fatalf("run was called with %q; ensureIdmapd should have found the existing process first", name)
		return nil, 0, nil
	}
	if err := ensureIdmapd(context.Background(), run); err != nil {
		t.Fatalf("ensureIdmapd: %v", err)
	}
}

// ensureNfsdcld mirrors ensureIdmapd's own idempotency, and separately pins
// the one thing that makes nfsdcld's absence dangerous rather than merely
// wasteful: it has to run before nfsd's threads do (EnsureNFSD's own
// ordering), which these tests cannot see from here -- that ordering is what
// § ensureNfsdcld's package comment is about, not something a unit test on
// the function alone can assert.
func TestEnsureNfsdcldStartsItWhenNotRunning(t *testing.T) {
	waitForProcessRunning(t, "nfsdcld", false)

	var calls []string
	run := func(_ context.Context, name string, _ ...string) ([]byte, int, error) {
		calls = append(calls, name)
		return nil, 0, nil
	}
	if err := ensureNfsdcld(context.Background(), run); err != nil {
		t.Fatalf("ensureNfsdcld: %v", err)
	}
	if len(calls) != 1 || calls[0] != "nfsdcld" {
		t.Errorf("calls = %v, want exactly one call to nfsdcld", calls)
	}
}

func TestEnsureNfsdcldSkipsStartingItWhenAlreadyRunning(t *testing.T) {
	startFakeProcess(t, "nfsdcld")
	waitForProcessRunning(t, "nfsdcld", true)

	run := func(_ context.Context, name string, _ ...string) ([]byte, int, error) {
		t.Fatalf("run was called with %q; ensureNfsdcld should have found the existing process first", name)
		return nil, 0, nil
	}
	if err := ensureNfsdcld(context.Background(), run); err != nil {
		t.Fatalf("ensureNfsdcld: %v", err)
	}
}

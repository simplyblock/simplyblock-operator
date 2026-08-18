package volumemigration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ctrlSpec describes one controller of the fake NVMe sysfs tree: its address, its
// kernel state, and the ANA state of the namespace legs it serves.
type ctrlSpec struct {
	addr string // "ip:port"
	name string // "nvme0"
	stat string // controller state
	ana  string // ANA state of its namespace legs ("" for no leg at all)
	// serves optionally narrows which of the subsystem's namespaces this
	// controller has a leg for; nil means all of them. A controller that serves
	// some namespaces and not others is the multi-namespace shape a connect
	// cannot see: it is live, and short a path for one volume on the subsystem.
	serves []int
}

func (c ctrlSpec) hasLeg(nsid int) bool {
	if c.serves == nil {
		return true
	}
	return slices.Contains(c.serves, nsid)
}

// writeSysfs builds the tree for pathsNQN with a single namespace; another
// subsystem is never needed, since the filtering by NQN is asserted through
// PresentAddresses instead.
func writeSysfs(t *testing.T, ctrls ...ctrlSpec) string {
	t.Helper()
	return writeSysfsNS(t, 1, ctrls...)
}

// writeSysfsNS builds the tree with nsCount namespaces (nsid 1..nsCount), each
// exposed as a subsystem-level block head with one per-controller leg — the shape
// the kernel exposes for multipath.
func writeSysfsNS(t *testing.T, nsCount int, ctrls ...ctrlSpec) string {
	nqn := pathsNQN
	t.Helper()
	root := t.TempDir()
	sub := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0")
	mustWrite := func(dir, file, val string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(val+"\n"), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", dir, file, err)
		}
	}
	mustWrite(sub, "subsysnqn", nqn)

	for i, c := range ctrls {
		host, port, _ := strings.Cut(c.addr, ":")
		ctrlDir := filepath.Join(root, "class", "nvme", c.name)
		mustWrite(ctrlDir, "subsysnqn", nqn)
		mustWrite(ctrlDir, "state", c.stat)
		mustWrite(ctrlDir, "address", "traddr="+host+",trsvcid="+port)
		mustWrite(ctrlDir, "transport", "tcp")
		// The subsystem lists its controllers as symlinks.
		if err := os.Symlink(ctrlDir, filepath.Join(sub, c.name)); err != nil {
			t.Fatalf("link controller: %v", err)
		}
		if c.ana == "" {
			continue
		}
		for nsid := 1; nsid <= nsCount; nsid++ {
			id := strconv.Itoa(nsid)
			nsDir := filepath.Join(sub, "nvme0n"+id)
			mustWrite(nsDir, "nsid", id)
			if !c.hasLeg(nsid) {
				continue
			}
			legDir := filepath.Join(ctrlDir, "nvme0c"+strconv.Itoa(i)+"n"+id)
			mustWrite(legDir, "ana_state", c.ana)
			mustWrite(legDir, "nsid", id)
		}
	}
	return root
}

const (
	pathsNQN  = "nqn.2023-02.io.simplyblock:cluster:lvol:vol-1"
	stateLive = "live"
)

var targetConns = []Connection{
	{NQN: pathsNQN, IP: "10.0.0.114", Port: 4428, Transport: "tcp"},
	{NQN: pathsNQN, IP: "10.0.0.112", Port: 4428, Transport: "tcp"},
}

func TestVerifyMigrationPaths_Ready(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	paths, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil)
	if err != nil {
		t.Fatalf("VerifyMigrationPaths: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(paths))
	}
	for _, p := range paths {
		if !p.Present || p.State != stateLive || p.Accessible() {
			t.Errorf("path %s is not established-and-parked", p)
		}
	}
}

// The case that let the corrupting migration through: the connect is refused by the
// target (the subsystem is not there yet), so no controller for that address exists.
// The old check could be satisfied by an unrelated path; this one must not be.
func TestVerifyMigrationPaths_ConnectDidNotTakeEffect(t *testing.T) {
	root := writeSysfs(t,
		// Only the source path exists; the target address never came up.
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	_, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil)
	if err == nil {
		t.Fatalf("expected an error when the target path is absent")
	}
	if !strings.Contains(err.Error(), "10.0.0.114:4428") ||
		!strings.Contains(err.Error(), "did not take effect") {
		t.Errorf("error = %q, want it to name the missing target path", err)
	}
}

// A controller stuck in "connecting" is what the kernel shows while the target keeps
// refusing the admin queue. It carries no I/O, so it is not a validated path.
func TestVerifyMigrationPaths_ControllerNotLive(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", "connecting", "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	_, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil)
	if err == nil {
		t.Fatalf("expected an error for a controller that is not live")
	}
	if !strings.Contains(err.Error(), "connecting") {
		t.Errorf("error = %q, want it to report the controller state", err)
	}
}

// Pre-cutover the target must not be serving. An optimized path here means reads can
// already land on a target that may not hold the data yet — the corruption signature.
func TestVerifyMigrationPaths_TargetAlreadyServing(t *testing.T) {
	for _, ana := range []string{"optimized", "non-optimized"} {
		t.Run(ana, func(t *testing.T) {
			root := writeSysfs(t,
				ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, ana, nil},
				ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
			)
			_, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil)
			if err == nil {
				t.Fatalf("expected an error for an accessible target path before cutover")
			}
			if !strings.Contains(err.Error(), "accessible before cutover") {
				t.Errorf("error = %q, want it to flag the premature ANA state", err)
			}
		})
	}
}

// A live controller with no namespace leg has nothing to take over at cutover.
// Naming it is atlas's job — the defect is that the controller is live and serves
// no path to the namespace, which is invisible to the connect that created it.
func TestVerifyMigrationPaths_NoNamespacePath(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	_, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil)
	if err == nil {
		t.Fatalf("expected an error for a controller without a namespace path")
	}
	if !strings.Contains(err.Error(), "controller-not-contributing") ||
		!strings.Contains(err.Error(), "10.0.0.114:4428") {
		t.Errorf("error = %q, want it to name the non-contributing path", err)
	}
}

// A subsystem whose controllers are live but which exports nothing at all: the
// connect is satisfied, no block device will ever appear, and every retry
// short-circuits. It is the defect that has no per-address symptom — nothing is
// missing at the address level — so only the subsystem-level diagnosis names it.
func TestVerifyMigrationPaths_SubsystemExportsNoNamespace(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "", nil},
	)
	_, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil)
	if err == nil {
		t.Fatalf("expected an error for a subsystem exporting no namespace")
	}
	if !strings.Contains(err.Error(), "no-namespace") {
		t.Errorf("error = %q, want it to name the empty subsystem", err)
	}
}

// The multi-namespace case this package exists for: the target path is live and
// carries the subsystem's first namespace, but not its second. Every per-address
// check passes — the controller is present, live and parked — while one volume on
// the shared subsystem would come out of the cutover a path short.
//
// Diagnosing this is what asking per namespace buys. A diagnosis keyed on the NQN
// alone cannot make the statement at all: several namespaces match, so there is no
// "the" namespace a controller can be said to be missing from.
func TestVerifyMigrationPaths_ControllerMissesOneNamespace(t *testing.T) {
	root := writeSysfsNS(t, 2,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", []int{1}},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	paths, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil)
	if err == nil {
		t.Fatalf("expected an error for a path missing one of the namespaces")
	}
	// The per-address view is clean, which is exactly why the subsystem-level
	// diagnosis has to be the one that catches it.
	for _, p := range paths {
		if !p.Present || p.State != stateLive || p.Accessible() {
			t.Fatalf("path %s is not established-and-parked; the fixture is wrong", p)
		}
	}
	if !strings.Contains(err.Error(), "controller-not-contributing") ||
		!strings.Contains(err.Error(), "namespace 2") {
		t.Errorf("error = %q, want it to name the namespace the path is short of", err)
	}
}

// A subsystem serving several namespaces over every expected path is ready: the
// per-namespace diagnosis must not turn a healthy shared subsystem into a failure.
func TestVerifyMigrationPaths_MultiNamespaceReady(t *testing.T) {
	root := writeSysfsNS(t, 3,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	if _, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, nil); err != nil {
		t.Fatalf("VerifyMigrationPaths on a healthy 3-namespace subsystem: %v", err)
	}
}

// Whether a path was already there is reported, so the operator log shows what our
// connect actually contributed on an HA cluster where the target may already listen.
func TestVerifyMigrationPaths_ReportsPreExisting(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", stateLive, "inaccessible", nil},
	)
	pre := map[string]bool{"10.0.0.112:4428": true}
	paths, err := VerifyMigrationPaths(context.Background(), root, pathsNQN, targetConns, pre)
	if err != nil {
		t.Fatalf("VerifyMigrationPaths: %v", err)
	}
	for _, p := range paths {
		want := p.Address == "10.0.0.112:4428"
		if p.PreExisting != want {
			t.Errorf("path %s: PreExisting = %v, want %v", p.Address, p.PreExisting, want)
		}
		if !strings.Contains(p.String(), "pre-existing") && p.PreExisting {
			t.Errorf("path %s does not render its origin", p)
		}
	}
}

func TestPresentAddresses(t *testing.T) {
	root := writeSysfs(t,
		ctrlSpec{"10.0.0.114:4428", "nvme0", stateLive, "inaccessible", nil},
		ctrlSpec{"10.0.0.112:4428", "nvme1", "connecting", "inaccessible", nil},
	)
	got, err := PresentAddresses(context.Background(), root, pathsNQN)
	if err != nil {
		t.Fatalf("PresentAddresses: %v", err)
	}
	// Both count as present: presence is about "was this address here before", not
	// about health.
	for _, addr := range []string{"10.0.0.114:4428", "10.0.0.112:4428"} {
		if !got[addr] {
			t.Errorf("address %s not reported as present", addr)
		}
	}
	// Another subsystem's controllers must not leak in.
	other, err := PresentAddresses(context.Background(), root, "nqn.other:lvol:x")
	if err != nil {
		t.Fatalf("PresentAddresses (other): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("addresses for another subsystem = %v, want none", other)
	}
}

func TestVerifyMigrationPaths_EmptyNQN(t *testing.T) {
	if _, err := VerifyMigrationPaths(context.Background(), t.TempDir(), "", targetConns, nil); err == nil {
		t.Errorf("expected an error for an empty NQN")
	}
}

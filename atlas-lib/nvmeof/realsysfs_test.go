package nvmeof

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs/deferrers"
	"github.com/simplyblock/atlas/nvme"
)

// These tests run the real sysfs resolver and Inspect over trees captured from a
// live cluster, replayed from the snapshots in testdata/sysfs.
//
// They exist because the expensive failure mode of this package is not missing a
// defect — it is inventing one. Every repair tears down a live data path, so a
// diagnosis that fires on a healthy volume is worse than no diagnosis at all.
// Hand-built fixtures cannot rule that out: they encode what we believe sysfs
// looks like, which is exactly the belief under test. Only real kernel output
// can, which is why these snapshots are worth carrying.
//
// The defect snapshots were produced with two nvmet targets on two nodes — see
// hack/nvmet/nvmet-lab.sh — and reproducing them by hand costs a
// cluster and a kernel. Replaying them costs nothing, so a state that took real
// hardware to reach is asserted on every ordinary `go test` run from here on.
//
// The snapshots are sanitized (capture-sysfs.sh sanitize): UUIDs and addresses
// are stand-ins, substituted consistently so the relationships that matter
// survive — the model still equals the master lvol UUID, which still appears in
// the NQN and in namespace 1's `uuid`.
//
// They also pin the identity the fabric actually presents:
//
//	model   the master lvol UUID, space-padded to 40 bytes
//	serial  "ha"
//	legs    /sys/class/nvme/<ctrl>/nvme<subsys>c<ctrl>n<nsid>/ana_state
//	cntlid  1..N on the primary storage node, 1000+ on the secondary
//
// Capture a fresh one with hack/nvmet/capture-sysfs.sh.

// loadSysfs rebuilds a sysfs tree from a snapshot under a temp directory and
// returns its root. The snapshot is "absolute path<TAB>value" per attribute,
// which is all the resolver reads: sysfs has no directory content of its own
// beyond the attribute files and the symlinks, and the resolver walks the
// class directories rather than following links out of them.
func loadSysfs(t *testing.T, name string) string {
	t.Helper()

	f, err := os.Open(filepath.Join("testdata", "sysfs", name+".tsv"))
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer deferrers.Close(f)

	root := t.TempDir()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for scanner.Scan() {
		path, value, _ := strings.Cut(scanner.Text(), "\t")
		if !strings.HasPrefix(path, "/sys/") {
			continue
		}
		dest := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/sys/")))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", dest, err)
		}
		if err := os.WriteFile(dest, []byte(value+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", dest, err)
		}
		n++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if n == 0 {
		t.Fatalf("snapshot %s held no attributes", name)
	}
	return root
}

func resolvers(root string) (nvme.SubsystemResolver, nvme.DeviceResolver) {
	cfg := nvme.SysfsConfig{SysRoot: root}
	return nvme.NewSysfsSubsystemResolver(cfg), nvme.NewSysfsDeviceResolver(cfg)
}

// TestSysfsSnapshot_HealthyClusterHasNoDefects is the false-positive guard. The
// snapshot is a production node carrying four subsystems and eight namespaces —
// three plain volumes and one five-namespace shared subsystem — every path live
// on two controllers. Not one of them may be diagnosed as anything.
func TestSysfsSnapshot_HealthyClusterHasNoDefects(t *testing.T) {
	subs, devs := resolvers(loadSysfs(t, "healthy"))
	ctx := context.Background()

	all, err := subs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no subsystems in the snapshot")
	}

	namespaces := 0
	for _, s := range all {
		for _, ns := range s.Namespaces {
			namespaces++
			// A dropped ANA leg would read as "this controller contributes no
			// path" — the exact false positive worth guarding, and one a
			// resolver bug would produce silently.
			if got, want := len(ns.Paths), len(s.Controllers); got != want {
				t.Errorf("%s namespace %s: %d ANA leg(s), want one per controller (%d)",
					s.ID, ns.Name, got, want)
			}
			if ns.UUID == "" {
				t.Errorf("%s namespace %s has no UUID; the guardian maps devices to lvols by it",
					s.ID, ns.Name)
			}

			defects, err := Inspect(ctx, subs, devs, nvme.DeviceSelector{NQN: s.NQN, NSID: ns.ID}, nil)
			if err != nil {
				t.Errorf("Inspect(%s nsid %d): %v", s.ID, ns.ID, err)
				continue
			}
			for _, d := range defects {
				t.Errorf("Inspect(%s nsid %d) reported a defect on a healthy cluster: %s", s.ID, ns.ID, d)
			}
		}
	}
	if namespaces == 0 {
		t.Fatal("snapshot held no namespaces")
	}
	t.Logf("%d subsystem(s), %d namespace(s), no defects", len(all), namespaces)
}

// TestSysfsSnapshot_ForcedDefects replays the three states nvmet can force and
// asserts both the verdict and its blast radius. The blast radius is the half
// that decides whether a repair may run unattended, so asserting the kind alone
// would leave the consequential part untested.
func TestSysfsSnapshot_ForcedDefects(t *testing.T) {
	// Both storage nodes are published, as for a real multipath volume. Passing
	// only the primary would reclassify the orphaned controller as a stale
	// endpoint — correct, but a different verdict with a different remedy.
	targetsFor := func(nqn string) []Target {
		return []Target{
			{NQN: nqn, Transport: TransportTCP, Address: "127.0.0.1", Port: 14420},
			{NQN: nqn, Transport: TransportTCP, Address: "10.0.0.1", Port: 14420},
		}
	}

	tests := []struct {
		snapshot   string
		nsid       nvme.NamespaceID
		wantKind   DefectKind
		wantScope  Scope
		wantVictim int // co-tenant namespaces a repair would strand
		why        string
	}{{
		snapshot:   "controller-not-contributing",
		nsid:       1,
		wantKind:   DefectControllerNotContributing,
		wantScope:  ScopeController,
		wantVictim: 0,
		why:        "the other controller still serves every namespace, so one leg can go",
	}, {
		snapshot:   "namespace-missing",
		nsid:       1,
		wantKind:   DefectNamespaceMissing,
		wantScope:  ScopeSubsystem,
		wantVictim: 1,
		why:        "the surviving namespace is another volume, and a teardown would take it down",
	}, {
		snapshot:   "no-namespace",
		nsid:       1,
		wantKind:   DefectNoNamespace,
		wantScope:  ScopeSubsystem,
		wantVictim: 0,
		why:        "nothing is exported, so no co-tenant block device exists to lose",
	}}

	for _, tc := range tests {
		t.Run(tc.snapshot, func(t *testing.T) {
			subs, devs := resolvers(loadSysfs(t, tc.snapshot))
			ctx := context.Background()

			all, err := subs.List(ctx)
			if err != nil || len(all) != 1 {
				t.Fatalf("List = %d subsystem(s), %v; want exactly the lab subsystem", len(all), err)
			}
			s := all[0]
			if len(s.Controllers) != 2 {
				t.Fatalf("snapshot has %d controller(s), want the two-target setup", len(s.Controllers))
			}

			defects, err := Inspect(ctx, subs, devs,
				nvme.DeviceSelector{NQN: s.NQN, NSID: tc.nsid}, targetsFor(s.NQN))
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}

			var got *Defect
			for i := range defects {
				if defects[i].Kind == tc.wantKind {
					got = &defects[i]
				}
			}
			if got == nil {
				t.Fatalf("defects = %v, want one of kind %s", kinds(defects), tc.wantKind)
			}
			if got.Scope != tc.wantScope {
				t.Errorf("scope = %s, want %s", got.Scope, tc.wantScope)
			}
			if !got.Repairable() {
				t.Error("defect is not repairable; nothing names the controllers to tear down")
			}
			if len(got.CoTenants) != tc.wantVictim {
				t.Errorf("co-tenants = %d %v, want %d — %s",
					len(got.CoTenants), got.CoTenants, tc.wantVictim, tc.why)
			}
			if got.Disruptive() != (tc.wantVictim > 0) {
				t.Errorf("Disruptive() = %v, want %v: it is what gates an unattended repair",
					got.Disruptive(), tc.wantVictim > 0)
			}
			t.Logf("%s", got)
		})
	}
}

// TestLiveSysfs points the same checks at a tree captured right now, for
// diagnosing a node by hand:
//
//	capture-sysfs.sh dump > snap.tsv && capture-sysfs.sh reconstruct snap.tsv ./sysroot
//	ATLAS_SYSROOT=$PWD/sysroot go test ./nvmeof/ -run TestLiveSysfs -v
func TestLiveSysfs(t *testing.T) {
	root := os.Getenv("ATLAS_SYSROOT")
	if root == "" {
		t.Skip("ATLAS_SYSROOT not set; this is the ad-hoc form, the snapshot tests cover CI")
	}
	subs, devs := resolvers(root)
	ctx := context.Background()

	all, err := subs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range all {
		t.Logf("subsystem %s model=%q serial=%q controllers=%d namespaces=%d",
			s.ID, s.Model, s.Serial, len(s.Controllers), len(s.Namespaces))
		for _, ctrl := range s.Controllers {
			t.Logf("  ctrl %s cntlid=%d state=%s addr=%s:%s",
				ctrl.ID, ctrl.CntlID, ctrl.State, ctrl.Address.TrAddr, ctrl.Address.TrSvcID)
		}
		for _, ns := range s.Namespaces {
			defects, err := Inspect(ctx, subs, devs, nvme.DeviceSelector{NQN: s.NQN, NSID: ns.ID}, nil)
			if err != nil {
				t.Errorf("  Inspect(nsid %d): %v", ns.ID, err)
				continue
			}
			if len(defects) == 0 {
				t.Logf("  nsid %d (%s): healthy", ns.ID, ns.UUID)
			}
			for _, d := range defects {
				t.Logf("  nsid %d DEFECT: %s", ns.ID, d)
			}
		}
	}
}

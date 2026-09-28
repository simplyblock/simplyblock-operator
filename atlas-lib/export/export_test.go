package export

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/volstack"
)

// stubLayer is a layer that does nothing, so a plan can be built and compared
// without a device under it.
type stubLayer struct{ name string }

func (s stubLayer) Name() string { return s.name }
func (s stubLayer) Observe(context.Context, volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	return volstack.StateReady, volstack.Artifact{}, nil
}
func (s stubLayer) Ensure(context.Context, volstack.Artifact) (volstack.Artifact, error) {
	return volstack.Artifact{}, nil
}
func (s stubLayer) Release(context.Context, volstack.Artifact) error { return nil }
func (s stubLayer) Destroy(context.Context, volstack.Artifact) error { return nil }

// fakeRunner records which verb an assembly chose, against which handle.
type fakeRunner struct {
	calls   []string
	handles []string
	plans   []volstack.Plan
	upErr   error
	growErr error
	downErr error
}

func (f *fakeRunner) Up(_ context.Context, handle string, plan volstack.Plan) (volstack.Artifact, error) {
	f.record("Up", handle, plan)
	return volstack.Artifact{Path: "/mnt"}, f.upErr
}

func (f *fakeRunner) Grow(_ context.Context, plan volstack.Plan) error {
	f.record("Grow", "", plan)
	return f.growErr
}

func (f *fakeRunner) Down(_ context.Context, handle string, plan volstack.Plan) error {
	f.record("Down", handle, plan)
	return f.downErr
}

func (f *fakeRunner) record(verb, handle string, plan volstack.Plan) {
	f.calls = append(f.calls, verb)
	f.handles = append(f.handles, handle)
	f.plans = append(f.plans, plan)
}

// harness is one assembler over a temporary exports directory.
type harness struct {
	t          *testing.T
	assembler  *Assembler
	runner     *fakeRunner
	exportsDir string
	commands   []string
	exportfs   error
	exportCode int
	planned    int
	planErr    error

	// Check's two commands, controlled separately from exportfs -ra above:
	// mountpointCode 0 is mounted, and exportfsVOut is nfsd's own export
	// table, which Check greps for the path rather than trusting the drop-in
	// file it wrote.
	mountpointCode int
	exportfsVOut   []byte
	exportfsVCode  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, runner: &fakeRunner{}, exportsDir: t.TempDir()}

	assembler, err := New(Config{
		Plan: func(context.Context, Spec) (volstack.Plan, error) {
			h.planned++
			if h.planErr != nil {
				return nil, h.planErr
			}
			return volstack.Plan{stubLayer{name: "fabric"}, stubLayer{name: "filesystem"}}, nil
		},
		Stack: h.runner,
		Run: func(_ context.Context, name string, args ...string) ([]byte, int, error) {
			h.commands = append(h.commands, strings.Join(append([]string{name}, args...), " "))
			// Into the same log as the stack verbs, because the order between
			// the two is what these tests are about.
			h.runner.calls = append(h.runner.calls, name)
			switch {
			case name == "mountpoint":
				return nil, h.mountpointCode, nil
			case name == "exportfs" && len(args) > 0 && args[0] == "-v":
				return h.exportfsVOut, h.exportfsVCode, nil
			default:
				return nil, h.exportCode, h.exportfs
			}
		},
		ExportsDir: h.exportsDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.assembler = assembler
	return h
}

func (h *harness) spec() Spec {
	return Spec{
		VolumeUUID: "vol-1",
		ClusterID:  "cluster-1",
		PoolID:     "pool-1",
		Path:       filepath.Join(h.t.TempDir(), "export"),
		FSID:       "fsid-1",
		Clients:    []string{"10.0.0.0/24"},
	}
}

func (h *harness) dropIn(spec Spec) string {
	h.t.Helper()
	content, err := os.ReadFile(filepath.Join(h.exportsDir, spec.dropInName()))
	if err != nil {
		return ""
	}
	return string(content)
}

func TestCreateBringsTheStackUpBeforePublishing(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()

	if err := h.assembler.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Published last, which is what makes a half-assembled export invisible to
	// clients rather than briefly broken for them.
	if got := strings.Join(h.runner.calls, ","); got != "Up,Grow,exportfs" {
		t.Errorf("order = %q, want Up,Grow,exportfs", got)
	}
	if h.dropIn(spec) == "" {
		t.Error("the export was not published")
	}
	if len(h.commands) != 1 || h.commands[0] != "exportfs -ra" {
		t.Errorf("commands = %v, want one exportfs -ra", h.commands)
	}
}

func TestCreateKeysTheRecordByAPrefixedHandle(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()

	if err := h.assembler.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A volume handle is what the node plugin records under, in the same
	// directory. An export keyed by the same string would overwrite it.
	if h.runner.handles[0] == spec.VolumeUUID || h.runner.handles[0] == spec.FSID {
		t.Errorf("handle = %q, which a staged volume could also be keyed by", h.runner.handles[0])
	}
	if want := "pnfs-" + spec.FSID; h.runner.handles[0] != want {
		t.Errorf("handle = %q, want %q", h.runner.handles[0], want)
	}
}

func TestCreateGrowsOntoTheSamePlanItBroughtUp(t *testing.T) {
	h := newHarness(t)

	if err := h.assembler.Create(context.Background(), h.spec()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The control plane can enlarge a volume under a live export, and nothing
	// else on this host ever grows its filesystem.
	if len(h.runner.plans) != 2 {
		t.Fatalf("stack calls = %d, want 2", len(h.runner.plans))
	}
	if h.runner.plans[0].Names()[1] != h.runner.plans[1].Names()[1] {
		t.Errorf("grew a different plan from the one brought up: %v then %v",
			h.runner.plans[0].Names(), h.runner.plans[1].Names())
	}
	if h.planned != 1 {
		t.Errorf("planned %d times, want 1: one resolution per assembly", h.planned)
	}
}

func TestCreateDoesNotPublishAStackThatWouldNotComeUp(t *testing.T) {
	h := newHarness(t)
	h.runner.upErr = errors.New("no device")
	spec := h.spec()

	if err := h.assembler.Create(context.Background(), spec); err == nil {
		t.Fatal("Create succeeded over a stack that did not come up")
	}
	if h.dropIn(spec) != "" {
		t.Error("an export nothing is mounted behind was published")
	}
	if len(h.commands) != 0 {
		t.Errorf("commands = %v, want none", h.commands)
	}
}

func TestCreateDoesNotPublishWhatItCouldNotGrow(t *testing.T) {
	h := newHarness(t)
	h.runner.growErr = errors.New("xfs_growfs: not mounted")
	spec := h.spec()

	if err := h.assembler.Create(context.Background(), spec); err == nil {
		t.Fatal("Create succeeded over a failed grow")
	}
	if h.dropIn(spec) != "" {
		t.Error("the export was published anyway")
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()

	for range 2 {
		if err := h.assembler.Create(context.Background(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	if got := strings.Join(h.runner.calls, ","); got != "Up,Grow,exportfs,Up,Grow,exportfs" {
		t.Errorf("order = %q: a reconciler runs this on every pass", got)
	}
	if entries, err := os.ReadDir(h.exportsDir); err != nil || len(entries) != 1 {
		t.Errorf("exports directory holds %v (err %v), want one entry", entries, err)
	}
}

func TestCreateRefusesAnEmptyClientSet(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()
	spec.Clients = nil

	err := h.assembler.Create(context.Background(), spec)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Create error = %v, want ErrInvalidSpec: the widening is to everyone", err)
	}
	if len(h.runner.calls) != 0 {
		t.Errorf("stack verbs = %v, want none", h.runner.calls)
	}
}

func TestCreateReportsAPlanItCannotResolve(t *testing.T) {
	h := newHarness(t)
	h.planErr = errors.New("the control plane did not answer")

	err := h.assembler.Create(context.Background(), h.spec())
	if err == nil || !strings.Contains(err.Error(), "the control plane did not answer") {
		t.Fatalf("Create error = %v, want the planner's own reason", err)
	}
}

func TestDeleteUnpublishesBeforeTakingTheStackDown(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()
	if err := h.assembler.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	h.runner.calls, h.commands = nil, nil

	if err := h.assembler.Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// exportfs runs while the filesystem is still mounted: no new client may
	// arrive while it is being taken away underneath. Down and never Destroy,
	// because the volume is the control plane's.
	if got := strings.Join(h.runner.calls, ","); got != "exportfs,Down" {
		t.Errorf("order = %q, want exportfs,Down", got)
	}
	if h.dropIn(spec) != "" {
		t.Error("the drop-in survived the delete")
	}
}

func TestDeleteToleratesAnAlreadyGoneExport(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()

	// Never created: a finalizer has to converge on a host that never assembled
	// this export, or on one that already tore it down.
	if err := h.assembler.Delete(context.Background(), spec); err != nil {
		t.Fatalf("Delete on an absent export: %v", err)
	}
	if got := strings.Join(h.runner.calls, ","); got != "exportfs,Down" {
		t.Errorf("order = %q, want exportfs,Down", got)
	}
}

func TestDeleteRefusesASpecThatNamesNoExport(t *testing.T) {
	h := newHarness(t)

	err := h.assembler.Delete(context.Background(), Spec{Path: "/var/lib/simplyblock/exports/x"})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Delete error = %v, want ErrInvalidSpec", err)
	}
}

// Check passes when the filesystem is mounted and nfsd's own table -- not the
// drop-in file this package wrote -- carries the path.
func TestCheckPassesWhenMountedAndPublished(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()
	h.exportfsVOut = []byte(spec.Path + " *(rw)\n")

	if err := h.assembler.Check(context.Background(), spec); err != nil {
		t.Errorf("Check: %v", err)
	}
}

// Regression shape: a crash or a hand unmount leaves the drop-in file
// claiming an export that reads as an empty directory to every client.
func TestCheckFailsWhenTheFilesystemIsNotMounted(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()
	h.exportfsVOut = []byte(spec.Path + " *(rw)\n")
	h.mountpointCode = 1

	if err := h.assembler.Check(context.Background(), spec); err == nil {
		t.Fatal("Check passed over a filesystem that mountpoint says is not mounted")
	}
}

// The drop-in file is what this package wrote; nfsd's export table is what a
// client's mount actually depends on, and the two can disagree if nfsd never
// read the file.
func TestCheckFailsWhenNFSDsTableDoesNotCarryTheExport(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()
	h.exportfsVOut = []byte("/var/lib/simplyblock/exports/somebody-else *(rw)\n")

	err := h.assembler.Check(context.Background(), spec)
	if err == nil {
		t.Fatal("Check passed over an export missing from nfsd's table")
	}
	if !strings.Contains(err.Error(), "not in nfsd's export table") {
		t.Errorf("error = %q, want it to name the actual cause", err)
	}
}

func TestExportLineCarriesPNFS(t *testing.T) {
	h := newHarness(t)
	spec := h.spec()
	spec.Clients = []string{"10.0.0.1", "10.0.0.2"}

	if err := h.assembler.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	line := h.dropIn(spec)
	for _, want := range []string{"pnfs", "sync", "fsid=fsid-1", "10.0.0.1(", "10.0.0.2("} {
		if !strings.Contains(line, want) {
			t.Errorf("export line %q is missing %q", line, want)
		}
	}
}

func TestNewRefusesAConfigItCannotWorkWith(t *testing.T) {
	full := Config{
		Plan:       func(context.Context, Spec) (volstack.Plan, error) { return nil, nil },
		Stack:      &fakeRunner{},
		Run:        func(context.Context, string, ...string) ([]byte, int, error) { return nil, 0, nil },
		ExportsDir: "/etc/exports.d",
	}

	for name, strip := range map[string]func(*Config){
		"no planner":     func(c *Config) { c.Plan = nil },
		"no runner":      func(c *Config) { c.Stack = nil },
		"no command run": func(c *Config) { c.Run = nil },
		"no exports dir": func(c *Config) { c.ExportsDir = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := full
			strip(&cfg)
			if _, err := New(cfg); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("New error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

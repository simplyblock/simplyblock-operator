//go:build linux

// The volume stack against a real kernel.
//
// The unit tests in atlas prove the layers decide correctly given what a fake
// told them. What they cannot prove is that the fakes tell the truth: whether a
// mount really is gone after a release, whether a volume group really comes back
// on a second bring-up rather than being made again, and whether the reading a
// device gives through O_DIRECT is the reading the catalog was written against.
// This suite runs on the node so that nothing is substituted for the kernel.
//
// It is driven by the host-side suite, which builds this binary, publishes the
// nvmet namespaces it names, and passes them in. Absent that, every test skips:
// a developer running the module's tests is not on a node and has no fabric.

package onnode

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// stackTimeout bounds one bring-up. A fabric connect waits for a namespace to
// appear, and an mkfs on a slow loop-backed target is not instant, but neither
// is minutes, so a hang fails rather than stalling the run.
const stackTimeout = 5 * time.Minute

// harness is what the host-side driver published for this run.
type harness struct {
	t       *testing.T
	node    *node
	targets []Target
	volume  Volume
	records string
}

// newHarness reads the run's parameters, and skips when they are absent.
func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("SB_ONNODE") == "" {
		t.Skip("not running on a node: SB_ONNODE is unset, and this suite needs a real fabric")
	}

	targets := []Target{readTarget(t, "SB_TARGET")}
	if os.Getenv("SB_TARGET2_NQN") != "" {
		targets = append(targets, readTarget(t, "SB_TARGET2"))
	}

	records := t.TempDir()
	uuid := envOr("SB_VOLUME_UUID", "00000000-0000-0000-0000-000000000000")
	return &harness{
		t:       t,
		node:    newNode(envOr("SB_HOST_NQN", "nqn.2014-08.org.nvmexpress:uuid:"+uuid), uuid),
		targets: targets,
		volume: Volume{
			UUID:        uuid,
			StagingPath: filepath.Join(t.TempDir(), "staging"),
			FsType:      envOr("SB_FSTYPE", "ext4"),
		},
		records: records,
	}
}

func readTarget(t *testing.T, prefix string) Target {
	t.Helper()
	port, err := strconv.Atoi(envOr(prefix+"_PORT", "4420"))
	if err != nil {
		t.Fatalf("%s_PORT: %v", prefix, err)
	}
	nsid, err := strconv.ParseUint(envOr(prefix+"_NSID", "1"), 10, 32)
	if err != nil {
		t.Fatalf("%s_NSID: %v", prefix, err)
	}
	nqn := os.Getenv(prefix + "_NQN")
	if nqn == "" {
		t.Fatalf("%s_NQN is unset, and there is no namespace to bring up without it", prefix)
	}
	return Target{NQN: nqn, Address: envOr(prefix+"_ADDR", "127.0.0.1"), Port: port, NSID: uint32(nsid)}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runner is the runner under test, recording to this run's own directory.
func (h *harness) runner() *volstack.Runner { return volstack.NewRunner(volstack.NewStore(h.records)) }

// handle names this volume's stack in the record.
func (h *harness) handle() string { return h.volume.UUID }

// up brings the plan up and fails the test if it could not.
func (h *harness) up(ctx context.Context, plan volstack.Plan) volstack.Artifact {
	h.t.Helper()
	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		h.t.Fatalf("bring the stack up: %v", err)
	}
	return art
}

// down releases the plan, which is the only verb an unstage calls.
func (h *harness) down(ctx context.Context, plan volstack.Plan) {
	h.t.Helper()
	if err := h.runner().Down(ctx, h.handle(), plan); err != nil {
		h.t.Fatalf("bring the stack down: %v", err)
	}
}

// A raw block volume is the plain plan with its top layer absent, so what it
// exposes is a device and never a path. A plan that grew a mount here would be
// one that had put the filesystem back as a conditional.
func TestRawBlockExposesADeviceAndNoPath(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	plan := h.node.RawBlock(h.targets[0])
	art := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	if len(art.Devices) != 1 {
		t.Fatalf("the fabric exposed %d devices, want the volume's one", len(art.Devices))
	}
	if art.Path != "" {
		t.Errorf("a raw block stack mounted %s, and nothing in its plan mounts", art.Path)
	}
	if _, err := os.Stat(art.Devices[0].Path); err != nil {
		t.Errorf("the device the stack reported does not exist: %v", err)
	}
}

// The plain plan is what NodeStageVolume performs today, and bringing it up
// twice is what kubelet does. The second Up must find the stack and change
// nothing, which for this plan means it must not reformat.
func TestPlainStackIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	plan := h.node.Plain(h.targets[0], h.volume)
	first := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	if first.Path != h.volume.StagingPath {
		t.Fatalf("mounted at %q, want %q", first.Path, h.volume.StagingPath)
	}

	// A file written between the two bring-ups is what a reformat would take
	// away, and it is a stronger assertion than counting mkfs calls: it fails
	// for any reason the data went missing, not only the one anticipated.
	marker := filepath.Join(first.Path, "written-between-bring-ups")
	if err := os.WriteFile(marker, []byte("survive"), 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}

	second := h.up(ctx, plan)
	if second.Path != first.Path {
		t.Errorf("the second bring-up mounted %q, want the first's %q", second.Path, first.Path)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the second bring-up did not preserve what the first staged: %v", err)
	}
}

// Release keeps the data. NodeUnstageVolume fires on an ordinary pod restart, so
// a stack that came down and went back up has to come back with what was on it.
func TestPlainStackSurvivesARestage(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	plan := h.node.Plain(h.targets[0], h.volume)
	art := h.up(ctx, plan)

	marker := filepath.Join(art.Path, "survives-an-unstage")
	want := []byte("the volume's data")
	if err := os.WriteFile(marker, want, 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}

	h.down(ctx, plan)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the staging path still answers after a release, so nothing was unmounted")
	}

	again := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	got, err := os.ReadFile(filepath.Join(again.Path, "survives-an-unstage")) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatalf("the restaged volume lost its data: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the restaged volume reads %q, want %q", got, want)
	}
}

// The LVM plan is where reactivation and creation are told apart, and the whole
// risk of the volume layer is in getting that wrong: a volume group read as
// absent is one a bring-up creates over.
func TestLVMStackReactivatesRatherThanRecreating(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	plan := h.node.LVM(h.targets[0], h.volume, lvm.LogicalVolumeDefinition{}, "")
	art := h.up(ctx, plan)

	marker := filepath.Join(art.Path, "under-the-volume-group")
	want := []byte("not recreated")
	if err := os.WriteFile(marker, want, 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}

	h.down(ctx, plan)

	again := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	got, err := os.ReadFile(filepath.Join(again.Path, "under-the-volume-group")) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatalf("the volume group was recreated rather than reactivated: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the reactivated volume reads %q, want %q", got, want)
	}
}

// The striped plan is the only one whose bottom layer is a composite, and the
// order its members are assembled in is what the record has to preserve: a
// stripe over the same members in another order is another device.
func TestStripedStackAssemblesItsMembers(t *testing.T) {
	h := newHarness(t)
	if len(h.targets) < 2 {
		t.Skip("striping needs a second namespace, and SB_TARGET2_NQN is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	definition := lvm.LogicalVolumeDefinition{}
	plan := h.node.Striped(h.targets, h.volume, definition)
	art := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	if art.Path != h.volume.StagingPath {
		t.Fatalf("mounted at %q, want %q", art.Path, h.volume.StagingPath)
	}
	marker := filepath.Join(art.Path, "across-both-members")
	if err := os.WriteFile(marker, []byte("striped"), 0o600); err != nil {
		t.Fatalf("write into the striped filesystem: %v", err)
	}
}

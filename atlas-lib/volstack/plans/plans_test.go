// What the plan shapes have to guarantee.
//
// Shape, mostly: every test here reads a plan's layer names and compares them
// with the row the design's plan table gives for that volume kind. A plan is a
// list of layers and nothing else, so the names in order are the whole of what a
// constructor decides, and asserting them is what keeps the table and the code
// from drifting apart.
//
// Nothing here touches a host. Building a plan resolves no device, runs no
// command, and reads no sysfs, which is the property that lets the CSI driver
// unit-test its plan selection. A test here that needed a kernel would be
// evidence that the property was lost.

package plans

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

// testNode is a node whose seams are all absent. Construction never calls
// through them, so a plan test needs no fakes at all.
func testNode() *Node {
	return NewNode(NodeConfig{HostNQN: "nqn.2014-08.org.nvmexpress:uuid:host", HostID: "host-id"})
}

// testVolume is the volume every plan below is built for.
func testVolume() Volume {
	return Volume{
		UUID:        "3f2b1c",
		StagingPath: "/var/lib/kubelet/staging/pvc-3f2b1c",
		FsType:      "xfs",
	}
}

// conn is one namespace the control plane published.
func conn(nqn string) lvol.Connection {
	return lvol.Connection{
		NQN:       nqn,
		NSID:      1,
		Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.0.0.1", Port: 4420}},
	}
}

// assertShape compares a plan with the row the plan table gives for it.
func assertShape(t *testing.T, plan volstack.Plan, want []string) {
	t.Helper()
	if got := plan.Names(); !slices.Equal(got, want) {
		t.Fatalf("plan shape: got %v, want %v", got, want)
	}
}

// TestRawBlockIsFabricAlone holds the plan table's first row. Raw block mode is
// the plain plan with its top layer absent rather than a flag inside a stage
// function, and asserting the shape here is what keeps it that way.
func TestRawBlockIsFabricAlone(t *testing.T) {
	assertShape(t, testNode().RawBlock(conn("nqn.2023-01.io.simplyblock:vol")), []string{"fabric"})
}

// TestPlainIsFabricThenFilesystem holds the plan table's second row, which is
// the RWO plan the node service performs today.
func TestPlainIsFabricThenFilesystem(t *testing.T) {
	plan := testNode().Plain(conn("nqn.2023-01.io.simplyblock:vol"), testVolume())
	assertShape(t, plan, []string{"fabric", "filesystem"})
}

// TestLVMIsTheFiveLayerStack holds the plan table's third row: the group is a
// layer between the physical volumes and the logical one rather than part of
// either.
func TestLVMIsTheFiveLayerStack(t *testing.T) {
	plan := testNode().LVM(conn("nqn.2023-01.io.simplyblock:vol"), testVolume(), LogicalVolumeOptions{})
	assertShape(t, plan, []string{
		"fabric", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume", "filesystem",
	})
}

// TestStripedReplacesTheFabricWithItsMembers holds the striped row's bottom.
// It is the only plan whose bottom is not a single layer, which is the whole
// reason the composite exists.
func TestStripedReplacesTheFabricWithItsMembers(t *testing.T) {
	conns := []lvol.Connection{conn("nqn:a"), conn("nqn:b"), conn("nqn:c")}
	plan := testNode().Striped(conns, testVolume(), LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Stripes: 3},
	})
	assertShape(t, plan, []string{
		"members", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume", "filesystem",
	})
}

// TestStripedKeepsTheMemberOrder is the guarantee behind that composite: a
// stripe assembled over the same members in a different order is a different
// device, so the order the caller gave is the order the members hold.
func TestStripedKeepsTheMemberOrder(t *testing.T) {
	conns := []lvol.Connection{conn("nqn:a"), conn("nqn:b"), conn("nqn:c")}
	plan := testNode().Striped(conns, testVolume(), LogicalVolumeOptions{})

	composite, ok := plan[0].(volstack.Composite)
	if !ok {
		t.Fatalf("the bottom of a striped plan is %T, which is not a composite", plan[0])
	}
	members := composite.Members()
	if len(members) != len(conns) {
		t.Fatalf("members: got %d, want one per connection (%d)", len(members), len(conns))
	}
	for i, member := range members {
		recorder, isRecorder := member.(volstack.Recorder)
		if !isRecorder {
			t.Fatalf("member %d is %T, which records nothing", i, member)
		}
		params, isFabric := recorder.Params().(layers.FabricParams)
		if !isFabric {
			t.Fatalf("member %d carries %T, want fabric parameters", i, recorder.Params())
		}
		if params.NQN != conns[i].NQN {
			t.Errorf("member %d: got %s, want %s", i, params.NQN, conns[i].NQN)
		}
	}
}

// TestFabricCarriesTheConnectionAndTheHost proves the node's identity and the
// published connection reach the bottom layer, since a fabric built without
// them connects as the wrong host or to nothing.
func TestFabricCarriesTheConnectionAndTheHost(t *testing.T) {
	plan := testNode().Plain(conn("nqn.2023-01.io.simplyblock:vol"), testVolume())

	params, ok := plan[0].(volstack.Recorder).Params().(layers.FabricParams)
	if !ok {
		t.Fatalf("the bottom layer carries %T, want fabric parameters", plan[0].(volstack.Recorder).Params())
	}
	if params.NQN != "nqn.2023-01.io.simplyblock:vol" {
		t.Errorf("nqn: got %s", params.NQN)
	}
	if params.NSID != 1 {
		t.Errorf("nsid: got %d, want 1", params.NSID)
	}
	if params.HostNQN != "nqn.2014-08.org.nvmexpress:uuid:host" {
		t.Errorf("host nqn: got %s", params.HostNQN)
	}
}

// TestFilesystemCarriesTheVolumesType proves the volume's filesystem reaches
// the top layer, which decides both what a blank device is formatted as and
// what the layer refuses to mount.
func TestFilesystemCarriesTheVolumesType(t *testing.T) {
	plan := testNode().Plain(conn("nqn:vol"), testVolume())

	params, ok := plan[len(plan)-1].(volstack.Recorder).Params().(layers.FilesystemParams)
	if !ok {
		t.Fatalf("the top layer carries %T, want filesystem parameters", plan[len(plan)-1])
	}
	if params.FsType != "xfs" {
		t.Errorf("fs type: got %s, want xfs", params.FsType)
	}
}

// TestLogicalVolumeCarriesItsDefinition proves the definition reaches the layer
// that acts on it. It decides the arguments lvcreate is given and the geometry
// the layer reports upward, so a plan that dropped it would create a linear
// volume and describe it as striped.
func TestLogicalVolumeCarriesItsDefinition(t *testing.T) {
	definition := lvm.LogicalVolumeDefinition{Stripes: 4, StripeChunkBytes: 65536}
	plan := testNode().Striped(
		[]lvol.Connection{conn("nqn:a"), conn("nqn:b"), conn("nqn:c"), conn("nqn:d")},
		testVolume(),
		LogicalVolumeOptions{Definition: definition},
	)

	params, ok := plan[3].(volstack.Recorder).Params().(layers.LVMLogicalVolumeParams)
	if !ok {
		t.Fatalf("the logical-volume layer carries %T", plan[3].(volstack.Recorder).Params())
	}
	if params.Stripes != 4 || params.StripeChunkBytes != 65536 {
		t.Errorf("definition: got %d stripes of %d bytes", params.Stripes, params.StripeChunkBytes)
	}
}

// TestVDOCarriesItsPoolAndCapability proves the two things that separate a VDO
// plan from a linear one: lvcreate's pool target, and the label a node must
// carry for the volume to be staged there at all.
func TestVDOCarriesItsPoolAndCapability(t *testing.T) {
	plan := testNode().LVM(conn("nqn:vol"), testVolume(), LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Deduplication: true, Compression: true},
		PoolName:   "vdopool",
		Capability: volstack.Capability("vdo"),
	})

	params, ok := plan[3].(volstack.Recorder).Params().(layers.LVMLogicalVolumeParams)
	if !ok {
		t.Fatalf("the logical-volume layer carries %T", plan[3].(volstack.Recorder).Params())
	}
	if params.PoolName != "vdopool" || !params.Deduplication || !params.Compression {
		t.Errorf("vdo parameters: got %+v", params)
	}

	requirements, ok := plan[3].(volstack.NodeRequirements)
	if !ok {
		t.Fatalf("the logical-volume layer declares no node requirements")
	}
	if requirements.NodeCapability() != volstack.Capability("vdo") {
		t.Errorf("capability: got %q, want vdo", requirements.NodeCapability())
	}
}

// TestLVMRawBlockIsTheLVMRowWithoutItsFilesystem proves that a volume the pod
// opens as a block device still gets the layers that compress and deduplicate
// it, and nothing that would format it. Deduplication is a property of the
// logical volume rather than of the filesystem above it, so dropping the LVM
// layers along with the filesystem would silently turn such a volume into a
// plain one.
func TestLVMRawBlockIsTheLVMRowWithoutItsFilesystem(t *testing.T) {
	options := LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Compression: true},
		PoolName:   "vdopool",
	}

	assertShape(t, testNode().LVMRawBlock(conn("nqn:vol"), testVolume(), options),
		[]string{"fabric", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume"})

	assertShape(t, testNode().LVM(conn("nqn:vol"), testVolume(), options),
		[]string{"fabric", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume", "filesystem"})
}

// TestThePoolSurvivesACloneResolution is the one place a plan's pool name has to
// reach a layer other than the one that creates the volume.
//
// A byte-level clone carries its source's LVM metadata, and resolving it renames
// the logical volume carrying the source's name. The pool is named the same in
// every volume's group, so a resolution that did not know to leave it alone
// would rename the pool instead and leave the logical volume pointing at one
// that no longer answers to what its metadata calls it.
//
// It runs the layer rather than reading the plan, because the physical-volume
// layer carries no record parameters by contract, so what it was built with is
// observable only in what it does.
func TestThePoolSurvivesACloneResolution(t *testing.T) {
	commands := &recordingLVM{out: map[string]string{
		"pvs": "vol-other\n",
		"lvs": "  vdopool\n  lv-other\n",
		// Adopted on its device before the import, so the group reads as owned
		// by the time anything is renamed in it.
		"vgs": "  " + lvm.OwnerTag + "\n",
	}}
	node := NewNode(NodeConfig{
		Manager: lvm.NewManagerWithRunner(commands.run),
		Content: clonedMember{},
	})

	plan := node.LVM(conn("nqn:vol"), testVolume(), LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Deduplication: true},
		PoolName:   "vdopool",
	})
	if _, err := plan[1].Ensure(context.Background(), clonedDevice()); err != nil {
		t.Fatalf("resolve the clone: %v", err)
	}

	renamed, ok := commands.renamed()
	if !ok {
		t.Fatalf("nothing was renamed, so the clone still carries its source's identity:\n%v", commands.calls)
	}
	if renamed == "vdopool" {
		t.Fatal("the clone resolution renamed the pool, which the logical volume above it points at by name")
	}
	if renamed != "lv-other" {
		t.Errorf("the clone resolution renamed %q, want the volume carrying the source's name", renamed)
	}
}

// TestALinearVolumePreservesNothing is the negative of the case above: a row
// with no pool has no structural volume to spare, so the first volume the
// resolution finds is the one carrying the source's name.
func TestALinearVolumePreservesNothing(t *testing.T) {
	commands := &recordingLVM{out: map[string]string{
		"pvs": "vol-other\n",
		"lvs": "  lv-other\n",
		"vgs": "  " + lvm.OwnerTag + "\n",
	}}
	node := NewNode(NodeConfig{
		Manager: lvm.NewManagerWithRunner(commands.run),
		Content: clonedMember{},
	})

	plan := node.LVM(conn("nqn:vol"), testVolume(), LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Stripes: 2},
	})
	if _, err := plan[1].Ensure(context.Background(), clonedDevice()); err != nil {
		t.Fatalf("resolve the clone: %v", err)
	}

	if renamed, ok := commands.renamed(); !ok || renamed != "lv-other" {
		t.Errorf("the clone resolution renamed %q (found: %t), want lv-other", renamed, ok)
	}
}

// clonedDevice is the one device a clone's physical-volume layer sits on.
func clonedDevice() volstack.Artifact {
	return volstack.Artifact{Devices: []blockdev.Device{{Path: "/dev/nvme1n1"}}}
}

// clonedMember reads every device as an LVM member, which is what a clone's
// device is: the label came across with the bytes.
type clonedMember struct{}

func (clonedMember) Read(context.Context, blockdev.Device) (blockdev.Reading, error) {
	return blockdev.Reading{Content: blockdev.ContentStackLayer, Type: "LVM2_member"}, nil
}

// recordingLVM answers the LVM commands a clone resolution runs, keyed on the
// command word, and keeps what was asked. It fakes the execution rather than the
// Manager, so the real argument building and output parsing run.
type recordingLVM struct {
	calls [][]string
	out   map[string]string
}

func (r *recordingLVM) run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, args)
	return r.out[args[0]], nil
}

// renamed is the logical volume an lvrename was pointed at, and reports whether
// there was one.
func (r *recordingLVM) renamed() (string, bool) {
	for _, call := range r.calls {
		if call[0] == "lvrename" && len(call) >= 3 {
			return call[2], true
		}
	}
	return "", false
}

// TestNamesDeriveFromTheVolumeAlone is the rule both consumers have to agree on
// character for character: a plan replayed on another host, or by a teardown
// that has only the handle, arrives at the same names.
func TestNamesDeriveFromTheVolumeAlone(t *testing.T) {
	v := testVolume()
	if got := v.VolumeGroup(); got != VolumeGroupName("3f2b1c") {
		t.Errorf("volume group: %s and %s disagree", got, VolumeGroupName("3f2b1c"))
	}
	if got := v.LogicalVolume(); got != LogicalVolumeName("3f2b1c") {
		t.Errorf("logical volume: %s and %s disagree", got, LogicalVolumeName("3f2b1c"))
	}
	if VolumeGroupName("3f2b1c") == LogicalVolumeName("3f2b1c") {
		t.Errorf("the group and the volume inside it cannot share a name: %s", v.VolumeGroup())
	}
}

// recordingFS is a layers.FilesystemOps that answers rather than acts. Only
// IsMountPoint is reached below, and the rest exist because the layer takes the
// whole interface.
type recordingFS struct {
	checked   []string
	formatted []formatCall
	mounted   []string
	isMounted bool
}

// formatCall is one mkfs, kept so a test can read what the plan asked for.
type formatCall struct {
	device  string
	fsType  string
	options []string
}

func (r *recordingFS) Format(_ context.Context, device, fsType string, options []string) error {
	r.formatted = append(r.formatted, formatCall{device, fsType, options})
	return nil
}

func (r *recordingFS) Mount(_ context.Context, _, target, _ string, _ []string) error {
	r.mounted = append(r.mounted, target)
	return nil
}

func (r *recordingFS) Unmount(context.Context, string) error { return nil }

func (r *recordingFS) ForceUnmount(context.Context, string) error { return nil }

func (r *recordingFS) Grow(context.Context, []string) error { return nil }

func (r *recordingFS) IsMountPoint(_ context.Context, path string) (bool, error) {
	r.checked = append(r.checked, path)
	return r.isMounted, nil
}

// blankDevice is a layers.ContentReader for a device carrying nothing, which is
// the only reading that permits a format.
type blankDevice struct{}

func (blankDevice) Read(context.Context, blockdev.Device) (blockdev.Reading, error) {
	return blockdev.Reading{Content: blockdev.ContentBlank}, nil
}

// aDevice is what the fabric below a filesystem layer hands upward.
func aDevice() volstack.Artifact {
	return volstack.Artifact{Devices: []blockdev.Device{{Name: "nvme0n1", Path: "/dev/nvme0n1"}}}
}

// TestTheNodesSeamsReachTheLayers is the other half of what a constructor
// decides. A plan of the right shape built over the wrong node would pass every
// test above and connect as no host, mount nowhere, and run no LVM, so this
// observes a plan far enough to see the node's own implementations being called
// with the volume's own values.
func TestTheNodesSeamsReachTheLayers(t *testing.T) {
	ops := &recordingFS{}
	ops.isMounted = true
	node := NewNode(NodeConfig{HostNQN: "nqn:host", HostID: "host-id", Filesystem: ops})
	volume := testVolume()

	plan := node.Plain(conn("nqn:vol"), volume)
	state, artifact, err := plan[1].Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("observe the filesystem layer: %v", err)
	}

	if !slices.Equal(ops.checked, []string{volume.StagingPath}) {
		t.Errorf("the layer checked %v, want the volume's staging path %s",
			ops.checked, volume.StagingPath)
	}
	if state != volstack.StateReady {
		t.Errorf("state: got %v, want ready for a mounted path", state)
	}
	if artifact.Path != volume.StagingPath {
		t.Errorf("artifact path: got %s, want %s", artifact.Path, volume.StagingPath)
	}
}

// TestTheVolumesReservationReachesMkfs proves a volume asking for reserved
// blocks gets them. The plan carries the property and the filesystem spells it,
// so a consumer never writes the flag itself.
func TestTheVolumesReservationReachesMkfs(t *testing.T) {
	ops := &recordingFS{}
	node := NewNode(NodeConfig{Filesystem: ops, Content: blankDevice{}})

	volume := testVolume()
	volume.FsType = "ext4"
	volume.ReservedBlocksPercent = "3"

	plan := node.Plain(conn("nqn:vol"), volume)
	if _, err := plan[1].Ensure(context.Background(), aDevice()); err != nil {
		t.Fatalf("ensure the filesystem: %v", err)
	}

	if len(ops.formatted) != 1 {
		t.Fatalf("formatted %d times, want once", len(ops.formatted))
	}
	options := strings.Join(ops.formatted[0].options, " ")
	if !strings.Contains(options, "-m 3") {
		t.Errorf("mkfs options are %q, want the reservation the volume asked for", options)
	}
}

// TestThePriorFormatRecordReachesTheFilesystem proves the guard is wired. The
// device reads blank and the node's record says this volume was formatted, so
// the plan must mount it and never format it: the record is what stands between
// a failed probe and a destroyed volume.
func TestThePriorFormatRecordReachesTheFilesystem(t *testing.T) {
	ops := &recordingFS{}
	var asked []string
	node := NewNode(NodeConfig{
		Filesystem: ops,
		Content:    blankDevice{},
		PriorFormat: func(_ context.Context, volume Volume) (string, error) {
			asked = append(asked, volume.UUID)
			return "ext4", nil
		},
	})

	volume := testVolume()
	volume.FsType = "ext4"

	plan := node.Plain(conn("nqn:vol"), volume)
	if _, err := plan[1].Ensure(context.Background(), aDevice()); err != nil {
		t.Fatalf("ensure the filesystem: %v", err)
	}

	if len(ops.formatted) != 0 {
		t.Fatalf("formatted a volume the record says was already formatted: %+v", ops.formatted)
	}
	if len(ops.mounted) != 1 {
		t.Errorf("mounted %d times, want once", len(ops.mounted))
	}
	if !slices.Equal(asked, []string{volume.UUID}) {
		t.Errorf("the record was asked about %v, want the volume being staged", asked)
	}
}

// TestAPlanWithoutARecordAsksNothing keeps the guard optional: a consumer that
// keeps no record leaves the seam nil and the reading decides alone.
func TestAPlanWithoutARecordAsksNothing(t *testing.T) {
	ops := &recordingFS{}
	node := NewNode(NodeConfig{Filesystem: ops, Content: blankDevice{}})

	volume := testVolume()
	volume.FsType = "ext4"

	plan := node.Plain(conn("nqn:vol"), volume)
	if _, err := plan[1].Ensure(context.Background(), aDevice()); err != nil {
		t.Fatalf("ensure the filesystem: %v", err)
	}
	if len(ops.formatted) != 1 {
		t.Errorf("formatted %d times, want once for a device nothing contradicts", len(ops.formatted))
	}
}

// TestANodeWithoutAManagerStillGetsOne closes the class of failure that a nil
// concrete seam is.
//
// The manager's methods take a pointer receiver, so a nil one is not an error at
// the call site: it panics inside the first LVM command a layer runs. In the CSI
// node plugin that is the process, and with it that node's CSI. A consumer that
// forgot to pass one, which is exactly what happened when the LVM rows were
// first selected, gets the shipped implementation instead of a crash.
func TestANodeWithoutAManagerStillGetsOne(t *testing.T) {
	plan := NewNode(NodeConfig{}).LVM(conn("nqn:vol"), testVolume(), LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Compression: true},
		PoolName:   "vdopool",
	})

	// Observing reaches the manager. It is allowed to fail — there is no LVM
	// here — but it must fail rather than panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a plan built without a manager panicked instead of failing: %v", r)
		}
	}()
	if _, _, err := plan[2].Observe(context.Background(), clonedDevice()); err == nil {
		t.Log("the volume-group layer answered without an LVM present, which is fine")
	}
}

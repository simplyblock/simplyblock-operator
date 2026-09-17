// Client-side compression and deduplication (issue #277): a VDO device
// assembled between the raw NVMe-oF device this node already connected and
// the filesystem staged on top of it.
//
// Built on atlas-lib/volstack's three LVM layers (lvmPhysicalVolume,
// lvmVolumeGroup, lvmLogicalVolume), which is design-node-volume-stack.md's
// Phase 2, in place of the flat atlas-lib/lvm/vdo package the original
// design (design-issue-277-client-side-compression.md §7.1) describes and
// PR #402 implemented directly: that package retired once the layered stack
// existed to absorb it, and nothing else ever imported it.
//
// Only the three LVM layers are adopted here, not fabric or filesystem.
// Adopting fabric would mean standing up atlas-lib/nvmeof as a second NVMe-oF
// connection path alongside internal/initiator for every volume kind this
// node stages, which is Phase 1 of the node-volume-stack design and unwired
// anywhere in this repository today — a much larger change than this feature
// needs. rawDeviceLayer below is the seam instead: it exposes the device path
// internal/initiator already connected as a volstack.Artifact, so the three
// LVM layers can be driven by the real volstack.Runner (gaining the stack
// record, and the idempotent Ensure/Release/Grow every layer already
// implements) without this package reimplementing any of it.
package node

import (
	"context"
	"fmt"
	"strconv"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
	"github.com/simplyblock/atlas/volstack/plans"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// vdoStackRecordDir is where this node's VDO stack records live
// (design-node-volume-stack.md §6), backed by a host path the csi-node
// DaemonSet mounts so it outlives a plugin restart, the same way
// guardian.StatePath does for the guardian's own state.
const vdoStackRecordDir = "/var/run/simplyblock/stacks"

// vdoPoolName is the fixed name of the VDO pool lvcreate --type vdo creates
// alongside the logical volume, inside every volume's own group. It is not
// per-volume: uniqueness comes from the volume group name, one group per
// volume. It has to stay a structural, unrenamed name across a clone
// resolution (design-issue-277 §7.4), which is why it is passed as
// lvmPhysicalVolume's PreserveLogicalVolumes below.
const vdoPoolName = "vdopool"

// vdoParams reports whether vc, a volume's context (the StorageClass
// parameters that provisioned it — see kube.Properties), asks for client-side
// compression or deduplication, and which. Either one alone requires the full
// VDO mechanism (design-issue-277 §6): a deduplication-only volume needs a
// working module and stack exactly as much as a compression-only one does.
func vdoParams(vc map[string]string) (compression, deduplication, wantsVDO bool) {
	compression, _ = strconv.ParseBool(vc[kube.ParamClientCompression])
	deduplication, _ = strconv.ParseBool(vc[kube.ParamClientDeduplication])
	return compression, deduplication, compression || deduplication
}

// vdoLvolID is the identity a volume's VDO stack is named after: the lvol
// UUID when volumeID parses as one, and the raw CSI volume id otherwise,
// which is what a volume provisioned before the v2 API migration or by an
// unrelated ID scheme still has. Naming derived from anything else would not
// survive a plan replayed on another host, or a teardown working from a
// stack record rather than a live parse (design-issue-277 §7.6).
func vdoLvolID(volumeID string) string {
	if handle, err := csicommon.ParseVolumeHandle(volumeID); err == nil {
		return handle.VolumeID
	}
	return volumeID
}

// rawDeviceLayer stands in for volstack's own fabric layer: it exposes a
// device this package's initiator.Initiator already connected, rather than
// connecting one of its own. See this file's package comment for why fabric
// itself is not adopted here.
type rawDeviceLayer struct {
	path    string
	resolve layers.DeviceResolver
}

func newRawDeviceLayer(path string, resolve layers.DeviceResolver) *rawDeviceLayer {
	if resolve == nil {
		resolve = blockdev.ResolveDevice
	}
	return &rawDeviceLayer{path: path, resolve: resolve}
}

func (l *rawDeviceLayer) Name() string { return "rawDevice" }

// Observe reports StateAbsent rather than an error when the device no longer
// resolves, tolerating a dead foundation the way every other layer's Observe
// does (design-node-volume-stack.md §7.4): a teardown reaching this layer
// after total path loss must not fail just because the device it stood on is
// exactly what is gone.
func (l *rawDeviceLayer) Observe(
	_ context.Context, _ volstack.Artifact,
) (volstack.State, volstack.Artifact, error) {
	dev, err := l.resolve(l.path)
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, nil
	}
	return volstack.StateReady, volstack.Artifact{Devices: []blockdev.Device{dev}}, nil
}

// Ensure has nothing to converge — the device is connected by
// internal/initiator before this layer's plan is ever built — so absence here
// means a caller invoked this stack out of order, and that is reported rather
// than silently producing an empty artifact for the layer above to fail on
// less clearly.
func (l *rawDeviceLayer) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	state, own, err := l.Observe(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}
	if state == volstack.StateAbsent {
		return volstack.Artifact{}, fmt.Errorf(
			"rawDevice: %s is not present; the initiator must connect it before the VDO stack can be brought up", l.path)
	}
	return own, nil
}

// Release does nothing: disconnecting the raw NVMe-oF path stays
// internal/initiator's job, called separately around this stack's Up/Down,
// exactly as the real fabric layer leaves the connection itself to its own
// Connector.
func (l *rawDeviceLayer) Release(context.Context, volstack.Artifact) error { return nil }

// Destroy does nothing: the namespace belongs to the control plane, and is
// removed by DeleteVolume rather than by anything this layer does.
func (l *rawDeviceLayer) Destroy(context.Context, volstack.Artifact) error { return nil }

// vdoStack builds and drives one volume's VDO stack: rawDevice ->
// lvmPhysicalVolume -> lvmVolumeGroup -> lvmLogicalVolume(vdo), through the
// real volstack.Runner. One instance is shared process-wide, the way the
// retired atlas-lib/lvm/vdo package's callers shared one lvm.Manager.
type vdoStack struct {
	manager *lvm.Manager
	content layers.ContentReader
	runner  *volstack.Runner
	resolve layers.DeviceResolver
}

// newVDOStack returns a stack recording its plans under recordDir, which has
// to be a host path that outlives the csi-node container (see
// design-node-volume-stack.md §6): a plugin restart is an ordinary event, and
// the record is what tells the restarted process what the previous one built.
func newVDOStack(recordDir string) *vdoStack {
	return &vdoStack{
		manager: lvm.NewManager(),
		content: blockdev.NewProber(),
		runner:  volstack.NewRunner(volstack.NewStore(recordDir)),
	}
}

// plan is the same three-layer, one-volume plan for every verb: Up brings it
// up, Down and Grow act on the identical shape so that the runner's survey
// walks the stack it actually built rather than a plan reconstructed
// differently for each verb.
func (s *vdoStack) plan(rawDevicePath, lvolID string, compression, deduplication bool) volstack.Plan {
	vg := plans.VolumeGroupName(lvolID)
	lvName := plans.LogicalVolumeName(lvolID)
	return volstack.Plan{
		newRawDeviceLayer(rawDevicePath, s.resolve),
		layers.NewLVMPhysicalVolume(layers.LVMPhysicalVolumeConfig{
			VolumeGroup:            vg,
			LogicalVolume:          lvName,
			PreserveLogicalVolumes: []string{vdoPoolName},
			Manager:                s.manager,
			Content:                s.content,
		}),
		layers.NewLVMVolumeGroup(layers.LVMVolumeGroupConfig{
			VolumeGroup: vg,
			Manager:     s.manager,
		}),
		layers.NewLVMLogicalVolume(layers.LVMLogicalVolumeConfig{
			VolumeGroup:   vg,
			LogicalVolume: lvName,
			PoolName:      vdoPoolName,
			Definition:    lvm.LogicalVolumeDefinition{Compression: compression, Deduplication: deduplication},
			// Not consumed until design-node-volume-stack.md's Phase 4 (a plan's
			// NodeRequirements feeding controller-side topology derivation) lands;
			// harmless to set now, and correct once it does.
			Capability: volstack.Capability(kube.LabelVDOCapable),
			Manager:    s.manager,
			Resolve:    s.resolve,
		}),
	}
}

// Up brings lvolID's VDO stack up over rawDevicePath — idempotently: a stack
// that already exists is reactivated, never recreated, by the layers'
// own Ensure (design-issue-277 §7.2). Returns the device to format and mount
// in place of the raw one.
func (s *vdoStack) Up(
	ctx context.Context, lvolID, rawDevicePath string, compression, deduplication bool,
) (string, error) {
	artifact, err := s.runner.Up(ctx, lvolID, s.plan(rawDevicePath, lvolID, compression, deduplication))
	if err != nil {
		return "", err
	}
	dev, ok := artifact.Device()
	if !ok {
		return "", fmt.Errorf(
			"vdoStack: bring-up of %s produced %d devices, want exactly one", lvolID, len(artifact.Devices))
	}
	return dev.Path, nil
}

// Down releases lvolID's VDO stack: deactivates the volume group and, when
// the backing device is already gone, falls back to removing the live
// device-mapper nodes directly (design-issue-277 §7.3, §8). It never
// destroys: NodeUnstageVolume is the only caller, and it fires whenever no
// pod on this node needs the volume, including an ordinary pod restart.
//
// Takes no compression/deduplication: LVMLogicalVolume.Release, the method
// this reaches, never reads its Definition (only create, reached from Ensure
// when the volume does not exist yet, does), so a real value here would be
// discarded, not merely unused.
func (s *vdoStack) Down(ctx context.Context, lvolID, rawDevicePath string) error {
	return s.runner.Down(ctx, lvolID, s.plan(rawDevicePath, lvolID, false, false))
}

// Grow extends lvolID's VDO pool and logical volume to the physical space
// rawDevicePath now reports, ahead of the filesystem resize (design-issue-277
// §9).
//
// Takes no compression/deduplication for the same reason Down does not:
// LVMLogicalVolume.Grow sizes the pool and the logical volume from the
// volume group and pool names alone and never reads Definition.
func (s *vdoStack) Grow(ctx context.Context, lvolID, rawDevicePath string) error {
	return s.runner.Grow(ctx, s.plan(rawDevicePath, lvolID, false, false))
}

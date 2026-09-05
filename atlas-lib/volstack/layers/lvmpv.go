// The physical-volume layer: a label on a device, and the question of whether the
// device was anybody's before the label went on.
//
// pvcreate destroys what it lands on exactly as mkfs does, so this layer answers
// that question the same way the filesystem layer above it does: from a positive
// reading that the device holds nothing. Asking LVM instead is the shape the VDO
// stack had, and LVM reports "no label here" identically for an empty device and
// for one it could not read.

package layers

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// lvmMemberType is what a physical-volume label reads as, and it is the one
// reading this layer may act on.
const lvmMemberType = "LVM2_member"

// LVMPhysicalVolumeConfig is what a physical-volume layer is built with.
type LVMPhysicalVolumeConfig struct {
	// VolumeGroup is the group this volume's label belongs to. A label naming
	// anything else is another volume's, which is what StateForeign reports.
	VolumeGroup string

	// LogicalVolume is the name the volume's logical volume carries.
	//
	// It belongs to the layer above, and this layer needs it anyway: resolving a
	// clone renames the volume group but leaves the logical volume inside named
	// after the source, and the layer above looks that volume up by name. Leaving
	// the rename to it would have it find no logical volume of its own, read the
	// group as an interrupted create, and lvcreate a second one beside the
	// clone's data.
	LogicalVolume string

	// PreserveLogicalVolumes are the structural logical volumes a stack creates
	// for itself and names identically in every volume, a VDO pool being the one
	// that exists today. They keep their names through a clone resolution, so
	// that exactly the one carrying the source's name is renamed.
	PreserveLogicalVolumes []string

	Manager *lvm.Manager
	Content ContentReader
}

// LVMPhysicalVolume puts an LVM physical-volume label on the device below it, and
// is the layer that decides whether that device was free to label.
type LVMPhysicalVolume struct {
	cfg LVMPhysicalVolumeConfig
}

// NewLVMPhysicalVolume returns the physical-volume layer for one volume.
func NewLVMPhysicalVolume(cfg LVMPhysicalVolumeConfig) *LVMPhysicalVolume {
	return &LVMPhysicalVolume{cfg: cfg}
}

// Name is what the record calls this layer.
func (l *LVMPhysicalVolume) Name() string { return "lvmPV" }

// Observe reads the device below and reports what may be done to it.
//
// Every reading that is neither a blank device nor an LVM label is an error
// rather than a state. A filesystem where a label was expected means the plan is
// wrong, and a partition table or another stack's header means the device is
// somebody's: none of them is a thing to converge, and labeling through the doubt
// is what this layer exists to refuse.
func (l *LVMPhysicalVolume) Observe(
	ctx context.Context, below volstack.Artifact,
) (volstack.State, volstack.Artifact, error) {
	state, _, own, err := l.observe(ctx, below)
	return state, own, err
}

// observe is Observe plus the device it decided on, which Ensure needs in order
// to act without reading the device a second time. On a degraded device the read
// is the expensive thing in the whole path.
func (l *LVMPhysicalVolume) observe(
	ctx context.Context, below volstack.Artifact,
) (volstack.State, lvm.PhysicalVolume, volstack.Artifact, error) {
	dev, ok := below.Device()
	if !ok {
		return volstack.StateAbsent, lvm.PhysicalVolume{}, volstack.Artifact{}, errors.New(
			"lvmPV: the layer below exposes no single device to label")
	}
	pv := lvm.PhysicalVolume{DevicePath: dev.Path}

	reading, err := l.cfg.Content.Read(ctx, dev)
	if err != nil {
		return volstack.StateAbsent, pv, volstack.Artifact{}, fmt.Errorf(
			"lvmPV: cannot read what %s carries, so it is not a device this may label: %w", dev.Path, err)
	}

	switch {
	case reading.Content == blockdev.ContentBlank:
		// Nothing of this layer exists yet, so it exposes nothing.
		return volstack.StateAbsent, pv, volstack.Artifact{}, nil

	case reading.Content == blockdev.ContentStackLayer && reading.Type == lvmMemberType:
		return l.identify(ctx, pv, below)

	default:
		return volstack.StateAbsent, pv, volstack.Artifact{}, fmt.Errorf(
			"lvmPV: refusing to label %s, which carries %s: %s", dev.Path, reading.Content, reading.Detail)
	}
}

// identify asks a labeled device whose label it is.
//
// The label is on the device, so the question left is one only LVM can answer,
// and a probe that fails is an error rather than an identity: LVM reports a
// device it could not lock the same way it reports one belonging to nobody.
func (l *LVMPhysicalVolume) identify(
	ctx context.Context, pv lvm.PhysicalVolume, below volstack.Artifact,
) (volstack.State, lvm.PhysicalVolume, volstack.Artifact, error) {
	group, err := l.cfg.Manager.VolumeGroup(ctx, pv)
	if err != nil {
		return volstack.StateAbsent, pv, volstack.Artifact{}, fmt.Errorf(
			"lvmPV: %s carries a label and its volume group cannot be read: %w", pv.DevicePath, err)
	}

	// The artifact is the device the label sits on, in every state but absent. A
	// foreign label is still something a teardown has to reach.
	own := volstack.Artifact{Devices: below.Devices}

	switch group.Name {
	case "", l.cfg.VolumeGroup:
		// A label belonging to no group at all is this layer's own object,
		// finished: pvcreate is the whole of what it does, and the group above it
		// belongs to the layer above. It is what an Ensure that succeeded and then
		// crashed leaves behind, so reading it as anything else makes the bring-up
		// non-convergent.
		return volstack.StateReady, pv, own, nil
	default:
		return volstack.StateForeign, pv, own, nil
	}
}

// Ensure labels a blank device and re-identifies a clone, and does nothing at all
// to a device already carrying this volume's label.
func (l *LVMPhysicalVolume) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	state, pv, own, err := l.observe(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}

	switch state {
	case volstack.StateReady:
		return own, nil

	case volstack.StateAbsent:
		if _, err := l.cfg.Manager.CreatePhysicalVolume(ctx, pv); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmPV: %w", err)
		}

	case volstack.StateForeign:
		// The data is the clone's own, and the only thing wrong with it is whose
		// name is on it, so this re-stamps the identity and touches nothing else.
		if _, err := l.cfg.Manager.ResolveClonedVolumeGroup(ctx, pv,
			lvm.VolumeGroup{Name: l.cfg.VolumeGroup}, l.cfg.LogicalVolume,
			l.cfg.PreserveLogicalVolumes...); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmPV: %w", err)
		}

	case volstack.StatePartial, volstack.StateInactive:
		// A label is written or it is not, so neither state is reachable here.
		return volstack.Artifact{}, fmt.Errorf(
			"lvmPV: %s reports %s, which a physical-volume label has no meaning for", pv.DevicePath, state)
	}

	return volstack.Artifact{Devices: below.Devices}, nil
}

// Release does nothing, and that asymmetry is why physical volumes are a layer of
// their own rather than part of the volume group above them. A label is not a
// hold: it is on the device wherever the device is, so there is nothing for this
// host to give up, and an unstage that removed one would be wiping the identity
// off a volume that is merely between pods.
func (l *LVMPhysicalVolume) Release(context.Context, volstack.Artifact) error { return nil }

// Destroy wipes the label, which is what makes the device blank again for
// anything that reads its content. Only a deletion path reaches it, and by then
// the volume group above the label is gone. If it is not, pvremove refuses, and
// that refusal is returned rather than overridden.
func (l *LVMPhysicalVolume) Destroy(ctx context.Context, below volstack.Artifact) error {
	dev, ok := below.Device()
	if !ok {
		return errors.New("lvmPV: the layer below exposes no single device to unlabel")
	}
	if err := l.cfg.Manager.RemovePhysicalVolume(ctx, lvm.PhysicalVolume{DevicePath: dev.Path}); err != nil {
		return fmt.Errorf("lvmPV: %w", err)
	}
	return nil
}

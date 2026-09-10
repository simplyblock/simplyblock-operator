// The volume-group layer: which members a volume is made of, and whether they
// are mapped on this host.
//
// Separate from the volume inside it because the two stop sharing a lifetime the
// moment a volume grows by gaining members rather than by its members growing.
// A striped export takes capacity that way: the group accepts new physical
// volumes and the logical volume is untouched until it is extended across them,
// which is two changes to two objects, in that order.
//
// It exposes the devices it was given, unchanged, the way the physical-volume
// layer below it does. A group is not a block device; what it does is make one
// possible.

package layers

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// LVMVolumeGroupConfig is what a volume-group layer is built with.
type LVMVolumeGroupConfig struct {
	// VolumeGroup is derived from the volume's identity, so a plan replayed on
	// another host arrives at the same name.
	VolumeGroup string

	Manager *lvm.Manager
}

// LVMVolumeGroup is one volume group over the members below it.
type LVMVolumeGroup struct {
	cfg LVMVolumeGroupConfig
}

// NewLVMVolumeGroup returns the volume-group layer for one volume.
func NewLVMVolumeGroup(cfg LVMVolumeGroupConfig) *LVMVolumeGroup {
	return &LVMVolumeGroup{cfg: cfg}
}

// Name is what the record calls this layer.
func (l *LVMVolumeGroup) Name() string {
	return "lvmVolumeGroup"
}

// membership is which of the devices below already belong to this volume's
// group, and which do not.
type membership struct {
	joined  []lvm.PhysicalVolume
	unknown []lvm.PhysicalVolume
}

// Observe reports how much of the group is here.
//
// The question is membership rather than activation. A group is made of the
// members that carry its name, and a member that carries none has not joined
// yet: that is what a volume gaining capacity looks like before anything has
// been extended across it, and what an interrupted create looks like too. Which
// of the two it is does not change what Ensure does about it.
func (l *LVMVolumeGroup) Observe(
	ctx context.Context, below volstack.Artifact,
) (volstack.State, volstack.Artifact, error) {
	held, err := l.membership(ctx, below)
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, err
	}

	own := volstack.Artifact{Devices: below.Devices, Geometry: below.Geometry}
	switch {
	case len(held.joined) == 0:
		// Nothing of this layer exists yet, so it exposes nothing.
		return volstack.StateAbsent, volstack.Artifact{}, nil
	case len(held.unknown) == 0:
		return volstack.StateReady, own, nil
	default:
		// Some members in and some out, which is either a create that stopped
		// halfway or a volume that has been given more to work with.
		return volstack.StatePartial, own, nil
	}
}

// membership asks each member which group it belongs to.
//
// Read off the device rather than looked up by name, because a name lookup
// answers "not found" for a device carrying somebody else's group, and a caller
// acting on that runs vgcreate over their data.
func (l *LVMVolumeGroup) membership(ctx context.Context, below volstack.Artifact) (membership, error) {
	if len(below.Devices) == 0 {
		return membership{}, errors.New(
			"lvmVolumeGroup: the layer below exposes no devices to make a volume group of")
	}

	var held membership
	for _, dev := range below.Devices {
		pv := lvm.PhysicalVolume{DevicePath: dev.Path}
		group, err := l.cfg.Manager.VolumeGroup(ctx, pv)
		if err != nil {
			return membership{}, fmt.Errorf(
				"lvmVolumeGroup: read the volume group on %s: %w", pv.DevicePath, err)
		}

		switch group.Name {
		case l.cfg.VolumeGroup:
			held.joined = append(held.joined, pv)
		case "":
			held.unknown = append(held.unknown, pv)
		default:
			// The layer below re-identifies a clone, and by the time a bring-up
			// reaches here it has. Meeting one anyway means the plan is wrong or
			// the walk is a read-only one over a stack that was never brought up,
			// and neither is a thing to converge: an error strands a teardown,
			// where creating over it would destroy the clone.
			return membership{}, fmt.Errorf(
				"lvmVolumeGroup: %s carries volume group %s rather than %s, "+
					"which lvmPhysicalVolume resolves and this layer may not",
				pv.DevicePath, group.Name, l.cfg.VolumeGroup)
		}
	}
	return held, nil
}

// Ensure makes the group span every member below and maps it on this host.
//
// Both halves are convergent, and the activation runs whatever the state was:
// a group that exists is not necessarily mapped here, since a release leaves it
// on the members and takes it off the host, which is the whole difference
// between the two.
func (l *LVMVolumeGroup) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	held, err := l.membership(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}

	switch {
	case len(held.joined) == 0:
		if _, err := l.cfg.Manager.CreateVolumeGroup(ctx, l.group(), held.unknown...); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolumeGroup: %w", err)
		}
	case len(held.unknown) > 0:
		// Members that have not joined yet. Adding them is what lets a volume grow
		// by gaining capacity rather than by its capacity growing, and it has to
		// happen before anything above tries to spread onto them.
		if err := l.cfg.Manager.ExtendVolumeGroup(ctx, l.group(), held.unknown...); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolumeGroup: %w", err)
		}
	}

	if err := l.cfg.Manager.ActivateVolumeGroup(ctx, l.group()); err != nil {
		return volstack.Artifact{}, fmt.Errorf("lvmVolumeGroup: %w", err)
	}
	return volstack.Artifact{Devices: below.Devices, Geometry: below.Geometry}, nil
}

// Release unmaps the group on this host and keeps every byte of it. It is what
// an unstage calls, and an unstage fires whenever no pod on this node needs the
// volume, which includes an ordinary restart.
//
// The force path is not exceptional. When the members are gone, LVM can no
// longer reach the metadata it wants to update and every retry fails, so the
// device-mapper nodes are removed directly. That escaping has to double the
// dashes the way device-mapper does, or it matches nothing.
func (l *LVMVolumeGroup) Release(ctx context.Context, _ volstack.Artifact) error {
	if err := l.cfg.Manager.DeactivateVolumeGroup(ctx, l.group()); err == nil {
		return nil
	}
	if err := l.cfg.Manager.RemoveOrphanedDMNodes(ctx, l.group()); err != nil {
		return fmt.Errorf("lvmVolumeGroup: %w", err)
	}
	return nil
}

// Destroy removes the group and with it the last thing that said those members
// belonged together. Only a deletion path reaches it, and the volume inside has
// already been removed by the layer above.
func (l *LVMVolumeGroup) Destroy(ctx context.Context, _ volstack.Artifact) error {
	if err := l.cfg.Manager.RemoveVolumeGroup(ctx, l.group()); err != nil {
		return fmt.Errorf("lvmVolumeGroup: %w", err)
	}
	return nil
}

// Grow takes whatever the members below now offer.
//
// Two ways, and a volume may be given either. A member that grew is resized in
// place, and a member that is new joins the group. Both leave free space the
// layer above extends onto, and neither changes the volume itself, which is why
// they belong here and the extending does not.
func (l *LVMVolumeGroup) Grow(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	held, err := l.membership(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}

	for _, pv := range held.joined {
		if err := l.cfg.Manager.ExpandPhysicalVolume(ctx, pv); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolumeGroup: %w", err)
		}
	}
	if len(held.unknown) > 0 {
		if err := l.cfg.Manager.ExtendVolumeGroup(ctx, l.group(), held.unknown...); err != nil {
			return volstack.Artifact{}, fmt.Errorf("lvmVolumeGroup: %w", err)
		}
	}
	return volstack.Artifact{Devices: below.Devices, Geometry: below.Geometry}, nil
}

func (l *LVMVolumeGroup) group() lvm.VolumeGroup {
	return lvm.VolumeGroup{Name: l.cfg.VolumeGroup}
}

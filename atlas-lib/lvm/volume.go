package lvm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// LogicalVolumeDefinition is what a logical volume is to be, as opposed to where
// it goes. A VolumeProvisioning handler reads the first two and contributes the
// arguments its type needs; the stripe fields are read by CreateLogicalVolume
// itself, because striping is LVM's own and belongs to no handler.
type LogicalVolumeDefinition struct {
	Deduplication bool
	Compression   bool

	// Stripes is how many members data is spread across, and zero or one means a
	// linear volume. A volume group spanning several members still produces a
	// linear volume unless lvcreate is told otherwise, so a striped volume has to
	// say so here or it is silently not one.
	Stripes int

	// StripeChunkBytes is the per-stripe chunk, and zero lets LVM choose. It
	// applies only alongside a stripe count, since there is nothing for a chunk
	// size to describe on a linear volume.
	StripeChunkBytes int64
}

type VolumeProvisioning interface {
	Name() string
	Handles(def LogicalVolumeDefinition) bool
	CreateVolumeArgs(def LogicalVolumeDefinition) []string
}

var volumeProvisioning = map[string]VolumeProvisioning{}

func RegisterVolumeProvisioning(handler VolumeProvisioning) {
	volumeProvisioning[handler.Name()] = handler
}

// RemovePhysicalVolume wipes a device's physical-volume label, which is what
// makes the device blank again for anything that looks at its content.
//
// It is convergent: a device that carries no label is already in the state the
// caller asked for, so that is not an error. A deletion may resume after a crash
// between the pvremove and whatever recorded it, and the second attempt has to
// finish rather than refuse.
//
// It is not forced. pvremove refuses to wipe a label a volume group still
// claims, and --force overrides exactly that refusal, so passing it would trade
// the only check LVM performs here for an assumption about the caller: that the
// volume group above the label is already gone. The assumption fails where it
// costs the most, at a teardown that crashed between the vgremove and this call,
// at a plan naming the wrong device, and at a clone staged beside its source. A
// caller that meets the refusal has a real problem and needs to hear about it.
//
// --yes is not the same flag. It answers the confirmation prompt for a label
// that is free to remove, which a node service has no terminal to answer, and it
// overrides nothing.
func (m *Manager) RemovePhysicalVolume(ctx context.Context, pv PhysicalVolume) error {
	_, err := m.exec(ctx, []string{pv.DevicePath}, "pvremove", "--yes", pv.DevicePath)
	if err != nil {
		if isNoPVLabel(err) {
			return nil
		}
		return fmt.Errorf("pvremove %s: %w", pv.DevicePath, err)
	}
	return nil
}

// isNoPVLabel reports whether err is pvremove's own "there was no label here"
// failure, which for it reads: No PV label found on <device>.
//
// It is separate from isNoPVSignature rather than folded into it, even though
// both mean the same thing about the device. That one decides whether a device
// is blank, and a caller reading blank proceeds to create over it, so widening
// what counts as blank widens what may be written over. This one only decides
// whether a removal that was asked for has already happened, where the same
// answer costs nothing.
func isNoPVLabel(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no pv label")
}

// CreatePhysicalVolume writes an LVM PV signature onto pv's device (pvcreate),
// scoped to that device, and returns pv unchanged for chaining into
// CreateVolumeGroup.
func (m *Manager) CreatePhysicalVolume(ctx context.Context, pv PhysicalVolume) (PhysicalVolume, error) {
	_, err := m.exec(ctx, []string{pv.DevicePath}, "pvcreate", pv.DevicePath)
	if err != nil {
		return PhysicalVolume{}, fmt.Errorf("pvcreate %s: %w", pv.DevicePath, err)
	}
	return pv, nil
}

// CreateVolumeGroup creates volumeGroup on top of pvs (vgcreate), scoped to
// those devices, and returns volumeGroup unchanged for chaining. pvs is
// variadic so a striped volume group across several members and a
// single-device volume group (VDO's case) share the same call shape.
func (m *Manager) CreateVolumeGroup(
	ctx context.Context, volumeGroup VolumeGroup, pvs ...PhysicalVolume,
) (VolumeGroup, error) {
	paths := devicePaths(pvs)
	args := append([]string{"vgcreate", volumeGroup.Name}, paths...)
	if _, err := m.exec(ctx, paths, args...); err != nil {
		return VolumeGroup{}, fmt.Errorf("vgcreate %s on %v: %w", volumeGroup.Name, paths, err)
	}
	return volumeGroup, nil
}

// ActivateVolumeGroup activates volumeGroup's logical volumes (vgchange -ay),
// never recreating or reformatting anything.
func (m *Manager) ActivateVolumeGroup(ctx context.Context, volumeGroup VolumeGroup) error {
	if _, err := m.exec(ctx, nil, "vgchange", "-ay", volumeGroup.Name); err != nil {
		return fmt.Errorf("activate VG %s: %w", volumeGroup.Name, err)
	}
	return nil
}

// DeactivateVolumeGroup deactivates (but does not destroy) volumeGroup
// (vgchange -an).
func (m *Manager) DeactivateVolumeGroup(ctx context.Context, volumeGroup VolumeGroup) error {
	if _, err := m.exec(ctx, nil, "vgchange", "-an", volumeGroup.Name); err != nil {
		return fmt.Errorf("deactivate VG %s: %w", volumeGroup.Name, err)
	}
	return nil
}

// RemoveVolumeGroup deactivates and removes volumeGroup, destroying its data
// (vgremove -f).
func (m *Manager) RemoveVolumeGroup(ctx context.Context, volumeGroup VolumeGroup) error {
	if _, err := m.exec(ctx, nil, "vgremove", "-f", volumeGroup.Name); err != nil {
		return fmt.Errorf("remove VG %s: %w", volumeGroup.Name, err)
	}
	return nil
}

// RemoveLogicalVolume removes logicalVolume and the data in it (lvremove).
//
// Convergent: a logical volume that is already gone is the state the caller asked
// for, which a deletion resuming after a crash depends on.
//
// --yes answers the confirmation prompt, and is not --force. A logical volume
// something still has open is one the kernel refuses to release, and that refusal
// is what stands between a mis-ordered teardown and a pod losing its filesystem
// underneath it, so it is returned rather than overridden.
func (m *Manager) RemoveLogicalVolume(ctx context.Context, logicalVolume LogicalVolume) error {
	path := logicalVolume.VolumeGroup.Name + "/" + logicalVolume.Name
	if _, err := m.exec(ctx, nil, "lvremove", "--yes", path); err != nil {
		if isNoSuchLogicalVolume(err) {
			return nil
		}
		return fmt.Errorf("lvremove %s: %w", path, err)
	}
	return nil
}

// isNoSuchLogicalVolume reports whether err is lvremove's own "there was nothing
// here" failure, which reads: Failed to find logical volume <vg>/<lv>.
func isNoSuchLogicalVolume(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "failed to find logical volume")
}

// lvAttrStateIndex is the position of the state character in lv_attr, LVM's
// ten-character attribute string. Character 5 is the state, where "a" is active.
const lvAttrStateIndex = 4

// LogicalVolumeActive reports whether logicalVolume is currently mapped on this
// host, which is what tells a complete volume group apart from one merely not
// activated here. The distinction decides between reactivating a volume and
// creating one, so it is not a question to answer by guessing.
//
// An attribute string too short to carry a state character is an error rather
// than a reading of "not active": folding it into false would have a caller
// activate a volume group whose real state it never learned.
func (m *Manager) LogicalVolumeActive(ctx context.Context, logicalVolume LogicalVolume) (bool, error) {
	path := logicalVolume.VolumeGroup.Name + "/" + logicalVolume.Name
	out, err := m.exec(ctx, nil, "lvs", "--noheadings", "-o", "lv_attr", path)
	if err != nil {
		return false, fmt.Errorf("read attributes of %s: %w", path, err)
	}
	attr := strings.TrimSpace(out)
	if len(attr) <= lvAttrStateIndex {
		return false, fmt.Errorf("attributes of %s read %q, which carries no state character", path, attr)
	}
	return attr[lvAttrStateIndex] == 'a', nil
}

// CreateLogicalVolume creates a logical volume named logicalVolume in
// volumeGroup, sized to consume the volume group's full free space, targeting
// volumeGroup/poolName (lvcreate), and returns its identity.
//
// poolName is not a physical volume, despite an earlier version of this
// function naming the parameter that way. lvcreate's <vg>/<pool> form names
// the pool a new logical volume is created inside, which only a pool-based
// VolumeProvisioning handler needs: VDO's handler creates both the pool and
// the logical volume in this one lvcreate call, via poolName.
//
// Internally, depending on the LogicalVolumeDefinition, the function may
// delegate parts of the creation process to a VolumeProvisioning handler.
func (m *Manager) CreateLogicalVolume(
	ctx context.Context, volumeGroup VolumeGroup, poolName, logicalVolume string, def LogicalVolumeDefinition,
) (LogicalVolume, error) {
	args := []string{"lvcreate", "-n", logicalVolume, "-l", "100%FREE"}
	args = append(args, stripeArgs(def)...)
	args = append(args, createTarget(volumeGroup, poolName), "--yes")

	for _, handler := range volumeProvisioning {
		if !handler.Handles(def) {
			continue
		}
		if additionalArgs := handler.CreateVolumeArgs(def); len(additionalArgs) > 0 {
			args = append(args, additionalArgs...)
		}
		break
	}
	if _, err := m.exec(ctx, nil, args...); err != nil {
		return LogicalVolume{}, fmt.Errorf("lvcreate %s/%s: %w", volumeGroup.Name, logicalVolume, err)
	}
	return LogicalVolume{VolumeGroup: volumeGroup, Name: logicalVolume}, nil
}

// createTarget is what lvcreate is pointed at.
//
// The <vg>/<pool> form names the pool a new logical volume is created inside,
// which only a pool-based type needs; VDO's handler creates the pool and the
// logical volume in one call and reaches it this way. A linear or striped volume
// has no pool, and "<vg>/" is not a target lvcreate accepts, so it is pointed at
// the volume group itself.
func createTarget(volumeGroup VolumeGroup, poolName string) string {
	if poolName == "" {
		return volumeGroup.Name
	}
	return volumeGroup.Name + "/" + poolName
}

// stripeArgs are the striping lvcreate needs, and nothing when the definition
// describes a linear volume.
//
// One stripe is not a stripe: lvcreate -i 1 is a linear volume spelled the long
// way, and a chunk size with no stripe count has nothing to apply to, so both
// degenerate cases pass nothing rather than an argument LVM has to interpret.
func stripeArgs(def LogicalVolumeDefinition) []string {
	if def.Stripes < 2 {
		return nil
	}
	args := []string{"-i", strconv.Itoa(def.Stripes)}
	if def.StripeChunkBytes > 0 {
		// LVM takes the chunk in kilobytes with its own suffix, and rejects a
		// plain byte count.
		args = append(args, "-I", strconv.FormatInt(def.StripeChunkBytes/1024, 10)+"k")
	}
	return args
}

// LogicalVolumeSize returns logicalVolume's current size, in bytes.
func (m *Manager) LogicalVolumeSize(ctx context.Context, logicalVolume LogicalVolume) (int64, error) {
	path := logicalVolume.VolumeGroup.Name + "/" + logicalVolume.Name
	out, err := m.exec(ctx, nil, "lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", path)
	if err != nil {
		return 0, fmt.Errorf("read size of %s: %w", path, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size of %s from %q: %w", path, out, err)
	}
	return size, nil
}

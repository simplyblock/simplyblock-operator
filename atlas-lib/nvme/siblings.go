package nvme

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/errs"
)

// This file answers the two "what else is attached alongside this device"
// questions a teardown has to ask, each in two forms:
//
//	Siblings / HasSiblings    other block devices backing the *same* volume
//	CoTenants / HasCoTenants  *other* volumes sharing the same subsystem
//
// The package-level functions are pure filters over a device snapshot the
// caller owns, the cheap form, since one List answers all four questions for
// every device in it. The Device methods are the convenience form: they re-scan
// through the resolver the device came from, so the answer reflects current
// kernel state rather than the snapshot the device was taken from.

// Siblings returns the devices in all that back the same logical volume as
// d, those sharing d's namespace UUID, excluding d itself.
//
// Simplyblock identifies a volume by its namespace UUID (the lvol UUID). When
// a volume is reachable as more than one block device (native NVMe multipath
// is disabled and each path exposes its own /dev/nvmeXnY, say), those
// block devices all carry that UUID and are siblings. With native multipath a
// volume has a single multipath head, so Siblings returns nothing.
//
// d need not appear in all. A device with no namespace UUID has no determinable
// identity, so Siblings returns nil for it.
func Siblings(d Device, all []Device) []Device {
	if d.Namespace.UUID == "" {
		return nil
	}
	var sibs []Device
	for _, o := range all {
		if IsSibling(d, o) {
			sibs = append(sibs, o)
		}
	}
	return sibs
}

// HasSiblings reports whether all holds another block device backing the same
// volume as d. It is the question-only form of Siblings: it stops at the first
// match and builds no list.
func HasSiblings(d Device, all []Device) bool {
	if d.Namespace.UUID == "" {
		return false
	}
	for _, o := range all {
		if IsSibling(d, o) {
			return true
		}
	}
	return false
}

// CoTenants returns the volumes in all that share d's subsystem: the other
// namespaces of a multi-namespace subsystem (simplyblock's "namespaced" lvols),
// excluding d's own namespace. It is empty for a single-namespace subsystem.
//
// Unlike Siblings (the same volume via different paths, keyed by UUID),
// CoTenants are *different* volumes that merely coexist on one subsystem. They
// share its controllers, so disconnecting the subsystem (writing any
// controller's delete_controller, say) tears every co-tenant down together.
// Check for them before doing so.
//
// A device with no subsystem NQN has no determinable subsystem, so CoTenants
// returns nil for it.
func CoTenants(d Device, all []Device) []Device {
	if d.Subsystem.NQN == "" {
		return nil
	}
	var out []Device
	seen := make(map[NamespaceID]bool, len(all))
	for _, o := range all {
		// One entry per co-tenant volume: without a multipath head a namespace
		// appears once per controller, and those repeats are the same volume,
		// not another tenant.
		if !IsCoTenant(d, o) || seen[o.Namespace.ID] {
			continue
		}
		seen[o.Namespace.ID] = true
		out = append(out, o)
	}
	return out
}

// HasCoTenants reports whether all holds another volume on d's subsystem, the
// question a teardown asks, since disconnecting the subsystem takes every
// co-tenant down with it, so a device that has any must not be disconnected on
// its own. It is the question-only form of CoTenants.
func HasCoTenants(d Device, all []Device) bool {
	if d.Subsystem.NQN == "" {
		return false
	}
	for _, o := range all {
		if IsCoTenant(d, o) {
			return true
		}
	}
	return false
}

// IsSibling reports whether o is another block device for d's volume: same
// namespace UUID, but a distinct namespace entry (identity is the sysfs path,
// which is unique per attached namespace).
func IsSibling(d, o Device) bool {
	return o.Namespace.UUID == d.Namespace.UUID && o.Namespace.SysfsPath != d.Namespace.SysfsPath
}

// IsCoTenant reports whether o is a different volume on d's subsystem. The
// subsystem is keyed by NQN, not by the kernel-assigned id, so the answer
// survives a rescan.
func IsCoTenant(d, o Device) bool {
	return o.Subsystem.NQN == d.Subsystem.NQN && o.Namespace.ID != d.Namespace.ID
}

// Siblings returns the other block devices backing the same volume as d. It is
// the convenience form of the package-level Siblings: it re-scans through the
// resolver d was resolved by, so the answer reflects current kernel state
// rather than the snapshot d was taken from, and filters that. A caller that already holds
// a snapshot, or asks about several devices, should call Siblings directly.
//
// It returns errs.ErrUnsupported for a device carrying no resolver, one
// assembled by hand rather than resolved. Bind it with WithResolver. A device
// with no namespace UUID yields nil without scanning.
func (d Device) Siblings(ctx context.Context) ([]Device, error) {
	all, err := d.sameVolume(ctx)
	if err != nil {
		return nil, err
	}
	return Siblings(d, all), nil
}

// HasSiblings reports whether another block device backs the same volume as d,
// re-scanning as Device.Siblings does and failing the same way.
func (d Device) HasSiblings(ctx context.Context) (bool, error) {
	all, err := d.sameVolume(ctx)
	if err != nil {
		return false, err
	}
	return HasSiblings(d, all), nil
}

// IsSibling reports whether o is another block device backing the same volume as
// d. It is the method form of the package-level IsSibling, for a call site that
// reads as a question about d. Both are pure, comparing the two snapshots.
func (d Device) IsSibling(o Device) bool {
	return IsSibling(d, o)
}

// CoTenants returns the other volumes sharing d's subsystem, re-scanning
// through the resolver d was resolved by. A teardown wants the current answer,
// not the one from scan time: a namespace attached since would be torn down
// with the subsystem without ever appearing in d's snapshot.
//
// It fails the same way Device.Siblings does. A device with no subsystem NQN
// yields nil without scanning.
func (d Device) CoTenants(ctx context.Context) ([]Device, error) {
	all, err := d.sameSubsystem(ctx)
	if err != nil {
		return nil, err
	}
	return CoTenants(d, all), nil
}

// HasCoTenants reports whether other volumes share d's subsystem, re-scanning
// as Device.CoTenants does and failing the same way.
func (d Device) HasCoTenants(ctx context.Context) (bool, error) {
	all, err := d.sameSubsystem(ctx)
	if err != nil {
		return false, err
	}
	return HasCoTenants(d, all), nil
}

// IsCoTenant reports whether o is a *different* volume sharing d's subsystem,
// the relation that forbids disconnecting the subsystem for d alone. It is the
// method form of the package-level IsCoTenant. Both are pure, comparing the
// two snapshots.
func (d Device) IsCoTenant(o Device) bool {
	return IsCoTenant(d, o)
}

// sameVolume re-scans for every device carrying d's namespace UUID, d included.
func (d Device) sameVolume(ctx context.Context) ([]Device, error) {
	if d.Namespace.UUID == "" {
		return nil, nil
	}
	return d.rescan(ctx, DeviceSelector{UUID: d.Namespace.UUID})
}

// sameSubsystem re-scans for every device on d's subsystem, d included.
func (d Device) sameSubsystem(ctx context.Context) ([]Device, error) {
	if d.Subsystem.NQN == "" {
		return nil, nil
	}
	return d.rescan(ctx, DeviceSelector{NQN: d.Subsystem.NQN})
}

// rescan re-resolves through the resolver d came from. An identity-less device
// never gets here: its callers above return no devices rather than an error,
// since nothing can be said about it and every one of them reads that as
// "nothing alongside."
func (d Device) rescan(ctx context.Context, sel DeviceSelector) ([]Device, error) {
	if d.resolver == nil {
		return nil, fmt.Errorf("device %s: not bound to a resolver: %w",
			d.Namespace.Name, errs.ErrUnsupported)
	}
	return d.resolver.ListWithSelector(ctx, sel)
}

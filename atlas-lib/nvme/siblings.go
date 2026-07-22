package nvme

import "context"

// Siblings returns the devices in all that back the same logical volume as
// d — those sharing d's namespace UUID — excluding d itself.
//
// Simplyblock identifies a volume by its namespace UUID (the lvol UUID). When
// a volume is reachable as more than one block device — e.g. native NVMe
// multipath is disabled and each path exposes its own /dev/nvmeXnY — those
// block devices all carry that UUID and are siblings. With native multipath a
// volume has a single multipath head, so Siblings returns nothing.
//
// Device need not appear in all. A device with no namespace UUID has no
// determinable identity, so Siblings returns nil for it.
func (d Device) Siblings(all []Device) []Device {
	if d.Namespace.UUID == "" {
		return nil
	}
	var sibs []Device
	for _, o := range all {
		// Same volume, but a distinct namespace entry (identity is the
		// sysfs path, which is unique per attached namespace).
		if o.Namespace.UUID == d.Namespace.UUID && o.Namespace.SysfsPath != d.Namespace.SysfsPath {
			sibs = append(sibs, o)
		}
	}
	return sibs
}

// SiblingsVia is a convenience over SiblingsIn: it re-scans through r to get a
// current, coherent device snapshot and returns d's siblings from it. Use it
// when you hold a Device and a resolver but not a device list.
func SiblingsVia(ctx context.Context, r DeviceResolver, d Device) ([]Device, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	return d.Siblings(all), nil
}

// CoTenants returns the other volumes sharing this device's subsystem — the
// distinct namespaces of a multi-namespace subsystem (simplyblock's
// "namespaced" lvols), excluding this device's own namespace. It is empty for
// a single-namespace subsystem.
//
// Unlike Siblings (the same volume via different paths, keyed by UUID),
// CoTenants are *different* volumes that merely coexist on one subsystem. They
// share its controllers, so disconnecting the subsystem — e.g. writing any
// controller's delete_controller — tears every co-tenant down together; check
// CoTenants before doing so. The result is drawn from the subsystem snapshot
// the device already carries, so it needs no rescan.
func (d Device) CoTenants() []Device {
	var out []Device
	for _, ns := range d.Subsystem.Namespaces {
		if ns.ID == d.Namespace.ID {
			continue
		}
		out = append(out, Device{Namespace: ns, Subsystem: d.Subsystem})
	}
	return out
}

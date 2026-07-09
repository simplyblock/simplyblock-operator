package nvme

import "context"

// SubsystemResolver enumerates and finds NVMe subsystems — the unit a
// logical volume maps to (one subsystem, its controller paths and
// namespaces). It is an interface so callers can fake it in tests without
// touching /sys.
type SubsystemResolver interface {
	// List returns every attached NVMe subsystem.
	List(ctx context.Context) ([]Subsystem, error)
	// ByNQN returns the subsystem with the given NQN, including all of its
	// controller paths and namespaces.
	ByNQN(ctx context.Context, nqn string) (Subsystem, error)
}

// DeviceResolver enumerates and finds attachable namespace devices
// (a namespace together with its subsystem). It is an interface so callers
// can fake it in tests without touching /sys.
type DeviceResolver interface {
	// List returns every attached NVMe device.
	List(ctx context.Context) ([]Device, error)
	// ByUUID returns the device whose namespace UUID matches
	// (simplyblock: the lvol UUID).
	ByUUID(ctx context.Context, uuid string) (Device, error)
	// ByDevicePath returns the device for a block node such as
	// "/dev/nvme0n1" (the subsystem multipath head).
	ByDevicePath(ctx context.Context, devicePath string) (Device, error)
	// ByNamespace returns the device identified by its subsystem NQN and
	// namespace id (NSID) — the precise coordinates of one namespace when a
	// subsystem exports several.
	ByNamespace(ctx context.Context, nqn string, nsid NamespaceID) (Device, error)
}

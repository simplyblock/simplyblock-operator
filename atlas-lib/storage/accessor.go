package storage

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// Accessor is everything one node can be asked about its storage, in one place.
//
// A caller reaches a node either entirely locally — it is running on it — or
// entirely over a link. There is no arrangement where the subsystems come from
// sysfs and the devices from somewhere else, so carrying the resolvers around
// separately only creates the possibility of mismatching them, and makes every
// function needing both take two parameters that have to agree. [Local] and
// storagerpc.Remote are the two ways to fill one in; after that, the code using
// it neither knows nor cares which it has.
//
// It is a struct rather than an interface because there is one implementation
// and there always will be: the variation is in what goes *in* it, not in the
// bundle. That is also what lets the re-scanning questions below live here —
// on an interface they would be an obligation on every implementer, when what
// they want is one shared implementation over whatever resolvers are present.
// A test composes one directly, storage.Accessor{DeviceResolver: fake}, and fakes the
// resolver, which is the seam worth faking.
//
// Further facets belong here as further fields as they are remoted: fabric
// connect/disconnect is the next one. Note when adding one that the remote side
// is capability-negotiated — a peer may serve devices and not fabrics — so a
// new field needs an answer for "this peer does not have it" beyond a non-nil
// value that fails on every call.
type Accessor struct {
	// SubsystemResolver resolves NVMe subsystems on the node.
	SubsystemResolver nvme.SubsystemResolver
	// DeviceResolver resolves attachable namespace devices on the node.
	DeviceResolver nvme.DeviceResolver
}

// Local is the storage of the node this process runs on, read through sysfs.
// The zero [nvme.SysfsConfig] uses the conventional /sys and /dev; override its
// roots to point at a fixture tree.
//
// This is what a CSI node plugin uses for its own work, and what it serves to
// the operator (see storagerpc.NewServer). Its counterpart is
// storagerpc.Remote, which fills in the same struct with clients that reach
// another node over a link; it lives in the subpackage so this one carries no
// gRPC.
func Local(cfg nvme.SysfsConfig) Accessor {
	return Accessor{
		SubsystemResolver: nvme.NewSysfsSubsystemResolver(cfg),
		DeviceResolver:    nvme.NewSysfsDeviceResolver(cfg),
	}
}

// Every lookup the two resolvers offer, flat on the accessor.
//
// Each name says what it returns, because on a bundle spanning both resolvers
// the return kind is the thing a bare ByUUID/ByNQN would leave the reader to
// guess. It is the same scheme the RPCs use, so a method here and the call it
// makes on the wire line up by name.
//
// The fields stay exported and reaching through them is equally correct;
// s.DeviceResolver is what you pass to something that wants an
// [nvme.DeviceResolver]. What these methods add is that a missing facet is
// reported rather than panicked on, uniformly.

// ListSubsystems returns every NVMe subsystem attached on the node.
func (s Accessor) ListSubsystems(ctx context.Context) ([]nvme.Subsystem, error) {
	subsystems, err := s.subsystems()
	if err != nil {
		return nil, err
	}
	return subsystems.List(ctx)
}

// SubsystemByNQN returns the subsystem with the given NQN, including all of its
// controller paths and namespaces. It reports errs.ErrNotFound when the node
// has no such subsystem attached.
func (s Accessor) SubsystemByNQN(ctx context.Context, nqn string) (nvme.Subsystem, error) {
	subsystems, err := s.subsystems()
	if err != nil {
		return nvme.Subsystem{}, err
	}
	return subsystems.ByNQN(ctx, nqn)
}

// ListDevices returns every NVMe device attached on the node.
func (s Accessor) ListDevices(ctx context.Context) ([]nvme.Device, error) {
	devices, err := s.devices()
	if err != nil {
		return nil, err
	}
	return devices.List(ctx)
}

// ListDevicesBySelector returns every device matching sel, in the same order
// ListDevices would. It hands back all matches rather than a winner — a
// selector satisfied by more than one device is something the caller has to
// see — and no match is an empty slice, not an error.
func (s Accessor) ListDevicesBySelector(ctx context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	devices, err := s.devices()
	if err != nil {
		return nil, err
	}
	return devices.ListWithSelector(ctx, sel)
}

// DeviceByUUID returns the device whose namespace UUID matches (simplyblock:
// the lvol UUID). It reports errs.ErrNotFound when nothing matches.
func (s Accessor) DeviceByUUID(ctx context.Context, uuid string) (nvme.Device, error) {
	devices, err := s.devices()
	if err != nil {
		return nvme.Device{}, err
	}
	return devices.ByUUID(ctx, uuid)
}

// DeviceByPath returns the device for a block node such as "/dev/nvme0n1" (the
// subsystem multipath head). It reports errs.ErrNotFound when nothing matches.
func (s Accessor) DeviceByPath(ctx context.Context, devicePath string) (nvme.Device, error) {
	devices, err := s.devices()
	if err != nil {
		return nvme.Device{}, err
	}
	return devices.ByDevicePath(ctx, devicePath)
}

// DeviceByNamespace returns the device identified by its subsystem NQN and
// namespace id — the precise coordinates of one namespace when a subsystem
// exports several. It reports errs.ErrNotFound when nothing matches.
func (s Accessor) DeviceByNamespace(ctx context.Context, nqn string, nsid nvme.NamespaceID) (nvme.Device, error) {
	devices, err := s.devices()
	if err != nil {
		return nvme.Device{}, err
	}
	return devices.ByNamespace(ctx, nqn, nsid)
}

// The four questions a teardown has to ask about what else is attached
// alongside a device, answered against current kernel state.
//
// [nvme] has these as pure filters over a snapshot the caller already holds,
// and that is the cheap form — one List answers all four for every device in
// it. These re-scan, which is what a teardown needs: a namespace attached since
// the snapshot was taken would never appear in it, and it is precisely the one
// that makes a disconnect destructive. They live here rather than on
// [nvme.Device] because a rescan needs a resolver, and a value that carries one
// hides the cost of asking — over a link, each of these is a round trip.

// Siblings returns the other block devices backing the same volume as d: those
// sharing its namespace UUID, excluding d itself. It is empty under native
// multipath, where a volume has a single head.
//
// A device with no namespace UUID has no determinable identity, so this yields
// nil without scanning.
func (s Accessor) Siblings(ctx context.Context, d nvme.Device) ([]nvme.Device, error) {
	all, err := s.sameVolume(ctx, d)
	if err != nil {
		return nil, err
	}
	return nvme.Siblings(d, all), nil
}

// HasSiblings reports whether another block device backs the same volume as d.
// It is the question-only form of [Accessor.Siblings] and costs the same scan.
func (s Accessor) HasSiblings(ctx context.Context, d nvme.Device) (bool, error) {
	all, err := s.sameVolume(ctx, d)
	if err != nil {
		return false, err
	}
	return nvme.HasSiblings(d, all), nil
}

// CoTenants returns the other volumes sharing d's subsystem — the namespaces of
// a multi-namespace subsystem. They share its controllers, so disconnecting the
// subsystem tears every co-tenant down with it; check for them before doing so.
//
// A device with no subsystem NQN yields nil without scanning.
func (s Accessor) CoTenants(ctx context.Context, d nvme.Device) ([]nvme.Device, error) {
	all, err := s.sameSubsystem(ctx, d)
	if err != nil {
		return nil, err
	}
	return nvme.CoTenants(d, all), nil
}

// HasCoTenants reports whether other volumes share d's subsystem — the question
// a teardown asks, since a device that has any must not be disconnected on its
// own. It is the question-only form of [Accessor.CoTenants].
func (s Accessor) HasCoTenants(ctx context.Context, d nvme.Device) (bool, error) {
	all, err := s.sameSubsystem(ctx, d)
	if err != nil {
		return false, err
	}
	return nvme.HasCoTenants(d, all), nil
}

// sameVolume re-scans for every device carrying d's namespace UUID, d included.
func (s Accessor) sameVolume(ctx context.Context, d nvme.Device) ([]nvme.Device, error) {
	if d.Namespace.UUID == "" {
		return nil, nil
	}
	return s.rescan(ctx, nvme.DeviceSelector{UUID: d.Namespace.UUID})
}

// sameSubsystem re-scans for every device on d's subsystem, d included.
func (s Accessor) sameSubsystem(ctx context.Context, d nvme.Device) ([]nvme.Device, error) {
	if d.Subsystem.NQN == "" {
		return nil, nil
	}
	return s.rescan(ctx, nvme.DeviceSelector{NQN: d.Subsystem.NQN})
}

// rescan asks the device resolver.
func (s Accessor) rescan(ctx context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	devices, err := s.devices()
	if err != nil {
		return nil, err
	}
	return devices.ListWithSelector(ctx, sel)
}

func (s Accessor) subsystems() (nvme.SubsystemResolver, error) {
	if s.SubsystemResolver == nil {
		return nil, fmt.Errorf("storage has no subsystem resolver: %w", errs.ErrUnsupported)
	}
	return s.SubsystemResolver, nil
}

func (s Accessor) devices() (nvme.DeviceResolver, error) {
	if s.DeviceResolver == nil {
		return nil, fmt.Errorf("storage has no device resolver: %w", errs.ErrUnsupported)
	}
	return s.DeviceResolver, nil
}

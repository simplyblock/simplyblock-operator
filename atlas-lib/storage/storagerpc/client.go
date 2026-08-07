package storagerpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"
	"github.com/simplyblock/atlas/storage/storagerpc/storagev1"
)

// Standing in for the local resolvers is the point of this package, so the
// substitution is asserted here rather than left to fail at a call site.
var (
	_ nvme.SubsystemResolver = (*SubsystemResolver)(nil)
	_ nvme.DeviceResolver    = (*DeviceResolver)(nil)
)

// Remote is the storage of the node at the other end of conn — a peer's
// connection from a link registry:
//
//	conn, err := hub.Registry().Conn(link.NodePeer(nodeName))
//	if errors.Is(err, link.ErrNoSession) {
//	    return ctrl.Result{RequeueAfter: backoff}, nil  // not a failure
//	}
//	dev, err := storagerpc.Remote(conn).Devices.ByUUID(ctx, lvolUUID)
//
// It is the counterpart of [storage.Local], filling in the same struct. Every call
// on it is a round trip; see the package documentation for what that means for
// a caller wanting several answers about one node.
func Remote(conn grpc.ClientConnInterface) storage.Accessor {
	return storage.Accessor{
		SubsystemResolver: NewSubsystemResolver(conn),
		DeviceResolver:    NewDeviceResolver(conn),
	}
}

// SubsystemResolver resolves NVMe subsystems on a remote node. It implements
// [nvme.SubsystemResolver], so code written against the local sysfs resolver
// works unchanged against a node across a link.
type SubsystemResolver struct {
	client storagev1.SubsystemServiceClient
}

// NewSubsystemResolver returns a subsystem resolver backed by conn — a peer's
// connection from a link registry:
//
//	conn, err := hub.Registry().Conn(link.NodePeer(nodeName))
//	subs := storagerpc.NewSubsystemResolver(conn)
func NewSubsystemResolver(conn grpc.ClientConnInterface) *SubsystemResolver {
	return &SubsystemResolver{client: storagev1.NewSubsystemServiceClient(conn)}
}

// List returns every NVMe subsystem attached on the node.
func (r *SubsystemResolver) List(ctx context.Context) ([]nvme.Subsystem, error) {
	resp, err := r.client.ListSubsystems(ctx, &storagev1.ListSubsystemsRequest{})
	if err != nil {
		return nil, fmt.Errorf("node: list subsystems: %w", class.FromStatus(err))
	}

	subsystems := resp.GetSubsystems()
	out := make([]nvme.Subsystem, len(subsystems))
	for i, sub := range subsystems {
		out[i] = subsystemFromProto(sub)
	}
	return out, nil
}

// ByNQN returns the subsystem with the given NQN, including all of its
// controller paths and namespaces. It reports errs.ErrNotFound when the node
// has no such subsystem attached.
func (r *SubsystemResolver) ByNQN(ctx context.Context, nqn string) (nvme.Subsystem, error) {
	resp, err := r.client.GetSubsystemByNQN(ctx, &storagev1.GetSubsystemByNQNRequest{Nqn: nqn})
	if err != nil {
		return nvme.Subsystem{}, fmt.Errorf("node: subsystem %s: %w", nqn, class.FromStatus(err))
	}
	if resp.GetSubsystem() == nil {
		return nvme.Subsystem{}, fmt.Errorf("node: subsystem %s: server answered with no subsystem", nqn)
	}
	return subsystemFromProto(resp.GetSubsystem()), nil
}

// DeviceResolver resolves NVMe namespace devices on a remote node. It
// implements [nvme.DeviceResolver].
type DeviceResolver struct {
	client storagev1.DeviceServiceClient
}

// NewDeviceResolver returns a device resolver backed by conn — a peer's
// connection from a link registry.
func NewDeviceResolver(conn grpc.ClientConnInterface) *DeviceResolver {
	return &DeviceResolver{client: storagev1.NewDeviceServiceClient(conn)}
}

// List returns every NVMe device attached on the node.
func (r *DeviceResolver) List(ctx context.Context) ([]nvme.Device, error) {
	resp, err := r.client.ListDevices(ctx, &storagev1.ListDevicesRequest{})
	if err != nil {
		return nil, fmt.Errorf("node: list devices: %w", class.FromStatus(err))
	}
	return r.devices(resp.GetDevices()), nil
}

// ListWithSelector returns every device matching sel, in the same order List
// would. No match is an empty slice, not an error.
func (r *DeviceResolver) ListWithSelector(
	ctx context.Context, sel nvme.DeviceSelector,
) ([]nvme.Device, error) {
	resp, err := r.client.ListDevicesBySelector(ctx, &storagev1.ListDevicesBySelectorRequest{
		Selector: selectorToProto(sel),
	})
	if err != nil {
		return nil, fmt.Errorf("node: list devices %s: %w", sel, class.FromStatus(err))
	}
	return r.devices(resp.GetDevices()), nil
}

// ByUUID returns the device whose namespace UUID matches (simplyblock: the lvol
// UUID). It reports errs.ErrNotFound when nothing matches.
func (r *DeviceResolver) ByUUID(ctx context.Context, uuid string) (nvme.Device, error) {
	resp, err := r.client.GetDeviceByUUID(ctx, &storagev1.GetDeviceByUUIDRequest{Uuid: uuid})
	if err != nil {
		return nvme.Device{}, fmt.Errorf("node: device uuid=%s: %w", uuid, class.FromStatus(err))
	}
	return r.device(resp.GetDevice(), "uuid="+uuid)
}

// ByDevicePath returns the device for a block node such as "/dev/nvme0n1". It
// reports errs.ErrNotFound when nothing matches.
func (r *DeviceResolver) ByDevicePath(ctx context.Context, devicePath string) (nvme.Device, error) {
	resp, err := r.client.GetDeviceByDevicePath(ctx, &storagev1.GetDeviceByDevicePathRequest{
		DevicePath: devicePath,
	})
	if err != nil {
		return nvme.Device{}, fmt.Errorf("node: device %s: %w", devicePath, class.FromStatus(err))
	}
	return r.device(resp.GetDevice(), devicePath)
}

// ByNamespace returns the device identified by its subsystem NQN and namespace
// id. It reports errs.ErrNotFound when nothing matches.
func (r *DeviceResolver) ByNamespace(
	ctx context.Context, nqn string, nsid nvme.NamespaceID,
) (nvme.Device, error) {
	resp, err := r.client.GetDeviceByNamespace(ctx, &storagev1.GetDeviceByNamespaceRequest{
		Nqn:  nqn,
		Nsid: uint32(nsid),
	})
	if err != nil {
		return nvme.Device{}, fmt.Errorf("node: device %s nsid=%d: %w", nqn, nsid, class.FromStatus(err))
	}
	return r.device(resp.GetDevice(), fmt.Sprintf("%s nsid=%d", nqn, nsid))
}

// device decodes one device.
//
// A response carrying no device where the server reported no error is a
// protocol fault, not an absence — the server signals absence with NOT_FOUND —
// so it must not be reported as errs.ErrNotFound, which would read as "the node
// does not have it" and send a caller down a recovery path for a bug.
func (r *DeviceResolver) device(d *storagev1.Device, what string) (nvme.Device, error) {
	if d == nil {
		return nvme.Device{}, fmt.Errorf("node: device %s: server answered with no device", what)
	}
	return deviceFromProto(d), nil
}

// devices decodes a list of devices.
func (r *DeviceResolver) devices(devices []*storagev1.Device) []nvme.Device {
	out := make([]nvme.Device, len(devices))
	for i, d := range devices {
		out[i] = deviceFromProto(d)
	}
	return out
}

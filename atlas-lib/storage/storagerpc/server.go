package storagerpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"
	"github.com/simplyblock/atlas/storage/storagerpc/storagev1"
)

// Capabilities a node peer announces when it serves these services, so the
// operator can tell what a peer answers before it calls and degrade rather than
// collect Unimplemented errors from an older build.
const (
	CapabilitySubsystems = "nvme.subsystems"
	CapabilityDevices    = "nvme.devices"
)

// Capabilities is what [NewServer] serves, for an agent's Hello.
func Capabilities() []string { return []string{CapabilitySubsystems, CapabilityDevices} }

// Server answers NVMe lookups against the node it runs on.
//
// It is a thin adapter, and deliberately so: every method is one call into the
// resolver underneath plus a conversion. The rules about what a lookup means —
// that a missing subsystem is ErrNotFound, that a selector returns all matches
// rather than a winner — live in the resolvers and are inherited here rather
// than restated, which is what keeps the remote answers identical to the local
// ones.
//
// Serve the storage of the node this runs on:
//
//	srv, err := storagerpc.NewServer(storage.Local(nvme.SysfsConfig{}))
//	// then, as the link agent's Register hook:
//	agent, err := link.NewAgent(link.AgentConfig{Register: srv.Register, ...})
type Server struct {
	storagev1.UnimplementedSubsystemServiceServer
	storagev1.UnimplementedDeviceServiceServer

	subs nvme.SubsystemResolver
	devs nvme.DeviceResolver
}

// NewServer serves s, which is normally [Local] — serving a [Remote] would make
// this node a proxy for another one, which nothing here needs and which turns
// one round trip into two.
//
// The resolvers are read once here rather than per call: a Storage addresses
// one node for its whole life, so re-asking it on every request would only be
// indirection.
func NewServer(s storage.Accessor) (*Server, error) {
	subs, devs := s.SubsystemResolver, s.DeviceResolver
	if subs == nil {
		return nil, fmt.Errorf("node server: storage has no subsystem resolver: %w", errs.ErrUnsupported)
	}
	if devs == nil {
		return nil, fmt.Errorf("node server: storage has no device resolver: %w", errs.ErrUnsupported)
	}
	return &Server{subs: subs, devs: devs}, nil
}

// Register adds both services to a gRPC server — what a link agent does with
// the server it is handed for its side of the session.
func (s *Server) Register(srv grpc.ServiceRegistrar) {
	storagev1.RegisterSubsystemServiceServer(srv, s)
	storagev1.RegisterDeviceServiceServer(srv, s)
}

// ListSubsystems returns every attached NVMe subsystem.
func (s *Server) ListSubsystems(
	ctx context.Context, _ *storagev1.ListSubsystemsRequest,
) (*storagev1.ListSubsystemsResponse, error) {
	subsystems, err := s.subs.List(ctx)
	if err != nil {
		return nil, class.Status(err)
	}

	out := make([]*storagev1.Subsystem, len(subsystems))
	for i, sub := range subsystems {
		out[i] = subsystemToProto(sub)
	}
	return &storagev1.ListSubsystemsResponse{Subsystems: out}, nil
}

// GetSubsystemByNQN returns one subsystem with its controllers and namespaces.
func (s *Server) GetSubsystemByNQN(
	ctx context.Context, req *storagev1.GetSubsystemByNQNRequest,
) (*storagev1.GetSubsystemByNQNResponse, error) {
	sub, err := s.subs.ByNQN(ctx, req.GetNqn())
	if err != nil {
		return nil, class.Status(err)
	}
	return &storagev1.GetSubsystemByNQNResponse{Subsystem: subsystemToProto(sub)}, nil
}

// ListDevices returns every attached NVMe device.
func (s *Server) ListDevices(
	ctx context.Context, _ *storagev1.ListDevicesRequest,
) (*storagev1.ListDevicesResponse, error) {
	devices, err := s.devs.List(ctx)
	if err != nil {
		return nil, class.Status(err)
	}
	return &storagev1.ListDevicesResponse{Devices: devicesToProto(devices)}, nil
}

// ListDevicesBySelector returns every device matching the selector.
func (s *Server) ListDevicesBySelector(
	ctx context.Context, req *storagev1.ListDevicesBySelectorRequest,
) (*storagev1.ListDevicesBySelectorResponse, error) {
	devices, err := s.devs.ListWithSelector(ctx, selectorFromProto(req.GetSelector()))
	if err != nil {
		return nil, class.Status(err)
	}
	return &storagev1.ListDevicesBySelectorResponse{Devices: devicesToProto(devices)}, nil
}

// GetDeviceByUUID returns the device whose namespace UUID matches.
func (s *Server) GetDeviceByUUID(
	ctx context.Context, req *storagev1.GetDeviceByUUIDRequest,
) (*storagev1.GetDeviceByUUIDResponse, error) {
	device, err := s.devs.ByUUID(ctx, req.GetUuid())
	if err != nil {
		return nil, class.Status(err)
	}
	return &storagev1.GetDeviceByUUIDResponse{Device: deviceToProto(device)}, nil
}

// GetDeviceByDevicePath returns the device for a block device node.
func (s *Server) GetDeviceByDevicePath(
	ctx context.Context, req *storagev1.GetDeviceByDevicePathRequest,
) (*storagev1.GetDeviceByDevicePathResponse, error) {
	device, err := s.devs.ByDevicePath(ctx, req.GetDevicePath())
	if err != nil {
		return nil, class.Status(err)
	}
	return &storagev1.GetDeviceByDevicePathResponse{Device: deviceToProto(device)}, nil
}

// GetDeviceByNamespace returns the device for one namespace of a subsystem.
func (s *Server) GetDeviceByNamespace(
	ctx context.Context, req *storagev1.GetDeviceByNamespaceRequest,
) (*storagev1.GetDeviceByNamespaceResponse, error) {
	device, err := s.devs.ByNamespace(ctx, req.GetNqn(), nvme.NamespaceID(req.GetNsid()))
	if err != nil {
		return nil, class.Status(err)
	}
	return &storagev1.GetDeviceByNamespaceResponse{Device: deviceToProto(device)}, nil
}

func devicesToProto(devices []nvme.Device) []*storagev1.Device {
	out := make([]*storagev1.Device, len(devices))
	for i, d := range devices {
		out[i] = deviceToProto(d)
	}
	return out
}

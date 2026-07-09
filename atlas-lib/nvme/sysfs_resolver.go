package nvme

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/sysfs"
)

// SysfsConfig configures the sysfs-backed resolvers. The zero value is
// valid and uses the conventional /sys and /dev locations; override the
// roots to point the resolvers at a fixture tree in tests.
type SysfsConfig struct {
	SysRoot string // sysfs mount point; default "/sys"
	DevRoot string // device-node directory; default "/dev"
}

func (c SysfsConfig) sysRoot() string {
	if c.SysRoot == "" {
		return sysfs.DefaultMount
	}
	return c.SysRoot
}

func (c SysfsConfig) devRoot() string {
	if c.DevRoot == "" {
		return sysfs.DefaultDev
	}
	return c.DevRoot
}

// SysfsSubsystemResolver implements SubsystemResolver by reading the local
// Linux sysfs hierarchy. Remote/over-the-network resolution is a separate
// implementation. Each call re-scans, so results reflect current kernel
// state.
type SysfsSubsystemResolver struct {
	cfg SysfsConfig
}

var _ SubsystemResolver = (*SysfsSubsystemResolver)(nil)

// NewSysfsSubsystemResolver returns a SubsystemResolver backed by local sysfs.
func NewSysfsSubsystemResolver(cfg SysfsConfig) *SysfsSubsystemResolver {
	return &SysfsSubsystemResolver{cfg: cfg}
}

func (r *SysfsSubsystemResolver) List(ctx context.Context) ([]Subsystem, error) {
	return scanSubsystems(r.cfg.sysRoot(), r.cfg.devRoot())
}

func (r *SysfsSubsystemResolver) ByNQN(ctx context.Context, nqn string) (Subsystem, error) {
	subs, err := scanSubsystems(r.cfg.sysRoot(), r.cfg.devRoot())
	if err != nil {
		return Subsystem{}, err
	}
	for _, s := range subs {
		if s.NQN == nqn {
			return s, nil
		}
	}
	return Subsystem{}, fmt.Errorf("subsystem nqn %q: %w", nqn, errs.ErrNotFound)
}

// SysfsDeviceResolver implements DeviceResolver by reading the local Linux
// sysfs hierarchy. Remote/over-the-network resolution is a separate
// implementation. Each call re-scans.
type SysfsDeviceResolver struct {
	cfg SysfsConfig
}

var _ DeviceResolver = (*SysfsDeviceResolver)(nil)

// NewSysfsDeviceResolver returns a DeviceResolver backed by local sysfs.
func NewSysfsDeviceResolver(cfg SysfsConfig) *SysfsDeviceResolver {
	return &SysfsDeviceResolver{cfg: cfg}
}

func (r *SysfsDeviceResolver) List(ctx context.Context) ([]Device, error) {
	return scanDevices(r.cfg.sysRoot(), r.cfg.devRoot())
}

func (r *SysfsDeviceResolver) ByUUID(ctx context.Context, uuid string) (Device, error) {
	return r.find(func(d Device) bool { return d.Namespace.UUID == uuid },
		"device uuid %q", uuid)
}

func (r *SysfsDeviceResolver) ByDevicePath(ctx context.Context, devicePath string) (Device, error) {
	return r.find(func(d Device) bool { return d.Namespace.DevicePath == devicePath },
		"device path %q", devicePath)
}

func (r *SysfsDeviceResolver) ByNamespace(ctx context.Context, nqn string, nsid NamespaceID) (Device, error) {
	return r.find(func(d Device) bool { return d.Subsystem.NQN == nqn && d.Namespace.ID == nsid },
		"device nqn %q nsid %d", nqn, nsid)
}

func (r *SysfsDeviceResolver) find(match func(Device) bool, format string, args ...any) (Device, error) {
	devs, err := scanDevices(r.cfg.sysRoot(), r.cfg.devRoot())
	if err != nil {
		return Device{}, err
	}
	for _, d := range devs {
		if match(d) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf(format+": %w", append(args, errs.ErrNotFound)...)
}

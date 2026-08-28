package nvme

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/sysfs"
)

// SysfsConfig configures the sysfs-backed resolvers. The zero value is
// valid and uses the conventional /sys and /dev locations. Override the
// roots to point the resolvers at a fixture tree in tests.
type SysfsConfig struct {
	SysRoot string // sysfs mount point, `/sys` by default
	DevRoot string // device-node directory, `/dev` by default
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
	devs, err := scanDevices(r.cfg.sysRoot(), r.cfg.devRoot())
	if err != nil {
		return nil, err
	}
	// Bind every device to this resolver so a follow-up question that needs a
	// fresh scan (Device.HasSiblings) can ask without being handed a resolver.
	for i := range devs {
		devs[i] = devs[i].WithResolver(r)
	}
	return devs, nil
}

func (r *SysfsDeviceResolver) ListWithSelector(ctx context.Context, sel DeviceSelector) ([]Device, error) {
	devs, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	return sel.Filter(devs), nil
}

func (r *SysfsDeviceResolver) ByUUID(ctx context.Context, uuid string) (Device, error) {
	return r.pick(ctx, DeviceSelector{UUID: uuid})
}

func (r *SysfsDeviceResolver) ByDevicePath(ctx context.Context, devicePath string) (Device, error) {
	return r.pick(ctx, DeviceSelector{DevicePath: devicePath})
}

func (r *SysfsDeviceResolver) ByNamespace(ctx context.Context, nqn string, nsid NamespaceID) (Device, error) {
	return r.pick(ctx, DeviceSelector{NQN: nqn, NSID: nsid})
}

// pick returns the most reachable match for sel, the single-result shape of the
// By* lookups, whose keys are all selector fields. Several matches mean a stale
// subsystem beside a fresh one, or one device per path with native multipath
// off. Ranking beats scan order, which favors the older instance. It is a
// preference only and still returns an unreachable device over none. Callers
// that must not be handed a wrong device judge the whole set through
// ListWithSelector (nvmeof.WaitForDevice does).
func (r *SysfsDeviceResolver) pick(ctx context.Context, sel DeviceSelector) (Device, error) {
	devs, err := r.ListWithSelector(ctx, sel)
	if err != nil {
		return Device{}, err
	}
	if len(devs) == 0 {
		return Device{}, fmt.Errorf("device %s: %w", sel, errs.ErrNotFound)
	}

	best, bestRank := devs[0], devs[0].rank()
	for _, d := range devs[1:] {
		if rk := d.rank(); rk > bestRank {
			best, bestRank = d, rk
		}
	}
	return best, nil
}

// The dmLinear layer: a dm-linear device between the fabric and whatever uses
// the volume, so the device the consumer holds survives a change of the
// namespace behind it.
//
// A volume's namespace can move to another NVMe subsystem (consistency-group
// co-location, sbcli docs/consistency-group-colocation.md §6). The new
// namespace is a different block device: without an indirection the pod's
// raw block device or the mounted filesystem would have to be torn down and
// set up again. With it, the fabric layer below brings up the new namespace,
// and this layer's Heal re-points its mapping there (suspend, load, resume)
// while the device above stays the same node with the same data.
package layers

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/devmapper"
	"github.com/simplyblock/atlas/volstack"
)

// DMMapper is the device-mapper surface the layer uses; *devmapper.Mapper
// satisfies it, and tests substitute a recorder.
type DMMapper interface {
	Table(ctx context.Context, name string) (devmapper.Target, error)
	Sectors(ctx context.Context, device string) (uint64, error)
	Create(ctx context.Context, name, device string, sectors uint64) error
	Swap(ctx context.Context, name, device string, sectors uint64) error
	Remove(ctx context.Context, name string) error
}

// DMLinearConfig is what a dmLinear layer is built with.
type DMLinearConfig struct {
	// Name is the mapping's name, derived from the volume's identity so a
	// restage finds the mapping it made (sb-<lvol uuid>).
	Name string
	// Mapper runs the device-mapper commands.
	Mapper DMMapper
	// Resolve describes the mapping's device node upward; defaults to
	// blockdev.ResolveDevice.
	Resolve DeviceResolver
}

// DMLinear is the indirection layer.
type DMLinear struct {
	cfg DMLinearConfig
	// stale is what the last Observe found: the mapping points somewhere else
	// than the device below now is. The heal loop asks Healthy without the
	// layer below, so the comparison is made where both are known.
	stale bool
}

// NewDMLinear returns the layer.
func NewDMLinear(cfg DMLinearConfig) *DMLinear {
	if cfg.Resolve == nil {
		cfg.Resolve = blockdev.ResolveDevice
	}
	return &DMLinear{cfg: cfg}
}

// Name is what the record calls this layer.
func (d *DMLinear) Name() string { return "dmLinear" }

// DMLinearName is the mapping name of the volume with UUID uuid.
func DMLinearName(uuid string) string { return "sb-" + uuid }

func deviceNumber(dev blockdev.Device) string { return fmt.Sprintf("%d:%d", dev.Major, dev.Minor) }

func belowDevice(below volstack.Artifact) (blockdev.Device, bool) {
	if len(below.Devices) != 1 {
		return blockdev.Device{}, false
	}
	return below.Devices[0], true
}

// Observe reports the mapping: absent, ready when it points at the device
// below, and partial when it points elsewhere (the namespace moved and the
// fabric below brought up the new one) or there is nothing below to point at.
func (d *DMLinear) Observe(ctx context.Context, below volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	target, err := d.cfg.Mapper.Table(ctx, d.cfg.Name)
	if errors.Is(err, devmapper.ErrNotFound) {
		d.stale = false
		return volstack.StateAbsent, volstack.Artifact{}, nil
	}
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, fmt.Errorf("dmLinear: read %s: %w", d.cfg.Name, err)
	}
	own, err := d.own()
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, err
	}
	dev, ok := belowDevice(below)
	d.stale = !ok || target.Device != deviceNumber(dev)
	if d.stale {
		return volstack.StatePartial, own, nil
	}
	return volstack.StateReady, own, nil
}

func (d *DMLinear) own() (volstack.Artifact, error) {
	dev, err := d.cfg.Resolve(devmapper.Path(d.cfg.Name))
	if err != nil {
		return volstack.Artifact{}, fmt.Errorf("dmLinear: resolve %s: %w", devmapper.Path(d.cfg.Name), err)
	}
	return volstack.Artifact{Devices: []blockdev.Device{dev}}, nil
}

// Ensure creates the mapping over the device below, or re-points it there.
func (d *DMLinear) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	state, own, err := d.Observe(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}
	switch state {
	case volstack.StateReady:
		return own, nil
	case volstack.StatePartial:
		if err := d.swap(ctx, below); err != nil {
			return volstack.Artifact{}, err
		}
		return d.own()
	}
	dev, ok := belowDevice(below)
	if !ok {
		return volstack.Artifact{}, fmt.Errorf("dmLinear: %s needs exactly one device below, got %d",
			d.cfg.Name, len(below.Devices))
	}
	sectors, err := d.cfg.Mapper.Sectors(ctx, dev.Path)
	if err != nil {
		return volstack.Artifact{}, fmt.Errorf("dmLinear: %w", err)
	}
	if err := d.cfg.Mapper.Create(ctx, d.cfg.Name, deviceNumber(dev), sectors); err != nil {
		return volstack.Artifact{}, fmt.Errorf("dmLinear: %w", err)
	}
	return d.own()
}

func (d *DMLinear) swap(ctx context.Context, below volstack.Artifact) error {
	dev, ok := belowDevice(below)
	if !ok {
		return fmt.Errorf("dmLinear: cannot re-point %s: no device below", d.cfg.Name)
	}
	sectors, err := d.cfg.Mapper.Sectors(ctx, dev.Path)
	if err != nil {
		return fmt.Errorf("dmLinear: %w", err)
	}
	if err := d.cfg.Mapper.Swap(ctx, d.cfg.Name, deviceNumber(dev), sectors); err != nil {
		return fmt.Errorf("dmLinear: %w", err)
	}
	d.stale = false
	return nil
}

// Release removes the mapping and keeps the data, which lives below it.
func (d *DMLinear) Release(ctx context.Context, _ volstack.Artifact) error {
	return d.cfg.Mapper.Remove(ctx, d.cfg.Name)
}

// Destroy has nothing durable to remove: the mapping writes no metadata.
func (d *DMLinear) Destroy(context.Context, volstack.Artifact) error { return nil }

// Healthy reports whether the mapping points at the device below, as the last
// Observe found it.
func (d *DMLinear) Healthy(context.Context, volstack.Artifact) (bool, error) { return !d.stale, nil }

// Heal re-points the mapping at the device below, which the fabric layer may
// just have brought up on another subsystem.
func (d *DMLinear) Heal(ctx context.Context, below, _ volstack.Artifact) error {
	if _, err := d.cfg.Mapper.Table(ctx, d.cfg.Name); errors.Is(err, devmapper.ErrNotFound) {
		_, err := d.Ensure(ctx, below)
		return err
	}
	return d.swap(ctx, below)
}

// DMLinearParams is what the record keeps of this layer.
type DMLinearParams struct {
	Name string `json:"name"`
}

// Params is the record's view of the layer.
func (d *DMLinear) Params() any { return DMLinearParams{Name: d.cfg.Name} }

// Package layers holds the implementations of the volume stack's layer
// contract. Each one wraps a node-level primitive atlas already provides, and
// what it adds is the four verbs and the states they dispatch on.
package layers

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
)

// FabricConfig is what a fabric layer is built with.
type FabricConfig struct {
	// Connection is where the control plane says the volume lives, with its
	// endpoints in the priority order it published them.
	Connection lvol.Connection

	// Connector attaches and detaches, and Devices answers what is attached.
	Connector nvmeof.Connector
	Devices   nvme.DeviceResolver

	// HostNQN and HostID are this node's identity, presented on every connect
	// so that an allowed-hosts pool authorizes the right host.
	HostNQN string
	HostID  string

	// DHCHAPSecret and DHCHAPCtrlSecret authenticate the connect. They are held
	// in memory for the life of the layer and never recorded: SecretRef is what
	// the record carries.
	DHCHAPSecret     string
	DHCHAPCtrlSecret string

	// SecretRef names where the secrets are read from, for a later process that
	// has to rebuild this layer from the record.
	SecretRef string
}

// Fabric is the NVMe-oF connection under a volume: the bottom of every plan.
//
// Its Release is a detach rather than a disconnect. Disconnecting a subsystem
// tears down every namespace on it, and a simplyblock subsystem provisioned with
// max_namespace_per_subsys holds several volumes, so tearing one down on one
// volume's unstage would rip the block device out from under every co-tenant on
// the node.
type Fabric struct {
	cfg FabricConfig
}

// NewFabric returns the fabric layer for one volume.
func NewFabric(cfg FabricConfig) *Fabric { return &Fabric{cfg: cfg} }

// Name is what the record calls this layer.
func (f *Fabric) Name() string { return "fabric" }

// selector identifies this volume's namespace among everything attached.
func (f *Fabric) selector() nvme.DeviceSelector {
	return nvme.DeviceSelector{NQN: f.cfg.Connection.NQN, NSID: nvme.NamespaceID(f.cfg.Connection.NSID)}
}

// Observe maps what the kernel has onto the stack's states.
//
// StateForeign and StateInactive do not arise here: a namespace carries no
// host-local identity to be another volume's, and it cannot be
// present-but-deactivated the way a volume group can.
func (f *Fabric) Observe(ctx context.Context, _ volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	dev, found, err := f.device(ctx)
	if err != nil {
		return volstack.StateAbsent, volstack.Artifact{}, err
	}
	if !found {
		return volstack.StateAbsent, volstack.Artifact{}, nil
	}

	// The device travels with every state but Absent, StatePartial included: one
	// that is present and cannot serve is still the device a release detaches.
	own := volstack.Artifact{Devices: []blockdev.Device{dev.Namespace.BlockDevice()}}
	if !dev.Accessible() {
		// Present in sysfs and unable to take I/O: a subsystem awaiting
		// reaping, or a volume whose paths have all gone into persistent loss.
		// A layer above one of these would format or mount a device that
		// answers nothing.
		return volstack.StatePartial, own, nil
	}
	return volstack.StateReady, own, nil
}

// Ensure attaches every published endpoint in the control plane's order and
// returns the namespace device.
//
// It attaches unconditionally rather than only when Observe found nothing: the
// connector is idempotent, and a subsystem that is attached over one path and
// missing another is exactly the state a re-attach repairs.
func (f *Fabric) Ensure(ctx context.Context, _ volstack.Artifact) (volstack.Artifact, error) {
	targets := nvmeof.Targets(f.cfg.Connection, f.targetOptions()...)
	if len(targets) == 0 {
		return volstack.Artifact{}, fmt.Errorf(
			"fabric: the control plane published no endpoint for %s", f.cfg.Connection.NQN)
	}
	if _, err := f.cfg.Connector.ConnectPaths(ctx, targets); err != nil {
		return volstack.Artifact{}, fmt.Errorf("fabric: attach %s: %w", f.cfg.Connection.NQN, err)
	}

	dev, err := nvmeof.WaitForDevice(ctx, f.cfg.Devices, f.selector())
	if err != nil {
		return volstack.Artifact{}, fmt.Errorf("fabric: wait for the namespace of %s: %w",
			f.cfg.Connection.NQN, err)
	}
	return volstack.Artifact{Devices: []blockdev.Device{dev.Namespace.BlockDevice()}}, nil
}

// Release gives up this host's hold on the volume, and gives up nothing else.
//
// The question of whether the subsystem may be torn down is asked by
// nvmeof.DetachDevice, which decides on whether the subsystem *can* hold other
// namespaces rather than on whether it currently does: one can join between the
// check and the disconnect, and then a "no co-tenants right now" answer would
// have been correct and still wrong.
//
// A device that is already gone is not an error. That is the normal case after
// total path loss, which is precisely when an unstage arrives.
func (f *Fabric) Release(ctx context.Context, _ volstack.Artifact) error {
	dev, found, err := f.device(ctx)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err := nvmeof.DetachDevice(ctx, f.cfg.Connector, dev); err != nil {
		return fmt.Errorf("fabric: detach %s: %w", f.cfg.Connection.NQN, err)
	}
	return nil
}

// Destroy does nothing. The namespace belongs to the control plane and is
// removed by DeleteVolume, so there is no durable object here for a node to take
// away.
func (f *Fabric) Destroy(context.Context, volstack.Artifact) error { return nil }

// Healthy reports whether the volume can currently take I/O, which is the read a
// heal dispatches on.
func (f *Fabric) Healthy(ctx context.Context, _ volstack.Artifact) (bool, error) {
	dev, found, err := f.device(ctx)
	if err != nil {
		return false, err
	}
	return found && dev.Accessible(), nil
}

// Heal re-attaches the paths that are missing. It creates nothing: the namespace
// is the control plane's and the data behind it already exists.
func (f *Fabric) Heal(ctx context.Context, below, _ volstack.Artifact) error {
	if _, err := f.Ensure(ctx, below); err != nil {
		return err
	}
	return nil
}

// FabricParams is what the record carries for this layer.
//
// It names where a secret is read from and never the secret. The record outlives
// the pod that wrote it, so a value here would put a credential in cleartext on
// every node that ever staged the volume.
type FabricParams struct {
	NQN       string `json:"nqn"`
	NSID      uint32 `json:"nsid,omitempty"`
	HostNQN   string `json:"hostNQN,omitempty"`
	SecretRef string `json:"secretRef,omitempty"`
}

// Params is what a later process needs to rebuild this layer.
func (f *Fabric) Params() any {
	return FabricParams{
		NQN:       f.cfg.Connection.NQN,
		NSID:      f.cfg.Connection.NSID,
		HostNQN:   f.cfg.HostNQN,
		SecretRef: f.cfg.SecretRef,
	}
}

// device resolves this volume's namespace, reporting whether it is attached at
// all. More than one match is the stale-subsystem case, and the most serviceable
// one is the answer: that is the device a layer above would be using.
func (f *Fabric) device(ctx context.Context) (nvme.Device, bool, error) {
	devices, err := f.cfg.Devices.ListWithSelector(ctx, f.selector())
	if err != nil {
		return nvme.Device{}, false, fmt.Errorf("fabric: look up the namespace of %s: %w",
			f.cfg.Connection.NQN, err)
	}
	if len(devices) == 0 {
		return nvme.Device{}, false, nil
	}
	best := devices[0]
	for _, d := range devices[1:] {
		if d.Accessible() && !best.Accessible() {
			best = d
		}
	}
	return best, true, nil
}

// targetOptions carries this node's identity and its credentials onto every
// path. A secret belongs to one host and subsystem pair, so it travels with the
// host NQN it was issued for.
func (f *Fabric) targetOptions() []nvmeof.TargetOption {
	var opts []nvmeof.TargetOption
	if f.cfg.HostNQN != "" {
		opts = append(opts, nvmeof.WithHostNQN(f.cfg.HostNQN))
	}
	if f.cfg.HostID != "" {
		opts = append(opts, nvmeof.WithHostID(f.cfg.HostID))
	}
	return opts
}

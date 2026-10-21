// The guest's adapters to the system, for the role machine's injected
// interfaces: connecting a volume, mounting the state volume, writing nfsd's
// key, and running systemd. They are thin, and the role machine's tests use
// fakes for all of them.

package agent

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

// HostIDFromNQN is this guest's NVMe host identifier: the UUID of the host NQN
// it connected with, without dashes, as 16 bytes. The connect presents exactly
// this identifier (the connector derives the host id from the NQN's own UUID), and
// the target reports it for every registration the guest makes. Reading it back
// from the controller's sysfs hostid would give the same value.
func HostIDFromNQN(hostNQN string) ([16]byte, error) {
	var id [16]byte
	u, ok := nqn.HostUUID(hostNQN)
	if !ok {
		return id, fmt.Errorf("host NQN %q carries no UUID", hostNQN)
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(u, "-", ""))
	if err != nil || len(raw) != len(id) {
		return id, fmt.Errorf("host NQN %q carries the malformed UUID %q", hostNQN, u)
	}
	copy(id[:], raw)
	return id, nil
}

// FabricConnector connects a volume over every path its connection publishes,
// with the fabric layer the node plugin stages block volumes with, and waits
// for the namespace's device.
type FabricConnector struct {
	Connector nvmeof.Connector
	Devices   nvme.DeviceResolver
}

// Connect implements Connector.
func (c FabricConnector) Connect(ctx context.Context, conn lvol.Connection, hostNQN string) (blockdev.Device, error) {
	fabric := layers.NewFabric(layers.FabricConfig{
		Connection: conn, Connector: c.Connector, Devices: c.Devices, HostNQN: hostNQN,
	})
	art, err := fabric.Ensure(ctx, volstack.Artifact{})
	if err != nil {
		return blockdev.Device{}, err
	}
	if len(art.Devices) == 0 {
		return blockdev.Device{}, fmt.Errorf("connecting %s: no device came up", conn.NQN)
	}
	return art.Devices[0], nil
}

// ContentProber reads what a device carries.
type ContentProber interface {
	Read(ctx context.Context, dev blockdev.Device) (blockdev.Reading, error)
}

// Filesystems is the mount and format side of the node's filesystem operations.
type Filesystems interface {
	Format(ctx context.Context, device, fsType string, options []string) error
	Mount(ctx context.Context, source, target, fsType string, options []string) error
	Unmount(ctx context.Context, target string) error
	IsMountPoint(ctx context.Context, path string) (bool, error)
}

// StateFSType is the state volume's filesystem.
const StateFSType = "ext4"

// FSMounter mounts the state volume, formatting it only when the content probe
// finds it blank. It is the same probe and the same mkfs and mount helpers the
// block path uses, so a device with anything on it is never formatted.
type FSMounter struct {
	Prober ContentProber
	FS     Filesystems
}

// Mount implements StateMounter.
func (m FSMounter) Mount(ctx context.Context, dev blockdev.Device, dir string) error {
	if mounted, err := m.FS.IsMountPoint(ctx, dir); err != nil {
		return fmt.Errorf("inspecting %s: %w", dir, err)
	} else if mounted {
		return nil
	}
	reading, err := m.Prober.Read(ctx, dev)
	if err != nil {
		return fmt.Errorf("reading what %s carries: %w", dev.Path, err)
	}
	switch {
	case reading.Content == blockdev.ContentBlank:
		if err := m.FS.Format(ctx, dev.Path, StateFSType, nil); err != nil {
			return fmt.Errorf("formatting %s: %w", dev.Path, err)
		}
	case reading.Content == blockdev.ContentFilesystem && reading.Type == StateFSType:
	default:
		return fmt.Errorf("the state volume %s carries %s %s, not a blank device or %s: %s",
			dev.Path, reading.Content, reading.Type, StateFSType, reading.Detail)
	}
	return m.FS.Mount(ctx, dev.Path, dir, StateFSType, nil)
}

// Unmount implements StateMounter.
func (m FSMounter) Unmount(ctx context.Context, dir string) error {
	return m.FS.Unmount(ctx, dir)
}

// ParamKeyWriter writes nfsd's reservation key to its module parameter.
type ParamKeyWriter struct {
	// Path is the parameter file.
	Path string
	// LoadModule loads nfsd, whose parameters exist only once it is loaded.
	LoadModule func(ctx context.Context) error
}

// NFSDKeyParameter is the module parameter patch 0023 adds.
const NFSDKeyParameter = "/sys/module/nfsd/parameters/pnfs_mds_pr_key"

// Write implements KeyWriter.
func (w ParamKeyWriter) Write(ctx context.Context, key uint64) error {
	if err := w.LoadModule(ctx); err != nil {
		return fmt.Errorf("loading nfsd: %w", err)
	}
	return os.WriteFile(w.Path, []byte(strconv.FormatUint(key, 10)), 0o600)
}

// Command runs one command and returns its combined output.
type Command func(ctx context.Context, name string, args ...string) ([]byte, error)

// Systemctl is the guest's systemd, driven through Run.
type Systemctl struct{ Run Command }

// Start starts a unit.
func (s Systemctl) Start(ctx context.Context, unit string) error {
	return s.run(ctx, "systemctl", "start", unit)
}

// Stop stops a unit.
func (s Systemctl) Stop(ctx context.Context, unit string) error {
	return s.run(ctx, "systemctl", "stop", unit)
}

// CreateTmpfiles creates what a tmpfiles.d file lists.
func (s Systemctl) CreateTmpfiles(ctx context.Context, conf string) error {
	return s.run(ctx, "systemd-tmpfiles", "--create", conf)
}

func (s Systemctl) run(ctx context.Context, name string, args ...string) error {
	if out, err := s.Run(ctx, name, args...); err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

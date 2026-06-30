// Package sysfs provides low-level access to the Linux NVMe sysfs
// hierarchy: path-layout constants and attribute-reading helpers. It is
// internal so these details stay out of the public nvme API and can change
// freely. Package nvme builds the public Subsystem/Controller/Namespace
// model on top of it.
//
// Observed layout (NVMe-oF/TCP, multipath, kernel 5.14 / Rocky 9):
//
//	<mount>/class/nvme/nvme0                   -> controller (path/leg)
//	  address, transport, state, cntlid, cntrltype, subsysnqn,
//	  hostnqn, hostid, queue_count, sqsize, kato, ctrl_loss_tmo,
//	  reconnect_delay, fast_io_fail_tmo, numa_node, dev, ...
//
//	<mount>/class/nvme-subsystem/nvme-subsys0  -> subsystem
//	  subsysnqn, model, serial, firmware_rev, subsystype, iopolicy,
//	  nvme0 -> ../../nvme-fabrics/ctl/nvme0    (controller links)
//	  nvme0n1/                                 (block namespace head)
//	  ng0n1/                                   (generic char namespace)
//
//	<mount>/class/nvme-subsystem/nvme-subsys0/nvme0n1 -> namespace
//	  nsid, uuid, nguid, wwid, csi, size, nuse, metadata_bytes,
//	  ro, hidden, dev, queue/logical_block_size, ...
//
// Per-controller namespace legs appear as nvmeXcYnZ under the controller
// (e.g. nvme0c0n1); the host I/O device is the subsystem-level nvmeXnY.
package sysfs

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultMount is the conventional sysfs mount point.
	DefaultMount = "/sys"
	// DefaultDev is the conventional device-node directory.
	DefaultDev = "/dev"

	// Subpaths under the sysfs mount point.
	ClassNVMe      = "class/nvme"           // one dir per controller (nvmeN)
	ClassSubsystem = "class/nvme-subsystem" // one dir per subsystem (nvme-subsysN)
	ClassFabrics   = "class/nvme-fabrics"   // NVMe-oF fabrics control
)

// ReadAttr reads the sysfs attribute file at the joined path and returns
// its contents with surrounding whitespace trimmed (the kernel terminates
// values with a newline and pads some fields with spaces).
func ReadAttr(elem ...string) (string, error) {
	b, err := os.ReadFile(filepath.Join(elem...))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// List returns the entry names of the joined directory path. A missing
// directory yields an empty slice and no error — the common case on hosts
// with no NVMe devices.
func List(elem ...string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(elem...))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

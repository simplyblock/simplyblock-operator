// A LogicalBlock cluster's per-node configuration.
//
// Regression: 2026-09-24-block-paths-written-to-the-nvme-variable — a cluster
// built out of logical block devices rendered its entries as if it were an NVMe
// one. The device paths went to NVME_DEVICES, which the init container passes
// as --nvme-devices and the backend resolves against `nvme list` namespace
// names, and LBLK was never written at all, so the class the whole deployment
// was for never reached node_configure.py.
//
// The init container is what made it visible: unlike the NVMe case above, whose
// return value is discarded, this one is a `set -e` script that exits 1, so the
// storage-node pods sat in Init:CrashLoopBackOff and every node of the cluster
// stayed Provisioning until the deployment timed out.

package node

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The device paths of a block cluster go to BLK_NAMES, which is the channel
// --blk-names reads, and not to the one that resolves namespace names.
func TestBlockPathsReachTheBlockNameList(t *testing.T) {
	got := renderedFor(t, simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock,
		"/dev/sda", "/dev/sdb")

	if names := got["BLK_NAMES"]; names != "/dev/sda,/dev/sdb" {
		t.Errorf("BLK_NAMES = %q, and a block cluster's devices are named by path", names)
	}
	if devices := got["NVME_DEVICES"]; devices != "" {
		t.Errorf("NVME_DEVICES = %q, which the backend matches against `nvme list` "+
			"namespace names, so a block path there selects nothing", devices)
	}
}

// LBLK is the switch the init container tests to pass --lblk, and without it
// the backend is asked for NVMe devices whatever the paths say.
func TestABlockClusterSaysSo(t *testing.T) {
	got := renderedFor(t, simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock,
		"/dev/sda")

	if lblk := got["LBLK"]; lblk != "true" {
		t.Errorf("LBLK = %q, and the init container passes --lblk on that word alone", lblk)
	}
}

// An NVMe cluster is unchanged: it says nothing about the block class, so the
// init container's block branches stay unvisited.
func TestAnNVMeClusterDoesNotSayItIsBlock(t *testing.T) {
	got := renderedFor(t, simplyblockv1alpha2.StorageClusterDeviceClassNVMe,
		"0000:01:00.0", "nvme0n1")

	if lblk := got["LBLK"]; lblk == "true" {
		t.Error("LBLK is true on an NVMe cluster, which would ask the backend for the wrong class")
	}
	if names := got["BLK_NAMES"]; names != "" {
		t.Errorf("BLK_NAMES = %q on an NVMe cluster", names)
	}
	if devices := got["NVME_DEVICES"]; devices != "nvme0n1" {
		t.Errorf("NVME_DEVICES = %q, and a bare name is still a namespace name", devices)
	}
}

// A cluster that states no class is NVMe, which is what the field defaults to
// and what describes every cluster written before it existed.
func TestAnUnstatedClassIsNotBlock(t *testing.T) {
	got := renderedFor(t, "", "nvme0n1")

	if lblk := got["LBLK"]; lblk == "true" {
		t.Error("an unstated device class rendered as the block one")
	}
}

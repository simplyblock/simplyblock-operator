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
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// stated is the word the init container tests LBLK against. It is spelled once
// because the script spells it once: `[ "${LBLK}" = "true" ]` matches that and
// nothing else, so a case asserting some other truthy spelling would pass here
// and mean nothing on a node.
const stated = "true"

// renderedWithJournal is renderedFor with a journal share stated, which is the
// one setting the two classes read through different variables.
func renderedWithJournal(
	t *testing.T, class simplyblockv1alpha2.StorageClusterDeviceClass, percent int32,
) map[string]string {
	t.Helper()
	cluster := &simplyblockv1alpha2.StorageCluster{
		Spec: simplyblockv1alpha2.StorageClusterSpec{DeviceClass: class},
	}
	node := &simplyblockv1alpha2.StorageNode{
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			Config: simplyblockv1alpha2.StorageNodeConfig{
				DeviceNames: []string{"/dev/sda"},
				JournalManager: &simplyblockv1alpha2.JournalManagerSpec{
					PercentPerDevice: ptr.To(percent),
				},
			},
		},
	}
	out := map[string]string{}
	for _, line := range strings.Split(renderNodeConfig(cluster, node), "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			out[key] = strings.Trim(value, "'")
		}
	}
	return out
}

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

	if lblk := got["LBLK"]; lblk != stated {
		t.Errorf("LBLK = %q, and the init container passes --lblk on that word alone", lblk)
	}
}

// An NVMe cluster is unchanged: it says nothing about the block class, so the
// init container's block branches stay unvisited.
func TestAnNVMeClusterDoesNotSayItIsBlock(t *testing.T) {
	got := renderedFor(t, simplyblockv1alpha2.StorageClusterDeviceClassNVMe,
		"0000:01:00.0", "nvme0n1")

	if lblk := got["LBLK"]; lblk == stated {
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

	if lblk := got["LBLK"]; lblk == stated {
		t.Error("an unstated device class rendered as the block one")
	}
}

// The journal share of a block cluster goes to LBLK_JM_PERCENT, which is the
// variable the init container reads. JM_PERCENT is what an NVMe cluster writes
// and what the block branch of the script never looks at.
func TestTheJournalShareOfABlockClusterIsReadable(t *testing.T) {
	got := renderedWithJournal(t, simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock, 7)

	if percent := got["LBLK_JM_PERCENT"]; percent != "7" {
		t.Errorf("LBLK_JM_PERCENT = %q, and --jm-percent is passed from that variable", percent)
	}
}

// An NVMe cluster keeps writing JM_PERCENT and says nothing under the block
// name, so the two classes cannot read each other's share.
func TestTheJournalShareOfAnNVMeClusterIsUnchanged(t *testing.T) {
	got := renderedWithJournal(t, simplyblockv1alpha2.StorageClusterDeviceClassNVMe, 7)

	if percent := got["JM_PERCENT"]; percent != "7" {
		t.Errorf("JM_PERCENT = %q", percent)
	}
	if percent := got["LBLK_JM_PERCENT"]; percent != "" {
		t.Errorf("LBLK_JM_PERCENT = %q on an NVMe cluster", percent)
	}
}

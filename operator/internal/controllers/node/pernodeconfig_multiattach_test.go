// A multi-attach cluster's per-node configuration. Its nodes' data device is
// the LUN the control plane creates on the array when the node is added, so
// the one local device a node names is its journal, whole: node_configure.py
// is told so with --lblk-journal-only, and never splits that device into a
// journal and a data partition.

package node

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func renderedNodeEnv(cluster *simplyblockv1alpha2.StorageCluster) map[string]string {
	node := &simplyblockv1alpha2.StorageNode{
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			Config: simplyblockv1alpha2.StorageNodeConfig{DeviceNames: []string{"/dev/nvme0n1"}},
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

func multiAttachBlockCluster() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{Spec: simplyblockv1alpha2.StorageClusterSpec{
		DeviceClass: simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock,
		RaidDevices: ptr.To(true),
		MultiAttach: &simplyblockv1alpha2.MultiAttachSpec{FaultTolerance: 1},
	}}
}

func TestAMultiAttachNodeNamesOnlyItsJournal(t *testing.T) {
	env := renderedNodeEnv(multiAttachBlockCluster())
	if env["LBLK_JOURNAL_ONLY"] != stated {
		t.Errorf("LBLK_JOURNAL_ONLY = %q, want %q", env["LBLK_JOURNAL_ONLY"], stated)
	}
	if env["LBLK"] != stated || env["BLK_NAMES"] != "/dev/nvme0n1" {
		t.Errorf("the journal device did not reach the block channel: %v", env)
	}
}

// With the journal on a LUN of the array the node uses no local device:
// node_configure.py is told so with --lblk-array-only and given no names.
func TestAJournalLUNNodeNamesNoDevice(t *testing.T) {
	cluster := multiAttachBlockCluster()
	cluster.Spec.MultiAttach.Array = &simplyblockv1alpha2.StorageArraySpec{Plugin: "pure-flasharray"}
	env := renderedNodeEnv(cluster)
	if env["LBLK_ARRAY_ONLY"] != stated || env["LBLK"] != stated {
		t.Errorf("LBLK_ARRAY_ONLY = %q, LBLK = %q", env["LBLK_ARRAY_ONLY"], env["LBLK"])
	}
	if env["BLK_NAMES"] != "" || env["LBLK_JOURNAL_ONLY"] != "" {
		t.Errorf("BLK_NAMES = %q, LBLK_JOURNAL_ONLY = %q; want neither", env["BLK_NAMES"], env["LBLK_JOURNAL_ONLY"])
	}

	cluster.Spec.MultiAttach.Array.Journal = &simplyblockv1alpha2.StorageArrayJournal{
		Placement: simplyblockv1alpha2.StorageArrayJournalLocal}
	env = renderedNodeEnv(cluster)
	if env["LBLK_JOURNAL_ONLY"] != stated || env["LBLK_ARRAY_ONLY"] != "" || env["BLK_NAMES"] != "/dev/nvme0n1" {
		t.Errorf("a Local journal: %v", env)
	}
}

// --lblk-journal-only and --lblk-journal-device exclude each other in
// node_configure.py, so the dedicated-journal toggle is not passed beside it.
func TestAMultiAttachNodeIgnoresTheDedicatedJournalToggle(t *testing.T) {
	cluster := multiAttachBlockCluster()
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{EnableJournalDevice: ptr.To(true)}
	env := renderedNodeEnv(cluster)
	if _, ok := env["LBLK_JOURNAL_DEVICE"]; ok {
		t.Errorf("LBLK_JOURNAL_DEVICE written beside LBLK_JOURNAL_ONLY")
	}
	if env["LBLK_JOURNAL_ONLY"] != stated {
		t.Errorf("LBLK_JOURNAL_ONLY = %q", env["LBLK_JOURNAL_ONLY"])
	}
}

func TestARegularBlockNodeIsNotJournalOnly(t *testing.T) {
	cluster := multiAttachBlockCluster()
	cluster.Spec.MultiAttach = nil
	if _, ok := renderedNodeEnv(cluster)["LBLK_JOURNAL_ONLY"]; ok {
		t.Error("a cluster that is not multi-attach wrote LBLK_JOURNAL_ONLY")
	}
}

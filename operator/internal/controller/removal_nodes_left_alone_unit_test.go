// A node in the middle of its removal is left alone by the operator's bulk
// paths: the node recycle (shutdown + forced restart of every node) and the
// worker drain before an MCO reboot. Either would otherwise shut it down --
// writing in_shutdown over its removal status -- and restart it back into
// service mid-removal.

package controller

import (
	"reflect"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

var removalStatuses = []string{
	nodeStatusMigratingDevices, nodeStatusMigratingLvols, nodeStatusInRemoval,
	utils.NodeStatusRemoved, nodeStatusRemovedFailed,
}

func TestIsNodeInRemoval(t *testing.T) {
	for _, s := range removalStatuses {
		if !isNodeInRemoval(s) {
			t.Errorf("%s not treated as in removal", s)
		}
	}
	for _, s := range []string{utils.NodeStatusOnline, utils.NodeStatusOffline, utils.NodeStatusInShutdown,
		nodeStatusInRestart, "pending_removal", "suspended"} {
		if isNodeInRemoval(s) {
			t.Errorf("%s treated as in removal; the node may still be up", s)
		}
	}
}

func TestRecycleNodeUUIDs_SkipsNodesInRemoval(t *testing.T) {
	nodes := make([]utils.NodeStatusResponse, 0, len(removalStatuses)+2)
	nodes = append(nodes, utils.NodeStatusResponse{UUID: "a", Status: utils.NodeStatusOnline})
	for i, s := range removalStatuses {
		nodes = append(nodes, utils.NodeStatusResponse{UUID: string(rune('m' + i)), Status: s})
	}
	nodes = append(nodes, utils.NodeStatusResponse{UUID: "z", Status: utils.NodeStatusOffline})
	if got := recycleNodeUUIDs(nodes); !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Errorf("recycle list = %v, want [a z]", got)
	}
}

func TestFindAllNodeUUIDs_SkipsNodesInRemoval(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNodeSet{}
	sn.Status.Nodes = []simplyblockv1alpha1.NodeStatus{
		{UUID: "up", Hostname: "w1", Status: utils.NodeStatusOnline},
		{UUID: "leaving", Hostname: "w1", Status: nodeStatusMigratingLvols},
		{UUID: "other", Hostname: "w2", Status: utils.NodeStatusOnline},
	}
	if got := findAllNodeUUIDs(sn, "w1"); !reflect.DeepEqual(got, []string{"up"}) {
		t.Errorf("worker nodes = %v, want [up]", got)
	}
}

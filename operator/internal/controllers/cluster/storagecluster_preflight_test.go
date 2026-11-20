// The follower's preflight (design-stretched-sync-replication-ramen.md §5.4):
// before it adopts the backend cluster the hub handed it, the follower checks
// what would otherwise fail later and less legibly, and holds in
// CheckingControlPlane with the reason rather than failing a deadline.

package cluster

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	atlascp "github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const foreignNodeUUID = "f0e1d2c3-0000-0000-0000-00000000000f"

// Regression: 2026-10-22-follower-sync-refusal-in-adopting: a follower handed a
// cluster without sync replication entered Adopting before it checked, refused
// there, and went Unavailable when Adopting's deadline passed, which reads as a
// failed adoption rather than as the wrong cluster. The check is the preflight's,
// before the adoption begins, and a hold there has no deadline.
func TestAFollowerHandedANonSyncClusterHoldsBeforeAdopting(t *testing.T) {
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), upgradeSecret())

	cluster := reconcileCluster(t, r, 6)
	if got := cluster.Status.Step.State; got != string(simplyblockv1alpha2.StorageClusterStepCheckingControlPlane) {
		t.Errorf("step = %q, want the follower held in CheckingControlPlane before the adoption", got)
	}
	if cluster.Status.UUID != "" {
		t.Errorf("status.uuid = %q: the follower joined a cluster that is not a sync cluster", cluster.Status.UUID)
	}
	if !strings.Contains(cluster.Status.Message, "not a sync-replication cluster") {
		t.Errorf("message = %q, want the reason for the hold", cluster.Status.Message)
	}
	if !rec.has(SyncReplicationMismatch) {
		t.Error("the hold emitted no SyncReplicationMismatch")
	}
}

// U-66: the follower's site already has a node in the backend cluster that is not
// one of this site's StorageNodes, which means another installation added it.
// Adopting would put two operators on one site's nodes, so the follower holds and
// names the node.
func TestAFollowerWhoseSiteHasAnotherInstallationsNodeHolds(t *testing.T) {
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
		storageNodes: func(string) ([]utils.NodeStatusResponse, error) {
			return []utils.NodeStatusResponse{
				{UUID: "a0000000-0000-0000-0000-00000000000a", Status: "online", Site: peerSite},
				{UUID: foreignNodeUUID, Status: "online", Site: thisSite},
			}, nil
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), upgradeSecret())

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != "" {
		t.Fatalf("status.uuid = %q: the follower adopted a cluster whose site %s another installation holds",
			cluster.Status.UUID, thisSite)
	}
	if got := cluster.Status.Step.State; got != string(simplyblockv1alpha2.StorageClusterStepCheckingControlPlane) {
		t.Errorf("step = %q, want the follower held in CheckingControlPlane", got)
	}
	if !strings.Contains(cluster.Status.Message, foreignNodeUUID) {
		t.Errorf("message = %q, want the other installation's node named", cluster.Status.Message)
	}
	if !rec.has(ForeignNodeOnSite) {
		t.Error("the hold emitted no ForeignNodeOnSite")
	}
}

// A node on the follower's site that is one of this site's own StorageNodes is
// not another installation's: a follower whose StorageCluster is recreated over
// nodes it added before adopts again.
func TestAFollowerAdoptsOverItsOwnSitesNodes(t *testing.T) {
	api := &fakeControlPlane{
		ready:   func() error { return nil },
		cluster: func(string) (atlascp.Cluster, error) { return syncCluster(), nil },
		storageNodes: func(string) ([]utils.NodeStatusResponse, error) {
			return []utils.NodeStatusResponse{{UUID: foreignNodeUUID, Status: "online", Site: thisSite}}, nil
		},
	}
	own := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: objectMeta(testClusterName + "-worker-1-0"),
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: testClusterName, WorkerNode: "worker-1"},
		Status:     simplyblockv1alpha2.StorageNodeStatus{UUID: foreignNodeUUID},
	}
	own.CreationTimestamp = metav1.Now()
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(asSite(simplyblockv1alpha2.StretchedRoleFollower)), upgradeSecret(), own)

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != testClusterUUID {
		t.Errorf("status.uuid = %q (step %q, message %q), want the follower to adopt over its own node",
			cluster.Status.UUID, cluster.Status.Step.State, cluster.Status.Message)
	}
	if rec.has(ForeignNodeOnSite) {
		t.Error("the follower's own node was reported as another installation's")
	}
}

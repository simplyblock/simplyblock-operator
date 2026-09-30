// A refused node DELETE is final only when the cluster is ready to hear it.
//
// The control plane answers every refused removal with a bare 400, whether
// the removal can never succeed or the cluster is merely busy ("not active",
// "rebalancing", "task found"). The cluster's own state tells the two apart.

package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func clusterInState(status string, rebalancing bool) *simplyblockv1alpha1.StorageCluster {
	c := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: opsTestNS},
	}
	c.Status.Status = status
	c.Status.Rebalancing = &rebalancing
	return c
}

func refusedDeleteFixtures(t *testing.T, cluster *simplyblockv1alpha1.StorageCluster) (
	*StorageNodeOpsReconciler, *simplyblockv1alpha1.StorageNodeOps, *simplyblockv1alpha1.StorageNode,
) {
	t.Helper()
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.2"}
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	ops.Status.SubPhase = simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving
	sns := newTestStorageNodeSet("sns", opsTestNS, "cluster")
	return newOpsReconciler(t, sn, ops, sns, cluster), ops, sn
}

func TestDrainRemove_RefusedDeleteWhileTheClusterIsNotActiveWaits(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha1.StorageCluster{
		"in_shrink":   clusterInState("in_shrink", false),
		"rebalancing": clusterInState(utils.ClusterStatusActive, true),
	} {
		t.Run(name, func(t *testing.T) {
			status := nodeStatusMigratingLvols
			deletes, posts := 0, 0
			srv := removeStepServer(t, &status, http.StatusBadRequest, &deletes, &posts)
			defer srv.Close()

			r, ops, sn := refusedDeleteFixtures(t, cluster)
			res, err := r.drainRemove(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL))
			if err != nil {
				t.Fatalf("drainRemove: %v", err)
			}
			if res.RequeueAfter == 0 {
				t.Error("expected a requeue while the cluster settles")
			}
			updated := reloadOps(t, r)
			if updated.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
				t.Errorf("a 400 while the cluster is %s failed the op: %q", name, updated.Status.Message)
			}
			if !strings.Contains(updated.Status.Message, "drain paused") {
				t.Errorf("message = %q, want the pause reason", updated.Status.Message)
			}
			if updated.Status.RemoveTriggered {
				t.Error("a refused DELETE was latched as sent")
			}
			if posts != 0 {
				t.Errorf("attempted %d resume(s) of a stopped node", posts)
			}
		})
	}
}

func TestDrainRemove_RefusedDeleteWithTheClusterActiveIsFinal(t *testing.T) {
	status := nodeStatusMigratingLvols
	deletes, posts := 0, 0
	srv := removeStepServer(t, &status, http.StatusBadRequest, &deletes, &posts)
	defer srv.Close()

	r, ops, sn := refusedDeleteFixtures(t, clusterInState(utils.ClusterStatusActive, false))
	if _, err := r.drainRemove(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainRemove: %v", err)
	}
	updated := reloadOps(t, r)
	if updated.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase: got %q, want Failed for a refusal with the cluster active", updated.Status.Phase)
	}
	if posts != 0 {
		t.Errorf("attempted %d resume(s) of a stopped node", posts)
	}
}

// A target the control plane refuses outright is burnt at once, and failures
// are counted on the pause path too. Together these were the run-10 loop: the
// drainee's fallback source was offered ~320 times in ten minutes, refused
// every time, and counted once, because the pause path deleted failed
// VolumeMigrations before counting them (2026-09-29).

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func refusedVM(uid, target, msg string) simplyblockv1alpha1.VolumeMigration {
	vm := simplyblockv1alpha1.VolumeMigration{}
	vm.UID = types.UID(uid)
	vm.Spec.PVName = "pv-a"
	vm.Spec.TargetNodeUUID = target
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseFailed
	vm.Status.ErrorMessage = msg
	return vm
}

func TestTargetRefusedByBackendBurnsTheTargetAtOnce(t *testing.T) {
	ops := &simplyblockv1alpha1.StorageNodeOps{}
	vm := refusedVM("uid-1", "node-2", "CreateMigration: create migration for subsystem nqn: status 400: "+
		`{"detail":"Cannot migrate to node node-2: source primary node-1 is offline and node-2 is currently serving as the fallback source for this volume"}`)
	if !targetRefusedByBackend(&vm) {
		t.Fatal("a refusal naming the target must be recognised")
	}
	recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{vm})
	if got := drainTargetsTriedFor(ops, "pv-a"); len(got) != 1 || got[0] != "node-2" {
		t.Errorf("the refused target must be recorded as tried on the first failure, got %v", got)
	}
}

func TestAnUnrelatedFailureIsNotABackendRefusal(t *testing.T) {
	for _, msg := range []string{
		"validation job failed on worker-3",
		"CreateMigration: status 400: Cannot migrate to node node-9: ...", // names another node
		"",
	} {
		vm := refusedVM("uid-x", "node-2", msg)
		if targetRefusedByBackend(&vm) {
			t.Errorf("%q must not burn node-2", msg)
		}
	}
}

// The pause path counts before it deletes. It used to delete first and count
// never, and a drain reads "cluster is rebalancing" most of the time.
func TestHandleFailedVolumeMigrations_CountsOnThePausePathToo(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "remove")
	sns := &simplyblockv1alpha1.StorageNodeSet{}
	sns.Name, sns.Namespace = "sns", opsTestNS
	sns.Spec.ClusterName = "cl"
	cluster := &simplyblockv1alpha1.StorageCluster{}
	cluster.Name, cluster.Namespace = "cl", opsTestNS
	rebalancing := true
	cluster.Status.Rebalancing = &rebalancing
	failed := drainVM("failed", simplyblockv1alpha1.VolumeMigrationPhaseFailed, drainTestNode2, "")
	failed.Status.ErrorMessage = "CreateMigration: status 400: Cannot migrate to node " + drainTestNode2 +
		": ... is currently serving as the fallback source for this volume"
	r := newOpsReconciler(t, sn, ops, sns, cluster, failed)
	if err := r.Status().Update(context.Background(), cluster); err != nil {
		t.Fatalf("seed cluster status: %v", err)
	}

	if _, handled := r.handleFailedVolumeMigrations(context.Background(), ops, nil,
		[]simplyblockv1alpha1.VolumeMigration{*failed}); !handled {
		t.Fatal("the failed CR was not handled")
	}
	updated := reloadOps(t, r)
	if got := drainTargetsTriedFor(updated, drainTestPVA); len(got) != 1 || got[0] != drainTestNode2 {
		t.Errorf("a refusal seen while the cluster is rebalancing must still burn the target, got %v", got)
	}
	if updated.Status.Message == "" {
		t.Error("the pause itself must still be reported")
	}
}

// The drain does not pause on its own volume migrations: with the API saying
// is_data_rebalancing=false it proceeds although status.rebalancing (the
// mirror of the wider is_re_balancing) is true. Without an answer from the
// API it keeps the old, conservative behaviour.
func TestClusterPauseCheck_IgnoresTheDrainsOwnMigrations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		apiBody    string
		wantPaused bool
	}{
		{"own migrations only", `{"id":"cl-uuid","is_re_balancing":true,"is_data_rebalancing":false}`, false},
		{"data rebalancing", `{"id":"cl-uuid","is_re_balancing":true,"is_data_rebalancing":true}`, true},
		{"older control plane", `{"id":"cl-uuid","is_re_balancing":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.apiBody))
			}))
			defer srv.Close()
			sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
			ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "remove")
			sns := &simplyblockv1alpha1.StorageNodeSet{}
			sns.Name, sns.Namespace = "sns", opsTestNS
			sns.Spec.ClusterName = "cl"
			cluster := &simplyblockv1alpha1.StorageCluster{}
			cluster.Name, cluster.Namespace = "cl", opsTestNS
			r := newOpsReconciler(t, sn, ops, sns, cluster)
			rebalancing := true
			cluster.Status.Rebalancing = &rebalancing
			cluster.Status.UUID = "cl-uuid"
			if err := r.Status().Update(context.Background(), cluster); err != nil {
				t.Fatalf("seed cluster status: %v", err)
			}
			if _, paused := r.clusterPauseCheck(context.Background(), ops, webapi.NewClient(srv.URL)); paused != tc.wantPaused {
				t.Errorf("paused = %v, want %v", paused, tc.wantPaused)
			}
		})
	}
}

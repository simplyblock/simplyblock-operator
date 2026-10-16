// What the storage-node API EndpointSlice publishes while a node is being
// migrated, which is the one case where a worker has to be in the slice before
// any StorageNode says it runs there. It lives beside the workload controller's
// other tests because the slice is that controller's to write.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	sourceWorkerAddress = "10.0.0.1"
	targetWorkerAddress = "10.0.0.2"
)

// aMigrationWorld is a cluster whose one node runs on opsWorker, with a second
// worker that a migration could target, and the given operations.
func aMigrationWorld(
	t *testing.T, operations ...*simplyblockv1alpha2.StorageNodeOps,
) (*StorageNodeWorkloadReconciler, client.Client) {
	t.Helper()

	source := aWorker(opsWorker, true)
	target := aWorker(opsTarget, true)
	target.Status.Addresses[0].Address = targetWorkerAddress

	objects := make([]client.Object, 0, 4+len(operations))
	objects = append(objects, anOpsCluster(), anOpsNode(), source, target)
	for _, operation := range operations {
		objects = append(objects, operation)
	}

	scheme := testsupport.NewScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNodeOps{}).
		Build()
	return &StorageNodeWorkloadReconciler{Client: apiClient, Scheme: scheme, Namespace: opsNamespace}, apiClient
}

// publishedWorkers is the hostname-to-address map the slice carries after a
// pass.
func publishedWorkers(t *testing.T, apiClient client.Client) map[string]string {
	t.Helper()

	var slice discoveryv1.EndpointSlice
	key := types.NamespacedName{
		Namespace: opsNamespace,
		Name:      utils.BuildStorageNodeEndpointSlice(anOpsCluster(), nil).Name,
	}
	if err := apiClient.Get(context.Background(), key, &slice); err != nil {
		t.Fatalf("read the published EndpointSlice: %v", err)
	}

	published := map[string]string{}
	for _, endpoint := range slice.Endpoints {
		if endpoint.Hostname != nil && len(endpoint.Addresses) > 0 {
			published[*endpoint.Hostname] = endpoint.Addresses[0]
		}
	}
	return published
}

// aMigrateToTheTarget is a Migrate operation against anOpsNode that targets opsTarget.
func aMigrateToTheTarget(name string) *simplyblockv1alpha2.StorageNodeOps {
	operation := anOperation(name, simplyblockv1alpha2.StorageNodeOpsActionMigrate)
	operation.Spec.Migrate = &simplyblockv1alpha2.MigrateSpec{TargetWorkerNode: opsTarget}
	operation.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
	return operation
}

// Regression: 2026-10-13-migrate-target-not-in-endpointslice — the slice was built
// from spec.workerNode alone, so the worker a migration was moving a node onto was
// never published while the node still named its source. The Preparing step waits
// for exactly that name to resolve, so every migration held there until its step
// deadline and then failed, with the target's pod running and healthy throughout.
func TestAMigrationTargetIsPublishedBeforeTheNodeMoves(t *testing.T) {
	r, apiClient := aMigrationWorld(t, aMigrateToTheTarget("move-it"))

	if err := r.reconcileEndpointSlice(context.Background(), anOpsCluster()); err != nil {
		t.Fatalf("reconcileEndpointSlice: %v", err)
	}

	want := map[string]string{
		utils.NodeHostnameLabel(opsWorker): sourceWorkerAddress,
		utils.NodeHostnameLabel(opsTarget): targetWorkerAddress,
	}
	got := publishedWorkers(t, apiClient)
	if len(got) != len(want) {
		t.Fatalf("published workers = %v, want %v", got, want)
	}
	for worker, address := range want {
		if got[worker] != address {
			t.Errorf("published workers = %v, want %v", got, want)
		}
	}
}

// Only an unfinished Migrate of one of this cluster's nodes publishes a worker
// ahead of its node.
func TestOnlyAnUnfinishedMigrationOfThisClusterPublishesATarget(t *testing.T) {
	finished := aMigrateToTheTarget("move-it")
	finished.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded
	restart := aMigrateToTheTarget("restart-it")
	restart.Spec.Action = simplyblockv1alpha2.StorageNodeOpsActionRestart
	elsewhere := aMigrateToTheTarget("move-theirs")
	elsewhere.Spec.NodeRef = "another-clusters-node"

	for name, operation := range map[string]*simplyblockv1alpha2.StorageNodeOps{
		"finished": finished, "restart": restart, "other cluster": elsewhere,
	} {
		t.Run(name, func(t *testing.T) {
			r, apiClient := aMigrationWorld(t, operation)
			if err := r.reconcileEndpointSlice(context.Background(), anOpsCluster()); err != nil {
				t.Fatalf("reconcileEndpointSlice: %v", err)
			}
			got := publishedWorkers(t, apiClient)
			if _, published := got[utils.NodeHostnameLabel(opsTarget)]; published {
				t.Errorf("published workers = %v, want the target left out", got)
			}
		})
	}
}

// A migration that has just been created wakes the pass of the cluster its node
// belongs to, which is what publishes the target before the operation's first
// step starts waiting for it. Any other action wakes nothing: it has no target to
// publish.
func TestAMigrationWakesItsClustersWorkloadPass(t *testing.T) {
	r, _ := aMigrationWorld(t)

	woken := r.clusterOfOperation(context.Background(), aMigrateToTheTarget("move-it"))
	if len(woken) != 1 || woken[0].Name != opsCluster || woken[0].Namespace != opsNamespace {
		t.Errorf("the migration woke %v, want its node's cluster", woken)
	}

	restart := anOperation("restart-it", simplyblockv1alpha2.StorageNodeOpsActionRestart)
	if woken := r.clusterOfOperation(context.Background(), restart); len(woken) != 0 {
		t.Errorf("a restart woke %v, want nothing", woken)
	}
}

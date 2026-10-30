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
	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	sourceWorkerAddress = "10.0.0.1"
	targetWorkerAddress = "10.0.0.2"
	thirdWorker         = "worker-3"
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

	objects := make([]client.Object, 0, 6+len(operations))
	objects = append(objects, anOpsCluster(), anOpsNode(), source, target,
		anAgentPodOn(opsWorker, sourceWorkerAddress), anAgentPodOn(opsTarget, targetWorkerAddress))
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

// reconcileTheSlice runs the slice step the way the pass does: over the one
// reading of the cluster's nodes.
func reconcileTheSlice(r *StorageNodeWorkloadReconciler) error {
	cluster := anOpsCluster()
	nodes, err := r.clusterNodes(context.Background(), cluster)
	if err != nil {
		return err
	}
	return r.reconcileEndpointSlice(context.Background(), cluster, nodes)
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

	if err := reconcileTheSlice(r); err != nil {
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

// Only a running (or lock-holding) Migrate of one of this cluster's nodes publishes a worker
// ahead of its node.
func TestOnlyAnUnfinishedMigrationOfThisClusterPublishesATarget(t *testing.T) {
	finished := aMigrateToTheTarget("move-it")
	finished.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded
	restart := aMigrateToTheTarget("restart-it")
	restart.Spec.Action = simplyblockv1alpha2.StorageNodeOpsActionRestart
	elsewhere := aMigrateToTheTarget("move-theirs")
	elsewhere.Spec.NodeRef = "another-clusters-node"
	queued := aMigrateToTheTarget("queued")
	queued.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhasePending

	for name, operation := range map[string]*simplyblockv1alpha2.StorageNodeOps{
		"finished": finished, "restart": restart, "other cluster": elsewhere,
		"queued behind the lock": queued,
	} {
		t.Run(name, func(t *testing.T) {
			r, apiClient := aMigrationWorld(t, operation)
			if err := reconcileTheSlice(r); err != nil {
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

// Regression: 2026-10-22-queued-migrate-displaces-target: the targets were one
// value per node, taken from whichever unfinished Migrate was listed last, and a
// Migrate waiting in Pending counted as unfinished. A second Migrate queued behind
// the running one therefore replaced the running one's target, and its Preparing
// step waited on a name that was no longer published.
func TestAQueuedMigrationDoesNotDisplaceTheRunningOnesTarget(t *testing.T) {
	running := aMigrateToTheTarget("move-it")
	queued := aMigrateToTheTarget("queued")
	queued.Spec.Migrate.TargetWorkerNode = thirdWorker
	queued.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhasePending

	r, apiClient := aMigrationWorld(t, running, queued)
	third := aWorker(thirdWorker, true)
	third.Status.Addresses[0].Address = "10.0.0.3"
	if err := apiClient.Create(context.Background(), third); err != nil {
		t.Fatalf("add the queued migration's worker: %v", err)
	}

	if err := reconcileTheSlice(r); err != nil {
		t.Fatalf("reconcileEndpointSlice: %v", err)
	}

	got := publishedWorkers(t, apiClient)
	if got[utils.NodeHostnameLabel(opsTarget)] != targetWorkerAddress {
		t.Errorf("published workers = %v, want the running migration's target", got)
	}
	if _, published := got[utils.NodeHostnameLabel(thirdWorker)]; published {
		t.Errorf("published workers = %v, want the queued migration's target left out", got)
	}
}

// The operation that holds the node's lock publishes its target in the instant
// before its phase moves to Running, because the lock is written first.
func TestTheMigrationHoldingTheLockPublishesItsTarget(t *testing.T) {
	holder := aMigrateToTheTarget("move-it")
	holder.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhasePending

	r, apiClient := aMigrationWorld(t, holder)
	var node simplyblockv1alpha2.StorageNode
	key := types.NamespacedName{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("read the node: %v", err)
	}
	node.Status.ActiveOpsRef = holder.Name
	if err := apiClient.Update(context.Background(), &node); err != nil {
		t.Fatalf("take the lock: %v", err)
	}

	if err := reconcileTheSlice(r); err != nil {
		t.Fatalf("reconcileEndpointSlice: %v", err)
	}

	got := publishedWorkers(t, apiClient)
	if got[utils.NodeHostnameLabel(opsTarget)] != targetWorkerAddress {
		t.Errorf("published workers = %v, want the lock holder's target", got)
	}
}

// Only the events that change what the slice publishes wake the pass: a
// migration appearing, disappearing, or changing phase. The status writes a step
// makes about every retry change nothing here and would otherwise rerun the
// whole workload pass each time.
func TestOnlyAMigrationsLifecycleWakesTheWorkloadPass(t *testing.T) {
	running := aMigrateToTheTarget("move-it")
	stepped := running.DeepCopy()
	stepped.Status.Message = "waiting for the node to leave online"
	finished := running.DeepCopy()
	finished.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded

	filter := migrationOpsChanged()
	if !filter.Create(event.CreateEvent{Object: running}) {
		t.Error("a new migration must wake the pass")
	}
	if !filter.Delete(event.DeleteEvent{Object: running}) {
		t.Error("a deleted migration must wake the pass")
	}
	if !filter.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: finished}) {
		t.Error("a phase change must wake the pass")
	}
	if filter.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: stepped}) {
		t.Error("a step write within one phase must not wake the pass")
	}
}

// Regression: 2026-10-22-slice-publishes-a-deleting-nodes-worker — the slice read
// the node list on its own, without the filter the rest of the pass uses, so a
// StorageNode on its way out kept its worker published. The pass treats such a
// node as gone everywhere else (no ConfigMap entry, no enrollment), and the slice
// has to agree with it.
func TestADeletingNodesWorkerIsNotPublished(t *testing.T) {
	r, apiClient := aMigrationWorld(t)
	leaving := anOpsNode()
	leaving.Name = "leaving"
	leaving.Spec.WorkerNode = thirdWorker
	leaving.Finalizers = []string{"test/hold"}
	third := aWorker(thirdWorker, true)
	third.Status.Addresses[0].Address = "10.0.0.3"
	for _, object := range []client.Object{leaving, third} {
		if err := apiClient.Create(context.Background(), object); err != nil {
			t.Fatalf("create %s: %v", object.GetName(), err)
		}
	}
	if err := apiClient.Delete(context.Background(), leaving); err != nil {
		t.Fatalf("start deleting the node: %v", err)
	}

	if err := reconcileTheSlice(r); err != nil {
		t.Fatalf("reconcileEndpointSlice: %v", err)
	}

	got := publishedWorkers(t, apiClient)
	if _, published := got[utils.NodeHostnameLabel(thirdWorker)]; published {
		t.Errorf("published workers = %v, want the deleting node's worker left out", got)
	}
	if got[utils.NodeHostnameLabel(opsWorker)] != sourceWorkerAddress {
		t.Errorf("published workers = %v, want the live node's worker kept", got)
	}
}

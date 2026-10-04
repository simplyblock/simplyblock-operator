// What wakes a reconcile, and what a pass that changed nothing writes.
//
// The mappings are what make the queue move. An operation waiting for a lock has
// nothing of its own to react to, so a controller watching only its own kind
// would leave every queued operation waiting out a requeue interval after the
// lock frees; a node waiting for its cluster's UUID would wait out the same
// interval after the cluster gets one; and a cordon would reach the node it is
// about only by the slow backstop.
//
// The other half is the opposite discipline. Both reconcilers watch their own
// objects, so a status write schedules another pass — which means a pass that
// found nothing new has to write nothing at all, or a node serving I/O
// reconciles itself in a loop for as long as the I/O lasts.
//
// design-crd-model.md §3.2 and design-storagenode.md §12.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// A cluster event wakes its own nodes and nobody else's, which is what the index
// on spec.clusterRef is for.
func TestAClusterEventWakesItsOwnNodes(t *testing.T) {
	elsewhere := anOpsNode()
	elsewhere.Name = "another-clusters-node"
	elsewhere.Spec.ClusterRef = "another-cluster"
	r, _ := aSteadyNode(t, aControlPlane(), elsewhere)

	requests := r.nodesOf(context.Background(), anOpsCluster())

	if len(requests) != 1 || requests[0].Name != opsNodeName {
		t.Errorf("the cluster woke %v, want its own node alone", requests)
	}
}

// A worker event wakes the nodes running on that worker, which is how a cordon
// reaches the node whose maintenance window it is about.
func TestAWorkerEventWakesTheNodesOnIt(t *testing.T) {
	elsewhere := anOpsNode()
	elsewhere.Name = "a-node-on-another-worker"
	elsewhere.Spec.WorkerNode = "worker-9"
	r, _ := aSteadyNode(t, aControlPlane(), elsewhere)

	requests := r.nodesOn(context.Background(), aWorker(opsWorker, true))

	if len(requests) != 1 || requests[0].Name != opsNodeName {
		t.Errorf("the worker woke %v, want the node that runs on it", requests)
	}
}

// A node event wakes every unfinished operation targeting it, which is what lets
// a released lock start the next operation immediately rather than after a
// requeue interval.
func TestANodeEventWakesTheOperationsWaitingOnIt(t *testing.T) {
	queued := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	over := anOperation("a-finished-restart", simplyblockv1alpha2.StorageNodeOpsActionRestart)
	over.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded

	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(anOpsNode(), anOpsCluster(), queued, over).
		WithIndex(&simplyblockv1alpha2.StorageNodeOps{}, nodeRefField,
			func(o client.Object) []string {
				return []string{o.(*simplyblockv1alpha2.StorageNodeOps).Spec.NodeRef}
			}).
		Build()
	r := &StorageNodeOpsReconciler{Client: apiClient, Scheme: scheme}

	requests := r.operationsOn(context.Background(), anOpsNode())

	if len(requests) != 1 || requests[0].Name != "a-suspend" {
		t.Errorf("the node woke %v, want the operation that is still waiting for it", requests)
	}
}

// A pass that found what the object already says writes nothing, because every
// write schedules another pass and a node under load would never settle.
func TestAPassThatFoundNothingNewWritesNothing(t *testing.T) {
	api := aControlPlane()
	api.nodes[opsNodeID] = NodeReading{
		UUID: opsNodeID, Status: nodeStatusOnline, ManagementIP: "10.0.0.1",
		Health: true, Hostname: "vm02_4420", CPUCount: 6, Volumes: 3,
		RPCPort: 4420, LvolPort: 4426, NVMeOFPort: 4421,
	}
	r, apiClient := aSteadyNode(t, api)

	settle(t, r)
	first := nodeRead(t, apiClient).ResourceVersion

	settle(t, r)
	second := nodeRead(t, apiClient).ResourceVersion

	if first != second {
		t.Errorf("the object moved from %s to %s on a pass that learned nothing, "+
			"and every write schedules another pass", first, second)
	}
}

// A reading that did move is written, or the object would report a node it has
// stopped describing.
func TestAPassThatFoundSomethingNewWritesIt(t *testing.T) {
	api := aControlPlane()
	r, apiClient := aSteadyNode(t, api)

	settle(t, r)
	before := nodeRead(t, apiClient).ResourceVersion

	api.reporting(nodeStatusSuspended)
	settle(t, r)

	after := nodeRead(t, apiClient)
	if after.ResourceVersion == before {
		t.Error("the node still reports online after the control plane suspended it")
	}
	if after.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOffline {
		t.Errorf("phase = %q, want Offline for a suspended node", after.Status.Phase)
	}
}

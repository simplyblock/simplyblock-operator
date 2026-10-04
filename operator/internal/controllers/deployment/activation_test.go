// Whether a finished deployment activates the cluster it built.
//
// The document knows what it created: status.nodeRefs names every StorageNode
// the expansion made, so it knows exactly how many have to come online before
// the cluster is whole. A deployment that stops at "the objects exist" leaves a
// cluster that serves nothing and an administrator holding a document that says
// Expanded, with no indication that one more thing is required of them.
//
// So the expansion waits for the nodes it created and then asks for the
// activation itself. The request is a StorageClusterOps like any other, raised
// by name so that re-entering the step finds the one it raised rather than
// asking twice.

package deployment

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// anExpandedDocument is a document whose nodes have been created, with the node
// objects in the phase given.
func anExpandedDocument(
	t *testing.T, phase simplyblockv1alpha2.StorageNodePhase,
) (*simplyblockv1alpha2.ClusterDeploymentConfig, *ClusterDeploymentConfigReconciler) {
	t.Helper()

	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	names := []string{theCluster + "-worker-1-0", theCluster + "-worker-2-0"}
	config.Status.NodeRefs = names

	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	for _, name := range names {
		node := &simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: theNamespace},
			Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: theCluster},
		}
		node.Status.Phase = phase
		objects = append(objects, node)
	}
	return config, reconcilerFor(t, objects...)
}

// Every node online is the deployment finished, so the activation is asked for.
func TestTheDeploymentActivatesOnceItsNodesAreOnline(t *testing.T) {
	config, r := anExpandedDocument(t, simplyblockv1alpha2.StorageNodePhaseOnline)

	done, err := r.activateCluster(context.Background(), config)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !done {
		t.Error("the step did not finish with every node online")
	}

	var ops simplyblockv1alpha2.StorageClusterOpsList
	if err := r.List(context.Background(), &ops); err != nil {
		t.Fatalf("listing the operations: %v", err)
	}
	if len(ops.Items) != 1 {
		t.Fatalf("raised %d operations, want the one activation", len(ops.Items))
	}
	if got := ops.Items[0].Spec.Action; got != simplyblockv1alpha2.StorageClusterOpsActionActivate {
		t.Errorf("raised a %q operation", got)
	}
	if got := ops.Items[0].Spec.ClusterRef; got != theCluster {
		t.Errorf("the operation names cluster %q", got)
	}
}

// A node still provisioning is a deployment that is not finished, so nothing is
// asked for yet.
func TestTheDeploymentWaitsWhileANodeIsStillComingUp(t *testing.T) {
	config, r := anExpandedDocument(t, simplyblockv1alpha2.StorageNodePhaseProvisioning)

	done, err := r.activateCluster(context.Background(), config)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if done {
		t.Error("the step finished while a node was still provisioning")
	}

	var ops simplyblockv1alpha2.StorageClusterOpsList
	if err := r.List(context.Background(), &ops); err != nil {
		t.Fatalf("listing the operations: %v", err)
	}
	if len(ops.Items) != 0 {
		t.Errorf("raised %d operations before the nodes were up", len(ops.Items))
	}
}

// Re-entering the step finds the operation it raised rather than raising a
// second one, which is what makes the step safe to repeat.
func TestTheActivationIsAskedForOnce(t *testing.T) {
	config, r := anExpandedDocument(t, simplyblockv1alpha2.StorageNodePhaseOnline)

	for range 3 {
		if _, err := r.activateCluster(context.Background(), config); err != nil {
			t.Fatalf("activate: %v", err)
		}
	}

	var ops simplyblockv1alpha2.StorageClusterOpsList
	if err := r.List(context.Background(), &ops, client.InNamespace(theNamespace)); err != nil {
		t.Fatalf("listing the operations: %v", err)
	}
	if len(ops.Items) != 1 {
		t.Errorf("three passes raised %d operations", len(ops.Items))
	}
}

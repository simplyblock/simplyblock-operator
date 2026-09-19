// A removal whose target the control plane no longer has.
//
// Removing a node is a sequence of calls against a backend node: suspend it,
// move its volumes, verify, delete it. Every one of them is written to tolerate a
// repeat, because a step recorded without its call having fired re-issues it. But
// a repeat is not the only thing that happens twice — the node can also be gone
// before the sequence reaches the step that would have removed it, either because
// an earlier attempt got that far or because somebody else removed it.
//
// The last step already reads a 404 as success. The earlier ones did not, so a
// removal that found its node missing at Suspending reported the 404 as a step
// that could not be advanced and retried it for as long as the operator ran:
//
//	the step could not be advanced ... step: Suspending
//	  error: suspend node ...: the control plane answered 404:
//	         'StorageNode 7838e194-... not found'
//
// Which is a removal refusing to finish because what it was removing is gone.

package node

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// goneControlPlane reports that the node is not there. Everything else is nil:
// a removal that reaches past this is reaching for a node it has been told does
// not exist, and should panic rather than pass.
type goneControlPlane struct {
	ControlPlane
}

func (goneControlPlane) StorageNode(
	context.Context, string, string,
) (NodeReading, bool, error) {
	return NodeReading{}, false, nil
}

func aRemoveOps() *simplyblockv1alpha2.StorageNodeOps {
	ops := &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-node-remove", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageNodeOpsSpec{
			Action:  simplyblockv1alpha2.StorageNodeOpsActionRemove,
			NodeRef: "a-node",
		},
	}
	return ops
}

func aRemover(t *testing.T) *StorageNodeOpsReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t)

	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: opsNodeName, Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "a-cluster"},
	}
	node.Status.UUID = "node-uuid"

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "a-cluster", Namespace: "simplyblock"},
	}
	cluster.Status.UUID = aBackendClusterID

	ops := aRemoveOps()
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, cluster, ops).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNodeOps{}).
		Build()

	return &StorageNodeOpsReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      goneControlPlane{},
	}
}

// Every step of a removal is done once the node is gone, because gone is what
// the removal was for.
func TestARemovalOfAGoneNodeIsDoneAtEveryStep(t *testing.T) {
	r := aRemover(t)

	for _, current := range []step{
		stepValidating, stepSuspending, stepMigratingVolumes, stepVerifying, stepRemoving,
	} {
		done, err := r.performRemoveStep(context.Background(), aRemoveOps(), current)
		if err != nil {
			t.Errorf("step %s: %v", current, err)
		}
		if !done {
			t.Errorf("step %s did not finish against a node the control plane does not have", current)
		}
	}
}

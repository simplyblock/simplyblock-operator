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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"

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

// A document that reaches Expanded reports the terminal step Completed, so
// `kubectl get` shows no step it is still on.
func TestAnExpandedDocumentReportsCompleted(t *testing.T) {
	config, r := anExpandedDocument(t, simplyblockv1alpha2.StorageNodePhaseOnline)
	config.Spec.Approved = true
	config.Status.Phase = simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding
	config.Status.Step = statemachine.KubeSnapshot{State: string(stepActivating)}
	if err := r.Status().Update(context.Background(), config); err != nil {
		t.Fatalf("record the step: %v", err)
	}

	if _, err := r.expand(context.Background(), config); err != nil {
		t.Fatalf("expand: %v", err)
	}

	stored := readDocument(t, r)
	if stored.Status.Phase != simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded {
		t.Fatalf("phase = %q, want Expanded", stored.Status.Phase)
	}
	if stored.Status.Step.State != string(simplyblockv1alpha2.ClusterDeploymentConfigStepCompleted) ||
		stored.Status.Step.Deadline != nil {
		t.Errorf("step = %+v, want Completed with no deadline", stored.Status.Step)
	}
}

func setPhaseOf(
	t *testing.T, r *ClusterDeploymentConfigReconciler, name string, phase simplyblockv1alpha2.StorageNodePhase,
) {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: theNamespace, Name: name}
	if err := r.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	node.Status.Phase = phase
	if err := r.Update(context.Background(), &node); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// Regression: 2026-11-25-activation-deadline-counts-node-adds — the first node
// add of a six-node deployment was stuck for ninety minutes. The Activating
// step counted that wait against its own hour, the document failed, and a
// failed document is terminal: when all six nodes came online forty minutes
// later nothing asked for the activation, and the cluster stayed unready.
func TestNodesStillBeingProvisionedDoNotRunOutTheActivationDeadline(t *testing.T) {
	ctx := context.Background()
	config, r := anExpandedDocument(t, simplyblockv1alpha2.StorageNodePhaseProvisioning)
	config = withActivatingStep(t, r, config, time.Now().Add(-time.Minute))

	if _, err := r.expand(ctx, config); err != nil {
		t.Fatalf("expand: %v", err)
	}
	held := reread(t, r, config)
	if held.Status.Phase == simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed {
		t.Fatalf("the document failed while its nodes were being provisioned: %s", held.Status.Message)
	}
	if deadline, has := held.Status.Step.KubeDeadline(); !has || !deadline.After(time.Now()) {
		t.Errorf("the wait for the nodes left the step's deadline at %v; it must not run while a node is on its way", deadline)
	}
	if !strings.Contains(held.Status.Message, config.Status.NodeRefs[0]) {
		t.Errorf("message = %q, want the node the document is waiting on", held.Status.Message)
	}
	if !emitted(r, AwaitingNodes) {
		t.Error("a document waiting for its nodes emitted no AwaitingNodes")
	}

	// The nodes arrive, long after the deadline the step entered with.
	for _, name := range config.Status.NodeRefs {
		setPhaseOf(t, r, name, simplyblockv1alpha2.StorageNodePhaseOnline)
	}
	if _, err := r.expand(ctx, held); err != nil {
		t.Fatalf("expand once the nodes are online: %v", err)
	}
	var ops simplyblockv1alpha2.StorageClusterOpsList
	if err := r.List(ctx, &ops, client.InNamespace(theNamespace)); err != nil {
		t.Fatalf("listing the operations: %v", err)
	}
	if len(ops.Items) != 1 || ops.Items[0].Spec.Action != simplyblockv1alpha2.StorageClusterOpsActionActivate {
		t.Errorf("the nodes came online and %d operations were raised; want the one activation", len(ops.Items))
	}
	if got := reread(t, r, config).Status.Phase; got != simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded {
		t.Errorf("phase = %q, want Expanded", got)
	}
}

// The deadline still bounds a deployment that is not going to finish: a node
// that failed is not on its way, whatever the others are doing.
func TestANodeThatFailedStillRunsOutTheActivationDeadline(t *testing.T) {
	for _, others := range []simplyblockv1alpha2.StorageNodePhase{
		simplyblockv1alpha2.StorageNodePhaseOnline,
		simplyblockv1alpha2.StorageNodePhaseProvisioning,
	} {
		ctx := context.Background()
		config, r := anExpandedDocument(t, others)
		setPhaseOf(t, r, config.Status.NodeRefs[1], simplyblockv1alpha2.StorageNodePhaseFailed)
		config = withActivatingStep(t, r, config, time.Now().Add(-time.Minute))

		if _, err := r.expand(ctx, config); err != nil {
			t.Fatalf("expand: %v", err)
		}
		if got := reread(t, r, config); got.Status.Phase != simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed {
			t.Errorf("with one node Failed and the other %s, phase = %q; want Failed past the deadline",
				others, got.Status.Phase)
		}
	}
}

// Within the deadline nothing is rewritten on every pass: the deadline is moved
// only once half of it is spent.
func TestTheActivationDeadlineIsNotRewrittenOnEveryPass(t *testing.T) {
	ctx := context.Background()
	config, r := anExpandedDocument(t, simplyblockv1alpha2.StorageNodePhaseProvisioning)
	entered := time.Now().Add(activatingDeadline - time.Minute).Truncate(time.Second)
	config = withActivatingStep(t, r, config, entered)

	if _, err := r.expand(ctx, config); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if deadline, _ := reread(t, r, config).Status.Step.KubeDeadline(); !deadline.Equal(entered) {
		t.Errorf("the deadline moved from %v to %v with most of it left", entered, deadline)
	}
}

// The removal's admission, asked in Validating while the node still serves.
//
// From the shutdown on there is no way back, so whatever would refuse the
// removal has to be asked before it: the failure-domain balance the removal
// would leave, and the control plane's own admission (fault-tolerance
// headroom, replica relocation, active tasks). A refusal holds the drain in
// Validating, where nothing has been done, rather than failing it with the
// node already offline.
//
// design-storagenode.md §8.2 and §8.3.

package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aNodeOnWorker is another storage node of the cluster, on the given worker
// and in the given failure domain.
func aNodeOnWorker(name, worker, domain string) *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: opsNamespace},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: opsCluster, WorkerNode: worker},
	}
	node.Status.FailureDomain = domain
	node.Status.UUID = name + "-uuid"
	return node
}

// inFailureDomains turns failure domains on for the cluster and puts the
// operation's node in domain.
func inFailureDomains(t *testing.T, c client.Client, domain string) {
	t.Helper()
	ctx := context.Background()
	var cluster simplyblockv1alpha2.StorageCluster
	if err := c.Get(ctx, types.NamespacedName{Name: opsCluster, Namespace: opsNamespace}, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.EnableFailureDomains = ptr.To(true)
	if err := c.Update(ctx, &cluster); err != nil {
		t.Fatal(err)
	}
	var node simplyblockv1alpha2.StorageNode
	if err := c.Get(ctx, types.NamespacedName{Name: opsNodeName, Namespace: opsNamespace}, &node); err != nil {
		t.Fatal(err)
	}
	node.Status.FailureDomain = domain
	if err := c.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
}

// notAdmitted asserts that Validating held on the admission.
func notAdmitted(t *testing.T, done bool, err error, says string) {
	t.Helper()
	var blocked *blockedStepError
	if !errors.As(err, &blocked) || blocked.reason != RemovalNotAdmitted {
		t.Fatalf("err = %v, want Validating held with %s", err, RemovalNotAdmitted)
	}
	if !strings.Contains(blocked.message, says) {
		t.Errorf("message = %q, want it to say %q", blocked.message, says)
	}
	if done {
		t.Error("Validating finished on a removal the admission refuses")
	}
}

// Regression: 2026-10-05-removal-admission-after-shutdown — the rework dropped
// the drain's failure-domain gate, so a removal that would leave a domain short
// of hosts was refused only by prepare-removal, after the drain had shut the
// node down, and the node was left offline.
func TestValidationHoldsOnAFailureDomainTheRemovalWouldBreak(t *testing.T) {
	r, c := anOpsWorld(t, aControlPlane(),
		aNodeOnWorker("node-b", "worker-2", "1"),
		aNodeOnWorker("node-c", "worker-3", "2"),
		aNodeOnWorker("node-d", "worker-4", "2"))
	inFailureDomains(t, c, "1")

	done, err := performing(t, r, aDrain(), stepValidating)
	notAdmitted(t, done, err, "failure domain 1")
}

// A worker running more than one storage node stays in its domain when one of
// them is removed, so the count the gate checks does not drop.
func TestASiblingOnTheSameWorkerKeepsTheHostInItsDomain(t *testing.T) {
	r, c := anOpsWorld(t, aControlPlane(),
		aNodeOnWorker("node-a2", opsWorker, "1"),
		aNodeOnWorker("node-b", "worker-2", "1"),
		aNodeOnWorker("node-c", "worker-3", "2"),
		aNodeOnWorker("node-d", "worker-4", "2"))
	inFailureDomains(t, c, "1")

	if done, err := performing(t, r, aDrain(), stepValidating); err != nil || !done {
		t.Errorf("done, err = %v, %v; the worker keeps a node in domain 1", done, err)
	}
}

// Regression: 2026-10-05-removal-admission-after-shutdown — the control plane's
// admission ran only inside prepare-removal, after the shutdown. Asked in
// Validating, a refusal holds the drain with the node still serving.
func TestValidationHoldsOnTheControlPlanesRefusal(t *testing.T) {
	const reason = "FTT=1 (npcs=1): cannot remove node, cluster already has 1 not-online node(s)"
	api := aControlPlane()
	api.admission = &RemovalAdmission{Admitted: false, Reason: reason}
	r, _ := anOpsWorld(t, api)

	done, err := performing(t, r, aDrain(), stepValidating)
	notAdmitted(t, done, err, reason)
}

// A control plane without the check is not a reason to hold: the admission
// still runs inside prepare-removal, as it always did.
func TestAControlPlaneWithoutTheAdmissionCheckIsNoHold(t *testing.T) {
	api := aControlPlane()
	api.admissionAbsent = true
	r, _ := anOpsWorld(t, api)

	if done, err := performing(t, r, aDrain(), stepValidating); err != nil || !done {
		t.Errorf("done, err = %v, %v; want Validating to finish", done, err)
	}
}

// Regression: 2026-10-06-admission-refusal-without-reason — the refusal was
// carried by its reason text, so a control plane answering admitted false with
// no reason read as an admission, and the drain went on to shut the node down.
func TestARefusalWithNoReasonStillHoldsValidation(t *testing.T) {
	api := aControlPlane()
	api.admission = &RemovalAdmission{Admitted: false}
	r, _ := anOpsWorld(t, api)

	done, err := performing(t, r, aDrain(), stepValidating)
	notAdmitted(t, done, err, "gave no reason")
}

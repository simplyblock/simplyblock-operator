// The StorageNodeOps guard: a validating webhook on DELETE that refuses to
// withdraw the record of an operation sitting on a step the graph declares no
// abort edge from.
//
// The operation's finalizer is the other half of this, and it is the weaker
// half: it releases the node's lock and aborts the fan-out, but it runs only on
// a graceful delete. A `kubectl delete --force --grace-period=0` skips it
// entirely, and admission is the only thing left in the path.
//
// What that costs on this kind is specific. A migrate at Promoting has already
// activated the target host's devices, failed the origin's, and re-homed the
// logical volumes, so there is nothing to unwind and the operation is what
// finishes the relocation. The record is what carries the topology re-point that
// is still owed, and withdrawing it leaves spec.workerNode and the worker's
// labels describing a host the node no longer runs on, with nothing left that
// knows to correct them.
//
// Deletion is therefore never a way to express something spec.abort could not:
// both channels ask one graph, and this file asks it through
// node.UnabortableSteps rather than restating the answer. What it states here is
// only the prose, because a refusal has to say what the step is waiting for and
// a bool cannot.
//
// design-crd-model.md §3.1 and design-storagenode.md §11 are the specification.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagenodeops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storagenodeops,verbs=delete,versions=v1alpha2,name=vstoragenodeops.simplyblock.io,admissionReviewVersions=v1

// StorageNodeOpsValidator refuses a delete that would strand a node operation
// mid-flight.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and while the operator is down nothing
// advances an operation anyway.
//
// It holds no client. Everything the decision rests on is in the object being
// deleted, which is what makes the answer the same on every replica and immune
// to a stale cache.
type StorageNodeOpsValidator struct{}

// undeletableNodeSteps says what each step with no abort edge is in the middle
// of. Which steps those are is node.UnabortableSteps's answer, and a test holds
// the two sets equal in both directions: an entry here for a step the graph can
// abort from refuses a delete the abort channel would have honored, and a
// missing entry admits the withdrawal of a record nothing else accounts for.
var undeletableNodeSteps = map[simplyblockv1alpha2.StorageNodeOpsStep]string{
	simplyblockv1alpha2.StorageNodeOpsStepAwaiting: "the control plane is carrying out the " +
		"action, and it is doing so whether or not this record exists",
	simplyblockv1alpha2.StorageNodeOpsStepRemoving: "the node is being taken out of the cluster",
	simplyblockv1alpha2.StorageNodeOpsStepRelocating: "the node is being restarted onto its " +
		"target host",
	simplyblockv1alpha2.StorageNodeOpsStepAwaitingNode: "the node is part-way through that " +
		"restart and this operation is the only thing watching it back",
	simplyblockv1alpha2.StorageNodeOpsStepPromoting: "the promote has activated the target " +
		"host's devices, failed the origin's, and re-homed the logical volumes, so the topology " +
		"re-point is all that is left and this record is what carries it",
	simplyblockv1alpha2.StorageNodeOpsStepShuttingDown: "the node is being taken down for host " +
		"maintenance",
	simplyblockv1alpha2.StorageNodeOpsStepReleasing: "the host is being handed over for " +
		"maintenance",
	simplyblockv1alpha2.StorageNodeOpsStepAwaitingHost: "the host is away, and this operation " +
		"is the only thing that will bring the node back from it",
	simplyblockv1alpha2.StorageNodeOpsStepRestarting: "the node is coming back from the " +
		"maintenance shutdown",
	simplyblockv1alpha2.StorageNodeOpsStepCleanup: "the labels and the disruption budget the " +
		"maintenance put in place are being taken back",
}

func (v *StorageNodeOpsValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Delete {
		return admission.Allowed("")
	}
	return v.admitDelete(req)
}

// admitDelete reads the object from req.OldObject, which is what the API server
// sends on a DELETE: there is no new object, and the step the operation is on is
// in the status of the one being removed.
func (v *StorageNodeOpsValidator) admitDelete(req admission.Request) admission.Response {
	if len(req.OldObject.Raw) == 0 {
		// Nothing to read means nothing to refuse on. Admitting is the only
		// answer that does not block a delete on the basis of no information.
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha2.StorageNodeOps
	if err := json.Unmarshal(req.OldObject.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// A terminal operation is a record of work that has finished, and
	// withdrawing it stops nothing.
	switch ops.Status.Phase {
	case simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageNodeOpsPhaseFailed,
		simplyblockv1alpha2.StorageNodeOpsPhaseAborted:
		return admission.Allowed("the operation is terminal")
	}

	step := simplyblockv1alpha2.StorageNodeOpsStep(ops.Status.Step.State)
	doing, undeletable := undeletableNodeSteps[step]
	if !undeletable {
		return admission.Allowed("")
	}

	return admission.Denied(fmt.Sprintf(
		"StorageNodeOps %s/%s is at step %s, where %s. Deleting the record would not stop that "+
			"work, it would remove the only thing accounting for it and leave node %s locked by "+
			"an object that no longer exists. Set spec.abort to stop an operation that can still "+
			"be stopped, and delete the record once it is terminal.",
		ops.Namespace, ops.Name, step, doing, ops.Spec.NodeRef))
}

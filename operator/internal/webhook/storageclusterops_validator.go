// The StorageClusterOps guard: a validating webhook on DELETE that refuses to
// withdraw the record of an operation sitting on a step the graph declares no
// abort edge from.
//
// The operation's finalizer releases the cluster's lock on a graceful delete,
// and a `kubectl delete --force --grace-period=0` skips it. Admission is what is
// left in that path, and on this kind the blast radius is the widest in the
// group: the lock this operation holds is the cluster's, so an abandoned one
// leaves every later cluster operation waiting on an object nobody can find.
//
// A rolling restart is the case that decides the rule. The walk holds the lock
// across every node of the fleet, and from ShuttingDownNode to RestartingNode
// the node it is on is offline with this operation the only thing that will
// bring it back. Withdrawing the record there does not stop the walk, it leaves
// a storage node down and the cluster locked.
//
// Deletion is therefore never a way to express something spec.abort could not:
// both channels ask one graph, and this file asks it through
// cluster.UnabortableSteps rather than restating the answer. What it states here
// is only the prose, because a refusal has to say what the step is waiting for
// and a bool cannot.
//
// design-crd-model.md §3.1 and design-storagecluster.md §7 are the
// specification.

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

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storageclusterops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storageclusterops,verbs=delete,versions=v1alpha2,name=vstorageclusterops.simplyblock.io,admissionReviewVersions=v1

// StorageClusterOpsValidator refuses a delete that would strand a cluster
// operation mid-flight.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and while the operator is down nothing
// advances an operation anyway.
//
// It holds no client. Everything the decision rests on is in the object being
// deleted, which is what makes the answer the same on every replica and immune
// to a stale cache.
type StorageClusterOpsValidator struct{}

// undeletableClusterSteps says what each step with no abort edge is in the
// middle of. Which steps those are is cluster.UnabortableSteps's answer, and a
// test holds the two sets equal in both directions: an entry here for a step the
// graph can abort from refuses a delete the abort channel would have honored,
// and a missing entry admits the withdrawal of a record nothing else accounts
// for.
var undeletableClusterSteps = map[simplyblockv1alpha2.StorageClusterOpsStep]string{
	simplyblockv1alpha2.StorageClusterOpsStepAwaiting: "the cluster is carrying out the action, " +
		"and it is doing so whether or not this record exists",
	simplyblockv1alpha2.StorageClusterOpsStepShuttingDown: "the cluster is going down, and the " +
		"start that follows is this operation's second half",
	simplyblockv1alpha2.StorageClusterOpsStepStarting: "the cluster is coming back from the " +
		"shutdown this operation performed",
	simplyblockv1alpha2.StorageClusterOpsStepShuttingDownNode: "a storage node is being taken " +
		"offline, and this walk is the only thing that will bring it back",
	simplyblockv1alpha2.StorageClusterOpsStepRefreshingPod: "the node is offline and its " +
		"storage-node pod is being replaced",
	simplyblockv1alpha2.StorageClusterOpsStepAwaitingPod: "the node is offline and its " +
		"replacement pod is still coming up",
	simplyblockv1alpha2.StorageClusterOpsStepRestartingNode: "the node is offline and is being " +
		"brought back from the shutdown this walk performed",
}

func (v *StorageClusterOpsValidator) Handle(
	_ context.Context, req admission.Request,
) admission.Response {
	if req.Operation != admissionv1.Delete {
		return admission.Allowed("")
	}
	return v.admitDelete(req)
}

// admitDelete reads the object from req.OldObject, which is what the API server
// sends on a DELETE: there is no new object, and the step the operation is on is
// in the status of the one being removed.
func (v *StorageClusterOpsValidator) admitDelete(req admission.Request) admission.Response {
	if len(req.OldObject.Raw) == 0 {
		// Nothing to read means nothing to refuse on. Admitting is the only
		// answer that does not block a delete on the basis of no information.
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha2.StorageClusterOps
	if err := json.Unmarshal(req.OldObject.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// A terminal operation is a record of work that has finished, and
	// withdrawing it stops nothing.
	switch ops.Status.Phase {
	case simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageClusterOpsPhaseFailed,
		simplyblockv1alpha2.StorageClusterOpsPhaseAborted:
		return admission.Allowed("the operation is terminal")
	}

	step := simplyblockv1alpha2.StorageClusterOpsStep(ops.Status.Step.State)
	doing, undeletable := undeletableClusterSteps[step]
	if !undeletable {
		return admission.Allowed("")
	}

	return admission.Denied(fmt.Sprintf(
		"StorageClusterOps %s/%s is at step %s, where %s. Deleting the record would not stop "+
			"that work, it would remove the only thing accounting for it and leave cluster %s "+
			"locked by an object that no longer exists. Set spec.abort to stop an operation that "+
			"can still be stopped, and delete the record once it is terminal.",
		ops.Namespace, ops.Name, step, doing, ops.Spec.ClusterRef))
}

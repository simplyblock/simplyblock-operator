// The PersistentVolumeOps guard: a validating webhook that refuses at create
// what no reconcile could ever make work, and refuses at delete what would
// strand a volume mid-move.
//
// The line it draws is between what cannot change and what can. A condition
// fixed for the object's whole life is a rejection; a condition true at one
// moment and false at the next is a phase and an event. An admission decision
// is made once and never revisited, so anything time-varying decided here would
// be decided wrongly for most of the object's life — which is why a node that
// is offline and a cluster that has not been created yet are both admitted.
//
// The agreement between spec.action and its parameter block is not here. CEL
// can see it, since it is a statement about one object's own fields, and the
// group's floor is that such a rule lives on the type where nothing can be
// installed in front of it. What is left for the webhook is every row that is a
// fact about a different object, which CEL cannot reach.
//
// design-persistentvolumeops.md §4.3 is the specification.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/volume"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-persistentvolumeops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=persistentvolumeops,verbs=create;delete,versions=v1alpha2,name=vpersistentvolumeops.simplyblock.io,admissionReviewVersions=v1

// PersistentVolumeOpsValidator stands in front of creating and deleting a
// volume operation.
//
// failurePolicy=Fail, for the reason the StorageNode validator gives: the
// webhook server runs inside the operator pod, so its availability tracks the
// operator's, and while the operator is down no migration would run anyway.
//
// Unlike the other Ops guards in this package it holds a client, because its
// create rules are facts about other objects: the volume, its driver, its
// handle, and the node the operation names.
type PersistentVolumeOpsValidator struct {
	Client client.Client
}

// undeletableVolumeSteps says what each step with no abort edge is in the
// middle of. Which steps those are is volume.UnabortableSteps's answer, and a
// test holds the two sets equal in both directions.
var undeletableVolumeSteps = map[simplyblockv1alpha2.PersistentVolumeOpsStep]string{
	simplyblockv1alpha2.PersistentVolumeOpsStepVerifying: "the copy has finished and the volume " +
		"has already moved, so what is left is the cleanup that makes the move safe: the " +
		"validation Jobs and the paths they connected on every consuming host",
}

func (v *PersistentVolumeOpsValidator) Handle(
	ctx context.Context, req admission.Request,
) admission.Response {
	switch req.Operation {
	case admissionv1.Create:
		return v.admitCreate(ctx, req)
	case admissionv1.Delete:
		return v.admitDelete(req)
	default:
		return admission.Allowed("")
	}
}

// admitCreate refuses an operation that could never run.
func (v *PersistentVolumeOpsValidator) admitCreate(
	ctx context.Context, req admission.Request,
) admission.Response {
	if len(req.Object.Raw) == 0 {
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := json.Unmarshal(req.Object.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	handle, denied := v.addressableVolume(ctx, &ops)
	if denied != nil {
		return *denied
	}
	return v.targetInTheVolumesCluster(ctx, &ops, handle)
}

// addressableVolume refuses a volume this operator has no means to move, and
// returns the handle the cluster is read out of when it can.
func (v *PersistentVolumeOpsValidator) addressableVolume(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (lvol.Handle, *admission.Response) {
	name := ops.Spec.PersistentVolumeName

	var pv corev1.PersistentVolume
	err := v.Client.Get(ctx, types.NamespacedName{Name: name}, &pv)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing creates an operation before its volume, so this is a typo.
		return lvol.Handle{}, denied(
			"there is no PersistentVolume %s to move. spec.persistentVolumeName names the "+
				"volume rather than the claim, because a claim can be deleted while its volume "+
				"is retained.", name)
	case err != nil:
		return lvol.Handle{}, errored(err)
	}

	if pv.Spec.CSI == nil {
		return lvol.Handle{}, denied(
			"volume %s has no CSI source, so it has no logical volume to move.", name)
	}

	// The check that earns this webhook. Every other refusal here is a
	// malformed request; a volume belonging to another CSI driver is a
	// well-formed request against the wrong object, and it is the mistake
	// somebody writing one by hand is likeliest to make, because `kubectl get
	// pv` lists every volume in the cluster and says nothing about which of
	// them this operator can move. Answering at the moment the mistake is made,
	// with the driver's name in the message, beats leaving an object sitting in
	// Failed for somebody to read.
	ours, err := v.driverNames(ctx)
	if err != nil {
		return lvol.Handle{}, errored(err)
	}
	if !ours[pv.Spec.CSI.Driver] {
		return lvol.Handle{}, denied(
			"volume %s was provisioned by driver %s, and this operator can only move volumes of "+
				"%s.", name, pv.Spec.CSI.Driver, joined(ours))
	}

	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !ok {
		return lvol.Handle{}, denied(
			"volume %s carries the volume handle %q, which is not <cluster>:<pool>:<volume>, so "+
				"there is nothing to address the backend with.", name, pv.Spec.CSI.VolumeHandle)
	}
	return handle, nil
}

// targetInTheVolumesCluster refuses a target node that does not exist and one
// that belongs to another cluster than the volume.
//
// The namespace is carried explicitly rather than derived, so that a
// hand-written spec can be read without performing a join to learn which object
// it names. That makes the mistake writable, and this is where it is refused,
// with both clusters in the message.
func (v *PersistentVolumeOpsValidator) targetInTheVolumesCluster(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, handle lvol.Handle,
) admission.Response {
	if ops.Spec.Migrate == nil {
		// The type's own rule covers this, and it runs whether or not the
		// webhook is installed.
		return admission.Allowed("")
	}
	ref := ops.Spec.Migrate.TargetNodeRef

	var node simplyblockv1alpha2.StorageNode
	err := v.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &node)
	switch {
	case apierrors.IsNotFound(err):
		return *denied("there is no StorageNode %s/%s to move volume %s to.",
			ref.Namespace, ref.Name, ops.Spec.PersistentVolumeName)
	case err != nil:
		return *errored(err)
	}

	var cluster simplyblockv1alpha2.StorageCluster
	err = v.Client.Get(ctx,
		client.ObjectKey{Namespace: node.Namespace, Name: node.Spec.ClusterRef}, &cluster)
	switch {
	case apierrors.IsNotFound(err):
		// A node naming a cluster that does not exist is a broken node rather
		// than a broken migration, and refusing here would report it against
		// the wrong object.
		return admission.Allowed("")
	case err != nil:
		return *errored(err)
	}

	if cluster.Status.UUID == "" {
		// The cluster has not been created in the backend, so there is nothing
		// to compare the volume's cluster against. That is a not-yet rather
		// than a mismatch, and it becomes the controller's condition.
		return admission.Allowed("")
	}

	if cluster.Status.UUID != handle.ClusterID {
		return *denied(
			"node %s/%s belongs to cluster %s and volume %s to cluster %s, and a volume cannot "+
				"move between clusters.",
			ref.Namespace, ref.Name, cluster.Status.UUID,
			ops.Spec.PersistentVolumeName, handle.ClusterID)
	}
	return admission.Allowed("")
}

// driverNames is the set of CSI drivers whose volumes this operator can move,
// read from the SimplyblockDriver objects because spec.driverName is settable
// and immutable: a deployment that chose another name at install has volumes
// carrying that name forever.
func (v *PersistentVolumeOpsValidator) driverNames(ctx context.Context) (map[string]bool, error) {
	var drivers simplyblockv1alpha2.SimplyblockDriverList
	if err := v.Client.List(ctx, &drivers); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for i := range drivers.Items {
		if name := drivers.Items[i].Spec.DriverName; name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		names[volume.CSIDriverName] = true
	}
	return names, nil
}

// admitDelete reads the object from req.OldObject, which is what the API server
// sends on a DELETE: there is no new object, and the step the operation is on is
// in the status of the one being removed.
func (v *PersistentVolumeOpsValidator) admitDelete(req admission.Request) admission.Response {
	if len(req.OldObject.Raw) == 0 {
		// Nothing to read means nothing to refuse on. Admitting is the only
		// answer that does not block a delete on the basis of no information.
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := json.Unmarshal(req.OldObject.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	switch ops.Status.Phase {
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted:
		return admission.Allowed("the operation is terminal")
	}

	step := simplyblockv1alpha2.PersistentVolumeOpsStep(ops.Status.Step.State)
	doing, undeletable := undeletableVolumeSteps[step]
	if !undeletable {
		return admission.Allowed("")
	}

	return *denied(
		"PersistentVolumeOps %s is at step %s, where %s. Deleting the record would not undo the "+
			"move, it would remove the only thing naming those paths, and a path left connected "+
			"with nothing tracking it blocks every later migration of volume %s. Set spec.abort "+
			"to stop an operation that can still be stopped, and delete the record once it is "+
			"terminal.", ops.Name, step, doing, ops.Spec.PersistentVolumeName)
}

// denied builds a refusal, as a pointer so a helper can return "no refusal."
func denied(format string, args ...any) *admission.Response {
	response := admission.Denied(fmt.Sprintf(format, args...))
	return &response
}

func errored(err error) *admission.Response {
	response := admission.Errored(http.StatusInternalServerError, err)
	return &response
}

// joined renders the driver names a message lists, sorted so the message is the
// same on every replica.
func joined(names map[string]bool) string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

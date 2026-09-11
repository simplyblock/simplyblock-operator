// The StorageBackupOps guard: a validating webhook that resolves every
// reference a restore names before the object is admitted, and refuses a DELETE
// from a step the action's graph declares no way out of.
//
// Every reference it checks is immutable, which is what makes admission the
// right place: a reference that is wrong at creation is wrong for the object's
// whole life, and the only remedy is to delete the object and write it again,
// which is what a rejected create asks for at no cost
// (design-storagebackup.md §7).
//
// The claim-name check is the one row where admission narrows a race it cannot
// close. A claim can be created between this and the operation's Validating
// step, so the step keeps its own check. Both are needed, and the destructive
// case is the reason: replacing a running workload's data with a backup's is the
// worst outcome this band can produce, so it is guarded twice rather than once.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagebackupops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storagebackupops,verbs=create;delete,versions=v1alpha2,name=vstoragebackupops.simplyblock.io,admissionReviewVersions=v1

// undeletableSteps are the steps from which a running operation's record may not
// be withdrawn.
//
// Both have a logical volume the control plane created behind them and a claim
// the operation is on its way to binding, so removing the record leaves both with
// nothing accounting for them. It is the same set the controller refuses an abort
// from, read from the same graph: the two channels ask one question, and the one
// that carries a reason forward is spec.abort, because it leaves an Aborted
// object to read where a delete leaves nothing.
//
// The webhook is the stronger of the two guards, because it also catches the
// `--force --grace-period=0` that a finalizer alone does not.
var undeletableSteps = map[simplyblockv1alpha2.StorageBackupOpsStep]string{
	simplyblockv1alpha2.StorageBackupOpsStepAwaitingVolume: "the control plane is still filling the " +
		"restored volume",
	simplyblockv1alpha2.StorageBackupOpsStepBinding: "the restored volume is being bound to its claim",
}

// StorageBackupOpsValidator resolves a restore's references at creation and
// guards its record against being withdrawn mid-flight.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and while the operator is down nothing
// advances a restore anyway.
type StorageBackupOpsValidator struct {
	Client client.Client
}

func (v *StorageBackupOpsValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	switch req.Operation {
	case admissionv1.Create:
		return v.admitCreate(ctx, req)
	case admissionv1.Delete:
		return v.admitDelete(req)
	default:
		return admission.Allowed("")
	}
}

// admitCreate resolves every reference the operation names.
func (v *StorageBackupOpsValidator) admitCreate(
	ctx context.Context, req admission.Request,
) admission.Response {
	var ops simplyblockv1alpha2.StorageBackupOps
	if err := json.Unmarshal(req.Object.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if denied := clusterMustExist(ctx, v.Client, ops.Namespace, ops.Spec.ClusterRef); denied != nil {
		return *denied
	}

	var backup simplyblockv1alpha2.StorageBackup
	err := v.Client.Get(ctx,
		client.ObjectKey{Name: ops.Spec.BackupRef, Namespace: ops.Namespace}, &backup)
	if apierrors.IsNotFound(err) {
		return admission.Denied(fmt.Sprintf(
			"spec.backupRef %q does not name a StorageBackup in namespace %q",
			ops.Spec.BackupRef, ops.Namespace))
	}
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	// Refusing a Failed backup is a reference check rather than a state check. A
	// backup whose phase is Failed has no copy behind it, so a restore naming it
	// is naming a record of something that does not exist, and that is decided
	// once and stays decided: a backup does not recover from Failed, it is
	// replaced by another backup.
	if backup.Status.Phase == simplyblockv1alpha2.StorageBackupPhaseFailed {
		return admission.Denied(fmt.Sprintf(
			"StorageBackup %q failed and has no copy to restore", ops.Spec.BackupRef))
	}

	if ops.Spec.Restore == nil {
		// Required is a marker the type carries, so this only fires where an
		// action was admitted without the block it is parameterized by.
		return admission.Denied(fmt.Sprintf(
			"spec.restore is required for action %s", ops.Spec.Action))
	}

	if denied := v.poolMustExist(ctx, &ops); denied != nil {
		return *denied
	}
	return v.claimMustNotExist(ctx, &ops)
}

// poolMustExist resolves spec.restore.targetPool.
//
// The pool is named rather than defaulted because a backup discovered from a
// shared store may have been written by another cluster, so the pool it came
// from is not one this cluster is obliged to have.
func (v *StorageBackupOpsValidator) poolMustExist(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) *admission.Response {
	var pool simplyblockv1alpha1.StoragePool
	err := v.Client.Get(ctx,
		client.ObjectKey{Name: ops.Spec.Restore.TargetPool, Namespace: ops.Namespace}, &pool)
	if apierrors.IsNotFound(err) {
		denied := admission.Denied(fmt.Sprintf(
			"spec.restore.targetPool %q does not name a StoragePool in namespace %q",
			ops.Spec.Restore.TargetPool, ops.Namespace))
		return &denied
	}
	if err != nil {
		errored := admission.Errored(http.StatusInternalServerError, err)
		return &errored
	}
	return nil
}

// claimMustNotExist refuses a restore onto a claim name that is already taken.
//
// This is a refusal rather than an adoption, and it is the check standing
// between a restore and overwriting a running workload's data.
func (v *StorageBackupOpsValidator) claimMustNotExist(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) admission.Response {
	var claim corev1.PersistentVolumeClaim
	err := v.Client.Get(ctx,
		client.ObjectKey{Name: ops.Spec.Restore.ClaimName, Namespace: ops.Namespace}, &claim)
	if apierrors.IsNotFound(err) {
		return admission.Allowed("")
	}
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.Denied(fmt.Sprintf(
		"a PersistentVolumeClaim named %q already exists in namespace %q. A restore creates its claim "+
			"and never adopts one, because adopting it would replace that workload's data with the "+
			"backup's; name a claim that does not exist yet.",
		ops.Spec.Restore.ClaimName, ops.Namespace))
}

// admitDelete refuses to withdraw the record of an operation that has produced
// something the graph cannot take back.
//
// The object is read from req.OldObject, which is what the API server sends on a
// DELETE: there is no new object to inspect, and the step the operation is on is
// in the status of the one being removed.
func (v *StorageBackupOpsValidator) admitDelete(req admission.Request) admission.Response {
	if len(req.OldObject.Raw) == 0 {
		// Nothing to read means nothing to refuse on. Admitting is the only
		// answer that does not block a delete on the basis of no information.
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha2.StorageBackupOps
	if err := json.Unmarshal(req.OldObject.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// A terminal operation is a record of work that has finished, and withdrawing
	// it stops nothing.
	switch ops.Status.Phase {
	case simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageBackupOpsPhaseFailed,
		simplyblockv1alpha2.StorageBackupOpsPhaseAborted:
		return admission.Allowed("the operation is terminal")
	}

	step := simplyblockv1alpha2.StorageBackupOpsStep(ops.Status.Step.State)
	waiting, undeletable := undeletableSteps[step]
	if !undeletable {
		return admission.Allowed("")
	}

	return admission.Denied(fmt.Sprintf(
		"StorageBackupOps %s/%s is at step %s, where %s. Deleting the record would not stop that work, "+
			"it would remove the only account of it. Set spec.abort to stop an operation that can still "+
			"be stopped, and delete the record once it is terminal.",
		ops.Namespace, ops.Name, step, waiting))
}

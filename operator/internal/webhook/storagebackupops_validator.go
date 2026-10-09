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
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagebackupops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storagebackupops,verbs=create;delete,versions=v1alpha2,name=vstoragebackupops.simplyblock.io,admissionReviewVersions=v1

// undeletableSteps are the steps from which a running operation's record may not
// be withdrawn.
//
// This is the one guard in the group that refuses more than the graph does, and
// Restoring below is the step it adds. Everywhere else the two channels agree
// exactly and a test holds them equal; here the delete is strictly the more
// dangerous of the two, because an abort leaves an Aborted object behind for the
// controller to clean up from and a delete leaves nothing at all. Binding an
// agreement test to this set would therefore force the weaker answer onto the
// stronger channel.
//
// The webhook is also the stronger of the two guards in the other sense, because
// it catches the `--force --grace-period=0` that a finalizer alone does not.
var undeletableSteps = map[simplyblockv1alpha2.StorageBackupOpsStep]string{
	// Restoring is here although the design's §6 names only the two below, and
	// the reason is a window the design does not model: the step asks the control
	// plane for a volume before the identifier it answers with can be written
	// down. An operation sitting at Restoring may therefore already have a volume
	// whose id nothing recorded, and the object is the only thing that can find
	// it again — by the deterministic name it was asked for. Admitting the delete
	// would discard exactly that.
	//
	// The operation's own finalizer discards the volume before letting the object
	// go, so this is not the only guard. It is the stronger one, because it also
	// catches the --force --grace-period=0 that skips the finalizer entirely.
	simplyblockv1alpha2.StorageBackupOpsStepRestoring: "the control plane may already have " +
		"accepted the restore, and this object is the only record of the volume it produced",
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

	// OperatorNamespace is where every StorageBackup is recorded. Empty means the
	// operation's own namespace.
	OperatorNamespace string

	// Reviewer authorizes a reference that leaves the operation's namespace
	// (design-storagebackup.md §7). It is asked only then, so an operation that
	// stays in its own namespace needs none.
	Reviewer AccessReviewer
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

	// A record of work already done is not checked against the present. Its
	// claim exists because the restore it records produced it, and the backup it
	// names may have been pruned long ago, so every check below would reject the
	// objects the upgrade's absorption exists to preserve. The controller never
	// runs one, which is what makes admitting it safe.
	if simplyblockv1alpha2.IsHistoricalRecord(&ops) {
		return admission.Allowed("a record of work already done")
	}

	// Authorization comes before resolution. A refusal that said "no such backup"
	// to somebody who may not read backups would tell them which ones exist.
	if denied := v.authorizeReferences(ctx, req, &ops); denied != nil {
		return *denied
	}

	if denied := clusterMustExist(ctx, v.Client, clusterNamespace(&ops), ops.Spec.ClusterRef.Name); denied != nil {
		return *denied
	}

	backupNamespace := v.backupNamespace(&ops)
	var backup simplyblockv1alpha2.StorageBackup
	err := v.Client.Get(ctx,
		client.ObjectKey{Name: ops.Spec.BackupRef, Namespace: backupNamespace}, &backup)
	if apierrors.IsNotFound(err) {
		return admission.Denied(fmt.Sprintf(
			"spec.backupRef %q does not name a StorageBackup in namespace %q",
			ops.Spec.BackupRef, backupNamespace))
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
	// v1alpha2 is the stored version. Reading the pool at v1alpha1 would be
	// answered only by the conversion webhook, which a fresh install does not
	// deploy, and this guard would then deny every restore for naming a pool the
	// cluster has.
	var pool simplyblockv1alpha2.StoragePool
	err := v.Client.Get(ctx,
		client.ObjectKey{Name: ops.Spec.Restore.TargetPool, Namespace: clusterNamespace(ops)}, &pool)
	if apierrors.IsNotFound(err) {
		denied := admission.Denied(fmt.Sprintf(
			"spec.restore.targetPool %q does not name a StoragePool in namespace %q",
			ops.Spec.Restore.TargetPool, clusterNamespace(ops)))
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
	claimNamespace := claimNamespaceOf(ops)
	var claim corev1.PersistentVolumeClaim
	err := v.Client.Get(ctx,
		client.ObjectKey{Name: ops.Spec.Restore.Claim.Name, Namespace: claimNamespace}, &claim)
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
		ops.Spec.Restore.Claim.Name, claimNamespace))
}

// backupNamespace is where the operation's backup is. The mirror records every
// backup in the operator's namespace.
func (v *StorageBackupOpsValidator) backupNamespace(ops *simplyblockv1alpha2.StorageBackupOps) string {
	if v.OperatorNamespace != "" {
		return v.OperatorNamespace
	}
	return ops.Namespace
}

// clusterNamespace is the namespace of the operation's cluster and of the pool it
// restores into.
func clusterNamespace(ops *simplyblockv1alpha2.StorageBackupOps) string {
	return ops.Spec.ClusterRef.NamespaceOr(ops.Namespace)
}

// claimNamespaceOf is the namespace the restored claim is created in.
func claimNamespaceOf(ops *simplyblockv1alpha2.StorageBackupOps) string {
	if ops.Spec.Restore != nil && ops.Spec.Restore.Claim.Namespace != "" {
		return ops.Spec.Restore.Claim.Namespace
	}
	return ops.Namespace
}

// authorizeReferences asks whether the requester may use each object the
// operation names outside its own namespace, and returns the first refusal.
//
// The operator reads every namespace, so resolving a reference proves only that
// the object exists. Without this, anyone able to create a StorageBackupOps in
// one namespace could restore any backup into any other. Each question is one
// SubjectAccessReview and is asked only where a reference crosses, so an
// operation that stays in its own namespace needs no reviewer at all
// (design-storagebackup.md §7).
func (v *StorageBackupOpsValidator) authorizeReferences(
	ctx context.Context, req admission.Request, ops *simplyblockv1alpha2.StorageBackupOps,
) *admission.Response {
	type question struct {
		namespace string
		attrs     authorizationv1.ResourceAttributes
	}
	var questions []question
	if namespace := v.backupNamespace(ops); namespace != ops.Namespace {
		questions = append(questions, question{namespace, authorizationv1.ResourceAttributes{
			Verb: "get", Group: simplyblockv1alpha2.GroupVersion.Group, Resource: "storagebackups",
			Namespace: namespace, Name: ops.Spec.BackupRef,
		}})
	}
	if namespace := clusterNamespace(ops); namespace != ops.Namespace {
		questions = append(questions, question{namespace, authorizationv1.ResourceAttributes{
			Verb: "get", Group: simplyblockv1alpha2.GroupVersion.Group, Resource: "storageclusters",
			Namespace: namespace, Name: ops.Spec.ClusterRef.Name,
		}})
	}
	if namespace := claimNamespaceOf(ops); namespace != ops.Namespace {
		questions = append(questions, question{namespace, authorizationv1.ResourceAttributes{
			Verb: "create", Resource: "persistentvolumeclaims", Namespace: namespace,
		}})
	}

	for _, q := range questions {
		if v.Reviewer == nil {
			errored := admission.Errored(http.StatusInternalServerError,
				fmt.Errorf("no access reviewer is configured, so a reference to namespace %q cannot be authorized",
					q.namespace))
			return &errored
		}
		allowed, err := v.Reviewer.Allowed(ctx, req.UserInfo, q.attrs)
		if err != nil {
			errored := admission.Errored(http.StatusInternalServerError, err)
			return &errored
		}
		if !allowed {
			denied := admission.Denied(fmt.Sprintf(
				"%q may not %s %s in namespace %q, which this operation refers to. The operation's "+
					"references are checked against the requester's own permissions because the operator "+
					"can read every namespace.",
				req.UserInfo.Username, q.attrs.Verb, q.attrs.Resource, q.namespace))
			return &denied
		}
	}
	return nil
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

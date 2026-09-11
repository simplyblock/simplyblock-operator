// The StorageBackup write guard: a validating webhook that refuses a CREATE and
// a DELETE from every identity except the operator's own, and admits the
// namespace controller's teardown so that deleting a namespace cannot deadlock.
//
// It exists because a StorageBackup is discovered rather than declared
// (design-storagebackup.md §5.1). Creating one by hand would claim a backup
// exists that the store does not hold, and every restore naming it would then
// fail against a copy that was never there. Deleting one would hide a backup
// that is still in the bucket and still restorable, and would not free a byte:
// the copy is governed by that bucket's lifecycle policy and by the retention
// the control plane applies, and a record that could be deleted invites the
// reading that deleting it reclaims the storage.
//
// It is the same guard the StorageDevice validator next door applies, for the
// same reason and with the same failure policy.

package webhook

import (
	"context"
	"fmt"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagebackup,mutating=false,failurePolicy=ignore,sideEffects=None,groups=storage.simplyblock.io,resources=storagebackups,verbs=create;delete,versions=v1alpha2,name=vstoragebackup.simplyblock.io,admissionReviewVersions=v1

// StorageBackupValidator refuses a StorageBackup write that is not the
// operator's own.
//
// failurePolicy=Ignore, which is the opposite of what the two reference
// validators in this band chose, and the difference is that this one guards
// DELETE. A webhook that fails closed on DELETE blocks the namespace controller
// as well as a user, so an operator that is down leaves every namespace holding
// a backup record stuck in Terminating with no way out but removing the webhook
// by hand. Failing open costs the opposite: somebody may write a record while
// the operator is down, and the mirror reconciles it away against what the store
// actually holds. One of the two is recoverable without an administrator.
type StorageBackupValidator struct {
	// Client reads the Namespace the object is in. The exemption below is a
	// property of the namespace rather than of the caller, so it cannot be
	// decided from the admission request alone.
	Client client.Client

	// OperatorNamespace is the namespace the operator runs in. Any service
	// account in it is the operator, and every other identity is refused.
	OperatorNamespace string
}

func (v *StorageBackupValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	switch req.Operation {
	case admissionv1.Create, admissionv1.Delete:
	default:
		return admission.Allowed("")
	}

	// The operator writes a backup record for two reasons and no others: the
	// store started reporting a copy, and it stopped.
	if strings.HasPrefix(req.UserInfo.Username, "system:serviceaccount:"+v.OperatorNamespace+":") {
		return admission.Allowed("operator-driven write")
	}

	if req.Operation == admissionv1.Create {
		return admission.Denied(fmt.Sprintf(
			"StorageBackup %s/%s cannot be created by hand: a backup object records a copy the operator "+
				"found in the cluster's store, and one written here would name a copy that does not "+
				"exist. Schedule backups with a StorageBackupPolicy instead.", req.Namespace, req.Name))
	}

	// Deleting a namespace makes Kubernetes delete the objects in it, and those
	// deletes come from the namespace controller rather than from the operator.
	// Refusing them leaves the namespace in Terminating forever, so the guard is
	// conditional on the namespace not itself being on its way out. A user's
	// `kubectl delete sb` and the teardown look identical to a rule that only
	// reads the request, and only one of them is wanted.
	terminating, err := v.namespaceIsTerminating(ctx, req.Namespace)
	if err != nil {
		// A namespace that cannot be read is not evidence that it is
		// terminating. Refusing is the closed side: the alternative admits every
		// delete for as long as the read keeps failing.
		return admission.Denied(fmt.Sprintf(
			"a StorageBackup is deleted by the operator rather than by hand, and whether "+
				"namespace %s is terminating could not be established: %v", req.Namespace, err))
	}
	if terminating {
		return admission.Allowed("namespace teardown")
	}

	return admission.Denied(fmt.Sprintf(
		"StorageBackup %s/%s is a record of a copy in the cluster's store and is removed by the "+
			"operator when the store stops reporting it. Deleting the record would not delete the "+
			"copy, which is governed by the bucket's lifecycle policy and by the control plane's "+
			"retention.", req.Namespace, req.Name))
}

// namespaceIsTerminating reports whether the object's namespace is being
// deleted.
func (v *StorageBackupValidator) namespaceIsTerminating(ctx context.Context, name string) (bool, error) {
	var namespace corev1.Namespace
	if err := v.Client.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
		return false, err
	}
	if namespace.DeletionTimestamp != nil {
		return true, nil
	}
	return namespace.Status.Phase == corev1.NamespaceTerminating, nil
}

// The StorageDevice delete guard: a validating webhook that refuses DELETE from
// every identity except the operator's own, and admits the namespace
// controller's teardown so that deleting a namespace cannot deadlock.
//
// It exists because a StorageDevice is discovered rather than declared. Nothing
// asked for the object, so withdrawing it is not a request anybody can make,
// and the object carries state that only its own continuity holds:
// status.activeOpsRef is a lock some operation took, the UID is what anything
// referring to the device recorded, and the phase is an observation with a
// history. A replacement object of the same name has none of that.
//
// design-storagedevice.md §5.3 is the specification.

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

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagedevice,mutating=false,failurePolicy=ignore,sideEffects=None,groups=storage.simplyblock.io,resources=storagedevices,verbs=delete,versions=v1alpha2,name=vstoragedevice.simplyblock.io,admissionReviewVersions=v1

// cascadeUsernames are the identities Kubernetes cascades an owner reference
// under, either of which may be the one a given cluster uses.
//
// A device object carries a controller reference to the StorageNode it was found
// on, so deleting the node is meant to take its devices with it. Kubernetes
// performs that cascade through the garbage collector, and the collector's
// deletes arrive as the control plane rather than as the operator: the reference
// was written by the operator, but the delete is Kubernetes' own request. A guard
// that reads only the caller therefore refuses the one path its own contract
// relies on.
//
// Which of the two arrives is a flag on the controller manager rather than
// anything this operator can require. The --use-service-account-credentials gives
// each controller its own service account, which is what a kubeadm cluster
// defaults to, and the garbage collector's is the first below. Without it every
// controller shares the controller manager's own identity, which is the second.
// A cluster answering with one of them is not a cluster misconfigured for the
// other, so both are admitted.
var cascadeUsernames = map[string]bool{
	"system:serviceaccount:kube-system:generic-garbage-collector": true,
	"system:kube-controller-manager":                              true,
}

// StorageDeviceValidator refuses a StorageDevice deletion that is not the
// operator's own.
//
// failurePolicy=Ignore, which is the opposite of what the StorageNode validator
// next door chose, and the difference is that this one guards DELETE. A webhook
// that fails closed on DELETE blocks the namespace controller as well as a user,
// so an operator that is down leaves every namespace holding a device stuck in
// Terminating with no way out but removing the webhook by hand. Failing open
// costs the opposite: somebody may delete a record while the operator is down,
// and the mirror rebuilds it from the control plane on the next sync. One of the
// two is recoverable without an administrator.
type StorageDeviceValidator struct {
	// Client reads the Namespace the object is in. The exemption below is a
	// property of the namespace rather than of the caller, so it cannot be
	// decided from the admission request alone.
	Client client.Client

	// OperatorNamespace is the namespace the operator runs in. Any service
	// account in it is the operator, and every other identity is refused.
	OperatorNamespace string
}

func (v *StorageDeviceValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Delete {
		return admission.Allowed("")
	}

	// A device object is deleted for two reasons and no others: the device
	// stopped being reported, which the operator does itself, and the owning node
	// was deleted, which Kubernetes cascades.
	if strings.HasPrefix(req.UserInfo.Username, "system:serviceaccount:"+v.OperatorNamespace+":") {
		return admission.Allowed("operator-driven deletion")
	}

	// The second of those two reasons is carried out by Kubernetes rather than by
	// the operator, so it needs its own exemption. The identities are matched
	// exactly rather than by prefix: what cascades is the control plane itself,
	// and every other account in kube-system is as much a stranger to a device
	// record as a user is.
	if cascadeUsernames[req.UserInfo.Username] {
		return admission.Allowed("owner cascade")
	}

	// Deleting a namespace makes Kubernetes delete the objects in it, and those
	// deletes come from the namespace controller rather than from the operator.
	// Refusing them leaves the namespace in Terminating forever, so the guard is
	// conditional on the namespace not itself being on its way out. A user's
	// `kubectl delete sd` and the teardown look identical to a rule that only
	// reads the request, and only one of them is wanted.
	terminating, err := v.namespaceTerminating(ctx, req.Namespace)
	if err != nil {
		// A namespace that cannot be read is not evidence that it is
		// terminating. Refusing is the closed side: the alternative admits every
		// delete for as long as the read keeps failing.
		return admission.Denied(fmt.Sprintf(
			"a StorageDevice is deleted by the operator rather than by hand, and whether "+
				"namespace %s is terminating could not be established: %v", req.Namespace, err))
	}
	if terminating {
		return admission.Allowed("namespace teardown")
	}

	return admission.Denied(fmt.Sprintf(
		"StorageDevice %s/%s is discovered rather than declared and is deleted by the operator "+
			"when the control plane stops reporting the device, or with the StorageNode that owns "+
			"it. Take the device out of service with a StorageDeviceOps instead of deleting its "+
			"record.", req.Namespace, req.Name))
}

// namespaceTerminating reports whether the object's namespace is being deleted.
func (v *StorageDeviceValidator) namespaceTerminating(ctx context.Context, name string) (bool, error) {
	var namespace corev1.Namespace
	if err := v.Client.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
		return false, err
	}
	if namespace.DeletionTimestamp != nil {
		return true, nil
	}
	return namespace.Status.Phase == corev1.NamespaceTerminating, nil
}

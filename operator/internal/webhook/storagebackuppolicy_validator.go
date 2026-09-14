// The StorageBackupPolicy reference check: a validating webhook that refuses a
// policy naming a cluster that does not exist.
//
// The field it checks is immutable, which is what makes admission the right
// place for it: a reference that is wrong at creation is wrong for the object's
// whole life, because the field can never be corrected. The only remedy is to
// delete the object and write it again, which is precisely what a rejected
// create would have asked for, except that the rejection says so immediately and
// at no cost, while the alternative is a policy parked in Failed that somebody
// has to read, diagnose, and clean up (design-storagebackup.md §7).
//
// spec.claimSelector is deliberately not checked. It is a selector rather than a
// reference, so there is no object for it to resolve to, and §4.1's reading that
// an absent selector selects nothing is reported with a SelectorEmpty event
// rather than refused: a policy covering nothing is a policy somebody may be
// about to label claims for.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagebackuppolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storagebackuppolicies,verbs=create,versions=v1alpha2,name=vstoragebackuppolicy.simplyblock.io,admissionReviewVersions=v1

// StorageBackupPolicyValidator refuses a policy whose spec.clusterRef names no
// StorageCluster in the same namespace.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and while the operator is down nothing
// reconciles a policy anyway. A window in which unresolvable immutable
// references are admitted is a window in which objects that can only be deleted
// are created.
type StorageBackupPolicyValidator struct {
	Client client.Client
}

func (v *StorageBackupPolicyValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	var policy simplyblockv1alpha2.StorageBackupPolicy
	if err := json.Unmarshal(req.Object.Raw, &policy); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if denied := clusterMustExist(ctx, v.Client, policy.Namespace, policy.Spec.ClusterRef); denied != nil {
		return *denied
	}
	return admission.Allowed("")
}

// clusterMustExist resolves a spec.clusterRef, and is shared by the two
// validators in this band that carry the field. It returns nil when the
// reference resolves, and the response to give back when it does not.
//
// The lookup is namespace-local, which is what makes it affordable: every
// reference in this band is a bare name meaning the same namespace, so each
// check is one Get rather than a List.
func clusterMustExist(
	ctx context.Context, c client.Client, namespace, clusterRef string,
) *admission.Response {
	var cluster simplyblockv1alpha1.StorageCluster
	err := c.Get(ctx, client.ObjectKey{Name: clusterRef, Namespace: namespace}, &cluster)
	if apierrors.IsNotFound(err) {
		denied := admission.Denied(fmt.Sprintf(
			"spec.clusterRef %q does not name a StorageCluster in namespace %q", clusterRef, namespace))
		return &denied
	}
	if err != nil {
		errored := admission.Errored(http.StatusInternalServerError, err)
		return &errored
	}
	return nil
}

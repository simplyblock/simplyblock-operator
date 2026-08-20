package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha1-replicationops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=replicationops,verbs=create,versions=v1alpha1,name=vreplicationops.simplyblock.io,admissionReviewVersions=v1

// ReplicationOpsValidator rejects a ReplicationOps whose spec.ref does not
// resolve to an existing resource for the given spec.scope:
//   - scope=policy or scope=target: ref must name an existing ReplicationPolicy.
//   - scope=volume:                 ref must name an existing ReplicationSlot.
//
// Catching this at admission time gives the user an immediate, actionable error
// instead of a ReplicationOps stuck in phase=Failed with an opaque message.
//
// failurePolicy=Fail: a ReplicationOps with a bad ref would always fail anyway;
// rejecting it at write time is strictly safer.
type ReplicationOpsValidator struct {
	Client client.Client
}

func (v *ReplicationOpsValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha1.ReplicationOps
	if err := json.Unmarshal(req.Object.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	ns := ops.Namespace
	ref := ops.Spec.Ref

	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopePolicy, utils.ReplicationOpsScopeTarget:
		var policy simplyblockv1alpha1.ReplicationPolicy
		err := v.Client.Get(ctx, types.NamespacedName{Name: ref, Namespace: ns}, &policy)
		if apierrors.IsNotFound(err) {
			return admission.Denied(fmt.Sprintf(
				"spec.ref %q does not name a ReplicationPolicy in namespace %q (scope=%s)",
				ref, ns, ops.Spec.Scope))
		}
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

	case utils.ReplicationOpsScopeVolume:
		var slot simplyblockv1alpha1.ReplicationSlot
		err := v.Client.Get(ctx, types.NamespacedName{Name: ref, Namespace: ns}, &slot)
		if apierrors.IsNotFound(err) {
			return admission.Denied(fmt.Sprintf(
				"spec.ref %q does not name a ReplicationSlot in namespace %q (scope=volume)",
				ref, ns))
		}
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	return admission.Allowed("")
}

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha1-storagenode,mutating=false,failurePolicy=ignore,sideEffects=None,groups=storage.simplyblock.io,resources=storagenodes,verbs=update,versions=v1alpha1,name=vstoragenode.simplyblock.io,admissionReviewVersions=v1

// StorageNodeValidator is a validating admission webhook that enforces
// spec.workerNode is only ever re-pointed by the operator itself. A StorageNode
// is bound to a worker host; users must not move it by editing the CR directly —
// relocation is driven exclusively through a StorageNodeOps(action=migrate),
// which the operator executes (drain-free restart onto the target host) before
// re-pointing spec.workerNode under its own service account.
//
// failurePolicy=Ignore mirrors the rebalancer injector: webhook unavailability
// must not block reconciliation.
type StorageNodeValidator struct {
	// OperatorNamespace is the namespace the operator runs in. Any service
	// account in this namespace (i.e. the operator itself) is permitted to change
	// spec.workerNode; every other identity is rejected.
	OperatorNamespace string
}

func (v *StorageNodeValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}

	var oldSN, newSN simplyblockv1alpha1.StorageNode
	if err := json.Unmarshal(req.OldObject.Raw, &oldSN); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if err := json.Unmarshal(req.Object.Raw, &newSN); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if oldSN.Spec.WorkerNode == newSN.Spec.WorkerNode {
		return admission.Allowed("")
	}

	// Only the operator (a service account in the operator namespace) may
	// re-point a StorageNode to a different worker.
	operatorSAPrefix := "system:serviceaccount:" + v.OperatorNamespace + ":"
	if strings.HasPrefix(req.UserInfo.Username, operatorSAPrefix) {
		return admission.Allowed("operator-driven workerNode change")
	}

	return admission.Denied(fmt.Sprintf(
		"spec.workerNode is immutable to users (attempted %q -> %q); "+
			"relocate a storage node with a StorageNodeOps(action=migrate) instead",
		oldSN.Spec.WorkerNode, newSN.Spec.WorkerNode))
}

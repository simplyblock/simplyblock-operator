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
//   - scope=cluster:                ref must name a StorageCluster, the action must
//     be migration, and spec.policyRef must name a ReplicationPolicy in mode migration.
//
// spec.policyRef is refused outside scope=cluster, and spec.dryRun outside
// action=migration.
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

	if msg := migrationSpecError(&ops); msg != "" {
		return admission.Denied(msg)
	}

	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopeTarget:
		var pair simplyblockv1alpha1.ReplicationPair
		err := v.Client.Get(ctx, types.NamespacedName{Name: ref, Namespace: ns}, &pair)
		if apierrors.IsNotFound(err) {
			return admission.Denied(fmt.Sprintf(
				"spec.ref %q does not name a ReplicationPair in namespace %q (scope=target)",
				ref, ns))
		}
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

	case utils.ReplicationOpsScopePolicy:
		var policy simplyblockv1alpha1.ReplicationPolicy
		err := v.Client.Get(ctx, types.NamespacedName{Name: ref, Namespace: ns}, &policy)
		if apierrors.IsNotFound(err) {
			return admission.Denied(fmt.Sprintf(
				"spec.ref %q does not name a ReplicationPolicy in namespace %q (scope=policy)",
				ref, ns))
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

	case utils.ReplicationOpsScopeCluster:
		if _, err := utils.ResolveClusterUUID(ctx, v.Client, ns, ref); err != nil {
			return admission.Denied(fmt.Sprintf(
				"spec.ref %q does not name a StorageCluster in namespace %q (scope=cluster)",
				ref, ns))
		}
		var policy simplyblockv1alpha1.ReplicationPolicy
		err := v.Client.Get(ctx, types.NamespacedName{Name: ops.Spec.PolicyRef, Namespace: ns}, &policy)
		if apierrors.IsNotFound(err) {
			return admission.Denied(fmt.Sprintf(
				"spec.policyRef %q does not name a ReplicationPolicy in namespace %q",
				ops.Spec.PolicyRef, ns))
		}
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		if policy.Spec.Mode != utils.ReplicationPolicyModeMigration {
			return admission.Denied(fmt.Sprintf(
				"spec.policyRef %q is in mode %q; a cluster migration needs a policy in mode %q",
				ops.Spec.PolicyRef, policy.Spec.Mode, utils.ReplicationPolicyModeMigration))
		}
	}

	return admission.Allowed("")
}

// migrationSpecError checks the fields that only a migration accepts.
func migrationSpecError(ops *simplyblockv1alpha1.ReplicationOps) string {
	migration := ops.Spec.Action == "migration"
	switch {
	case ops.Spec.Scope == utils.ReplicationOpsScopeCluster && !migration:
		return "scope=cluster is only valid with action=migration"
	case ops.Spec.Scope == utils.ReplicationOpsScopeCluster && ops.Spec.PolicyRef == "":
		return "scope=cluster requires spec.policyRef, the migration policy to enroll volumes under"
	case ops.Spec.Scope != utils.ReplicationOpsScopeCluster && ops.Spec.PolicyRef != "":
		return "spec.policyRef is only valid with scope=cluster"
	case ops.Spec.DryRun && !migration:
		return "spec.dryRun is only valid with action=migration"
	}
	return ""
}

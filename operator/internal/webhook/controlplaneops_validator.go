// The ControlPlaneOps guard: a validating webhook that resolves
// spec.controlPlaneRef at creation and refuses an operation naming a control
// plane none of its actions can act on.
//
// Every action acts on what the operator installed — Restart recycles a
// workload, Upgrade replaces its image, and Backup asks the FoundationDBCluster
// the operator applied — so an operation naming an external control plane is one
// that can only fail. Admission is where the check belongs, because an operation
// that can only fail belongs in an error message on the terminal that wrote it
// rather than in a Failed object somebody has to go and read.
//
// Admission is also where the check *can* live, because the answer cannot move:
// spec.controlPlaneRef is immutable and ControlPlane.spec.source is immutable, so
// a control plane admitted as managed stays managed for the life of the
// operation. What can still happen is the target being deleted, and an operation
// whose target has vanished is a missing-target failure the controller handles.
//
// design-controlplane.md §6 is the specification.

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

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-controlplaneops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=controlplaneops,verbs=create,versions=v1alpha2,name=vcontrolplaneops.simplyblock.io,admissionReviewVersions=v1

// ControlPlaneOpsValidator refuses an operation whose target cannot be operated
// on, and one whose action is missing the block that parameterizes it.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and while the operator is down nothing
// advances an operation anyway.
type ControlPlaneOpsValidator struct {
	Client client.Client
}

func (v *ControlPlaneOpsValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha2.ControlPlaneOps
	if err := json.Unmarshal(req.Object.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// v1alpha2 is the stored version. Reading the target at v1alpha1 would be
	// answered only by the conversion webhook, which a fresh install does not
	// deploy, and this guard would then deny every operation for naming a
	// control plane the cluster has.
	var target simplyblockv1alpha2.ControlPlane
	key := client.ObjectKey{Name: ops.Spec.ControlPlaneRef, Namespace: ops.Namespace}
	err := v.Client.Get(ctx, key, &target)
	switch {
	case apierrors.IsNotFound(err):
		return admission.Denied(fmt.Sprintf(
			"spec.controlPlaneRef %q does not name a ControlPlane in namespace %q",
			ops.Spec.ControlPlaneRef, ops.Namespace))
	case err != nil:
		return admission.Errored(http.StatusInternalServerError, err)
	}

	if target.Spec.Source.Managed == nil {
		return admission.Denied(fmt.Sprintf(
			"ControlPlane %q is external, and every action of this kind acts on something the "+
				"operator installed: Restart recycles a workload, Upgrade replaces its image, and "+
				"Backup asks the FoundationDBCluster the operator applied. An external control "+
				"plane is an endpoint and a credential, so there is nothing here for any of them "+
				"to act on.",
			ops.Spec.ControlPlaneRef))
	}

	return actionBlockPresent(&ops)
}

// actionBlockPresent refuses an action whose parameter block is absent.
//
// The blocks are optional on the type because each action ignores the others',
// so the API cannot require one without requiring all three. Checking it here is
// what turns "the operation failed at its first step" into an error on the
// terminal that wrote the object.
func actionBlockPresent(ops *simplyblockv1alpha2.ControlPlaneOps) admission.Response {
	switch ops.Spec.Action {
	case simplyblockv1alpha2.ControlPlaneOpsActionUpgrade:
		if ops.Spec.Upgrade == nil || ops.Spec.Upgrade.Image == "" {
			return admission.Denied(
				"action Upgrade needs spec.upgrade.image, which names the version to move to")
		}
	case simplyblockv1alpha2.ControlPlaneOpsActionBackup:
		if ops.Spec.Backup == nil || ops.Spec.Backup.BlobStore == "" {
			return admission.Denied(
				"action Backup needs spec.backup.blobStore, which names where the backup goes")
		}
	case simplyblockv1alpha2.ControlPlaneOpsActionRestart:
		// Restart takes no required block: an empty spec.restart.components
		// recycles the whole control plane, which is the common case and a
		// legitimate one to write nothing for.
	}
	return admission.Allowed("")
}

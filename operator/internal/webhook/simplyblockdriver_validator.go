// The admission half of the SimplyblockDriver singleton: a Kubernetes cluster
// holds one CSI driver deployment, and this is what stops the second from being
// written.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §3.4, which also carries the controller half for the object written while this
// webhook was not serving.

package webhook

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-simplyblockdriver,mutating=false,failurePolicy=ignore,sideEffects=None,groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=create,versions=v1alpha2,name=vsimplyblockdriver.simplyblock.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=get;list;watch

// SimplyblockDriverValidator denies a second SimplyblockDriver anywhere in the
// Kubernetes cluster.
//
// Twelve of the objects a driver owns are cluster-scoped: five ClusterRole and
// ClusterRoleBinding pairs, the CSIDriver registration, and the
// VolumeSnapshotClass. Two SimplyblockDrivers do not get a copy of those each,
// they get one object written twice, and the bindings are where that surfaces,
// since each names a ServiceAccount together with its namespace and two
// controllers alternate the subject. Below the names the node plugin registers
// at /var/lib/kubelet/plugins/<driverName>/csi.sock and the kubelet registers one
// plugin per driver name, so two node plugins on one worker contend for a path
// no object name reaches.
//
// failurePolicy=Ignore, unlike most validators here, and the install is what
// decides it. The chart renders a SimplyblockDriver beside the Deployment that
// serves this webhook, so the one CREATE a first install performs arrives
// seconds after the webhook configuration is registered and about a minute
// before the operator answers on the service. Under Fail the release stops
// there, on a cluster left holding an operator, a control plane, and no CSI
// driver, and the only way out is to run the install a second time.
//
// Nothing is given up by ignoring it, because this webhook was never the
// enforcement of record: a driver admitted while it was unavailable is refused
// by the controller, which holds the object at Installing, applies nothing, and
// emits DuplicateDriver (§3.4). What Fail bought was a clearer message at the
// moment of typing, and it cost every first install.
//
// The rule is CREATE and not UPDATE, because an edit to the object that already
// exists is not a second one, and a rule over every operation would lock the
// running deployment's own spec.
type SimplyblockDriverValidator struct {
	Client client.Client
}

func (v *SimplyblockDriverValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	var existing simplyblockv1alpha2.SimplyblockDriverList
	if err := v.Client.List(ctx, &existing); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if len(existing.Items) == 0 {
		return admission.Allowed("")
	}

	holder := driver.DeploymentHolder(existing.Items)
	return admission.Denied(fmt.Sprintf(
		"a Kubernetes cluster holds one SimplyblockDriver, and %s/%s already holds it; "+
			"edit that object rather than creating a second, which would contend with it "+
			"over the cluster-scoped RBAC, the CSIDriver registration, and the node plugin's "+
			"kubelet socket",
		holder.Namespace, holder.Name))
}

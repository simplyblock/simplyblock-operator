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
	"sort"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-simplyblockdriver,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=simplyblockdrivers,verbs=create,versions=v1alpha2,name=vsimplyblockdriver.simplyblock.io,admissionReviewVersions=v1

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
// failurePolicy=Fail, like every other validator here. What it blocks while
// unavailable is the creation of a driver, which is a deployment-time action
// rather than a data-path one, and the object written in that window is caught
// by the controller instead.
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

	holder := oldestDriver(existing.Items)
	return admission.Denied(fmt.Sprintf(
		"a Kubernetes cluster holds one SimplyblockDriver, and %s/%s already holds it; "+
			"edit that object rather than creating a second, which would contend with it "+
			"over the cluster-scoped RBAC, the CSIDriver registration, and the node plugin's "+
			"kubelet socket",
		holder.Namespace, holder.Name))
}

// oldestDriver picks the object that holds the deployment. It is the oldest, and
// the namespace and name decide a tie, so that this webhook and the controller
// reach the same answer from the same list without a lock between them.
func oldestDriver(items []simplyblockv1alpha2.SimplyblockDriver) simplyblockv1alpha2.SimplyblockDriver {
	sorted := make([]simplyblockv1alpha2.SimplyblockDriver, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return sorted[0]
}

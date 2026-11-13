// The StorageDeviceOps guard: a validating webhook on CREATE that refuses an
// operation on a device of a stretched cluster's remote node. Such a device is
// listed on this site for visibility; its node is the other site's, and that
// site's operator runs its operations (design-stretched-sync-replication-ramen.md
// §6.2).

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagedeviceops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storagedeviceops,verbs=create,versions=v1alpha2,name=vstoragedeviceops.simplyblock.io,admissionReviewVersions=v1

// StorageDeviceOpsValidator refuses an operation on a device whose node is a
// stretched cluster's remote node.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// while it is down, nothing advances an operation anyway.
type StorageDeviceOpsValidator struct {
	// Client reads the target device and its node.
	Client client.Reader
}

func (v *StorageDeviceOpsValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var ops simplyblockv1alpha2.StorageDeviceOps
	if err := json.Unmarshal(req.Object.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if v.Client == nil {
		return admission.Allowed("")
	}
	var device simplyblockv1alpha2.StorageDevice
	key := client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.DeviceRef}
	if err := v.Client.Get(ctx, key, &device); err != nil {
		// The operation's controller refuses a device that does not exist.
		return admission.Allowed("")
	}
	return refuseRemoteNode(ctx, v.Client, ops.Namespace, device.Spec.NodeRef,
		fmt.Sprintf("StorageDeviceOps on device %s of", device.Name))
}

// refuseRemoteNode denies an operation whose target is a stretched cluster's
// remote node, naming the site whose operator runs it, and admits anything
// else, including a node that cannot be read.
func refuseRemoteNode(
	ctx context.Context, reader client.Reader, namespace, nodeName, what string,
) admission.Response {
	if reader == nil {
		return admission.Allowed("")
	}
	var node simplyblockv1alpha2.StorageNode
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodeName}, &node); err != nil ||
		!node.IsRemote() {
		return admission.Allowed("")
	}
	return admission.Denied(fmt.Sprintf(
		"RemoteNode: %s StorageNode %s is refused: the node belongs to site %s of a stretched cluster and "+
			"is listed on this site from the control plane; run the operation on site %s, whose operator "+
			"manages it", what, node.Name, node.Spec.Config.Site, node.Spec.Config.Site))
}

// The StorageCluster guard: a validating webhook that refuses a second cluster
// in a namespace.
//
// The limit is not a policy choice, it is what the workload can express. A
// cluster renders a storage-node DaemonSet, a headless Service, an EndpointSlice,
// and a serving certificate, and three of those carry names that are constants
// rather than anything derived from the cluster --
// atlas-lib/kube.StorageNodeSetAPIServiceName and the TLS secret beside it. The
// first cluster of a namespace takes ownership of them and the second cannot.
//
// What that looks like without this guard is the reason it is admission and not
// a note in a document. The workload pass applies seven things in order and
// stops at the first that fails; the certificate is the second and the DaemonSet
// is the seventh. So the second cluster gets its StorageCluster, its
// StorageNodes, and no workload at all, and every one of its nodes then waits on
// a hostname that resolves to nothing, reporting HostUnreachable until whatever
// is waiting on them gives up. The cause is one debug line in the operator's log
// naming an ownership conflict on an object nobody was looking at.
//
// Refusing the create says the same thing at the only moment it is cheap.

package webhook

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagecluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storageclusters,verbs=create;update,versions=v1alpha2,name=vstoragecluster.simplyblock.io,admissionReviewVersions=v1

// StorageClusterValidator refuses a cluster that would share a namespace with
// one already there.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and a cluster created while the
// operator is down has nothing to reconcile it anyway.
type StorageClusterValidator struct {
	Client  client.Client
	Decoder admission.Decoder
}

// Handle admits a cluster when it is the only one its namespace holds.
func (v *StorageClusterValidator) Handle(
	ctx context.Context, req admission.Request,
) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}

	var cluster simplyblockv1alpha2.StorageCluster
	if err := v.Decoder.Decode(req, &cluster); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var existing simplyblockv1alpha2.StorageClusterList
	if err := v.Client.List(ctx, &existing,
		client.InNamespace(cluster.Namespace)); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	for i := range existing.Items {
		// The object under review is in this list on an update, and on a create
		// whose admission is being replayed against a cache that has already
		// seen it. Neither is a second cluster.
		if other := existing.Items[i].Name; other != cluster.Name {
			return admission.Denied(fmt.Sprintf(
				"namespace %s already holds StorageCluster %s, and a namespace holds one: "+
					"the storage-node Service, its TLS secret, and its serving certificate "+
					"are named for the namespace rather than for the cluster, so a second "+
					"cluster here would come up with storage nodes and no workload behind "+
					"them. Put %s in a namespace of its own",
				cluster.Namespace, other, cluster.Name))
		}
	}
	return admission.Allowed("")
}

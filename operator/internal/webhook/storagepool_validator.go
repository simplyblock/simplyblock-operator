// The StoragePool admission guard: a validating webhook that refuses a pool
// naming a StorageCluster that does not exist.
//
// It answers exactly one question — whether the named object is there — and
// leaves everything about the cluster's readiness to the reconcile. The line
// between the two is which of them can ever become true. spec.clusterRef is
// immutable, so a pool naming a cluster that does not exist can never be
// corrected: the only remedy is to delete it and write it again, which is what
// the rejection asks for directly. A cluster that exists and has no UUID yet is
// a not-yet rather than a mistake — the cluster is being created, the pool is
// being applied alongside it, and a manifest declaring both at once is the
// ordinary way to bring a deployment up — so that one is admitted and the
// controller waits.
//
// failurePolicy=Fail. The webhook server runs in the operator pod, so while it
// is unavailable nothing reconciles a pool anyway, and admitting a pool whose
// immutable reference cannot be resolved creates an object that can only be
// deleted.
//
// design-storagepool.md §3.4 is the specification.

package webhook

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagepool,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storagepools,verbs=create,versions=v1alpha2,name=vstoragepool.simplyblock.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

// StoragePoolValidator refuses a StoragePool whose spec.clusterRef names no
// StorageCluster in the pool's own namespace.
//
// The rule is CREATE only. spec.clusterRef is immutable, so an update cannot
// introduce a dangling reference, and a rule over every operation would refuse
// an edit to a pool whose cluster has since been deleted — which is exactly the
// object somebody is trying to clean up.
type StoragePoolValidator struct {
	// Client reads the StorageCluster the pool names. The answer is a property
	// of the cluster rather than of the request, so it cannot be decided from
	// the admission review alone.
	Client client.Client

	// Decoder turns the request's raw object into a pool.
	Decoder admission.Decoder
}

func (v *StoragePoolValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	pool := &simplyblockv1alpha2.StoragePool{}
	if err := v.Decoder.Decode(req, pool); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	namespace := pool.Namespace
	if namespace == "" {
		// A namespaced object created through a namespaced endpoint may arrive
		// with the field unset, because the path carries it instead.
		namespace = req.Namespace
	}

	var cluster simplyblockv1alpha1.StorageCluster
	err := v.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pool.Spec.ClusterRef}, &cluster)
	switch {
	case apierrors.IsNotFound(err):
		return admission.Denied(fmt.Sprintf(
			"spec.clusterRef names StorageCluster %q, and there is none by that name in namespace %s. "+
				"A pool lives in its cluster's namespace and the reference is immutable, so this "+
				"object could never be corrected; create the cluster first, or fix the name here",
			pool.Spec.ClusterRef, namespace))
	case err != nil:
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// The cluster exists. Whether it is finished is the controller's question:
	// a pool applied in the same manifest as its cluster holds at Pending and
	// reports ClusterNotReady until the cluster has a UUID to create it in.
	return admission.Allowed("")
}

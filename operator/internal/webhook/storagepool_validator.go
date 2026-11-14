// The StoragePool admission guard: a validating webhook that refuses a pool
// naming a StorageCluster that does not exist, and a striped pool its cluster
// cannot serve.
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
// design-storagepool.md §3.4 is the specification. The striping checks are
// csi-driver/docs/striped-volumes.md's: the CRD's CEL rules refuse what a pool
// says about itself, and this refuses what depends on the cluster it names.

package webhook

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// matchPolicy=Equivalent is load-bearing rather than a default worth inheriting.
// The CRD serves v1alpha1 as well, and under the API server's default Exact
// policy a rule naming only v1alpha2 does not see a v1alpha1 create at all — so
// a client writing the older version would place a pool naming a cluster that
// does not exist, which spec.clusterRef being immutable makes permanent.
// Equivalent has the API server convert the request to v1alpha2 and send it
// here, so the guard holds whichever version was written.

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagepool,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,sideEffects=None,groups=storage.simplyblock.io,resources=storagepools,verbs=create,versions=v1alpha2,name=vstoragepool.simplyblock.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch

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

	var cluster simplyblockv1alpha2.StorageCluster
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

	if width := stripeWidth(pool); width >= 2 {
		reason, err := v.stripeRefusal(ctx, namespace, &cluster, width)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		if reason != "" {
			return admission.Denied(reason)
		}
	}

	// The cluster exists. Whether it is finished is the controller's question:
	// a pool applied in the same manifest as its cluster holds at Pending and
	// reports ClusterNotReady until the cluster has a UUID to create it in.
	return admission.Allowed("")
}

// stripeWidth is the stripe width a pool's volumes are created with, and 0 for
// a pool whose volumes are not striped.
func stripeWidth(pool *simplyblockv1alpha2.StoragePool) int32 {
	if d := pool.Spec.VolumeDefaults; d != nil && d.StripeWidth != nil {
		return *d.StripeWidth
	}
	return 0
}

// stripeRefusal is why a pool whose volumes are striped width wide cannot be
// served by the cluster it names, or "" when it can.
//
// A stretched cluster is refused outright, because the CSI driver refuses every
// striped volume on one: the site gate that leads a volume from one site knows
// one logical volume where a striped volume has several.
//
// The width has to fit the storage nodes, since every member goes to a
// different one and a volume never spans clusters. For a supercluster that is
// each member that may take new volumes, because a volume is placed in one
// member and striped there. A cluster, or member, with no storage nodes yet
// says nothing: it is being brought up alongside the pool, and the driver
// refuses a claim that does not fit when the claim is made.
func (v *StoragePoolValidator) stripeRefusal(
	ctx context.Context, namespace string, cluster *simplyblockv1alpha2.StorageCluster, width int32,
) (string, error) {
	if cluster.Spec.Stretched != nil ||
		(cluster.Spec.EnableSyncReplication != nil && *cluster.Spec.EnableSyncReplication) {
		return fmt.Sprintf(
			"spec.volumeDefaults.stripeWidth is %d, and StorageCluster %q is synchronously replicated across "+
				"sites, where striped volumes are not supported", width, cluster.Name), nil
	}

	targets := []string{cluster.Name}
	if cluster.Spec.Supercluster != nil || cluster.Status.Role == simplyblockv1alpha2.StorageClusterRoleSupercluster {
		targets = targets[:0]
		for _, member := range cluster.Status.Members {
			if member.PlacementEligible {
				targets = append(targets, member.Name)
			}
		}
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := v.Client.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	counts := storageNodeCounts(nodes.Items)
	for _, target := range targets {
		if n := counts[target]; n > 0 && int32(n) < width {
			return fmt.Sprintf(
				"spec.volumeDefaults.stripeWidth is %d, and StorageCluster %q has %d storage nodes; a striped "+
					"volume puts every member on a different storage node of one cluster, so no claim on this "+
					"pool could be provisioned", width, target, n), nil
		}
	}
	return "", nil
}

// storageNodeCounts is how many storage nodes each cluster has, counting the
// ones that are or will be serving: a node on its way out is not one a member
// can be placed on.
func storageNodeCounts(nodes []simplyblockv1alpha2.StorageNode) map[string]int {
	counts := map[string]int{}
	for i := range nodes {
		switch nodes[i].Status.Phase {
		case simplyblockv1alpha2.StorageNodePhaseRemoving, simplyblockv1alpha2.StorageNodePhaseRemoved:
			continue
		}
		counts[nodes[i].Spec.ClusterRef]++
	}
	return counts
}

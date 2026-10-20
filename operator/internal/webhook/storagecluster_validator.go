// The StorageCluster guard: a validating webhook that refuses a second cluster
// with a storage-node workload in a namespace, and enforces what a supercluster
// and its members may be (design-superclusters.md §4.2, §4.4).
//
// The namespace limit is not a policy choice, it is what the workload can
// express. A cluster renders a storage-node DaemonSet, a headless Service, an
// EndpointSlice, and a serving certificate, and three of those carry names that
// are constants rather than anything derived from the cluster --
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
// A supercluster renders no workload, so it does not count toward the limit.
// The limit goes once every cluster records workload names of its own
// (design-superclusters.md §6.1), which needs the control plane to accept them.
//
// The supercluster rules sit here rather than in CEL where they need to read
// another object (a member's supercluster, a supercluster's members) or to
// enumerate the keys a spec carries, which CEL cannot do.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-storagecluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storageclusters,verbs=create;update;delete,versions=v1alpha2,name=vstoragecluster.simplyblock.io,admissionReviewVersions=v1

// StorageClusterValidator refuses a cluster that would share a namespace with
// another workload-carrying cluster, a member whose supercluster does not
// resolve, a supercluster carrying fields that describe nodes, and the
// deletion of a supercluster its members still name.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and a cluster created while the
// operator is down has nothing to reconcile it anyway.
type StorageClusterValidator struct {
	Client  client.Client
	Decoder admission.Decoder
}

// superclusterSpecFields are the spec fields a supercluster may carry with a
// value of its own: the field that makes it one, and the thresholds that apply
// to the members' combined capacity. Every other field describes nodes, which
// a supercluster does not have.
var superclusterSpecFields = map[string]bool{
	"supercluster":      true,
	"warningThreshold":  true,
	"criticalThreshold": true,
}

// Handle admits a cluster when every rule below holds.
func (v *StorageClusterValidator) Handle(
	ctx context.Context, req admission.Request,
) admission.Response {
	switch req.Operation {
	case admissionv1.Delete:
		return v.admitDelete(ctx, req)
	case admissionv1.Create, admissionv1.Update:
	default:
		return admission.Allowed("")
	}

	var cluster simplyblockv1alpha2.StorageCluster
	if err := v.Decoder.Decode(req, &cluster); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if cluster.Namespace == "" {
		cluster.Namespace = req.Namespace
	}

	if cluster.Spec.Supercluster != nil {
		return v.admitSupercluster(req)
	}
	if req.Operation == admissionv1.Create && cluster.Spec.SuperclusterRef != "" {
		if response := v.admitMembership(ctx, &cluster); !response.Allowed {
			return response
		}
	}
	return v.admitWorkload(ctx, &cluster)
}

// admitWorkload refuses a second cluster with a storage-node workload in one
// namespace. A supercluster renders none, so it is neither refused nor counted.
func (v *StorageClusterValidator) admitWorkload(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) admission.Response {
	var existing simplyblockv1alpha2.StorageClusterList
	if err := v.Client.List(ctx, &existing,
		client.InNamespace(cluster.Namespace)); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	for i := range existing.Items {
		other := &existing.Items[i]
		// The object under review is in this list on an update, and on a create
		// whose admission is being replayed against a cache that has already
		// seen it. Neither is a second cluster.
		if other.Name == cluster.Name || other.Spec.Supercluster != nil {
			continue
		}
		return admission.Denied(fmt.Sprintf(
			"namespace %s already holds StorageCluster %s, and a namespace holds one cluster "+
				"with storage nodes: the storage-node Service, its TLS secret, and its serving "+
				"certificate are named for the namespace rather than for the cluster, so a second "+
				"cluster here would come up with storage nodes and no workload behind them. "+
				"Put %s in a namespace of its own",
			cluster.Namespace, other.Name, cluster.Name))
	}
	return admission.Allowed("")
}

// admitMembership refuses a member whose superclusterRef names no supercluster
// in its namespace. The field is immutable, so a member admitted with a
// reference that does not resolve could only be deleted.
func (v *StorageClusterValidator) admitMembership(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) admission.Response {
	var supercluster simplyblockv1alpha2.StorageCluster
	err := v.Client.Get(ctx, client.ObjectKey{
		Namespace: cluster.Namespace, Name: cluster.Spec.SuperclusterRef,
	}, &supercluster)
	switch {
	case apierrors.IsNotFound(err):
		return admission.Denied(fmt.Sprintf(
			"StorageCluster %s names supercluster %q in spec.superclusterRef, and namespace %s "+
				"holds no StorageCluster by that name. The reference is set at creation and never "+
				"changed, so create the supercluster first, or fix the name",
			cluster.Name, cluster.Spec.SuperclusterRef, cluster.Namespace))
	case err != nil:
		return admission.Errored(http.StatusInternalServerError, err)
	case supercluster.Spec.Supercluster == nil:
		return admission.Denied(fmt.Sprintf(
			"StorageCluster %s names %q in spec.superclusterRef, and %s is a standalone cluster "+
				"rather than a supercluster. A cluster becomes a supercluster only when it is "+
				"created with spec.supercluster",
			cluster.Name, cluster.Spec.SuperclusterRef, cluster.Spec.SuperclusterRef))
	}
	return admission.Allowed("")
}

// admitSupercluster refuses a supercluster whose spec carries a field other
// than its own and the capacity thresholds. A field holding its schema default
// counts as unset, because the API server writes defaults before admission and
// the author of the object wrote none of them.
func (v *StorageClusterValidator) admitSupercluster(req admission.Request) admission.Response {
	var raw struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(req.Object.Raw, &raw); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	defaults, err := storageClusterSpecDefaults()
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	var refused []string
	for field, value := range raw.Spec {
		if superclusterSpecFields[field] {
			continue
		}
		if def, ok := defaults[field]; ok && sameJSON(def, value) {
			continue
		}
		refused = append(refused, "spec."+field)
	}
	if len(refused) == 0 {
		return admission.Allowed("")
	}
	sort.Strings(refused)
	name := raw.Metadata.Name
	if name == "" {
		name = req.Name
	}
	return admission.Denied(fmt.Sprintf(
		"supercluster %s carries %s, and a supercluster has no nodes for it to describe. "+
			"A supercluster may carry only spec.supercluster, spec.warningThreshold, and "+
			"spec.criticalThreshold; sizing and node settings belong on each member",
		name, strings.Join(refused, ", ")))
}

// admitDelete refuses the deletion of a supercluster while a member names it.
// Every member names its supercluster in an immutable field, so a member left
// behind would name nothing for the rest of its life.
func (v *StorageClusterValidator) admitDelete(
	ctx context.Context, req admission.Request,
) admission.Response {
	if len(req.OldObject.Raw) == 0 {
		return admission.Allowed("")
	}
	var cluster simplyblockv1alpha2.StorageCluster
	if err := json.Unmarshal(req.OldObject.Raw, &cluster); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if cluster.Spec.Supercluster == nil {
		return admission.Allowed("")
	}
	namespace := cluster.Namespace
	if namespace == "" {
		namespace = req.Namespace
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := v.Client.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	var members []string
	for i := range clusters.Items {
		if clusters.Items[i].Spec.SuperclusterRef == cluster.Name {
			members = append(members, clusters.Items[i].Name)
		}
	}
	if len(members) == 0 {
		return admission.Allowed("")
	}
	sort.Strings(members)
	return admission.Denied(fmt.Sprintf(
		"supercluster %s still has members (%s), and each names it in spec.superclusterRef, "+
			"which never changes. Delete the members first",
		cluster.Name, strings.Join(members, ", ")))
}

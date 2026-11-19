// The StorageClass admission webhook: a class assigned to a StoragePool by its
// labels is refused when it departs from the pool where a class may not
// (design-storagepool.md §5): it reaches another cluster or pool than its labels
// assign it to, or it stripes otherwise than the pool. It lives with the other
// validators because it is one, and the comparison itself is the pool
// package's, which the pool controller reports with.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

// +kubebuilder:webhook:path=/validate-storage-k8s-io-v1-storageclass,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.k8s.io,resources=storageclasses,verbs=create;update,versions=v1,name=storageclass-validator.simplyblock.io,admissionReviewVersions=v1

// StorageClassValidator refuses a class that disagrees with the pool it is
// assigned to. The WebhookConfiguration sends it only classes carrying the
// pool label (matchConditions), so a failurePolicy of Fail never blocks a
// class that names no pool while the operator is unavailable.
type StorageClassValidator struct {
	Client client.Client
}

// poolAssignment is the pool a class's three labels assign it to.
type poolAssignment struct {
	namespace, cluster, pool string
}

// assignmentOf reads the assignment labels, and reports false for a class
// missing any of them, which assigns it to no pool (pool.AssignmentLabels).
func assignmentOf(class *storagev1.StorageClass) (poolAssignment, bool) {
	a := poolAssignment{
		namespace: class.Labels[pool.LabelNamespace],
		cluster:   class.Labels[pool.LabelCluster],
		pool:      class.Labels[pool.LabelPool],
	}
	return a, a.namespace != "" && a.cluster != "" && a.pool != ""
}

// Handle admits or refuses one StorageClass write.
//
// A class's parameters are immutable, so an update is checked only when it
// assigns the class to a pool it was not assigned to, and a class that
// predates this webhook keeps being editable. A pool that does not exist yet
// cannot be compared with, and the class is admitted: the pool controller
// reports the disagreement once the pool is there.
func (v *StorageClassValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	class := &storagev1.StorageClass{}
	if err := json.Unmarshal(req.Object.Raw, class); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	assignment, ok := assignmentOf(class)
	if !ok {
		return admission.Allowed("not assigned to a storage pool")
	}
	if len(req.OldObject.Raw) > 0 {
		old := &storagev1.StorageClass{}
		if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if was, ok := assignmentOf(old); ok && was == assignment {
			return admission.Allowed("the pool assignment is unchanged")
		}
	}

	p := &simplyblockv1alpha2.StoragePool{}
	err := v.Client.Get(ctx, client.ObjectKey{Namespace: assignment.namespace, Name: assignment.pool}, p)
	switch {
	case apierrors.IsNotFound(err):
		return admission.Allowed("the storage pool does not exist yet")
	case err != nil:
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("read storage pool %s/%s: %w",
			assignment.namespace, assignment.pool, err))
	case p.Spec.ClusterRef != assignment.cluster:
		// The labels name a pool of that name in another cluster, which is not
		// this pool, and assign the class to nothing that exists.
		return admission.Allowed("the labels assign the class to no existing storage pool")
	}

	cluster := &simplyblockv1alpha2.StorageCluster{}
	err = v.Client.Get(ctx, client.ObjectKey{Namespace: assignment.namespace, Name: p.Spec.ClusterRef}, cluster)
	if err != nil && !apierrors.IsNotFound(err) {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("read storage cluster %s/%s: %w",
			assignment.namespace, p.Spec.ClusterRef, err))
	}

	agreement := pool.CompareClass(class.Parameters, p, cluster.Status.UUID)
	if len(agreement.Disagreements) > 0 {
		return admission.Denied(fmt.Sprintf("storage class %q is assigned to storage pool %s/%s and departs from it: %s",
			class.Name, p.Namespace, p.Name, strings.Join(agreement.Disagreements, "; ")))
	}
	return admission.Allowed("agrees with its storage pool")
}

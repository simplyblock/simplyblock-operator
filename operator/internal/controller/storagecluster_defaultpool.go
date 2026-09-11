// The one StoragePool a StorageCluster is created with.
//
// A cluster with no pool can hold no volumes, so the first pool is not a
// decision worth making a prerequisite: the cluster's own creation path creates
// it, owns it by the same controller reference every pool has, and names it for
// the cluster it belongs to. What the default is for is the case where somebody
// wants storage from a cluster they just created and has not yet decided how to
// divide it.
//
// It is an ordinary pool in every other respect, deletion included. Its limits
// are empty, so it inherits whatever the control plane's own defaults are; it
// can be edited; and it deletes like any other pool, held only by the two things
// that hold any of them.
//
// **Nothing recreates it.** A cluster that has outgrown one pool per tenant is
// not obliged to keep one, so the annotation below records that the pool was
// written and the reconcile does not write it again. That is the same rule the
// pool controller applies to the default StorageClass, and for the same reason:
// recreating an object somebody deliberately deleted is the operator arguing
// with an administrator about something neither of them needs.
//
// design-storagepool.md §4.4 is the specification.

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/pool"
)

// annotationDefaultPool records the name of the pool this cluster was created
// with. It is an annotation rather than a status field because it is a fact
// about work the operator did once, not an observation it keeps making, and it
// has to survive a status that is rebuilt from the control plane on every pass.
const annotationDefaultPool = "storage.simplyblock.io/default-pool"

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools,verbs=get;list;watch;create

// ensureDefaultPool writes the cluster's first pool, once.
//
// A failure here is reported and not returned. The default pool is a
// convenience, and a cluster whose pool could not be written is still a working
// cluster somebody can author a pool in; failing the cluster's reconcile over it
// would hold up everything else the cluster does.
func (r *StorageClusterReconciler) ensureDefaultPool(
	ctx context.Context, cluster *simplyblockv1alpha1.StorageCluster,
) {
	log := logf.FromContext(ctx)

	if cluster.Annotations[annotationDefaultPool] != "" {
		return
	}

	name := pool.DefaultPoolName(cluster.Name)
	p := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace},
		Spec: simplyblockv1alpha2.StoragePoolSpec{
			ClusterRef: cluster.Name,
			// No limits and no volume defaults. The control plane's own
			// defaults are what an administrator who has not yet decided how to
			// divide the cluster should get, and spec.volumeDefaults is
			// immutable once set: guessing a filesystem or a ceiling here would
			// be guessing one nobody could change afterward.
		},
	}
	if err := controllerutil.SetControllerReference(cluster, p, r.Scheme); err != nil {
		log.Error(err, "leaving the default pool unwritten", "pool", name)
		return
	}

	if err := r.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
		log.Error(err, "writing the cluster's default pool", "pool", name)
		return
	}

	if err := r.markDefaultPoolWritten(ctx, cluster, name); err != nil {
		// The pool exists and the note of it does not, so the next pass tries
		// the create again and finds it already there. That is the harmless
		// half of the two orderings; writing the note first would be the other
		// one, where a failed create is never retried.
		log.Error(err, "recording the cluster's default pool", "pool", name)
		return
	}
	log.Info("wrote the cluster's default pool", "pool", name)
}

func (r *StorageClusterReconciler) markDefaultPoolWritten(
	ctx context.Context, cluster *simplyblockv1alpha1.StorageCluster, name string,
) error {
	base := cluster.DeepCopy()
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[annotationDefaultPool] = name
	if err := r.Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("annotate cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}

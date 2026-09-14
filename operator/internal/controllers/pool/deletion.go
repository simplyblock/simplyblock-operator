// Deleting a pool, and what stops a cluster deletion from taking tenant data
// with it.
//
// A StorageCluster owns its pools by controller reference, so deleting a cluster
// cascades to them. That is the dangerous half of the arrangement and the
// finalizer here is the other: a pool refuses to finish deleting while anything
// Kubernetes knows about still refers to it, and the cluster's own deletion
// queues up behind it. The result is a `kubectl delete storagecluster` that
// visibly does not finish, with an event naming the pool and what is still in
// it — which is the whole point, because correct behavior here looks exactly
// like a stuck finalizer and an administrator who reaches for --force gets the
// outcome the hold exists to prevent.
//
// Two things hold it, and they are separate reasons because the remedies are
// different.
//
// **An authored StorageClass holds it**, because the operator cannot clean the
// class up. It is cluster-scoped, somebody else wrote it, and deleting
// somebody's provisioning contract to let a pool go is not a trade this operator
// gets to make. Refusing is the honest alternative: the class is one object,
// whoever wrote it can delete it, and the pool then goes. The class the operator
// wrote for a default pool does not hold, because deleting that one is cleanup
// rather than a decision — the operator deletes what it created and refuses on
// what it did not, which is why one label is enough to tell the two apart.
//
// **A bound PersistentVolume holds it**, because the data is real.
//
// Only objects Kubernetes knows about hold a deletion, and that is the rule
// rather than a compromise: a hold is a promise to a person that something they
// can see still needs them, and a control-plane volume they cannot list and
// cannot delete through Kubernetes cannot be that. The consequence is stated
// plainly — a pool holding a volume provisioned out of band will delete — and
// what covers it is that the control plane refuses to delete a pool with volumes
// in it, so the failure surfaces as PoolDeletionFailed rather than as lost data.
//
// design-storagepool.md §6 is the specification.

package pool

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// reconcileDeletion runs the teardown and the two holds in front of it.
//
// An empty clusterUUID means the cluster is already gone, which is not an edge
// case: it is the cascade, and garbage collection deletes a pool as soon as its
// owner is removed. Everything here still applies in that case except the one
// call that needs a control plane, because both holds are about Kubernetes
// objects that are still there to be seen, and the local cleanup is Kubernetes
// objects too. Only the backend DELETE is skipped.
func (r *StoragePoolReconciler) reconcileDeletion(
	ctx context.Context,
	p *simplyblockv1alpha2.StoragePool,
	api *webapi.Client,
	clusterUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(p, FinalizerStoragePool) {
		return ctrl.Result{}, nil
	}
	if r.VolumeScopes != nil && clusterUUID != "" && p.Status.UUID != "" {
		r.VolumeScopes.Remove(cpinformer.Scope{clusterUUID, p.Status.UUID})
	}

	classes, _, err := AssignedClasses(ctx, r.Client, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	if authored := authoredClasses(classes); len(authored) > 0 {
		r.event(p, corev1.EventTypeWarning, StorageClassStillAssigned,
			"this pool cannot be deleted while %s assigned to it; "+
				"delete %s and the pool goes on its own",
			pluralClasses(authored), strings.Join(authored, ", "))
		return r.hold(ctx, p, simplyblockv1alpha2.StoragePoolPhaseDeleting,
			fmt.Sprintf("held: storage class %s is still assigned", strings.Join(authored, ", ")),
			requeueHeld)
	}

	bound, err := r.boundVolumes(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(bound) > 0 {
		r.event(p, corev1.EventTypeWarning, VolumesStillBound,
			"this pool cannot be deleted while %d persistent volume(s) are bound to it, "+
				"starting with %q; delete the claims and the pool goes on its own",
			len(bound), bound[0])
		return r.hold(ctx, p, simplyblockv1alpha2.StoragePoolPhaseDeleting,
			fmt.Sprintf("held: %d persistent volume(s) still bound", len(bound)),
			requeueHeld)
	}

	// Nothing holds it. The operator's own classes go first, because a class
	// that outlived its pool provisions claims that then fail in the control
	// plane rather than in Kubernetes.
	for i := range classes {
		class := &classes[i]
		if err := r.Delete(ctx, class); err != nil && client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("delete the storage class %q: %w", class.Name, err)
		}
		log.Info("deleted the storage class the operator wrote for this pool", "class", class.Name)
	}

	if p.Status.UUID != "" {
		// A cluster that is already gone took its pools with it, so there is no
		// backend pool left to delete and nothing to ask. Skipping the call is
		// the only thing an absent cluster changes.
		if clusterUUID != "" {
			if err := r.deleteBackendPool(ctx, api, clusterUUID, p); err != nil {
				r.event(p, corev1.EventTypeWarning, PoolDeletionFailed,
					"the control plane refused to delete pool %q: %v", p.Name, err)
				return ctrl.Result{RequeueAfter: requeueBackend}, nil
			}
		}
		// The labels come off last, because they are what a volume still in the
		// pool would have needed, and clearing them before the pool is gone
		// would strip a running workload's access on the way to a delete that
		// might yet fail.
		if err := r.syncNodeLabels(ctx, p, nil); err != nil {
			log.Error(err, "clearing the pool's node labels")
			return ctrl.Result{RequeueAfter: requeueNotReady}, nil
		}
	}

	return r.releaseFinalizer(ctx, p)
}

func (r *StoragePoolReconciler) releaseFinalizer(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) (ctrl.Result, error) {
	base := p.DeepCopy()
	controllerutil.RemoveFinalizer(p, FinalizerStoragePool)
	if err := r.Patch(ctx, p, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("release the finalizer on pool %s/%s: %w",
			p.Namespace, p.Name, err)
	}
	return ctrl.Result{}, nil
}

// deleteBackendPool asks the control plane to drop the pool. A 404 is success,
// since a pool already gone is a pool deleted; every other refusal is retried,
// because the likeliest one is the backend saying the pool still holds volumes,
// and treating that as success is how data is lost.
func (r *StoragePoolReconciler) deleteBackendPool(
	ctx context.Context,
	api *webapi.Client,
	clusterUUID string,
	p *simplyblockv1alpha2.StoragePool,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, p.Status.UUID)
	body, status, err := api.Do(ctx, http.MethodDelete, endpoint, nil)
	switch {
	case err != nil:
		return err
	case status == http.StatusNotFound:
		return nil
	case status >= 300:
		return fmt.Errorf("status %d: %s", status, string(body))
	}
	return nil
}

// authoredClasses returns the names of the classes the operator did not write,
// which are the ones it refuses to delete.
func authoredClasses(classes []storagev1.StorageClass) []string {
	var names []string
	for i := range classes {
		if !IsOperatorManaged(&classes[i]) {
			names = append(names, classes[i].Name)
		}
	}
	return names
}

func pluralClasses(names []string) string {
	if len(names) == 1 {
		return "a storage class is"
	}
	return fmt.Sprintf("%d storage classes are", len(names))
}

// boundVolumes returns the names of the PersistentVolumes provisioned out of
// this pool.
//
// The match is on the volume handle's pool segment rather than on the volume's
// StorageClass, and the difference matters exactly when it is needed: a class
// deleted out of band would otherwise hide every volume it provisioned, and a
// deletion held on nothing is the same as no hold at all. The segment is
// compared against both the pool's UUID and its name, because a handle
// provisioned before the v2 API migration encodes the name and the field it
// lives in is immutable.
func (r *StoragePoolReconciler) boundVolumes(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) ([]string, error) {
	var volumes corev1.PersistentVolumeList
	if err := r.List(ctx, &volumes); err != nil {
		return nil, fmt.Errorf("list the persistent volumes: %w", err)
	}

	var bound []string
	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if !kube.IsManaged(pv) {
			continue
		}
		segment := handlePoolSegment(pv.Spec.CSI.VolumeHandle)
		if segment == "" {
			continue
		}
		if segment == p.Status.UUID || segment == p.Name {
			bound = append(bound, pv.Name)
		}
	}
	return bound, nil
}

// handlePoolSegment reads the middle field of a clusterID:poolID:volumeID
// handle. It is split by hand rather than through lvol.VolumeHandle.Split
// because that one requires three UUIDs and the whole reason to look here is the
// generation of handles whose pool segment is a name.
func handlePoolSegment(handle string) string {
	parts := strings.Split(strings.TrimSpace(handle), ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

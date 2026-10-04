// What one pass has to know about the world before it can act: which volume,
// which cluster, which pool, and which node the volume is being moved to.
//
// None of it is declared on the operation. A PersistentVolumeOps names a
// volume and a target node object, and everything else is read out of the
// volume's CSI handle — the cluster, the pool, and the volume itself. That
// handle is stamped on the volume at provisioning and is immutable for its
// life, so no StorageClass is consulted and a class edited, replaced, or
// deleted out of band changes nothing about an existing volume's
// addressability.
//
// design-persistentvolumeops.md §3 is the specification.

package volume

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// subject is the resolved world one pass acts on.
type subject struct {
	pv     *corev1.PersistentVolume
	handle lvol.Handle

	// cluster is the StorageCluster reporting the volume's cluster UUID. Its
	// namespace is where this operation's Jobs are created, which a
	// cluster-scoped object has no namespace of its own to decide.
	cluster     *simplyblockv1alpha2.StorageCluster
	clusterUUID string

	targetRef  simplyblockv1alpha2.StorageNodeReference
	targetUUID string
}

func (s *subject) namespace() string      { return s.cluster.Namespace }
func (s *subject) targetNodeName() string { return s.targetRef.Name }

// resolve reads the operation's world, or says why it cannot.
//
// Three outcomes are distinguished, because the operation does something
// different with each. errVolumeGone ends the operation without it having gone
// wrong. A terminalStepError is a condition no reconcile can fix, and fails it.
// Anything else is a not-yet and is retried.
func (r *PersistentVolumeOpsReconciler) resolve(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (*subject, error) {
	var pv corev1.PersistentVolume
	err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.PersistentVolumeName}, &pv)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("%w: %s", errVolumeGone, ops.Spec.PersistentVolumeName)
	case err != nil:
		return nil, fmt.Errorf("read volume %s: %w", ops.Spec.PersistentVolumeName, err)
	case !pv.DeletionTimestamp.IsZero():
		// A volume being deleted is one whose backing logical volume the driver
		// is about to remove, so moving it is work nobody will read.
		return nil, fmt.Errorf("%w: %s is being deleted", errVolumeGone, pv.Name)
	}

	handle, err := r.addressOf(ctx, &pv)
	if err != nil {
		return nil, err
	}

	cluster, err := r.clusterReporting(ctx, handle.ClusterID)
	if err != nil {
		return nil, err
	}

	target, err := r.targetNode(ctx, ops, cluster)
	if err != nil {
		return nil, err
	}

	return &subject{
		pv:          &pv,
		handle:      handle,
		cluster:     cluster,
		clusterUUID: handle.ClusterID,
		targetRef:   ops.Spec.Migrate.TargetNodeRef,
		targetUUID:  target,
	}, nil
}

// addressOf reads the cluster, pool, and volume out of the volume's CSI handle.
//
// ParseHandle rather than Split, because the pool segment is not always a UUID:
// volumes provisioned before the v2 API migration encode the pool's name, and a
// PersistentVolume outlives every driver upgrade, so a cluster holds a mixture
// indefinitely. Nothing here needs the pool typed — the migration is addressed
// by cluster and subsystem — so requiring a UUID would refuse to move volumes
// that are otherwise perfectly movable.
func (r *PersistentVolumeOpsReconciler) addressOf(
	ctx context.Context, pv *corev1.PersistentVolume,
) (lvol.Handle, error) {
	if pv.Spec.CSI == nil {
		return lvol.Handle{}, fatalf(
			"volume %s has no CSI source, so it has no logical volume to move", pv.Name)
	}

	ours, err := r.driverNames(ctx)
	if err != nil {
		return lvol.Handle{}, err
	}
	if !ours[pv.Spec.CSI.Driver] {
		return lvol.Handle{}, fatalf(
			"volume %s was provisioned by driver %s, which this operator has no means to act on",
			pv.Name, pv.Spec.CSI.Driver)
	}

	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !ok {
		return lvol.Handle{}, fatalf(
			"volume %s carries the handle %q, which is not <cluster>:<pool>:<volume>",
			pv.Name, pv.Spec.CSI.VolumeHandle)
	}
	return handle, nil
}

// driverNames is the set of CSI drivers whose volumes this operator can move.
//
// It is read from the SimplyblockDriver objects rather than fixed, because
// spec.driverName is settable and immutable: a deployment that chose another
// name at install has volumes carrying that name forever, and matching only the
// default would refuse every one of them. The default stands in when no driver
// object exists, which is a cluster whose driver this operator does not manage.
func (r *PersistentVolumeOpsReconciler) driverNames(ctx context.Context) (map[string]bool, error) {
	var drivers simplyblockv1alpha2.SimplyblockDriverList
	if err := r.List(ctx, &drivers); err != nil {
		return nil, fmt.Errorf("list the CSI drivers this operator manages: %w", err)
	}
	names := map[string]bool{}
	for i := range drivers.Items {
		if name := drivers.Items[i].Spec.DriverName; name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		names[CSIDriverName] = true
	}
	return names, nil
}

// clusterReporting finds the StorageCluster whose status reports this UUID.
//
// A cluster that reports none has not been created in the backend yet, which is
// a not-yet rather than a mismatch, so it is retried: the operation may have
// been written against a cluster that is still coming up.
func (r *PersistentVolumeOpsReconciler) clusterReporting(
	ctx context.Context, uuid string,
) (*simplyblockv1alpha2.StorageCluster, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return nil, fmt.Errorf("list the storage clusters: %w", err)
	}
	for i := range clusters.Items {
		if clusters.Items[i].Status.UUID == uuid {
			return &clusters.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no StorageCluster reports cluster %s yet", uuid)
}

// targetNode resolves the backend UUID of the node the volume is moving to.
//
// The operation names a Kubernetes object, so that a migration can be written
// by hand without looking a UUID up, and this is where the two are joined. A
// node in a different cluster than the volume is refused at admission; it is
// checked again here because admission happens once and the objects can change
// afterward.
func (r *PersistentVolumeOpsReconciler) targetNode(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	cluster *simplyblockv1alpha2.StorageCluster,
) (string, error) {
	ref := ops.Spec.Migrate.TargetNodeRef

	var node simplyblockv1alpha2.StorageNode
	err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &node)
	switch {
	case apierrors.IsNotFound(err):
		return "", fatalf("no StorageNode %s/%s to move the volume to", ref.Namespace, ref.Name)
	case err != nil:
		return "", fmt.Errorf("read the target node %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	if node.Namespace != cluster.Namespace || node.Spec.ClusterRef != cluster.Name {
		return "", fatalf(
			"node %s/%s belongs to cluster %s and the volume to %s, and a volume cannot move between clusters",
			ref.Namespace, ref.Name, node.Spec.ClusterRef, cluster.Name)
	}

	if node.Status.UUID == "" {
		return "", fmt.Errorf("node %s/%s has not been created in the backend yet", ref.Namespace, ref.Name)
	}
	if node.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOnline {
		// A fact about now rather than about the operation. The node a drain
		// fans fifty migrations out to may well be online by the time the
		// fifteenth of them acquires its lock, which is why this is a phase and
		// an event rather than an admission rejection.
		r.event(ops, corev1.EventTypeWarning, ReasonTargetNodeNotReady,
			"Node %s/%s is %s rather than online", ref.Namespace, ref.Name, node.Status.Phase)
		return "", fmt.Errorf("node %s/%s is %s rather than online",
			ref.Namespace, ref.Name, node.Status.Phase)
	}
	return node.Status.UUID, nil
}

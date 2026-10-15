// Bucket delivery: reading the ObjectStore's gateway Deployment's pod
// placement, diffing it against status.mountDeliveredNodes, and calling
// DeliverBucket/WithdrawBucket for whatever changed (design-bucket-
// provisioning.md §5.3, W-12). Runs on every reconcile pass while
// Provisioning or Ready, independent of the phase machine itself: a gateway
// pod rescheduled mid-Ready still needs its new node delivered to within one
// reconcile, and Ready has no outgoing edge for that to ride on.
//
// No field is read off the ObjectStore: the gateway Deployment and its pods
// are named and labeled deterministically from spec.objectStoreRef alone
// (design-objectstore.md), which is what lets this reconciler find them
// without ever reading that object's own spec or status.

package objectstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/bucketdelivery/bucketdeliveryrpc"
	"github.com/simplyblock/atlas/link"
	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// gatewayInstanceLabel is the label every gateway pod of an ObjectStore
// carries, fixed by design-objectstore.md rather than read off the
// ObjectStore object.
const gatewayInstanceLabel = "objectstore.simplyblock.io/instance"

// desiredDeliveryNodes reports the distinct, scheduled nodes an ObjectStore's
// gateway Deployment currently places pods on.
//
// apierrors.IsNotFound on the returned error means the Deployment itself
// does not exist (not yet created, or deleted): design §6's failure table
// says a bucket's existing deliveries are left in place in that case, not
// withdrawn, which the caller distinguishes from a Deployment that exists
// with zero pods (U-14, where everything is withdrawn) by checking for it.
func (r *S3BucketReconciler) desiredDeliveryNodes(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) ([]string, error) {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: bucket.Namespace, Name: bucket.Spec.ObjectStoreRef,
	}, &deployment); err != nil {
		return nil, err
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(bucket.Namespace),
		client.MatchingLabels{gatewayInstanceLabel: bucket.Spec.ObjectStoreRef}); err != nil {
		return nil, err
	}

	nodeSet := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Not yet scheduled is not yet a node to deliver to. Terminating
		// (DeletionTimestamp set, lingering until kubelet reports it gone)
		// is no longer one either: a rolling replacement already has its
		// successor pod listed on the new node, and waiting for this one to
		// fully disappear would delay withdrawing from the old one for no
		// reason this reconciler needs to.
		if pod.DeletionTimestamp == nil && pod.Spec.NodeName != "" {
			nodeSet[pod.Spec.NodeName] = struct{}{}
		}
	}
	nodes := make([]string, 0, len(nodeSet))
	for node := range nodeSet {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes, nil
}

// diffDeliveryNodes reports which nodes a bucket newly needs delivered to
// and which it needs withdrawn from, comparing the gateway's current pod
// placement against what status already records. Pure, and sorted, so two
// equal node sets always diff to nil regardless of discovery order (U-16).
func diffDeliveryNodes(desired, delivered []string) (toDeliver, toWithdraw []string) {
	desiredSet := make(map[string]struct{}, len(desired))
	for _, n := range desired {
		desiredSet[n] = struct{}{}
	}
	deliveredSet := make(map[string]struct{}, len(delivered))
	for _, n := range delivered {
		deliveredSet[n] = struct{}{}
	}

	for _, n := range desired {
		if _, ok := deliveredSet[n]; !ok {
			toDeliver = append(toDeliver, n)
		}
	}
	for _, n := range delivered {
		if _, ok := desiredSet[n]; !ok {
			toWithdraw = append(toWithdraw, n)
		}
	}
	sort.Strings(toDeliver)
	sort.Strings(toWithdraw)
	return toDeliver, toWithdraw
}

// reconcileMountDelivery converges status.mountDeliveredNodes toward the
// gateway's current pod placement, and reports whether that set is now
// fully converged (nothing left to deliver or withdraw) so
// reconcileProvisioning knows whether Ready is reached. It is also called
// while already Ready (Reconcile's terminal path), where the return value is
// not read: a gateway pod moving to a new node is this function's job to
// notice regardless of phase.
func (r *S3BucketReconciler) reconcileMountDelivery(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket, export *simplyblockv1alpha2.NFSExport,
) (complete bool, err error) {
	if r.Registry == nil {
		// Mount delivery is not wired in this process. Nothing this pass can
		// do.
		return false, nil
	}
	if export == nil {
		// No matched NFSExport (gone, out from under a Ready bucket; not a
		// design-covered scenario). Nothing new can be delivered without its
		// VolumeContext, so this pass is skipped rather than guessed at;
		// existing deliveries are left exactly as they are.
		return false, nil
	}

	desired, err := r.desiredDeliveryNodes(ctx, bucket)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// design §6: no gateway Deployment to target, so existing
			// deliveries are left exactly as they are.
			return len(bucket.Status.MountDeliveredNodes) == 0, nil
		}
		return false, err
	}

	toDeliver, toWithdraw := diffDeliveryNodes(desired, bucket.Status.MountDeliveredNodes)
	if len(toDeliver) == 0 && len(toWithdraw) == 0 {
		return true, nil
	}

	delivered := make(map[string]struct{}, len(bucket.Status.MountDeliveredNodes))
	for _, n := range bucket.Status.MountDeliveredNodes {
		delivered[n] = struct{}{}
	}

	var firstErr error
	for _, node := range toDeliver {
		err := r.deliverToNode(ctx, bucket, export, node)
		switch {
		case err == nil:
			delivered[node] = struct{}{}
			r.recordMountDeliveryCall(bucket, node, "deliver", "success")
		case errors.Is(err, link.ErrNoSession):
			// Normal during a node's own rollout, not a failure (the same
			// rule NFSExportReconciler's own HasSession check follows): left
			// out of the delivered set so the next pass retries it, without
			// erroring this one.
			r.recordMountDeliveryCall(bucket, node, "deliver", "no_session")
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("delivering bucket %s to node %s: %w", bucket.Spec.BucketName, node, err)
			}
			r.recordMountDeliveryCall(bucket, node, "deliver", "error")
		}
	}
	for _, node := range toWithdraw {
		if err := r.withdrawFromNode(ctx, bucket, node); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("withdrawing bucket %s from node %s: %w", bucket.Spec.BucketName, node, err)
			}
			r.recordMountDeliveryCall(bucket, node, "withdraw", "error")
			continue
		}
		delete(delivered, node)
		r.recordMountDeliveryCall(bucket, node, "withdraw", "success")
	}

	newSet := make([]string, 0, len(delivered))
	for node := range delivered {
		newSet = append(newSet, node)
	}
	sort.Strings(newSet)
	if err := r.writeStatus(ctx, bucket, func(s *objectstorev1alpha1.S3BucketStatus) {
		s.MountDeliveredNodes = newSet
	}); err != nil {
		return false, err
	}

	if firstErr != nil {
		return false, firstErr
	}
	remainingDeliver, remainingWithdraw := diffDeliveryNodes(desired, newSet)
	return len(remainingDeliver) == 0 && len(remainingWithdraw) == 0, nil
}

// recordMountDeliveryCall publishes simplyblock_objectstore_mount_delivery_calls_total
// (design §10): one DeliverBucket/WithdrawBucket call outcome, by node and
// result.
func (r *S3BucketReconciler) recordMountDeliveryCall(bucket *objectstorev1alpha1.S3Bucket, node, operation, outcome string) {
	mountDeliveryCallsTotal.WithLabelValues(bucket.Spec.ObjectStoreRef, node, operation, outcome).Inc()
}

// deliverToNode asks one node's CSI plugin to deliver the bucket's mount.
// The VolumeContext DeliverBucket needs is read from the matched NFSExport
// rather than stored anywhere new: export_service is its ServiceAddress and
// export_path is its spec.exportPath, the same two fields a kubelet-driven
// pNFS stage would have received from CreateVolume.
func (r *S3BucketReconciler) deliverToNode(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket, export *simplyblockv1alpha2.NFSExport, node string,
) error {
	conn, err := r.Registry.Conn(link.NodePeer(node))
	if err != nil {
		return err
	}
	handle, err := r.boundVolumeHandle(ctx, bucket)
	if err != nil {
		return err
	}
	if handle == "" {
		return fmt.Errorf("bucket %s has no bound volume handle yet", bucket.Spec.BucketName)
	}
	return bucketdeliveryrpc.Remote(conn).Deliver(ctx, bucketdeliveryrpc.Bucket{
		Name:         bucket.Spec.BucketName,
		VolumeHandle: handle,
		VolumeContext: map[string]string{
			"access_protocol": "nfs",
			"export_service":  export.Status.ServiceAddress,
			"export_path":     export.Spec.ExportPath,
		},
	})
}

// bucketsForGatewayPod maps a gateway pod event to every S3Bucket in its
// namespace naming that pod's ObjectStore, the trigger SetupWithManager
// registers this reconciler's Pod watch with. A plain List rather than a
// field indexer: the number of buckets a cluster carries is not the kind of
// scale this would need one for.
func (r *S3BucketReconciler) bucketsForGatewayPod(ctx context.Context, pod *corev1.Pod) []ctrlreconcile.Request {
	objectStoreRef := pod.Labels[gatewayInstanceLabel]
	if objectStoreRef == "" {
		return nil
	}
	var buckets objectstorev1alpha1.S3BucketList
	if err := r.List(ctx, &buckets, client.InNamespace(pod.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "listing buckets for a gateway pod event", "pod", pod.Name)
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range buckets.Items {
		if buckets.Items[i].Spec.ObjectStoreRef == objectStoreRef {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: client.ObjectKeyFromObject(&buckets.Items[i])})
		}
	}
	return requests
}

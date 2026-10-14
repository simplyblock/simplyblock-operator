// S3BucketReconciler drives one S3 bucket's backing volume from creation to
// Ready and back: the PersistentVolumeClaim, the NFSExport bound to it, and
// (once W-12 lands) the per-node mount deliveries that make its gateway's
// pods see it (design-bucket-provisioning.md).
//
// Bucket delivery itself (listing the ObjectStore's gateway pods, diffing
// the node set, and calling DeliverBucket/WithdrawBucket) is W-12's own
// work, built on the same reconciler. Until it lands, mountDeliveryComplete
// is the seam: a bucket reaches Ready once its PVC is bound and its NFSExport
// is Ready, with nothing yet to wait on for delivery, rather than this phase
// machine waiting forever on a step nothing performs yet.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/bucketdelivery/bucketdeliveryrpc"
	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/statemachine"
	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// phase is an alias so the graph below reads as a table.
type phase = objectstorev1alpha1.S3BucketPhase

const (
	phasePending      = objectstorev1alpha1.S3BucketPhasePending
	phaseProvisioning = objectstorev1alpha1.S3BucketPhaseProvisioning
	phaseReady        = objectstorev1alpha1.S3BucketPhaseReady
	phaseDegraded     = objectstorev1alpha1.S3BucketPhaseDegraded
	phaseDeleting     = objectstorev1alpha1.S3BucketPhaseDeleting
)

// Condition types this reconciler sets. VolumeBound, ExportReady, and
// MountDelivered are design Appendix A's own names. ObjectStoreReady is the
// one gap in that list (the design named MountDelivered but not what covers
// spec.objectStoreRef resolving at all), added here rather than overloading
// ExportReady for an unrelated kind of not-found.
const (
	conditionVolumeBound    = "VolumeBound"
	conditionObjectStoreOK  = "ObjectStoreReady"
	conditionExportReady    = "ExportReady"
	conditionMountDelivered = "MountDelivered"
)

// The controller's cadence, in one place so it can be read and tuned.
const (
	// No PersistentVolumeClaim is Bound yet. A StorageClass that never binds
	// one (design §9) is expected to retry forever rather than degrade, so
	// this is the only pace that case has.
	s3BucketPendingRequeue = 10 * time.Second
	// How long Provisioning may wait on an NFSExport appearing, becoming
	// Ready, and (once W-12 reads it) mount delivery completing, before the
	// bucket degrades.
	s3BucketProvisioningDeadline = 5 * time.Minute
	// Come back with a re-read object after a write that bumped
	// resourceVersion, so status is not written over a copy already stale.
	s3BucketFreshCopyRequeue = time.Second
)

// S3BucketReconciler reconciles S3Bucket objects.
type S3BucketReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Registry resolves a node name to its csi-link session, for bucket
	// delivery (W-12). Nil until then: this reconciler's own phase machine
	// does not yet need it.
	Registry PeerRegistry
}

// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=s3buckets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=s3buckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=s3buckets/finalizers,verbs=update
// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=objectstores,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=nfsexports,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *S3BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("s3bucket", req.NamespacedName)

	var bucket objectstorev1alpha1.S3Bucket
	if err := r.Get(ctx, req.NamespacedName, &bucket); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !bucket.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &bucket)
	}

	// Degraded is terminal on purpose, the same reason NFSExport's is: it is
	// reached by refusing to act, and retrying the refusal on a timer would
	// bury the event that explains it.
	if bucket.Status.Phase == phaseDegraded {
		return ctrl.Result{}, nil
	}

	if added, err := r.ensureFinalizer(ctx, &bucket); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{RequeueAfter: s3BucketFreshCopyRequeue}, nil
	}

	sm, err := r.restore(ctx, &bucket)
	if err != nil {
		r.event(&bucket, corev1.EventTypeWarning, "PhaseUnrecognized",
			fmt.Sprintf("cannot restore phase %q", bucket.Status.Phase))
		return ctrl.Result{}, err
	}
	defer sm.Close()

	if sm.IsTerminal() {
		return ctrl.Result{}, nil
	}

	if sm.TimeoutReached() {
		reason, message := r.provisioningFailure(ctx, &bucket)
		return ctrl.Result{}, r.toDegraded(ctx, &bucket, sm, reason, message)
	}

	switch sm.CurrentState() {
	case phasePending:
		return r.reconcilePending(ctx, &bucket, sm, logger)
	case phaseProvisioning:
		return r.reconcileProvisioning(ctx, &bucket, sm, logger)
	default:
		// restore refuses to build a machine for an unknown phase, so this is
		// a known phase with nothing to do in it.
		return ctrl.Result{}, nil
	}
}

// reconcilePending ensures the backing PVC exists and waits for it to bind.
// The PVC create is the side effect. Nothing here needs an OnEnter hook
// because Pending is the graph's own starting point.
func (r *S3BucketReconciler) reconcilePending(
	ctx context.Context,
	bucket *objectstorev1alpha1.S3Bucket,
	sm *statemachine.Machine[phase],
	logger logr,
) (ctrl.Result, error) {
	pvc, err := r.ensurePVC(ctx, bucket)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring the backing PersistentVolumeClaim: %w", err)
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		r.event(bucket, corev1.EventTypeWarning, "PersistentVolumeClaimNotBound",
			fmt.Sprintf("waiting for PersistentVolumeClaim %s to bind", pvc.Name))
		if err := r.writeStatus(ctx, bucket, func(s *objectstorev1alpha1.S3BucketStatus) {
			s.Phase = phasePending
			s.PersistentVolumeClaimName = pvc.Name
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type: conditionVolumeBound, Status: metav1.ConditionFalse,
				Reason: "PersistentVolumeClaimNotBound", Message: "waiting for the backing claim to bind",
			})
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: s3BucketPendingRequeue}, nil
	}

	logger.Info("backing claim bound, entering Provisioning", "pvc", pvc.Name)
	if err := sm.TransitionTo(ctx, phaseProvisioning); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Provisioning: %w", err)
	}
	if err := r.writeStatus(ctx, bucket, func(s *objectstorev1alpha1.S3BucketStatus) {
		s.Phase = phaseProvisioning
		s.PersistentVolumeClaimName = pvc.Name
		s.Message = "waiting for the NFSExport and mount delivery"
		setDeadline(s, sm)
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionVolumeBound, Status: metav1.ConditionTrue, Reason: "PersistentVolumeClaimBound",
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: s3BucketFreshCopyRequeue}, nil
}

// reconcileProvisioning waits on the ObjectStore existing, the NFSExport
// appearing and becoming Ready, and (once W-12 reads mountDeliveryComplete)
// every node confirming delivery. Not every pass is a transition: most of
// them find nothing new and only re-poll.
func (r *S3BucketReconciler) reconcileProvisioning(
	ctx context.Context,
	bucket *objectstorev1alpha1.S3Bucket,
	sm *statemachine.Machine[phase],
	logger logr,
) (ctrl.Result, error) {
	exists, err := r.objectStoreExists(ctx, bucket)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !exists {
		r.event(bucket, corev1.EventTypeWarning, "ObjectStoreNotFound",
			fmt.Sprintf("ObjectStore %q does not exist", bucket.Spec.ObjectStoreRef))
		return ctrl.Result{}, r.toDegraded(ctx, bucket, sm, "ObjectStoreNotFound",
			fmt.Sprintf("spec.objectStoreRef %q does not resolve to an ObjectStore", bucket.Spec.ObjectStoreRef))
	}

	export, err := r.matchingNFSExport(ctx, bucket)
	if err != nil {
		return ctrl.Result{}, err
	}
	if export == nil {
		// Bounded by the phase deadline, checked once per pass in Reconcile
		// before this is ever called.
		return ctrl.Result{RequeueAfter: s3BucketFreshCopyRequeue}, nil
	}

	if export.Status.Phase == simplyblockv1alpha2.NFSExportPhaseDegraded {
		return ctrl.Result{}, r.toDegraded(ctx, bucket, sm, "ExportDegraded",
			fmt.Sprintf("NFSExport %s is Degraded: %s", export.Name, export.Status.Message))
	}
	if export.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady {
		if err := r.writeStatus(ctx, bucket, func(s *objectstorev1alpha1.S3BucketStatus) {
			s.NFSExportName = export.Name
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: s3BucketFreshCopyRequeue}, nil
	}

	if !r.mountDeliveryComplete(bucket) {
		return ctrl.Result{RequeueAfter: s3BucketFreshCopyRequeue}, nil
	}

	logger.Info("provisioning satisfied, entering Ready", "export", export.Name)
	if err := sm.TransitionTo(ctx, phaseReady); err != nil {
		return ctrl.Result{}, fmt.Errorf("transition to Ready: %w", err)
	}
	if err := r.writeStatus(ctx, bucket, func(s *objectstorev1alpha1.S3BucketStatus) {
		s.Phase = phaseReady
		s.NFSExportName = export.Name
		s.Message = ""
		s.PhaseDeadline = nil
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionObjectStoreOK, Status: metav1.ConditionTrue, Reason: "ObjectStoreFound",
		})
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionExportReady, Status: metav1.ConditionTrue, Reason: "NFSExportReady",
		})
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionMountDelivered, Status: metav1.ConditionTrue, Reason: "NothingToDeliverYet",
			Message: "mount delivery is not yet implemented (W-12); treated as satisfied",
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(bucket, corev1.EventTypeNormal, "BucketReady", fmt.Sprintf("bucket %s is ready", bucket.Spec.BucketName))
	return ctrl.Result{}, nil
}

// mountDeliveryComplete reports whether every node this bucket's gateway
// serves from has confirmed delivery. W-12 replaces this with a real check
// against the ObjectStore's gateway Deployment's pods and
// status.mountDeliveredNodes. Until it lands there is no gateway node set
// computed anywhere, so treating delivery as satisfied is what lets Ready be
// reached at all rather than waiting forever on a step nothing performs.
func (r *S3BucketReconciler) mountDeliveryComplete(*objectstorev1alpha1.S3Bucket) bool {
	return true
}

// provisioningFailure names why Provisioning timed out, inspecting the same
// state advanceProvisioning reads. Called once, from Reconcile, when
// sm.TimeoutReached() is true.
func (r *S3BucketReconciler) provisioningFailure(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (reason, message string) {
	if exists, err := r.objectStoreExists(ctx, bucket); err == nil && !exists {
		return "ObjectStoreNotFound", fmt.Sprintf("spec.objectStoreRef %q does not resolve to an ObjectStore", bucket.Spec.ObjectStoreRef)
	}
	if export, err := r.matchingNFSExport(ctx, bucket); err == nil {
		if export == nil {
			return "NoMatchingExport", "no NFSExport appeared referencing the bound volume within the deadline"
		}
		if export.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady {
			return "NoMatchingExport", fmt.Sprintf("NFSExport %s did not reach Ready within the deadline", export.Name)
		}
	}
	// NFSExport is Ready, so the only thing left Provisioning could still be
	// waiting on is mount delivery (W-12).
	return "MountDeliveryFailed", "mount delivery was not confirmed on every node within the deadline"
}

// toDegraded parks the bucket and says why, in a condition and an event.
func (r *S3BucketReconciler) toDegraded(
	ctx context.Context,
	bucket *objectstorev1alpha1.S3Bucket,
	sm *statemachine.Machine[phase],
	reason, message string,
) error {
	if err := sm.TransitionTo(ctx, phaseDegraded); err != nil {
		return fmt.Errorf("transition to Degraded: %w", err)
	}
	r.event(bucket, corev1.EventTypeWarning, reason, message)
	conditionType := conditionExportReady
	switch reason {
	case "ObjectStoreNotFound":
		conditionType = conditionObjectStoreOK
	case "MountDeliveryFailed":
		conditionType = conditionMountDelivered
	}
	return r.writeStatus(ctx, bucket, func(s *objectstorev1alpha1.S3BucketStatus) {
		s.Phase = phaseDegraded
		s.Message = message
		s.PhaseDeadline = nil
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: conditionType, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		})
	})
}

// ensurePVC creates the backing PersistentVolumeClaim if it does not already
// exist, and returns the current copy either way. Idempotent: a reconcile
// that died between the create and the status write finds the claim already
// there on its next pass.
func (r *S3BucketReconciler) ensurePVC(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (*corev1.PersistentVolumeClaim, error) {
	name := pvcNameFor(bucket.Spec.BucketName)
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: bucket.Namespace, Name: name}, &pvc)
	switch {
	case err == nil:
		return &pvc, nil
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	pvc = corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: bucket.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &bucket.Spec.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(bucket.Spec.CapacityBytes, resource.BinarySI),
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(bucket, &pvc, r.Scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference on PVC %s: %w", name, err)
	}
	if err := r.Create(ctx, &pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: bucket.Namespace, Name: name}, &pvc); err != nil {
		return nil, err
	}
	return &pvc, nil
}

// matchingNFSExport finds the NFSExport bound to bucket's PVC, by reading the
// bound PersistentVolume's CSI volume handle and matching it against every
// NFSExport's spec.volumeRef. nil, nil when the PVC is not yet bound or no
// export matches it yet, both of which are waits rather than errors.
func (r *S3BucketReconciler) matchingNFSExport(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (*simplyblockv1alpha2.NFSExport, error) {
	handle, err := r.boundVolumeHandle(ctx, bucket)
	if err != nil || handle == "" {
		return nil, err
	}

	var exports simplyblockv1alpha2.NFSExportList
	if err := r.List(ctx, &exports); err != nil {
		return nil, err
	}
	for i := range exports.Items {
		if exports.Items[i].Spec.VolumeRef == handle {
			return &exports.Items[i], nil
		}
	}
	return nil, nil
}

// boundVolumeHandle reads the CSI volume handle of bucket's backing PVC's
// bound PersistentVolume, empty when the PVC is not yet bound. Shared by
// matchingNFSExport and the delete path's withdrawal, which both need the
// same identity the NFSExport's own spec.volumeRef is matched against.
func (r *S3BucketReconciler) boundVolumeHandle(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (string, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: bucket.Namespace, Name: pvcNameFor(bucket.Spec.BucketName),
	}, &pvc); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if pvc.Spec.VolumeName == "" {
		return "", nil
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if pv.Spec.CSI == nil {
		return "", nil
	}
	return pv.Spec.CSI.VolumeHandle, nil
}

// objectStoreExists reports whether spec.objectStoreRef names an ObjectStore
// in the bucket's own namespace.
func (r *S3BucketReconciler) objectStoreExists(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (bool, error) {
	var store objectstorev1alpha1.ObjectStore
	err := r.Get(ctx, client.ObjectKey{Namespace: bucket.Namespace, Name: bucket.Spec.ObjectStoreRef}, &store)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// reconcileDelete releases the PVC and drops the finalizer, on every path
// including the one where nothing was ever provisioned. Withdrawing the
// mount from every delivered node is W-12's addition, once there is a
// Registry and a recorded node set to withdraw from.
func (r *S3BucketReconciler) reconcileDelete(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(bucket, objectstorev1alpha1.S3BucketFinalizer) {
		return ctrl.Result{}, nil
	}

	if bucket.Status.Phase != phaseDeleting {
		// Best-effort and not retried on its own: the finalizer release below
		// is what must complete, and a failed status write here only costs an
		// observer the Deleting phase label, not correctness.
		_ = r.writeStatus(ctx, bucket, func(s *objectstorev1alpha1.S3BucketStatus) { s.Phase = phaseDeleting })
	}

	// Withdraw every delivered node before the PVC goes, per design §6: a
	// node told to unpublish after its volume is already gone has nothing
	// left to unpublish from. Empty until W-12 populates
	// status.mountDeliveredNodes and wires a Registry.
	for _, node := range bucket.Status.MountDeliveredNodes {
		if r.Registry == nil {
			break
		}
		if err := r.withdrawFromNode(ctx, bucket, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("withdrawing bucket from node %s: %w", node, err)
		}
	}

	if bucket.Status.PersistentVolumeClaimName != "" {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: bucket.Status.PersistentVolumeClaimName, Namespace: bucket.Namespace,
		}}
		if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting PersistentVolumeClaim %s: %w", pvc.Name, err)
		}
	}

	return ctrl.Result{}, r.releaseFinalizer(ctx, bucket)
}

// withdrawFromNode asks one node's CSI plugin to withdraw this bucket's
// mount. link.ErrNoSession is not an error here: a node that is not linked
// right now has nothing live to withdraw from either way, and the finalizer
// must still be able to complete while a node plugin is mid-rollout.
func (r *S3BucketReconciler) withdrawFromNode(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket, node string,
) error {
	conn, err := r.Registry.Conn(link.NodePeer(node))
	if errors.Is(err, link.ErrNoSession) {
		return nil
	}
	if err != nil {
		return err
	}
	handle, err := r.boundVolumeHandle(ctx, bucket)
	if err != nil {
		return err
	}
	return bucketdeliveryrpc.Remote(conn).Withdraw(ctx, bucketdeliveryrpc.Bucket{
		Name:         bucket.Spec.BucketName,
		VolumeHandle: handle,
	})
}

// releaseFinalizer drops the bucket's finalizer against a fresh copy.
func (r *S3BucketReconciler) releaseFinalizer(ctx context.Context, bucket *objectstorev1alpha1.S3Bucket) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh objectstorev1alpha1.S3Bucket
		if err := r.Get(ctx, client.ObjectKeyFromObject(bucket), &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		controllerutil.RemoveFinalizer(&fresh, objectstorev1alpha1.S3BucketFinalizer)
		return r.Update(ctx, &fresh)
	})
}

// ensureFinalizer adds the finalizer if it is absent, reporting whether it wrote.
func (r *S3BucketReconciler) ensureFinalizer(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (bool, error) {
	if controllerutil.ContainsFinalizer(bucket, objectstorev1alpha1.S3BucketFinalizer) {
		return false, nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh objectstorev1alpha1.S3Bucket
		if err := r.Get(ctx, client.ObjectKeyFromObject(bucket), &fresh); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(&fresh, objectstorev1alpha1.S3BucketFinalizer) {
			return nil
		}
		controllerutil.AddFinalizer(&fresh, objectstorev1alpha1.S3BucketFinalizer)
		return r.Update(ctx, &fresh)
	})
	return err == nil, err
}

// restore rebuilds the phase machine from status. No entry hooks run: the
// transition into the phase already happened.
func (r *S3BucketReconciler) restore(
	ctx context.Context, bucket *objectstorev1alpha1.S3Bucket,
) (*statemachine.Machine[phase], error) {
	snap := statemachine.Snapshot[phase]{State: bucket.Status.Phase}
	if bucket.Status.PhaseDeadline != nil {
		snap.Deadline = bucket.Status.PhaseDeadline.Time
	}
	return statemachine.NewFromSnapshot(ctx, s3BucketPhases(), snap)
}

// s3BucketPhases is the declared graph. An edge not written here is an error
// at the transition rather than a silent status write. Degraded has no
// exits, reached by refusing to act and left only by deleting the bucket,
// the same shape NFSExport's own Degraded already has in this manager.
func s3BucketPhases() statemachine.Config[phase] {
	return statemachine.Config[phase]{
		Initial: phasePending,
		States: map[phase]statemachine.StateDef[phase]{
			phasePending: {To: []phase{phaseProvisioning}},
			phaseProvisioning: {
				To: []phase{phaseReady, phaseDegraded},
				OnEnter: func(context.Context, phase, phase) (time.Duration, error) {
					return s3BucketProvisioningDeadline, nil
				},
			},
			phaseReady:    {To: []phase{}},
			phaseDegraded: {},
		},
	}
}

// writeStatus applies mutate to a freshly read copy, retrying on conflict.
// Re-reading is the point: a blind overwrite reverts a concurrent writer.
func (r *S3BucketReconciler) writeStatus(
	ctx context.Context,
	bucket *objectstorev1alpha1.S3Bucket,
	mutate func(*objectstorev1alpha1.S3BucketStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh objectstorev1alpha1.S3Bucket
		if err := r.Get(ctx, client.ObjectKeyFromObject(bucket), &fresh); err != nil {
			return err
		}
		mutate(&fresh.Status)
		fresh.Status.ObservedGeneration = fresh.Generation
		if err := r.Status().Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.Status.DeepCopyInto(&bucket.Status)
		return nil
	})
}

// setDeadline copies the machine's bound into status, to outlive the process.
func setDeadline(s *objectstorev1alpha1.S3BucketStatus, sm *statemachine.Machine[phase]) {
	snap := sm.Snapshot()
	if snap.Deadline.IsZero() {
		s.PhaseDeadline = nil
		return
	}
	s.PhaseDeadline = &metav1.Time{Time: snap.Deadline}
}

func (r *S3BucketReconciler) event(bucket *objectstorev1alpha1.S3Bucket, kind, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(bucket, nil, kind, reason, reason, "%s", message)
	}
}

// logr is the sliver of logr.Logger this package's reconcile helpers use,
// named locally so they do not have to import logr just to accept it.
type logr interface {
	Info(msg string, keysAndValues ...any)
}

// s3BucketConcurrency is how many buckets reconcile at once. Each one is
// independent (design §7.3), so this is a throughput knob, not a correctness
// one.
const s3BucketConcurrency = 8

func s3BucketControllerOptions() controller.Options {
	return controller.Options{MaxConcurrentReconciles: s3BucketConcurrency}
}

// SetupWithManager wires the controller. Owns the PersistentVolumeClaim it
// creates, so a manual delete of it is noticed and repaired on the bucket's
// own reconcile.
func (r *S3BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&objectstorev1alpha1.S3Bucket{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		WithOptions(s3BucketControllerOptions()).
		Complete(r)
}

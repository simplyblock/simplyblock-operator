package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create

const (
	// labelPinnedVolumePV labels a controller-created VolumeMigration with the PV
	// name it targets, so all pin-driven migrations for a PV can be found without
	// knowing the (target-dependent) object name.
	labelPinnedVolumePV = "storage.simplyblock.io/pinned-volume-pv"

	// pvcPinRequeueUnbound is how long to wait before rechecking a PVC whose
	// backing PV is not provisioned yet.
	pvcPinRequeueUnbound = 15 * time.Second
	// pvcPinRequeueMigrating is how long to wait while an earlier migration for
	// the same PV is still in flight before requesting the next one.
	pvcPinRequeueMigrating = 30 * time.Second
)

// PersistentVolumeClaimReconciler watches PVCs for changes to the
// simplyblock.io/pinned-volume annotation and, when the pinned storage node
// changes, requests a VolumeMigration to move the volume's backing logical
// volume onto that node.
//
// Change detection is a strict diff: the controller only acts when the
// pinned-volume value differs from the AnnotationPinnedVolumeApplied marker it
// writes after acting, so its own annotation writes do not re-trigger a
// migration. A validating admission webhook rejects an unknown storage node at
// write time; the re-validation here is a defense-in-depth backstop (e.g. for
// values that predate the webhook, or a node removed after the pin was set).
type PersistentVolumeClaimReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	apiClient *webapi.Client
}

func (r *PersistentVolumeClaimReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, req.NamespacedName, pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	desired := pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolume]
	applied := pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied]

	// Strict change-diff gate: nothing to do unless the pinned target changed.
	if desired == applied {
		return ctrl.Result{}, nil
	}

	// Unpinning does not move the volume; just record that there is no pending
	// target so a later re-pin is detected as a change.
	if desired == "" {
		return r.setApplied(ctx, pvc, "")
	}

	// The backing volume is resolved from the bound PV's CSI handle. An unbound
	// PVC has no volume to migrate yet — wait, do not treat it as an error.
	if pvc.Spec.VolumeName == "" {
		log.Info("PVC not bound yet; waiting before requesting pin migration", "pvc", req.NamespacedName)
		return ctrl.Result{RequeueAfter: pvcPinRequeueUnbound}, nil
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: pvcPinRequeueUnbound}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get PV %q: %w", pvc.Spec.VolumeName, err)
	}
	clusterUUID, poolUUID, volumeUUID, ok := splitCSIVolumeHandle(pv)
	if !ok {
		r.Recorder.Eventf(pvc, nil, corev1.EventTypeWarning, "NotSimplyblockVolume", "NotSimplyblockVolume",
			"PV %q is not a simplyblock CSI volume; pinned-volume annotation ignored", pv.Name)
		return ctrl.Result{}, nil
	}

	// Backstop validation: confirm the target is a real storage node.
	nodes, err := r.apiClient.GetStorageNodes(ctx, clusterUUID)
	if err != nil {
		log.Error(err, "cannot list storage nodes; requeuing", "cluster", clusterUUID)
		return ctrl.Result{RequeueAfter: pvcPinRequeueUnbound}, nil
	}
	if !containsStorageNode(nodes, desired) {
		return r.rejectTarget(ctx, pvc, desired)
	}

	// Already on the requested node — record it as applied so we stop reconciling
	// until the next change.
	vol, err := r.apiClient.GetVolume(ctx, clusterUUID, poolUUID, volumeUUID)
	if err != nil {
		log.Error(err, "cannot read volume; requeuing", "volume", volumeUUID)
		return ctrl.Result{RequeueAfter: pvcPinRequeueUnbound}, nil
	}
	if vol != nil && vol.PrimaryNodeUUID == desired {
		log.Info("Volume already on requested node; marking pin applied",
			"volume", volumeUUID, "node", desired)
		return r.setApplied(ctx, pvc, desired)
	}

	// Serialize per PV: wait for any in-flight pin migration to finish before
	// requesting another (e.g. when the target changed while one was running).
	active, err := r.hasActiveMigration(ctx, pvc.Namespace, pv.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if active {
		log.Info("A pin migration for this PV is still in flight; waiting", "pv", pv.Name)
		return ctrl.Result{RequeueAfter: pvcPinRequeueMigrating}, nil
	}

	if err := r.createMigration(ctx, pvc, pv.Name, desired); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(pvc, nil, corev1.EventTypeNormal, "MigrationRequested", "MigrationRequested",
		"Requested migration of PV %q to storage node %q", pv.Name, desired)
	return r.setApplied(ctx, pvc, desired)
}

// createMigration creates a VolumeMigration for the PV/target, owned by the PVC
// so it is garbage-collected with the claim. The name is deterministic in
// (pv, target) so a retried reconcile is idempotent (AlreadyExists is tolerated).
func (r *PersistentVolumeClaimReconciler) createMigration(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	pvName, target string,
) error {
	vm := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pinMigrationName(pvName, target),
			Namespace: pvc.Namespace,
			Labels:    map[string]string{labelPinnedVolumePV: pvName},
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         pvName,
			TargetNodeUUID: target,
		},
	}
	// A plain owner reference (not a controller reference) is enough to garbage
	// collect the VolumeMigration when the PVC is deleted. SetControllerReference
	// would additionally set blockOwnerDeletion=true, which the API server only
	// permits when the operator can update persistentvolumeclaims/finalizers —
	// a privilege it neither has nor needs here.
	if err := controllerutil.SetOwnerReference(pvc, vm, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on VolumeMigration: %w", err)
	}
	if err := r.Create(ctx, vm); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create VolumeMigration %q: %w", vm.Name, err)
	}
	return nil
}

// hasActiveMigration reports whether a non-terminal pin-driven VolumeMigration
// exists for the given PV.
func (r *PersistentVolumeClaimReconciler) hasActiveMigration(
	ctx context.Context,
	namespace, pvName string,
) (bool, error) {
	var list simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingLabels{labelPinnedVolumePV: pvName},
	); err != nil {
		return false, fmt.Errorf("list VolumeMigrations for PV %q: %w", pvName, err)
	}
	for _, vm := range list.Items {
		if !isTerminalMigrationPhase(vm.Status.Phase) {
			return true, nil
		}
	}
	return false, nil
}

// rejectTarget records a Warning for an unknown storage node and remembers the
// rejected value so the warning is not re-emitted on every reconcile. It does not
// update the applied marker, so a corrected value is still seen as a change.
func (r *PersistentVolumeClaimReconciler) rejectTarget(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	target string,
) (ctrl.Result, error) {
	if pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeRejected] == target {
		return ctrl.Result{}, nil
	}
	r.Recorder.Eventf(pvc, nil, corev1.EventTypeWarning, "InvalidPinTarget", "InvalidPinTarget",
		"pinned-volume target %q is not a known storage node; ignoring", target)
	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeRejected] = target
	if err := r.Patch(ctx, pvc, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("record rejected pin target: %w", err)
	}
	return ctrl.Result{}, nil
}

// setApplied records value as the applied pin target and clears any stale
// rejection marker, closing the change-diff for this value.
func (r *PersistentVolumeClaimReconciler) setApplied(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	value string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	if value == "" {
		delete(pvc.Annotations, simplyblockv1alpha1.AnnotationPinnedVolumeApplied)
	} else {
		pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolumeApplied] = value
	}
	delete(pvc.Annotations, simplyblockv1alpha1.AnnotationPinnedVolumeRejected)
	if err := r.Patch(ctx, pvc, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("record applied pin target: %w", err)
	}
	return ctrl.Result{}, nil
}

// splitCSIVolumeHandle parses a simplyblock CSI volume handle
// ("<clusterUUID>:<poolUUID>:<volumeUUID>") from a PV. ok is false when the PV is
// not a simplyblock CSI volume or the handle is malformed.
func splitCSIVolumeHandle(pv *corev1.PersistentVolume) (clusterUUID, poolUUID, volumeUUID string, ok bool) {
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return "", "", "", false
	}
	parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func containsStorageNode(nodes []webapi.StorageNodeInfo, uuid string) bool {
	for _, n := range nodes {
		if n.UUID == uuid {
			return true
		}
	}
	return false
}

func isTerminalMigrationPhase(phase simplyblockv1alpha1.VolumeMigrationPhase) bool {
	switch phase {
	case simplyblockv1alpha1.VolumeMigrationPhaseCompleted,
		simplyblockv1alpha1.VolumeMigrationPhaseFailed,
		simplyblockv1alpha1.VolumeMigrationPhaseAborted:
		return true
	default:
		return false
	}
}

// pinMigrationName is a deterministic, DNS-label-safe VolumeMigration name for a
// (PV, target) pair. Deterministic so a retried reconcile hits AlreadyExists
// instead of creating duplicates; target-dependent so a new target yields a new
// object rather than colliding with a finished migration to the old target.
func pinMigrationName(pvName, target string) string {
	sum := sha256.Sum256([]byte(pvName + "\x00" + target))
	return "pvc-pin-" + hex.EncodeToString(sum[:])[:16]
}

// pinnedVolumeChanged returns true only when the pinned-volume annotation is
// present on create or its value changes on update, so the controller is not
// woken by unrelated PVC edits (including its own applied/rejected writes).
var pinnedVolumeChanged = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		return e.Object.GetAnnotations()[simplyblockv1alpha1.AnnotationPinnedVolume] != ""
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetAnnotations()[simplyblockv1alpha1.AnnotationPinnedVolume] !=
			e.ObjectNew.GetAnnotations()[simplyblockv1alpha1.AnnotationPinnedVolume]
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

func (r *PersistentVolumeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.apiClient = webapi.NewClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(pinnedVolumeChanged)).
		Named("pinnedvolume").
		Complete(r)
}

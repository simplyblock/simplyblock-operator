package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/simplyblock/atlas/kube"
	atlaslvol "github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create

const (
	// labelPinnedVolumePV labels a controller-created VolumeMigration with a
	// hash of the PV name (see pinPVLabelValue), so all pin-driven migrations for
	// a PV can be found without knowing the (target-dependent) object name. The
	// value is hashed because PV names can exceed the 63-char label-value limit;
	// the full PV name is preserved in VolumeMigration.spec.pvName.
	labelPinnedVolumePV = "storage.simplyblock.io/pinned-volume-pv"

	// pvcPinRequeueUnbound is how long to wait before rechecking a PVC whose
	// backing PV is not provisioned yet.
	pvcPinRequeueUnbound = 15 * time.Second
	// pvcPinRequeueMigrating is how long to wait while an earlier migration for
	// the same PV is still in flight before requesting the next one.
	pvcPinRequeueMigrating = 30 * time.Second
)

// PersistentVolumeClaimReconciler watches PVCs for changes to the pin annotation
// (kube.PinnedNode: the canonical simplyblock.io/selected-storage-node, or a
// legacy host-id fallback) and, when the pinned storage node changes, requests a
// VolumeMigration to move the volume's backing logical volume onto that node.
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

	// Mover raises a pinned volume's move as whichever kind this deployment
	// runs. Unset means the kind this API group documents.
	Mover volumemigration.Mover
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

	// desired is the pin target, honoring the canonical selected-storage-node and,
	// for pre-existing PVCs, the legacy host-id annotations (see kube.PinnedNode).
	// setApplied normalizes those legacy annotations into selected-storage-node
	// when it records the applied target, so the annotation state converges.
	desired := kube.PinnedNode(pvc.Annotations)
	applied := pvc.Annotations[kube.AnnoSelectedStorageNodeApplied]

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
	clusterUUID, poolUUID, volumeUUID, ok := csiVolumeHandleParts(pv)
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

	// The VolumeMigration must be created in the StorageCluster CR's namespace:
	// the VolumeMigration reconciler resolves the cluster's migration settings
	// (rebalancer image, enabled flag) within the migration's own namespace, and
	// PVCs may live in a different namespace than the cluster.
	cluster, err := r.resolveClusterCR(ctx, clusterUUID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("No StorageCluster resource manages this cluster; cannot migrate",
			"cluster", clusterUUID, "pv", pv.Name)
		r.Recorder.Eventf(pvc, nil, corev1.EventTypeWarning, "NoStorageCluster", "NoStorageCluster",
			"no StorageCluster resource manages cluster %q; cannot migrate PV %q", clusterUUID, pv.Name)
		return ctrl.Result{RequeueAfter: pvcPinRequeueUnbound}, nil
	}

	// Serialize per PV: wait for any in-flight pin migration to finish before
	// requesting another (e.g. when the target changed while one was running).
	active, err := r.hasActiveMigration(ctx, cluster.Namespace, pv.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if active {
		log.Info("A pin migration for this PV is still in flight; waiting", "pv", pv.Name)
		return ctrl.Result{RequeueAfter: pvcPinRequeueMigrating}, nil
	}

	if err := r.createMigration(ctx, cluster, pv.Name, desired); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(pvc, nil, corev1.EventTypeNormal, "MigrationRequested", "MigrationRequested",
		"Requested migration of PV %q to storage node %q", pv.Name, desired)
	return r.setApplied(ctx, pvc, desired)
}

// createMigration raises the move of a pinned volume onto its requested node,
// as whichever kind this deployment runs.
//
// The name is deterministic in (volume, target) so a retried reconcile is
// idempotent, and the move is raised in the owning StorageCluster's namespace:
// a claim may live in another namespace than its cluster, and a cross-namespace
// owner reference is invalid.
func (r *PersistentVolumeClaimReconciler) createMigration(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	pvName, target string,
) error {
	return r.mover().Start(ctx, volumemigration.MoveRequest{
		Name:           pinMigrationName(pvName, target),
		Namespace:      cluster.Namespace,
		PVName:         pvName,
		TargetNodeUUID: target,
		Labels:         map[string]string{labelPinnedVolumePV: pinPVLabelValue(pvName)},
		Owner:          cluster,
		OwnerKind:      "StorageCluster",
		Scheme:         r.Scheme,
	})
}

// mover is this controller's channel for raising a move, defaulted so a
// reconciler built without one raises the kind this API group documents.
func (r *PersistentVolumeClaimReconciler) mover() volumemigration.Mover {
	if r.Mover != nil {
		return r.Mover
	}
	return volumemigration.NewMover(r.Client, r.Scheme, false)
}

// hasActiveMigration reports whether a non-terminal pin-driven move exists for
// the given volume.
func (r *PersistentVolumeClaimReconciler) hasActiveMigration(
	ctx context.Context,
	namespace, pvName string,
) (bool, error) {
	moves, err := r.mover().List(ctx, namespace,
		map[string]string{labelPinnedVolumePV: pinPVLabelValue(pvName)})
	if err != nil {
		return false, fmt.Errorf("list the moves of PV %q: %w", pvName, err)
	}
	for _, move := range moves {
		if !move.Phase.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

// resolveClusterCR returns the StorageCluster whose reported UUID matches
// clusterUUID, searching all namespaces. The VolumeMigration must be created in
// this CR's namespace, since the VolumeMigration reconciler resolves the
// cluster's migration settings there. Returns (nil, nil) when no CR matches.
func (r *PersistentVolumeClaimReconciler) resolveClusterCR(
	ctx context.Context,
	clusterUUID string,
) (*simplyblockv1alpha2.StorageCluster, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return nil, fmt.Errorf("list StorageClusters: %w", err)
	}
	for i := range clusters.Items {
		if clusters.Items[i].Status.UUID == clusterUUID {
			return &clusters.Items[i], nil
		}
	}
	return nil, nil
}

// rejectTarget records a Warning for an unknown storage node and remembers the
// rejected value so the warning is not re-emitted on every reconcile. It does not
// update the applied marker, so a corrected value is still seen as a change.
func (r *PersistentVolumeClaimReconciler) rejectTarget(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	target string,
) (ctrl.Result, error) {
	if pvc.Annotations[kube.AnnoSelectedStorageNodeRejected] == target {
		return ctrl.Result{}, nil
	}
	r.Recorder.Eventf(pvc, nil, corev1.EventTypeWarning, "InvalidPinTarget", "InvalidPinTarget",
		"pinned-volume target %q is not a known storage node; ignoring", target)
	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[kube.AnnoSelectedStorageNodeRejected] = target
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
		delete(pvc.Annotations, kube.AnnoSelectedStorageNodeApplied)
	} else {
		pvc.Annotations[kube.AnnoSelectedStorageNodeApplied] = value
		// Normalize legacy pin annotations into the canonical one so the state
		// converges: a pre-existing host-id pin (which drove this reconcile via
		// kube.PinnedNode) is rewritten to selected-storage-node and the legacy
		// forms are dropped.
		pvc.Annotations[kube.AnnoSelectedStorageNode] = value
		delete(pvc.Annotations, kube.AnnoHostID)
		delete(pvc.Annotations, kube.DeprecatedAnnoHostID)
	}
	delete(pvc.Annotations, kube.AnnoSelectedStorageNodeRejected)
	if err := r.Patch(ctx, pvc, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("record applied pin target: %w", err)
	}
	return ctrl.Result{}, nil
}

// csiVolumeHandleParts returns the parts of a PV's simplyblock CSI volume
// handle through the atlas helpers, so the handle grammar lives in one place.
// ok is false when the PV is not a simplyblock CSI volume or the handle is
// malformed.
func csiVolumeHandleParts(pv *corev1.PersistentVolume) (clusterUUID, poolRef, volumeUUID string, ok bool) {
	raw, err := kube.VolumeHandleFromPV(pv)
	if err != nil {
		return "", "", "", false
	}
	h, parsed := atlaslvol.ParseHandle(raw)
	if !parsed {
		return "", "", "", false
	}
	return h.ClusterID, h.PoolRef, h.VolumeID, true
}

func containsStorageNode(nodes []webapi.StorageNodeInfo, uuid string) bool {
	for _, n := range nodes {
		if n.UUID == uuid {
			return true
		}
	}
	return false
}

// pinMigrationName is a deterministic, DNS-label-safe VolumeMigration name for a
// (PV, target) pair. Deterministic so a retried reconcile hits AlreadyExists
// instead of creating duplicates; target-dependent so a new target yields a new
// object rather than colliding with a finished migration to the old target.
func pinMigrationName(pvName, target string) string {
	sum := sha256.Sum256([]byte(pvName + "\x00" + target))
	return "pvc-pin-" + hex.EncodeToString(sum[:])[:16]
}

// pinPVLabelValue derives a label-safe value from a PV name for labelPinnedVolumePV.
// PV names can exceed the 63-character label-value limit, so the name is hashed to
// a fixed-length hex string; the full PV name remains in VolumeMigration.spec.pvName.
// createMigration and hasActiveMigration must use this same derivation.
func pinPVLabelValue(pvName string) string {
	sum := sha256.Sum256([]byte(pvName))
	return hex.EncodeToString(sum[:])[:32]
}

// pinnedVolumeChanged returns true only when the pinned-volume annotation is
// present on create or its value changes on update, so the controller is not
// woken by unrelated PVC edits (including its own applied/rejected writes).
var pinnedVolumeChanged = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		return kube.IsPinnedVolume(e.Object.GetAnnotations())
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		// Compare the effective pin target (canonical or legacy) so pre-existing
		// host-id PVCs wake the controller, while its own normalization writes —
		// which keep the pin target unchanged — do not.
		return kube.PinnedNode(e.ObjectOld.GetAnnotations()) !=
			kube.PinnedNode(e.ObjectNew.GetAnnotations())
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

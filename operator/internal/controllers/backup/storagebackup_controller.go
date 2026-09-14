// The StorageBackup mirror: it turns what a cluster's store holds into one
// Kubernetes object per backup. Nothing declares a backup, so this reconciler
// owns the whole creation path as well as the update and delete ones, which is
// what makes it different from the reconcilers that converge a user's spec.
//
// It is the same shape as the StorageDevice mirror next door and for the same
// reason, and the one place the two differ is worth stating: a device belongs to
// a node that can be asked about it, while a backup belongs to a bucket that
// reports through the control plane and nothing else. So an absence here is
// decided by whether the cluster's stream has synced, and by nothing about any
// other object.

package backup

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// PersistentVolumeLvolIDIndex indexes PersistentVolume objects by the logical
// volume their CSI handle names.
//
// The mirror needs it because the control plane reports which volume a backup
// was taken from and nothing about the claim that volume backs, and there is one
// object per backup: resolving the claim by listing every volume in the cluster
// would be work proportional to backups times volumes on every reconcile.
const PersistentVolumeLvolIDIndex = "spec.csi.volumeHandle.lvolID"

// IndexPersistentVolumeLvolID is the index function for
// [PersistentVolumeLvolIDIndex], exported so that a test's client can register
// the same index the manager does. A volume this product did not provision, and
// one whose handle is malformed, are not indexed: neither can be the source of a
// simplyblock backup.
func IndexPersistentVolumeLvolID(o client.Object) []string {
	pv, ok := o.(*corev1.PersistentVolume)
	if !ok || pv.Spec.CSI == nil {
		return nil
	}
	handle, wellFormed := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !wellFormed {
		return nil
	}
	return []string{handle.VolumeID}
}

// backupRetry is how long the mirror waits before looking again at something
// that is not wrong, only not ready: a cluster object that has not appeared yet,
// or a scope whose first snapshot is still in flight.
const backupRetry = time.Second

// Control-plane backup statuses, in the control plane's own spelling. They are
// listed here rather than shared with the subscription because grouping them
// into phases is this file's whole job.
const (
	cpBackupPending    = "pending"
	cpBackupInProgress = "in_progress"
	cpBackupCompleted  = "completed"
	cpBackupFailed     = "failed"
	cpBackupMerging    = "merging"
	cpBackupDeleting   = "deleting"
)

// BackupCache is the read surface the reconciler needs from the backup
// subscription: a trigger stream (each event naming a StorageBackup object), a
// by-object-name lookup of desired state, and a per-scope synced check. It keeps
// the reconciler independent of how backups are retrieved and cached.
type BackupCache interface {
	// Triggers is the reconcile-trigger stream; each event names a StorageBackup.
	Triggers() <-chan event.GenericEvent
	// Lookup returns the backup the named object mirrors and its scope, or
	// ok=false if the control plane no longer reports it.
	Lookup(key types.NamespacedName) (cpinformer.Scope, subscriptions.BackupDTO, bool)
	// Synced reports whether a scope's initial snapshot has been applied.
	Synced(scope cpinformer.Scope) bool
}

// StorageBackupReconciler mirrors a cluster's store into StorageBackup objects.
// It is triggered per-object — by the subscription (a backup changed) and by the
// object itself (drift and startup) — reads desired state from the cache rather
// than from the control-plane API, and writes through a workqueue, so the stream
// is unaffected by API latency or write failures.
type StorageBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Backups  BackupCache
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes;persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager watches StorageBackup objects (for drift and to enumerate
// stale ones at startup) and the subscription's trigger stream (for
// control-plane changes). Both enqueue a StorageBackup to reconcile.
func (r *StorageBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &corev1.PersistentVolume{},
		PersistentVolumeLvolIDIndex, IndexPersistentVolumeLvolID,
	); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageBackup{}).
		Named("storagebackup").
		WatchesRawSource(source.Channel(r.Backups.Triggers(), &handler.EnqueueRequestForObject{})).
		Complete(r)
}

// Reconcile converges one StorageBackup toward the cache's view of the copy the
// store holds: create or update while the backup is reported, delete once it is
// not. An object whose backup is absent from a not-yet-synced scope is left
// alone, because a cold cache is an absence of information rather than
// information.
func (r *StorageBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	scope, dto, inCache := r.Backups.Lookup(req.NamespacedName)

	var sb simplyblockv1alpha2.StorageBackup
	err := r.Get(ctx, req.NamespacedName, &sb)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	switch {
	case inCache && len(scope) == 1:
		return r.upsert(ctx, req.NamespacedName, scope, dto)

	case exists:
		return r.unreported(ctx, &sb)

	default:
		return ctrl.Result{}, nil // nothing cached and no object — nothing to do
	}
}

// upsert creates the object if it is missing and brings its status to what the
// control plane reports.
func (r *StorageBackupReconciler) upsert(
	ctx context.Context,
	key types.NamespacedName,
	scope cpinformer.Scope,
	dto subscriptions.BackupDTO,
) (ctrl.Result, error) {
	cluster, err := r.clusterFor(ctx, key.Namespace, scope[0])
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		// The stream named a cluster this namespace has no object for. That is
		// a race at startup rather than an error, and the next trigger or the
		// requeue resolves it.
		return ctrl.Result{RequeueAfter: backupRetry}, nil
	}

	taken, err := r.resolveSource(ctx, scope[0], dto)
	if err != nil {
		return ctrl.Result{}, err
	}

	created, err := r.ensureObject(ctx, key, cluster, dto, taken)
	if err != nil {
		return ctrl.Result{}, err
	}
	if created {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal,
			ReasonBackupDiscovered, ReasonBackupDiscovered,
			"Backup %s was found in the store and recorded as StorageBackup %s", dto.ID, key.Name)
	}

	return ctrl.Result{}, r.writeStatus(ctx, key, cluster, scope, dto, taken)
}

// ensureObject creates the StorageBackup when it is missing, and reports whether
// it did. The spec is identity and never changes, so an existing object is left
// alone apart from its labels, which follow the claim the copy came from and can
// move when a claim is deleted and the volume is rebound.
func (r *StorageBackupReconciler) ensureObject(
	ctx context.Context,
	key types.NamespacedName,
	cluster *simplyblockv1alpha1.StorageCluster,
	dto subscriptions.BackupDTO,
	taken simplyblockv1alpha2.BackupSource,
) (bool, error) {
	var existing simplyblockv1alpha2.StorageBackup
	err := r.Get(ctx, key, &existing)
	if err == nil {
		return false, r.reconcileLabels(ctx, &existing, cluster, taken)
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}

	sb := &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    backupLabels(cluster, taken),
		},
		Spec: simplyblockv1alpha2.StorageBackupSpec{
			ClusterRef: cluster.Name,
			BackupID:   dto.ID,
		},
	}
	// No owner reference. §13 wants a policy to own the backups taken under it,
	// and the control plane reports nothing about which policy took a copy, so
	// the edge cannot be built from what is on the wire. Ownership would in any
	// case be the wrong lifetime here: the object goes when the store stops
	// reporting the copy, which is what unreported below does.
	if err := r.Create(ctx, sb); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// reconcileLabels brings an existing object's labels to what the source now
// resolves to. A backup outlives the claim it was taken from, so the claim label
// is dropped rather than kept stale once the claim is gone: a selector that
// still matched would name a claim nobody can restore into.
func (r *StorageBackupReconciler) reconcileLabels(
	ctx context.Context,
	sb *simplyblockv1alpha2.StorageBackup,
	cluster *simplyblockv1alpha1.StorageCluster,
	taken simplyblockv1alpha2.BackupSource,
) error {
	want := backupLabels(cluster, taken)
	if labelsMatch(sb.Labels, want) {
		return nil
	}
	patch := client.MergeFrom(sb.DeepCopy())
	if sb.Labels == nil {
		sb.Labels = map[string]string{}
	}
	for _, key := range []string{
		simplyblockv1alpha2.BackupLabelCluster,
		simplyblockv1alpha2.BackupLabelClaim,
	} {
		if value, ok := want[key]; ok {
			sb.Labels[key] = value
		} else {
			delete(sb.Labels, key)
		}
	}
	return r.Patch(ctx, sb, patch)
}

// backupLabels are the labels a backup object carries, which is how it is found
// by the thing somebody knows: the cluster, and the claim whose data is in it.
func backupLabels(
	cluster *simplyblockv1alpha1.StorageCluster, taken simplyblockv1alpha2.BackupSource,
) map[string]string {
	labels := map[string]string{simplyblockv1alpha2.BackupLabelCluster: cluster.Name}
	if taken.ClaimName != "" {
		labels[simplyblockv1alpha2.BackupLabelClaim] = taken.ClaimName
	}
	return labels
}

// labelsMatch reports whether the object already carries exactly the keys this
// mirror owns, with the values it wants. Keys it does not own are ignored, so a
// label somebody else put on the object is not fought over.
func labelsMatch(have, want map[string]string) bool {
	for _, key := range []string{
		simplyblockv1alpha2.BackupLabelCluster,
		simplyblockv1alpha2.BackupLabelClaim,
	} {
		wanted, wantIt := want[key]
		got, haveIt := have[key]
		if wantIt != haveIt || wanted != got {
			return false
		}
	}
	return true
}

// writeStatus brings the object's status to what the control plane reports, and
// emits the events and metrics a phase change owes.
//
// The status is rewritten from the cache on every pass rather than patched
// field by field, because the cache is the whole of what is known: there is no
// state here the operator computed and could lose.
func (r *StorageBackupReconciler) writeStatus(
	ctx context.Context,
	key types.NamespacedName,
	cluster *simplyblockv1alpha1.StorageCluster,
	scope cpinformer.Scope,
	dto subscriptions.BackupDTO,
	taken simplyblockv1alpha2.BackupSource,
) error {
	phase := backupPhaseFor(dto.Status)

	var previous simplyblockv1alpha2.StorageBackupPhase
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var sb simplyblockv1alpha2.StorageBackup
		if err := r.Get(ctx, key, &sb); err != nil {
			return err
		}
		previous = sb.Status.Phase

		desired := sb.Status
		desired.Phase = phase
		desired.APIStatus = dto.Status
		desired.ClusterID = scope[0]
		desired.Backup = backupCopyFrom(dto)
		desired.Message = backupMessageFor(dto.Status)
		desired.ObservedGeneration = sb.Generation
		// The source group is written once and never updated. The pool a volume
		// was in when it was backed up is a fact about the backup, and rewriting
		// it when the volume moves would destroy the only record of where the
		// data came from, which is what a restore reads.
		if desired.Source == nil {
			desired.Source = &taken
		}

		if reflect.DeepEqual(sb.Status, desired) {
			return nil
		}
		patch := client.MergeFromWithOptions(sb.DeepCopy(), client.MergeFromWithOptimisticLock{})
		sb.Status = desired
		return r.Status().Patch(ctx, &sb, patch)
	})
	if err != nil {
		return err
	}

	r.announce(ctx, key, cluster, previous, phase, dto)
	return nil
}

// announce emits the event and the metrics a phase change owes, and nothing at
// all when the phase did not move. A backup reaching a terminal phase is counted
// once, which is what makes the counter a rate rather than a poll.
func (r *StorageBackupReconciler) announce(
	ctx context.Context,
	key types.NamespacedName,
	cluster *simplyblockv1alpha1.StorageCluster,
	previous, current simplyblockv1alpha2.StorageBackupPhase,
	dto subscriptions.BackupDTO,
) {
	if previous == current {
		return
	}

	var sb simplyblockv1alpha2.StorageBackup
	if err := r.Get(ctx, key, &sb); err != nil {
		// The object went while this pass ran. There is nothing to attach an
		// event to, and the next reconcile decides what that means.
		logf.FromContext(ctx).V(1).Info("backup object went before its event could be written",
			"backup", key.String())
		return
	}
	policy := sb.Labels[simplyblockv1alpha2.BackupLabelPolicy]

	switch current {
	case simplyblockv1alpha2.StorageBackupPhaseAvailable:
		r.Recorder.Eventf(&sb, nil, corev1.EventTypeNormal,
			ReasonBackupAvailable, ReasonBackupAvailable,
			"Backup %s completed and is restorable", dto.ID)
		backupCompletionsTotal.WithLabelValues(cluster.Name, policy, "succeeded").Inc()
		observeBackupCost(cluster.Name, policy, dto)

	case simplyblockv1alpha2.StorageBackupPhaseFailed:
		r.Recorder.Eventf(&sb, nil, corev1.EventTypeWarning,
			ReasonBackupFailed, ReasonBackupFailed,
			"Backup %s failed in the control plane", dto.ID)
		backupCompletionsTotal.WithLabelValues(cluster.Name, policy, "failed").Inc()
	}
}

// observeBackupCost records what the copy took and what it costs to keep. Both
// come from the backup's own reported numbers rather than from what the operator
// watched, because the stream coalesces and a small backup can be complete
// before the operator ever saw it start.
func observeBackupCost(clusterName, policy string, dto subscriptions.BackupDTO) {
	if dto.Size > 0 {
		backupSizeBytes.WithLabelValues(clusterName, policy).Observe(float64(dto.Size))
	}
	if dto.CreatedAt > 0 && dto.CompletedAt > dto.CreatedAt {
		backupDurationSeconds.WithLabelValues(clusterName, policy).
			Observe(float64(dto.CompletedAt - dto.CreatedAt))
	}
}

// unreported handles an object whose backup the control plane no longer reports.
//
// Whether that is information depends on whether anything has been said at all.
// A synced scope that does not mention a backup is a backup that has left the
// store, which is the ordinary end of a copy's life: retention pruned it, or
// somebody emptied the bucket. A scope that has not synced has said nothing, and
// deleting on that would empty the inventory every time the operator restarted.
//
// A pruned backup is expected, so its object is deleted silently rather than
// marked Failed. The mirror distinguishes a backup that is gone because
// retention removed it from one that is gone because something went wrong, and
// only the second is worth an event on the backup itself.
func (r *StorageBackupReconciler) unreported(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
) (ctrl.Result, error) {
	// An object the mirror has never written carries no backend cluster, and the
	// scope it would be judged against has to come from its spec instead. That is
	// how a record written by hand is reconciled away rather than left forever:
	// the write guard fails open, deliberately, so a webhook outage cannot
	// deadlock a namespace teardown, and this is what makes that trade safe.
	clusterID := sb.Status.ClusterID
	if clusterID == "" {
		resolved, err := r.clusterIDFor(ctx, sb)
		if err != nil {
			return ctrl.Result{}, err
		}
		if resolved == "" {
			// The spec names a cluster this namespace does not have, so nothing
			// can ever report the backup and the record describes nothing.
			return ctrl.Result{}, r.discardUnbacked(ctx, sb)
		}
		clusterID = resolved
	}

	if !r.Backups.Synced(cpinformer.Scope{clusterID}) {
		return ctrl.Result{RequeueAfter: backupRetry}, nil
	}

	if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	cluster, err := r.clusterFor(ctx, sb.Namespace, clusterID)
	if err != nil || cluster == nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, ReasonBackupGone, ReasonBackupGone,
		"Backup %s left the store, so StorageBackup %s was removed", sb.Spec.BackupID, sb.Name)
	return ctrl.Result{}, nil
}

// clusterIDFor is the backend cluster a backup object's spec names, and the
// empty string when this namespace has no such cluster or it has no id yet.
func (r *StorageBackupReconciler) clusterIDFor(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
) (string, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(sb.Namespace)); err != nil {
		return "", err
	}
	for i := range clusters.Items {
		if clusters.Items[i].Name == sb.Spec.ClusterRef {
			return clusters.Items[i].Status.UUID, nil
		}
	}
	return "", nil
}

// discardUnbacked removes a record naming a cluster that does not exist.
//
// Only a hand-written object reaches this. The mirror names the cluster it
// discovered the backup on, so an object it created always resolves, and one
// that does not is a record of a copy no store in this namespace can hold.
func (r *StorageBackupReconciler) discardUnbacked(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
) error {
	logf.FromContext(ctx).Info("removing a backup record naming a cluster that does not exist",
		"backup", sb.Name, "clusterRef", sb.Spec.ClusterRef)
	if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// clusterFor returns the StorageCluster in this namespace whose backend id is
// the one given, or nil when the namespace has none.
func (r *StorageBackupReconciler) clusterFor(
	ctx context.Context, namespace, clusterID string,
) (*simplyblockv1alpha1.StorageCluster, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range clusters.Items {
		if clusters.Items[i].Status.UUID == clusterID {
			return &clusters.Items[i], nil
		}
	}
	return nil, nil
}

// resolveSource builds status.source from what the control plane reports about
// the backup and what Kubernetes knows about the volume it names.
//
// The split is not a choice. The control plane reports the logical volume, the
// snapshot, and the node, and nothing about the claim, the pool name, or the
// filesystem — which of those the store itself could answer is
// design-storagebackup.md §14 Q3. Until that is settled, the PersistentVolume
// carrying the volume's handle is where the Kubernetes half comes from, and a
// backup whose volume is gone simply has less source recorded rather than none.
func (r *StorageBackupReconciler) resolveSource(
	ctx context.Context, clusterID string, dto subscriptions.BackupDTO,
) (simplyblockv1alpha2.BackupSource, error) {
	taken := simplyblockv1alpha2.BackupSource{
		LvolID:       dto.LvolID,
		LvolName:     dto.LvolName,
		SnapshotID:   dto.SnapshotID,
		SnapshotName: dto.SnapshotName,
		NodeID:       dto.NodeID,
		ClusterUUID:  firstNonEmpty(dto.SourceClusterID, clusterID),
	}
	if dto.LvolID == "" {
		return taken, nil
	}

	var volumes corev1.PersistentVolumeList
	if err := r.List(ctx, &volumes,
		client.MatchingFields{PersistentVolumeLvolIDIndex: dto.LvolID}); err != nil {
		return taken, err
	}
	if len(volumes.Items) == 0 {
		return taken, nil
	}

	pv := &volumes.Items[0]
	taken.PersistentVolumeName = pv.Name
	taken.FSType = pv.Spec.CSI.FSType
	if handle, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle)); ok {
		// The pool segment is a UUID on a volume provisioned since the v2 API
		// and a name on an older one, and both occur indefinitely because a
		// PersistentVolume outlives every driver upgrade. Recording it in
		// whichever field it actually is keeps a restore from resolving a name
		// as an id.
		if lvol.IsCanonicalUUID(handle.PoolRef) {
			taken.PoolUUID = handle.PoolRef
		} else {
			taken.PoolName = handle.PoolRef
		}
	}

	// The claim is read through the volume's claimRef rather than by listing
	// claims, and only when it is in this namespace: a backup object belongs
	// beside its cluster, and naming a claim in another namespace would be a
	// label nothing in this namespace can resolve.
	if ref := pv.Spec.ClaimRef; ref != nil {
		taken.ClaimName = ref.Name
		taken.ClaimNamespace = ref.Namespace
	}
	return taken, nil
}

// backupCopyFrom is the status.backup group, built from what the control plane
// reports about the copy.
func backupCopyFrom(dto subscriptions.BackupDTO) *simplyblockv1alpha2.BackupCopy {
	copied := &simplyblockv1alpha2.BackupCopy{
		BackupID:         dto.ID,
		S3ID:             dto.S3ID,
		PreviousBackupID: dto.PrevBackupID,
		StartedAt:        unixToTime(dto.CreatedAt),
		CompletedAt:      unixToTime(dto.CompletedAt),
	}
	// Zero is a size the control plane reports for a copy that has transferred
	// nothing yet, and the field is a pointer so that it is distinguishable from
	// unset.
	if dto.Size != 0 {
		copied.Size = ptr.To(dto.Size)
	}
	return copied
}

// backupPhaseFor groups the control plane's own lifecycle strings into the four
// values the kind declares. The string itself is kept verbatim in
// status.apiStatus, so nothing is lost by the grouping.
//
// Merging and deleting are folded into Creating rather than given values of
// their own, because both describe a copy that is being rewritten and is
// therefore not stably restorable. An unrecognized status is Pending, which says
// the operator does not know rather than claiming the copy is usable.
func backupPhaseFor(status string) simplyblockv1alpha2.StorageBackupPhase {
	switch status {
	case cpBackupCompleted:
		return simplyblockv1alpha2.StorageBackupPhaseAvailable
	case cpBackupFailed:
		return simplyblockv1alpha2.StorageBackupPhaseFailed
	case cpBackupInProgress, cpBackupMerging, cpBackupDeleting:
		return simplyblockv1alpha2.StorageBackupPhaseCreating
	case cpBackupPending:
		return simplyblockv1alpha2.StorageBackupPhasePending
	default:
		return simplyblockv1alpha2.StorageBackupPhasePending
	}
}

// backupMessageFor is the one sentence status.message carries. It names the
// control plane's own string, because that is the thing a reader would otherwise
// have to go and look up.
func backupMessageFor(status string) string {
	switch status {
	case cpBackupCompleted:
		return "The copy is complete and can be restored"
	case cpBackupFailed:
		return "The control plane reported the backup as failed"
	case cpBackupMerging:
		return "The control plane is merging this copy into its chain"
	case cpBackupDeleting:
		return "The control plane is removing this copy"
	default:
		return fmt.Sprintf("The control plane reports the backup as %q", status)
	}
}

// unixToTime converts a control-plane timestamp, reading a non-positive value as
// no timestamp at all rather than as an instant in 1970.
func unixToTime(seconds int64) *metav1.Time {
	if seconds <= 0 {
		return nil
	}
	t := metav1.NewTime(time.Unix(seconds, 0).UTC())
	return &t
}

// firstNonEmpty is the first of its arguments that is set, or the empty string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

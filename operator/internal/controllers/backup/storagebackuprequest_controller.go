// The StorageBackup request reconciler: it takes the backup a StorageBackup with
// spec.source asks for. The mirror owns the other kind, a record of a copy the
// store holds, and records this copy too once the store reports it.
//
// It writes to the control plane and reads from its streams. What the snapshot
// and the backup look like comes from the subscriptions' caches, and a change in
// either wakes the request through the snapshot's name, so nothing polls the
// control plane's API.

package backup

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/lvol"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	requestRetry = 10 * time.Second // a prerequisite is missing
	// requestResync is the net under a missed stream event, not the way a request
	// advances: the streams wake it when the snapshot or the copy changes.
	requestResync = 5 * time.Minute
)

// The snapshot a request takes is named from its UID, which is what ties a
// stream event back to the request.
const (
	// RequestSnapshotIndex indexes requests by the name of the snapshot they take.
	RequestSnapshotIndex = "spec.source.snapshotName"

	// requestSnapshotPrefix starts the snapshot's name, which ends in the
	// request's UID so that a retried pass derives the same name.
	requestSnapshotPrefix = "sbk-"
)

// IndexRequestSnapshot is the index function for [RequestSnapshotIndex],
// exported so that a test's client can register what the manager does.
func IndexRequestSnapshot(o client.Object) []string {
	sb, ok := o.(*simplyblockv1alpha2.StorageBackup)
	if !ok || sb.Spec.Source == nil {
		return nil
	}
	return []string{requestSnapshotPrefix + string(sb.UID)}
}

// BackupRequestClient is the control-plane surface a request writes through.
// What it reads comes from the streams.
type BackupRequestClient interface {
	CreateSnapshot(ctx context.Context, clusterID, poolID, volumeID, name string) (string, error)
	CreateBackup(ctx context.Context, clusterID, snapshotID string) error
}

// RequestSnapshots is the read surface a request needs from the snapshot
// subscription.
type RequestSnapshots interface {
	// FindByName returns the snapshot of that name in one pool's scope.
	FindByName(scope cpinformer.Scope, name string) (subscriptions.SnapshotDTO, bool)
	// Synced reports whether a scope's first frame has been applied.
	Synced(scope cpinformer.Scope) bool
	// Triggers carries the names of snapshots that changed.
	Triggers() <-chan event.GenericEvent
}

// RequestBackups is the read surface a request needs from the backup
// subscription.
type RequestBackups interface {
	// FindBySnapshot returns the backup of a snapshot in one cluster's scope.
	FindBySnapshot(clusterID, snapshotID string) (subscriptions.BackupDTO, bool)
	// Synced reports whether a scope's first frame has been applied.
	Synced(scope cpinformer.Scope) bool
	// SnapshotTriggers carries the snapshot names of backups that changed.
	SnapshotTriggers() <-chan event.GenericEvent
}

// StorageBackupRequestReconciler snapshots the claim's volume, asks the control
// plane to back the snapshot up, and follows the copy. Each call is preceded by
// a read that shows whether it already happened, so a retry repeats nothing.
type StorageBackupRequestReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	API       BackupRequestClient
	Snapshots RequestSnapshots
	Backups   RequestBackups
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes;persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *StorageBackupRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	isRequest := predicate.NewPredicateFuncs(func(o client.Object) bool {
		sb, ok := o.(*simplyblockv1alpha2.StorageBackup)
		return ok && sb.Spec.Source != nil
	})
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &simplyblockv1alpha2.StorageBackup{},
		RequestSnapshotIndex, IndexRequestSnapshot,
	); err != nil {
		return err
	}
	wake := handler.EnqueueRequestsFromMapFunc(r.requestsForSnapshot)
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageBackup{}, builder.WithPredicates(isRequest)).
		Named("storagebackuprequest").
		WatchesRawSource(source.Channel(r.Snapshots.Triggers(), wake)).
		WatchesRawSource(source.Channel(r.Backups.SnapshotTriggers(), wake)).
		Complete(r)
}

// requestsForSnapshot maps a stream event, which carries a snapshot's name, to
// the request that took the snapshot. It reads the manager's cache, so it makes
// no API call.
func (r *StorageBackupRequestReconciler) requestsForSnapshot(
	ctx context.Context, o client.Object,
) []reconcile.Request {
	var requests simplyblockv1alpha2.StorageBackupList
	if err := r.List(ctx, &requests, client.MatchingFields{RequestSnapshotIndex: o.GetName()}); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(requests.Items))
	for i := range requests.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&requests.Items[i])})
	}
	return out
}

// requestError is a request that cannot proceed. A permanent one fails it, the
// others hold it in Pending because the missing thing may appear later.
type requestError struct {
	msg       string
	permanent bool
}

func (e requestError) Error() string { return e.msg }

func blocked(format string, a ...any) error { return requestError{msg: fmt.Sprintf(format, a...)} }
func refused(format string, a ...any) error {
	return requestError{msg: fmt.Sprintf(format, a...), permanent: true}
}

// requestSubject is what a request resolves to before any call is made.
type requestSubject struct {
	claim, volume string
	fsType        string
	handle        lvol.Handle
}

func (r *StorageBackupRequestReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var sb simplyblockv1alpha2.StorageBackup
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if sb.Spec.Source == nil || sb.Status.Phase == simplyblockv1alpha2.StorageBackupPhaseAvailable ||
		sb.Status.Phase == simplyblockv1alpha2.StorageBackupPhaseFailed {
		return ctrl.Result{}, nil
	}

	s, err := r.resolve(ctx, &sb)
	if err != nil {
		return r.hold(ctx, &sb, err)
	}
	snapshot, err := r.ensureSnapshot(ctx, &sb, s)
	if err != nil || snapshot.ID == "" {
		return r.hold(ctx, &sb, err)
	}
	backup, err := r.ensureBackup(ctx, s, snapshot.ID)
	if err != nil || backup == nil {
		return r.hold(ctx, &sb, err)
	}
	return r.report(ctx, &sb, s, snapshot, *backup)
}

// resolve finds the claim's volume and the cluster that provisioned it. The
// cluster is found by the UUID in the volume's handle, in any namespace.
func (r *StorageBackupRequestReconciler) resolve(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
) (*requestSubject, error) {
	name := sb.Spec.Source.ClaimName
	var claim corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, blocked("claim %s does not exist in namespace %s", name, sb.Namespace)
		}
		return nil, err
	}
	if claim.Spec.VolumeName == "" {
		return nil, blocked("claim %s is not bound to a volume yet", name)
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.VolumeName}, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, blocked("volume %s of claim %s does not exist", claim.Spec.VolumeName, name)
		}
		return nil, err
	}
	if pv.Spec.CSI == nil {
		return nil, refused("claim %s is not backed by a simplyblock volume", name)
	}
	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !ok {
		return nil, refused("claim %s is not backed by a simplyblock volume", name)
	}
	if !lvol.IsCanonicalUUID(handle.PoolRef) {
		return nil, refused("volume %s names its pool %q, not its identifier", pv.Name, handle.PoolRef)
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return nil, err
	}
	for _, c := range clusters.Items {
		if c.Status.UUID != handle.ClusterID {
			continue
		}
		if c.Spec.Backup == nil {
			return nil, blocked("cluster %s has no backup location configured", c.Name)
		}
		return &requestSubject{claim: name, volume: pv.Name, fsType: pv.Spec.CSI.FSType, handle: handle}, nil
	}
	return nil, blocked("no StorageCluster reports cluster %s", handle.ClusterID)
}

// ensureSnapshot returns the snapshot this request owns, taking it if needed.
// An empty ID means it is not visible yet. The stream's cache is the record of
// what exists, so an unsynced scope is not evidence that the snapshot is absent:
// it holds the request in Pending rather than asking for a second snapshot.
func (r *StorageBackupRequestReconciler) ensureSnapshot(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup, s *requestSubject,
) (subscriptions.SnapshotDTO, error) {
	h, name := s.handle, requestSnapshotPrefix+string(sb.UID)
	scope := cpinformer.Scope{h.ClusterID, h.PoolRef}
	if snap, ok := r.Snapshots.FindByName(scope, name); ok {
		return snap, nil
	}
	if !r.Snapshots.Synced(scope) {
		return subscriptions.SnapshotDTO{}, blocked("the snapshot stream for pool %s has not synced yet", h.PoolRef)
	}
	id, err := r.API.CreateSnapshot(ctx, h.ClusterID, h.PoolRef, h.VolumeID, name)
	if errors.Is(err, errs.ErrAlreadyExists) {
		return subscriptions.SnapshotDTO{}, nil // an earlier pass took it, the stream has not reported it
	}
	return subscriptions.SnapshotDTO{ID: id, Name: name}, err
}

// ensureBackup returns the backup of the snapshot, requesting it if needed. Nil
// means it is not visible yet: the stream reports the copy a moment after the
// control plane accepts the request, and wakes this request when it does.
func (r *StorageBackupRequestReconciler) ensureBackup(
	ctx context.Context, s *requestSubject, snapshotID string,
) (*subscriptions.BackupDTO, error) {
	if b, ok := r.Backups.FindBySnapshot(s.handle.ClusterID, snapshotID); ok {
		return &b, nil
	}
	if !r.Backups.Synced(cpinformer.Scope{s.handle.ClusterID}) {
		return nil, blocked("the backup stream for cluster %s has not synced yet", s.handle.ClusterID)
	}
	if err := r.API.CreateBackup(ctx, s.handle.ClusterID, snapshotID); err != nil &&
		!errors.Is(err, errs.ErrAlreadyExists) {
		return nil, err
	}
	return nil, nil
}

// report records the backup's progress and ends the request when it is done.
func (r *StorageBackupRequestReconciler) report(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
	s *requestSubject, snap subscriptions.SnapshotDTO, backup subscriptions.BackupDTO,
) (ctrl.Result, error) {
	phase := backupPhaseFor(backup.Status)
	copied := backupCopyFrom(backup)

	previous := sb.Status.Phase
	err := r.writeStatus(ctx, sb, func(st *simplyblockv1alpha2.StorageBackupStatus) {
		st.Phase, st.APIStatus, st.Backup = phase, backup.Status, copied
		st.Message = backupMessageFor(backup.Status)
		st.ClusterID = s.handle.ClusterID
		st.Source = &simplyblockv1alpha2.BackupSource{
			ClaimName: s.claim, ClaimNamespace: sb.Namespace, PersistentVolumeName: s.volume,
			PoolUUID: s.handle.PoolRef, LvolID: s.handle.VolumeID, FSType: s.fsType,
			SnapshotID: snap.ID, SnapshotName: snap.Name, ClusterUUID: s.handle.ClusterID,
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if previous != phase {
		switch phase {
		case simplyblockv1alpha2.StorageBackupPhaseAvailable:
			r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, ReasonBackupAvailable, ReasonBackupAvailable,
				"Backup %s of claim %s is complete", backup.ID, s.claim)
		case simplyblockv1alpha2.StorageBackupPhaseFailed:
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, ReasonBackupFailed, ReasonBackupFailed,
				"Backup %s of claim %s failed", backup.ID, s.claim)
		}
	}
	if phase == simplyblockv1alpha2.StorageBackupPhaseAvailable || phase == simplyblockv1alpha2.StorageBackupPhaseFailed {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: requestResync}, nil
}

// hold turns an error, or a not-yet-visible result (nil), into a status. A
// requestError holds the request in Pending or fails it, a retryable error is
// returned for controller-runtime to back off, and any other fails it.
func (r *StorageBackupRequestReconciler) hold(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup, err error,
) (ctrl.Result, error) {
	var re requestError
	phase, msg := simplyblockv1alpha2.StorageBackupPhaseFailed, ""
	switch {
	case err == nil:
		return ctrl.Result{RequeueAfter: requestResync}, nil
	case errors.As(err, &re):
		msg = re.msg
		if !re.permanent {
			phase = simplyblockv1alpha2.StorageBackupPhasePending
		}
	case class.Retryable(err):
		return ctrl.Result{}, err
	default:
		msg = err.Error()
	}

	changed := sb.Status.Phase != phase || sb.Status.Message != msg
	if err := r.writeStatus(ctx, sb, func(st *simplyblockv1alpha2.StorageBackupStatus) {
		st.Phase, st.Message = phase, msg
	}); err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		reason := ReasonBackupRequestBlocked
		if phase == simplyblockv1alpha2.StorageBackupPhaseFailed {
			reason = ReasonBackupFailed
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, reason, reason, "%s", msg)
	}
	if phase == simplyblockv1alpha2.StorageBackupPhaseFailed {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: requestRetry}, nil
}

// writeStatus applies a change under an optimistic lock and records the
// generation it was computed from.
func (r *StorageBackupRequestReconciler) writeStatus(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
	change func(*simplyblockv1alpha2.StorageBackupStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur simplyblockv1alpha2.StorageBackup
		if err := r.Get(ctx, client.ObjectKeyFromObject(sb), &cur); err != nil {
			return err
		}
		want := *cur.Status.DeepCopy()
		change(&want)
		want.ObservedGeneration = cur.Generation
		sb.Status = cur.Status
		if reflect.DeepEqual(cur.Status, want) {
			return nil
		}
		patch := client.MergeFromWithOptions(cur.DeepCopy(), client.MergeFromWithOptimisticLock{})
		cur.Status = want
		if err := r.Status().Patch(ctx, &cur, patch); err != nil {
			return err
		}
		sb.Status = cur.Status
		return nil
	})
}

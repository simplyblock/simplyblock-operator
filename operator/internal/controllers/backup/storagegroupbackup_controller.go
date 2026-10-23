// The StorageGroupBackup reconciler: it takes one group backup of a consistency
// group on demand (design-consistency-group-backup.md §7.5).
//
// It takes one generation of the group, records its sequence, and asks the
// control plane to back that generation up as one unit. Neither call is made
// twice for one object. The take carries an id derived from the object's UID,
// which the control plane answers a retry of with the generation it already
// took. Once the sequence is recorded, a lost answer to the backup call is
// recovered from the backup stream by the generation and the group's member
// volumes. It then follows the member copies to Available or Failed. The mirror
// records those copies beside this object, which is not this reconciler's work:
// it only reads where they are.

package backup

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/lvol"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// StorageGroupBackupIndex indexes StorageGroupBackups by the bare id of the group
// backup they produced. The mirror reads it to place a member copy beside its
// owner, and a member copy's stream event is mapped through it to the owner.
const StorageGroupBackupIndex = "status.groupBackupID"

// IndexStorageGroupBackup is the index function for [StorageGroupBackupIndex],
// exported so that a test's client can register what the manager does.
func IndexStorageGroupBackup(o client.Object) []string {
	sgb, ok := o.(*simplyblockv1alpha2.StorageGroupBackup)
	if !ok || sgb.Status.GroupBackupID == "" {
		return nil
	}
	return []string{sgb.GroupBackupLabelValue()}
}

// groupTakePrefix starts the id a group backup takes its generation under,
// which ends in the object's UID so a retried pass sends the same id.
const groupTakePrefix = "sgb-"

// GroupBackupClient is the control-plane surface a group backup writes through:
// resolve the group, take one generation, and back that generation up as one
// unit. What it reads comes from the backup stream.
type GroupBackupClient interface {
	ConsistencyGroupByName(ctx context.Context, clusterID, name string) (string, error)
	TakeGroupSnapshot(ctx context.Context, clusterID, groupID, requestID string) (int, error)
	CreateGroupBackup(ctx context.Context, clusterID, groupID string, groupSeq int) (controlplane.CreatedGroupBackup, error)
}

// GroupBackupStream is the read surface a group backup needs from the backup
// subscription.
type GroupBackupStream interface {
	// Synced reports whether a scope's first frame has been applied.
	Synced(scope cpinformer.Scope) bool
	// FindGroupMembers returns the member copies of one group backup.
	FindGroupMembers(clusterID, groupBackupID string) []subscriptions.BackupDTO
	// FindGroupBackupByGeneration returns the group backup of a generation whose
	// members include one of the volumes.
	FindGroupBackupByGeneration(clusterID string, seq int64, lvolIDs []string) (string, bool)
	// GroupTriggers carries the bare ids of group backups whose members changed.
	GroupTriggers() <-chan event.GenericEvent
}

// StorageGroupBackupReconciler takes a group backup on demand and follows it.
type StorageGroupBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      GroupBackupClient
	Backups  GroupBackupStream
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagegroupbackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagegroupbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes;persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager relies on the mirror for the [StorageGroupBackupIndex] and
// the [PersistentVolumeLvolIDIndex].
func (r *StorageGroupBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageGroupBackup{}).
		Named("storagegroupbackup").
		WatchesRawSource(source.Channel(r.Backups.GroupTriggers(),
			handler.EnqueueRequestsFromMapFunc(r.requestsForGroupBackup))).
		Complete(r)
}

// requestsForGroupBackup maps a member copy's stream event, which carries its
// group backup's bare id, to the StorageGroupBackup that produced it.
func (r *StorageGroupBackupReconciler) requestsForGroupBackup(
	ctx context.Context, o client.Object,
) []reconcile.Request {
	var owners simplyblockv1alpha2.StorageGroupBackupList
	if err := r.List(ctx, &owners, client.MatchingFields{StorageGroupBackupIndex: o.GetName()}); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(owners.Items))
	for i := range owners.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&owners.Items[i])})
	}
	return out
}

// groupSubject is what a group backup resolves to before any call is made: the
// cluster the group's claims were provisioned on, and their volumes.
type groupSubject struct {
	clusterID string
	lvolIDs   []string
}

func (r *StorageGroupBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sgb simplyblockv1alpha2.StorageGroupBackup
	if err := r.Get(ctx, req.NamespacedName, &sgb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch sgb.Status.Phase {
	case simplyblockv1alpha2.StorageGroupBackupPhaseAvailable, simplyblockv1alpha2.StorageGroupBackupPhaseFailed:
		return ctrl.Result{}, nil
	}

	if sgb.Status.GroupBackupID == "" {
		s, err := r.resolve(ctx, &sgb)
		if err != nil {
			return r.hold(ctx, &sgb, err)
		}
		if done, err := r.ensureGroupBackup(ctx, &sgb, s); err != nil || !done {
			return r.hold(ctx, &sgb, err)
		}
	}
	return r.report(ctx, &sgb)
}

// resolve finds the claims in the object's namespace labeled into the group,
// and the one cluster their volumes live on.
func (r *StorageGroupBackupReconciler) resolve(
	ctx context.Context, sgb *simplyblockv1alpha2.StorageGroupBackup,
) (*groupSubject, error) {
	name := sgb.Spec.Source.ConsistencyGroupName
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(sgb.Namespace),
		client.MatchingLabels{consistencyGroupLabel: name}); err != nil {
		return nil, err
	}
	if len(claims.Items) == 0 {
		return nil, blocked("no claim in namespace %s is labeled into consistency group %s", sgb.Namespace, name)
	}

	s := &groupSubject{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.VolumeName == "" {
			return nil, blocked("claim %s of consistency group %s is not bound to a volume yet", claim.Name, name)
		}
		var pv corev1.PersistentVolume
		if err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.VolumeName}, &pv); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, blocked("volume %s of claim %s does not exist", claim.Spec.VolumeName, claim.Name)
			}
			return nil, err
		}
		if pv.Spec.CSI == nil {
			return nil, refused("claim %s is not backed by a simplyblock volume", claim.Name)
		}
		handle, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
		if !ok {
			return nil, refused("claim %s is not backed by a simplyblock volume", claim.Name)
		}
		if s.clusterID != "" && s.clusterID != handle.ClusterID {
			return nil, refused("the claims of consistency group %s are on two clusters, %s and %s",
				name, s.clusterID, handle.ClusterID)
		}
		s.clusterID = handle.ClusterID
		s.lvolIDs = append(s.lvolIDs, handle.VolumeID)
	}
	if err := clusterMayBackUp(ctx, r.Client, s.clusterID); err != nil {
		return nil, err
	}
	return s, nil
}

// ensureGroupBackup takes the generation if none is recorded, then finds or asks
// for its group backup, and reports whether the group backup's id is recorded.
func (r *StorageGroupBackupReconciler) ensureGroupBackup(
	ctx context.Context, sgb *simplyblockv1alpha2.StorageGroupBackup, s *groupSubject,
) (bool, error) {
	name := sgb.Spec.Source.ConsistencyGroupName
	groupID, err := r.API.ConsistencyGroupByName(ctx, s.clusterID, name)
	if errors.Is(err, errs.ErrNotFound) {
		return false, blocked("cluster %s has no consistency group named %s yet", s.clusterID, name)
	}
	if err != nil {
		return false, err
	}

	seq := sgb.Status.Generation
	if seq == 0 {
		// Recorded before the backup is asked for, so a retry backs up this
		// generation. The id makes a lost write cost nothing: a retake under it
		// is answered with this generation.
		taken, err := r.API.TakeGroupSnapshot(ctx, s.clusterID, groupID, groupTakePrefix+string(sgb.UID))
		if err != nil {
			return false, err
		}
		seq = int64(taken)
		if err := r.writeStatus(ctx, sgb, func(st *simplyblockv1alpha2.StorageGroupBackupStatus) {
			st.Phase = simplyblockv1alpha2.StorageGroupBackupPhaseCreating
			st.ClusterID = s.clusterID
			st.Generation = seq
			st.Message = fmt.Sprintf("Took generation %d of consistency group %s", seq, name)
		}); err != nil {
			return false, err
		}
	}

	// The stream is the record of what exists, so a cold one is no evidence
	// that the group backup was never asked for.
	if !r.Backups.Synced(cpinformer.Scope{s.clusterID}) {
		return false, blocked("the backup stream for cluster %s has not synced yet", s.clusterID)
	}
	if id, ok := r.Backups.FindGroupBackupByGeneration(s.clusterID, seq, s.lvolIDs); ok {
		return true, r.recordGroupBackup(ctx, sgb, id, int32(len(s.lvolIDs)))
	}
	created, err := r.API.CreateGroupBackup(ctx, s.clusterID, groupID, int(seq))
	if err != nil {
		// The control plane answers every refusal with a 409, including a node
		// that is briefly offline, so the object waits rather than failing.
		return false, blocked("the control plane did not accept the backup of generation %d of %s: %v", seq, name, err)
	}
	return true, r.recordGroupBackup(ctx, sgb, created.ID, int32(created.MemberCount))
}

// recordGroupBackup writes the group backup's id and how many members must
// complete before it commits.
func (r *StorageGroupBackupReconciler) recordGroupBackup(
	ctx context.Context, sgb *simplyblockv1alpha2.StorageGroupBackup, id string, members int32,
) error {
	return r.writeStatus(ctx, sgb, func(st *simplyblockv1alpha2.StorageGroupBackupStatus) {
		st.Phase = simplyblockv1alpha2.StorageGroupBackupPhaseCreating
		st.GroupBackupID = id
		st.MemberCount = members
		st.Message = fmt.Sprintf("Backing up %d member(s) as group backup %s", members, id)
	})
}

// report follows the member copies: Failed when any member failed, Available
// when every expected member completed, Creating until then.
func (r *StorageGroupBackupReconciler) report(
	ctx context.Context, sgb *simplyblockv1alpha2.StorageGroupBackup,
) (ctrl.Result, error) {
	copies := r.Backups.FindGroupMembers(sgb.Status.ClusterID, sgb.Status.GroupBackupID)
	members, err := r.membersFrom(ctx, sgb, copies)
	if err != nil {
		return ctrl.Result{}, err
	}

	completed, failed := 0, ""
	var size int64
	var last int64
	for _, c := range copies {
		switch backupPhaseFor(c.Status) {
		case simplyblockv1alpha2.StorageBackupPhaseAvailable:
			completed++
			size += c.Size
			last = max(last, c.CompletedAt)
		case simplyblockv1alpha2.StorageBackupPhaseFailed:
			failed = c.ID
		}
	}

	st := sgb.Status
	phase := simplyblockv1alpha2.StorageGroupBackupPhaseCreating
	message := fmt.Sprintf("%d of %d member(s) of group backup %s are complete",
		completed, st.MemberCount, st.GroupBackupID)
	condition := metav1.Condition{
		Type: simplyblockv1alpha2.GroupBackupConditionAvailable, Status: metav1.ConditionFalse,
		Reason: "MembersUploading", Message: message,
	}
	switch {
	case failed != "":
		phase = simplyblockv1alpha2.StorageGroupBackupPhaseFailed
		message = fmt.Sprintf("member backup %s of group backup %s failed", failed, st.GroupBackupID)
		condition.Reason, condition.Message = "MemberFailed", message
	case st.MemberCount > 0 && completed >= int(st.MemberCount):
		phase = simplyblockv1alpha2.StorageGroupBackupPhaseAvailable
		message = fmt.Sprintf("Group backup %s of consistency group %s is complete",
			st.GroupBackupID, sgb.Spec.Source.ConsistencyGroupName)
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "AllMembersCompleted", message
	}

	previous := st.Phase
	if err := r.writeStatus(ctx, sgb, func(st *simplyblockv1alpha2.StorageGroupBackupStatus) {
		st.Phase, st.Message, st.Members = phase, message, members
		condition.ObservedGeneration = sgb.Generation
		apimeta.SetStatusCondition(&st.Conditions, condition)
		if phase == simplyblockv1alpha2.StorageGroupBackupPhaseAvailable {
			st.Size = &size
			st.CompletedAt = unixToTime(last)
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	if previous != phase {
		switch phase {
		case simplyblockv1alpha2.StorageGroupBackupPhaseAvailable:
			r.Recorder.Eventf(sgb, nil, corev1.EventTypeNormal, ReasonGroupBackupAvailable, ReasonGroupBackupAvailable, "%s", message)
		case simplyblockv1alpha2.StorageGroupBackupPhaseFailed:
			r.Recorder.Eventf(sgb, nil, corev1.EventTypeWarning, ReasonGroupBackupFailed, ReasonGroupBackupFailed, "%s", message)
		}
	}
	if phase == simplyblockv1alpha2.StorageGroupBackupPhaseCreating {
		return ctrl.Result{RequeueAfter: requestResync}, nil
	}
	return ctrl.Result{}, nil
}

// membersFrom lists the member copies the stream reports, naming the claim each
// was taken from and the record the mirror placed beside this object, if it
// has placed one yet.
func (r *StorageGroupBackupReconciler) membersFrom(
	ctx context.Context, sgb *simplyblockv1alpha2.StorageGroupBackup, copies []subscriptions.BackupDTO,
) ([]simplyblockv1alpha2.GroupBackupMember, error) {
	members := make([]simplyblockv1alpha2.GroupBackupMember, 0, len(copies))
	for _, c := range copies {
		m := simplyblockv1alpha2.GroupBackupMember{BackupID: c.ID, LvolName: c.LvolName, Phase: backupPhaseFor(c.Status)}

		name := simplyblockv1alpha2.StorageBackupName(c.ID)
		var placed simplyblockv1alpha2.StorageBackup
		err := r.Get(ctx, types.NamespacedName{Namespace: sgb.Namespace, Name: name}, &placed)
		switch {
		case err == nil:
			m.BackupName = name
		case !apierrors.IsNotFound(err):
			return nil, err
		}

		var volumes corev1.PersistentVolumeList
		if err := r.List(ctx, &volumes, client.MatchingFields{PersistentVolumeLvolIDIndex: c.LvolID}); err != nil {
			return nil, err
		}
		if len(volumes.Items) > 0 && volumes.Items[0].Spec.ClaimRef != nil {
			m.ClaimName = volumes.Items[0].Spec.ClaimRef.Name
		}
		members = append(members, m)
	}
	return members, nil
}

// hold turns an error into a status: a requestError holds the object in Pending
// or fails it, a retryable error is returned for controller-runtime to back off,
// and any other fails it.
func (r *StorageGroupBackupReconciler) hold(
	ctx context.Context, sgb *simplyblockv1alpha2.StorageGroupBackup, err error,
) (ctrl.Result, error) {
	var re requestError
	phase, msg := simplyblockv1alpha2.StorageGroupBackupPhaseFailed, ""
	switch {
	case err == nil:
		return ctrl.Result{RequeueAfter: requestResync}, nil
	case errors.As(err, &re):
		msg = re.msg
		if !re.permanent {
			phase = simplyblockv1alpha2.StorageGroupBackupPhasePending
			if sgb.Status.Generation != 0 {
				// A generation is taken: the group backup is under way, and the
				// thing it waits for is a step inside it.
				phase = simplyblockv1alpha2.StorageGroupBackupPhaseCreating
			}
		}
	case class.Retryable(err):
		return ctrl.Result{}, err
	default:
		msg = err.Error()
	}

	changed := sgb.Status.Phase != phase || sgb.Status.Message != msg
	if err := r.writeStatus(ctx, sgb, func(st *simplyblockv1alpha2.StorageGroupBackupStatus) {
		st.Phase, st.Message = phase, msg
	}); err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		reason := ReasonBackupRequestBlocked
		if phase == simplyblockv1alpha2.StorageGroupBackupPhaseFailed {
			reason = ReasonGroupBackupFailed
		}
		r.Recorder.Eventf(sgb, nil, corev1.EventTypeWarning, reason, reason, "%s", msg)
	}
	if phase == simplyblockv1alpha2.StorageGroupBackupPhaseFailed {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: requestRetry}, nil
}

// writeStatus applies a change under an optimistic lock and records the
// generation it was computed from.
func (r *StorageGroupBackupReconciler) writeStatus(
	ctx context.Context, sgb *simplyblockv1alpha2.StorageGroupBackup,
	change func(*simplyblockv1alpha2.StorageGroupBackupStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur simplyblockv1alpha2.StorageGroupBackup
		if err := r.Get(ctx, client.ObjectKeyFromObject(sgb), &cur); err != nil {
			return err
		}
		want := *cur.Status.DeepCopy()
		change(&want)
		want.ObservedGeneration = cur.Generation
		sgb.Status = cur.Status
		if reflect.DeepEqual(cur.Status, want) {
			return nil
		}
		patch := client.MergeFromWithOptions(cur.DeepCopy(), client.MergeFromWithOptimisticLock{})
		cur.Status = want
		if err := r.Status().Patch(ctx, &cur, patch); err != nil {
			return err
		}
		sgb.Status = cur.Status
		return nil
	})
}

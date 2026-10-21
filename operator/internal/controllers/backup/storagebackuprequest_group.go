// The group path of the StorageBackup request reconciler: a request with
// spec.source.consistencyGroupName backs up a whole consistency group on demand
// (design-consistency-group-backup.md §7.5).
//
// It takes one generation of the group, records its sequence, and asks the
// control plane to back that generation up as one unit. Neither call is made
// twice for one request. The take carries an id derived from the request's UID,
// which the control plane answers a retry of with the generation it already
// took. Once the sequence is recorded, a lost answer to the backup call is
// recovered from the backup stream by the generation and the group's member
// volumes. The member copies are
// recorded beside the cluster by the mirror, as every copy is. This object is
// the group's handle, and it follows the members to Available or Failed.

package backup

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// RequestGroupBackupIndex indexes group requests by the bare id of the group
// backup they produced, which is what a member copy's stream event carries.
const RequestGroupBackupIndex = "status.source.group.groupBackupID.requested"

// groupTakePrefix starts the id a group request takes its generation under,
// which ends in the request's UID so a retried pass sends the same id.
const groupTakePrefix = "sbk-"

// IndexRequestGroupBackup is the index function for [RequestGroupBackupIndex],
// exported so that a test's client can register what the manager does.
func IndexRequestGroupBackup(o client.Object) []string {
	sb, ok := o.(*simplyblockv1alpha2.StorageBackup)
	if !ok || !sb.IsGroupRequest() || sb.GroupBackupID() == "" {
		return nil
	}
	return []string{simplyblockv1alpha2.GroupBackupLabelValue(sb.GroupBackupID())}
}

// requestsForGroupBackup maps a member copy's stream event, which carries its
// group backup's bare id, to the request that produced the group backup.
func (r *StorageBackupRequestReconciler) requestsForGroupBackup(
	ctx context.Context, o client.Object,
) []reconcile.Request {
	var requests simplyblockv1alpha2.StorageBackupList
	if err := r.List(ctx, &requests, client.MatchingFields{RequestGroupBackupIndex: o.GetName()}); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(requests.Items))
	for i := range requests.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&requests.Items[i])})
	}
	return out
}

// groupSubject is what a group request resolves to before any call is made: the
// cluster the group's claims were provisioned on, and their volumes.
type groupSubject struct {
	clusterID string
	lvolIDs   []string
}

// reconcileGroup advances a group request by one pass.
func (r *StorageBackupRequestReconciler) reconcileGroup(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
) (ctrl.Result, error) {
	if sb.GroupBackupID() == "" {
		s, err := r.resolveGroup(ctx, sb)
		if err != nil {
			return r.hold(ctx, sb, err)
		}
		if done, err := r.ensureGroupBackup(ctx, sb, s); err != nil || !done {
			return r.hold(ctx, sb, err)
		}
	}
	return r.reportGroup(ctx, sb)
}

// resolveGroup finds the claims in the request's namespace labeled into the
// group, and the one cluster their volumes live on.
func (r *StorageBackupRequestReconciler) resolveGroup(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
) (*groupSubject, error) {
	name := sb.Spec.Source.ConsistencyGroupName
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(sb.Namespace),
		client.MatchingLabels{consistencyGroupLabel: name}); err != nil {
		return nil, err
	}
	if len(claims.Items) == 0 {
		return nil, blocked("no claim in namespace %s is labeled into consistency group %s", sb.Namespace, name)
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
	if err := r.clusterMayBackUp(ctx, s.clusterID); err != nil {
		return nil, err
	}
	return s, nil
}

// ensureGroupBackup takes the generation if none is recorded, then finds or
// asks for its group backup, and reports whether the group backup's id is now
// recorded.
func (r *StorageBackupRequestReconciler) ensureGroupBackup(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup, s *groupSubject,
) (bool, error) {
	name := sb.Spec.Source.ConsistencyGroupName
	groupID, err := r.API.ConsistencyGroupByName(ctx, s.clusterID, name)
	if errors.Is(err, errs.ErrNotFound) {
		return false, blocked("cluster %s has no consistency group named %s yet", s.clusterID, name)
	}
	if err != nil {
		return false, err
	}

	seq := int64(0)
	if g := sb.Source().Group; g != nil {
		seq = g.Generation
	}
	if seq == 0 {
		// The take carries an id derived from the request's UID, so a pass that
		// took the generation and lost the write recording it is answered with
		// the same generation. The sequence is recorded before the backup is
		// asked for, so a retry backs up this generation.
		taken, err := r.API.TakeGroupSnapshot(ctx, s.clusterID, groupID, groupTakePrefix+string(sb.UID))
		if err != nil {
			return false, err
		}
		seq = int64(taken)
		if err := r.writeStatus(ctx, sb, func(st *simplyblockv1alpha2.StorageBackupStatus) {
			st.Phase = simplyblockv1alpha2.StorageBackupPhaseCreating
			st.ClusterID = s.clusterID
			st.Message = fmt.Sprintf("Took generation %d of consistency group %s", seq, name)
			st.Source = &simplyblockv1alpha2.BackupSource{
				ClaimNamespace: sb.Namespace, ClusterUUID: s.clusterID,
				Group: &simplyblockv1alpha2.BackupGroup{ConsistencyGroupName: name, Generation: seq},
			}
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
		return true, r.recordGroupBackup(ctx, sb, id, int32(len(s.lvolIDs)))
	}
	created, err := r.API.CreateGroupBackup(ctx, s.clusterID, groupID, int(seq))
	if err != nil {
		// The control plane answers every refusal with a 409, including a node
		// that is briefly offline, so the request waits rather than failing.
		return false, blocked("the control plane did not accept the backup of generation %d of %s: %v", seq, name, err)
	}
	return true, r.recordGroupBackup(ctx, sb, created.ID, int32(created.MemberCount))
}

// recordGroupBackup writes the group backup's id and how many members must
// complete before it commits.
func (r *StorageBackupRequestReconciler) recordGroupBackup(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup, id string, members int32,
) error {
	return r.writeStatus(ctx, sb, func(st *simplyblockv1alpha2.StorageBackupStatus) {
		st.Phase = simplyblockv1alpha2.StorageBackupPhaseCreating
		st.Message = fmt.Sprintf("Backing up %d member(s) as group backup %s", members, id)
		if st.Source == nil {
			st.Source = &simplyblockv1alpha2.BackupSource{}
		}
		if st.Source.Group == nil {
			st.Source.Group = &simplyblockv1alpha2.BackupGroup{}
		}
		st.Source.Group.GroupBackupID = id
		st.Source.Group.MemberCount = members
	})
}

// reportGroup follows the member copies: Failed when any member failed,
// Available when every expected member completed, Creating until then.
func (r *StorageBackupRequestReconciler) reportGroup(
	ctx context.Context, sb *simplyblockv1alpha2.StorageBackup,
) (ctrl.Result, error) {
	group := sb.Source().Group
	members := r.Backups.FindGroupMembers(sb.Status.ClusterID, group.GroupBackupID)

	completed, failed := 0, ""
	for _, m := range members {
		switch backupPhaseFor(m.Status) {
		case simplyblockv1alpha2.StorageBackupPhaseAvailable:
			completed++
		case simplyblockv1alpha2.StorageBackupPhaseFailed:
			failed = m.ID
		}
	}
	phase := simplyblockv1alpha2.StorageBackupPhaseCreating
	message := fmt.Sprintf("%d of %d member(s) of group backup %s are complete",
		completed, group.MemberCount, group.GroupBackupID)
	switch {
	case failed != "":
		phase = simplyblockv1alpha2.StorageBackupPhaseFailed
		message = fmt.Sprintf("member backup %s of group backup %s failed", failed, group.GroupBackupID)
	case group.MemberCount > 0 && completed >= int(group.MemberCount):
		phase = simplyblockv1alpha2.StorageBackupPhaseAvailable
		message = fmt.Sprintf("Group backup %s of consistency group %s is complete",
			group.GroupBackupID, group.ConsistencyGroupName)
	}

	previous := sb.Status.Phase
	if err := r.writeStatus(ctx, sb, func(st *simplyblockv1alpha2.StorageBackupStatus) {
		st.Phase, st.Message = phase, message
	}); err != nil {
		return ctrl.Result{}, err
	}
	if previous != phase {
		switch phase {
		case simplyblockv1alpha2.StorageBackupPhaseAvailable:
			r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, ReasonBackupAvailable, ReasonBackupAvailable, "%s", message)
		case simplyblockv1alpha2.StorageBackupPhaseFailed:
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, ReasonBackupFailed, ReasonBackupFailed, "%s", message)
		}
	}
	if phase == simplyblockv1alpha2.StorageBackupPhaseCreating {
		return ctrl.Result{RequeueAfter: requestResync}, nil
	}
	return ctrl.Result{}, nil
}

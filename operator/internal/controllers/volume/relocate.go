// The Relocate action (design-superclusters.md §8): a volume moved to another
// member cluster of its supercluster, together with every volume sharing its
// NVMe-oF subsystem, by the control plane's replication in migration mode.
//
// The steps follow the planned cutover the replication slot drives:
// replicate until in sync, commit with the source's deletion, wait for a
// volume's cutover to be prepared, connect the target's paths on every
// consuming node while the cutover waits for them, release it, and verify that
// the volume's handle resolves to the target. The control plane cuts the
// volumes of one subsystem over one at a time, so AwaitingCutover,
// AttachingPaths, and Proceeding run once per volume, in turn.
//
// It lives beside Migrate's steps because the two share the lock, the
// subject, and the operation's lifecycle; what differs is the control plane's
// mechanism, which is replication between clusters rather than a migration
// inside one.

package volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// RelocationClient is the control-plane surface a Relocate needs.
type RelocationClient interface {
	StartVolumeMove(ctx context.Context, h lvol.VolumeHandle, target controlplane.MoveTarget) error
	StopVolumeReplication(ctx context.Context, h lvol.VolumeHandle) error
	GetVolumeReplicationInfo(ctx context.Context, h lvol.VolumeHandle) (controlplane.ReplicationStatus, error)
	CommitVolumeMove(ctx context.Context, h lvol.VolumeHandle) (string, error)
	GetVolumeReplicationRelationship(ctx context.Context, h lvol.VolumeHandle) (controlplane.Relationship, error)
	Connection(ctx context.Context, h lvol.VolumeHandle, opts ...lvol.ConnectionOption) (lvol.Connection, error)
	ProceedVolumeCutover(ctx context.Context, h lvol.VolumeHandle) error
	Task(ctx context.Context, clusterID, taskID string) (controlplane.Task, error)
}

const (
	stepReplicating     = simplyblockv1alpha2.PersistentVolumeOpsStepReplicating
	stepCommitting      = simplyblockv1alpha2.PersistentVolumeOpsStepCommitting
	stepAwaitingCutover = simplyblockv1alpha2.PersistentVolumeOpsStepAwaitingCutover
	stepAttachingPaths  = simplyblockv1alpha2.PersistentVolumeOpsStepAttachingPaths
	stepProceeding      = simplyblockv1alpha2.PersistentVolumeOpsStepProceeding
)

// RelocatedToAnnotation is kube.AnnoRelocatedTo, which this controller writes
// and the CSI driver reads.
const RelocatedToAnnotation = kube.AnnoRelocatedTo

const (
	// relocateIntervalMinutes is how often the source takes the snapshots
	// replication ships until the commit. The control plane takes none at
	// zero, and the move would wait for a user's snapshot.
	relocateIntervalMinutes = 1

	// modePreconnect is the host-side Job mode connecting a cutover's target
	// paths, which the replication slot's cutover uses as well.
	modePreconnect = "replication-preconnect"

	// preconnectJobDeadline caps a path Job. The binary tries three times,
	// five seconds apart.
	preconnectJobDeadline = 120 * time.Second

	// cutoverDoneResult is what a cutover task reports when it succeeded.
	cutoverDoneResult = "cutover done"
)

// performRelocate advances the current step of a Relocate.
func (r *PersistentVolumeOpsReconciler) performRelocate(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, current step,
) (bool, error) {
	switch current {
	case stepValidating:
		return r.validateRelocation(ctx, ops, subject)
	case stepReplicating:
		return r.replicate(ctx, ops, subject)
	case stepCommitting:
		return r.commitRelocation(ctx, ops, subject)
	case stepAwaitingCutover:
		return r.awaitCutover(ctx, ops, subject)
	case stepAttachingPaths:
		return r.attachTargetPaths(ctx, ops, subject)
	case stepProceeding:
		return r.proceed(ctx, ops, subject)
	case stepVerifying:
		return r.verifyRelocation(ctx, ops, subject)
	default:
		return false, fatalf("step %s has no implementation for Relocate", current)
	}
}

// validateRelocation resolves the target member and its pool, lists the
// volumes moving with the subsystem, and refuses what a move cannot carry.
func (r *PersistentVolumeOpsReconciler) validateRelocation(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	if ops.Status.Relocate != nil && len(ops.Status.Relocate.Volumes) > 0 {
		return true, nil
	}
	source := subject.cluster
	if source.Spec.SuperclusterRef == "" {
		return false, fatalf("cluster %s is not a member of a supercluster", source.Name)
	}
	targetName := ops.Spec.Relocate.TargetClusterRef
	if targetName == source.Name {
		return false, fatalf("volume %s is already in member %s", subject.pv.Name, source.Name)
	}
	var target simplyblockv1alpha2.StorageCluster
	err := r.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: targetName}, &target)
	switch {
	case apierrors.IsNotFound(err):
		return false, fatalf("no StorageCluster %s/%s to move the volume to", source.Namespace, targetName)
	case err != nil:
		return false, fmt.Errorf("read the target member %s: %w", targetName, err)
	}
	if target.Spec.SuperclusterRef != source.Spec.SuperclusterRef {
		return false, fatalf("cluster %s is not a member of supercluster %s", targetName, source.Spec.SuperclusterRef)
	}
	if !utils.AcceptsPlacement(&target) || target.Status.UUID == "" {
		r.event(ops, corev1.EventTypeNormal, ReasonTargetMemberNotReady,
			"Member %s is %s and does not accept volumes yet", targetName, target.Status.Phase)
		return false, nil
	}

	pool, err := r.targetPool(ctx, source, &target, subject.handle.PoolRef)
	if err != nil || pool == "" {
		return false, err
	}

	volumes, err := r.relocatedVolumes(ctx, subject)
	if err != nil {
		return false, err
	}

	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Relocate = &simplyblockv1alpha2.RelocateStatus{
			SourceClusterUUID: subject.clusterUUID,
			TargetClusterUUID: target.Status.UUID,
			TargetPoolUUID:    pool,
			Volumes:           volumes,
		}
	}); err != nil {
		return false, err
	}
	return true, nil
}

// targetPool is the target member's backend pool of the supercluster pool the
// volume's pool belongs to. The supercluster's StoragePool records one
// backend pool per member, so the source's entry names the pool and the
// target's entry gives its counterpart.
func (r *PersistentVolumeOpsReconciler) targetPool(
	ctx context.Context, source, target *simplyblockv1alpha2.StorageCluster, sourcePool string,
) (string, error) {
	var pools simplyblockv1alpha2.StoragePoolList
	if err := r.List(ctx, &pools, client.InNamespace(source.Namespace)); err != nil {
		return "", fmt.Errorf("list the storage pools: %w", err)
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		if pool.Spec.ClusterRef != source.Spec.SuperclusterRef {
			continue
		}
		holds := false
		for _, m := range pool.Status.Members {
			if m.Cluster == source.Name && m.UUID == sourcePool {
				holds = true
			}
		}
		if !holds {
			continue
		}
		for _, m := range pool.Status.Members {
			if m.Cluster == target.Name {
				if m.UUID == "" {
					return "", fmt.Errorf("pool %s has not been created in member %s yet", pool.Name, target.Name)
				}
				return m.UUID, nil
			}
		}
		return "", fmt.Errorf("pool %s has no entry for member %s yet", pool.Name, target.Name)
	}
	return "", fatalf("no pool of supercluster %s holds pool %s of member %s",
		source.Spec.SuperclusterRef, sourcePool, source.Name)
}

// relocatedVolumes are the volumes moving together: the subsystem's, each with
// the PersistentVolume carrying it. An encrypted volume or one in a
// consistency group refuses the whole move, because its siblings cannot move
// without it.
func (r *PersistentVolumeOpsReconciler) relocatedVolumes(
	ctx context.Context, subject *subject,
) ([]simplyblockv1alpha2.RelocatedVolume, error) {
	named, err := r.API.Volume(ctx, subject.handle.Handle())
	if err != nil {
		return nil, fmt.Errorf("read volume %s: %w", subject.handle.Handle(), err)
	}
	members, err := r.API.SubsystemVolumes(ctx, subject.clusterUUID, named.NQN)
	if err != nil {
		return nil, fmt.Errorf("list the volumes of subsystem %s: %w", named.NQN, err)
	}
	wanted := expectedMembers(members, subject.handle.VolumeID)

	pvs, err := r.volumesFronting(ctx, wanted)
	if err != nil {
		return nil, err
	}
	byVolume := map[string]*corev1.PersistentVolume{}
	for _, pv := range pvs {
		handle, _ := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
		byVolume[handle.VolumeID] = pv
	}

	volumes := make([]simplyblockv1alpha2.RelocatedVolume, 0, len(wanted))
	for id := range wanted {
		entry := simplyblockv1alpha2.RelocatedVolume{VolumeUUID: id}
		if pv := byVolume[id]; pv != nil {
			entry.PersistentVolumeName = pv.Name
			if pv.Spec.CSI.VolumeAttributes[kube.ParamEncryption] == "true" {
				return nil, fatalf("volume %s is encrypted: a move deletes its source and with it the "+
					"only copy of the key, which replication does not carry", pv.Name)
			}
			if group, err := r.consistencyGroupOf(ctx, pv); err != nil {
				return nil, err
			} else if group != "" {
				return nil, fatalf("volume %s is in consistency group %s, whose members stay in one member",
					pv.Name, group)
			}
		}
		volumes = append(volumes, entry)
	}
	sortVolumes(volumes)
	return volumes, nil
}

// consistencyGroupOf is the consistency group the volume's claim names, or an
// empty string when it names none.
func (r *PersistentVolumeOpsReconciler) consistencyGroupOf(
	ctx context.Context, pv *corev1.PersistentVolume,
) (string, error) {
	ref := pv.Spec.ClaimRef
	if ref == nil {
		return "", nil
	}
	var claim corev1.PersistentVolumeClaim
	err := r.Reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &claim)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read claim %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return claim.Labels[kube.LabelConsistencyGroup], nil
}

// sourceHandle addresses one moving volume in the source member. The volumes
// of one subsystem share the named volume's pool.
func sourceHandle(ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, volumeID string) lvol.VolumeHandle {
	return lvol.NewVolumeHandle(ops.Status.Relocate.SourceClusterUUID, subject.handle.PoolRef, volumeID)
}

// replicate starts each volume's replication into the target and waits until
// all of them are in sync. The start is sent only to a volume not replicating
// yet, and a start the control plane refuses with a 4xx is a reason no
// reconcile changes.
func (r *PersistentVolumeOpsReconciler) replicate(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	relocate := ops.Status.Relocate
	inSync := true
	for _, v := range relocate.Volumes {
		h := sourceHandle(ops, subject, v.VolumeUUID)
		info, err := r.API.GetVolumeReplicationInfo(ctx, h)
		if err != nil {
			return false, fmt.Errorf("replication status of volume %s: %w", v.VolumeUUID, err)
		}
		if info.Role == "" || info.Role == "none" {
			err := r.API.StartVolumeMove(ctx, h, controlplane.MoveTarget{
				ClusterID:       relocate.TargetClusterUUID,
				PoolID:          relocate.TargetPoolUUID,
				IntervalMinutes: relocateIntervalMinutes,
			})
			if refused := clientRefusal(err); refused != nil {
				return false, fatalf("the control plane refused to start moving volume %s: %v", v.VolumeUUID, refused)
			}
			if err != nil {
				return false, fmt.Errorf("start moving volume %s: %w", v.VolumeUUID, err)
			}
			r.event(ops, corev1.EventTypeNormal, ReasonReplicationStarted,
				"Volume %s replicates into member %s", v.VolumeUUID, ops.Spec.Relocate.TargetClusterRef)
			inSync = false
			continue
		}
		if info.State != "in_sync" {
			inSync = false
		}
	}
	return inSync, nil
}

// clientRefusal is err when it is the control plane refusing the request with
// a 4xx, which retrying does not change.
func clientRefusal(err error) error {
	var status *controlplane.StatusError
	if errors.As(err, &status) && status.StatusCode >= http.StatusBadRequest &&
		status.StatusCode < http.StatusInternalServerError {
		return err
	}
	return nil
}

// commitRelocation records that the cutover is committed, then asks the
// control plane to cut each volume over and delete its source, recording the
// task it answers with. The commit is resent until a task is recorded: a
// resend the control plane already holds answers with the same task.
func (r *PersistentVolumeOpsReconciler) commitRelocation(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	if !ops.Status.Relocate.Committed {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			status.Relocate.Committed = true
		}); err != nil {
			return false, err
		}
	}
	for i, v := range ops.Status.Relocate.Volumes {
		if v.TaskID != "" || v.CutOver {
			continue
		}
		h := sourceHandle(ops, subject, v.VolumeUUID)
		task, err := r.API.CommitVolumeMove(ctx, h)
		if errors.Is(err, errs.ErrNotFound) {
			// The source is gone: the commit landed before, the cutover ran,
			// and its delete_source removed the volume this call addressed.
			done, rerr := r.cutOverPerRelationship(ctx, ops, subject, i)
			if rerr != nil {
				return false, rerr
			}
			if done {
				continue
			}
		}
		if err != nil {
			return false, fmt.Errorf("commit the move of volume %s: %w", v.VolumeUUID, err)
		}
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			status.Relocate.Volumes[i].TaskID = task
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// cutOverPerRelationship marks volume i cut over when its relationship says
// the target serves it, and reports whether it did.
func (r *PersistentVolumeOpsReconciler) cutOverPerRelationship(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, i int,
) (bool, error) {
	v := ops.Status.Relocate.Volumes[i]
	rel, err := r.API.GetVolumeReplicationRelationship(ctx, sourceHandle(ops, subject, v.VolumeUUID))
	if errors.Is(err, errs.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("replication relationship of volume %s: %w", v.VolumeUUID, err)
	}
	if !servedByTarget(rel) {
		return false, nil
	}
	return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Relocate.Volumes[i].CutOver = true
		status.Relocate.Volumes[i].TargetVolumeUUID = rel.TargetLvolID
	})
}

// servedByTarget is a relationship whose cutover has finished.
func servedByTarget(rel controlplane.Relationship) bool {
	return rel.State == controlplane.RelationshipCutoverDone || rel.State == controlplane.RelationshipFailedOver
}

// cuttingOver is the volume whose cutover is next: the first not cut over.
func cuttingOver(ops *simplyblockv1alpha2.PersistentVolumeOps) int {
	for i, v := range ops.Status.Relocate.Volumes {
		if !v.CutOver {
			return i
		}
	}
	return -1
}

// awaitCutover waits until the control plane has prepared the volume's
// cutover: caught the target up with the latest writes and recorded the
// relationship. Until then the cutover task's own report is the progress.
func (r *PersistentVolumeOpsReconciler) awaitCutover(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	i := cuttingOver(ops)
	if i < 0 {
		return true, nil
	}
	v := ops.Status.Relocate.Volumes[i]
	rel, err := r.API.GetVolumeReplicationRelationship(ctx, sourceHandle(ops, subject, v.VolumeUUID))
	switch {
	case errors.Is(err, errs.ErrNotFound):
		// The control plane records the relationship once the cutover is
		// prepared, after the delta has converged.
	case err != nil:
		return false, fmt.Errorf("replication relationship of volume %s: %w", v.VolumeUUID, err)
	case rel.State == controlplane.RelationshipCutoverPending || servedByTarget(rel):
		return true, nil
	}
	return false, r.cutoverProgress(ctx, ops, v)
}

// cutoverProgress is what the volume's cutover task last reported, as a wait.
func (r *PersistentVolumeOpsReconciler) cutoverProgress(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, v simplyblockv1alpha2.RelocatedVolume,
) error {
	if v.TaskID == "" {
		return nil
	}
	task, err := r.API.Task(ctx, ops.Status.Relocate.SourceClusterUUID, v.TaskID)
	if err != nil || task.Result == "" {
		return nil
	}
	return waitingf("the control plane is preparing the cutover of volume %s: %s", v.VolumeUUID, task.Result)
}

// attachTargetPaths connects the target's paths on every node consuming the
// subsystem while the volume's cutover is pending, one Job per node. A failing
// Job is replaced and the cutover not released; the control plane proceeds
// alone after its own timeout, and the nodes left without the target's paths
// then are recorded.
func (r *PersistentVolumeOpsReconciler) attachTargetPaths(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	i := cuttingOver(ops)
	if i < 0 {
		return true, nil
	}
	v := ops.Status.Relocate.Volumes[i]
	h := sourceHandle(ops, subject, v.VolumeUUID)
	rel, err := r.API.GetVolumeReplicationRelationship(ctx, h)
	switch {
	case err != nil:
		return false, fmt.Errorf("replication relationship of volume %s: %w", v.VolumeUUID, err)
	case servedByTarget(rel):
		return true, r.recordNodesMissingPaths(ctx, ops, v.VolumeUUID)
	case rel.State != controlplane.RelationshipCutoverPending:
		return false, nil
	}

	if ops.Status.Relocate.PathJobs == nil {
		return false, r.startPathJobs(ctx, ops, subject, h)
	}
	return r.pathJobsPassed(ctx, ops, subject, h)
}

// recordNodesMissingPaths records the consuming nodes that do not hold the
// target's paths although the control plane cut the volume over: every
// consumer when no path Job was started, otherwise the nodes whose Job has not
// succeeded.
func (r *PersistentVolumeOpsReconciler) recordNodesMissingPaths(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, volumeUUID string,
) error {
	var missing []string
	if ops.Status.Relocate.PathJobs == nil {
		nodes, err := r.relocationConsumers(ctx, ops)
		if err != nil {
			return err
		}
		missing = nodes
	} else {
		for _, recorded := range ops.Status.Relocate.PathJobs {
			if recorded.Succeeded {
				continue
			}
			var job batchv1.Job
			err := r.Get(ctx, client.ObjectKey{Namespace: recorded.Namespace, Name: recorded.Name}, &job)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil && jobOutcome(&job) == jobSucceeded {
				continue
			}
			missing = append(missing, recorded.Node)
		}
	}
	missing = slices.DeleteFunc(missing, func(node string) bool {
		return slices.Contains(ops.Status.Relocate.NodesMissingPaths, node)
	})
	if len(missing) == 0 {
		return nil
	}
	r.event(ops, corev1.EventTypeWarning, ReasonCutoverWithoutPaths,
		"The control plane cut volume %s over before nodes %s held the target's paths; "+
			"their pods lose the volume until they are restarted", volumeUUID, strings.Join(missing, ", "))
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Relocate.NodesMissingPaths = append(status.Relocate.NodesMissingPaths, missing...)
		sort.Strings(status.Relocate.NodesMissingPaths)
	})
}

// relocationConsumers are the nodes running a pod that mounts a moving
// volume's claim, sorted.
func (r *PersistentVolumeOpsReconciler) relocationConsumers(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) ([]string, error) {
	nodes := map[string]struct{}{}
	for _, v := range ops.Status.Relocate.Volumes {
		if v.PersistentVolumeName == "" {
			continue
		}
		var pv corev1.PersistentVolume
		if err := r.Reader.Get(ctx, client.ObjectKey{Name: v.PersistentVolumeName}, &pv); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		node, err := r.consumerNodeOf(ctx, &pv)
		if err != nil {
			return nil, err
		}
		if node != "" {
			nodes[node] = struct{}{}
		}
	}
	return sortedNodes(nodes), nil
}

// startPathJobs records and creates one path Job per consuming node. A move
// nothing consumes records an empty list, and needs no paths.
func (r *PersistentVolumeOpsReconciler) startPathJobs(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, h lvol.VolumeHandle,
) error {
	nodes, err := r.relocationConsumers(ctx, ops)
	if err != nil {
		if isConsumerNotReady(err) {
			r.event(ops, corev1.EventTypeNormal, ReasonWaitingForConsumer, "%s", err.Error())
			return nil
		}
		return err
	}
	jobs := make([]simplyblockv1alpha2.ValidationJob, 0, len(nodes))
	for _, node := range nodes {
		jobs = append(jobs, simplyblockv1alpha2.ValidationJob{
			Namespace: subject.namespace(), Name: jobName(modePreconnect, ops.Name, string(ops.UID), node), Node: node,
		})
	}
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Relocate.PathJobs = jobs
	}); err != nil {
		return err
	}
	for _, job := range jobs {
		if err := r.createPathJob(ctx, subject, h, job); err != nil {
			return err
		}
	}
	return nil
}

// createPathJob creates one node's path Job, carrying every path the control
// plane answers for the subsystem while the cutover is pending: the target's
// beside the source's.
func (r *PersistentVolumeOpsReconciler) createPathJob(
	ctx context.Context, subject *subject, h lvol.VolumeHandle, job simplyblockv1alpha2.ValidationJob,
) error {
	conn, err := r.API.Connection(ctx, h)
	if err != nil {
		return fmt.Errorf("paths of volume %s: %w", h, err)
	}
	connections := make([]vmigration.Connection, 0, len(conn.Endpoints))
	for _, e := range conn.Endpoints {
		connections = append(connections, vmigration.Connection{
			NQN: conn.NQN, IP: e.Address, Port: e.Port, Transport: e.Transport,
			NrIoQueues: e.NrIOQueues, ReconnectDelay: e.ReconnectDelaySec,
			CtrlLossTmo:   migrationCtrlLossTimeout,
			FastIOFailTmo: intOr(e.FastIOFailTMOSec), KeepAliveTmo: e.KeepAliveTMOSec,
		})
	}
	encoded, err := json.Marshal(connections)
	if err != nil {
		return err
	}
	placement, err := vmigration.JobPlacementOf(ctx, r.Client, subject.namespace(), subject.clusterUUID)
	if err != nil {
		return err
	}
	built := vmigration.BuildJob(vmigration.JobParams{
		Name:          job.Name,
		Namespace:     job.Namespace,
		Hostname:      job.Node,
		Image:         placement.Image,
		Tolerations:   placement.Tolerations,
		ContainerName: "nvme-preconnect",
		Mode:          modePreconnect,
		Env: []corev1.EnvVar{
			{Name: "REPL_CONNECTIONS", Value: string(encoded)},
			{Name: "VMIG_SYS_ROOT", Value: "/host/sys"},
		},
		BackoffLimit: 1,
		TTL:          jobTTL,
		Deadline:     int64(preconnectJobDeadline.Seconds()),
	})
	if err := r.Create(ctx, built); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create path job %s: %w", job.Name, err)
	}
	return nil
}

func intOr(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// pathJobsPassed reports whether every node holds the target's paths. A Job
// that failed is replaced, and its node holds the cutover until one passes.
func (r *PersistentVolumeOpsReconciler) pathJobsPassed(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, h lvol.VolumeHandle,
) (bool, error) {
	passed := true
	for i, recorded := range ops.Status.Relocate.PathJobs {
		if recorded.Succeeded {
			continue
		}
		var job batchv1.Job
		err := r.Get(ctx, client.ObjectKey{Namespace: recorded.Namespace, Name: recorded.Name}, &job)
		if apierrors.IsNotFound(err) {
			passed = false
			if err := r.createPathJob(ctx, subject, h, recorded); err != nil {
				return false, err
			}
			continue
		}
		if err != nil {
			return false, err
		}
		switch jobOutcome(&job) {
		case jobSucceeded:
			if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
				status.Relocate.PathJobs[i].Succeeded = true
			}); err != nil {
				return false, err
			}
		case jobFailed:
			passed = false
			r.event(ops, corev1.EventTypeWarning, ReasonPathAttachHeld,
				"Node %s could not connect the target's paths; the cutover is held and the Job retried",
				recorded.Node)
			if err := r.Delete(ctx, &job, client.PropagationPolicy("Background")); err != nil &&
				!apierrors.IsNotFound(err) {
				return false, err
			}
		default:
			passed = false
		}
	}
	return passed, nil
}

// proceed releases the volume's cutover while it is pending, and finishes
// once the target serves the volume, however the cutover was released: by
// this call, or by the control plane's own timeout. Proceeded is recorded
// before the first release; the release is resent while the cutover waits.
func (r *PersistentVolumeOpsReconciler) proceed(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (bool, error) {
	i := cuttingOver(ops)
	if i < 0 {
		return true, nil
	}
	v := ops.Status.Relocate.Volumes[i]
	h := sourceHandle(ops, subject, v.VolumeUUID)
	rel, err := r.API.GetVolumeReplicationRelationship(ctx, h)
	if err != nil {
		return false, fmt.Errorf("replication relationship of volume %s: %w", v.VolumeUUID, err)
	}
	if servedByTarget(rel) {
		return r.cutOverPerRelationship(ctx, ops, subject, i)
	}
	if rel.State != controlplane.RelationshipCutoverPending {
		return false, nil
	}
	if !v.Proceeded {
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
			status.Relocate.Volumes[i].Proceeded = true
		}); err != nil {
			return false, err
		}
	}
	if err := r.API.ProceedVolumeCutover(ctx, h); err != nil && !errors.Is(err, errs.ErrNotFound) {
		return false, fmt.Errorf("release the cutover of volume %s: %w", v.VolumeUUID, err)
	}
	return false, nil
}

// verifyRelocation waits for every cutover task, asserts that each moved
// volume's handle resolves to the target, records the new handle on its
// PersistentVolume, and deletes the path Jobs. A move that left consuming
// nodes without the target's paths fails after that, naming them: the volume
// did move, and the driver follows it from its mark, but the pods on those
// nodes lost it.
func (r *PersistentVolumeOpsReconciler) verifyRelocation(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, _ *subject,
) (bool, error) {
	relocate := ops.Status.Relocate
	for _, v := range relocate.Volumes {
		if v.TaskID == "" {
			continue
		}
		task, err := r.API.Task(ctx, relocate.SourceClusterUUID, v.TaskID)
		if errors.Is(err, errs.ErrNotFound) {
			// Finished tasks age out; the relationship already said the
			// target serves the volume.
			continue
		}
		if err != nil {
			return false, fmt.Errorf("task %s: %w", v.TaskID, err)
		}
		if !task.Done() {
			return false, nil
		}
		if task.Result != cutoverDoneResult {
			return false, fatalf("the cutover task of volume %s reported %q", v.VolumeUUID, task.Result)
		}
	}

	for _, v := range relocate.Volumes {
		if v.PersistentVolumeName == "" {
			continue
		}
		if err := r.markRelocated(ctx, ops, v.PersistentVolumeName); err != nil {
			return false, err
		}
	}
	if err := r.deletePathJobs(ctx, ops); err != nil {
		return false, err
	}
	if missing := relocate.NodesMissingPaths; len(missing) > 0 {
		return false, fatalf("%s: the volume moved to member %s, but the control plane cut it over before "+
			"nodes %s held the target's paths; the pods on them lose the volume until they are restarted",
			ReasonCutoverWithoutPaths, ops.Spec.Relocate.TargetClusterRef, strings.Join(missing, ", "))
	}
	return true, nil
}

// markRelocated resolves the PersistentVolume's handle through the control
// plane's relationships and records where it ends, which must be the target.
func (r *PersistentVolumeOpsReconciler) markRelocated(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, name string,
) error {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	start, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !ok {
		return fatalf("volume %s carries an unparsable handle", name)
	}
	if mark, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Annotations[RelocatedToAnnotation])); ok {
		// A volume moved before resolves from its mark.
		start = mark
	}
	sameClient := func(context.Context, string) (MigrationClient, error) { return r.API, nil }
	hops, _, err := controlplane.WalkChain[MigrationClient](ctx, start, r.API, sameClient)
	if err != nil {
		return fmt.Errorf("resolve the handle of volume %s: %w", name, err)
	}
	end := hops[len(hops)-1].Handle
	if end.ClusterID != ops.Status.Relocate.TargetClusterUUID {
		return fatalf("%s: the handle of volume %s resolves to %s, not into member %s",
			ReasonHandleUnresolved, name, end.Handle(), ops.Spec.Relocate.TargetClusterRef)
	}
	if pv.Annotations[RelocatedToAnnotation] == string(end.Handle()) {
		return nil
	}
	patch := client.MergeFromWithOptions(pv.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if pv.Annotations == nil {
		pv.Annotations = map[string]string{}
	}
	pv.Annotations[RelocatedToAnnotation] = string(end.Handle())
	return r.Patch(ctx, &pv, patch)
}

// deletePathJobs deletes the path Jobs. The paths they connected stay: after
// the cutover the target's paths are the volume's data path.
func (r *PersistentVolumeOpsReconciler) deletePathJobs(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) error {
	for _, recorded := range ops.Status.Relocate.PathJobs {
		job := &batchv1.Job{}
		job.Namespace, job.Name = recorded.Namespace, recorded.Name
		if err := r.Delete(ctx, job, client.PropagationPolicy("Background")); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("delete path job %s: %w", recorded.Name, err)
		}
	}
	return nil
}

// discardRelocation takes back a Relocate that ends early: the replication of
// every volume while nothing is committed, and the path Jobs either way. A
// committed move is not stopped; its cutover runs to the end in the control
// plane whatever the operation does.
func (r *PersistentVolumeOpsReconciler) discardRelocation(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) error {
	relocate := ops.Status.Relocate
	if relocate == nil {
		return nil
	}
	if err := r.deletePathJobs(ctx, ops); err != nil {
		return err
	}
	if relocate.Committed || subject == nil {
		return nil
	}
	for _, v := range relocate.Volumes {
		if err := r.API.StopVolumeReplication(ctx, sourceHandle(ops, subject, v.VolumeUUID)); err != nil &&
			!errors.Is(err, errs.ErrNotFound) {
			return fmt.Errorf("stop replicating volume %s: %w", v.VolumeUUID, err)
		}
	}
	return nil
}

// sortVolumes orders the moving volumes by UUID, so the order they are cut
// over in is stable across passes.
func sortVolumes(volumes []simplyblockv1alpha2.RelocatedVolume) {
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].VolumeUUID < volumes[j].VolumeUUID })
}

// sortedNodes is a node set as a sorted list, so the Job set is stable.
func sortedNodes(nodes map[string]struct{}) []string {
	out := make([]string, 0, len(nodes))
	for node := range nodes {
		out = append(out, node)
	}
	sort.Strings(out)
	return out
}

// relocateSuccessor is the step after the current one when the graph
// branches: after a release, the next volume's cutover, or the verification
// once every volume is cut over.
func relocateSuccessor(ops *simplyblockv1alpha2.PersistentVolumeOps, machine *statemachine.Machine[step]) step {
	if machine.CurrentState() == stepProceeding {
		if cuttingOver(ops) >= 0 {
			return stepAwaitingCutover
		}
		return stepVerifying
	}
	return firstSuccessor(machine)
}

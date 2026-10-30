// The migration action of ReplicationOps: a planned online move of volumes to
// the target cluster of a ReplicationPair. It lives apart from the failover and
// failback actions because it is the only action that runs for hours. It hands
// every affected volume to one migration plan in the storage control plane,
// then follows that plan's progress and forecast until each volume has cut
// over. The per-volume cutover itself (preconnect, cutover-proceed) stays with
// the ReplicationSlot reconciler, which reacts to the backend's cutover_pending.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// replOpsPlanPoll is how often a running plan's progress is read back. A
	// migration runs for minutes to hours, and the forecast moves slowly.
	replOpsPlanPoll = 30 * time.Second
	// replOpsWaitPoll is how often the operation re-checks slots it is waiting
	// on before the plan exists: enrollment and the first attach.
	replOpsWaitPoll = 15 * time.Second

	// The persisted sub-phases of a migration. A later pass decides what to do
	// from these and status.planID alone.
	replOpsSubphaseEnrolling           = "Enrolling"
	replOpsSubphaseAwaitingReplication = "AwaitingReplication"
	replOpsSubphaseCreatingPlan        = "CreatingPlan"
	replOpsSubphaseTrackingPlan        = "TrackingPlan"
	// replOpsSubphaseAwaitingCutover marks the per-volume commit path, used
	// when the control plane has no migration plans.
	replOpsSubphaseAwaitingCutover = "AwaitingCutover"

	// Migration plan statuses, as the control plane reports them.
	migrationPlanRunning  = "running"
	migrationPlanDone     = "done"
	migrationPlanPartial  = "partial"
	migrationPlanFailed   = "failed"
	migrationPlanCanceled = "canceled"

	// Per-volume plan states that end a volume's migration.
	migrationVolumeDone     = "done"
	migrationVolumeFailed   = "failed"
	migrationVolumeCanceled = "canceled"
	migrationVolumeSkipped  = "skipped"
)

// migrationPlanRequest is the body of POST .../migration-plans.
type migrationPlanRequest struct {
	TargetClusterID string   `json:"target_cluster_id,omitempty"`
	LvolIDs         []string `json:"lvol_ids"`
	DeleteSource    bool     `json:"delete_source"`
	DryRun          bool     `json:"dry_run,omitempty"`
	// Name identifies the plan as this operation's, so a pass that died
	// between creating the plan and recording its ID adopts it instead of
	// creating a second one.
	Name string `json:"name,omitempty"`
}

// migrationPlanVolume is one volume of a MigrationPlanDTO.
type migrationPlanVolume struct {
	LvolID         string   `json:"lvol_id"`
	Name           string   `json:"name"`
	State          string   `json:"state"`
	TargetNodeID   string   `json:"target_node_id"`
	Error          string   `json:"error"`
	BytesTotal     float64  `json:"bytes_total"`
	BytesRemaining float64  `json:"bytes_remaining"`
	ETASeconds     *float64 `json:"eta_seconds"`
}

// migrationPlanForecast is the forecast block of a MigrationPlanDTO.
type migrationPlanForecast struct {
	BytesRemaining        float64         `json:"bytes_remaining"`
	ETASeconds            *float64        `json:"eta_seconds"`
	CompletesAt           json.RawMessage `json:"completes_at"`
	ThroughputBytesPerSec float64         `json:"throughput_bytes_per_sec"`
	ThroughputMeasured    bool            `json:"throughput_measured"`
	// Converging is absent from a control plane that predates the verdict.
	Converging       *bool    `json:"converging"`
	DirtyBytesPerSec float64  `json:"dirty_bytes_per_sec"`
	NotConvergingLVS []string `json:"not_converging_lvs"`
}

// migrationPlan is the MigrationPlanDTO the control plane returns.
type migrationPlan struct {
	ID       *string                `json:"id"`
	Name     string                 `json:"name"`
	Status   string                 `json:"status"`
	Volumes  []migrationPlanVolume  `json:"volumes"`
	Forecast *migrationPlanForecast `json:"forecast"`
	Warnings []string               `json:"warnings"`
}

// migrationVolume is one volume an operation migrates, with the Kubernetes
// objects it was resolved from.
type migrationVolume struct {
	slot *simplyblockv1alpha1.ReplicationSlot
	// pvcName and slotRef are what the results report: bare names in the
	// operation's namespace, "<namespace>/<name>" for a cluster-scoped
	// migration's claims elsewhere.
	pvcName  string
	slotRef  string
	volumeID string
}

// migrationScope is what an operation resolved its scope to.
type migrationScope struct {
	sourceUUID string
	targetUUID string
	volumes    []migrationVolume
	// skipped are claims a cluster-scoped migration leaves alone, already as
	// results.
	skipped []simplyblockv1alpha1.ReplicationOpsResult
	// enrolling is true while a claim the operation annotated has no slot yet.
	enrolling bool
}

// reconcileMigration drives a planned online migration for scope=policy,
// scope=volume, and scope=cluster. The policy is the ops lock holder: the
// named policy, the slot's policy, or spec.policyRef.
func (r *ReplicationOpsReconciler) reconcileMigration(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, types.NamespacedName{Name: policy.Spec.PairRef, Namespace: policy.Namespace}, &pair); err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("get ReplicationPair %q: %v", policy.Spec.PairRef, err))
	}
	return r.runMigration(ctx, ops, &pair, policy, apiClient)
}

// reconcileTargetMigration drives a migration of every slot on the pair.
func (r *ReplicationOpsReconciler) reconcileTargetMigration(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	return r.runMigration(ctx, ops, pair, nil, apiClient)
}

// runMigration is the migration state machine. Each pass reads where the
// operation is from status (planID, subphase) and does the next step.
func (r *ReplicationOpsReconciler) runMigration(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	pair *simplyblockv1alpha1.ReplicationPair,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	sourceUUID, err := utils.ResolveClusterUUID(ctx, r.Client, ops.Namespace, pair.Spec.SourceCluster)
	if err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	if ops.Status.PlanID != "" {
		return r.trackMigrationPlan(ctx, ops, sourceUUID, apiClient)
	}
	if ops.Status.Subphase == replOpsSubphaseAwaitingCutover {
		return r.trackCommittedSlots(ctx, ops)
	}

	scope, failure, err := r.resolveMigrationScope(ctx, ops, pair, policy, sourceUUID, apiClient)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failure != "" {
		return r.failOps(ctx, ops, failure)
	}

	results := append([]simplyblockv1alpha1.ReplicationOpsResult{}, scope.skipped...)
	var ready []migrationVolume
	waiting := scope.enrolling
	for _, v := range scope.volumes {
		res := simplyblockv1alpha1.ReplicationOpsResult{PVCRef: v.pvcName, SlotRef: v.slotRef}
		if ops.Spec.DryRun {
			// A dry run forecasts the scope as it stands: the plan itself
			// starts replication for a volume that has none yet.
			ready = append(ready, v)
			continue
		}
		if v.slot == nil {
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultPending)
			res.Detail = "enrolled; waiting for its replication slot"
			results = append(results, res)
			waiting = true
			continue
		}
		switch simplyblockv1alpha1.ReplicationSlotState(v.slot.Status.State) {
		case simplyblockv1alpha1.ReplicationSlotStateReplicating:
			ready = append(ready, v)
			continue
		case simplyblockv1alpha1.ReplicationSlotStateCutoverDone:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultSkipped)
			res.Detail = "already cut over"
		case simplyblockv1alpha1.ReplicationSlotStateError:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultFailed)
			res.Detail = fmt.Sprintf("replication slot is in error: %s", v.slot.Status.Message)
		case simplyblockv1alpha1.ReplicationSlotStateFailedOver:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultSkipped)
			res.Detail = "volume is failed over; fail it back before migrating"
		default:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultPending)
			res.Detail = fmt.Sprintf("waiting for replication to start (slot state %q)", v.slot.Status.State)
			waiting = true
		}
		results = append(results, res)
	}

	if waiting && !ops.Spec.DryRun {
		subphase := replOpsSubphaseAwaitingReplication
		if scope.enrolling {
			subphase = replOpsSubphaseEnrolling
		}
		r.patchMigrationStatus(ctx, ops, subphase, "Waiting for every volume to replicate before the plan is created", results, nil, nil)
		return ctrl.Result{RequeueAfter: replOpsWaitPoll}, nil
	}

	if len(ready) == 0 {
		return r.finishMigration(ctx, ops, results, "No volume left to migrate")
	}

	req := migrationPlanRequest{
		TargetClusterID: scope.targetUUID,
		DeleteSource:    ops.Spec.DeleteSource,
		DryRun:          ops.Spec.DryRun,
		Name:            migrationPlanName(ops),
	}
	for _, v := range ready {
		req.LvolIDs = append(req.LvolIDs, v.volumeID)
	}

	if !ops.Spec.DryRun {
		// Write ahead: a pass that dies after the POST finds CreatingPlan and
		// looks for the plan by name before creating another one.
		if ops.Status.Subphase == replOpsSubphaseCreatingPlan {
			if plan := r.findMigrationPlan(ctx, apiClient, sourceUUID, req.Name); plan != nil {
				return r.adoptMigrationPlan(ctx, ops, plan, ready, results)
			}
		}
		r.setSubphase(ctx, ops, replOpsSubphaseCreatingPlan)
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/migration-plans/", sourceUUID)
	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, req)
	if err == nil && status == http.StatusNotFound {
		if ops.Spec.DryRun {
			return r.failOps(ctx, ops, "the storage control plane has no migration plans, so it cannot forecast a migration")
		}
		return r.commitPerVolume(ctx, ops, sourceUUID, ready, results, apiClient)
	}
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		return r.failOps(ctx, ops, fmt.Sprintf("create migration plan: %v", err))
	}

	var plan migrationPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("decode migration plan: %v", err))
	}

	if ops.Spec.DryRun {
		merged := mergePlanResults(results, &plan, ready)
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrationForecast", "MigrationForecast",
			"Dry run: %d volume(s), %s", len(plan.Volumes), describeForecast(plan.Forecast))
		return r.completeMigration(ctx, ops, simplyblockv1alpha1.ReplicationOpsPhaseSucceeded,
			fmt.Sprintf("Dry run: %d volume(s), %s", len(plan.Volumes), describeForecast(plan.Forecast)),
			merged, forecastFromPlan(plan.Forecast), plan.Warnings)
	}
	return r.adoptMigrationPlan(ctx, ops, &plan, ready, results)
}

// adoptMigrationPlan records a created plan as this operation's and mirrors
// its first progress report.
func (r *ReplicationOpsReconciler) adoptMigrationPlan(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	plan *migrationPlan,
	ready []migrationVolume,
	results []simplyblockv1alpha1.ReplicationOpsResult,
) (ctrl.Result, error) {
	if plan.ID == nil || *plan.ID == "" {
		return r.failOps(ctx, ops, "the storage control plane returned a migration plan without an ID")
	}
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.PlanID = *plan.ID
	ops.Status.Subphase = replOpsSubphaseTrackingPlan
	ops.Status.Message = fmt.Sprintf("Migration plan %s created for %d volume(s)", *plan.ID, len(plan.Volumes))
	ops.Status.Results = mergePlanResults(results, plan, ready)
	previous := ops.Status.Forecast
	ops.Status.Forecast = forecastFromPlan(plan.Forecast)
	ops.Status.Warnings = plan.Warnings
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		// The plan exists either way. CreatingPlan stays recorded, so the next
		// pass adopts it by name rather than creating another.
		return ctrl.Result{Requeue: true}, nil
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrationPlanCreated", "MigrationPlanCreated",
		"Migration plan %s: %d volume(s), %s", *plan.ID, len(plan.Volumes), describeForecast(plan.Forecast))
	r.noteConvergence(ops, previous, plan.Forecast)
	return ctrl.Result{RequeueAfter: replOpsPlanPoll}, nil
}

// trackMigrationPlan reads the plan back and mirrors it until it ends.
func (r *ReplicationOpsReconciler) trackMigrationPlan(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	sourceUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/migration-plans/%s", sourceUUID, ops.Status.PlanID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err == nil && status == http.StatusNotFound {
		return r.failOps(ctx, ops, fmt.Sprintf("migration plan %s no longer exists in the storage control plane", ops.Status.PlanID))
	}
	if err != nil || status >= 300 {
		// A transient read failure says nothing about the migration, which
		// keeps running in the control plane: try again later.
		log.Info("Could not read migration plan; retrying", "plan", ops.Status.PlanID, "status", status, "err", err)
		return ctrl.Result{RequeueAfter: replOpsPlanPoll}, nil
	}
	var plan migrationPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		log.Info("Could not decode migration plan; retrying", "plan", ops.Status.PlanID, "err", err)
		return ctrl.Result{RequeueAfter: replOpsPlanPoll}, nil
	}

	slots, err := r.slotsByVolumeID(ctx, slotNamespaceOf(ops))
	if err != nil {
		return ctrl.Result{}, err
	}
	var known []migrationVolume
	for id, slot := range slots {
		known = append(known, migrationVolume{
			slot: slot, volumeID: id,
			pvcName: utils.RefFrom(slot.Namespace, slot.Spec.PVCRef, ops.Namespace),
			slotRef: utils.RefFrom(slot.Namespace, slot.Name, ops.Namespace),
		})
	}
	results := mergePlanResults(keptResults(ops.Status.Results), &plan, known)
	forecast := forecastFromPlan(plan.Forecast)
	r.markMigratedVolumes(ctx, ops, apiClient, results)

	switch plan.Status {
	case migrationPlanDone:
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrationComplete", "MigrationComplete",
			"Migration plan %s complete (%d volume(s))", ops.Status.PlanID, len(plan.Volumes))
		return r.finishMigration(ctx, ops, results, fmt.Sprintf("Migration complete; plan %s", ops.Status.PlanID))
	case migrationPlanPartial, migrationPlanFailed, migrationPlanCanceled:
		return r.completeMigration(ctx, ops, simplyblockv1alpha1.ReplicationOpsPhaseFailed,
			fmt.Sprintf("Migration plan %s ended %s: %d of %d volume(s) did not migrate; see results",
				ops.Status.PlanID, plan.Status, countUnmigrated(&plan), len(plan.Volumes)),
			results, forecast, plan.Warnings)
	default:
		r.noteConvergence(ops, ops.Status.Forecast, plan.Forecast)
		r.patchMigrationStatus(ctx, ops, replOpsSubphaseTrackingPlan,
			fmt.Sprintf("Migrating %d volume(s): %s", len(plan.Volumes), describeForecast(plan.Forecast)),
			results, forecast, plan.Warnings)
		return ctrl.Result{RequeueAfter: replOpsPlanPoll}, nil
	}
}

// commitPerVolume is the path for a control plane without migration plans:
// one commit per volume, as before plans existed, followed by tracking the
// slots until each has cut over.
func (r *ReplicationOpsReconciler) commitPerVolume(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	sourceUUID string,
	ready []migrationVolume,
	results []simplyblockv1alpha1.ReplicationOpsResult,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	anyQueued := false
	for _, v := range ready {
		res := simplyblockv1alpha1.ReplicationOpsResult{PVCRef: v.pvcName, SlotRef: v.slotRef}
		_, poolID, volumeID, ok := splitVolumeHandle(v.slot.Spec.VolumeID)
		if !ok {
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultFailed)
			res.Detail = "invalid VolumeID"
			results = append(results, res)
			continue
		}
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/commit",
			sourceUUID, poolID, volumeID)
		var commitBody interface{}
		if ops.Spec.DeleteSource {
			commitBody = map[string]interface{}{"delete_source": true}
		}
		_, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, commitBody)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d", status)
			}
			log.Error(err, "POST replication/commit failed", "slot", v.slot.Name)
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultFailed)
			res.Detail = fmt.Sprintf("commit failed: %v", err)
			results = append(results, res)
			continue
		}
		// Without a plan nothing reports per-volume progress, and the slot only
		// polls the backend for cutover_pending while it is in that state, so
		// it is put there now, as the commit path always did.
		slotPatch := client.MergeFrom(v.slot.DeepCopy())
		v.slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending)
		v.slot.Status.Message = fmt.Sprintf("Cutover commit queued via ReplicationOps %s", ops.Name)
		if err := r.Status().Patch(ctx, v.slot, slotPatch); err != nil {
			log.Error(err, "failed to update slot status to cutover_pending", "slot", v.slot.Name)
		}
		res.Status = string(simplyblockv1alpha1.ReplicationOpsResultPending)
		res.Detail = "cutover commit queued"
		results = append(results, res)
		anyQueued = true
	}
	if !anyQueued {
		return r.completeMigration(ctx, ops, simplyblockv1alpha1.ReplicationOpsPhaseFailed,
			"Migration commit failed for every volume; see results for details", results, nil, nil)
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrationCommitQueued", "MigrationCommitQueued",
		"Cutover commit queued per volume (the control plane has no migration plans)")
	r.patchMigrationStatus(ctx, ops, replOpsSubphaseAwaitingCutover,
		"Cutover commits queued; waiting for every volume to cut over", results, nil, nil)
	return ctrl.Result{RequeueAfter: replOpsPlanPoll}, nil
}

// trackCommittedSlots follows the per-volume commit path through slot states.
func (r *ReplicationOpsReconciler) trackCommittedSlots(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
) (ctrl.Result, error) {
	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(ops.Status.Results))
	pending := 0
	for _, res := range ops.Status.Results {
		if res.Status != string(simplyblockv1alpha1.ReplicationOpsResultPending) || res.SlotRef == "" {
			results = append(results, res)
			continue
		}
		var slot simplyblockv1alpha1.ReplicationSlot
		slotNS, slotName := utils.SplitRef(res.SlotRef, ops.Namespace)
		err := r.Get(ctx, types.NamespacedName{Name: slotName, Namespace: slotNS}, &slot)
		switch {
		case apierrors.IsNotFound(err):
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultFailed)
			res.Detail = "replication slot was deleted during the migration"
		case err != nil:
			return ctrl.Result{}, fmt.Errorf("get ReplicationSlot %q: %w", res.SlotRef, err)
		case slot.Status.State == string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone):
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultSucceeded)
			res.Detail = ""
			res.State = migrationVolumeDone
			res.TargetLvolID = slot.Status.TargetLvolID
		case slot.Status.State == string(simplyblockv1alpha1.ReplicationSlotStateError):
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultFailed)
			res.Detail = slot.Status.Message
		default:
			pending++
		}
		results = append(results, res)
	}
	if pending > 0 {
		r.patchMigrationStatus(ctx, ops, replOpsSubphaseAwaitingCutover,
			fmt.Sprintf("Waiting for %d volume(s) to cut over", pending), results, nil, nil)
		return ctrl.Result{RequeueAfter: replOpsPlanPoll}, nil
	}
	return r.finishMigration(ctx, ops, results, "Migration complete")
}

// resolveMigrationScope turns the operation's scope into volumes. The returned
// failure string is a terminal reason; the error is a transient one.
func (r *ReplicationOpsReconciler) resolveMigrationScope(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	pair *simplyblockv1alpha1.ReplicationPair,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	sourceUUID string,
	apiClient *webapi.Client,
) (*migrationScope, string, error) {
	log := logf.FromContext(ctx)
	scope := &migrationScope{sourceUUID: sourceUUID}
	if target, err := utils.ResolveClusterUUID(ctx, r.Client, ops.Namespace, pair.Spec.TargetCluster); err == nil {
		scope.targetUUID = target
	} else {
		// The plan can still resolve the target from the volumes' own
		// replication, which already points at it.
		log.Info("Target cluster UUID not resolvable locally; leaving it to the control plane",
			"target", pair.Spec.TargetCluster, "err", err)
	}

	var slots []simplyblockv1alpha1.ReplicationSlot
	var err error
	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopeCluster:
		failure, err := r.resolveClusterScope(ctx, ops, policy, scope, apiClient)
		return scope, failure, err
	case utils.ReplicationOpsScopeTarget:
		slots, err = r.collectSlotsForPair(ctx, pair.Name, ops.Namespace)
	default:
		slots, err = r.collectAffectedSlots(ctx, ops, policy)
	}
	if err != nil {
		return nil, "", err
	}
	for i := range slots {
		slot := &slots[i]
		_, _, volumeID, ok := splitVolumeHandle(slot.Spec.VolumeID)
		if !ok {
			return nil, fmt.Sprintf("invalid VolumeID %q on slot %s", slot.Spec.VolumeID, slot.Name), nil
		}
		scope.volumes = append(scope.volumes, migrationVolume{
			slot: slot, pvcName: slot.Spec.PVCRef, slotRef: slot.Name, volumeID: volumeID})
	}
	return scope, "", nil
}

// resolveClusterScope collects every bound claim, in every namespace, whose
// volume lives on the source cluster, and enrolls those that are not
// replicated yet under spec.policyRef by annotating the claim -- with
// "<namespace>/<name>" when the claim lives outside the policy's namespace.
// The PVC annotation watcher then creates the slot beside the claim, exactly
// as for a claim a user annotated.
func (r *ReplicationOpsReconciler) resolveClusterScope(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	scope *migrationScope,
	apiClient *webapi.Client,
) (string, error) {
	if failure := r.clusterMigrationMismatch(ctx, ops, policy, scope.sourceUUID); failure != "" {
		return failure, nil
	}

	// A workload's claim lives in its own namespace, not the storage one: the
	// scope is every namespace, and a slot is found beside its claim.
	slotByPVC, err := r.slotsByClaim(ctx)
	if err != nil {
		return "", err
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs); err != nil {
		return "", fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	for i := range pvcs.Items {
		claim, err := r.locateClusterClaim(ctx, apiClient, &pvcs.Items[i], slotByPVC, ops.Namespace)
		if err != nil {
			return "", err
		}
		if claim == nil {
			continue
		}
		if err := r.scopeClusterClaim(ctx, ops, policy, scope, claim); err != nil {
			return "", err
		}
	}
	return "", nil
}

// clusterMigrationMismatch is why spec.policyRef cannot drive a cluster
// migration of spec.ref, or "" when it can: the policy must replicate from
// that cluster and be in migration mode.
func (r *ReplicationOpsReconciler) clusterMigrationMismatch(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	sourceUUID string,
) string {
	refUUID, err := utils.ResolveClusterUUID(ctx, r.Client, ops.Namespace, ops.Spec.Ref)
	if err != nil {
		return fmt.Sprintf("resolve StorageCluster %q: %v", ops.Spec.Ref, err)
	}
	if refUUID != sourceUUID {
		return fmt.Sprintf("spec.policyRef %q replicates from cluster %s, not from %q",
			policy.Name, sourceUUID, ops.Spec.Ref)
	}
	if policy.Spec.Mode != utils.ReplicationPolicyModeMigration {
		return fmt.Sprintf("spec.policyRef %q is in mode %q; a cluster migration needs mode %q",
			policy.Name, policy.Spec.Mode, utils.ReplicationPolicyModeMigration)
	}
	return ""
}

// slotsByClaim maps every ReplicationSlot, in every namespace, that names a
// claim, by "<namespace>/<claim>".
func (r *ReplicationOpsReconciler) slotsByClaim(
	ctx context.Context,
) (map[string]*simplyblockv1alpha1.ReplicationSlot, error) {
	var slotList simplyblockv1alpha1.ReplicationSlotList
	if err := r.List(ctx, &slotList); err != nil {
		return nil, fmt.Errorf("list ReplicationSlots: %w", err)
	}
	slotByPVC := make(map[string]*simplyblockv1alpha1.ReplicationSlot, len(slotList.Items))
	for i := range slotList.Items {
		if s := &slotList.Items[i]; s.Spec.PVCRef != "" {
			slotByPVC[s.Namespace+"/"+s.Spec.PVCRef] = s
		}
	}
	return slotByPVC, nil
}

// clusterClaim is a bound claim a cluster-scoped migration considers, with
// where its volume came from and where it lives now.
type clusterClaim struct {
	pvc *corev1.PersistentVolumeClaim
	// pvcRef names the claim as the operation reports it.
	pvcRef string
	// slot is the claim's ReplicationSlot, nil when it has none.
	slot *simplyblockv1alpha1.ReplicationSlot
	// finished is true when that slot completed a migration.
	finished bool
	// originCluster is the cluster the PV's handle, or the slot, names.
	originCluster string
	// clusterID and volumeID locate the volume now.
	clusterID string
	volumeID  string
}

// locateClusterClaim resolves where a claim's volume lives, or returns nil
// for a claim a cluster migration does not consider: unbound, being deleted,
// without a CSI volume, or with a handle that is not a simplyblock one.
//
// "Lives on" is where the volume is now, not what the PV's handle says: the
// handle is fixed at provisioning, and after a completed migration it names
// the cluster the volume left. A claim whose slot finished a migration is
// therefore located through the control plane's relationships, and that
// finished slot does not hold the claim: it is superseded by the new policy.
func (r *ReplicationOpsReconciler) locateClusterClaim(
	ctx context.Context,
	apiClient *webapi.Client,
	pvc *corev1.PersistentVolumeClaim,
	slotByPVC map[string]*simplyblockv1alpha1.ReplicationSlot,
	opsNamespace string,
) (*clusterClaim, error) {
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" || !pvc.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get PersistentVolume %q: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.CSI == nil {
		return nil, nil
	}
	slot := slotByPVC[pvc.Namespace+"/"+pvc.Name]
	origin := pv.Spec.CSI.VolumeHandle
	if slot != nil {
		origin = slot.Spec.VolumeID
	}
	originCluster, _, _, ok := splitVolumeHandle(origin)
	if !ok {
		return nil, nil
	}
	// An attached slot already addresses the current volume (the slot
	// reconciler resolves it at attach); anything else is located from
	// its handle.
	current := origin
	finished := slot != nil &&
		slot.Status.State == string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone)
	if slot == nil || slot.Status.State == "" || finished {
		var err error
		if current, err = currentVolumeHandle(ctx, apiClient, origin); err != nil {
			return nil, fmt.Errorf("locate the volume of claim %s/%s: %w", pvc.Namespace, pvc.Name, err)
		}
	}
	clusterID, _, volumeID, _ := splitVolumeHandle(current)
	return &clusterClaim{
		pvc:           pvc,
		pvcRef:        utils.RefFrom(pvc.Namespace, pvc.Name, opsNamespace),
		slot:          slot,
		finished:      finished,
		originCluster: originCluster,
		clusterID:     clusterID,
		volumeID:      volumeID,
	}, nil
}

// skipClaim records a claim the cluster migration leaves alone, and why.
func (s *migrationScope) skipClaim(pvcRef, detail string) {
	s.skipped = append(s.skipped, simplyblockv1alpha1.ReplicationOpsResult{
		PVCRef: pvcRef, Status: string(simplyblockv1alpha1.ReplicationOpsResultSkipped), Detail: detail,
	})
}

// scopeClusterClaim classifies one located claim: a volume that left the
// source cluster is reported as moved, a live slot under another policy is
// skipped, a live slot under spec.policyRef joins the scope as it is, and
// any other claim is enrolled.
func (r *ReplicationOpsReconciler) scopeClusterClaim(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	scope *migrationScope,
	claim *clusterClaim,
) error {
	if claim.clusterID != scope.sourceUUID {
		if claim.originCluster == scope.sourceUUID {
			scope.skipClaim(claim.pvcRef, fmt.Sprintf("moved to cluster %s", claim.clusterID))
		}
		return nil
	}
	if slot := claim.slot; slot != nil && !claim.finished {
		if utils.CanonicalRef(slot.Spec.PolicyRef, slot.Namespace) != policy.Namespace+"/"+policy.Name {
			scope.skipClaim(claim.pvcRef, fmt.Sprintf("follows replication policy %q", slot.Spec.PolicyRef))
			return nil
		}
		scope.volumes = append(scope.volumes, migrationVolume{
			slot: slot, pvcName: claim.pvcRef, volumeID: claim.volumeID,
			slotRef: utils.RefFrom(slot.Namespace, slot.Name, ops.Namespace),
		})
		return nil
	}
	return r.enrollClusterClaim(ctx, ops, policy, scope, claim)
}

// enrollClusterClaim brings a claim without a live slot under spec.policyRef:
// it joins the scope and, unless this is a dry run, the claim is annotated
// with the policy (or a finished slot under that same policy is retired) so
// the PVC annotation watcher creates its slot.
func (r *ReplicationOpsReconciler) enrollClusterClaim(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	scope *migrationScope,
	claim *clusterClaim,
) error {
	pvc, slot := claim.pvc, claim.slot
	policyRef := policy.Namespace + "/" + policy.Name
	assigned, err := r.assignedPolicy(ctx, pvc)
	if err != nil {
		return err
	}
	// The policy a finished migration left on the claim is spent: the volume
	// has arrived, and that policy replicates from where it no longer is.
	if assigned != "" && utils.CanonicalRef(assigned, pvc.Namespace) != policyRef && !claim.finished {
		scope.skipClaim(claim.pvcRef, fmt.Sprintf("assigned replication policy %q", assigned))
		return nil
	}
	scope.volumes = append(scope.volumes, migrationVolume{pvcName: claim.pvcRef, volumeID: claim.volumeID})
	if ops.Spec.DryRun {
		return nil
	}
	scope.enrolling = true
	if claim.finished && utils.CanonicalRef(slot.Spec.PolicyRef, slot.Namespace) == policyRef {
		// Back under the policy it finished with (A -> B -> A on one pair):
		// the slot's name is taken by the finished one, so it goes, and the
		// watcher creates a fresh one for the annotation that stays.
		if err := r.Delete(ctx, slot); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("retire finished ReplicationSlot %q: %w", slot.Name, err)
		}
		return nil
	}
	if assigned == "" || utils.CanonicalRef(assigned, pvc.Namespace) != policyRef {
		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Annotations == nil {
			pvc.Annotations = map[string]string{}
		}
		pvc.Annotations[utils.AnnotationReplicationPolicy] = utils.RefFrom(policy.Namespace, policy.Name, pvc.Namespace)
		if err := r.Patch(ctx, pvc, patch); err != nil {
			return fmt.Errorf("annotate PersistentVolumeClaim %s: %w", claim.pvcRef, err)
		}
	}
	return nil
}

// assignedPolicy is the replication policy a claim follows by annotation: its
// own, else its StorageClass's.
func (r *ReplicationOpsReconciler) assignedPolicy(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
	if p := pvc.Annotations[utils.AnnotationReplicationPolicy]; p != "" {
		return p, nil
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "", nil
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get StorageClass %q: %w", *pvc.Spec.StorageClassName, err)
	}
	return sc.Annotations[utils.AnnotationReplicationPolicy], nil
}

// findMigrationPlan returns the plan this operation created, by name, or nil.
func (r *ReplicationOpsReconciler) findMigrationPlan(
	ctx context.Context,
	apiClient *webapi.Client,
	sourceUUID, name string,
) *migrationPlan {
	body, status, err := apiClient.Do(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2/clusters/%s/migration-plans/", sourceUUID), nil)
	if err != nil || status >= 300 {
		return nil
	}
	var plans []migrationPlan
	if err := json.Unmarshal(body, &plans); err != nil {
		return nil
	}
	for i := range plans {
		if plans[i].Name == name && plans[i].ID != nil {
			return &plans[i]
		}
	}
	return nil
}

// slotNamespaceOf is where an operation's slots live: every namespace for a
// cluster-scoped migration, the operation's own otherwise.
func slotNamespaceOf(ops *simplyblockv1alpha1.ReplicationOps) string {
	if ops.Spec.Scope == utils.ReplicationOpsScopeCluster {
		return ""
	}
	return ops.Namespace
}

// slotsByVolumeID maps each slot in the namespace ("" for every namespace) by
// its lvol UUID.
func (r *ReplicationOpsReconciler) slotsByVolumeID(
	ctx context.Context,
	namespace string,
) (map[string]*simplyblockv1alpha1.ReplicationSlot, error) {
	var list simplyblockv1alpha1.ReplicationSlotList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list ReplicationSlots: %w", err)
	}
	out := make(map[string]*simplyblockv1alpha1.ReplicationSlot, len(list.Items))
	for i := range list.Items {
		if _, _, id, ok := splitVolumeHandle(list.Items[i].Spec.VolumeID); ok {
			out[id] = &list.Items[i]
		}
	}
	return out, nil
}

// patchMigrationStatus writes progress without ending the operation.
func (r *ReplicationOpsReconciler) patchMigrationStatus(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	subphase, message string,
	results []simplyblockv1alpha1.ReplicationOpsResult,
	forecast *simplyblockv1alpha1.ReplicationOpsForecast,
	warnings []string,
) {
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Subphase = subphase
	ops.Status.Message = message
	ops.Status.Results = results
	if forecast != nil {
		ops.Status.Forecast = forecast
	}
	if warnings != nil {
		ops.Status.Warnings = warnings
	}
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		logf.FromContext(ctx).Error(err, "failed to record migration progress", "ops", ops.Name)
	}
}

// finishMigration ends the operation: Succeeded when no volume failed, and
// Failed, with the results kept, when any did.
func (r *ReplicationOpsReconciler) finishMigration(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	results []simplyblockv1alpha1.ReplicationOpsResult,
	message string,
) (ctrl.Result, error) {
	failed := 0
	for _, res := range results {
		if res.Status == string(simplyblockv1alpha1.ReplicationOpsResultFailed) {
			failed++
		}
	}
	if failed > 0 {
		return r.completeMigration(ctx, ops, simplyblockv1alpha1.ReplicationOpsPhaseFailed,
			fmt.Sprintf("%d volume(s) did not migrate; see results", failed), results, ops.Status.Forecast, nil)
	}
	return r.completeMigration(ctx, ops, simplyblockv1alpha1.ReplicationOpsPhaseSucceeded, message, results, ops.Status.Forecast, nil)
}

// completeMigration writes the terminal phase together with the results and
// forecast, which succeedOps and failOps do not carry, then releases the lock.
func (r *ReplicationOpsReconciler) completeMigration(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	phase simplyblockv1alpha1.ReplicationOpsPhase,
	message string,
	results []simplyblockv1alpha1.ReplicationOpsResult,
	forecast *simplyblockv1alpha1.ReplicationOpsForecast,
	warnings []string,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	if warnings != nil {
		ops.Status.Warnings = warnings
	}
	ops.Status.Phase = string(phase)
	ops.Status.Subphase = ""
	ops.Status.Message = message
	ops.Status.CompletedAt = &now
	ops.Status.Results = results
	ops.Status.Forecast = forecast
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	if phase == simplyblockv1alpha1.ReplicationOpsPhaseFailed {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "Failed", "Failed",
			"ReplicationOps %s failed: %s", ops.Name, message)
	}
	r.releaseLock(ctx, ops)
	return ctrl.Result{}, nil
}

// migrationPlanName is the name a plan carries so that it can be found again.
func migrationPlanName(ops *simplyblockv1alpha1.ReplicationOps) string {
	return fmt.Sprintf("replicationops/%s/%s/%s", ops.Namespace, ops.Name, ops.UID)
}

// keptResults are the results a plan does not report: claims the operation
// skipped before the plan existed.
func keptResults(results []simplyblockv1alpha1.ReplicationOpsResult) []simplyblockv1alpha1.ReplicationOpsResult {
	var kept []simplyblockv1alpha1.ReplicationOpsResult
	for _, res := range results {
		if res.SlotRef == "" && res.Status == string(simplyblockv1alpha1.ReplicationOpsResultSkipped) {
			kept = append(kept, res)
		}
	}
	return kept
}

// mergePlanResults is the results list for a plan: the results the plan does
// not cover, then one per plan volume, named by its slot and claim where known.
func mergePlanResults(
	base []simplyblockv1alpha1.ReplicationOpsResult,
	plan *migrationPlan,
	volumes []migrationVolume,
) []simplyblockv1alpha1.ReplicationOpsResult {
	byID := make(map[string]migrationVolume, len(volumes))
	for _, v := range volumes {
		byID[v.volumeID] = v
	}
	inPlan := make(map[string]bool, len(plan.Volumes))
	for _, pv := range plan.Volumes {
		if v, ok := byID[pv.LvolID]; ok {
			if v.slotRef != "" {
				inPlan[v.slotRef] = true
			}
			inPlan["pvc/"+v.pvcName] = true
		}
	}
	out := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(base)+len(plan.Volumes))
	for _, res := range base {
		// A volume waiting for its slot before the plan existed is reported by
		// the plan now.
		if inPlan[res.SlotRef] || (res.PVCRef != "" && inPlan["pvc/"+res.PVCRef]) {
			continue
		}
		out = append(out, res)
	}
	for _, pv := range plan.Volumes {
		res := simplyblockv1alpha1.ReplicationOpsResult{
			State:          pv.State,
			TargetNodeID:   pv.TargetNodeID,
			Detail:         pv.Error,
			BytesRemaining: int64(math.Round(pv.BytesRemaining)),
		}
		if pv.ETASeconds != nil {
			res.ETASeconds = int64(math.Ceil(*pv.ETASeconds))
		}
		if v, ok := byID[pv.LvolID]; ok {
			res.PVCRef = v.pvcName
			res.SlotRef = v.slotRef
		} else if res.Detail == "" {
			res.Detail = "volume " + pv.LvolID
		}
		switch pv.State {
		case migrationVolumeDone:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultSucceeded)
		case migrationVolumeFailed, migrationVolumeCanceled:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultFailed)
		case migrationVolumeSkipped:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultSkipped)
		default:
			res.Status = string(simplyblockv1alpha1.ReplicationOpsResultPending)
		}
		out = append(out, res)
	}
	return out
}

// countUnmigrated counts the plan volumes that did not reach done.
func countUnmigrated(plan *migrationPlan) int {
	n := 0
	for _, v := range plan.Volumes {
		if v.State != migrationVolumeDone {
			n++
		}
	}
	return n
}

// forecastFromPlan converts the plan's forecast into status form.
func forecastFromPlan(f *migrationPlanForecast) *simplyblockv1alpha1.ReplicationOpsForecast {
	if f == nil {
		return nil
	}
	out := &simplyblockv1alpha1.ReplicationOpsForecast{
		BytesRemaining:        int64(math.Round(f.BytesRemaining)),
		ThroughputBytesPerSec: int64(math.Round(f.ThroughputBytesPerSec)),
		ThroughputMeasured:    f.ThroughputMeasured,
		DirtyBytesPerSec:      int64(math.Round(f.DirtyBytesPerSec)),
		NotConvergingLVS:      f.NotConvergingLVS,
	}
	if f.Converging != nil {
		converging := *f.Converging
		out.Converging = &converging
	}
	if f.ETASeconds != nil && !notConverging(f) {
		out.ETASeconds = int64(math.Ceil(*f.ETASeconds))
	}
	if t := parsePlanTime(f.CompletesAt); t != nil && !notConverging(f) {
		out.CompletesAt = &metav1.Time{Time: *t}
	}
	return out
}

// notConverging is the control plane's verdict that the clients write some
// source lvstore at least as fast as it replicates. Unknown counts as no.
func notConverging(f *migrationPlanForecast) bool {
	return f != nil && f.Converging != nil && !*f.Converging
}

// noteConvergence emits one event per change of the convergence verdict: a
// warning when the lag stops being able to close, and a normal event when it
// can again. A verdict that stays the same is not repeated on every poll.
func (r *ReplicationOpsReconciler) noteConvergence(
	ops *simplyblockv1alpha1.ReplicationOps,
	previous *simplyblockv1alpha1.ReplicationOpsForecast,
	next *migrationPlanForecast,
) {
	if next == nil || next.Converging == nil {
		return
	}
	wasNotConverging := previous != nil && previous.Converging != nil && !*previous.Converging
	switch {
	case !*next.Converging && !wasNotConverging:
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "NotConverging", "NotConverging",
			"%s", describeForecast(next))
	case *next.Converging && wasNotConverging:
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "Converging", "Converging",
			"Replication outruns the clients' writes again: %s", describeForecast(next))
	}
}

// formatRate is a byte rate for messages, in MiB/s.
func formatRate(bytesPerSec float64) string {
	return fmt.Sprintf("%.1f MiB/s", bytesPerSec/(1<<20))
}

// parsePlanTime reads a timestamp the control plane wrote either as RFC 3339
// text or as epoch seconds.
func parsePlanTime(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return &t
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			t := time.Unix(int64(f), 0).UTC()
			return &t
		}
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f <= 0 {
			// The control plane writes 0 for "no completion time".
			return nil
		}
		t := time.Unix(int64(f), 0).UTC()
		return &t
	}
	return nil
}

// describeForecast is the one-line forecast used in messages and events.
func describeForecast(f *migrationPlanForecast) string {
	if notConverging(f) {
		return fmt.Sprintf("Not converging: clients write %s, faster than lvstore(s) %s replicate; "+
			"no cutover can start until the writes drop below the replication throughput",
			formatRate(f.DirtyBytesPerSec), strings.Join(f.NotConvergingLVS, ", "))
	}
	if f == nil || f.ETASeconds == nil {
		return "no forecast yet"
	}
	basis := "measured throughput"
	if !f.ThroughputMeasured {
		basis = "assumed throughput"
	}
	return fmt.Sprintf("%.1f GiB to copy, about %s left (%s)",
		f.BytesRemaining/(1<<30), (time.Duration(*f.ETASeconds) * time.Second).Round(time.Second), basis)
}

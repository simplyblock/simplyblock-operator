package controller

// The trigger and snapshot actions of ReplicationOps. A trigger takes a
// snapshot of each volume now and replicates it, outside the policy's interval
// (sbctl volume replication-trigger). A snapshot takes one crash-consistent
// generation of a consistency-group policy's volumes now (sbctl cluster
// replication-policy-snapshot), through the group the policy owns on the
// pair's source cluster.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	replicationOpsActionTrigger  = "trigger"
	replicationOpsActionSnapshot = "snapshot"
)

// sourceClusterUUID is the backend cluster of the pair's source, which owns the
// replicated volumes and their consistency groups.
func (r *ReplicationOpsReconciler) sourceClusterUUID(
	ctx context.Context, namespace, pairName string,
) (string, error) {
	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, types.NamespacedName{Name: pairName, Namespace: namespace}, &pair); err != nil {
		return "", fmt.Errorf("get ReplicationPair %q: %w", pairName, err)
	}
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, namespace, pair.Spec.SourceCluster)
	if err != nil {
		return "", fmt.Errorf("resolve cluster UUID: %w", err)
	}
	return clusterUUID, nil
}

// reconcileTrigger replicates every slot's volume now. A trigger takes a new
// snapshot per call, so each outcome is written to status as it happens, and a
// slot status already records is not triggered again when a reconcile resumes.
func (r *ReplicationOpsReconciler) reconcileTrigger(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	slots []simplyblockv1alpha1.ReplicationSlot,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	r.setSubphase(ctx, ops, "TriggeringReplication")

	recorded := map[string]bool{}
	for _, res := range ops.Status.Results {
		recorded[res.SlotRef] = true
	}
	anyFailed := false
	for _, res := range ops.Status.Results {
		anyFailed = anyFailed || res.Status == string(simplyblockv1alpha1.ReplicationOpsResultFailed)
	}

	for i := range slots {
		slot := &slots[i]
		if recorded[slot.Name] {
			continue
		}
		result := simplyblockv1alpha1.ReplicationOpsResult{
			SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
		}
		if _, poolID, volumeID, ok := splitVolumeHandle(slot.Spec.VolumeID); !ok {
			result.Status, result.Detail = string(simplyblockv1alpha1.ReplicationOpsResultFailed), "invalid VolumeID"
		} else {
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/trigger",
				clusterUUID, poolID, volumeID)
			_, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
			if err == nil && status >= 300 {
				err = fmt.Errorf("status %d", status)
			}
			if err != nil {
				result.Status = string(simplyblockv1alpha1.ReplicationOpsResultFailed)
				result.Detail = fmt.Sprintf("trigger failed: %v", err)
			}
		}
		anyFailed = anyFailed || result.Status == string(simplyblockv1alpha1.ReplicationOpsResultFailed)

		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Results = append(ops.Status.Results, result)
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("record the trigger of %s: %w", slot.Name, err)
		}
	}

	if anyFailed {
		return r.failOps(ctx, ops, "Replication trigger failed for one or more volumes; see results for details")
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "ReplicationTriggered", "ReplicationTriggered",
		"Replication triggered for %d volumes", len(ops.Status.Results))
	return r.succeedOps(ctx, ops, "Replication triggered; each volume replicates a new snapshot", ops.Status.Results)
}

// consistencyGroupRef is the part of a control-plane consistency group the
// snapshot action reads.
type consistencyGroupRef struct {
	ID       string  `json:"id"`
	PolicyID *string `json:"policy_id"`
}

// reconcileSnapshot takes one generation of each policy's consistency group.
// The take carries the operation's UID as its request id, so a retried take
// returns the generation the first one took rather than taking another. With
// requireGroup a policy that has no group fails the operation. Without it (scope
// target, over every policy of a pair) such a policy is skipped.
func (r *ReplicationOpsReconciler) reconcileSnapshot(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policies []simplyblockv1alpha1.ReplicationPolicy,
	clusterUUID string,
	requireGroup bool,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	r.setSubphase(ctx, ops, "TakingGeneration")

	groupsPath := fmt.Sprintf("/api/v2/clusters/%s/consistency-groups/", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, groupsPath, nil)
	if err == nil && status >= 300 {
		err = fmt.Errorf("status %d", status)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list the consistency groups of cluster %s: %w", clusterUUID, err)
	}
	var groups []consistencyGroupRef
	if err := json.Unmarshal(body, &groups); err != nil {
		return ctrl.Result{}, fmt.Errorf("decode the consistency groups of cluster %s: %w", clusterUUID, err)
	}
	groupOf := map[string]string{}
	for _, g := range groups {
		if g.PolicyID != nil && *g.PolicyID != "" {
			groupOf[*g.PolicyID] = g.ID
		}
	}

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(policies))
	anyFailed := false
	for i := range policies {
		policy := &policies[i]
		result := simplyblockv1alpha1.ReplicationOpsResult{PolicyRef: policy.Name}
		groupID := groupOf[policy.Status.BackendPolicyID]
		switch {
		case policy.Status.BackendPolicyID == "":
			result.Status, result.Detail = string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				"the policy has no backend policy yet"
		case groupID == "" && requireGroup:
			return r.failOps(ctx, ops, fmt.Sprintf(
				"ReplicationPolicy %s has no consistency group; a snapshot takes a generation of one", policy.Name))
		case groupID == "":
			result.Status, result.Detail = string(simplyblockv1alpha1.ReplicationOpsResultSkipped),
				"the policy has no consistency group"
		default:
			seq, err := takeGeneration(ctx, apiClient, groupsPath+groupID+"/snapshots", string(ops.UID))
			if err != nil {
				result.Status, result.Detail = string(simplyblockv1alpha1.ReplicationOpsResultFailed),
					fmt.Sprintf("snapshot of group %s failed: %v", groupID, err)
			} else {
				result.Status, result.Generation = string(simplyblockv1alpha1.ReplicationOpsResultSucceeded), seq
				result.Detail = fmt.Sprintf("generation %d of group %s", seq, groupID)
			}
		}
		anyFailed = anyFailed || result.Status == string(simplyblockv1alpha1.ReplicationOpsResultFailed)
		results = append(results, result)
	}

	if anyFailed {
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Results = results
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("record the snapshot results: %w", err)
		}
		return r.failOps(ctx, ops, "Consistency-group snapshot failed for one or more policies; see results for details")
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "GenerationTaken", "GenerationTaken",
		"Consistency-group snapshot taken for %d policies", len(results))
	return r.succeedOps(ctx, ops, "Consistency-group generation taken", results)
}

// takeGeneration posts one generation take and returns its sequence number.
func takeGeneration(ctx context.Context, apiClient *webapi.Client, endpoint, requestID string) (int64, error) {
	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, map[string]string{"request_id": requestID})
	if err == nil && status >= 300 {
		err = fmt.Errorf("status %d: %s", status, body)
	}
	if err != nil {
		return 0, err
	}
	var generation struct {
		GroupSeq int64 `json:"group_seq"`
	}
	if err := json.Unmarshal(body, &generation); err != nil {
		return 0, fmt.Errorf("decode the generation: %w", err)
	}
	return generation.GroupSeq, nil
}

// policiesOfPair lists the ReplicationPolicies that use the pair.
func (r *ReplicationOpsReconciler) policiesOfPair(
	ctx context.Context, namespace, pairName string,
) ([]simplyblockv1alpha1.ReplicationPolicy, error) {
	var policies simplyblockv1alpha1.ReplicationPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(namespace),
		client.MatchingFields{"spec.pairRef": pairName}); err != nil {
		return nil, fmt.Errorf("list policies for pair %q: %w", pairName, err)
	}
	return policies.Items, nil
}

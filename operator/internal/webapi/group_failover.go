// Group test-failover reads: the control-plane calls the TestFailover controller
// makes to recover a whole consistency group. A group drill needs three things
// the per-volume path does not: the group's replication policy (the group form
// of the recovery point is keyed on the policy, not the group), the one
// group-consistent generation of replicated snapshots on the target, and each
// member volume's K8s identity (PVC name and namespace) so the recovered PVCs
// can be named and their source PVs read for staging metadata.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// GroupReplicationPolicy is the subset of a replication policy the group drill
// needs. A consistency-group policy is matched to its group by placement
// (group_lvs_name and group_node_id), because the policy DTO does not carry the
// group id.
type GroupReplicationPolicy struct {
	ID               string `json:"id"`
	ConsistencyGroup bool   `json:"consistency_group"`
	GroupLvsName     string `json:"group_lvs_name"`
	GroupNodeID      string `json:"group_node_id"`
}

// ResolveGroupPolicyID returns the id of the consistency-group replication
// policy whose group placement matches (lvsName, nodeID), or "" when none does.
// The group's own lvs_name/node_id come from the ConsistencyGroup record; a
// policy is the group's when both agree and it declares a consistency group.
func (c *Client) ResolveGroupPolicyID(
	ctx context.Context,
	clusterUUID, lvsName, nodeID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/policies/", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("list replication policies: %w", err)
	}
	if statusCode >= 300 {
		return "", fmt.Errorf("list replication policies: status %d: %s", statusCode, string(body))
	}
	var policies []GroupReplicationPolicy
	if err := json.Unmarshal(body, &policies); err != nil {
		return "", fmt.Errorf("unmarshal replication policies: %w", err)
	}
	for i := range policies {
		p := policies[i]
		if p.ConsistencyGroup && p.GroupLvsName == lvsName && p.GroupNodeID == nodeID {
			return p.ID, nil
		}
	}
	return "", nil
}

// ReplicatedGroupSnapshot is one member's replicated snapshot on the target
// cluster, at one group-consistent generation. It is the cloneable point for
// that member: cluster and pool address the target backend, snapshot is the
// snapshot to clone, and size sizes the recovered PVC.
type ReplicatedGroupSnapshot struct {
	SnapshotID string `json:"snapshot_id"`
	ClusterID  string `json:"cluster_id"`
	PoolID     string `json:"pool_id"`
	LvolID     string `json:"lvol_id"`
	Size       int64  `json:"size"`
	GroupSeq   int    `json:"group_seq"`
}

// latestGenerationResponse is the group form of latest-snapshot: one generation
// number and one replicated snapshot per current member, all on the target.
type latestGenerationResponse struct {
	GroupSeq int                       `json:"group_seq"`
	Members  []ReplicatedGroupSnapshot `json:"members"`
}

// LatestReplicatedGeneration resolves the latest group-consistent generation on
// the target for a policy, returning the generation and one replicated snapshot
// per member. The control plane refuses (400) when no generation is complete for
// every member or when members straddle generations, which is what makes the
// recovered set crash-consistent; found is false only when there is no
// generation yet (nothing has replicated).
func (c *Client) LatestReplicatedGeneration(
	ctx context.Context,
	clusterUUID, policyID string,
) (groupSeq int, members []ReplicatedGroupSnapshot, found bool, err error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/policies/%s/latest-generation", clusterUUID, policyID)
	body, statusCode, doErr := c.Do(ctx, http.MethodGet, endpoint, nil)
	if statusCode == http.StatusNotFound {
		return 0, nil, false, nil
	}
	if doErr != nil {
		return 0, nil, false, fmt.Errorf("resolve latest replicated generation: %w", doErr)
	}
	if statusCode >= 300 {
		return 0, nil, false, fmt.Errorf("resolve latest replicated generation: status %d: %s", statusCode, string(body))
	}
	var dto latestGenerationResponse
	if err := json.Unmarshal(body, &dto); err != nil {
		return 0, nil, false, fmt.Errorf("decode latest-generation: %w", err)
	}
	return dto.GroupSeq, dto.Members, len(dto.Members) > 0, nil
}

// MemberVolume is a group member's source volume, resolved to the K8s identity
// the drill needs: the PVC name and namespace it was provisioned for, so the
// recovered PVC can be named and the source PV read for staging metadata.
type MemberVolume struct {
	LvolID    string `json:"id"`
	PVCName   string `json:"pvc_name"`
	Namespace string `json:"namespace"`
	PoolID    string `json:"pool_id"`
	Size      int64  `json:"size"`
}

// ResolveMemberVolumes maps each of the given member lvol ids to its source
// volume's K8s identity on the source cluster. A group member carries only an
// lvol id, so the pool is not known up front; this enumerates the cluster's
// pools and their volumes once and matches. Members it cannot find are omitted
// from the result, so the caller can tell an incomplete resolution from a
// complete one by the map size.
func (c *Client) ResolveMemberVolumes(
	ctx context.Context,
	clusterUUID string,
	lvolIDs []string,
) (map[string]MemberVolume, error) {
	want := make(map[string]struct{}, len(lvolIDs))
	for _, id := range lvolIDs {
		want[id] = struct{}{}
	}

	poolsEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, poolsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list storage pools: status %d: %s", statusCode, string(body))
	}
	var pools []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("unmarshal storage pools: %w", err)
	}

	found := make(map[string]MemberVolume, len(lvolIDs))
	for _, pool := range pools {
		if len(found) == len(want) {
			break
		}
		volsEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes", clusterUUID, pool.ID)
		vbody, vstatus, verr := c.Do(ctx, http.MethodGet, volsEndpoint, nil)
		if verr != nil {
			return nil, fmt.Errorf("list volumes in pool %s: %w", pool.ID, verr)
		}
		if vstatus >= 300 {
			return nil, fmt.Errorf("list volumes in pool %s: status %d: %s", pool.ID, vstatus, string(vbody))
		}
		var vols []MemberVolume
		if err := json.Unmarshal(vbody, &vols); err != nil {
			return nil, fmt.Errorf("unmarshal volumes in pool %s: %w", pool.ID, err)
		}
		for i := range vols {
			v := vols[i]
			if _, ok := want[v.LvolID]; !ok {
				continue
			}
			if v.PoolID == "" {
				v.PoolID = pool.ID
			}
			found[v.LvolID] = v
		}
	}
	return found, nil
}

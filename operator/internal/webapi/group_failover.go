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
	"strings"
)

// storagePoolsListPathFmt is the cluster-scoped storage-pools list endpoint.
const storagePoolsListPathFmt = "/api/v2/clusters/%s/storage-pools/"

// ReplicatedGroupSnapshot is one member's replicated snapshot on the target
// cluster, at one group-consistent generation. It is the cloneable point for
// that member: cluster and pool address the target backend, snapshot is the
// snapshot to clone, and size sizes the recovered PVC. LvolID is the replica
// volume on the TARGET the snapshot belongs to, not the source member;
// SourceLvolID is the source member whose data it holds, empty on a control
// plane that does not report it yet.
type ReplicatedGroupSnapshot struct {
	SnapshotID   string `json:"snapshot_id"`
	ClusterID    string `json:"cluster_id"`
	PoolID       string `json:"pool_id"`
	LvolID       string `json:"lvol_id"`
	SourceLvolID string `json:"source_lvol_id"`
	Size         int64  `json:"size"`
	GroupSeq     int    `json:"group_seq"`
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
//
// The control plane stores the PVC identity in one field as "namespace/name" and
// uses the separate "namespace" field for the NVMe namespace, not the K8s one, so
// the K8s namespace and name are split out of PVCRef rather than read from the
// volume's namespace field.
type MemberVolume struct {
	LvolID       string
	PVCName      string
	PVCNamespace string
	PoolID       string
	Size         int64
}

// memberVolumeDTO is the wire shape ResolveMemberVolumes decodes before splitting
// the namespaced PVC reference into a namespace and a name.
type memberVolumeDTO struct {
	LvolID string `json:"id"`
	PVCRef string `json:"pvc_name"`
	PoolID string `json:"pool_id"`
	Size   int64  `json:"size"`
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

	poolsEndpoint := fmt.Sprintf(storagePoolsListPathFmt, clusterUUID)
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
		var vols []memberVolumeDTO
		if err := json.Unmarshal(vbody, &vols); err != nil {
			return nil, fmt.Errorf("unmarshal volumes in pool %s: %w", pool.ID, err)
		}
		for i := range vols {
			v := vols[i]
			if _, ok := want[v.LvolID]; !ok {
				continue
			}
			ns, name := splitPVCRef(v.PVCRef)
			poolID := v.PoolID
			if poolID == "" {
				poolID = pool.ID
			}
			found[v.LvolID] = MemberVolume{
				LvolID:       v.LvolID,
				PVCName:      name,
				PVCNamespace: ns,
				PoolID:       poolID,
				Size:         v.Size,
			}
		}
	}
	return found, nil
}

// splitPVCRef splits a "namespace/name" PVC reference into its namespace and
// name. A reference with no slash is taken as a bare name in no namespace.
func splitPVCRef(ref string) (namespace, name string) {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

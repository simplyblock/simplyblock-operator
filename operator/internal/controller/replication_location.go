// Where a replicated volume lives now, as opposed to where its PersistentVolume
// says it was provisioned.
//
// A PV's volume handle is immutable, so after a completed migration it still
// names the source cluster and the source volume, which --delete-source has
// removed. Everything that addresses the volume through the control plane --
// a new ReplicationSlot attaching a policy, a cluster-scoped migration choosing
// its volumes -- needs the volume that serves the data today. The walk itself
// is atlas-lib's (controlplane.ActiveVolume); this file only feeds it the
// relationship reads through the operator's own control-plane client.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// relationshipResponse is the part of
// GET /api/v2/clusters/{c}/replication/relationships/{vol} the walk reads.
type relationshipResponse struct {
	IsSource        bool   `json:"is_source"`
	SourceClusterID string `json:"source_cluster_id"`
	SourceLvolID    string `json:"source_lvol_id"`
	TargetClusterID string `json:"target_cluster_id"`
	TargetPoolID    string `json:"target_pool_id"`
	TargetLvolID    string `json:"target_lvol_id"`
	ActiveLvolID    string `json:"active_lvol_id"`
}

// relationshipLookup reads relationships through apiClient. The endpoint is
// the cluster-scoped one, which answers for a volume that no longer exists.
func relationshipLookup(apiClient *webapi.Client) controlplane.RelationshipLookup {
	return func(ctx context.Context, h lvol.Handle) (controlplane.Relationship, error) {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/relationships/%s", h.ClusterID, h.VolumeID)
		body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return controlplane.Relationship{}, err
		}
		if status == http.StatusNotFound {
			return controlplane.Relationship{}, fmt.Errorf("relationship of %s: %w", h, errs.ErrNotFound)
		}
		if status >= 300 {
			return controlplane.Relationship{}, fmt.Errorf("status %d: %s", status, string(body))
		}
		var rel relationshipResponse
		if err := json.Unmarshal(body, &rel); err != nil {
			return controlplane.Relationship{}, fmt.Errorf("decode relationship of %s: %w", h, err)
		}
		return controlplane.Relationship{
			IsSource:        rel.IsSource,
			SourceClusterID: rel.SourceClusterID,
			SourceLvolID:    rel.SourceLvolID,
			TargetClusterID: rel.TargetClusterID,
			TargetPoolID:    rel.TargetPoolID,
			TargetLvolID:    rel.TargetLvolID,
			ActiveLvolID:    rel.ActiveLvolID,
		}, nil
	}
}

// currentVolumeHandle is the handle of the volume serving the data behind
// handle today: handle itself unless a completed cutover or fail-over moved it.
// A handle the control plane could not have issued (non-UUID cluster or
// volume) has no relationships to walk and is returned as it is.
func currentVolumeHandle(ctx context.Context, apiClient *webapi.Client, handle string) (string, error) {
	h, ok := lvol.ParseHandle(lvol.VolumeHandle(handle))
	if !ok {
		return handle, nil
	}
	cur, err := controlplane.ActiveVolume(ctx, h, relationshipLookup(apiClient))
	if err != nil {
		return "", err
	}
	return cur.String(), nil
}

package controlplane

// The sync-replication verbs of a stretched cluster
// (design-stretched-sync-replication-ramen.md §9.3): where a volume is led
// from, the demote on the site it leaves, and the promote on the site it moves
// to. On a sync cluster each verb names a site, which the generated client's
// parameters do not carry yet (shared/openapi.json is exported from a control
// plane without them), so the site rides a request editor and the status read,
// a route the spec does not have, is an ordinary GET.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

// VolumeSyncStatus is where a volume of a sync cluster is led from, and how its
// replicas stand.
type VolumeSyncStatus struct {
	// Site is the site the volume is led from.
	Site string
	// Role is the volume's role on that site.
	Role string
	// State is the cluster-wide replication state, the worst over every store.
	State string
	// PeerReady is true when the other site's replicas are complete, not
	// degraded, and not resyncing.
	PeerReady bool
}

// SyncPromoteResult is the outcome of a promote the backend accepted.
type SyncPromoteResult struct {
	// InProgress is true while the promote task runs; the caller calls again.
	InProgress bool
}

// SyncRefusedError is a sync verb the backend refused with a 409: a gate that
// did not pass, or a promote or demote its decision table refuses. Volumes are
// the volumes of the store still served on the other site, which a caller
// reports as what blocks the move.
type SyncRefusedError struct {
	Message  string
	Volumes  []string
	Problems []string
}

func (e *SyncRefusedError) Error() string {
	if len(e.Volumes) > 0 {
		return fmt.Sprintf("%s (volumes %s)", e.Message, strings.Join(e.Volumes, ", "))
	}
	return e.Message
}

// ErrSyncPeerSiteOffline is a planned promote refused because the site the
// volume is served from is not online: the 412 a caller escalates to a forced
// promote on, once the backend's proof that the site is lost allows it.
var ErrSyncPeerSiteOffline = errors.New("the site the volume is served from is not online")

// syncDetail is the body of a sync-replication refusal.
type syncDetail struct {
	Detail struct {
		Message  string   `json:"message"`
		Volumes  []string `json:"volumes"`
		Problems []string `json:"problems"`
		TaskID   string   `json:"task_id"`
	} `json:"detail"`
}

func parseSyncDetail(body []byte) syncDetail {
	var d syncDetail
	_ = json.Unmarshal(body, &d)
	return d
}

// withQuery adds query parameters to a generated request.
func withQuery(params map[string]string) cpapi.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		q := req.URL.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
		return nil
	}
}

// syncError turns a refused sync verb into the error a caller classifies.
func syncError(what string, code int, body []byte) error {
	d := parseSyncDetail(body)
	switch code {
	case http.StatusConflict:
		return &SyncRefusedError{Message: d.Detail.Message, Volumes: d.Detail.Volumes, Problems: d.Detail.Problems}
	case http.StatusPreconditionFailed:
		return fmt.Errorf("%s: %s: %w", what, d.Detail.Message, ErrSyncPeerSiteOffline)
	default:
		return respError(what, code, body)
	}
}

// SyncStatus reads where the volume is led from. No site is sent: the backend
// answers for the site the volume is led from.
func (c *Client) SyncStatus(ctx context.Context, h lvol.VolumeHandle) (VolumeSyncStatus, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return VolumeSyncStatus{}, err
	}
	body, err := c.rawGet(ctx, "api/v2/clusters", cluster.String(), "storage-pools", pool.String(),
		"volumes", volume.String(), "replication/sync-status")
	if err != nil {
		return VolumeSyncStatus{}, fmt.Errorf("sync status of %s: %w", h, err)
	}
	var dto struct {
		Site      string `json:"site"`
		Role      string `json:"role"`
		State     string `json:"state"`
		PeerReady bool   `json:"peer_ready"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		return VolumeSyncStatus{}, fmt.Errorf("sync status of %s: %w", h, err)
	}
	return VolumeSyncStatus{Site: dto.Site, Role: dto.Role, State: dto.State, PeerReady: dto.PeerReady}, nil
}

// SyncDemoteVolume fences the volume on site, the site it leaves. A store still
// serving other volumes there is a *SyncRefusedError naming them.
func (c *Client) SyncDemoteVolume(ctx context.Context, h lvol.VolumeHandle, site string) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationDemoteApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationDemotePostWithResponse(
		ctx, cluster, pool, volume, withQuery(map[string]string{"site": site}))
	if err != nil {
		return fmt.Errorf("demote volume %s on site %s: %w", h, site, err)
	}
	if code := resp.StatusCode(); code != http.StatusNoContent && code != http.StatusOK {
		return syncError(fmt.Sprintf("demote volume %s on site %s", h, site), code, resp.Body)
	}
	return nil
}

// SyncPromoteVolume promotes the volume on site, planned or forced. A promote
// the backend is still running is InProgress, which the caller calls again; a
// planned promote whose serving site is not online is ErrSyncPeerSiteOffline.
func (c *Client) SyncPromoteVolume(ctx context.Context, h lvol.VolumeHandle, site string, planned bool) (SyncPromoteResult, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return SyncPromoteResult{}, err
	}
	params := &cpapi.ClustersStoragePoolsVolumesReplicationFailoverApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationFailoverPostParams{
		Planned: &planned,
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationFailoverApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationFailoverPostWithResponse(
		ctx, cluster, pool, volume, params, withQuery(map[string]string{"site": site}))
	if err != nil {
		return SyncPromoteResult{}, fmt.Errorf("promote volume %s on site %s: %w", h, site, err)
	}
	switch code := resp.StatusCode(); code {
	case http.StatusOK, http.StatusNoContent:
		return SyncPromoteResult{}, nil
	case http.StatusConflict:
		// The backend's 409 is either the promote it is still running, which
		// it asks to be called again for, or a refusal naming what blocks it.
		if d := parseSyncDetail(resp.Body); len(d.Detail.Volumes) == 0 && len(d.Detail.Problems) == 0 &&
			strings.Contains(d.Detail.Message, "in progress") {
			return SyncPromoteResult{InProgress: true}, nil
		}
		return SyncPromoteResult{}, syncError(fmt.Sprintf("promote volume %s on site %s", h, site), code, resp.Body)
	default:
		return SyncPromoteResult{}, syncError(fmt.Sprintf("promote volume %s on site %s", h, site), code, resp.Body)
	}
}

// SiteNode is a storage node's management address and site, which is how a
// fence's CIDRs are resolved to a site.
type SiteNode struct {
	ID     string
	MgmtIP string
	Site   string
}

// SiteNodes lists the cluster's storage nodes with their sites. The listing is
// read as raw JSON because the generated model does not carry the site yet.
func (c *Client) SiteNodes(ctx context.Context, clusterID string) ([]SiteNode, error) {
	body, err := c.rawGet(ctx, "api/v2/clusters", clusterID, "storage-nodes/")
	if err != nil {
		return nil, fmt.Errorf("storage nodes of cluster %s: %w", clusterID, err)
	}
	var dtos []struct {
		ID     string `json:"id"`
		MgmtIP string `json:"mgmt_ip"`
		Site   string `json:"site"`
	}
	if err := json.Unmarshal(body, &dtos); err != nil {
		return nil, fmt.Errorf("storage nodes of cluster %s: %w", clusterID, err)
	}
	nodes := make([]SiteNode, 0, len(dtos))
	for _, d := range dtos {
		nodes = append(nodes, SiteNode{ID: d.ID, MgmtIP: d.MgmtIP, Site: d.Site})
	}
	return nodes, nil
}

// FenceSite fences a lost site of a stretched cluster. The control plane runs
// the fence as a task, proving the site lost before it fences any of its
// volumes: a fence still running is not done, and the call is repeated until
// the site is recorded fenced. A fence the control plane refuses is a
// *SyncRefusedError.
func (c *Client) FenceSite(ctx context.Context, clusterID, site string) (done bool, err error) {
	code, body, err := c.rawDo(ctx, http.MethodPost, "api/v2/clusters", clusterID, "sites", site, "fence")
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusAccepted:
		return false, nil
	}
	return false, syncError("fence of site "+site, code, body)
}

// UnfenceSite reports whether the control plane no longer records a site lost.
// It changes nothing: a site returns by its own activation once its nodes are
// back.
func (c *Client) UnfenceSite(ctx context.Context, clusterID, site string) (cleared bool, err error) {
	code, body, err := c.rawDo(ctx, http.MethodPost, "api/v2/clusters", clusterID, "sites", site, "unfence")
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusConflict:
		return false, nil
	}
	return false, syncError("unfence of site "+site, code, body)
}

// rawGet reads a route the generated client does not cover. A non-200 answer
// is the error syncError makes of it.
func (c *Client) rawGet(ctx context.Context, path ...string) ([]byte, error) {
	code, body, err := c.rawDo(ctx, http.MethodGet, path...)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, syncError("GET "+strings.Join(path, "/"), code, body)
	}
	return body, nil
}

// rawDo calls a route the generated client does not cover, authenticated and
// transported as every other call, and returns its status and body.
func (c *Client) rawDo(ctx context.Context, method string, path ...string) (int, []byte, error) {
	endpoint, err := url.JoinPath(c.cfg.Endpoint, path...)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	if err := bearerAuth(c.cfg.Token)(ctx, req); err != nil {
		return 0, nil, err
	}
	resp, err := (&http.Client{Timeout: c.cfg.Timeout, Transport: c.cfg.Transport}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

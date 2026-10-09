// Hand-rolled client for the v2 synchronous-replication write routes, keyed on
// the caller's site: demote and failover (promote). These routes are not in the
// generated cpapi client, because shared/openapi.json is exported from a control
// plane that does not yet carry them. Until that spec ships and cpapi is
// regenerated, this file issues the requests directly against the client's own
// transport and bearer token, which is the same connection every generated call
// uses.
//
// The status read is deliberately not here: GetVolumeReplicationInfo on a sync
// cluster calls the existing generated status route without a site, and the
// backend fills it from the sync status (design-sync-replication-csi-addons.md
// §5.3, §7). Only the write verbs need the site, so only they are hand-rolled.
//
// Contract: sbcli docs/sync-replication.md §3. Driver mapping and the status-code
// protocol: design-sync-replication-csi-addons.md §6, §11.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/simplyblock/atlas/lvol"
)

// SyncStatusError carries the HTTP status of a refused sync switchover, which is
// the backend's protocol rather than an opaque failure: 409 is always retryable
// and never a reason to force, 412 is the single escalation trigger (the other
// site is offline on an unforced promote), and 400 is a bad or missing site. The
// driver maps the status onto a gRPC code from this (design §11).
type SyncStatusError struct {
	Op      string
	Status  int
	Message string
}

func (e *SyncStatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %d %s", e.Op, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %d", e.Op, e.Status)
}

// SyncPromoteResult is the connection entries the backend returns once the
// volume is served on the target site (design §6.1). The driver does not act on
// them: node staging reads the connection separately at publish time.
type SyncPromoteResult struct {
	LvolID            string   `json:"lvol_id"`
	ConnectionStrings []string `json:"connection_strings"`
}

// SyncDemote fences the volume on site. A 204 is success, including when the
// volume is not served there (a no-op, so a retry is safe). A 409 is a planned
// gate refusal and a 500 an ANA RPC failure, both carried as a SyncStatusError
// for the driver to return retryable.
func (c *Client) SyncDemote(ctx context.Context, h lvol.VolumeHandle, site string) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	code, body, err := c.syncDo(ctx, http.MethodPost,
		volumeSyncPath(cluster.String(), pool.String(), volume.String(), "demote"),
		url.Values{"site": {site}})
	if err != nil {
		return fmt.Errorf("sync demote %s: %w", h, err)
	}
	if code == http.StatusNoContent {
		return nil
	}
	return &SyncStatusError{Op: "sync demote " + string(h), Status: code, Message: syncMessage(body)}
}

// SyncPromote serves the volume on site. planned=false is a forced disaster
// fail-over. A 200 carries the connection entries. A 409 is in progress or a
// refused gate (retryable), a 412 is the peer offline on an unforced promote
// (the one escalation trigger), and a 400 is a bad or missing site, each carried
// as a SyncStatusError.
func (c *Client) SyncPromote(
	ctx context.Context, h lvol.VolumeHandle, site string, planned bool,
) (SyncPromoteResult, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return SyncPromoteResult{}, err
	}
	query := url.Values{"site": {site}, "planned": {strconv.FormatBool(planned)}}
	code, body, err := c.syncDo(ctx, http.MethodPost,
		volumeSyncPath(cluster.String(), pool.String(), volume.String(), "failover"), query)
	if err != nil {
		return SyncPromoteResult{}, fmt.Errorf("sync promote %s: %w", h, err)
	}
	if code == http.StatusOK {
		var result SyncPromoteResult
		_ = json.Unmarshal(body, &result)
		return result, nil
	}
	return SyncPromoteResult{},
		&SyncStatusError{Op: "sync promote " + string(h), Status: code, Message: syncMessage(body)}
}

// syncDo issues one request against a v2 sync route with the client's transport
// and bearer token, returning the status code and body for the caller to map.
func (c *Client) syncDo(
	ctx context.Context, method, path string, query url.Values,
) (int, []byte, error) {
	target := strings.TrimRight(c.cfg.Endpoint, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return 0, nil, err
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := (&http.Client{Timeout: c.cfg.Timeout, Transport: c.cfg.Transport}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

func volumeSyncPath(cluster, pool, volume, verb string) string {
	return fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/%s",
		cluster, pool, volume, verb)
}

// SyncGroupPromoteResult is the per-member connection entries a group promote
// returns once every member is served on the target site (design §13).
type SyncGroupPromoteResult struct {
	Members []SyncPromoteResult `json:"members"`
}

// SyncDemoteGroup fences every member of the group on site, in order. A 204 is
// success, including an empty group. A 409 is a gate refusal, carried as a
// SyncStatusError (design §13). The backend drives the group as one unit, so
// this is one call for all members, not one per member.
func (c *Client) SyncDemoteGroup(ctx context.Context, gh lvol.GroupHandle, site string) error {
	code, body, err := c.syncDo(ctx, http.MethodPost,
		groupSyncPath(gh.ClusterID, gh.GroupID, "demote"), url.Values{"site": {site}})
	if err != nil {
		return fmt.Errorf("sync demote group %s: %w", gh.Handle(), err)
	}
	if code == http.StatusNoContent {
		return nil
	}
	return &SyncStatusError{
		Op: "sync demote group " + string(gh.Handle()), Status: code, Message: syncMessage(body)}
}

// SyncPromoteGroup serves every member of the group on site as one unit.
// planned=false is a forced disaster fail-over. A 200 carries the per-member
// connection entries; 409 (in progress or refused), 412 (peer offline), and 400
// (bad site) map exactly as the volume route (design §13).
func (c *Client) SyncPromoteGroup(
	ctx context.Context, gh lvol.GroupHandle, site string, planned bool,
) (SyncGroupPromoteResult, error) {
	query := url.Values{"site": {site}, "planned": {strconv.FormatBool(planned)}}
	code, body, err := c.syncDo(ctx, http.MethodPost,
		groupSyncPath(gh.ClusterID, gh.GroupID, "failover"), query)
	if err != nil {
		return SyncGroupPromoteResult{}, fmt.Errorf("sync promote group %s: %w", gh.Handle(), err)
	}
	if code == http.StatusOK {
		var result SyncGroupPromoteResult
		_ = json.Unmarshal(body, &result)
		return result, nil
	}
	return SyncGroupPromoteResult{}, &SyncStatusError{
		Op: "sync promote group " + string(gh.Handle()), Status: code, Message: syncMessage(body)}
}

func groupSyncPath(cluster, group, verb string) string {
	return fmt.Sprintf(
		"/api/v2/clusters/%s/consistency-groups/%s/replication/%s",
		cluster, group, verb)
}

// syncMessage pulls the backend's detail.message out of a FastAPI error envelope
// where present, falling back to the raw body (design §10, error envelope).
func syncMessage(body []byte) string {
	var envelope struct {
		Detail struct {
			Message string `json:"message"`
		} `json:"detail"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Detail.Message != "" {
		return envelope.Detail.Message
	}
	return strings.TrimSpace(string(body))
}

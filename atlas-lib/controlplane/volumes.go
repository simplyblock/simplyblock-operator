package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

// Volume fetches the identity of a logical volume by handle.
func (c *Client) Volume(ctx context.Context, h lvol.VolumeHandle) (lvol.Volume, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return lvol.Volume{}, err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesDetailApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdGetWithResponse(ctx, cluster, pool, volume)
	if err != nil {
		return lvol.Volume{}, fmt.Errorf("volume %s: %w", h, err)
	}
	if resp.JSON200 == nil {
		return lvol.Volume{}, statusError(h, resp.StatusCode(), resp.Body)
	}
	d := resp.JSON200
	return lvol.Volume{
		ID:        h,
		Name:      d.Name,
		Pool:      d.PoolName,
		SizeBytes: uint64(d.Size),
		NQN:       d.Nqn,
	}, nil
}

// Connection fetches how to reach a logical volume over NVMe-oF.
func (c *Client) Connection(ctx context.Context, h lvol.VolumeHandle) (lvol.Connection, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return lvol.Connection{}, err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesConnectApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdConnectGetWithResponse(
		ctx, cluster, pool, volume,
		&cpapi.ClustersStoragePoolsVolumesConnectApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdConnectGetParams{},
	)
	if err != nil {
		return lvol.Connection{}, fmt.Errorf("connect %s: %w", h, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return lvol.Connection{}, statusError(h, resp.StatusCode(), resp.Body)
	}

	// The /connect body is untyped in the spec (FastAPI declares no response
	// model), so decode it here: a list of per-path connect entries.
	var entries []connectEndpoint
	if err := json.Unmarshal(resp.Body, &entries); err != nil {
		return lvol.Connection{}, fmt.Errorf("connect %s: decode response: %w", h, err)
	}
	if len(entries) == 0 {
		return lvol.Connection{}, fmt.Errorf("connect %s: %w", h, errs.ErrNotConnected)
	}

	conn := lvol.Connection{NQN: entries[0].NQN}
	for _, e := range entries {
		conn.Endpoints = append(conn.Endpoints, lvol.Endpoint{
			Transport: e.Transport,
			Address:   e.IP,
			Port:      e.Port,
		})
	}
	return conn, nil
}

// ListVolumes returns every volume in the given cluster's pool.
func (c *Client) ListVolumes(ctx context.Context, clusterID, poolID string) ([]lvol.Volume, error) {
	cluster, pool, err := parseIDs(clusterID, poolID)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesListApiV2ClustersClusterIdStoragePoolsPoolIdVolumesGetWithResponse(ctx, cluster, pool)
	if err != nil {
		return nil, fmt.Errorf("list volumes in %s/%s: %w", clusterID, poolID, err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("list volumes in %s/%s: control-plane returned %d: %s",
			clusterID, poolID, resp.StatusCode(), strings.TrimSpace(string(resp.Body)))
	}
	out := make([]lvol.Volume, 0, len(*resp.JSON200))
	for _, d := range *resp.JSON200 {
		out = append(out, lvol.Volume{
			ID:        lvol.VolumeHandle(clusterID + ":" + poolID + ":" + d.Id.String()),
			Name:      d.Name,
			Pool:      d.PoolName,
			SizeBytes: uint64(d.Size),
			NQN:       d.Nqn,
		})
	}
	return out, nil
}

// ResizeVolume grows the volume to sizeBytes.
func (c *Client) ResizeVolume(ctx context.Context, h lvol.VolumeHandle, sizeBytes uint64) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	size, err := sizeToInt(sizeBytes)
	if err != nil {
		return fmt.Errorf("resize volume %s: %w", h, err)
	}
	resp, err := c.api.ClustersStoragePoolsVolumesUpdateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdPutWithResponse(
		ctx, cluster, pool, volume, cpapi.UpdatableLVolParams{Size: &size})
	if err != nil {
		return fmt.Errorf("resize volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return statusError(h, code, resp.Body)
	}
	return nil
}

// DeleteVolume removes a volume. It is idempotent: a volume that is already
// gone (404) is not an error.
func (c *Client) DeleteVolume(ctx context.Context, h lvol.VolumeHandle) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesDeleteApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdDeleteWithResponse(ctx, cluster, pool, volume)
	if err != nil {
		return fmt.Errorf("delete volume %s: %w", h, err)
	}
	switch resp.StatusCode() {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return statusError(h, resp.StatusCode(), resp.Body)
	}
}

// CreateVolumeParams are the inputs for creating a volume. Only Name and
// SizeBytes are required; zero-valued optional fields are omitted so the
// control plane applies its defaults.
type CreateVolumeParams struct {
	Name      string
	SizeBytes uint64

	HAType                string // ha_type, e.g. "ha" or "single"
	Encrypt               bool
	Namespaced            bool
	MaxNamespacePerSubsys int
	NDCS                  int
	NPCS                  int
	Replicate             bool
	ReplicationClusterID  string
	HostID                string
	PVCName               string

	// QoS limits; 0 means unset.
	MaxRWIOPS   int
	MaxRWMbytes int
	MaxRMbytes  int
	MaxWMbytes  int
}

func (p CreateVolumeParams) toUnderscore(size int) cpapi.UnderscoreCreateParams {
	u := cpapi.UnderscoreCreateParams{Name: p.Name, Size: size}
	if p.HAType != "" {
		ha := cpapi.CreateParamsHaType(p.HAType)
		u.HaType = &ha
	}
	if p.Encrypt {
		u.Encrypt = ptr(true)
	}
	if p.Namespaced {
		u.Namespaced = ptr(true)
	}
	if p.MaxNamespacePerSubsys > 0 {
		u.MaxNamespacePerSubsys = ptr(p.MaxNamespacePerSubsys)
	}
	if p.NDCS > 0 {
		u.Ndcs = ptr(p.NDCS)
	}
	if p.NPCS > 0 {
		u.Npcs = ptr(p.NPCS)
	}
	if p.Replicate {
		u.DoReplicate = ptr(true)
	}
	if p.ReplicationClusterID != "" {
		u.ReplicationClusterId = ptr(p.ReplicationClusterID)
	}
	if p.HostID != "" {
		u.HostId = ptr(p.HostID)
	}
	if p.PVCName != "" {
		u.PvcName = ptr(p.PVCName)
	}
	if p.MaxRWIOPS > 0 {
		u.MaxRwIops = ptr(p.MaxRWIOPS)
	}
	if p.MaxRWMbytes > 0 {
		u.MaxRwMbytes = ptr(p.MaxRWMbytes)
	}
	if p.MaxRMbytes > 0 {
		u.MaxRMbytes = ptr(p.MaxRMbytes)
	}
	if p.MaxWMbytes > 0 {
		u.MaxWMbytes = ptr(p.MaxWMbytes)
	}
	return u
}

// CreateVolume creates a volume in the cluster's pool and returns its handle.
func (c *Client) CreateVolume(ctx context.Context, clusterID, poolID string, params CreateVolumeParams) (lvol.VolumeHandle, error) {
	cluster, pool, err := parseIDs(clusterID, poolID)
	if err != nil {
		return "", err
	}
	size, err := sizeToInt(params.SizeBytes)
	if err != nil {
		return "", fmt.Errorf("create volume %q: %w", params.Name, err)
	}
	var body cpapi.ClustersStoragePoolsVolumesCreateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesPostJSONRequestBody
	if err := body.FromUnderscoreCreateParams(params.toUnderscore(size)); err != nil {
		return "", fmt.Errorf("create volume %q: %w", params.Name, err)
	}
	resp, err := c.api.ClustersStoragePoolsVolumesCreateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesPostWithResponse(ctx, cluster, pool, nil, body)
	if err != nil {
		return "", fmt.Errorf("create volume %q: %w", params.Name, err)
	}
	id, err := createdID("create volume "+params.Name, resp.HTTPResponse, resp.Body)
	if err != nil {
		return "", err
	}
	return lvol.VolumeHandle(clusterID + ":" + poolID + ":" + id), nil
}

// CloneVolumeParams are the inputs for cloning a snapshot into a new volume.
// Name and SnapshotID are required.
type CloneVolumeParams struct {
	Name         string
	SnapshotID   string
	SizeBytes    uint64 // 0 inherits the snapshot's size
	PVCName      string
	PVCNamespace string
	// DeleteSnapshotOnDelete deletes the source snapshot when the clone is
	// deleted.
	DeleteSnapshotOnDelete bool
}

// CloneVolume creates a new volume from a snapshot and returns its handle.
func (c *Client) CloneVolume(ctx context.Context, clusterID, poolID string, params CloneVolumeParams) (lvol.VolumeHandle, error) {
	cluster, pool, err := parseIDs(clusterID, poolID)
	if err != nil {
		return "", err
	}
	clone := cpapi.UnderscoreCloneParams{Name: params.Name, SnapshotId: &params.SnapshotID}
	if params.SizeBytes > 0 {
		size, err := sizeToInt(params.SizeBytes)
		if err != nil {
			return "", fmt.Errorf("clone volume %q: %w", params.Name, err)
		}
		clone.Size = ptr(size)
	}
	if params.PVCName != "" {
		clone.PvcName = ptr(params.PVCName)
	}
	if params.PVCNamespace != "" {
		clone.PvcNamespace = ptr(params.PVCNamespace)
	}
	if params.DeleteSnapshotOnDelete {
		clone.DeleteSnapOnLvolDelete = ptr(true)
	}
	var body cpapi.ClustersStoragePoolsVolumesCreateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesPostJSONRequestBody
	if err := body.FromUnderscoreCloneParams(clone); err != nil {
		return "", fmt.Errorf("clone volume %q: %w", params.Name, err)
	}
	resp, err := c.api.ClustersStoragePoolsVolumesCreateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesPostWithResponse(ctx, cluster, pool, nil, body)
	if err != nil {
		return "", fmt.Errorf("clone volume %q: %w", params.Name, err)
	}
	id, err := createdID("clone volume "+params.Name, resp.HTTPResponse, resp.Body)
	if err != nil {
		return "", err
	}
	return lvol.VolumeHandle(clusterID + ":" + poolID + ":" + id), nil
}

// createdID extracts the new resource's id from a creation's Location header
// (e.g. ".../volumes/<id>/"). It errors on a non-2xx response.
func createdID(what string, resp *http.Response, body []byte) (string, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", respError(what, resp.StatusCode, body)
	}
	loc := strings.Trim(resp.Header.Get("Location"), "/")
	if loc == "" {
		return "", fmt.Errorf("%s: created but response carried no Location header", what)
	}
	if i := strings.LastIndex(loc, "/"); i >= 0 {
		loc = loc[i+1:]
	}
	return loc, nil
}

// ptr returns a pointer to v — for the many optional (pointer) fields the
// generated request bodies use.
func ptr[T any](v T) *T { return &v }

// sizeToInt converts a byte size to the int the API expects, rejecting values
// that would overflow int (i.e. on 32-bit builds, or absurd sizes).
func sizeToInt(bytes uint64) (int, error) {
	if bytes > math.MaxInt {
		return 0, fmt.Errorf("size %d bytes exceeds the maximum supported (%d)", bytes, uint64(math.MaxInt))
	}
	return int(bytes), nil
}

// connectEndpoint is one element of the /connect response.
type connectEndpoint struct {
	Transport string `json:"transport"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	NQN       string `json:"nqn"`
}

// statusError maps a non-success control-plane response for a volume to an
// error, using the shared ErrNotFound sentinel for 404 so callers can match
// with errors.Is.
func statusError(h lvol.VolumeHandle, code int, body []byte) error {
	if code == http.StatusNotFound {
		return fmt.Errorf("volume %s: %w", h, errs.ErrNotFound)
	}
	return fmt.Errorf("volume %s: control-plane returned %d: %s", h, code, strings.TrimSpace(string(body)))
}

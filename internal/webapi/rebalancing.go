package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// StorageNodeInfo holds fields from GET /api/v2/clusters/{id}/storage-nodes/.
type StorageNodeInfo struct {
	UUID       string `json:"id"`
	Status     string `json:"status"`
	Healthy    bool   `json:"health_check"`
	TotalBytes int64  `json:"total_capacity_bytes"`
}

// CapacityStat holds the capacity sub-object present on VolumeDTO.
type CapacityStat struct {
	SizeUsed int64 `json:"size_used"`
}

// VolumeInfo holds fields from VolumeDTO returned by
// GET /api/v2/clusters/{id}/storage-pools/{id}/volumes/.
type VolumeInfo struct {
	UUID                  string       `json:"id"`
	Name                  string       `json:"name"`
	PrimaryNodeUUID       string       `json:"storage_node_id"`
	Status                string       `json:"status"`
	Migrating             bool         `json:"migrating"`
	Capacity              CapacityStat `json:"capacity"`
	IOPS                  float64      `json:"iops"`
	ThroughputBytesPerSec float64      `json:"throughput_bytes_per_sec"`
}

// StoragePoolInfo holds the fields needed from GET /api/v2/clusters/{id}/storage-pools/.
type StoragePoolInfo struct {
	UUID string `json:"id"`
	Name string `json:"name"`
}

// ContinueMigrationParams is the request body for the continue migration endpoint.
// MigrationID is identified via the URL path; this body carries optional tuning params only.
type ContinueMigrationParams struct {
	MaxRetries      int `json:"max_retries,omitempty"`
	DeadlineSeconds int `json:"deadline_seconds,omitempty"`
}

// LvolConnectResp holds the NVMe-oF connection parameters for a logical volume,
// as returned by CreateMigration for the new target-side paths that must be
// connected and validated before calling ContinueMigration.
type LvolConnectResp struct {
	Nqn            string `json:"nqn"`
	ReconnectDelay int    `json:"reconnect-delay"`
	NrIoQueues     int    `json:"nr-io-queues"`
	CtrlLossTmo    int    `json:"ctrl-loss-tmo"`
	FastIOFailTmo  int    `json:"fast-io-fail-tmo"`
	KeepAliveTmo   int    `json:"keep-alive-tmo"`
	Port           int    `json:"port"`
	TargetType     string `json:"transport"`
	IP             string `json:"ip"`
	Connect        string `json:"connect"`
	NSID           int    `json:"ns-id"`
	HostIface      string `json:"host-iface,omitempty"`
}

// MigrateParams is the request body for
// POST /api/v2/clusters/{id}/storage-pools/{id}/volumes/{id}/migrations.
type MigrateParams struct {
	TargetNodeID string `json:"target_node_id"`
}

// MigrationDTO is returned by POST (create), GET (poll), and ContinueMigration.
type MigrationDTO struct {
	ID                        string            `json:"id"`
	LvolID                    string            `json:"lvol_id"`
	SourceNodeID              string            `json:"source_node_id"`
	TargetNodeID              string            `json:"target_node_id"`
	Phase                     string            `json:"phase"`
	Status                    string            `json:"status"`
	SnapsTotal                int               `json:"snaps_total"`
	SnapsMigrated             int               `json:"snaps_migrated"`
	IntermediateSnapRounds    int               `json:"intermediate_snap_rounds"`
	MaxIntermediateSnapRounds int               `json:"max_intermediate_snap_rounds"`
	RetryCount                int               `json:"retry_count"`
	MaxRetries                int               `json:"max_retries"`
	ErrorMessage              string            `json:"error_message"`
	StartedAt                 int64             `json:"started_at"`
	CompletedAt               int64             `json:"completed_at"`
	ConnectStrings            []LvolConnectResp `json:"connect_strings"`
}

// Migration status values reported in MigrationDTO.Status. The status field —
// not error_message — is the authoritative signal for whether a migration has
// finished and whether it succeeded: a transient error_message may linger from
// a retried-then-recovered step even when the migration ultimately completes.
const (
	// MigrationStatusNew, MigrationStatusRunning, MigrationStatusSuspended and
	// MigrationStatusCutover are non-terminal: the migration is still in flight.
	MigrationStatusNew       = "new"
	MigrationStatusRunning   = "running"
	MigrationStatusSuspended = "suspended"
	MigrationStatusCutover   = "cutover"

	// MigrationStatusDone, MigrationStatusFailed and MigrationStatusCancelled are
	// the terminal states. Only MigrationStatusDone is a success.
	MigrationStatusDone      = "done"
	MigrationStatusFailed    = "failed"
	MigrationStatusCancelled = "cancelled"
)

// GetStoragePools lists all storage pools for the given cluster.
func (c *Client) GetStoragePools(
	ctx context.Context,
	clusterUUID string,
) ([]StoragePoolInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list storage pools: status %d: %s", statusCode, string(body))
	}
	var pools []StoragePoolInfo
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("unmarshal storage pools: %w", err)
	}
	return pools, nil
}

// GetStorageNodes lists all storage nodes for the given cluster.
func (c *Client) GetStorageNodes(
	ctx context.Context,
	clusterUUID string,
) ([]StorageNodeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list storage nodes: %w", err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list storage nodes: status %d: %s", statusCode, string(body))
	}
	var nodes []StorageNodeInfo
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("unmarshal storage nodes: %w", err)
	}
	return nodes, nil
}

// GetPoolVolumes lists all volumes in the given storage pool.
func (c *Client) GetPoolVolumes(
	ctx context.Context,
	clusterUUID, poolUUID string,
) ([]VolumeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/", clusterUUID, poolUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list pool volumes: %w", err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list pool volumes: status %d: %s", statusCode, string(body))
	}
	var volumes []VolumeInfo
	if err := json.Unmarshal(body, &volumes); err != nil {
		return nil, fmt.Errorf("unmarshal pool volumes: %w", err)
	}
	return volumes, nil
}

// GetVolume fetches a single volume by its cluster/pool/volume UUIDs (all known
// from the CSI volume handle). Returns (nil, nil) when the volume no longer exists.
func (c *Client) GetVolume(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID string,
) (*VolumeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/", clusterUUID, poolUUID, volumeUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get volume %s: %w", volumeUUID, err)
	}
	if statusCode == http.StatusNotFound {
		return nil, nil
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("get volume %s: status %d: %s", volumeUUID, statusCode, string(body))
	}
	// The endpoint may return a single object or a one-element array depending on
	// the backend version; accept both.
	var one VolumeInfo
	if err := json.Unmarshal(body, &one); err == nil && one.UUID != "" {
		return &one, nil
	}
	var list []VolumeInfo
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("unmarshal volume %s: %w", volumeUUID, err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// StorageNodeNIC is one network interface entry returned by the storage-node
// /nics endpoint. Address is the data-network IP the lvol subsystem listens on
// (the management IP is reported separately). Field tags match the capitalised,
// space-containing keys the control plane emits for this endpoint.
type StorageNodeNIC struct {
	ID         string `json:"ID"`
	DeviceName string `json:"Device name"`
	Address    string `json:"Address"`
	NetType    string `json:"Net type"`
	Status     string `json:"Status"`
}

// GetStorageNodeNICs returns the data-network interfaces for a single storage
// node. Used to target the fio latency baseline at the node's data-NIC IP rather
// than its management IP (the lvol subsystem does not listen on mgmt_ip).
func (c *Client) GetStorageNodeNICs(
	ctx context.Context,
	clusterUUID, nodeUUID string,
) ([]StorageNodeNIC, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/nics", clusterUUID, nodeUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get NICs for node %s: %w", nodeUUID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("get NICs for node %s: status %d: %s", nodeUUID, statusCode, string(body))
	}
	var nics []StorageNodeNIC
	if err := json.Unmarshal(body, &nics); err != nil {
		return nil, fmt.Errorf("unmarshal node NICs: %w", err)
	}
	return nics, nil
}

// CreateMigration submits a new volume migration request.
// Returns a MigrationDTO containing the migration ID and the NVMe-oF
// connection strings for the target-side paths. The caller must establish and
// validate those paths before calling ContinueMigration.
//
// If the API reports that a migration already exists for the volume, any
// existing migrations are cancelled and the request is retried once. The API
// signals this as either 409 or 400 with an "...already exists... Cancel it
// first" detail depending on deployment, so both are handled.
func (c *Client) CreateMigration(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID, targetNodeID string,
) (*MigrationDTO, error) {
	logger := log.FromContext(ctx)
	params := MigrateParams{TargetNodeID: targetNodeID}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations", clusterUUID, poolUUID, volumeUUID)

	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, params)
	if err != nil {
		return nil, fmt.Errorf("create migration for volume %s: %w", volumeUUID, err)
	}

	if isExistingMigrationConflict(statusCode, body) {
		logger.Info("CreateMigration rejected: a migration already exists for the volume; cancelling before retry",
			"volume", volumeUUID, "status", statusCode)
		if cancelErr := c.cancelMigrationForVolume(ctx, clusterUUID, poolUUID, volumeUUID); cancelErr != nil {
			return nil, fmt.Errorf("create migration for volume %s: cancel existing migrations: %w", volumeUUID, cancelErr)
		}
		body, statusCode, err = c.Do(ctx, http.MethodPost, endpoint, params)
		if err != nil {
			return nil, fmt.Errorf("create migration for volume %s (retry): %w", volumeUUID, err)
		}
	}

	// FIXME: logging the full response body is a debugging aid and should be
	// removed — or at least masked — before this is considered production-ready,
	// since the body may carry NVMe connection details (NQNs, IPs) or other
	// sensitive fields.
	logger.Info("CreateMigration response", "volume", volumeUUID, "status", statusCode, "body", string(body))

	if statusCode >= 300 {
		return nil, fmt.Errorf("create migration for volume %s: status %d: %s", volumeUUID, statusCode, string(body))
	}
	var m MigrationDTO
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal migration response: %w", err)
	}
	logger.Info("CreateMigration parsed", "volume", volumeUUID, "migration_id", m.ID, "connect_strings", len(m.ConnectStrings))
	return &m, nil
}

// isExistingMigrationConflict reports whether a CreateMigration response
// indicates an existing migration is blocking the request. Some deployments
// return 409; others return 400 with a detail like "An active migration for
// <vol> already exists targeting a different node (...). Cancel it first." Only
// that specific 400 is treated as a conflict — other 400s (bad request, volume
// already on the target node, etc.) must not trigger a cancel-and-retry.
func isExistingMigrationConflict(statusCode int, body []byte) bool {
	if statusCode == http.StatusConflict {
		return true
	}
	if statusCode == http.StatusBadRequest {
		b := strings.ToLower(string(body))
		return strings.Contains(b, "already exists") && strings.Contains(b, "migration")
	}
	return false
}

// cancelMigrationForVolume lists migrations for the volume, finds the one
// matching volumeUUID, and cancels it.
func (c *Client) cancelMigrationForVolume(ctx context.Context, clusterUUID, poolUUID, volumeUUID string) error {
	logger := log.FromContext(ctx)
	migrations, err := c.GetMigrations(ctx, clusterUUID, poolUUID, volumeUUID)
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, m := range migrations {
		if m.LvolID != volumeUUID {
			continue
		}
		logger.Info("Cancelling existing migration for volume", "migration", m.ID, "volume", volumeUUID)
		if err := c.CancelMigration(ctx, clusterUUID, poolUUID, volumeUUID, m.ID); err != nil {
			return fmt.Errorf("cancel migration %s: %w", m.ID, err)
		}
		return nil
	}
	return fmt.Errorf("no migration found for volume %s", volumeUUID)
}

// GetMigrations lists all migrations for the given volume.
func (c *Client) GetMigrations(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID string,
) ([]MigrationDTO, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations", clusterUUID, poolUUID, volumeUUID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("list migrations for cluster %s: %w", clusterUUID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("list migrations for cluster %s: status %d: %s", clusterUUID, statusCode, string(body))
	}
	var migrations []MigrationDTO
	if err := json.Unmarshal(body, &migrations); err != nil {
		return nil, fmt.Errorf("unmarshal migrations response: %w", err)
	}
	return migrations, nil
}

// GetMigration fetches the current status of a migration by its ID.
func (c *Client) GetMigration(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID, migrationID string,
) (*MigrationDTO, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations/%s", clusterUUID, poolUUID, volumeUUID, migrationID)
	body, statusCode, err := c.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("get migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	var m MigrationDTO
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal migration response: %w", err)
	}
	return &m, nil
}

// ContinueMigration kicks off the actual data migration after the caller has
// created and validated the new NVMe-oF connection paths on the target node.
// It must be called after CreateMigration and a successful path validation.
func (c *Client) ContinueMigration(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID, migrationID string,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations/%s/continue", clusterUUID, poolUUID, volumeUUID, migrationID)
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, ContinueMigrationParams{})
	if err != nil {
		return fmt.Errorf("continue migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return fmt.Errorf("continue migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	return nil
}

// CancelMigration cancels an in-progress migration by its ID.
func (c *Client) CancelMigration(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID, migrationID string,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations/%s", clusterUUID, poolUUID, volumeUUID, migrationID)
	body, statusCode, err := c.Do(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("cancel migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return fmt.Errorf("cancel migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	return nil
}

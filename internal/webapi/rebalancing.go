package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// ContinueMigrationParams is the request body for
// POST /api/v2/clusters/{id}/migrations/continue.
type ContinueMigrationParams struct {
	MigrationID string `json:"migration_id"`
}

// LvolConnectResp holds the NVMe-oF connection parameters for a logical volume,
// as returned by CreateMigration for the new target-side paths that must be
// connected and validated before calling ContinueMigration.
type LvolConnectResp struct {
	Nqn            string `json:"nqn"`
	ReconnectDelay int    `json:"reconnect-delay"`
	NrIoQueues     int    `json:"nr-io-queues"`
	CtrlLossTmo    int    `json:"ctrl-loss-tmo"`
	Port           int    `json:"port"`
	TargetType     string `json:"transport"`
	IP             string `json:"ip"`
	Connect        string `json:"connect"`
	NSID           int    `json:"ns_id"`
	HostIface      string `json:"host-iface,omitempty"`
}

// MigrateParams is the request body for
// POST /api/v2/clusters/{id}/storage-pools/{id}/volumes/{id}/migrations.
type MigrateParams struct {
	TargetNodeID string `json:"target_node_id"`
}

// CreateMigrationResponse is returned by POST /api/v2/clusters/{id}/migrations/.
// It contains only the migration ID and the NVMe-oF connection strings for the
// new target-side paths that the operator must establish and validate before
// calling ContinueMigration.
type CreateMigrationResponse struct {
	// MigrationID is the UUID of the newly created migration record.
	MigrationID    string            `json:"migration_id"`
	ConnectStrings []LvolConnectResp `json:"connect_strings"`
}

// MigrationDTO is returned by GET /api/v2/clusters/{id}/migrations/{id}/
// and by ContinueMigration. Used for status polling.
type MigrationDTO struct {
	ID                        string `json:"id"`
	LvolID                    string `json:"lvol_id"`
	SourceNodeID              string `json:"source_node_id"`
	TargetNodeID              string `json:"target_node_id"`
	Phase                     string `json:"phase"`
	Status                    string `json:"status"`
	SnapsTotal                int    `json:"snaps_total"`
	SnapsMigrated             int    `json:"snaps_migrated"`
	IntermediateSnapRounds    int    `json:"intermediate_snap_rounds"`
	MaxIntermediateSnapRounds int    `json:"max_intermediate_snap_rounds"`
	RetryCount                int    `json:"retry_count"`
	MaxRetries                int    `json:"max_retries"`
	ErrorMessage              string `json:"error_message"`
	StartedAt                 int64  `json:"started_at"`
	CompletedAt               int64  `json:"completed_at"`
}

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

// CreateMigration submits a new volume migration request.
// Returns a CreateMigrationResponse containing the migration ID and the NVMe-oF
// connection strings for the target-side paths. The caller must establish and
// validate those paths before calling ContinueMigration.
func (c *Client) CreateMigration(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID, targetNodeID string,
) (*CreateMigrationResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations", clusterUUID, poolUUID, volumeUUID)
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, MigrateParams{
		TargetNodeID: targetNodeID,
	})
	if err != nil {
		return nil, fmt.Errorf("create migration for volume %s: %w", volumeUUID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("create migration for volume %s: status %d: %s", volumeUUID, statusCode, string(body))
	}
	var m CreateMigrationResponse
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal migration response: %w", err)
	}
	return &m, nil
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
) (*MigrationDTO, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations/%s/continue", clusterUUID, poolUUID, volumeUUID, migrationID)
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, ContinueMigrationParams{
		MigrationID: migrationID,
	})
	if err != nil {
		return nil, fmt.Errorf("continue migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("continue migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	var m MigrationDTO
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal continue migration response: %w", err)
	}
	return &m, nil
}

// CancelMigration cancels an in-progress migration by its ID.
func (c *Client) CancelMigration(
	ctx context.Context,
	clusterUUID, poolUUID, volumeUUID, migrationID string,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/migrations/%s/cancel", clusterUUID, poolUUID, volumeUUID, migrationID)
	body, statusCode, err := c.Do(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("cancel migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return fmt.Errorf("cancel migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	return nil
}

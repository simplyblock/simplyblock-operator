/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

// GetStoragePools lists all storage pools for the given cluster.
func (c *Client) GetStoragePools(
	ctx context.Context,
	clusterSecret, clusterUUID string,
) ([]StoragePoolInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, statusCode, err := c.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
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
	clusterSecret, clusterUUID string,
) ([]StorageNodeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, statusCode, err := c.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
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
	clusterSecret, clusterUUID, poolUUID string,
) ([]VolumeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/", clusterUUID, poolUUID)
	body, statusCode, err := c.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
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

// MigrateParams is the request body for POST /api/v2/clusters/{id}/migrations/.
type MigrateParams struct {
	VolumeID     string `json:"volume_id"`
	TargetNodeID string `json:"target_node_id"`
}

// MigrationDTO is returned by GET /api/v2/clusters/{id}/migrations/{migration_id}/.
type MigrationDTO struct {
	ID            string `json:"id"`
	LvolID        string `json:"lvol_id"`
	SourceNodeID  string `json:"source_node_id"`
	TargetNodeID  string `json:"target_node_id"`
	Phase         string `json:"phase"`
	Status        string `json:"status"`
	SnapsTotal    int    `json:"snaps_total"`
	SnapsMigrated int    `json:"snaps_migrated"`
	RetryCount    int    `json:"retry_count"`
	MaxRetries    int    `json:"max_retries"`
	ErrorMessage  string `json:"error_message"`
	StartedAt     int64  `json:"started_at"`
	CompletedAt   int64  `json:"completed_at"`
}

// CreateMigration starts a new volume migration to targetNodeID.
// Returns the migration record including the ID needed for subsequent status polls.
func (c *Client) CreateMigration(
	ctx context.Context,
	clusterSecret, clusterUUID, volumeUUID, targetNodeID string,
) (*MigrationDTO, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/migrations/", clusterUUID)
	body, statusCode, err := c.Do(ctx, clusterSecret, http.MethodPost, endpoint, MigrateParams{
		VolumeID:     volumeUUID,
		TargetNodeID: targetNodeID,
	})
	if err != nil {
		return nil, fmt.Errorf("create migration for volume %s: %w", volumeUUID, err)
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("create migration for volume %s: status %d: %s", volumeUUID, statusCode, string(body))
	}
	var m MigrationDTO
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal migration response: %w", err)
	}
	return &m, nil
}

// GetMigration fetches the current status of a migration by its ID.
func (c *Client) GetMigration(
	ctx context.Context,
	clusterSecret, clusterUUID, migrationID string,
) (*MigrationDTO, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/migrations/%s/", clusterUUID, migrationID)
	body, statusCode, err := c.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
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

// CancelMigration cancels an in-progress migration by its ID.
func (c *Client) CancelMigration(
	ctx context.Context,
	clusterSecret, clusterUUID, migrationID string,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/migrations/%s/cancel", clusterUUID, migrationID)
	body, statusCode, err := c.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("cancel migration %s: %w", migrationID, err)
	}
	if statusCode >= 300 {
		return fmt.Errorf("cancel migration %s: status %d: %s", migrationID, statusCode, string(body))
	}
	return nil
}

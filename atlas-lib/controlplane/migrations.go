package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

// VolumeMigration is a migration of a volume between storage nodes.
type VolumeMigration struct {
	ID            string
	LvolID        string
	SourceNodeID  string
	TargetNodeID  string
	Phase         string
	Status        string
	ErrorMessage  string
	RetryCount    int
	MaxRetries    int
	SnapsMigrated int
	SnapsTotal    int
}

func volumeMigrationFromDTO(d cpapi.MigrationDTO) VolumeMigration {
	return VolumeMigration{
		ID:            d.Id.String(),
		LvolID:        d.LvolId,
		SourceNodeID:  d.SourceNodeId,
		TargetNodeID:  d.TargetNodeId,
		Phase:         d.Phase,
		Status:        d.Status,
		ErrorMessage:  d.ErrorMessage,
		RetryCount:    d.RetryCount,
		MaxRetries:    d.MaxRetries,
		SnapsMigrated: d.SnapsMigrated,
		SnapsTotal:    d.SnapsTotal,
	}
}

// ListVolumeMigrations returns the migrations of the volume identified by h.
func (c *Client) ListVolumeMigrations(ctx context.Context, h lvol.VolumeHandle) ([]VolumeMigration, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return nil, err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesMigrationsListApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdMigrationsGetWithResponse(ctx, cluster, pool, volume)
	if err != nil {
		return nil, fmt.Errorf("list migrations for volume %s: %w", h, err)
	}
	if resp.JSON200 == nil {
		return nil, respError("migrations for volume "+string(h), resp.StatusCode(), resp.Body)
	}
	out := make([]VolumeMigration, 0, len(*resp.JSON200))
	for _, d := range *resp.JSON200 {
		out = append(out, volumeMigrationFromDTO(d))
	}
	return out, nil
}

// GetVolumeMigration returns a single migration of the volume identified by h.
// It wraps errs.ErrNotFound when the migration does not exist.
func (c *Client) GetVolumeMigration(ctx context.Context, h lvol.VolumeHandle, migrationID string) (VolumeMigration, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return VolumeMigration{}, err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return VolumeMigration{}, err
	}
	resp, err := c.api.ClusterStoragePoolsVolumesMigrationsDetailApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdMigrationsMigrationIdGetWithResponse(ctx, cluster, pool, volume, migration)
	if err != nil {
		return VolumeMigration{}, fmt.Errorf("get migration %s: %w", migrationID, err)
	}
	if resp.JSON200 == nil {
		return VolumeMigration{}, respError("migration "+migrationID, resp.StatusCode(), resp.Body)
	}
	return volumeMigrationFromDTO(*resp.JSON200), nil
}

// CancelVolumeMigration cancels a migration of the volume identified by h.
func (c *Client) CancelVolumeMigration(ctx context.Context, h lvol.VolumeHandle, migrationID string) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClusterStoragePoolsVolumesMigrationsCancelApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdMigrationsMigrationIdDeleteWithResponse(ctx, cluster, pool, volume, migration)
	if err != nil {
		return fmt.Errorf("cancel migration %s: %w", migrationID, err)
	}
	return migrationActionResult("cancel migration "+migrationID, resp.StatusCode(), resp.Body)
}

// ContinueVolumeMigration resumes a paused (e.g. pre-created) migration of the
// volume identified by h.
func (c *Client) ContinueVolumeMigration(ctx context.Context, h lvol.VolumeHandle, migrationID string) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	migration, err := parseUUID("migration id", migrationID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClusterStoragePoolsVolumesMigrationsContinueApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdMigrationsMigrationIdContinuePostWithResponse(
		ctx, cluster, pool, volume, migration, cpapi.UnderscoreContinueParams{})
	if err != nil {
		return fmt.Errorf("continue migration %s: %w", migrationID, err)
	}
	return migrationActionResult("continue migration "+migrationID, resp.StatusCode(), resp.Body)
}

// CreateVolumeMigration starts migrating the volume identified by h to the
// target storage node and returns the created migration.
func (c *Client) CreateVolumeMigration(ctx context.Context, h lvol.VolumeHandle, targetNodeID string) (VolumeMigration, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return VolumeMigration{}, err
	}
	target, err := parseUUID("target node id", targetNodeID)
	if err != nil {
		return VolumeMigration{}, err
	}
	body := cpapi.UnderscoreMigrationParams{TargetNodeId: target}
	resp, err := c.api.ClusterStoragePoolsVolumesMigrationsCreateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdMigrationsPostWithResponse(ctx, cluster, pool, volume, nil, body)
	if err != nil {
		return VolumeMigration{}, fmt.Errorf("create migration for volume %s: %w", h, err)
	}
	// The create endpoint returns the full MigrationDTO body (untyped in the
	// spec's response, so decode it here).
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return VolumeMigration{}, respError("create migration for volume "+string(h), code, resp.Body)
	}
	var d cpapi.MigrationDTO
	if err := json.Unmarshal(resp.Body, &d); err != nil {
		return VolumeMigration{}, fmt.Errorf("create migration for volume %s: decode response: %w", h, err)
	}
	return volumeMigrationFromDTO(d), nil
}

// migrationActionResult treats any 2xx as success for the fire-and-forget
// cancel/continue actions (their bodies are untyped in the spec).
func migrationActionResult(what string, code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	return respError(what, code, body)
}

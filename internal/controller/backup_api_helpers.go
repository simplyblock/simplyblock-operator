package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

type backupVolumeAPIResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type backupSnapshotAPIResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type backupAPIResponse struct {
	ID           string `json:"id"`
	S3ID         int64  `json:"s3_id"`
	LvolID       string `json:"lvol_id"`
	LvolName     string `json:"lvol_name"`
	SnapshotID   string `json:"snapshot_id"`
	SnapshotName string `json:"snapshot_name"`
	Status       string `json:"status"`
	PrevBackupID string `json:"prev_backup_id"`
	CreatedAt    int64  `json:"created_at"`
	CompletedAt  int64  `json:"completed_at"`
}

func fetchVolumes(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID string) ([]backupVolumeAPIResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/", clusterUUID, poolUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("fetch volumes failed: status=%d body=%s", status, string(body))
	}

	var volumes []backupVolumeAPIResponse
	if err := json.Unmarshal(body, &volumes); err != nil {
		return nil, fmt.Errorf("unmarshal volumes: %w", err)
	}
	return volumes, nil
}

func fetchVolumeByName(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID, volumeName string) (*backupVolumeAPIResponse, error) {
	volumes, err := fetchVolumes(ctx, apiClient, clusterSecret, clusterUUID, poolUUID)
	if err != nil {
		return nil, err
	}
	for i := range volumes {
		if volumes[i].Name == volumeName {
			return &volumes[i], nil
		}
	}
	return nil, nil
}

func fetchVolumeByID(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID, volumeID string) (*backupVolumeAPIResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/", clusterUUID, poolUUID, volumeID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status >= 300 {
		return nil, fmt.Errorf("fetch volume failed: status=%d body=%s", status, string(body))
	}

	var volume backupVolumeAPIResponse
	if err := json.Unmarshal(body, &volume); err != nil {
		return nil, fmt.Errorf("unmarshal volume: %w", err)
	}
	return &volume, nil
}

func fetchSnapshotsForVolume(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID, volumeID string) ([]backupSnapshotAPIResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/snapshots", clusterUUID, poolUUID, volumeID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("fetch snapshots failed: status=%d body=%s", status, string(body))
	}

	var snapshots []backupSnapshotAPIResponse
	if err := json.Unmarshal(body, &snapshots); err != nil {
		return nil, fmt.Errorf("unmarshal snapshots: %w", err)
	}
	return snapshots, nil
}

func fetchBackups(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID string) ([]backupAPIResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("fetch backups failed: status=%d body=%s", status, string(body))
	}

	var backups []backupAPIResponse
	if err := json.Unmarshal(body, &backups); err != nil {
		return nil, fmt.Errorf("unmarshal backups: %w", err)
	}
	return backups, nil
}

func findSnapshotByName(snapshots []backupSnapshotAPIResponse, snapshotName string) *backupSnapshotAPIResponse {
	for i := range snapshots {
		if snapshots[i].Name == snapshotName {
			return &snapshots[i]
		}
	}
	return nil
}

func findBackupBySnapshotID(backups []backupAPIResponse, snapshotID string) *backupAPIResponse {
	for i := range backups {
		if backups[i].SnapshotID == snapshotID {
			return &backups[i]
		}
	}
	return nil
}

func unixTimePtr(ts int64) *metav1.Time {
	if ts <= 0 {
		return nil
	}
	t := metav1.NewTime(time.Unix(ts, 0).UTC())
	return &t
}

func generatedBackupSnapshotName(resourceName string, resourceUID string) string {
	suffix := resourceUID
	if suffix == "" {
		suffix = resourceName
	}
	suffix = strings.ToLower(strings.ReplaceAll(suffix, "_", "-"))
	if len(suffix) > 16 {
		suffix = suffix[:16]
	}
	return fmt.Sprintf("backup-%s", suffix)
}

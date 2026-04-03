package utils

import (
	"encoding/json"
	"testing"
)

func TestClusterAddParamsMarshalBackupConfig(t *testing.T) {
	snapshotBackups := true
	withCompression := false
	secondaryTarget := int32(0)
	localTesting := true

	params := ClusterAddParams{
		Name: "test-cluster",
		BackupConfig: &BackupConfig{
			AccessKeyID:     "username",
			SecretAccessKey: "password",
			LocalEndpoint:   "http://10.10.11.10:9000",
			SnapshotBackups: &snapshotBackups,
			WithCompression: &withCompression,
			SecondaryTarget: &secondaryTarget,
			LocalTesting:    &localTesting,
		},
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	backupConfig, ok := got["backup_config"].(map[string]any)
	if !ok {
		t.Fatalf("expected backup_config object, got %T", got["backup_config"])
	}

	if backupConfig["access_key_id"] != "username" {
		t.Fatalf("unexpected access_key_id: %#v", backupConfig["access_key_id"])
	}
	if backupConfig["secret_access_key"] != "password" {
		t.Fatalf("unexpected secret_access_key: %#v", backupConfig["secret_access_key"])
	}
	if backupConfig["local_endpoint"] != "http://10.10.11.10:9000" {
		t.Fatalf("unexpected local_endpoint: %#v", backupConfig["local_endpoint"])
	}
	if backupConfig["snapshot_backups"] != true {
		t.Fatalf("unexpected snapshot_backups: %#v", backupConfig["snapshot_backups"])
	}
	if backupConfig["with_compression"] != false {
		t.Fatalf("unexpected with_compression: %#v", backupConfig["with_compression"])
	}
	if backupConfig["secondary_target"] != float64(0) {
		t.Fatalf("unexpected secondary_target: %#v", backupConfig["secondary_target"])
	}
	if backupConfig["local_testing"] != true {
		t.Fatalf("unexpected local_testing: %#v", backupConfig["local_testing"])
	}
}

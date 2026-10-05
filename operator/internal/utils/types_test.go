package utils

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Regression: 2026-10-01-backup-config-wire-keys. The control plane answers 422
// to a backup_config key its schema does not declare, which no cluster with a
// backup store could get past.
func TestBackupConfigKeysAreDeclared(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	declared := spec.Components.Schemas["UnresolvedBackupConfig"].Properties
	if len(declared) == 0 {
		t.Fatal("shared/openapi.json declares no UnresolvedBackupConfig properties")
	}

	typ := reflect.TypeOf(BackupConfig{})
	for i := 0; i < typ.NumField(); i++ {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if _, ok := declared[key]; !ok {
			t.Errorf("BackupConfig.%s is sent as %q, which UnresolvedBackupConfig does not declare",
				typ.Field(i).Name, key)
		}
	}
}

func TestClusterAddParamsMarshalBackupConfig(t *testing.T) {
	snapshotBackups := true
	withCompression := false
	secondaryTarget := int32(0)

	params := ClusterAddParams{
		Name: "test-cluster",
		BackupConfig: &BackupConfig{
			Credentials:     &BackupCredentials{AccessKeyID: "username", SecretAccessKey: "password"},
			Endpoint:        "http://10.10.11.10:9000",
			BucketName:      "backups",
			Region:          "eu-central-1",
			SnapshotBackups: &snapshotBackups,
			WithCompression: &withCompression,
			SecondaryTarget: &secondaryTarget,
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

	credentials, _ := backupConfig["credentials"].(map[string]any)
	if credentials["access_key_id"] != "username" || credentials["secret_access_key"] != "password" {
		t.Fatalf("unexpected credentials: %#v", backupConfig["credentials"])
	}
	for key, want := range map[string]any{
		"endpoint":         "http://10.10.11.10:9000",
		"bucket_name":      "backups",
		"region":           "eu-central-1",
		"snapshot_backups": true,
		"with_compression": false,
		"secondary_target": float64(0),
	} {
		if backupConfig[key] != want {
			t.Fatalf("unexpected %s: %#v, want %#v", key, backupConfig[key], want)
		}
	}
}

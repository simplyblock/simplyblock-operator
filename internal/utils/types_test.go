package utils

import (
	"encoding/json"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestClusterAddParamsMarshalBackupConfig(t *testing.T) {
	params := ClusterAddParams{
		Name: "test-cluster",
		BackupConfig: &apiextensionsv1.JSON{
			Raw: []byte(`{
					"access_key_id":"username",
					"secret_access_key":"password",
					"local_endpoint":"http://10.10.11.10:9000",
				"snapshot_backups":true,
				"with_compression":false,
				"secondary_target":0,
				"local_testing":true
			}`),
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
	if backupConfig["local_endpoint"] != "http://10.10.11.10:9000" {
		t.Fatalf("unexpected local_endpoint: %#v", backupConfig["local_endpoint"])
	}
	if backupConfig["snapshot_backups"] != true {
		t.Fatalf("unexpected snapshot_backups: %#v", backupConfig["snapshot_backups"])
	}
	if backupConfig["with_compression"] != false {
		t.Fatalf("unexpected with_compression: %#v", backupConfig["with_compression"])
	}
}

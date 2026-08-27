package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// backupConfigBody is what GET /clusters/{id}/backup-config returns: a full
// BackupConfig, credentials included and masked. The import endpoint takes a
// BackupLocation and forbids unknown fields, so the two extra keys here are the
// point of the test.
const backupConfigBody = `{
	"bucket_name": "simplyblock-backup-src",
	"region": "eu-central-1",
	"endpoint": "http://minio:9000",
	"secondary_target": 0,
	"with_compression": true,
	"snapshot_backups": true,
	"verify_tls": false,
	"use_path_style": true,
	"credentials": {
		"access_key_id": "**********",
		"secret_access_key": "**********"
	},
	"s3_thread_pool_size": 32
}`

func importTestClient(t *testing.T, handler func(*http.Request) (*http.Response, error)) *webapi.Client {
	t.Helper()
	return &webapi.Client{
		BaseURL:    "http://simplyblock.test",
		HttpClient: &http.Client{Transport: roundTripFunc(handler)},
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestBackupImportSendsBucketLocation(t *testing.T) {
	var importBody map[string]json.RawMessage

	apiClient := importTestClient(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v2/clusters/src-uuid/backup-config":
			return jsonResponse(backupConfigBody), nil
		case "/api/v2/clusters/dst-uuid/backups/import":
			payload, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read import body: %v", err)
			}
			if err := json.Unmarshal(payload, &importBody); err != nil {
				t.Fatalf("unmarshal import body: %v", err)
			}
			return jsonResponse(`{"imported": 3}`), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})

	r := &BackupImportReconciler{}
	location, err := r.fetchBackupLocation(context.Background(), apiClient, "src-uuid")
	if err != nil {
		t.Fatalf("fetchBackupLocation: %v", err)
	}
	if location.BucketName != "simplyblock-backup-src" {
		t.Fatalf("bucket = %q", location.BucketName)
	}

	imported, err := r.importBackup(context.Background(), apiClient, "dst-uuid",
		json.RawMessage(`[{"backup_id":"b1"}]`), location)
	if err != nil {
		t.Fatalf("importBackup: %v", err)
	}
	if imported != 3 {
		t.Fatalf("imported = %d, want 3", imported)
	}

	// The control plane's _ImportManifests requires both; without the location it
	// rejects the body outright, which is how this path broke silently before.
	if _, ok := importBody["metadata"]; !ok {
		t.Fatal("import body has no metadata")
	}
	rawLocation, ok := importBody["location"]
	if !ok {
		t.Fatal("import body has no location")
	}

	var sent map[string]any
	if err := json.Unmarshal(rawLocation, &sent); err != nil {
		t.Fatalf("unmarshal location: %v", err)
	}

	// BackupLocation is extra="forbid", so anything beyond its own fields is a 422
	// — and `credentials` would additionally hand the source cluster's keys back
	// to an endpoint that has no use for them.
	allowed := map[string]bool{
		"bucket_name": true, "region": true, "endpoint": true,
		"secondary_target": true, "with_compression": true,
		"snapshot_backups": true, "verify_tls": true, "use_path_style": true,
	}
	for key := range sent {
		if !allowed[key] {
			t.Errorf("location carries %q, which the import endpoint forbids", key)
		}
	}

	for key, want := range map[string]any{
		"bucket_name":      "simplyblock-backup-src",
		"region":           "eu-central-1",
		"endpoint":         "http://minio:9000",
		"with_compression": true,
		"verify_tls":       false,
		"use_path_style":   true,
	} {
		if sent[key] != want {
			t.Errorf("location[%q] = %v, want %v", key, sent[key], want)
		}
	}
}

func TestBackupImportLocationOmitsUnresolvedFields(t *testing.T) {
	// A cluster backing up to AWS names neither region nor endpoint: the SDK
	// resolves both. Sending "" instead would read as a deliberate choice.
	apiClient := importTestClient(t, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(`{
			"bucket_name": "b",
			"region": null,
			"endpoint": null,
			"secondary_target": 0,
			"with_compression": false,
			"snapshot_backups": true,
			"verify_tls": true,
			"use_path_style": false,
			"credentials": null
		}`), nil
	})

	r := &BackupImportReconciler{}
	location, err := r.fetchBackupLocation(context.Background(), apiClient, "src-uuid")
	if err != nil {
		t.Fatalf("fetchBackupLocation: %v", err)
	}

	encoded, err := json.Marshal(location)
	if err != nil {
		t.Fatalf("marshal location: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(encoded, &sent); err != nil {
		t.Fatalf("unmarshal location: %v", err)
	}

	for _, key := range []string{"region", "endpoint"} {
		if _, present := sent[key]; present {
			t.Errorf("location carries %q; an unresolved field must be absent", key)
		}
	}
}

func TestBackupImportRefusesClusterWithoutBackupConfig(t *testing.T) {
	apiClient := importTestClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"detail":"no backup configuration"}`)),
		}, nil
	})

	r := &BackupImportReconciler{}
	_, err := r.fetchBackupLocation(context.Background(), apiClient, "src-uuid")
	if err == nil {
		t.Fatal("expected an error for a cluster with no backup configuration")
	}
	if !strings.Contains(err.Error(), "no backup configuration") {
		t.Fatalf("error = %v", err)
	}
}

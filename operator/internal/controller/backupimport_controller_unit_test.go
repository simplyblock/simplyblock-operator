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

// exportBody is what GET /clusters/{id}/backups/export returns: manifests
// grouped by the bucket each lives in. Two groups, because a cluster that has
// imported from elsewhere holds backups in more than its own bucket, and the
// import has to keep them apart.
const exportBody = `{
	"schema_version": 1,
	"groups": [
		{
			"location": {"bucket_name": "simplyblock-backup-src", "region": "eu-central-1"},
			"manifests": [{"backup_id": "b1"}]
		},
		{
			"location": {"bucket_name": "imported-from"},
			"manifests": [{"backup_id": "b2"}]
		}
	]
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

func TestBackupImportForwardsTheExportDocument(t *testing.T) {
	var importBody map[string]json.RawMessage
	var paths []string

	apiClient := importTestClient(t, func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/api/v2/clusters/src-uuid/backups/export":
			return jsonResponse(exportBody), nil
		case "/api/v2/clusters/dst-uuid/backups/import":
			payload, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read import body: %v", err)
			}
			if err := json.Unmarshal(payload, &importBody); err != nil {
				t.Fatalf("unmarshal import body: %v", err)
			}
			return jsonResponse(`{"imported": 2}`), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})

	r := &BackupImportReconciler{}
	exported, err := r.exportBackup(context.Background(), apiClient, "src-uuid", "b1")
	if err != nil {
		t.Fatalf("exportBackup: %v", err)
	}

	imported, err := r.importBackup(context.Background(), apiClient, "dst-uuid", exported)
	if err != nil {
		t.Fatalf("importBackup: %v", err)
	}
	if imported != 2 {
		t.Fatalf("imported = %d, want 2", imported)
	}

	// The source cluster is asked for the backups and nothing else. It used to
	// be asked for its backup configuration too, purely to learn the bucket --
	// which made an import require the source to still be up, exactly what a
	// recovery cannot assume.
	for _, path := range paths {
		if strings.Contains(path, "backup-config") {
			t.Errorf("import consulted %s; the export already says where the backups are", path)
		}
	}

	// _ImportManifests forbids unknown fields, so a stray `location` beside the
	// document is a 422 rather than something the control plane ignores.
	if _, ok := importBody["location"]; ok {
		t.Error("import body carries a location; the export document holds them")
	}
	raw, ok := importBody["metadata"]
	if !ok {
		t.Fatal("import body has no metadata")
	}

	var sent struct {
		Groups []struct {
			Location struct {
				BucketName string `json:"bucket_name"`
			} `json:"location"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if len(sent.Groups) != 2 {
		t.Fatalf("sent %d groups, want 2", len(sent.Groups))
	}
	for i, want := range []string{"simplyblock-backup-src", "imported-from"} {
		if got := sent.Groups[i].Location.BucketName; got != want {
			t.Errorf("group %d bucket = %q, want %q", i, got, want)
		}
	}
}

func TestBackupImportRefusesAnEmptyExport(t *testing.T) {
	// A document with groups but no manifests describes nothing. Passing it on
	// would import zero backups and report success.
	apiClient := importTestClient(t, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(`{"schema_version": 1, "groups": [
			{"location": {"bucket_name": "b"}, "manifests": []}
		]}`), nil
	})

	r := &BackupImportReconciler{}
	_, err := r.exportBackup(context.Background(), apiClient, "src-uuid", "b1")
	if err == nil {
		t.Fatal("expected an error for an export describing no backups")
	}
	if !strings.Contains(err.Error(), "no completed backups") {
		t.Fatalf("error = %v", err)
	}
}

func TestBackupImportRefusesAMissingBackup(t *testing.T) {
	apiClient := importTestClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"detail":"not found"}`)),
		}, nil
	})

	r := &BackupImportReconciler{}
	_, err := r.exportBackup(context.Background(), apiClient, "src-uuid", "b1")
	if err == nil {
		t.Fatal("expected an error for a backup the source cluster does not have")
	}
	if !strings.Contains(err.Error(), "not found on source cluster") {
		t.Fatalf("error = %v", err)
	}
}

// What the BackupImport reconciler reads off the control plane's export
// endpoint, and what it refuses.
//
// The tests live here rather than beside a client because there is no client:
// the export and import calls are built by hand against the untyped webapi
// transport, so the response shape is asserted nowhere else and a change to it
// is invisible until a restore has nothing to read.

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

const (
	exportTestCluster = "11111111-1111-1111-1111-111111111111"
	exportTestBackup  = "44444444-4444-4444-4444-444444444444"
)

// exportDocument is one BackupExport as the control plane serves it: manifests
// grouped by the location each was read from, because a cluster can hold
// backups in several buckets at once and a chain never spans two.
const exportDocument = `{"schema_version":1,"groups":[` +
	`{"location":{"bucket":"backups","endpoint":"https://s3.example.invalid"},` +
	`"manifests":[{"backup_id":"` + exportTestBackup + `","s3_id":7}]}]}`

// exportServer answers the export endpoint with one body and returns a
// reconciler pointed at it. The server is built from the spec rather than from a
// bare handler, so a test keeps failing if the endpoint itself moves.
func exportServer(t *testing.T, body string) *BackupImportReconciler {
	t.Helper()
	server := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", false)
	t.Cleanup(server.Close)
	server.Register(http.MethodGet,
		"/api/v2/clusters/"+exportTestCluster+"/backups/export",
		webapimock.RouteResponse{Status: http.StatusOK, Body: body})
	return &BackupImportReconciler{APIClient: webapi.NewClient(server.URL())}
}

// TestExportBackupReadsTheGroupedExportDocument covers the shape the endpoint
// answers with. It returns the body untouched, because the import endpoint
// takes the same document back and decoding it twice would only add a place for
// the two readings to disagree.
func TestExportBackupReadsTheGroupedExportDocument(t *testing.T) {
	r := exportServer(t, exportDocument)

	body, err := r.exportBackup(context.Background(), r.APIClient, exportTestCluster, exportTestBackup)
	if err != nil {
		t.Fatalf("exportBackup: %v", err)
	}

	var got, want any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("returned body is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(exportDocument), &want); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, want) {
		t.Errorf("exportBackup returned %s, want it forwarded verbatim", body)
	}
}

// TestExportBackupRejectsAnExportCarryingNothing covers the negative half: an
// export that names locations but no manifests would import as a success and
// leave the restore with nothing to read, so it fails here instead.
func TestExportBackupRejectsAnExportCarryingNothing(t *testing.T) {
	for name, body := range map[string]string{
		"no groups":                 `{"schema_version":1,"groups":[]}`,
		"a group with no manifests": `{"schema_version":1,"groups":[{"location":{"bucket":"b"},"manifests":[]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := exportServer(t, body)

			if _, err := r.exportBackup(
				context.Background(), r.APIClient, exportTestCluster, exportTestBackup,
			); err == nil || !strings.Contains(err.Error(), "no completed backups") {
				t.Errorf("err = %v, want one naming the empty export", err)
			}
		})
	}
}

// jsonEqual compares two decoded documents by their re-encoding, which orders
// object keys and so makes the comparison independent of the order the wire
// used.
func jsonEqual(a, b any) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(encodedA) == string(encodedB)
}

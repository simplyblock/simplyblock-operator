// The API end to end over httptest: uploading the fixture bundle, reading its
// analysis back, building a document, and the requests the server refuses.

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/analysis"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/cdcedit"
)

// do sends a request to the server as the browser on the agent's machine would.
func do(t *testing.T, s http.Handler, method, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Host = "127.0.0.1:8099"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func upload(t *testing.T, s http.Handler, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("bundle", "bundle.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return do(t, s, http.MethodPost, "/api/bundles", mw.FormDataContentType(), &body)
}

type uploaded struct {
	ID       string            `json:"id"`
	Analysis analysis.Analysis `json:"analysis"`
}

func uploadFixture(t *testing.T, s http.Handler) uploaded {
	t.Helper()
	rec := upload(t, s, bundletest.Archive(t, bundletest.Fixture(t, "bundle-v10"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var out uploaded
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the upload's answer does not parse: %v", err)
	}
	return out
}

func TestAnUploadedBundleIsAnalyzedAndKept(t *testing.T) {
	s := New()
	up := uploadFixture(t, s)

	if up.ID == "" || len(up.Analysis.Workers) != 3 {
		t.Fatalf("the upload answered id %q with %d workers", up.ID, len(up.Analysis.Workers))
	}
	rec := do(t, s, http.MethodGet, "/api/bundles/"+up.ID, "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "worker-02") {
		t.Errorf("reading it back: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, http.MethodGet, "/api/bundles/nothing-like-it", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown bundle answered %d", rec.Code)
	}
}

func TestADocumentIsBuiltFromThePrefill(t *testing.T) {
	s := New()
	up := uploadFixture(t, s)

	rec := do(t, s, http.MethodGet, "/api/bundles/"+up.ID+"/cdc/prefill", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prefill: %d %s", rec.Code, rec.Body)
	}
	var req cdcedit.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(req)
	rec = do(t, s, http.MethodPost, "/api/bundles/"+up.ID+"/cdc", "application/json", bytes.NewReader(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("build: %d %s", rec.Code, rec.Body)
	}
	var built analysis.CDC
	if err := json.Unmarshal(rec.Body.Bytes(), &built); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(built.YAML, "kind: ClusterDeploymentConfig") || len(built.NodeSets) == 0 {
		t.Errorf("the build answered %+v", built)
	}
	for _, f := range built.Findings {
		if f.Severity == analysis.SeverityError {
			t.Errorf("the rebuilt draft has an error: %+v", f)
		}
	}
}

func TestARequestNoDocumentCanStateIsRefused(t *testing.T) {
	s := New()
	up := uploadFixture(t, s)
	body := `{"name":"x","namespace":"simplyblock","mode":"nvme","workers":[{"name":"worker-01","devices":[]}]}`

	rec := do(t, s, http.MethodPost, "/api/bundles/"+up.ID+"/cdc", "application/json", strings.NewReader(body))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "worker-01") {
		t.Errorf("answered %d %s", rec.Code, rec.Body)
	}
}

func TestAFileThatIsNotABundleIsRefused(t *testing.T) {
	if rec := upload(t, New(), []byte("not a bundle")); rec.Code != http.StatusBadRequest {
		t.Errorf("answered %d %s", rec.Code, rec.Body)
	}
}

func TestARequestForAnotherHostIsRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "attacker.example:8099"
	rec := httptest.NewRecorder()
	New().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a request addressed to attacker.example answered %d", rec.Code)
	}
}

func TestThePageIsServed(t *testing.T) {
	rec := do(t, New(), http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("the page answered %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// The API end to end over httptest: uploading the fixture bundle, reading its
// analysis back, building a document, and the requests the server refuses.

package server

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/analysis"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/cdcedit"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/opsedit"
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

// Regression: 2026-10-24-editor-null-workers. A bundle with no CDC whose run
// found no usable device prefilled no worker, and the empty list went out as
// null, which the editor could not read.
func TestThePrefillOfABundleWithoutAUsableDeviceIsAList(t *testing.T) {
	s := New()
	rec := upload(t, s, bundletest.Archive(t, bundletest.Fixture(t, "bundle-run-failed"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var up uploaded
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}

	rec = do(t, s, http.MethodGet, "/api/bundles/"+up.ID+"/cdc/prefill", "", nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("prefill: %d %s", rec.Code, rec.Body)
	}
	if got := string(raw["workers"]); !strings.HasPrefix(got, "[") {
		t.Errorf("workers is %s, want a list", got)
	}
}

func TestTheMetaNamesTheEnvironments(t *testing.T) {
	rec := do(t, New(), http.MethodGet, "/api/meta", "", nil)
	var meta struct {
		Environments []string `json:"environments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("meta: %d %s", rec.Code, rec.Body)
	}
	if len(meta.Environments) != 9 || meta.Environments[0] != "Vanilla" {
		t.Errorf("the environments are %v", meta.Environments)
	}
}

func prefillOf(t *testing.T, s http.Handler, id string) cdcedit.Request {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/bundles/"+id+"/cdc/prefill", "", nil)
	var req cdcedit.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil {
		t.Fatalf("prefill: %d %s", rec.Code, rec.Body)
	}
	return req
}

func TestTheStripeOptionsAreTheEditorsChoices(t *testing.T) {
	s := New()
	up := uploadFixture(t, s)
	body, _ := json.Marshal(prefillOf(t, s, up.ID))

	rec := do(t, s, http.MethodPost, "/api/bundles/"+up.ID+"/cdc/stripes", "application/json", bytes.NewReader(body))
	var out struct {
		Options []analysis.StripeOption `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("stripes: %d %s", rec.Code, rec.Body)
	}
	valid := map[string]bool{}
	for _, o := range out.Options {
		valid[o.Name] = o.Valid
	}
	if !valid["1+1"] || valid["2+1"] {
		t.Errorf("for three workers the options are %v", valid)
	}
}

func TestAForcedJournalSilencesTheTieWarning(t *testing.T) {
	s := New()
	up := uploadFixture(t, s)
	req := prefillOf(t, s, up.ID)
	req.Mode = cdcedit.ModeBlock
	on := true
	req.Cluster.EnableJournalDevice = &on
	// Switching the document's mode switches every worker's, as the editor does:
	// a worker's own mode is what names its devices.
	for i := range req.Workers {
		req.Workers[i].Mode = cdcedit.ModeBlock
		req.Workers[i].Devices = []string{"/dev/nvme0n1", "/dev/nvme1n1", "/dev/nvme2n1"}
	}

	ties := func() int {
		body, _ := json.Marshal(req)
		rec := do(t, s, http.MethodPost, "/api/bundles/"+up.ID+"/cdc", "application/json", bytes.NewReader(body))
		var built analysis.CDC
		if err := json.Unmarshal(rec.Body.Bytes(), &built); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("build: %d %s", rec.Code, rec.Body)
		}
		n := 0
		for _, f := range built.Findings {
			if f.Reason == "JournalDeviceTied" {
				n++
			}
		}
		return n
	}
	if n := ties(); n != 3 {
		t.Errorf("%d tie warnings for three workers of equal disks, want 3", n)
	}
	req.Cluster.ForceJournalDevice = true
	if n := ties(); n != 0 {
		t.Errorf("%d tie warnings with the journal forced", n)
	}
}

func TestTheMetaCarriesTheCollectionCommand(t *testing.T) {
	rec := do(t, New(), http.MethodGet, "/api/meta", "", nil)
	var meta struct {
		Collect struct {
			Command     string `json:"command"`
			WithOptions string `json:"withOptions"`
		} `json:"collect"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("meta: %d %s", rec.Code, rec.Body)
	}
	if meta.Collect.Command != "curl -s -L https://install.simplyblock.io/scripts/discovery-bundler | bash" {
		t.Errorf("the collection command is %q", meta.Collect.Command)
	}
	if !strings.Contains(meta.Collect.WithOptions, "bash -s -- -n <namespace> --run <run>") {
		t.Errorf("the command with options is %q", meta.Collect.WithOptions)
	}
}

// A bundle stays in memory: the multipart parser's own budget is 32 MiB, and
// past it the default handling writes the upload to a temporary file, which is
// customer data on the agent's disk.
func TestALargeUploadNeverTouchesTheDisk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	noise := make([]byte, 40<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	data := bundletest.Archive(t, bundletest.Fixture(t, "bundle-v10"), func(name string, data []byte) ([]byte, bool) {
		if name == "version.txt" {
			return append(data, noise...), true
		}
		return data, true
	})
	if len(data) <= 32<<20 {
		t.Fatalf("the archive is %d bytes, under the parser's budget", len(data))
	}

	if rec := upload(t, New(), data); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the upload left %d file(s) in the temporary directory, the first %s", len(entries), entries[0].Name())
	}
}

func TestTheRunEditorNeedsNoBundle(t *testing.T) {
	s := New()
	rec := do(t, s, http.MethodGet, "/api/operatorops/prefill", "", nil)
	var req opsedit.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("prefill: %d %s", rec.Code, rec.Body)
	}

	req.Discover.Workers = []string{"worker-1"}
	req.Discover.NodeSelector = map[string]string{"role": "storage"}
	body, _ := json.Marshal(req)
	rec = do(t, s, http.MethodPost, "/api/operatorops", "application/json", bytes.NewReader(body))
	var out struct {
		YAML     string            `json:"yaml"`
		Findings []opsedit.Finding `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("build: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(out.YAML, "kind: OperatorOps") || len(out.Findings) == 0 {
		t.Errorf("the build answered %+v", out)
	}
}

func TestTheRunEditorTakesABundlesRun(t *testing.T) {
	s := New()
	up := uploadFixture(t, s)
	rec := do(t, s, http.MethodGet, "/api/operatorops/prefill?bundle="+up.ID, "", nil)
	var req opsedit.Request
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("prefill: %d %s", rec.Code, rec.Body)
	}
	if req.Name != "discover-fleet-03" {
		t.Errorf("the run after discover-fleet-02 is named %q", req.Name)
	}
}

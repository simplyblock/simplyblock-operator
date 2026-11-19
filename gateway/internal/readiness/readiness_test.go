package readiness

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandlerAnswersOKWhenRootHasNoUnmountedBuckets(t *testing.T) {
	root := t.TempDir()
	rec := httptest.NewRecorder()
	handler(root)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHandlerAnswersServiceUnavailableNamingAnUnmountedBucket(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "stale-bucket"), 0o755); err != nil {
		t.Fatalf("creating stale-bucket: %v", err)
	}
	rec := httptest.NewRecorder()
	handler(root)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := rec.Body.String(); !strings.Contains(body, "stale-bucket") {
		t.Errorf("body = %q, want it to name stale-bucket", body)
	}
}

func TestHandlerAnswersInternalErrorWhenTheRootIsGone(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(filepath.Join(t.TempDir(), "missing"))(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

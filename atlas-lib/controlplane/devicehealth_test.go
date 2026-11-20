package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs"
)

func TestDeviceHealthInfoCarriesWhatTheDeviceReported(t *testing.T) {
	var path string
	c := deviceClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": testDeviceID, "supported": true, "source": "spdk",
			"critical_warning": 4, "temperature_celsius": 41,
			"available_spare_percentage": 100, "available_spare_threshold_percentage": 10,
			"percentage_used": 3, "media_errors": 0, "power_on_hours": 12000,
		})
	})

	got, err := c.DeviceHealthInfo(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID)
	if err != nil {
		t.Fatalf("DeviceHealthInfo: %v", err)
	}
	if !strings.HasSuffix(path, "/devices/"+testDeviceID+"/health-info") {
		t.Errorf("path = %s, want the device's health-info", path)
	}
	if !got.Supported || got.Source != "spdk" {
		t.Errorf("supported = %v, source = %q", got.Supported, got.Source)
	}
	if got.CriticalWarning == nil || *got.CriticalWarning != 4 {
		t.Errorf("critical warning = %v, want 4", got.CriticalWarning)
	}
	if got.SMARTPassed != nil || got.ReallocatedSectors != nil {
		t.Error("a field the device did not report was filled in")
	}
}

// An older control plane does not send `supported`, and every device it
// answers for has a health log: absence is not `unsupported`.
func TestAHealthInfoWithoutSupportedIsSupported(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+testDeviceID+`","critical_warning":0}`)
	})
	got, err := c.DeviceHealthInfo(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID)
	if err != nil {
		t.Fatalf("DeviceHealthInfo: %v", err)
	}
	if !got.Supported {
		t.Error("supported = false for a response that did not say so")
	}
}

func TestDeviceHealthInfoReportsNotFound(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err := c.DeviceHealthInfo(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestReportDeviceHealthAssessmentPutsTheWholeAssessment(t *testing.T) {
	var method, path string
	var body map[string]any
	c := deviceClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	})

	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	latency := 1500000.0
	err := c.ReportDeviceHealthAssessment(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID,
		DeviceHealthAssessment{
			Reporter: "simplyblock-operator", ReportedAt: at, Valid: 15 * time.Minute,
			Performance: &DevicePerformanceAssessment{Verdict: "slow", Reason: "r", Since: &at, LatencyMicros: &latency},
			SMART:       &DeviceSMARTAssessment{Verdict: "ok", CheckedAt: &at},
		})
	if err != nil {
		t.Fatalf("ReportDeviceHealthAssessment: %v", err)
	}
	if method != http.MethodPut || !strings.HasSuffix(path, "/devices/"+testDeviceID+"/health-assessment") {
		t.Errorf("%s %s, want PUT .../health-assessment", method, path)
	}
	if body["valid_seconds"] != float64(900) || body["reporter"] != "simplyblock-operator" {
		t.Errorf("body = %v", body)
	}
	perf, _ := body["performance"].(map[string]any)
	if perf["verdict"] != "slow" || perf["latency_us"] != latency {
		t.Errorf("performance = %v", perf)
	}
	smart, _ := body["smart"].(map[string]any)
	if smart["verdict"] != "ok" {
		t.Errorf("smart = %v", smart)
	}
}

func TestReportDeviceHealthAssessmentReportsARefusal(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) })
	err := c.ReportDeviceHealthAssessment(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID,
		DeviceHealthAssessment{ReportedAt: time.Now(), Valid: time.Minute})
	if err == nil {
		t.Error("a 422 was reported as success")
	}
}

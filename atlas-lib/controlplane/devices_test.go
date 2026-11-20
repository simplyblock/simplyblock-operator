package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

const (
	testDeviceCluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
	testDeviceNode    = "a1111111-1111-4111-8111-111111111111"
	testDeviceID      = "b2222222-2222-4222-8222-222222222222"
)

func deviceClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Endpoint: srv.URL, Token: "t"})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return c
}

func TestDeviceReportsWhatTheControlPlaneHolds(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": testDeviceID, "cluster_id": testDeviceCluster,
			"storage_node_id": testDeviceNode, "status": "online",
			"model": "SAMSUNG MZQL2", "serial_number": "S64H", "nvme_controller": "nvme0",
			"pcie_address": "0000:5e:00.0", "size": 1920383410176,
			"cluster_device_order": 0, "io_error": false, "is_partition": false,
			"retries_exhausted": false, "nvmf_ips": []string{},
			"capacity": map[string]any{},
		})
	})

	got, err := c.Device(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID)
	if err != nil {
		t.Fatalf("reading the device: %v", err)
	}
	if got.ID != testDeviceID || got.Status != "online" {
		t.Errorf("Device = %+v, want the id and status the control plane reported", got)
	}
	if got.SerialNumber != "S64H" || got.PCIeAddress != "0000:5e:00.0" {
		t.Errorf("Device = %+v, want the hardware identity carried through", got)
	}
}

// A device the control plane does not hold is a finding a caller tells apart
// from a transport failure, because the two lead to different actions: one
// means the device is gone and the other means to try again.
func TestDeviceReportsNotFound(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if _, err := c.Device(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestRestartDeviceIssuesThePost(t *testing.T) {
	var path, method string
	c := deviceClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
	})

	if err := c.RestartDevice(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); err != nil {
		t.Fatalf("restarting: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if want := "/api/v2/clusters/" + testDeviceCluster + "/storage-nodes/" + testDeviceNode +
		"/devices/" + testDeviceID + "/restart"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
}

// A refused restart is an error rather than a silence. The operation that
// issued it records the step as failed, and a call that reported success on a
// 500 would leave it waiting for a restart nobody performed.
func TestRestartDeviceReportsARefusal(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"device is busy"}`))
	})

	if err := c.RestartDevice(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); err == nil {
		t.Error("a refused restart was reported as performed")
	}
}

// Every identifier is a UUID in the v2 API, so one that is not is refused here
// rather than becoming a request path the control plane answers with a 422.
func TestDeviceCallsRefuseAnIdentifierThatIsNotAUUID(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a malformed identifier reached the control plane")
		w.WriteHeader(http.StatusOK)
	})

	if err := c.RestartDevice(t.Context(), "not-a-uuid", testDeviceNode, testDeviceID); err == nil {
		t.Error("a cluster id that is not a UUID was accepted")
	}
	if _, err := c.Device(t.Context(), testDeviceCluster, testDeviceNode, "nope"); err == nil {
		t.Error("a device id that is not a UUID was accepted")
	}
}

// A Fail is two calls, and the first of them is the removal: the control plane
// refuses to fail a device that is still in the data path, so a client that
// offered only the second would issue a call that can only be refused.
func TestRemoveDeviceIssuesThePost(t *testing.T) {
	var path, method, query string
	c := deviceClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, method, query = r.URL.Path, r.Method, r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.RemoveDevice(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if want := "/api/v2/clusters/" + testDeviceCluster + "/storage-nodes/" + testDeviceNode +
		"/devices/" + testDeviceID + "/remove"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	// The removal is never forced. A force flag is what turns the control
	// plane's own refusals into silence, and every one of them is a state the
	// caller needs to see rather than override.
	if strings.Contains(query, "force=true") {
		t.Errorf("query = %q, want no force", query)
	}
}

func TestFailDeviceIssuesThePost(t *testing.T) {
	var path, method string
	c := deviceClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.FailDevice(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); err != nil {
		t.Fatalf("failing: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if want := "/api/v2/clusters/" + testDeviceCluster + "/storage-nodes/" + testDeviceNode +
		"/devices/" + testDeviceID + "/fail"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
}

// The control plane refuses to fail a device that is not already removed, and
// refuses either call while a migration is running anywhere in the cluster.
// Both arrive as a non-2xx, and reporting one as performed would leave the
// caller waiting for a device to report a state nothing asked it for.
func TestDeviceRemovalAndFailureReportARefusal(t *testing.T) {
	c := deviceClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Device must be in removed status"}`))
	})

	if err := c.RemoveDevice(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); err == nil {
		t.Error("a refused removal was reported as performed")
	}
	if err := c.FailDevice(t.Context(), testDeviceCluster, testDeviceNode, testDeviceID); err == nil {
		t.Error("a refused failure was reported as performed")
	}
}

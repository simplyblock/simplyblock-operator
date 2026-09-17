package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

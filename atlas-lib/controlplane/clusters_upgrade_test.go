// Tests for what a release upgrade reads from and asks of a cluster: the images
// a node is started with when nothing names one, the write-protection
// generation, and the switch to v2. They sit beside the other cluster tests,
// against the same test server, because they are cluster calls.
package controlplane

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// clusterWithImagesDTO is a cluster as a control plane that reports its default
// images and its write-protection generation answers it.
const clusterWithImagesDTO = clusterDTOOpen + `,"write_protection_v2":true,` +
	`"default_spdk_image":"mirror.example.com/simplyblock-io/ultra:26.5.0",` +
	`"default_spdk_proxy_image":"mirror.example.com/simplyblock-io/simplyblock:26.5.0"}`

func TestGetClusterReadsTheDefaultImagesAndWriteProtection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(clusterWithImagesDTO))
	})

	got, err := c.GetCluster(t.Context(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if got.WriteProtectionV2 == nil || !*got.WriteProtectionV2 {
		t.Errorf("WriteProtectionV2 = %v, want true", got.WriteProtectionV2)
	}
	if got.DefaultSpdkImage != "mirror.example.com/simplyblock-io/ultra:26.5.0" {
		t.Errorf("DefaultSpdkImage = %q", got.DefaultSpdkImage)
	}
	if got.DefaultSpdkProxyImage != "mirror.example.com/simplyblock-io/simplyblock:26.5.0" {
		t.Errorf("DefaultSpdkProxyImage = %q", got.DefaultSpdkProxyImage)
	}
}

func TestGetClusterFromAnOlderControlPlaneReportsNoImages(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(clusterDTO))
	})

	got, err := c.GetCluster(t.Context(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if got.WriteProtectionV2 != nil || got.DefaultSpdkImage != "" || got.DefaultSpdkProxyImage != "" {
		t.Errorf("got %+v, want the generation unknown and no images", got)
	}
}

func TestSwitchWriteProtectionPostsTheSwitch(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		want := "/api/v2/clusters/" + testCluster + "/switch-write-protection"
		if r.Method != http.MethodPost || r.URL.Path != want {
			t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, want)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.SwitchWriteProtection(t.Context(), testCluster); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestSwitchWriteProtectionReportsASwitchThatDidNotComplete(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"The write-protection switch did not complete; the cluster stays on v1"}`))
	})

	err := c.SwitchWriteProtection(t.Context(), testCluster)
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 StatusError", err)
	}
	if !strings.Contains(err.Error(), "stays on v1") {
		t.Errorf("err = %v, want the control plane's reason carried", err)
	}
}

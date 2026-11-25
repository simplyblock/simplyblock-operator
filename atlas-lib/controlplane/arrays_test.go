// Tests for the storage-array calls of a multi-attach cluster: what the
// registration sends, that the credentials travel only in the request, and how
// the control plane's view of an array is read back.

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

const arrayDTO = `{"id":"arr-1","plugin":"pure-flasharray","endpoint":"https://pure01.example",` +
	`"options":{"protocol":"iscsi"},"lun_size":1099511627776,"has_credentials":true,` +
	`"credential_ref":{"kind":"managed","backend":"vault"},` +
	`"capabilities":{"protocols":["iscsi","fc"],"max_attachments":0}}`

// A multi-attach cluster is created with multi_attach and its fault tolerance;
// any other cluster sends neither key.
func TestAMultiAttachClusterIsCreatedWithItsFaultTolerance(t *testing.T) {
	for _, tc := range []struct {
		multi   bool
		ftt     int
		wantMA  any
		wantFTT any
	}{{true, 2, true, float64(2)}, {false, 0, nil, nil}} {
		var body map[string]any
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(clusterDTO))
		})
		if _, err := c.CreateCluster(context.Background(), CreateClusterParams{
			Name: "prod", MaxSubsys: 20, SpdkVCPUCount: 8, RaidDevices: tc.multi,
			MultiAttach: tc.multi, FaultTolerance: tc.ftt,
		}); err != nil {
			t.Fatal(err)
		}
		if body["multi_attach"] != tc.wantMA || body["ftt"] != tc.wantFTT {
			t.Errorf("multi=%v: multi_attach=%v ftt=%v, want %v %v",
				tc.multi, body["multi_attach"], body["ftt"], tc.wantMA, tc.wantFTT)
		}
	}
}

func TestClientRegisterArraySendsTheRegistration(t *testing.T) {
	var body map[string]any
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(arrayDTO))
	})

	got, err := c.RegisterArray(context.Background(), testCluster, ArrayRegistration{
		Plugin:       "pure-flasharray",
		Endpoint:     "https://pure01.example",
		Options:      map[string]string{"protocol": "iscsi"},
		Credentials:  map[string]string{"api_token": "T0KEN"},
		LunSizeBytes: 1 << 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/clusters/"+testCluster+"/arrays/") {
		t.Errorf("path = %s", path)
	}
	if body["plugin"] != "pure-flasharray" || body["endpoint"] != "https://pure01.example" ||
		body["lun_size"] != float64(1<<40) {
		t.Errorf("body = %v", body)
	}
	if creds, _ := body["credentials"].(map[string]any); creds["api_token"] != "T0KEN" {
		t.Errorf("credentials = %v", body["credentials"])
	}
	if _, ok := body["credential_ref"]; ok {
		t.Errorf("credential_ref sent without one: %v", body["credential_ref"])
	}
	if opts, _ := body["options"].(map[string]any); opts["protocol"] != "iscsi" {
		t.Errorf("options = %v", body["options"])
	}
	if got.ID != "arr-1" || got.Plugin != "pure-flasharray" || !got.HasCredentials ||
		got.CredentialKind != "managed" || got.LunSizeBytes != 1<<40 {
		t.Errorf("array = %+v", got)
	}
	// The view never carries the credentials back.
	if strings.Contains(got.String(), "T0KEN") {
		t.Errorf("view leaks the credentials: %s", got.String())
	}
}

// An external secret is named instead of sent: no credentials key at all.
func TestClientRegisterArrayWithAnExternalSecret(t *testing.T) {
	var body map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(arrayDTO))
	})
	if _, err := c.RegisterArray(context.Background(), testCluster, ArrayRegistration{
		Plugin:        "netapp-ontap",
		CredentialRef: map[string]string{"kind": "vault-kv", "mount": "secret", "path": "arrays/a1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["credentials"]; ok {
		t.Errorf("credentials sent with an external secret: %v", body["credentials"])
	}
	if ref, _ := body["credential_ref"].(map[string]any); ref["path"] != "arrays/a1" {
		t.Errorf("credential_ref = %v", body["credential_ref"])
	}
}

// A refusal (the plugin could not connect, the credentials were rejected)
// carries the control plane's answer, which is what the cluster reports.
func TestClientRegisterArrayRefusal(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"array plugin 'pure-flasharray' could not connect"}`))
	})
	_, err := c.RegisterArray(context.Background(), testCluster, ArrayRegistration{Plugin: "pure-flasharray"})
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "could not connect") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientListArrays(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + arrayDTO + "]"))
	})
	arrays, err := c.ListArrays(context.Background(), testCluster)
	if err != nil {
		t.Fatal(err)
	}
	if len(arrays) != 1 || arrays[0].ID != "arr-1" || arrays[0].Options["protocol"] != "iscsi" {
		t.Fatalf("arrays = %+v", arrays)
	}

	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.ListArrays(context.Background(), testCluster); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("ListArrays on a missing cluster: %v, want ErrNotFound", err)
	}
}

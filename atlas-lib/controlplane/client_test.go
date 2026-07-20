package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

const (
	testCluster = "11111111-1111-1111-1111-111111111111"
	testPool    = "22222222-2222-2222-2222-222222222222"
	testVolume  = "33333333-3333-3333-3333-333333333333"
	testHandle  = lvol.VolumeHandle(testCluster + ":" + testPool + ":" + testVolume)
)

// newTestClient wires a Client to a test server whose handler h serves the
// control-plane responses.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Endpoint: srv.URL, Token: "sekret"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientVolume(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sekret" {
			t.Errorf("auth header = %q, want Bearer sekret", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/volumes/33333333-3333-3333-3333-333333333333/") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"vol1","pool_name":"pool1","size":20971520,` +
			`"nqn":"nqn.2023-02.io.simplyblock:c:lvol:v"}`))
	})

	v, err := c.Volume(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "vol1" || v.Pool != "pool1" || v.SizeBytes != 20971520 ||
		v.NQN != "nqn.2023-02.io.simplyblock:c:lvol:v" || v.ID != testHandle {
		t.Errorf("mapped volume = %+v", v)
	}
}

func TestClientVolumeNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.Volume(context.Background(), testHandle); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClientConnection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/connect") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"transport":"tcp","ip":"10.10.10.1","port":4420,"nqn":"nqn.x"},` +
			`{"transport":"tcp","ip":"10.10.10.2","port":4420,"nqn":"nqn.x"}]`))
	})

	conn, err := c.Connection(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if conn.NQN != "nqn.x" {
		t.Errorf("NQN = %q, want nqn.x", conn.NQN)
	}
	if len(conn.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2 (multipath)", len(conn.Endpoints))
	}
	if conn.Endpoints[0].Address != "10.10.10.1" || conn.Endpoints[0].Port != 4420 ||
		conn.Endpoints[0].Transport != "tcp" {
		t.Errorf("endpoint[0] = %+v", conn.Endpoints[0])
	}
}

func TestClientListVolumes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/storage-pools/"+testPool+"/volumes/") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testVolume + `","name":"vol1","pool_name":"pool1","size":100,"nqn":"nqn.a"},` +
			`{"id":"44444444-4444-4444-4444-444444444444","name":"vol2","pool_name":"pool1","size":200,"nqn":"nqn.b"}]`))
	})

	vols, err := c.ListVolumes(context.Background(), testCluster, testPool)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 {
		t.Fatalf("got %d volumes, want 2", len(vols))
	}
	if vols[0].ID != testHandle {
		t.Errorf("vols[0].ID = %q, want %q", vols[0].ID, testHandle)
	}
	if vols[1].Name != "vol2" || vols[1].SizeBytes != 200 {
		t.Errorf("vols[1] = %+v", vols[1])
	}
}

func TestClientResizeVolume(t *testing.T) {
	var gotBody string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.ResizeVolume(context.Background(), testHandle, 4096); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"size":4096`) {
		t.Errorf("request body = %q, want it to carry size 4096", gotBody)
	}
}

func TestClientDeleteVolume(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		if err := c.DeleteVolume(context.Background(), testHandle); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("already gone is not an error", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		if err := c.DeleteVolume(context.Background(), testHandle); err != nil {
			t.Errorf("DeleteVolume on a 404 = %v, want nil (idempotent)", err)
		}
	})
}

func TestSplitHandleInvalid(t *testing.T) {
	c := &Client{}
	for _, bad := range []lvol.VolumeHandle{"only-one", "a:b", "x:y:z", "a:b:c:d"} {
		if _, err := c.Volume(context.Background(), bad); err == nil {
			t.Errorf("Volume(%q) expected an error for a malformed handle", bad)
		}
	}
}

func TestClientCreateVolume(t *testing.T) {
	const newID = "99999999-9999-9999-9999-999999999999"
	var body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Location", "/api/v2/clusters/"+testCluster+"/storage-pools/"+testPool+"/volumes/"+newID+"/")
		w.WriteHeader(http.StatusCreated)
	})
	h, err := c.CreateVolume(context.Background(), testCluster, testPool, CreateVolumeParams{Name: "v1", SizeBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if want := lvol.VolumeHandle(testCluster + ":" + testPool + ":" + newID); h != want {
		t.Errorf("handle = %q, want %q", h, want)
	}
	if !strings.Contains(body, `"name":"v1"`) || !strings.Contains(body, `"size":1073741824`) {
		t.Errorf("request body = %q", body)
	}
}

func TestClientCloneVolume(t *testing.T) {
	const newID = "aaaaaaaa-1111-2222-3333-444444444444"
	var body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Location", "/api/v2/clusters/"+testCluster+"/storage-pools/"+testPool+"/volumes/"+newID+"/")
		w.WriteHeader(http.StatusCreated)
	})
	h, err := c.CloneVolume(context.Background(), testCluster, testPool, CloneVolumeParams{Name: "clone1", SnapshotID: "snap-1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := lvol.VolumeHandle(testCluster + ":" + testPool + ":" + newID); h != want {
		t.Errorf("handle = %q, want %q", h, want)
	}
	if !strings.Contains(body, `"snapshot_id":"snap-1"`) {
		t.Errorf("request body = %q", body)
	}
}

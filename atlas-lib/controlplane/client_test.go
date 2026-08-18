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
		_, _ = w.Write([]byte(`{"id":"` + testVolume + `","name":"vol1","pool_name":"pool1",` +
			`"size":20971520,"ns_id":1,"nqn":"nqn.2023-02.io.simplyblock:c:lvol:v"}`))
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
		_, _ = w.Write([]byte(`[{"transport":"tcp","ip":"10.10.10.1","port":4420,"nqn":"nqn.x","ns-id":1},` +
			`{"transport":"tcp","ip":"10.10.10.2","port":4420,"nqn":"nqn.x","ns-id":1}]`))
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

// The /connect entries carry the connect parameters the control plane picked
// for each path (hyphenated keys, matching the nvme connect options); all of
// them have to survive decoding.
func TestClientConnectionCarriesConnectParameters(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"transport":"tcp","ip":"10.10.10.1","port":4420,"nqn":"nqn.x",` +
			`"reconnect-delay":2,"ctrl-loss-tmo":60,"fast-io-fail-tmo":0,"nr-io-queues":8,` +
			`"keep-alive-tmo":5,"host-iface":"eth1","tls":true,"ns-id":3,` +
			`"connect":"nvme connect ...","allowed-hosts":["nqn.host"]}]`))
	})

	conn, err := c.Connection(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if conn.NSID != 3 {
		t.Errorf("NSID = %d, want 3", conn.NSID)
	}
	e := conn.Endpoints[0]
	if e.NrIOQueues != 8 || e.ReconnectDelaySec != 2 || e.KeepAliveTMOSec != 5 {
		t.Errorf("queues/delay/kato = %d/%d/%d, want 8/2/5", e.NrIOQueues, e.ReconnectDelaySec, e.KeepAliveTMOSec)
	}
	if e.CtrlLossTMOSec == nil || *e.CtrlLossTMOSec != 60 {
		t.Errorf("ctrl-loss-tmo = %v, want 60", e.CtrlLossTMOSec)
	}
	// 0 means "fail I/O immediately", which must not be lost as "unset".
	if e.FastIOFailTMOSec == nil || *e.FastIOFailTMOSec != 0 {
		t.Errorf("fast-io-fail-tmo = %v, want 0", e.FastIOFailTMOSec)
	}
	if e.HostIface != "eth1" || !e.TLS {
		t.Errorf("host-iface/tls = %q/%v, want eth1/true", e.HostIface, e.TLS)
	}
}

// An access-controlled volume is resolved for one host: the control plane
// authorizes that NQN and answers with the secret it issued to it, so the NQN
// has to reach the query string.
func TestClientConnectionForHost(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("host_nqn")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"transport":"tcp","ip":"10.10.10.1","port":4420,"nqn":"nqn.x","ns-id":1,` +
			`"connect":"nvme connect --transport=tcp --traddr=10.10.10.1 ` +
			`--hostnqn=nqn.host --dhchap-secret=DHHC-1:00:host-secret: ` +
			`--dhchap-ctrl-secret=DHHC-1:00:ctrl-secret:"}]`))
	})

	conn, err := c.Connection(context.Background(), testHandle, lvol.ForHost("nqn.host"))
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "nqn.host" {
		t.Errorf("host_nqn = %q, want it forwarded to the control plane", gotQuery)
	}
	// The secrets live in the prebuilt connect line and nowhere else.
	e := conn.Endpoints[0]
	if e.DHCHAPSecret != "DHHC-1:00:host-secret:" {
		t.Errorf("DHCHAPSecret = %q, want it from the connect line", e.DHCHAPSecret)
	}
	if e.DHCHAPCtrlSecret != "DHHC-1:00:ctrl-secret:" {
		t.Errorf("DHCHAPCtrlSecret = %q, want it from the connect line", e.DHCHAPCtrlSecret)
	}
}

// Without ForHost the query stays clean rather than carrying an empty host_nqn,
// which the control plane would have to tell apart from an absent one.
func TestClientConnectionWithoutHostOmitsTheParameter(t *testing.T) {
	var hadParam bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, hadParam = r.URL.Query()["host_nqn"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"transport":"tcp","ip":"10.10.10.1","port":4420,"nqn":"nqn.x","ns-id":1,` +
			`"connect":"nvme connect --transport=tcp --traddr=10.10.10.1"}]`))
	})

	conn, err := c.Connection(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if hadParam {
		t.Error("host_nqn present in the query, want it omitted when no host was named")
	}
	// An ungated volume's connect line carries no secrets, which is not an error.
	if e := conn.Endpoints[0]; e.DHCHAPSecret != "" || e.DHCHAPCtrlSecret != "" {
		t.Errorf("dhchap secrets = %q/%q, want none", e.DHCHAPSecret, e.DHCHAPCtrlSecret)
	}
}

func TestClientListVolumes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/storage-pools/"+testPool+"/volumes/") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testVolume + `","name":"vol1","pool_name":"pool1","size":100,` +
			`"ns_id":1,"nqn":"nqn.a"},{"id":"44444444-4444-4444-4444-444444444444","name":"vol2",` +
			`"pool_name":"pool1","size":200,"ns_id":2,"nqn":"nqn.b"}]`))
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

// TestClientConnectionKeyDrift is the end-to-end shape of a version skew: the
// control plane answers 200 with a body that decodes, but under the wrong key
// for the namespace id. The client must refuse it rather than hand back an
// endpoint that would attach namespace 0.
func TestClientConnectionKeyDrift(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"transport":"tcp","ip":"10.10.10.1","port":4420,"nqn":"nqn.x","ns_id":1}]`))
	})

	_, err := c.Connection(context.Background(), testHandle)
	if !errors.Is(err, errs.ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
	if !strings.Contains(err.Error(), "ns-id") {
		t.Errorf("error %q does not name the key the client expected", err)
	}
}

// TestClientConnectionRequiresNamespace covers the promise only /connect makes:
// the endpoint always carries the namespace to attach, so an entry without one
// is a control plane that changed its mind, not a path we can use.
func TestClientConnectionRequiresNamespace(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"transport":"tcp","ip":"10.10.10.1","port":4420,"nqn":"nqn.x","ns-id":null}]`))
	})

	if _, err := c.Connection(context.Background(), testHandle); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

// TestClientVolumeKeyDrift covers the same skew on a typed (spec-modelled)
// response, where the generated client does the decoding.
func TestClientVolumeKeyDrift(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + testVolume + `","name":"vol1","pool":"pool1",` +
			`"size":20971520,"ns_id":1,"nqn":"nqn.2023-02.io.simplyblock:c:lvol:v"}`))
	})

	if _, err := c.Volume(context.Background(), testHandle); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

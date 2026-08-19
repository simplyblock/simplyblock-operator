package cpsim_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"

	cpsim "github.com/simplyblock/simplyblock-operator/test/integration/controlplane"
)

// The simulator is driven through atlas-lib's own control-plane client rather
// than through raw HTTP, because that client is the reason it exists — and it
// strict-decodes every response against the rules in
// atlas-lib/internal/cpapi/validation.yaml. A field named wrong here is a
// failure, not a silently zero value, which is the entire point: the shape it
// answers with is checked by the code that consumes the real one.
const token = "cluster-secret"

// fixture is a control plane with one cluster, one pool and the nodes a test
// asks for.
type fixture struct {
	sim     *cpsim.Server
	client  *controlplane.Client
	cluster uuid.UUID
	pool    uuid.UUID
}

func newFixture(t *testing.T, dataNIC string, qpairs int) *fixture {
	t.Helper()

	f := &fixture{
		sim:     cpsim.New(cpsim.WithToken(token)),
		cluster: uuid.New(),
		pool:    uuid.New(),
	}
	if err := f.sim.Start(); err != nil {
		t.Fatalf("start simulator: %v", err)
	}
	t.Cleanup(func() { _ = f.sim.Close() })

	f.sim.AddCluster(cpsim.Cluster{ID: f.cluster, QpairCount: qpairs, DataNIC: dataNIC})
	f.sim.AddPool(cpsim.Pool{ID: f.pool, ClusterID: f.cluster, Name: "testpool"})

	c, err := controlplane.New(controlplane.Config{Endpoint: f.sim.URL(), Token: token})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	f.client = c
	return f
}

// addNode registers a storage node with one data NIC per address.
func (f *fixture) addNode(hostname string, addrs ...string) uuid.UUID {
	id := uuid.New()
	nics := make([]cpsim.NIC, 0, len(addrs))
	for _, a := range addrs {
		nics = append(nics, cpsim.NIC{IP: a, TrType: "TCP"})
	}
	f.sim.AddNode(cpsim.StorageNode{
		ID: id, ClusterID: f.cluster, Hostname: hostname,
		MgmtIP: "10.5.0.1", Status: "online", DataNICs: nics,
	})
	return id
}

// addVolume registers a volume served by nodes, primary first.
func (f *fixture) addVolume(nodes []uuid.UUID, hosts ...cpsim.AllowedHost) (uuid.UUID, lvol.VolumeHandle) {
	id := uuid.New()
	f.sim.AddVolume(cpsim.Volume{
		ID: id, PoolID: f.pool, ClusterID: f.cluster,
		Name: "vol-" + id.String()[:8], PoolName: "testpool",
		NQN:          "nqn.2023-02.io.simplyblock:" + f.cluster.String() + ":lvol:" + id.String(),
		NSID:         1,
		SizeBytes:    1 << 30,
		Fabric:       "tcp",
		Port:         4420,
		Nodes:        nodes,
		AllowedHosts: hosts,
	})
	return id, lvol.VolumeHandle(fmt.Sprintf("%s:%s:%s", f.cluster, f.pool, id))
}

func TestConnection_SingleNodeVolume(t *testing.T) {
	f := newFixture(t, "eth0", 4)
	node := f.addNode("node-1", "10.5.0.2")
	_, h := f.addVolume([]uuid.UUID{node})

	conn, err := f.client.Connection(context.Background(), h)
	if err != nil {
		t.Fatalf("Connection: %v", err)
	}

	if len(conn.Endpoints) != 1 {
		t.Fatalf("want 1 endpoint, got %d: %+v", len(conn.Endpoints), conn.Endpoints)
	}
	if conn.NSID != 1 {
		t.Errorf("NSID = %d, want 1", conn.NSID)
	}
	if !strings.HasPrefix(conn.NQN, "nqn.") {
		t.Errorf("NQN = %q, want an nqn.* subsystem name", conn.NQN)
	}

	// The tunables are the control plane's own constants, and a client acts on
	// every one of them.
	e := conn.Endpoints[0]
	if e.Transport != "tcp" || e.Address != "10.5.0.2" || e.Port != 4420 {
		t.Errorf("endpoint = %s://%s:%d, want tcp://10.5.0.2:4420", e.Transport, e.Address, e.Port)
	}
	if e.NrIOQueues != 4 {
		t.Errorf("NrIOQueues = %d, want the cluster's qpair count of 4", e.NrIOQueues)
	}
	if e.ReconnectDelaySec != 2 {
		t.Errorf("ReconnectDelaySec = %d, want 2", e.ReconnectDelaySec)
	}
	if e.KeepAliveTMOSec != 4 {
		t.Errorf("KeepAliveTMOSec = %d, want 4 over tcp", e.KeepAliveTMOSec)
	}
	if e.CtrlLossTMOSec == nil || *e.CtrlLossTMOSec != 3600 {
		t.Errorf("CtrlLossTMOSec = %v, want 3600", e.CtrlLossTMOSec)
	}
	if e.FastIOFailTMOSec == nil || *e.FastIOFailTMOSec != 8 {
		t.Errorf("FastIOFailTMOSec = %v, want 8", e.FastIOFailTMOSec)
	}
	if e.HostIface != "eth0" {
		t.Errorf("HostIface = %q, want the cluster's client data NIC", e.HostIface)
	}
	if e.TLS {
		t.Error("TLS set on a volume with no pre-shared key")
	}
}

// An HA volume's endpoints must arrive primary first: that order is the control
// plane's priority order, and the attach path relies on it to bring up the
// optimized path before the others.
func TestConnection_HAVolumeKeepsNodeOrder(t *testing.T) {
	f := newFixture(t, "", 2)
	primary := f.addNode("node-1", "10.5.0.2")
	secondary := f.addNode("node-2", "10.5.0.3")
	_, h := f.addVolume([]uuid.UUID{primary, secondary})

	conn, err := f.client.Connection(context.Background(), h)
	if err != nil {
		t.Fatalf("Connection: %v", err)
	}
	if len(conn.Endpoints) != 2 {
		t.Fatalf("want 2 endpoints, got %d", len(conn.Endpoints))
	}
	if got := []string{conn.Endpoints[0].Address, conn.Endpoints[1].Address}; got[0] != "10.5.0.2" || got[1] != "10.5.0.3" {
		t.Errorf("endpoint order = %v, want the primary 10.5.0.2 first", got)
	}
	if conn.Endpoints[0].HostIface != "" {
		t.Errorf("HostIface = %q, want it unset when the cluster names no data NIC",
			conn.Endpoints[0].HostIface)
	}
}

// One entry per NIC per node, which is how a single node offers two paths.
func TestConnection_OneEndpointPerDataNIC(t *testing.T) {
	f := newFixture(t, "", 1)
	node := f.addNode("node-1", "10.5.0.2", "10.6.0.2")
	_, h := f.addVolume([]uuid.UUID{node})

	conn, err := f.client.Connection(context.Background(), h)
	if err != nil {
		t.Fatalf("Connection: %v", err)
	}
	if len(conn.Endpoints) != 2 {
		t.Fatalf("want 2 endpoints for a two-NIC node, got %d", len(conn.Endpoints))
	}
}

// The secrets have no fields of their own in the response: the control plane puts
// them in the prebuilt command line, and the client parses them back out. A
// simulator that skipped the command line would leave that parsing untested.
func TestConnection_DHCHAPSecretsRideInTheConnectCommand(t *testing.T) {
	f := newFixture(t, "", 1)
	node := f.addNode("node-1", "10.5.0.2")
	const hostNQN = "nqn.2014-08.org.nvmexpress:uuid:11111111-1111-4111-8111-111111111111"
	_, h := f.addVolume([]uuid.UUID{node}, cpsim.AllowedHost{
		NQN:           hostNQN,
		DHCHAPKey:     "DHHC-1:00:host-secret:",
		DHCHAPCtrlKey: "DHHC-1:00:ctrl-secret:",
		PSK:           "NVMeTLSkey-1:01:psk:",
	})

	conn, err := f.client.Connection(context.Background(), h, lvol.ForHost(hostNQN))
	if err != nil {
		t.Fatalf("Connection: %v", err)
	}
	e := conn.Endpoints[0]
	if e.DHCHAPSecret != "DHHC-1:00:host-secret:" {
		t.Errorf("DHCHAPSecret = %q, want it parsed out of the connect command", e.DHCHAPSecret)
	}
	if e.DHCHAPCtrlSecret != "DHHC-1:00:ctrl-secret:" {
		t.Errorf("DHCHAPCtrlSecret = %q, want it parsed out of the connect command", e.DHCHAPCtrlSecret)
	}
	if !e.TLS {
		t.Error("TLS not set on a volume whose host has a pre-shared key")
	}
}

// A volume with an ACL refuses a connect that names no host, and one that names
// a host not on it. Both are 404s carrying the control plane's own message.
func TestConnection_AccessControl(t *testing.T) {
	f := newFixture(t, "", 1)
	node := f.addNode("node-1", "10.5.0.2")
	_, h := f.addVolume([]uuid.UUID{node}, cpsim.AllowedHost{
		NQN: "nqn.2014-08.org.nvmexpress:uuid:11111111-1111-4111-8111-111111111111",
	})

	t.Run("no host named", func(t *testing.T) {
		if _, err := f.client.Connection(context.Background(), h); err == nil {
			t.Fatal("Connection succeeded without a host NQN on an access-controlled volume")
		}
	})

	t.Run("host not allowed", func(t *testing.T) {
		_, err := f.client.Connection(context.Background(), h,
			lvol.ForHost("nqn.2014-08.org.nvmexpress:uuid:22222222-2222-4222-8222-222222222222"))
		if err == nil {
			t.Fatal("Connection succeeded for a host that is not on the list")
		}
	})
}

func TestVolume_Identity(t *testing.T) {
	f := newFixture(t, "", 1)
	node := f.addNode("node-1", "10.5.0.2")
	id, h := f.addVolume([]uuid.UUID{node})

	v, err := f.client.Volume(context.Background(), h)
	if err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if v.Name != "vol-"+id.String()[:8] {
		t.Errorf("Name = %q", v.Name)
	}
	if v.Pool != "testpool" {
		t.Errorf("Pool = %q, want testpool", v.Pool)
	}
	if v.SizeBytes != 1<<30 {
		t.Errorf("SizeBytes = %d, want %d", v.SizeBytes, 1<<30)
	}
}

func TestStoragePool_ResolvedByName(t *testing.T) {
	f := newFixture(t, "", 1)

	p, err := f.client.StoragePoolByName(context.Background(), f.cluster.String(), "testpool")
	if err != nil {
		t.Fatalf("StoragePoolByName: %v", err)
	}
	if p.ID != f.pool.String() {
		t.Errorf("pool ID = %s, want %s", p.ID, f.pool)
	}
}

func TestStorageNodes_Listed(t *testing.T) {
	f := newFixture(t, "", 1)
	f.addNode("node-1", "10.5.0.2")
	f.addNode("node-2", "10.5.0.3")

	nodes, err := f.client.ListStorageNodes(context.Background(), f.cluster.String())
	if err != nil {
		t.Fatalf("ListStorageNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(nodes))
	}
}

// The token is the cluster secret. A simulator that ignored it would let a
// misconfigured client pass here and fail against the real thing.
func TestAuth_RejectsAWrongToken(t *testing.T) {
	f := newFixture(t, "", 1)
	node := f.addNode("node-1", "10.5.0.2")
	_, h := f.addVolume([]uuid.UUID{node})

	wrong, err := controlplane.New(controlplane.Config{Endpoint: f.sim.URL(), Token: "nope"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := wrong.Volume(context.Background(), h); err == nil {
		t.Fatal("Volume succeeded with the wrong bearer token")
	}
}

// An endpoint the simulator does not simulate answers 501, not 404. The
// difference matters when a test starts failing: 404 reads as "the volume is not
// there" and sends the reader after the fixture, while 501 says the simulator is
// what is missing.
func TestUnimplementedEndpoint_Answers501(t *testing.T) {
	f := newFixture(t, "", 1)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		f.sim.URL()+"/api/v2/clusters/", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v2/clusters/: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d for an endpoint the simulator stubs",
			resp.StatusCode, http.StatusNotImplemented)
	}
}

// The routing is the generated one, so a path the spec declares is reachable
// whether or not the simulator answers it — which is the difference between this
// and a hand-registered mux, where an endpoint nobody thought of is a 404
// indistinguishable from a missing resource.
func TestRouting_CoversEveryDeclaredEndpoint(t *testing.T) {
	f := newFixture(t, "", 1)

	// A path that exists in the spec but is not implemented, and one that does
	// not exist at all.
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/v2/clusters/", http.StatusNotImplemented},
		{"/api/v2/not-a-real-path", http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			f.sim.URL()+tc.path, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}
}

func TestVolume_NotFound(t *testing.T) {
	f := newFixture(t, "", 1)
	h := lvol.VolumeHandle(fmt.Sprintf("%s:%s:%s", f.cluster, f.pool, uuid.New()))

	if _, err := f.client.Volume(context.Background(), h); err == nil {
		t.Fatal("Volume succeeded for a volume that was never registered")
	}
}

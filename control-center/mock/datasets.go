package main

import (
	"fmt"
	"strings"
)

// Test data sets. A Scenario is a parameterized world; the generator (gen.go)
// turns one into a coherent object graph — devices reference their nodes,
// slots their PVCs, DRPCs their DRPolicy — with the remaining detail drawn
// from the seeded pools below. `--dataset auto` picks one at random; the same
// --seed always reproduces the same world.

type Scenario struct {
	Name        string
	Description string

	Workers        int // storage workers (each gets a StorageNode)
	MgmtNodes      int
	DevicesPerNode int
	Pools          int
	Volumes        int // PVC/PV pairs spread over the app namespaces
	AppNamespaces  []string
	Tenants        []string // sb-<tenant> namespaces; [0] holds the cluster+DR, extras are empty

	OfflineNodes    int // nodes reported offline/unhealthy
	DegradedDevices int
	WithDR          bool // Ramen kinds + replication chain
	DRFailoverIn    bool // a DRPC caught mid-failover, slots at cutover_pending
	RunningOps      bool // seed in-flight Ops for the simulator to advance
	FailedOps       int  // failed operations in the history
	EventStorm      bool
	Snapshots       int
	Backups         int

	// Several storage clusters per world. Clusters renders that many clusters
	// into the primary tenant namespace (0 and 1 both mean one), and
	// ClusterNamespaces renders one cluster per listed namespace instead.
	// Every cluster gets its own Workers, Pools, Volumes, and so on.
	Clusters          int
	ClusterNamespaces []string

	// Fleet labels the console groups by: storage.simplyblock.io/site, /stage,
	// and /tenant on every StorageCluster when set. Tenant defaults to the
	// cluster namespace without its sb- prefix when ClusterNamespaces is used.
	// SitePerCluster gives every cluster its own site (<Site>-NN) and places
	// its workers in a zone of that name, the many-edge-sites layout.
	Site, Stage, Tenant string
	SitePerCluster      bool
}

func scenarios() []Scenario {
	return []Scenario{
		{
			Name:        "small-healthy",
			Description: "3-node lab cluster in one tenant, everything green",
			Workers:     3, MgmtNodes: 1, DevicesPerNode: 2, Pools: 1, Volumes: 8,
			AppNamespaces: []string{"demo"},
			Tenants:       []string{"tenant-a"},
			Snapshots:     3, Backups: 2,
		},
		{
			Name:        "medium-degraded",
			Description: "8 nodes, one offline, degraded devices, a restart in flight",
			Workers:     8, MgmtNodes: 3, DevicesPerNode: 3, Pools: 2, Volumes: 24,
			AppNamespaces: []string{"prod-db", "prod-queue"},
			OfflineNodes:  1, DegradedDevices: 2, RunningOps: true, FailedOps: 2,
			Snapshots: 8, Backups: 5,
		},
		{
			Name:        "large-scale",
			Description: "32 nodes, four pools, two tenants (one empty, ready to provision)",
			Workers:     32, MgmtNodes: 3, DevicesPerNode: 4, Pools: 4, Volumes: 120,
			AppNamespaces: []string{"prod-db", "prod-queue", "analytics", "ci"},
			Tenants:       []string{"tenant-a", "tenant-b"},
			OfflineNodes:  0, RunningOps: true, FailedOps: 1,
			Snapshots: 24, Backups: 12,
		},
		{
			Name:        "dr-failover",
			Description: "two-site DR, a DRPC mid-failover, slots at cutover",
			Workers:     6, MgmtNodes: 3, DevicesPerNode: 3, Pools: 2, Volumes: 16,
			AppNamespaces: []string{"prod-db", "prod-web"},
			WithDR:        true, DRFailoverIn: true, RunningOps: true,
			Snapshots: 6, Backups: 6,
		},
		{
			Name:        "chaos",
			Description: "everything at once: failures, storms, DR, migrations, two tenants",
			Workers:     10, MgmtNodes: 3, DevicesPerNode: 3, Pools: 3, Volumes: 40,
			AppNamespaces: []string{"prod-db", "prod-queue", "prod-web", "batch"},
			Tenants:       []string{"tenant-a", "tenant-b"},
			OfflineNodes:  2, DegradedDevices: 4, WithDR: true, RunningOps: true,
			FailedOps: 5, EventStorm: true,
			Snapshots: 12, Backups: 8,
		},
	}
}

func scenarioByName(name string) (Scenario, bool) {
	for _, s := range scenarios() {
		if s.Name == name {
			return s, true
		}
	}
	return Scenario{}, false
}

// ---- fleets ---------------------------------------------------------------------
//
// A Fleet is what a console configured with several Kubernetes contexts sees:
// one world per context, each served under `/k8s/<name>/` and `/cp/<name>/`, with
// the labels the console falls back to when neither the StorageCluster nor
// its Namespace carries site, stage, or tenant. A plain scenario is a
// one-context fleet named local.

type FleetContext struct {
	Name     string
	Labels   map[string]string // site, stage, tenant: the context's configured labels
	Scenario Scenario
	Seed     uint64 // 0 derives the world's seed from --seed and the context name
}

type Fleet struct {
	Name        string
	Description string
	Contexts    []FleetContext
}

// edgeScenario is the small two-worker cluster an edge site runs, placed in
// the system namespace the way a single-purpose edge Kubernetes cluster is.
func edgeScenario(site string) Scenario {
	return Scenario{
		Name:        site,
		Description: "two-worker edge cluster at " + site,
		Workers:     2, MgmtNodes: 1, DevicesPerNode: 2, Pools: 1, Volumes: 4,
		AppNamespaces:     []string{"edge-apps"},
		ClusterNamespaces: []string{"simplyblock"},
		Site:              site,
		Snapshots:         1, Backups: 1,
	}
}

func fleets() []Fleet {
	stage := func(ctx, dataset string) FleetContext {
		sc, _ := scenarioByName(dataset)
		return FleetContext{Name: ctx, Labels: map[string]string{"stage": ctx}, Scenario: sc}
	}
	tenants := Scenario{
		Name:        "tenants",
		Description: "one cluster per tenant namespace",
		Workers:     3, MgmtNodes: 3, DevicesPerNode: 2, Pools: 2, Volumes: 10,
		AppNamespaces:     []string{"acme-apps", "globex-apps", "initech-apps"},
		ClusterNamespaces: []string{"sb-acme", "sb-globex", "sb-initech"},
		Stage:             "prd", RunningOps: true, FailedOps: 1,
		Snapshots: 4, Backups: 2,
	}
	namespaces := Scenario{
		Name:        "namespaces",
		Description: "two clusters in sb-blue and sb-green",
		Workers:     4, MgmtNodes: 3, DevicesPerNode: 2, Pools: 2, Volumes: 12,
		AppNamespaces:     []string{"blue-apps", "green-apps"},
		ClusterNamespaces: []string{"sb-blue", "sb-green"},
		OfflineNodes:      1, DegradedDevices: 1, RunningOps: true,
		Snapshots: 4, Backups: 2,
	}
	edgeShared := Scenario{
		Name:        "edge-shared",
		Description: "twelve small edge clusters in one Kubernetes cluster",
		Workers:     2, MgmtNodes: 3, DevicesPerNode: 2, Pools: 1, Volumes: 3,
		AppNamespaces: []string{"edge-apps"},
		Clusters:      12, Site: "edge", SitePerCluster: true,
		OfflineNodes: 1, DegradedDevices: 1, RunningOps: true,
		Snapshots: 1, Backups: 1,
	}
	var sites []FleetContext
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("edge-%02d", i)
		sites = append(sites, FleetContext{Name: name, Labels: map[string]string{"site": name}, Scenario: edgeScenario(name)})
	}
	return []Fleet{
		{
			Name:        "fleet-stages",
			Description: "three contexts by lifecycle stage: dev, uat, prd",
			Contexts:    []FleetContext{stage("dev", "small-healthy"), stage("uat", "medium-degraded"), stage("prd", "large-scale")},
		},
		{
			Name:        "fleet-tenants",
			Description: "one context, one cluster per sb-<tenant> namespace",
			Contexts:    []FleetContext{{Name: "hub", Labels: map[string]string{"stage": "prd"}, Scenario: tenants}},
		},
		{
			Name:        "fleet-namespaces",
			Description: "one context, clusters in sb-blue and sb-green",
			Contexts:    []FleetContext{{Name: "shared", Labels: map[string]string{}, Scenario: namespaces}},
		},
		{
			Name:        "fleet-edge-shared",
			Description: "one context, twelve two-worker edge clusters with their own sites and zones",
			Contexts:    []FleetContext{{Name: "edge", Labels: map[string]string{"stage": "prd"}, Scenario: edgeShared}},
		},
		{
			Name:        "fleet-edge-sites",
			Description: "eight edge contexts, one small cluster each",
			Contexts:    sites,
		},
	}
}

// fleetByName resolves a fleet or a plain scenario (as a one-context fleet
// named local).
func fleetByName(name string) (Fleet, bool) {
	for _, f := range fleets() {
		if f.Name == name {
			return f, true
		}
	}
	if sc, ok := scenarioByName(name); ok {
		return oneContextFleet(sc), true
	}
	return Fleet{}, false
}

func oneContextFleet(sc Scenario) Fleet {
	return Fleet{Name: sc.Name, Description: sc.Description, Contexts: []FleetContext{{Name: "local", Labels: map[string]string{}, Scenario: sc}}}
}

// datasetNames is the --dataset help text: every scenario, then every fleet.
func datasetNames() string {
	var names []string
	for _, s := range scenarios() {
		names = append(names, s.Name)
	}
	for _, f := range fleets() {
		names = append(names, f.Name)
	}
	return strings.Join(names, " | ")
}

// ---- pools the generator draws from ------------------------------------------

var nvmeModels = []struct {
	Model     string
	SizeTB    float64
	SerialPfx string
}{
	{"SAMSUNG MZQL21T9HCJR-00A07", 1.92, "S64FNE0R"},
	{"SAMSUNG MZQL23T8HCLS-00A07", 3.84, "S64HNE0T"},
	{"KIOXIA KCD81RUG3T84", 3.84, "8DQ0A0"},
	{"KIOXIA KCD81RUG7T68", 7.68, "8DS0A1"},
	{"INTEL SSDPF2KX038T1", 3.84, "PHAX2"},
	{"Micron_7450_MTFDKCC3T2TFS", 3.2, "2313"},
	{"Dell Ent NVMe CM6 RI 3.84TB", 3.84, "Y2Q0A"},
}

var dcSites = []string{"fra1", "ams2", "iad1", "sin1"}

var appVolumeNames = []string{
	"data", "wal", "logs", "index", "journal", "state", "cache", "segments",
}

var appWorkloads = []string{
	"postgres", "kafka", "redis", "elastic", "minio", "mongo", "clickhouse", "etcd",
}

var eventReasons = []struct {
	Type, Reason, Message string
}{
	{"Normal", "NodeOnline", "storage node reported online"},
	{"Normal", "DeviceRestarted", "device restarted and rejoined the cluster map"},
	{"Normal", "VolumeProvisioned", "logical volume provisioned"},
	{"Normal", "SnapshotCreated", "snapshot created"},
	{"Normal", "MigrationCompleted", "volume migration completed"},
	{"Warning", "HighLatency", "p99 latency above baseline for 5m"},
	{"Warning", "DeviceDegraded", "device reported degraded: media errors increasing"},
	{"Warning", "CapacityWarning", "pool utilization crossed the warning threshold"},
	{"Warning", "NodeUnreachable", "storage node missed 3 keepalive windows"},
	{"Warning", "ReplicationLagging", "replication slot lag above interval"},
}

var taskTypes = []string{
	"device_restart", "node_restart", "new_device_migration", "failed_device_migration",
	"port_restart", "node_add",
}

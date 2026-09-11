package main

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

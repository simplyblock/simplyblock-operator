package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// The generator turns a Scenario into a coherent world inside the Store.
// Coherence is the point: every StorageDevice's nodeRef names a StorageNode
// that exists, every ReplicationSlot's pvcRef a bound PVC, every DRPC the
// DRPolicy, every PV handle a cluster and pool UUID the CRDs report, because
// a UI is tested by following exactly those links.
//
// The object formats follow the operator and the CSI driver: PV handles are
// <clusterUUID>:<poolUUID>:<lvolUUID>, snapshot handles the same triple,
// StorageClasses are simplyblock-<ns>-<cluster>-<pool>, workers carry
// simplyblock.io/storage-node-uuid.<clusterUUID>.<socket> labels, the node
// API runs in the StorageNodeSet DaemonSet pod and SPDK in the backend's
// snode-spdk-pod-<rpcPort>-<clusterUUID>.
//
// A world holds one or several storage clusters (Scenario.Clusters,
// Scenario.ClusterNamespaces); the world-level objects (namespaces, workers,
// control plane, tenancy scaffold, RBAC) are built once and the per-cluster
// objects once per cluster, each cluster from its own sub-context.

type genCtx struct {
	st   *Store
	rng  *rand.Rand
	sc   Scenario
	now  time.Time
	site string

	// The hub namespace layout (tenancy.go). ns is the cluster's namespace
	// (sb-<tenant>, or one of ClusterNamespaces) — the RBAC target most
	// storage objects live in.
	ns      string   // the cluster namespace
	sysNs   string   // simplyblock-system (control plane)
	mcNs    string   // sb-mc-<managed>   (transport: projected intent + status)
	mcName  string   // managed cluster name
	tenant  string   // primary tenant (holds the generated cluster + DR)
	tenants []string // all tenants (extras are provisioned-but-empty)

	// Per-cluster state; the world context carries the first cluster's after
	// the cluster loop so the world-level builders can refer to it.
	clusterIdx   int
	clusterName  string
	clusterUUID  string
	clusterSite  string            // the site label; "" when the scenario sets none
	zone         string            // the zone the cluster's workers sit in
	tenantLabel  string            // storage.simplyblock.io/tenant, when set
	prefix       string            // object-name prefix when clusters share a namespace
	nodeSetName  string            // the cluster's StorageNodeSet
	workerNames  []string          // the cluster's workers
	nodes        []genNode         // the cluster's storage nodes, in order
	nodeUUIDs    []string          // nodes[i].uuid, kept for the DR builders
	poolNames    []string          // pool CR names
	poolUUIDs    map[string]string // pool CR name -> backend uuid
	pvcRefs      []pvcRef          // every generated PVC, for snapshots/backups/slots
	drCluster    string
	allWorkers   []string // every worker of the world, for the control plane objects
	clusters     []*genCtx
	workerCounts map[string]int // workers numbered per site
}

// genNode is what the pods, labels, tasks, and event log need to know about
// a storage node after nodesAndDevices built it.
type genNode struct {
	name, uuid, worker, status string
	rpcPort                    int
	devices                    []genDevice
}

type genDevice struct {
	name, uuid, status string
}

type pvcRef struct {
	Namespace, Name, VolumeID, PoolUUID, StorageClass string
	SizeGi                                            int
}

// clusterPlan is where one cluster goes before anything is created.
type clusterPlan struct {
	name, ns, site, zone, tenant, prefix string
	workers                              []string
}

func Generate(st *Store, sc Scenario, seed uint64, systemNs string) {
	g := &genCtx{
		st:  st,
		rng: rand.New(rand.NewPCG(seed, seed^0xda3e39cb94b95bdb)),
		sc:  sc, now: time.Now().UTC(),
		drCluster:    "cluster2",
		workerCounts: map[string]int{},
	}
	g.site = dcSites[g.rng.IntN(int(len(dcSites)))]
	if sc.Site != "" {
		g.site = sc.Site
	}
	g.mcName = fmt.Sprintf("prod-%s-1", g.site)
	g.sysNs = systemNs
	g.mcNs = nsManagedCluster(g.mcName)
	g.tenants = sc.Tenants
	if len(g.tenants) == 0 {
		g.tenants = []string{"tenant-a"}
	}
	g.tenant = g.tenants[0]
	g.ns = nsTenant(g.tenant)

	plans := g.clusterPlans()
	// the world builders refer to the first cluster
	g.clusterName, g.ns, g.clusterUUID = plans[0].name, plans[0].ns, g.uuid()

	g.namespaces(plans)
	g.k8sNodes(plans)
	g.tenancyScaffold() // ManagedCluster, NodePoolAllocation, StorageClusterClass

	for i, plan := range plans {
		cg := g.forCluster(i, plan)
		cg.storageCluster()
		if i == 0 {
			cg.projectStorageCluster() // transport copy in sb-mc-* (hub→agent, drift)
		}
		cg.nodesAndDevices()
		cg.storageNodePods()
		cg.poolsAndClasses()
		cg.volumes()
		cg.snapshots()
		cg.backups()
		cg.tasks()
		cg.opsHistory()
		if sc.WithDR && i == 0 {
			cg.replicationChain()
			cg.drApplications()
			cg.ramen()
		}
		cg.events()
		cg.seedEventLog()
		g.clusters = append(g.clusters, cg)
		g.allWorkers = append(g.allWorkers, cg.workerNames...)
	}
	g.adopt(g.clusters[0])

	g.workloads()
	g.controlPlanePods()
	g.secrets()
	g.controlPlaneObjects()
	g.accessControl() // sb:* ClusterRoles, RoleBindings, AccessGrants
}

// clusterPlans lays the clusters out: one per ClusterNamespaces entry, else
// Clusters of them in the primary tenant namespace. Clusters sharing a
// namespace get a name prefix so their nodes, pools, and ops never collide.
func (g *genCtx) clusterPlans() []clusterPlan {
	var plans []clusterPlan
	switch {
	case len(g.sc.ClusterNamespaces) > 0:
		for i, ns := range g.sc.ClusterNamespaces {
			plans = append(plans, g.planCluster(i, ns))
		}
	default:
		for i := 0; i < max(1, g.sc.Clusters); i++ {
			plans = append(plans, g.planCluster(i, g.ns))
		}
	}
	perNs := map[string]int{}
	for _, p := range plans {
		perNs[p.ns]++
	}
	for i := range plans {
		if perNs[plans[i].ns] > 1 {
			plans[i].prefix = plans[i].name + "-"
		}
	}
	return plans
}

func (g *genCtx) planCluster(i int, ns string) clusterPlan {
	p := clusterPlan{name: fmt.Sprintf("cluster%d", i+1), ns: ns, site: g.sc.Site, tenant: g.sc.Tenant}
	if g.sc.SitePerCluster {
		p.site = fmt.Sprintf("%s-%02d", g.site, i+1)
		p.name = p.site
		p.zone = p.site
	}
	if p.tenant == "" && len(g.sc.ClusterNamespaces) > 0 && strings.HasPrefix(ns, "sb-") {
		p.tenant = strings.TrimPrefix(ns, "sb-")
	}
	workerSite := g.site
	if p.site != "" {
		workerSite = p.site
	}
	for j := 0; j < g.sc.Workers; j++ {
		g.workerCounts[workerSite]++
		p.workers = append(p.workers, fmt.Sprintf("worker-%s-%02d", workerSite, g.workerCounts[workerSite]))
	}
	return p
}

// forCluster derives the sub-context one cluster is built from; the rng is
// shared so the world stays a pure function of the seed.
func (g *genCtx) forCluster(i int, p clusterPlan) *genCtx {
	cg := *g
	cg.clusterIdx = i
	cg.clusterName, cg.ns = p.name, p.ns
	cg.clusterUUID = g.clusterUUID
	if i > 0 {
		cg.clusterUUID = g.uuid()
	}
	cg.clusterSite, cg.zone, cg.tenantLabel, cg.prefix = p.site, p.zone, p.tenant, p.prefix
	cg.nodeSetName = p.prefix + "sb-primary-nodes"
	cg.workerNames = p.workers
	cg.nodes, cg.nodeUUIDs, cg.poolNames, cg.pvcRefs = nil, nil, nil, nil
	cg.poolUUIDs = map[string]string{}
	cg.clusters = nil
	return &cg
}

// adopt copies a cluster's state back into the world context.
func (g *genCtx) adopt(c *genCtx) {
	g.clusterName, g.ns, g.clusterUUID = c.clusterName, c.ns, c.clusterUUID
	g.clusterSite, g.zone, g.tenantLabel, g.prefix = c.clusterSite, c.zone, c.tenantLabel, c.prefix
	g.nodeSetName, g.workerNames, g.nodes, g.nodeUUIDs = c.nodeSetName, c.workerNames, c.nodes, c.nodeUUIDs
	g.poolNames, g.poolUUIDs, g.pvcRefs = c.poolNames, c.poolUUIDs, c.pvcRefs
}

// ---- helpers ------------------------------------------------------------------

func (g *genCtx) def(group, resource string) ResourceDef {
	version := "v1alpha1"
	switch group {
	case "", ocmGroup, kvGroup, rbacGroup, apiExtGroup, "apps", "storage.k8s.io", "snapshot.storage.k8s.io", "networking.k8s.io":
		version = "v1"
	}
	if resource == "clusterdeploymentconfigs" || resource == "operatorops" {
		version = "v1alpha2"
	}
	d, ok := g.st.Lookup(group, version, resource)
	if !ok {
		panic("generator uses unregistered resource " + group + "/" + resource)
	}
	return d
}

func (g *genCtx) add(group, resource, ns, name string, spec, status map[string]any, extra map[string]any) map[string]any {
	d := g.def(group, resource)
	obj := map[string]any{
		"metadata": map[string]any{
			"name":              name,
			"uid":               g.uuid(),
			"creationTimestamp": g.past(240),
		},
	}
	if spec != nil {
		obj["spec"] = spec
	}
	if status != nil {
		obj["status"] = status
	}
	for k, v := range extra {
		if k == "labels" || k == "annotations" {
			obj["metadata"].(map[string]any)[k] = v
		} else {
			obj[k] = v
		}
	}
	// Paradigm stamping: every simplyblock kind carries the drift primitives
	// the design mandates (design-crd-model.md §7.9) — actor provenance,
	// managed-by, and status.observedGeneration — so the mock's world matches
	// the target model even though main sets these on almost nothing.
	if group == sbGroup {
		g.stampParadigm(obj, resource)
	}
	created, err := g.st.Create(d, ns, obj)
	if err != nil {
		panic(fmt.Sprintf("generator conflict for %s/%s %s: %v", group, resource, name, err))
	}
	return created
}

// stampParadigm adds actor annotations, the managed-by label, and
// status.observedGeneration (= generation 1 at create) to a hub-authored
// simplyblock object, plus a standard Ready/Synced condition for entity kinds.
func (g *genCtx) stampParadigm(obj map[string]any, resource string) {
	meta := obj["metadata"].(map[string]any)
	stampActor(meta, "admin@simplyblock.io", g.uuid(), "sb-infra-admins,platform-eng", g.past(240))
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
		meta["labels"] = labels
	}
	if _, ok := labels[labManagedBy]; !ok {
		labels[labManagedBy] = "hub"
	}
	st, _ := obj["status"].(map[string]any)
	if st == nil {
		return
	}
	// observedGeneration == generation means "the reported status is current"
	// (design §7.9: without it a stale status and a disagreeing one look the same).
	if _, ok := st["observedGeneration"]; !ok {
		st["observedGeneration"] = float64(1)
	}
	// Entity kinds (not *ops, not tasks) get a standardized conditions surface;
	// ops kinds keep their phase model, replication kinds already carry conditions.
	if _, has := st["conditions"]; !has && isEntityKind(resource) {
		st["conditions"] = readyCondition("Ready", "True", "Reconciled", "resource reconciled", g.recent(30), 1)
	}
}

func isEntityKind(resource string) bool {
	switch resource {
	case "storageclusters", "storagenodes", "storagenodesets", "storagedevices",
		"storagepools", "controlplanes", "managedclusters", "storageclusterclasses",
		"nodepoolallocations", "protectedapplications":
		return true
	}
	return false
}

func (g *genCtx) uuid() string            { return uuidFrom(g.rng) }
func (g *genCtx) pct(p int) bool          { return int(g.rng.UintN(100)) < p }
func (g *genCtx) pick(ss []string) string { return ss[g.rng.IntN(int(len(ss)))] }

// past returns an RFC3339 timestamp up to maxHours in the past.
func (g *genCtx) past(maxHours int) string {
	d := time.Duration(g.rng.IntN(int(maxHours)*3600)) * time.Second
	return g.now.Add(-d).Format(time.RFC3339)
}

func (g *genCtx) recent(maxMinutes int) string {
	d := time.Duration(g.rng.IntN(int(maxMinutes)*60)) * time.Second
	return g.now.Add(-d).Format(time.RFC3339)
}

func gib(n int) string { return fmt.Sprintf("%dGi", n) }

// name prefixes an object name when clusters share a namespace.
func (g *genCtx) name(s string) string { return g.prefix + s }

// nodeName is the StorageNode name of the cluster's i-th node (1-based).
func (g *genCtx) nodeName(i int) string { return g.name(fmt.Sprintf("sn-%02d", i)) }

// clusterLabels are the fleet labels a StorageCluster carries when the
// scenario sets them.
func (g *genCtx) clusterLabels() map[string]any {
	labels := map[string]any{}
	if g.clusterSite != "" {
		labels["storage.simplyblock.io/site"] = g.clusterSite
	}
	if g.sc.Stage != "" {
		labels["storage.simplyblock.io/stage"] = g.sc.Stage
	}
	if g.tenantLabel != "" {
		labels["storage.simplyblock.io/tenant"] = g.tenantLabel
	}
	return labels
}

// ---- world building -----------------------------------------------------------

// namespaces builds the hub tenancy layout (§2.1) with hierarchy labels
// (§2.2). Structure is in labels, not names, because namespace names are DNS
// labels: the scope tree, RoleBinding propagation and admission bindings all
// select on these labels.
func (g *genCtx) namespaces(plans []clusterPlan) {
	nsDef := g.def("", "namespaces")
	mk := func(name string, labels map[string]any) {
		if _, err := g.st.Get(nsDef, "", name); err == nil {
			return
		}
		labels["kubernetes.io/metadata.name"] = name
		g.add("", "namespaces", "", name, nil,
			map[string]any{"phase": "Active"},
			map[string]any{"labels": labels})
	}
	// plain namespaces
	for _, n := range []string{"default", "kube-system"} {
		mk(n, map[string]any{})
	}
	// control plane
	mk(g.sysNs, map[string]any{labScopeKind: "system"})
	// managed cluster (transport)
	mk(g.mcNs, map[string]any{
		labScopeKind: scopeManagedCluster, labManagedCluster: g.mcName,
	})
	// tenant namespaces (the RBAC target). A tenant holds one or more whole
	// clusters plus its DR-policies and DR-applications; global admins create
	// and manage these. The primary tenant carries the generated cluster + DR;
	// extra tenants are provisioned-but-empty, ready for a tenant-admin to fill.
	for i, t := range g.tenants {
		labels := map[string]any{labScopeKind: scopeTenant, labTenant: t}
		if i == 0 {
			labels[labManagedCluster] = g.mcName
			labels[labStorageCluster] = g.clusterName
		}
		mk(nsTenant(t), labels)
	}
	// the namespaces ClusterNamespaces names, as tenant namespaces
	for _, p := range plans {
		labels := map[string]any{labScopeKind: scopeTenant, labManagedCluster: g.mcName}
		if p.tenant != "" {
			labels[labTenant] = p.tenant
		}
		mk(p.ns, labels)
	}
	// application namespaces (k8s workloads the recipe editor discovers)
	for _, n := range g.sc.AppNamespaces {
		mk(n, map[string]any{labScopeKind: scopeApplication})
	}
}

// k8sNodes creates the management nodes and every cluster's workers. The
// storage-node-uuid labels come later, when nodesAndDevices knows the uuids.
func (g *genCtx) k8sNodes(plans []clusterPlan) {
	mk := func(name, role, zone string, storage bool, nodeSet string) {
		labels := map[string]any{
			"kubernetes.io/hostname":          name,
			"topology.kubernetes.io/zone":     zone,
			"topology.kubernetes.io/region":   g.site,
			"node-role.kubernetes.io/" + role: "",
		}
		if storage {
			labels["io.simplyblock.node-type"] = "simplyblock-storage-plane"
			labels["io.simplyblock.storagenodeset"] = nodeSet
		}
		g.add("", "nodes", "", name, map[string]any{}, map[string]any{
			"capacity": map[string]any{
				"cpu": fmt.Sprint(16 + 16*g.rng.UintN(4)), "memory": gib(64 + 64*int(g.rng.UintN(4))), "pods": "110",
			},
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "KubeletReady",
				"lastTransitionTime": g.past(240),
			}},
			"addresses": []any{
				map[string]any{"type": "InternalIP", "address": fmt.Sprintf("10.%d.%d.%d", 10+g.rng.UintN(4), g.rng.UintN(255), 2+g.rng.UintN(250))},
				map[string]any{"type": "Hostname", "address": name},
			},
			"nodeInfo": map[string]any{
				"kubeletVersion": "v1.31.4", "osImage": "Ubuntu 24.04.1 LTS",
				"containerRuntimeVersion": "containerd://1.7.23", "kernelVersion": "6.8.0-49-generic",
			},
		}, map[string]any{"labels": labels})
	}
	for i := 1; i <= g.sc.MgmtNodes; i++ {
		mk(fmt.Sprintf("mgmt-%s-%d", g.site, i), "control-plane", g.site+string(rune('a'+g.rng.UintN(3))), false, "")
	}
	for _, p := range plans {
		for _, name := range p.workers {
			zone := p.zone
			if zone == "" {
				zone = g.site + string(rune('a'+g.rng.UintN(3)))
			}
			mk(name, "worker", zone, true, p.prefix+"sb-primary-nodes")
		}
	}
}

func (g *genCtx) storageCluster() {
	g.add(sbGroup, "storageclusters", g.ns, g.clusterName,
		map[string]any{
			"stripe":               map[string]any{"dataChunks": 2, "parityChunks": 1},
			"fabricType":           "tcp",
			"maxSubsystemCount":    500,
			"rpcBasePort":          8080,
			"nvmfBasePort":         4420,
			"warningThreshold":     map[string]any{"capacity": 75, "provisionedCapacity": 180},
			"criticalThreshold":    map[string]any{"capacity": 90, "provisionedCapacity": 220},
			"enableFailureDomains": g.sc.Workers >= 6,
		},
		map[string]any{
			"uuid":                g.clusterUUID,
			"clusterName":         g.clusterName,
			"phase":               "Active",
			"status":              "active",
			"configured":          true,
			"storageNodes":        float64(g.sc.Workers),
			"mgmtNodes":           float64(g.sc.MgmtNodes),
			"maxFaultTolerance":   float64(1),
			"erasureCodingScheme": "2+1",
			"nqn":                 "nqn.2023-02.io.simplyblock:" + g.clusterUUID,
			"created":             g.past(2400),
			"lastUpdated":         g.recent(10),
		}, map[string]any{"labels": g.clusterLabels()})
}

func (g *genCtx) nodesAndDevices() {
	offline := map[int]bool{}
	for len(offline) < min(g.sc.OfflineNodes, g.sc.Workers) {
		offline[int(g.rng.IntN(int(g.sc.Workers)))] = true
	}
	degradedLeft := g.sc.DegradedDevices
	nodeDef := g.def("", "nodes")

	var setNodes []any
	for i, worker := range g.workerNames {
		nodeUUID := g.uuid()
		g.nodeUUIDs = append(g.nodeUUIDs, nodeUUID)
		name := g.nodeName(i + 1)
		status := "online"
		health := true
		if offline[i] {
			status, health = "offline", false
		}
		total := int64(0)
		rpcPort := 8080 + i
		node := genNode{name: name, uuid: nodeUUID, worker: worker, status: status, rpcPort: rpcPort}

		model := nvmeModels[g.rng.IntN(int(len(nvmeModels)))]
		for d := 0; d < g.sc.DevicesPerNode; d++ {
			devTotal := int64(model.SizeTB * 1e12)
			used := int64(float64(devTotal) * (0.2 + g.rng.Float64()*0.5))
			total += devTotal
			phase, devStatus := "Online", "online"
			if degradedLeft > 0 && g.pct(30) {
				phase, devStatus = "Degraded", "unavailable"
				degradedLeft--
			}
			if !health {
				phase, devStatus = "Unknown", "offline"
			}
			role := "Storage"
			if d == 0 && g.sc.DevicesPerNode > 1 {
				role = "Journal"
			}
			serial := fmt.Sprintf("%s%07d", model.SerialPfx, g.rng.UintN(9999999))
			devUUID := g.uuid()
			devName := fmt.Sprintf("%s-nvme%d", name, d)
			g.add(sbGroup, "storagedevices", g.ns, devName,
				map[string]any{"deviceID": devUUID, "nodeRef": name},
				map[string]any{
					"clusterID": g.clusterUUID, "nodeID": nodeUUID,
					"phase": phase, "deviceStatus": devStatus, "role": role,
					"capacity": map[string]any{"totalBytes": float64(devTotal), "usedBytes": float64(used)},
					"hardware": map[string]any{
						"model": model.Model, "serialNumber": serial,
						"pciAddress":     fmt.Sprintf("0000:%02x:00.0", 0x17+d),
						"nvmeController": fmt.Sprintf("nvme%d", d),
					},
				}, nil)
			node.devices = append(node.devices, genDevice{name: devName, uuid: devUUID, status: devStatus})
		}

		used := int64(float64(total) * (0.25 + g.rng.Float64()*0.4))
		nodeStatus := map[string]any{
			"uuid": nodeUUID, "hostname": worker, "status": status, "health": health,
			"failureDomain": float64(i % 3),
			"ports": map[string]any{
				"management": fmt.Sprintf("10.10.%d.%d:5000", i/250, 2+i%250),
				"nvmeof":     float64(4420), "rpc": float64(rpcPort), "lvol": float64(9110 + i),
			},
			"resources": map[string]any{
				"cpu": float64(8 + 4*g.rng.UintN(3)), "memory": gib(32 + 32*int(g.rng.UintN(3))),
				"devices": fmt.Sprint(g.sc.DevicesPerNode),
				"volumes": float64(g.rng.IntN(int(1 + g.sc.Volumes/max(1, g.sc.Workers)*2))),
				"capacity": map[string]any{
					"totalBytes": float64(total), "usedBytes": float64(used), "sampledAt": g.recent(5),
				},
			},
			"uptime":   fmt.Sprintf("%dd%dh", 1+g.rng.UintN(90), g.rng.UintN(24)),
			"postedAt": g.recent(2),
			"latencyMetrics": map[string]any{
				"nodeUUID": nodeUUID, "baselineMeasuredAt": g.past(48),
				"baselineP50NS": float64(80000 + g.rng.UintN(40000)),
				"baselineP99NS": float64(400000 + g.rng.UintN(300000)),
			},
		}
		g.add(sbGroup, "storagenodes", g.ns, name,
			map[string]any{
				"workerNode": worker, "nodeIndex": float64(i), "socketId": float64(0), "socketIndex": float64(0),
				"storageNodeSetRef": g.nodeSetName,
			}, nodeStatus, map[string]any{"labels": map[string]any{
				"storage.simplyblock.io/storagenodeset": g.nodeSetName,
				"storage.simplyblock.io/worker":         worker,
			}})
		g.nodes = append(g.nodes, node)

		// the per-slot label the CSI driver reads for topology
		if kn, err := g.st.Get(nodeDef, "", worker); err == nil {
			labels, _ := getPath(kn, "metadata.labels").(map[string]any)
			labels["simplyblock.io/storage-node-uuid."+g.clusterUUID+".0"] = nodeUUID
			g.st.Update(nodeDef, "", kn)
		}

		setNodes = append(setNodes, map[string]any{
			"uuid": nodeUUID, "hostname": worker, "status": status, "health": health,
			"failureDomain": float64(i % 3), "devices": fmt.Sprint(g.sc.DevicesPerNode),
			"nvmfPort": float64(4420), "rpcPort": float64(rpcPort), "lvolPort": float64(9110 + i),
			"mgmtIp": fmt.Sprintf("10.10.%d.%d", i/250, 2+i%250),
		})
	}

	g.add(sbGroup, "storagenodesets", g.ns, g.nodeSetName,
		map[string]any{
			"clusterName": g.clusterName, "workerNodes": toAny(g.workerNames),
			"mgmtIfname": "eth0", "dataIfname": []any{"eth1"},
			"maxParallelNodeAdds": float64(2),
		},
		map[string]any{
			"totalNodes":   float64(g.sc.Workers),
			"onlineNodes":  float64(g.sc.Workers - len(offline)),
			"offlineNodes": float64(len(offline)),
			"nodes":        setNodes,
		}, nil)
}

// storageNodePods creates, per storage node, the StorageNodeSet DaemonSet
// pod that runs the node API (in the cluster namespace) and the SPDK pod the
// backend creates (in the control plane namespace, host networked, pinned to
// the worker by hostname).
func (g *genCtx) storageNodePods() {
	for _, n := range g.nodes {
		ready := n.status != "offline"
		g.addPodFull(g.ns, fmt.Sprintf("%s-%s", g.nodeSetName, randSuffixLocal(g.rng)), n.worker, ready,
			map[string]any{
				"app":                           "storage-node",
				"simplyblock-cluster":           g.clusterName,
				"io.simplyblock.storagenodeset": g.nodeSetName,
				"io.simplyblock.node-type":      "simplyblock-storage-plane",
			},
			[]map[string]any{{"name": "s-node-api-container", "image": "quay.io/simplyblock-io/simplyblock:26.3.0"}},
			map[string]any{"hostNetwork": true})
		rpc := fmt.Sprint(n.rpcPort)
		g.addPodFull(g.sysNs, "snode-spdk-pod-"+rpc+"-"+g.clusterUUID, n.worker, ready,
			map[string]any{
				"role": "simplyblock-storage-node",
				"app":  "spdk-app-" + rpc,
			},
			[]map[string]any{
				{"name": "spdk-container", "image": "quay.io/simplyblock-io/spdk:26.3.0"},
				{"name": "spdk-proxy-container", "image": "quay.io/simplyblock-io/spdk-proxy:26.3.0",
					"env": []any{map[string]any{"name": "RPC_PORT", "value": rpc}}},
			},
			map[string]any{
				"hostNetwork":  true,
				"nodeSelector": map[string]any{"kubernetes.io/hostname": n.worker},
			})
	}
}

// addPodFull writes a pod with the given labels, containers, and extra spec,
// Running and Ready unless ready is false (then Running with the containers
// restarting, which is what a node whose SPDK is down looks like).
func (g *genCtx) addPodFull(ns, name, node string, ready bool, labels map[string]any, containers []map[string]any, extraSpec map[string]any) {
	spec := map[string]any{"nodeName": node, "containers": toAny(containers)}
	for k, v := range extraSpec {
		spec[k] = v
	}
	var statuses []any
	for _, c := range containers {
		restarts := float64(g.rng.UintN(3))
		if !ready {
			restarts = float64(12 + g.rng.UintN(40))
		}
		statuses = append(statuses, map[string]any{"name": c["name"], "ready": ready, "restartCount": restarts})
	}
	g.add("", "pods", ns, name, spec,
		map[string]any{
			"phase": "Running",
			"conditions": []any{map[string]any{
				"type": "Ready", "status": boolStr(ready),
			}},
			"containerStatuses": statuses,
			"podIP":             fmt.Sprintf("10.244.%d.%d", g.rng.UintN(64), 2+g.rng.UintN(250)),
		},
		map[string]any{
			"labels":      labels,
			"annotations": map[string]any{"log-collector/enabled": "true"},
		})
}

func (g *genCtx) poolsAndClasses() {
	for i := 0; i < g.sc.Pools; i++ {
		pool := g.name([]string{"vol-store", "db-tier", "bulk", "scratch"}[i%4])
		poolUUID := g.uuid()
		g.poolNames = append(g.poolNames, pool)
		g.poolUUIDs[pool] = poolUUID
		g.add(sbGroup, "storagepools", g.ns, pool,
			map[string]any{
				"clusterName":            g.clusterName,
				"capacityLimit":          gib(4096 * (i + 1)),
				"logicalVolumeMaxSize":   gib(2048),
				"qos":                    map[string]any{"iops": float64(200000)},
				"storageClassParameters": map[string]any{"filesystem": "xfs", "fabric": "tcp"},
			},
			map[string]any{"uuid": poolUUID, "status": "active"}, nil)
		g.add("storage.k8s.io", "storageclasses", "", g.storageClassName(pool), nil, nil, map[string]any{
			"labels": map[string]any{
				"storage.simplyblock.io/namespace": g.ns,
				"storage.simplyblock.io/cluster":   g.clusterName,
				"storage.simplyblock.io/pool":      pool,
			},
			"provisioner":          "csi.simplyblock.io",
			"reclaimPolicy":        "Delete",
			"allowVolumeExpansion": true,
			"volumeBindingMode":    "Immediate",
			"parameters": map[string]any{
				"cluster_id": g.clusterUUID, "pool_name": pool,
				"csi.storage.k8s.io/fstype": "xfs", "distr_ndcs": "2", "distr_npcs": "1",
			},
		})
	}
}

// storageClassName is the operator's per-pool StorageClass name.
func (g *genCtx) storageClassName(pool string) string {
	return "simplyblock-" + g.ns + "-" + g.clusterName + "-" + pool
}

// volumes creates PVC/PV pairs; the PV handle is the CSI driver's
// <clusterUUID>:<poolUUID>:<lvolUUID>.
func (g *genCtx) volumes() {
	for i := 0; i < g.sc.Volumes; i++ {
		appNs := g.sc.AppNamespaces[i%len(g.sc.AppNamespaces)]
		app := appWorkloads[int(g.rng.IntN(int(len(appWorkloads))))]
		pvcName := fmt.Sprintf("%s-%s-%d", g.pick(appVolumeNames), app, i)
		if g.clusterIdx > 0 {
			pvcName = fmt.Sprintf("c%d-%s", g.clusterIdx+1, pvcName)
		}
		pool := g.poolNames[i%len(g.poolNames)]
		sc := g.storageClassName(pool)
		size := []int{20, 50, 100, 250, 500}[g.rng.UintN(5)]
		volID := g.uuid()
		pvName := "pvc-" + volID

		annotations := map[string]any{}
		if g.sc.WithDR && g.pct(60) {
			annotations["storage.simplyblock.io/replication-policy"] = "policy-prod"
		}
		if g.pct(30) {
			annotations["storage.simplyblock.io/backup-policy"] = g.name("nightly")
		}
		g.add("", "persistentvolumeclaims", appNs, pvcName,
			map[string]any{
				"accessModes":      []any{"ReadWriteOnce"},
				"resources":        map[string]any{"requests": map[string]any{"storage": gib(size)}},
				"storageClassName": sc,
				"volumeName":       pvName,
				"volumeMode":       "Filesystem",
			},
			map[string]any{
				"phase": "Bound", "accessModes": []any{"ReadWriteOnce"},
				"capacity": map[string]any{"storage": gib(size)},
			},
			map[string]any{
				"labels":      map[string]any{"app": app},
				"annotations": annotations,
			})
		g.add("", "persistentvolumes", "", pvName,
			map[string]any{
				"capacity":    map[string]any{"storage": gib(size)},
				"accessModes": []any{"ReadWriteOnce"},
				"csi": map[string]any{
					"driver":       "csi.simplyblock.io",
					"volumeHandle": g.clusterUUID + ":" + g.poolUUIDs[pool] + ":" + volID,
					"fsType":       "xfs",
					"volumeAttributes": map[string]any{
						"cluster_id": g.clusterUUID, "pool_name": pool,
						"storage.kubernetes.io/csiProvisionerIdentity": "csi.simplyblock.io",
					},
				},
				"claimRef": map[string]any{
					"kind": "PersistentVolumeClaim", "namespace": appNs, "name": pvcName,
				},
				"storageClassName":              sc,
				"persistentVolumeReclaimPolicy": "Delete",
			},
			map[string]any{"phase": "Bound"}, nil)
		g.pvcRefs = append(g.pvcRefs, pvcRef{appNs, pvcName, volID, g.poolUUIDs[pool], sc, size})
	}
}

// snapshots creates VolumeSnapshot/VolumeSnapshotContent pairs; the content's
// handle is <clusterUUID>:<poolUUID>:<snapshotUUID>.
func (g *genCtx) snapshots() {
	if len(g.pvcRefs) == 0 {
		return
	}
	classDef := g.def("snapshot.storage.k8s.io", "volumesnapshotclasses")
	if _, err := g.st.Get(classDef, "", "simplyblock-snapshots"); err != nil {
		g.add("snapshot.storage.k8s.io", "volumesnapshotclasses", "", "simplyblock-snapshots", nil, nil, map[string]any{
			"driver":         "csi.simplyblock.io",
			"deletionPolicy": "Delete",
		})
	}
	for i := 0; i < g.sc.Snapshots; i++ {
		p := g.pvcRefs[g.rng.IntN(int(len(g.pvcRefs)))]
		name := fmt.Sprintf("snap-%s-%d", p.Name, i)
		snapUUID := g.uuid()
		contentName := "snapcontent-" + snapUUID
		g.add("snapshot.storage.k8s.io", "volumesnapshots", p.Namespace, name,
			map[string]any{
				"volumeSnapshotClassName": "simplyblock-snapshots",
				"source":                  map[string]any{"persistentVolumeClaimName": p.Name},
			},
			map[string]any{
				"readyToUse": true, "creationTime": g.past(72),
				"boundVolumeSnapshotContentName": contentName,
				"restoreSize":                    gib(p.SizeGi),
			}, nil)
		g.add("snapshot.storage.k8s.io", "volumesnapshotcontents", "", contentName,
			map[string]any{
				"driver":         "csi.simplyblock.io",
				"deletionPolicy": "Delete",
				"source":         map[string]any{"volumeHandle": g.clusterUUID + ":" + p.PoolUUID + ":" + p.VolumeID},
				"volumeSnapshotRef": map[string]any{
					"kind": "VolumeSnapshot", "name": name, "namespace": p.Namespace,
				},
			},
			map[string]any{"readyToUse": true, "snapshotHandle": g.clusterUUID + ":" + p.PoolUUID + ":" + snapUUID}, nil)
	}
}

func (g *genCtx) backups() {
	if g.sc.Backups == 0 {
		return
	}
	var attached []any
	for i := 0; i < min(3, len(g.pvcRefs)); i++ {
		p := g.pvcRefs[i]
		attached = append(attached, map[string]any{
			"lvolID": p.VolumeID, "pvcName": p.Name, "pvcNamespace": p.Namespace,
		})
	}
	g.add(sbGroup, "backuppolicies", g.ns, g.name("nightly"),
		map[string]any{
			"clusterName": g.clusterName, "schedule": "0 2 * * *",
			"maxVersions": float64(7), "maxAge": "168h",
		},
		map[string]any{
			"phase": "Active", "policyID": g.uuid(), "clusterUUID": g.clusterUUID,
			"attachedLvols": attached,
		}, nil)

	for i := 0; i < g.sc.Backups; i++ {
		p := g.pvcRefs[g.rng.IntN(int(len(g.pvcRefs)))]
		phase := "Done"
		if i == 0 && g.sc.RunningOps {
			phase = "Running"
		}
		g.add(sbGroup, "storagebackups", g.ns, fmt.Sprintf("backup-%s-%d", p.Name, i),
			map[string]any{
				"clusterName": g.clusterName,
				"pvcRef":      map[string]any{"name": p.Name, "namespace": p.Namespace},
			},
			map[string]any{
				"phase": phase, "backupID": g.uuid(), "clusterUUID": g.clusterUUID,
				"lvolID": p.VolumeID, "lvolName": p.Name,
				"size":      float64(p.SizeGi) * float64(1<<30) * 0.6,
				"createdAt": g.past(96), "completedAt": g.past(90),
				"poolName": g.poolNames[0], "pvcNamespace": p.Namespace,
			}, nil)
	}
}

// tasks seeds the control plane's task list: master tasks (node_restart,
// device_restart, node_add) with their sub-tasks, each sub-task naming its
// master in function_params.parent_task_id, plus the Task CR that mirrors
// the list the operator polls.
func (g *genCtx) tasks() {
	if len(g.nodes) == 0 {
		return
	}
	var entries []any
	mk := func(fn, status, result string, node *genNode, dev *genDevice, parent string, retry int) map[string]any {
		var nodeID, devID any
		params := map[string]any{}
		if node != nil {
			nodeID = node.uuid
			params["node_id"] = node.uuid
		}
		if dev != nil {
			devID = dev.uuid
			params["device_id"] = dev.uuid
		}
		if parent != "" {
			params["parent_task_id"] = parent
		}
		t := map[string]any{
			"id": g.uuid(), "cluster_id": g.clusterUUID,
			"device_id": devID, "storage_node_id": nodeID,
			"status": status, "canceled": false,
			"function_name": fn, "function_params": params, "function_result": result,
			"max_retry": float64(3), "retry": float64(retry),
		}
		g.st.AddTask(g.clusterUUID, t)
		entries = append(entries, map[string]any{
			"uuid": t["id"], "taskType": fn, "taskStatus": status, "taskResult": result,
			"startedAt": g.past(24), "retried": float64(retry), "canceled": false,
		})
		return t
	}
	// a master with the given sub-task family over a node's devices
	master := func(fn, subFn string, node *genNode, status string, failed bool) {
		result := ""
		if status == "done" {
			result = "success"
		}
		m := mk(fn, status, result, node, nil, "", 0)
		for i := range node.devices {
			subStatus, subResult, retry := "done", "success", 0
			switch {
			case status == "new":
				subStatus, subResult = "new", ""
			case status == "running" && i == 0:
				subStatus, subResult = "running", ""
			case status == "running" && i > 0:
				subStatus, subResult = "new", ""
			case status == "done" && failed && i == len(node.devices)-1:
				subResult, retry = "failed: device unreachable", 3
			}
			mk(subFn, subStatus, subResult, node, &node.devices[i], getStr(m, "id"), retry)
		}
	}
	nodeAt := func(i int) *genNode { return &g.nodes[i%len(g.nodes)] }

	// history: every node was added, one restarted
	for i := range g.nodes {
		master("node_add", "new_device_migration", nodeAt(i), "done", false)
	}
	master("node_restart", "device_restart", nodeAt(1), "done", g.sc.FailedOps > 0)
	master("device_restart", "failed_device_migration", nodeAt(2), "done", false)
	if g.sc.RunningOps {
		master("node_restart", "device_restart", nodeAt(1), "running", false)
		master("device_restart", "failed_device_migration", nodeAt(0), "new", false)
	}
	g.add(sbGroup, "tasks", g.ns, g.name("cluster-tasks"),
		map[string]any{"clusterName": g.clusterName, "subtasks": true},
		map[string]any{"tasks": entries}, nil)
}

// opsHistory seeds completed and failed Ops plus — when the scenario says so —
// in-flight ones the simulator will carry forward.
func (g *genCtx) opsHistory() {
	mkStatus := func(phase string, hoursAgo int) map[string]any {
		st := map[string]any{"phase": phase, "startedAt": g.past(hoursAgo)}
		switch phase {
		case "Succeeded":
			st["completedAt"] = g.past(hoursAgo - 1)
			st["message"] = "completed"
		case "Failed":
			st["completedAt"] = g.past(hoursAgo - 1)
			st["message"] = "timed out waiting for node to come back online"
		case "Running":
			st["message"] = "in progress"
		}
		return st
	}

	g.add(sbGroup, "storageclusterops", g.ns, g.name("activate-initial"),
		map[string]any{"action": "activate", "clusterRef": g.clusterName},
		mkStatus("Succeeded", 200), nil)

	failed := g.sc.FailedOps
	for i := 0; i < failed; i++ {
		node := g.nodeName(1 + g.rng.IntN(int(g.sc.Workers)))
		g.add(sbGroup, "storagenodeops", g.ns, fmt.Sprintf("restart-%s-failed-%d", node, i),
			map[string]any{"action": "restart", "storageNodeRef": node},
			mkStatus("Failed", 24+i*10), nil)
	}

	if g.sc.RunningOps && g.sc.Workers >= 2 {
		st := mkStatus("Running", 1)
		st["subPhase"] = "Restarting"
		g.add(sbGroup, "storagenodeops", g.ns, g.name("restart-sn-02"),
			map[string]any{"action": "restart", "storageNodeRef": g.nodeName(2)}, st, nil)

		var pending []string
		for i := 3; i <= g.sc.Workers; i++ {
			pending = append(pending, g.nodeName(i))
		}
		g.add(sbGroup, "storageclusterops", g.ns, g.name("rolling-restart"),
			map[string]any{"action": "node-rolling-restart", "clusterRef": g.clusterName},
			map[string]any{
				"phase": "Running", "startedAt": g.recent(30),
				"nodeRollingRestartStatus": map[string]any{
					"nodePhase":      "Running",
					"processedNodes": []any{g.nodeName(1)},
					"pendingNodes":   toAny(pending),
				},
			}, nil)

		if len(g.pvcRefs) > 0 && len(g.nodeUUIDs) >= 2 {
			p := g.pvcRefs[0]
			g.add(sbGroup, "volumemigrations", g.ns, "migrate-"+p.Name,
				map[string]any{"pvName": "pvc-" + p.VolumeID, "targetNodeUUID": g.nodeUUIDs[1]},
				map[string]any{
					"phase": "Running", "startedAt": g.recent(10),
					"clusterUUID": g.clusterUUID, "volumeUUID": p.VolumeID,
					"sourceNodeUUID": g.nodeUUIDs[0], "migrationUUID": g.uuid(),
				}, map[string]any{"labels": map[string]any{"storage.simplyblock.io/drain-node": g.nodeUUIDs[0]}})
		}
	}

	if g.clusterIdx == 0 {
		g.add(sbGroup, "operatorops", g.ns, "discover-initial",
			map[string]any{"action": "Discover", "discover": map[string]any{"configName": "sb-primary-config"}},
			map[string]any{
				"phase": "Succeeded", "startedAt": g.past(240), "completedAt": g.past(239),
				"configRef": "sb-primary-config", "environment": "Vanilla",
				"workers": toAny(g.workerNames),
			}, nil)
	}
}

func nodeNames(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("sn-%02d", i))
	}
	return out
}

// seedEventLog writes the cluster's history into the control plane event
// log: activation, every node and device coming online, the outages the
// scenario seeds, capacity notices, and journal manager events, oldest
// first so the newest-first read order matches the control plane's.
func (g *genCtx) seedEventLog() {
	base := g.now.Add(-time.Duration(20+g.rng.IntN(60)) * time.Hour)
	at := base
	step := func(maxMinutes int) time.Time {
		at = at.Add(time.Duration(1+g.rng.IntN(int(maxMinutes))) * time.Minute)
		return at
	}
	add := func(t time.Time, nodeID, event, level, msg, storageID, status string) {
		g.st.AppendLog(g.clusterUUID, LogRecord{
			Date: logDate(t), NodeID: nodeID, Event: event, Level: level,
			Message: msg, StorageID: storageID, VUID: "", Status: status,
		})
	}
	add(step(5), "", "STATUS_CHANGE", "Info", "Cluster status changed from: unready to: in_activation", g.clusterUUID, "in_activation")
	for _, n := range g.nodes {
		add(step(3), n.uuid, "STATUS_CHANGE", "Info", "Storage node status changed from: in_creation to: online", n.uuid, "online")
		add(step(1), n.uuid, "node_status", "Info", "Storage node is online", n.uuid, "online")
		for _, d := range n.devices {
			add(step(1), n.uuid, "STATUS_CHANGE", "Info", "Device status changed from: unavailable to: online", d.uuid, "online")
			add(step(1), n.uuid, "device_status", "Info", "Device is online", d.uuid, "online")
		}
		add(step(1), n.uuid, "JM_DEVICE_STATUS", "Info", "Journal manager device created on node "+n.uuid, n.uuid, "online")
	}
	add(step(5), "", "STATUS_CHANGE", "Info", "Cluster status changed from: in_activation to: active", g.clusterUUID, "active")

	// the outages the scenario seeds, as the transitions that led to them
	for _, n := range g.nodes {
		if n.status == "offline" {
			add(step(60), n.uuid, "STATUS_CHANGE", "Error", "Storage node status changed from: online to: offline", n.uuid, "offline")
			add(step(1), n.uuid, "node_status", "Error", "Storage node is offline", n.uuid, "offline")
			for _, d := range n.devices {
				add(step(1), n.uuid, "STATUS_CHANGE", "Warning", "Device status changed from: online to: "+d.status, d.uuid, d.status)
			}
			add(step(1), n.uuid, "JM_RESTART", "Warning", "Journal manager restart requested on node "+n.uuid, n.uuid, "")
			continue
		}
		for _, d := range n.devices {
			if d.status != "online" {
				add(step(30), n.uuid, "STATUS_CHANGE", "Warning", "Device status changed from: online to: "+d.status, d.uuid, d.status)
				add(step(1), n.uuid, "device_status", "Warning", "Device is "+d.status, d.uuid, d.status)
			}
		}
	}
	if g.sc.OfflineNodes > 0 || g.sc.DegradedDevices > 0 {
		add(step(5), "", "STATUS_CHANGE", "Warning", "Cluster status changed from: active to: degraded", g.clusterUUID, "degraded")
		add(step(120), "", "STATUS_CHANGE", "Info", "Cluster status changed from: degraded to: active", g.clusterUUID, "active")
	}
	// capacity notices
	util := 40 + g.rng.IntN(45)
	level := "Info"
	if util >= 75 {
		level = "Warning"
	}
	add(step(90), "", "CAPACITY", level, fmt.Sprintf("Cluster capacity utilization reached: %d%%", util), g.clusterUUID, "")
	if g.sc.EventStorm {
		for i := 0; i < 150; i++ {
			n := g.nodes[g.rng.IntN(int(len(g.nodes)))]
			d := n.devices[g.rng.IntN(int(len(n.devices)))]
			add(step(2), n.uuid, "device_status", "Warning", "Device reported media errors increasing", d.uuid, d.status)
		}
	}
}
func (g *genCtx) replicationChain() {
	cond := func(typ, reason, msg string) []any {
		return []any{map[string]any{
			"type": typ, "status": "True", "reason": reason, "message": msg,
			"lastTransitionTime": g.past(48), "observedGeneration": float64(1),
		}}
	}
	g.add(sbGroup, "replicationpairs", g.ns, "pair-east-west",
		map[string]any{"sourceCluster": g.clusterName, "targetCluster": g.drCluster},
		map[string]any{
			"ready": true, "backendTargetID": g.uuid(),
			"conditions": cond("Ready", "Connected", "target cluster reachable"),
		}, nil)
	g.add(sbGroup, "replicationpolicies", g.ns, "policy-prod",
		map[string]any{"pairRef": "pair-east-west", "interval": "5m", "mode": "failover", "snapshotRetention": float64(3)},
		map[string]any{
			"ready": true, "backendPolicyID": g.uuid(), "slotCount": float64(min(len(g.pvcRefs), 6)),
			"conditions": cond("Ready", "Reconciled", "policy active"),
		}, nil)

	states := []string{"replicating", "replicating", "replicating", "attaching"}
	if g.sc.DRFailoverIn {
		states = []string{"cutover_pending", "cutover_pending", "replicating", "failed_over"}
	}
	for i := 0; i < min(len(g.pvcRefs), 6); i++ {
		p := g.pvcRefs[i]
		state := states[i%len(states)]
		g.add(sbGroup, "replicationslots", g.ns, fmt.Sprintf("slot-%s", p.Name),
			map[string]any{
				"policyRef": "policy-prod",
				"pvcRef":    p.Namespace + "/" + p.Name,
				"volumeID":  p.VolumeID,
			},
			map[string]any{
				"state": state, "direction": "source",
				"sourceLvolID": p.VolumeID, "targetLvolID": g.uuid(),
				"targetNQN":        "nqn.2023-02.io.simplyblock:" + g.uuid(),
				"lastReplicatedAt": g.recent(6),
				"conditions":       cond("Replicating", "InSync", "slot within interval"),
			}, nil)
	}

	if g.sc.DRFailoverIn {
		g.add(sbGroup, "replicationops", g.ns, "failover-prod",
			map[string]any{"action": "failover", "scope": "policy", "ref": "policy-prod"},
			map[string]any{
				"phase": "Running", "subphase": "cutover", "startedAt": g.recent(5),
				"results": []any{},
			}, nil)
	}
}

func (g *genCtx) ramen() {
	for _, mc := range []string{g.clusterName, g.drCluster} {
		region := map[string]string{g.clusterName: "east", g.drCluster: "west"}[mc]
		g.add(ramenGroup, "drclusters", "", mc,
			map[string]any{"s3ProfileName": "s3-" + region, "region": region},
			map[string]any{
				"phase": "Available",
				"conditions": []any{map[string]any{
					"type": "Fenced", "status": "False", "reason": "Clean",
					"lastTransitionTime": g.past(100), "message": "cluster is clean",
				}},
			}, nil)
		g.add(ocmGroup, "managedclusters", "", mc,
			map[string]any{"hubAcceptsClient": true},
			map[string]any{
				"conditions": []any{map[string]any{
					"type": "ManagedClusterConditionAvailable", "status": "True",
					"reason": "ManagedClusterAvailable", "lastTransitionTime": g.past(100),
				}},
				"version": map[string]any{"kubernetes": "v1.31.4"},
			}, nil)
	}
	g.add(ramenGroup, "drclusterconfigs", "", "dr-cluster-config",
		map[string]any{"replicationSchedules": []any{"5m"}}, nil, nil)
	g.add(ramenGroup, "drpolicies", "", "policy-5m",
		map[string]any{
			"drClusters":         []any{g.clusterName, g.drCluster},
			"schedulingInterval": "5m",
		},
		map[string]any{
			"conditions": []any{map[string]any{
				"type": "Validated", "status": "True", "reason": "Succeeded",
				"lastTransitionTime": g.past(100), "message": "drpolicy validated",
			}},
		}, nil)

	for i, appNs := range g.sc.AppNamespaces {
		phase, action := "Deployed", ""
		if g.sc.DRFailoverIn && i == 0 {
			phase, action = "FailingOver", "Failover"
		}
		spec := map[string]any{
			"drPolicyRef":      map[string]any{"name": "policy-5m"},
			"placementRef":     map[string]any{"kind": "Placement", "name": appNs + "-placement"},
			"pvcSelector":      map[string]any{"matchLabels": map[string]any{}},
			"preferredCluster": g.clusterName,
			"failoverCluster":  g.drCluster,
		}
		if action != "" {
			spec["action"] = action
		}
		g.add(ramenGroup, "drplacementcontrols", appNs, appNs+"-drpc", spec,
			map[string]any{
				"phase": phase, "lastUpdateTime": g.recent(5),
				"preferredDecision": map[string]any{"clusterName": g.clusterName, "clusterNamespace": appNs},
				"conditions": []any{map[string]any{
					"type": "Available", "status": "True", "reason": phase,
					"lastTransitionTime": g.recent(30), "observedGeneration": float64(1),
				}},
			}, nil)

		var protected []any
		for _, p := range g.pvcRefs {
			if p.Namespace == appNs {
				protected = append(protected, map[string]any{
					"name": p.Name, "namespace": p.Namespace,
					"protectedByVolSync": false,
					"replicationID":      map[string]any{"id": p.VolumeID},
				})
			}
		}
		g.add(ramenGroup, "volumereplicationgroups", appNs, appNs+"-vrg",
			map[string]any{
				"replicationState": "primary",
				"pvcSelector":      map[string]any{"matchLabels": map[string]any{}},
				"s3Profiles":       []any{"s3-east", "s3-west"},
				"async":            map[string]any{"schedulingInterval": "5m"},
			},
			map[string]any{
				"state": "Primary", "protectedPVCs": protected,
				"lastUpdateTime": g.recent(5),
			}, nil)

		g.add(ramenGroup, "recipes", appNs, appNs+"-recipe",
			map[string]any{
				"appType": appNs,
				"groups": []any{map[string]any{
					"name": "workloads", "type": "resource",
					"includedResourceTypes": []any{"deployment", "statefulset", "configmap", "service"},
				}},
				"hooks": []any{map[string]any{
					"name": "quiesce", "type": "exec", "namespace": appNs,
					"ops": []any{map[string]any{"name": "flush", "command": "sync"}},
				}},
				"workflows": []any{
					map[string]any{"name": "backup", "sequence": []any{
						map[string]any{"hook": "quiesce/flush"}, map[string]any{"group": "workloads"},
					}},
					map[string]any{"name": "restore", "sequence": []any{
						map[string]any{"group": "workloads"},
					}},
				},
			}, nil, nil)
	}
}

// workloads populates each app namespace with what the recipe editor
// discovers: deployments, statefulsets, replicasets, pods, services,
// configmaps, ingresses.
func (g *genCtx) workloads() {
	for _, appNs := range g.sc.AppNamespaces {
		app := appWorkloads[int(g.rng.IntN(int(len(appWorkloads))))]
		labels := map[string]any{"app": app}
		g.add("apps", "deployments", appNs, app+"-frontend",
			map[string]any{
				"replicas": float64(2),
				"selector": map[string]any{"matchLabels": labels},
			},
			map[string]any{"replicas": float64(2), "readyReplicas": float64(2), "availableReplicas": float64(2)},
			map[string]any{"labels": labels})
		rsName := app + "-frontend-" + randSuffixLocal(g.rng)
		g.add("apps", "replicasets", appNs, rsName,
			map[string]any{"replicas": float64(2), "selector": map[string]any{"matchLabels": labels}},
			map[string]any{"replicas": float64(2), "readyReplicas": float64(2)},
			map[string]any{"labels": labels})
		g.add("apps", "statefulsets", appNs, app,
			map[string]any{
				"replicas":    float64(3),
				"serviceName": app + "-headless",
				"selector":    map[string]any{"matchLabels": labels},
			},
			map[string]any{"replicas": float64(3), "readyReplicas": float64(3)},
			map[string]any{"labels": labels})
		for i := 0; i < 3; i++ {
			g.addPod(appNs, fmt.Sprintf("%s-%d", app, i), app, g.allWorkers[int(g.rng.IntN(int(len(g.allWorkers))))], "Running")
		}
		g.add("", "services", appNs, app,
			map[string]any{
				"type": "ClusterIP", "selector": labels,
				"ports": []any{map[string]any{"port": float64(5432), "targetPort": float64(5432)}},
			}, nil, map[string]any{"labels": labels})
		g.add("", "configmaps", appNs, app+"-config", nil, nil, map[string]any{
			"data": map[string]any{"MODE": "production", "REPLICAS": "3"},
		})
		g.add("networking.k8s.io", "ingresses", appNs, app,
			map[string]any{"rules": []any{map[string]any{"host": app + ".example.com"}}}, nil,
			map[string]any{"labels": labels})
	}
}

func (g *genCtx) addPod(ns, name, app, node, phase string) {
	g.add("", "pods", ns, name,
		map[string]any{
			"nodeName": node,
			"containers": []any{map[string]any{
				"name": app, "image": "quay.io/simplyblock-io/" + app + ":latest",
			}},
		},
		map[string]any{
			"phase": phase,
			"conditions": []any{map[string]any{
				"type": "Ready", "status": map[bool]string{true: "True", false: "False"}[phase == "Running"],
			}},
			"containerStatuses": []any{map[string]any{
				"name": app, "ready": phase == "Running",
				"restartCount": float64(g.rng.UintN(3)),
			}},
			"podIP": fmt.Sprintf("10.244.%d.%d", g.rng.UintN(64), 2+g.rng.UintN(250)),
		},
		map[string]any{"labels": map[string]any{"app": app}})
}

// controlPlaneWorkloads is the control plane as the chart deploys it: one
// Deployment per component, selected by app=<name>, with the containers the
// log views pick from.
var controlPlaneWorkloads = []struct {
	name       string
	containers []string
}{
	{"simplyblock-admin-control", []string{"simplyblock-control"}},
	{"simplyblock-webappapi", []string{"webappapi"}},
	{"simplyblock-monitoring", []string{
		"cluster-monitor", "storage-node-monitor", "device-monitor", "lvol-monitor", "health-check",
		"port-stat-collector", "capacity-collector", "lvol-stat-collector", "cap-stat-collector", "main-distr-event-collector",
	}},
	{"simplyblock-tasks", []string{
		"tasks-runner-device-restart", "tasks-runner-node-restart", "tasks-runner-failed-migration",
		"tasks-runner-new-dev-migration", "tasks-runner-port-restart", "tasks-runner-node-add",
	}},
	{"simplyblock-fdb-controller-manager", []string{"manager"}},
	{"simplyblock-operator", []string{"manager"}},
}

// controlPlanePods creates the control plane Deployments and their pods, the
// FoundationDB pods, and the console's own helpers, all on the management
// nodes and all marked for log collection.
func (g *genCtx) controlPlanePods() {
	mgmt := fmt.Sprintf("mgmt-%s-1", g.site)
	for _, wl := range controlPlaneWorkloads {
		labels := map[string]any{"app": wl.name}
		g.add("apps", "deployments", g.sysNs, wl.name,
			map[string]any{
				"replicas": float64(1),
				"selector": map[string]any{"matchLabels": labels},
			},
			map[string]any{"replicas": float64(1), "readyReplicas": float64(1), "availableReplicas": float64(1)},
			map[string]any{"labels": labels})
		var containers []map[string]any
		for _, c := range wl.containers {
			containers = append(containers, map[string]any{"name": c, "image": "quay.io/simplyblock-io/simplyblock:26.3.0"})
		}
		g.addPodFull(g.sysNs, wl.name+"-"+randSuffixLocal(g.rng)+"-"+randSuffixLocal(g.rng), mgmt, true, labels, containers, nil)
	}
	for i := 1; i <= 3; i++ {
		g.addPodFull(g.sysNs, fmt.Sprintf("simplyblock-fdb-storage-%d", i), mgmt, true,
			map[string]any{
				"foundationdb.org/fdb-cluster-name":  "simplyblock-fdb",
				"foundationdb.org/fdb-process-class": "storage",
			},
			[]map[string]any{{"name": "foundationdb", "image": "foundationdb/foundationdb:7.1.57"}}, nil)
	}
	for _, cp := range []string{"simplyblock-prometheus", "simplyblock-control-center", "sb-agent"} {
		g.addPod(g.sysNs, cp+"-"+randSuffixLocal(g.rng), cp, mgmt, "Running")
	}
}

// secrets writes a structurally real Helm release Secret — name, type and
// data.release encoding (base64 within the JSON encoding, gzip inside) match
// what Helm stores, so the console's release view parses it like the real
// thing.
func (g *genCtx) secrets() {
	release := map[string]any{
		"name":      "simplyblock-operator",
		"namespace": g.sysNs,
		"version":   1,
		"info": map[string]any{
			"status":         "deployed",
			"first_deployed": g.past(240),
			"last_deployed":  g.past(24),
			"description":    "Install complete",
		},
		"chart": map[string]any{
			"metadata": map[string]any{
				"name": "simplyblock-operator", "version": "26.2.7",
				"appVersion": "latest", "apiVersion": "v2",
			},
		},
		"config": map[string]any{"controlCenter": map[string]any{"enabled": true}},
	}
	raw, _ := json.Marshal(release)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw)
	zw.Close()
	// Helm double-encodes: the Secret's data value is base64(gzip(json)), and
	// the API then serves data values base64-encoded again.
	inner := base64.StdEncoding.EncodeToString(gz.Bytes())
	outer := base64.StdEncoding.EncodeToString([]byte(inner))

	g.add("", "secrets", g.sysNs, "sh.helm.release.v1.simplyblock-operator.v1", nil, nil, map[string]any{
		"type": "helm.sh/release.v1",
		"data": map[string]any{"release": outer},
		"labels": map[string]any{
			"owner": "helm", "name": "simplyblock-operator", "status": "deployed", "version": "1",
		},
	})
	g.add("", "secrets", g.sysNs, "backup-s3-credentials", nil, nil, map[string]any{
		"type": "Opaque",
		"data": map[string]any{
			"access_key": base64.StdEncoding.EncodeToString([]byte("MOCKACCESSKEY")),
			"secret_key": base64.StdEncoding.EncodeToString([]byte("mock-secret-not-real")),
		},
	})
}

func (g *genCtx) controlPlaneObjects() {
	g.add(sbGroup, "controlplanes", g.sysNs, "simplyblock",
		map[string]any{"image": "quay.io/simplyblock-io/simplyblock:26.3.0"},
		map[string]any{"phase": "Ready", "lastChecked": g.recent(2), "message": "control plane healthy"}, nil)

	var groups []any
	groups = append(groups, map[string]any{
		"name": "default", "workers": toAny(g.workerNames),
		"mgmtInterface": "eth0", "dataInterfaces": []any{"eth1"},
	})
	// The deployment config targets a managed cluster, so it lives in the
	// transport namespace (Kubernetes > cluster > Discovery & deployment).
	g.add(sbGroup, "clusterdeploymentconfigs", g.mcNs, "sb-primary-config",
		map[string]any{
			"approved":    true,
			"environment": "Vanilla",
			"cluster": map[string]any{
				"name": g.clusterName, "fabricType": "tcp",
				"stripe": map[string]any{"dataChunks": 2, "parityChunks": 1},
			},
			"nodeSets": []any{map[string]any{"name": "sb-primary-nodes", "groups": groups}},
		},
		map[string]any{
			"phase": "Expanded", "clusterRef": g.clusterName,
			"nodeRefs": toAny(nodeNames(1, g.sc.Workers)),
		}, nil)
}

func (g *genCtx) events() {
	n := 25
	if g.sc.EventStorm {
		n = 220
	}
	for i := 0; i < n; i++ {
		ev := eventReasons[g.rng.IntN(int(len(eventReasons)))]
		if !g.sc.EventStorm && ev.Type == "Warning" && g.pct(50) {
			ev = eventReasons[0]
		}
		node := g.nodeName(1 + int(g.rng.IntN(int(g.sc.Workers))))
		ts := g.recent(600)
		g.add("", "events", g.ns, fmt.Sprintf("%s.%s", node, randSuffixLocal(g.rng)),
			nil, nil, map[string]any{
				"type": ev.Type, "reason": ev.Reason,
				"message": ev.Message,
				"involvedObject": map[string]any{
					"kind": "StorageNode", "name": node,
					"namespace": g.ns, "apiVersion": sbGroup + "/v1alpha1",
				},
				"source":         map[string]any{"component": "simplyblock-operator"},
				"firstTimestamp": ts, "lastTimestamp": ts,
				"count": float64(1 + g.rng.UintN(5)),
			})
	}
}

// tenancyScaffold creates the cluster-scoped hub objects: the ManagedCluster
// registration (with agent connectivity), the NodePoolAllocation privileged-op
// envelope (§2.4), and a StorageClusterClass.
func (g *genCtx) tenancyScaffold() {
	agentConnected := true
	drift := g.driftPresent()
	lastSync := g.recent(2)
	if !agentConnected {
		lastSync = g.past(3)
	}
	g.add(sbGroup, "managedclusters", "", g.mcName,
		map[string]any{
			"kubernetesEndpoint": fmt.Sprintf("https://%s.k8s.example.com:6443", g.mcName),
			"region":             g.site,
			"transportNamespace": g.mcNs,
		},
		map[string]any{
			"phase":             "Available",
			"agentConnected":    agentConnected,
			"lastSyncTime":      lastSync,
			"kubernetesVersion": "v1.31.4",
			"storageClusters":   []any{g.clusterName},
			// hub-only conditions the managed cluster cannot know about itself
			"conditions": []any{
				map[string]any{"type": "AgentConnected", "status": boolStr(agentConnected), "reason": "Heartbeat", "message": "agent client cert valid; pulling intent", "lastTransitionTime": g.past(48), "observedGeneration": float64(1)},
				map[string]any{"type": "InSync", "status": boolStr(!drift), "reason": ternStr(drift, "DriftDetected", "Synced"), "message": ternStr(drift, "projected status differs from managed cluster", "hub and managed cluster agree"), "lastTransitionTime": lastSync, "observedGeneration": float64(1)},
			},
		}, nil)

	g.add(sbGroup, "nodepoolallocations", "", g.clusterName+"-alloc",
		map[string]any{
			"managedCluster":          g.mcName,
			"boundNamespace":          g.ns,
			"allowedNodeSelector":     map[string]any{"io.simplyblock.node-type": "simplyblock-storage-plane"},
			"allowedDevicePatterns":   []any{"/dev/disk/by-id/nvme-"},
			"maxIsolatedCoresPerNode": float64(8),
			"maxHugepagesGiPerNode":   float64(32),
		},
		map[string]any{"phase": "Bound", "boundNamespace": g.ns}, nil)

	g.add(sbGroup, "storageclusterclasses", "", "standard-nvme",
		map[string]any{
			"stripe":     map[string]any{"dataChunks": 2, "parityChunks": 1},
			"fabricType": "tcp",
		},
		map[string]any{"phase": "Ready"}, nil)
}

// projectStorageCluster writes the transport-namespace projection of the
// StorageCluster (§1.3, §2.1): the same kind, placed in sb-mc-* by the hub
// controller, whose STATUS is authored by the agent (connectivity, last sync,
// drift, observedGeneration) — a projection, not a byte copy.
func (g *genCtx) projectStorageCluster() {
	drift := g.driftPresent()
	appliedGen, observedGen := 1, 1
	if drift {
		appliedGen = 2 // hub advanced spec; agent has not applied it yet (pending)
	}
	obj := map[string]any{
		"metadata": map[string]any{
			"name":       g.clusterName,
			"uid":        g.uuid(),
			"generation": float64(appliedGen),
			"labels": map[string]any{
				labManagedBy:      "hub",
				labManagedCluster: g.mcName,
				labStorageCluster: g.clusterName,
				labTenant:         g.tenant,
			},
			"annotations": map[string]any{
				"simplyblock.io/projection-of": g.ns + "/" + g.clusterName,
			},
		},
		// hub-owned spec projection (allocation ref, DR pairing carried here)
		"spec": map[string]any{
			"stripe":                map[string]any{"dataChunks": 2, "parityChunks": 1},
			"fabricType":            "tcp",
			"nodePoolAllocationRef": g.clusterName + "-alloc",
		},
		// agent-authored status
		"status": map[string]any{
			"uuid":               g.clusterUUID,
			"phase":              "Active",
			"observedGeneration": float64(observedGen),
			"lastSyncTime":       g.recent(2),
			"agentConnected":     true,
			"conditions": []any{
				map[string]any{"type": "AgentConnected", "status": "True", "reason": "Heartbeat", "message": "agent pulling intent from " + g.mcNs, "lastTransitionTime": g.past(48), "observedGeneration": float64(observedGen)},
				map[string]any{"type": "Applied", "status": boolStr(!drift), "reason": ternStr(drift, "Pending", "Applied"), "message": ternStr(drift, fmt.Sprintf("spec generation %d not yet applied (observed %d)", appliedGen, observedGen), "spec applied on the managed cluster"), "lastTransitionTime": g.recent(20), "observedGeneration": float64(observedGen)},
			},
		},
	}
	stampActor(obj["metadata"].(map[string]any), "hub-controller", g.uuid(), "system", g.past(240))
	d := g.def(sbGroup, "storageclusters")
	if _, err := g.st.Create(d, g.mcNs, obj); err != nil {
		panic(fmt.Sprintf("projection create failed: %v", err))
	}
}

// drApplications creates the DR-application main object (one per app namespace)
// in the tenant namespace, plus a completed ApplicationFailover for the app
// caught mid-failover. This is the third main object the role model gates
// (sb:dr-application-reader covers it and its sub-objects).
func (g *genCtx) drApplications() {
	for i, appNs := range g.sc.AppNamespaces {
		var pvcs []any
		for _, p := range g.pvcRefs {
			if p.Namespace == appNs {
				pvcs = append(pvcs, map[string]any{"name": p.Name, "namespace": appNs})
			}
		}
		phase := "Protected"
		if g.sc.DRFailoverIn && i == 0 {
			phase = "FailingOver"
		}
		g.add(sbGroup, "protectedapplications", g.ns, appNs,
			map[string]any{
				"applicationNamespace": appNs,
				"drPolicyRef":          "policy-prod",
				"clusters":             []any{g.clusterName, g.drCluster},
				"pvcSelector":          map[string]any{"matchLabels": map[string]any{}},
			},
			map[string]any{
				"phase": phase, "protectedPVCs": pvcs,
				"currentCluster": g.clusterName,
				"lastBackupTime": g.recent(15),
			}, nil)

		if g.sc.DRFailoverIn && i == 0 {
			g.add(sbGroup, "applicationfailovers", g.ns, appNs+"-failover",
				map[string]any{
					"applicationRef": appNs,
					"targetCluster":  g.drCluster,
				},
				map[string]any{
					"phase": "Running", "startedAt": g.recent(5),
					"message": "promoting replicas on " + g.drCluster,
				}, nil)
		}
	}
}

// accessControl creates the RBAC vocabulary and the grants. The role model is
// product-facing: a global sb:infra-admin (who creates/manages tenant
// namespaces and does the bindings), and per tenant a full-admin sb:tenant-admin
// (the provisioning role — CRUD on every object) plus one read-only role per
// main object (sb:cluster-reader / sb:dr-policy-reader / sb:dr-application-reader),
// each covering that object and its sub-objects. The hub API server is the
// single policy decision point, so a grant here and one made with kubectl are
// the same object.
func (g *genCtx) accessControl() {
	for _, r := range sbRoles() {
		verbs := readVerbs()
		if r.Write {
			verbs = adminVerbs()
		}
		resources := make([]any, len(r.Resources))
		for i, res := range r.Resources {
			resources[i] = res
		}
		// non-simplyblock resources sb:infra-admin needs live in other groups
		apiGroups := []any{sbGroup}
		if r.Name == "sb:infra-admin" {
			apiGroups = []any{sbGroup, "", rbacGroup}
		}
		// aggregated role (rules filled by the controller from labelled children)
		g.add(rbacGroup, "clusterroles", "", r.Name, nil, nil, map[string]any{
			"aggregationRule": map[string]any{
				"clusterRoleSelectors": []any{map[string]any{"matchLabels": map[string]any{r.AggLabel: "true"}}},
			},
			"rules": []any{},
		})
		// rules-carrying child, labelled to aggregate into the role above
		g.add(rbacGroup, "clusterroles", "", r.Name+"-rules", nil, nil, map[string]any{
			"labels": map[string]any{r.AggLabel: "true"},
			"rules": []any{map[string]any{
				"apiGroups": apiGroups, "resources": resources, "verbs": verbs,
			}},
		})
	}

	// The global admin, cluster-wide.
	g.clusterRoleBinding("sb-infra-admins", "sb:infra-admin")

	// Per tenant: a full-admin grant and the three read-only grants. The
	// primary tenant gets a full spread; extra (empty) tenants get just an
	// admin so a tenant-admin can provision into them.
	for i, t := range g.tenants {
		ns := nsTenant(t)
		g.grant(t+"-admins", "sb:tenant-admin", ns)
		if i == 0 {
			g.grant(t+"-cluster-viewers", "sb:cluster-reader", ns)
			g.grant(t+"-dr-operators", "sb:dr-policy-reader", ns)
			g.grant(t+"-app-viewers", "sb:dr-application-reader", ns)
		}
	}

	// An AccessGrant CR (the thin CR that adds expiry/reason/DR fan-out, §4.3),
	// scoped to the primary tenant.
	g.add(sbGroup, "accessgrants", g.ns, g.tenant+"-admins",
		map[string]any{
			"subject": map[string]any{"kind": "Group", "name": g.tenant + "-admins"},
			"role":    "sb:tenant-admin",
			"scope":   map[string]any{"kind": scopeTenant, "tenant": g.tenant},
			"reason":  "OPS-4821 tenant ownership",
		},
		map[string]any{
			"phase": "Reconciled", "boundNamespaces": []any{g.ns},
			"conditions": readyCondition("Ready", "True", "BoundingComplete", "RoleBinding created in "+g.ns, g.past(72), 1),
		}, nil)
}

func (g *genCtx) grant(group, role, ns string) {
	g.add(rbacGroup, "rolebindings", ns, fmt.Sprintf("%s-%s", role[3:], group),
		nil, nil, map[string]any{
			"labels":  map[string]any{labManagedBy: "control-center", labScopeKind: scopeTenant},
			"roleRef": map[string]any{"apiGroup": rbacGroup, "kind": "ClusterRole", "name": role},
			"subjects": []any{map[string]any{
				"kind": "Group", "name": group, "apiGroup": rbacGroup,
			}},
		})
}

func (g *genCtx) clusterRoleBinding(group, role string) {
	g.add(rbacGroup, "clusterrolebindings", "", fmt.Sprintf("%s-%s", role[3:], group),
		nil, nil, map[string]any{
			"labels":  map[string]any{labManagedBy: "control-center", labScopeKind: scopeCluster},
			"roleRef": map[string]any{"apiGroup": rbacGroup, "kind": "ClusterRole", "name": role},
			"subjects": []any{map[string]any{
				"kind": "Group", "name": group, "apiGroup": rbacGroup,
			}},
		})
}

// driftPresent is the mock's stand-in for a managed cluster whose reported
// status lags the hub spec — surfaced so the UI's "applied vs pending" and
// drift views have something to render.
func (g *genCtx) driftPresent() bool {
	return g.sc.DegradedDevices > 0 || g.sc.OfflineNodes > 0
}

func boolStr(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func ternStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func toAny[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func randSuffixLocal(r *rand.Rand) string {
	const alphabet = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, 5)
	for i := range b {
		b[i] = alphabet[r.IntN(int(len(alphabet)))]
	}
	return string(b)
}

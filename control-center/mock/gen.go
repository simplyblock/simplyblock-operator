package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"
)

// The generator turns a Scenario into a coherent world inside the Store.
// Coherence is the point: every StorageDevice's nodeRef names a StorageNode
// that exists, every ReplicationSlot's pvcRef a bound PVC, every DRPC the
// DRPolicy — because a UI is tested by following exactly those links.

type genCtx struct {
	st   *Store
	rng  *rand.Rand
	sc   Scenario
	now  time.Time
	site string

	// The hub namespace layout (tenancy.go). ns is the storage-cluster
	// namespace (sb-sc-<cluster>) — the RBAC target that most storage objects
	// live in — so the existing generators need no per-call change.
	ns     string // sb-sc-<cluster>  (user-facing storage-cluster namespace)
	sysNs  string // simplyblock-system (control plane)
	mcNs   string // sb-mc-<managed>   (transport: projected intent + status)
	mcName string // managed cluster name

	workerNames []string
	nodeUUIDs   []string
	poolNames   []string
	pvcRefs     []pvcRef // every generated PVC, for snapshots/backups/slots
	clusterUUID string
	clusterName string
	drCluster   string
}

type pvcRef struct {
	Namespace, Name, VolumeID, StorageClass string
	SizeGi                                  int
}

func Generate(st *Store, sc Scenario, seed uint64, systemNs string) {
	g := &genCtx{
		st:  st,
		rng: rand.New(rand.NewPCG(seed, seed^0xda3e39cb94b95bdb)),
		sc:  sc, now: time.Now().UTC(),
		clusterName: "cluster1",
		drCluster:   "cluster2",
	}
	g.site = dcSites[g.rng.IntN(int(len(dcSites)))]
	g.clusterUUID = g.uuid()
	g.mcName = fmt.Sprintf("prod-%s-1", g.site)
	g.sysNs = systemNs
	g.mcNs = nsManagedCluster(g.mcName)
	g.ns = nsStorageCluster(g.clusterName)

	g.namespaces()
	g.k8sNodes()
	g.tenancyScaffold() // ManagedCluster, NodePoolAllocation, StorageClusterClass
	g.storageCluster()
	g.projectStorageCluster() // transport copy in sb-mc-* (hub→agent, drift)
	g.nodesAndDevices()
	g.poolsAndClasses()
	g.volumes()
	g.snapshots()
	g.backups()
	g.tasks()
	g.opsHistory()
	if sc.WithDR {
		g.replicationChain()
		g.ramen()
	}
	g.workloads()
	g.platformPods()
	g.secrets()
	g.controlPlaneObjects()
	g.accessControl() // sb:* ClusterRoles, RoleBindings, AccessGrants
	g.events()
}

// ---- helpers ------------------------------------------------------------------

func (g *genCtx) def(group, resource string) ResourceDef {
	version := "v1alpha1"
	switch group {
	case "", ocmGroup, kvGroup, rbacGroup, "apps", "storage.k8s.io", "snapshot.storage.k8s.io", "networking.k8s.io":
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

// ---- world building -----------------------------------------------------------

// namespaces builds the hub tenancy layout (§2.1) with hierarchy labels
// (§2.2). Structure is in labels, not names, because namespace names are DNS
// labels: the scope tree, RoleBinding propagation and admission bindings all
// select on these labels.
func (g *genCtx) namespaces() {
	mk := func(name string, labels map[string]any) {
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
	// storage cluster (user-facing / RBAC target)
	mk(g.ns, map[string]any{
		labScopeKind: scopeStorageCluster, labManagedCluster: g.mcName,
		labStorageCluster: g.clusterName,
	})
	// DR system
	mk(drSystemNs, map[string]any{labScopeKind: scopeDR})
	// application namespaces (scope tree leaves)
	for _, n := range g.sc.AppNamespaces {
		mk(n, map[string]any{labScopeKind: scopeApplication})
	}
}

func (g *genCtx) k8sNodes() {
	mk := func(name, role string, storage bool) {
		labels := map[string]any{
			"kubernetes.io/hostname":          name,
			"topology.kubernetes.io/zone":     g.site + string(rune('a'+g.rng.UintN(3))),
			"node-role.kubernetes.io/" + role: "",
		}
		if storage {
			labels["io.simplyblock.node-type"] = "simplyblock-storage-plane"
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
		mk(fmt.Sprintf("mgmt-%s-%d", g.site, i), "control-plane", false)
	}
	for i := 1; i <= g.sc.Workers; i++ {
		name := fmt.Sprintf("worker-%s-%02d", g.site, i)
		g.workerNames = append(g.workerNames, name)
		mk(name, "worker", true)
	}
}

func (g *genCtx) storageCluster() {
	g.add(sbGroup, "storageclusters", g.ns, g.clusterName,
		map[string]any{
			"stripe":               map[string]any{"dataChunks": 2, "parityChunks": 1},
			"fabricType":           "tcp",
			"maxSubsystemCount":    500,
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
		}, nil)
}

func (g *genCtx) nodesAndDevices() {
	offline := map[int]bool{}
	for len(offline) < g.sc.OfflineNodes {
		offline[int(g.rng.IntN(int(g.sc.Workers)))] = true
	}
	degradedLeft := g.sc.DegradedDevices

	var setNodes []any
	for i, worker := range g.workerNames {
		nodeUUID := g.uuid()
		g.nodeUUIDs = append(g.nodeUUIDs, nodeUUID)
		name := fmt.Sprintf("sn-%02d", i+1)
		status := "online"
		health := true
		if offline[i] {
			status, health = "offline", false
		}
		total := int64(0)

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
			g.add(sbGroup, "storagedevices", g.ns, fmt.Sprintf("%s-nvme%d", name, d),
				map[string]any{"deviceID": serial, "nodeRef": name},
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
		}

		used := int64(float64(total) * (0.25 + g.rng.Float64()*0.4))
		nodeStatus := map[string]any{
			"uuid": nodeUUID, "hostname": worker, "status": status, "health": health,
			"failureDomain": float64(i % 3),
			"ports": map[string]any{
				"management": fmt.Sprintf("10.10.%d.%d:5000", i/250, 2+i%250),
				"nvmeof":     float64(4420), "rpc": float64(8080), "lvol": float64(9110 + i),
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
				"workerNode": worker, "nodeIndex": float64(i),
				"storageNodeSetRef": "sb-primary-nodes",
			}, nodeStatus, nil)

		setNodes = append(setNodes, map[string]any{
			"uuid": nodeUUID, "hostname": worker, "status": status, "health": health,
			"failureDomain": float64(i % 3), "devices": fmt.Sprint(g.sc.DevicesPerNode),
			"nvmfPort": float64(4420), "rpcPort": float64(8080), "lvolPort": float64(9110 + i),
			"mgmtIp": fmt.Sprintf("10.10.%d.%d", i/250, 2+i%250),
		})
	}

	g.add(sbGroup, "storagenodesets", g.ns, "sb-primary-nodes",
		map[string]any{
			"clusterName": g.clusterName, "workerNodes": toAny(g.workerNames),
			"mgmtIfname": "eth0", "dataIfname": []any{"eth1"},
			"maxParallelNodeAdds": float64(2),
		},
		map[string]any{
			"totalNodes":   float64(g.sc.Workers),
			"onlineNodes":  float64(g.sc.Workers - g.sc.OfflineNodes),
			"offlineNodes": float64(g.sc.OfflineNodes),
			"nodes":        setNodes,
		}, nil)
}

func (g *genCtx) poolsAndClasses() {
	for i := 0; i < g.sc.Pools; i++ {
		pool := []string{"vol-store", "db-tier", "bulk", "scratch"}[i%4]
		g.poolNames = append(g.poolNames, pool)
		g.add(sbGroup, "storagepools", g.ns, pool,
			map[string]any{
				"clusterName":            g.clusterName,
				"capacityLimit":          gib(4096 * (i + 1)),
				"qos":                    map[string]any{"iops": float64(200000)},
				"storageClassParameters": map[string]any{"filesystem": "xfs", "fabric": "tcp"},
			},
			map[string]any{"uuid": g.uuid(), "status": "active"}, nil)
		g.add("storage.k8s.io", "storageclasses", "", "simplyblock-"+pool, nil, nil, map[string]any{
			"provisioner":          "csi.simplyblock.io",
			"reclaimPolicy":        "Delete",
			"allowVolumeExpansion": true,
			"volumeBindingMode":    "Immediate",
			"parameters": map[string]any{
				"pool_name": pool, "distr_ndcs": "2", "distr_npcs": "1",
			},
		})
	}
}

func (g *genCtx) volumes() {
	for i := 0; i < g.sc.Volumes; i++ {
		appNs := g.sc.AppNamespaces[i%len(g.sc.AppNamespaces)]
		app := appWorkloads[int(g.rng.IntN(int(len(appWorkloads))))]
		pvcName := fmt.Sprintf("%s-%s-%d", g.pick(appVolumeNames), app, i)
		pool := g.poolNames[i%len(g.poolNames)]
		sc := "simplyblock-" + pool
		size := []int{20, 50, 100, 250, 500}[g.rng.UintN(5)]
		volID := g.uuid()
		pvName := "pvc-" + volID

		annotations := map[string]any{}
		if g.sc.WithDR && g.pct(60) {
			annotations["storage.simplyblock.io/replication-policy"] = "policy-prod"
		}
		if g.pct(30) {
			annotations["storage.simplyblock.io/backup-policy"] = "nightly"
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
					"driver": "csi.simplyblock.io", "volumeHandle": volID,
					"fsType": "xfs",
				},
				"claimRef": map[string]any{
					"kind": "PersistentVolumeClaim", "namespace": appNs, "name": pvcName,
				},
				"storageClassName":              sc,
				"persistentVolumeReclaimPolicy": "Delete",
			},
			map[string]any{"phase": "Bound"}, nil)
		g.pvcRefs = append(g.pvcRefs, pvcRef{appNs, pvcName, volID, sc, size})
	}
}

func (g *genCtx) snapshots() {
	if len(g.pvcRefs) == 0 {
		return
	}
	g.add("snapshot.storage.k8s.io", "volumesnapshotclasses", "", "simplyblock-snapshots", nil, nil, map[string]any{
		"driver":         "csi.simplyblock.io",
		"deletionPolicy": "Delete",
	})
	for i := 0; i < g.sc.Snapshots; i++ {
		p := g.pvcRefs[g.rng.IntN(int(len(g.pvcRefs)))]
		name := fmt.Sprintf("snap-%s-%d", p.Name, i)
		contentName := "snapcontent-" + g.uuid()
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
				"volumeSnapshotRef": map[string]any{
					"kind": "VolumeSnapshot", "name": name, "namespace": p.Namespace,
				},
			},
			map[string]any{"readyToUse": true, "snapshotHandle": g.uuid()}, nil)
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
	g.add(sbGroup, "backuppolicies", g.ns, "nightly",
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

func (g *genCtx) tasks() {
	var entries []any
	n := 4 + int(g.rng.UintN(6))
	for i := 0; i < n; i++ {
		status := "done"
		result := "success"
		if g.sc.RunningOps && i == 0 {
			status, result = "running", ""
		}
		if g.sc.FailedOps > 0 && i == 1 {
			result = "failed: device unreachable"
		}
		entries = append(entries, map[string]any{
			"uuid": g.uuid(), "taskType": g.pick(taskTypes),
			"taskStatus": status, "taskResult": result,
			"startedAt": g.past(24), "retried": float64(g.rng.UintN(3)),
			"canceled": false,
		})
	}
	g.add(sbGroup, "tasks", g.ns, "cluster-tasks",
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

	g.add(sbGroup, "storageclusterops", g.ns, "activate-initial",
		map[string]any{"action": "activate", "clusterRef": g.clusterName},
		mkStatus("Succeeded", 200), nil)

	failed := g.sc.FailedOps
	for i := 0; i < failed; i++ {
		g.add(sbGroup, "storagenodeops", g.ns, fmt.Sprintf("restart-sn-%02d-failed-%d", 1+int(g.rng.IntN(int(g.sc.Workers))), i),
			map[string]any{"action": "restart", "storageNodeRef": fmt.Sprintf("sn-%02d", 1+g.rng.IntN(int(g.sc.Workers)))},
			mkStatus("Failed", 24+i*10), nil)
	}

	if g.sc.RunningOps {
		st := mkStatus("Running", 1)
		st["subPhase"] = "Restarting"
		g.add(sbGroup, "storagenodeops", g.ns, "restart-sn-02",
			map[string]any{"action": "restart", "storageNodeRef": "sn-02"}, st, nil)

		g.add(sbGroup, "storageclusterops", g.ns, "rolling-restart",
			map[string]any{"action": "node-rolling-restart", "clusterRef": g.clusterName},
			map[string]any{
				"phase": "Running", "startedAt": g.recent(30),
				"nodeRollingRestartStatus": map[string]any{
					"nodePhase":      "Running",
					"processedNodes": []any{"sn-01"},
					"pendingNodes":   toAny(nodeNames(3, g.sc.Workers)),
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
				}, nil)
		}
	}

	g.add(sbGroup, "operatorops", g.ns, "discover-initial",
		map[string]any{"action": "Discover", "discover": map[string]any{"configName": "sb-primary-config"}},
		map[string]any{
			"phase": "Succeeded", "startedAt": g.past(240), "completedAt": g.past(239),
			"configRef": "sb-primary-config", "environment": "Vanilla",
			"workers": toAny(g.workerNames),
		}, nil)
}

func nodeNames(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("sn-%02d", i))
	}
	return out
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
			g.addPod(appNs, fmt.Sprintf("%s-%d", app, i), app, g.workerNames[int(g.rng.IntN(int(len(g.workerNames))))], "Running")
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

func (g *genCtx) platformPods() {
	for i, worker := range g.workerNames {
		phase := "Running"
		if i < g.sc.OfflineNodes {
			phase = "Pending"
		}
		g.addPod(g.sysNs, fmt.Sprintf("simplyblock-storage-node-ds-%s", randSuffixLocal(g.rng)), "storage-node", worker, phase)
	}
	mgmt := fmt.Sprintf("mgmt-%s-1", g.site)
	for _, cp := range []string{"simplyblock-operator", "simplyblock-webappapi", "simplyblock-prometheus", "simplyblock-control-center", "sb-agent"} {
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
		nodeIdx := int(g.rng.IntN(int(g.sc.Workers)))
		ts := g.recent(600)
		g.add("", "events", g.ns, fmt.Sprintf("sn-%02d.%s", nodeIdx+1, randSuffixLocal(g.rng)),
			nil, nil, map[string]any{
				"type": ev.Type, "reason": ev.Reason,
				"message": ev.Message,
				"involvedObject": map[string]any{
					"kind": "StorageNode", "name": fmt.Sprintf("sn-%02d", nodeIdx+1),
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
				labScopeKind:      scopeStorageCluster,
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

// accessControl creates the RBAC vocabulary and a few grants (§2.3, §4.2): the
// eight aggregated sb:* ClusterRoles (each with a rules-carrying child role),
// RoleBindings for the grants, and AccessGrant CRs. The hub API server is the
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
				"apiGroups": []any{sbGroup}, "resources": resources, "verbs": verbs,
			}},
		})
	}

	// Grants: infra-admin cluster-wide, cluster-admin + pool-admin on the
	// storage cluster, dr-admin on sb-dr-system. Bindings carry the same
	// managed-by/scope labels the controller writes (§4.2).
	g.clusterRoleBinding("sb-infra-admins", "sb:infra-admin")
	g.grant("sb-cluster1-admins", "sb:cluster-admin", g.ns, scopeStorageCluster)
	g.grant("team-a-pool-admins", "sb:pool-admin", g.ns, scopeStoragePool)
	if g.sc.WithDR {
		g.grant("sb-dr-admins", "sb:dr-admin", drSystemNs, scopeDR)
	}

	// An AccessGrant CR (the thin CR that adds expiry/reason/DR fan-out, §4.3).
	g.add(sbGroup, "accessgrants", g.ns, "cluster1-admins",
		map[string]any{
			"subject": map[string]any{"kind": "Group", "name": "sb-cluster1-admins"},
			"role":    "sb:cluster-admin",
			"scope":   map[string]any{"kind": scopeStorageCluster, "storageCluster": g.clusterName},
			"reason":  "OPS-4821 cluster ownership",
		},
		map[string]any{
			"phase": "Reconciled", "boundNamespaces": []any{g.ns},
			"conditions": readyCondition("Ready", "True", "BoundingComplete", "RoleBinding created in "+g.ns, g.past(72), 1),
		}, nil)
}

func (g *genCtx) grant(group, role, ns, scopeKind string) {
	g.add(rbacGroup, "rolebindings", ns, fmt.Sprintf("%s-%s", role[3:], group),
		nil, nil, map[string]any{
			"labels":  map[string]any{labManagedBy: "control-center", labScopeKind: scopeKind},
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

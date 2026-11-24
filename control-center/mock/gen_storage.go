package main

// The storage world, shaped by storage.simplyblock.io/v1alpha2 exactly as the
// operator's CRDs on main validate it (operator/config/crd/bases): a
// StorageCluster, its StorageNodes and StorageDevices with the labels and
// names the operator writes, the StoragePools and the StorageClasses assigned
// to them, the backup kinds, the ControlPlane and the CSI driver, and the
// operations history as Ops objects standing in the operator's own step
// graphs. It is a separate file from gen.go because every field here is a
// schema field; a value that is not in the CRD does not get added here.

import (
	"fmt"
	"strings"
)

// names the operator derives (atlas-lib/kube and the deployment expansion)
func storageNodeName(cluster, worker string, slot int) string {
	return fmt.Sprintf("%s-%s-%d", cluster, worker, slot)
}
func storageDeviceName(node, deviceID string) string {
	short := deviceID
	if len(short) > 8 {
		short = short[:8]
	}
	return node + "-" + short
}
func defaultPoolName(cluster string) string { return cluster + "-default" }
func defaultStorageClassName(ns, cluster string) string {
	rest := ns + "-" + cluster
	for strings.HasPrefix(rest, "simplyblock-") {
		rest = strings.TrimPrefix(rest, "simplyblock-")
	}
	return "simplyblock-" + rest
}

const (
	labSbCluster   = "storage.simplyblock.io/cluster"
	labSbNode      = "storage.simplyblock.io/node"
	labSbWorker    = "storage.simplyblock.io/worker"
	labSbNamespace = "storage.simplyblock.io/namespace"
	labSbPool      = "storage.simplyblock.io/pool"
	labSbManagedBy = "storage.simplyblock.io/managed-by"
	labDrainNode   = "storage.simplyblock.io/drain-node"
	labTargetNode  = "storage.simplyblock.io/target-node"
	labRebalancer  = "storage.simplyblock.io/rebalancer"
)

// ownerRef is a controller owner reference to an object the generator created.
func ownerRef(obj map[string]any) []any {
	return []any{map[string]any{
		"apiVersion": obj["apiVersion"], "kind": obj["kind"],
		"name": getStr(obj, "metadata.name"), "uid": getStr(obj, "metadata.uid"),
		"controller": true, "blockOwnerDeletion": true,
	}}
}

type genNode struct {
	name, worker, uuid, mgmtIP, dataIP string
	status, phase                      string // control plane status, operator phase
	devices                            []genDevice
	obj                                map[string]any
}
type genDevice struct {
	name, id, status, phase, role string
	obj                           map[string]any
}

// clusterCreationStep is where a created cluster's machine rests
// (StorageClusterStepPersisting is the terminal state of the creation graph).
const clusterCreationStep = "Persisting"

func (g *genCtx) storageCluster() {
	phase, cpStatus := "Online", "active"
	if g.sc.OfflineNodes > 0 || g.sc.DegradedDevices > 0 {
		phase, cpStatus = "Degraded", "degraded"
	}
	spec := map[string]any{
		"clusterMode": "Standard", "deviceClass": "NVMe", "fabricType": "tcp",
		"stripe":            map[string]any{"dataChunks": float64(2), "parityChunks": float64(1)},
		"vcpuCount":         float64(8),
		"maxSubsystemCount": float64(500),
		"minHugePagesSize":  "8Gi",
		"nvmfBasePort":      float64(4420), "rpcBasePort": float64(8080), "snodeApiPort": float64(50001),
		"enableFailureDomains":        g.sc.Workers >= 6,
		"enableMetadataJournal":       true,
		"maxConcurrentWorkerRestarts": float64(1),
		"warningThreshold":            map[string]any{"capacity": float64(75), "provisionedCapacity": float64(180)},
		"criticalThreshold":           map[string]any{"capacity": float64(90), "provisionedCapacity": float64(220)},
		"storageNodes": map[string]any{
			"mgmtInterface": "eth0", "dataInterfaces": []any{"eth1"},
			"nodesPerSocket": float64(1), "nodeProvisioningBudget": float64(1),
			"image": "simplyblock/storage-node:26.3.0", "imagePullPolicy": "Always",
			"enableJournalDevice": g.sc.DevicesPerNode > 1,
			"network":             map[string]any{"spdkOnHostNetwork": true},
		},
	}
	if g.sc.Backups > 0 {
		spec["backup"] = map[string]any{
			"bucket": "sb-backups-" + g.site, "endpoint": "https://s3." + g.site + ".example.com", "region": g.site,
			"credentialsSecretRef": map[string]any{"name": "sb-backup-s3"},
		}
	}
	status := map[string]any{
		"uuid": g.clusterUUID, "clusterName": g.clusterName,
		"phase": phase, "status": cpStatus, "configured": true,
		"erasureCodingScheme": "2x1", "maxFaultTolerance": float64(1),
		"nqn":                         "nqn.2023-02.io.simplyblock:" + g.clusterUUID,
		"maxConcurrentWorkerRestarts": float64(1),
		"rebalancing":                 false,
		"step":                        map[string]any{"state": clusterCreationStep},
		"tasks":                       []any{},
		"provisioningSlots":           []any{},
		"volumeMoveGeneration":        float64(g.rng.UintN(12)),
		"workloadNames": map[string]any{
			"nodeAPIService": g.clusterName + "-node-api", "nodeAPITLSSecret": g.clusterName + "-node-api-tls",
			"spdkProxyService": g.clusterName + "-spdk-proxy", "spdkProxyTLSSecret": g.clusterName + "-spdk-proxy-tls",
		},
	}
	status["realignedGeneration"] = status["volumeMoveGeneration"]
	if g.sc.Workers >= 6 {
		var fds []any
		for i, z := range g.failureDomains() {
			fds = append(fds, map[string]any{"name": z, "index": float64(i)})
		}
		status["failureDomains"] = fds
	}
	g.cluster = g.add(sbGroup, "storageclusters", g.ns, g.clusterName, spec, status, nil)
}

// failureDomains are the labels the nodes declare in spec.config.failureDomain,
// three zones named after the site.
func (g *genCtx) failureDomains() []string {
	return []string{g.site + "-a", g.site + "-b", g.site + "-c"}
}

func (g *genCtx) nodesAndDevices() {
	offline := map[int]bool{}
	for len(offline) < g.sc.OfflineNodes {
		offline[int(g.rng.IntN(int(g.sc.Workers)))] = true
	}
	degradedLeft := g.sc.DegradedDevices
	fds := g.failureDomains()

	for i, worker := range g.workerNames {
		n := genNode{worker: worker, uuid: g.uuid(), name: storageNodeName(g.clusterName, worker, 0)}
		n.mgmtIP = fmt.Sprintf("10.10.%d.%d", i/250, 2+i%250)
		n.dataIP = fmt.Sprintf("10.20.%d.%d", i/250, 2+i%250)
		n.status, n.phase = "online", "Online"
		health := true
		if offline[i] {
			n.status, n.phase, health = "offline", "Offline", false
		}
		g.nodeUUIDs = append(g.nodeUUIDs, n.uuid)

		spec := map[string]any{
			"clusterRef": g.clusterName, "workerNode": worker,
			"slot": float64(0), "socketId": "0", "nodeIndex": float64(0), "nodeSet": "sb-primary-nodes",
			"config": map[string]any{
				"sizing":        map[string]any{"vcpuCount": float64(8), "minHugePagesSize": "8Gi"},
				"mgmtInterface": "eth0", "dataInterfaces": []any{"eth1"},
				"expand": false,
			},
		}
		if g.sc.Workers >= 6 {
			spec["config"].(map[string]any)["failureDomain"] = fds[i%3]
		}

		total := int64(0)
		model := nvmeModels[g.rng.IntN(int(len(nvmeModels)))]
		for d := 0; d < g.sc.DevicesPerNode; d++ {
			total += int64(model.SizeTB * 1e12)
		}
		used := int64(float64(total) * (0.25 + g.rng.Float64()*0.4))
		status := map[string]any{
			"uuid": n.uuid, "hostname": worker, "status": n.status, "health": health, "phase": n.phase,
			"ports": map[string]any{
				"management": n.mgmtIP, "nvmeof": float64(4420), "rpc": float64(8080), "lvol": float64(9110 + i),
			},
			"resources": map[string]any{
				"cpu": float64(8), "memory": gib(32 + 32*int(g.rng.UintN(3))),
				"devices":  map[string]any{"online": float64(g.sc.DevicesPerNode), "total": float64(g.sc.DevicesPerNode)},
				"volumes":  float64(g.rng.IntN(int(1 + g.sc.Volumes/max(1, g.sc.Workers)*2))),
				"capacity": map[string]any{"totalBytes": float64(total), "usedBytes": float64(used), "sampledAt": g.recent(5)},
			},
			"uptime": fmt.Sprintf("%dd%dh", 1+g.rng.UintN(90), g.rng.UintN(24)),
			"network": []any{map[string]any{
				"network": "data", "phase": "Attached", "address": n.dataIP, "assigned": n.dataIP + "/24",
				"interface": "eth1", "hostReachable": true,
			}},
			"step":          map[string]any{"state": "Resolving"},
			"nodeAddTaskID": g.uuid(),
			"latencyMetrics": map[string]any{
				"nodeUUID": n.uuid, "baselineMeasuredAt": g.past(48),
				"baselineP50NS": float64(80000 + g.rng.UintN(40000)),
				"baselineP99NS": float64(400000 + g.rng.UintN(300000)),
			},
		}
		if g.sc.Workers >= 6 {
			status["failureDomain"] = fds[i%3]
		}
		if !health {
			status["message"] = "the control plane reports the node offline"
			status["uptime"] = ""
		}
		n.obj = g.add(sbGroup, "storagenodes", g.ns, n.name, spec, status, map[string]any{
			"labels":          map[string]any{labSbCluster: g.clusterName, labSbWorker: worker},
			"ownerReferences": ownerRef(g.cluster),
		})

		for d := 0; d < g.sc.DevicesPerNode; d++ {
			dev := genDevice{id: g.uuid(), status: "online", phase: "Online", role: "Storage"}
			dev.name = storageDeviceName(n.name, dev.id)
			if d == 0 && g.sc.DevicesPerNode > 1 {
				dev.role, dev.status = "Journal", "JM_DEV"
			}
			smartVerdict, perfVerdict := "Ok", "Ok"
			mediaErrors := float64(0)
			smartReason := "no critical warning, spare above threshold"
			if degradedLeft > 0 && dev.role == "Storage" && g.pct(30) {
				dev.phase, dev.status = "Degraded", "unavailable"
				smartVerdict, mediaErrors, smartReason = "Warning", float64(3+g.rng.UintN(40)), "media errors increasing"
				degradedLeft--
			}
			if !health {
				dev.phase, dev.status = "Unknown", "unavailable"
			}
			devTotal := int64(model.SizeTB * 1e12)
			serial := fmt.Sprintf("%s%07d", model.SerialPfx, g.rng.UintN(9999999))
			deviceOnline := float64(g.sc.DevicesPerNode)
			if dev.phase == "Degraded" || dev.phase == "Unknown" {
				deviceOnline--
				setPath(n.obj, "status.resources.devices.online", deviceOnline)
			}
			status := map[string]any{
				"clusterID": g.clusterUUID, "nodeID": n.uuid,
				"phase": dev.phase, "deviceStatus": dev.status, "role": dev.role,
				"capacity": map[string]any{"totalBytes": float64(devTotal)},
				"hardware": map[string]any{
					"model": model.Model, "serialNumber": serial,
					"pciAddress":     fmt.Sprintf("0000:%02x:00.0", 0x17+d),
					"nvmeController": fmt.Sprintf("nvme%d", d),
				},
				"health": map[string]any{
					"smart": map[string]any{
						"verdict": smartVerdict, "checkedAt": g.recent(120), "source": "spdk", "reason": smartReason,
						"temperatureCelsius": float64(31 + g.rng.UintN(14)), "percentageUsed": float64(g.rng.UintN(40)),
						"availableSparePercent": float64(90 + g.rng.UintN(11)), "availableSpareThresholdPercent": float64(10),
						"mediaErrors": mediaErrors, "criticalWarning": float64(0),
						"powerOnHours": float64(2000 + g.rng.UintN(30000)), "unsafeShutdowns": float64(g.rng.UintN(4)),
					},
					"performance": map[string]any{
						"verdict": perfVerdict, "assessedAt": g.recent(60), "since": g.past(200),
						"latencyMicroseconds": float64(80 + g.rng.UintN(60)), "fleetLatencyMicroseconds": float64(95),
						"latencyRatioPercent": float64(80 + g.rng.UintN(50)), "iopsRatioPercent": float64(85 + g.rng.UintN(30)),
						"reason": "latency within the fleet band",
					},
				},
			}
			if dev.phase == "Degraded" {
				status["message"] = "the control plane reports the device unavailable"
			}
			dev.obj = g.add(sbGroup, "storagedevices", g.ns, dev.name,
				map[string]any{"deviceID": dev.id, "nodeRef": n.name}, status,
				map[string]any{
					"labels":          map[string]any{labSbCluster: g.clusterName, labSbNode: n.name, labSbWorker: worker},
					"ownerReferences": ownerRef(n.obj),
				})
			n.devices = append(n.devices, dev)
		}
		g.st.Update(g.def(sbGroup, "storagenodes"), g.ns, n.obj)
		g.nodes = append(g.nodes, n)
	}
}

func (g *genCtx) poolsAndClasses() {
	for i := 0; i < g.sc.Pools; i++ {
		pool := defaultPoolName(g.clusterName)
		class := defaultStorageClassName(g.ns, g.clusterName)
		if i > 0 {
			pool = []string{"db-tier", "bulk", "scratch"}[(i-1)%3]
			class = class + "-" + pool
		}
		g.poolNames = append(g.poolNames, pool)
		poolUUID := g.uuid()
		g.poolUUIDs = append(g.poolUUIDs, poolUUID)
		limits := map[string]any{"capacity": fmt.Sprintf("%dTi", 4*(i+1)), "maxVolumeSize": "2Ti"}
		defaults := map[string]any{"filesystem": "xfs", "fabric": "tcp"}
		params := map[string]any{"cluster_id": g.clusterUUID, "pool_name": pool, "csi.storage.k8s.io/fstype": "xfs", "fabric": "tcp"}
		if i == 1 {
			defaults["iops"] = float64(200000)
			params["max_iops"] = "200000"
		}
		status := map[string]any{
			"uuid": poolUUID, "phase": "Ready", "status": "active",
			"limits":            map[string]any{"capacity": limits["capacity"], "host": g.nodes[0].mgmtIP},
			"storageClassNames": []any{class},
		}
		if i == 0 {
			status["defaultStorageClassName"] = class
		}
		g.add(sbGroup, "storagepools", g.ns, pool,
			map[string]any{"clusterRef": g.clusterName, "limits": limits, "volumeDefaults": defaults},
			status, nil)
		g.add("storage.k8s.io", "storageclasses", "", class, nil, nil, map[string]any{
			"labels": map[string]any{
				labSbNamespace: g.ns, labSbCluster: g.clusterName, labSbPool: pool, labSbManagedBy: "StorageCluster",
			},
			"provisioner":          "csi.simplyblock.io",
			"reclaimPolicy":        "Delete",
			"allowVolumeExpansion": true,
			"volumeBindingMode":    "Immediate",
			"parameters":           params,
		})
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
			"name": p.Name, "persistentVolumeName": "pvc-" + p.VolumeID, "lvolID": p.VolumeID,
			"clusterID": g.clusterUUID, "attachedAt": g.past(300),
		})
	}
	policyID := g.uuid()
	g.add(sbGroup, "storagebackuppolicies", g.ns, "nightly",
		map[string]any{
			"clusterRef": g.clusterName, "schedule": "0 2 * * *",
			"maxVersions":   float64(7), "maxAge": "168h",
			"claimSelector": map[string]any{"matchLabels": map[string]any{"simplyblock.io/backup-policy": "nightly"}},
		},
		map[string]any{
			"phase": "Active", "policyID": policyID, "clusterID": g.clusterUUID,
			"attachedClaims": attached, "lastBackupAt": g.past(20),
		}, nil)

	for i := 0; i < g.sc.Backups; i++ {
		p := g.pvcRefs[g.rng.IntN(int(len(g.pvcRefs)))]
		phase, apiStatus := "Available", "done"
		if i == 0 && g.sc.RunningOps {
			phase, apiStatus = "Creating", "running"
		}
		node := g.nodes[g.rng.IntN(int(len(g.nodes)))]
		status := map[string]any{
			"phase": phase, "apiStatus": apiStatus, "clusterID": g.clusterUUID,
			"backup": map[string]any{
				"backupID": g.uuid(), "size": float64(p.SizeGi) * float64(1<<30) * 0.6,
				"startedAt": g.past(96), "s3ID": float64(1000 + i),
			},
			"source": map[string]any{
				"claimName": p.Name, "claimNamespace": p.Namespace, "persistentVolumeName": "pvc-" + p.VolumeID,
				"lvolID": p.VolumeID, "lvolName": p.Name, "poolName": g.poolNames[0], "poolUUID": g.poolUUIDs[0],
				"clusterUUID": g.clusterUUID, "fsType": "xfs", "volumeMode": "Filesystem", "nodeID": node.uuid,
			},
		}
		if phase == "Available" {
			status["backup"].(map[string]any)["completedAt"] = g.past(90)
		}
		g.add(sbGroup, "storagebackups", p.Namespace, fmt.Sprintf("backup-%s-%d", p.Name, i),
			map[string]any{"source": map[string]any{"claimName": p.Name}}, status, nil)
	}
}

func (g *genCtx) controlPlaneObjects() {
	g.add(sbGroup, "controlplanes", g.sysNs, "simplyblock",
		map[string]any{"source": map[string]any{"local": map[string]any{
			"image": "quay.io/simplyblock-io/simplyblock:26.3.0", "imagePullPolicy": "IfNotPresent", "replicas": float64(2),
			"foundationDB": map[string]any{"replicas": float64(3), "storageClassName": "local-path"},
			"tls":          map[string]any{"enableTLS": true, "enableMutualTLS": true, "provider": "cert-manager"},
		}}},
		map[string]any{
			"phase": "Available", "endpoint": "https://simplyblock-webappapi." + g.sysNs + ".svc:5000",
			"version": "26.3.0", "lastChecked": g.recent(2), "storageClassName": "local-path",
			"step": map[string]any{"state": "Installed"},
			"components": []any{
				map[string]any{"name": "simplyblock-webappapi", "desired": float64(2), "ready": float64(2), "essential": true},
				map[string]any{"name": "simplyblock-admin-control", "desired": float64(1), "ready": float64(1), "essential": true},
				map[string]any{"name": "simplyblock-tasks", "desired": float64(1), "ready": float64(1), "essential": true},
				map[string]any{"name": "simplyblock-monitoring", "desired": float64(1), "ready": float64(1), "essential": false},
				map[string]any{"name": "foundationdb", "desired": float64(3), "ready": float64(3), "essential": true},
			},
		}, nil)

	g.add(sbGroup, "simplyblockdrivers", g.sysNs, "simplyblock",
		map[string]any{"driverName": "csi.simplyblock.io", "enableVolumeSnapshots": true, "controllerReplicas": float64(1)},
		map[string]any{
			"phase": "Ready", "version": "26.3.0", "controllerReady": true,
			"nodesReady": float64(g.sc.Workers + g.sc.MgmtNodes), "nodesTotal": float64(g.sc.Workers + g.sc.MgmtNodes),
			"snapshotSupport": "Detected", "origin": "Created",
		}, nil)

	var pcie []any
	for d := 0; d < g.sc.DevicesPerNode; d++ {
		pcie = append(pcie, fmt.Sprintf("0000:%02x:00.0", 0x17+d))
	}
	var nodeRefs []any
	for _, n := range g.nodes {
		nodeRefs = append(nodeRefs, n.name)
	}
	g.deployConfig = g.add(sbGroup, "clusterdeploymentconfigs", g.ns, "sb-primary-config",
		map[string]any{
			"approved":    true,
			"environment": "Vanilla",
			"cluster": map[string]any{
				"name": g.clusterName, "fabricType": "tcp", "vcpuCount": float64(8), "maxSubsystemCount": float64(500),
				"stripe": map[string]any{"dataChunks": float64(2), "parityChunks": float64(1)},
			},
			"nodeSets": []any{map[string]any{"name": "sb-primary-nodes", "groups": []any{map[string]any{
				"name": "default", "workers": toAny(g.workerNames),
				"mgmtInterface": "eth0", "dataInterfaces": []any{"eth1"},
				"devices": map[string]any{"nvme": pcie},
			}}}},
		},
		map[string]any{
			"phase": "Expanded", "step": map[string]any{"state": "Completed"},
			"clusterRef": g.clusterName, "nodeRefs": nodeRefs, "expansionStartedAt": g.past(240),
		}, nil)
}

// opsStatus is the status every Ops kind shares, at a step of its graph.
func (g *genCtx) opsStatus(phase, step string, hoursAgo int, message string) map[string]any {
	st := map[string]any{"phase": phase, "message": message}
	switch phase {
	case "Succeeded":
		st["startedAt"], st["completedAt"] = g.past(hoursAgo), g.past(hoursAgo-1)
		st["step"] = map[string]any{"state": "Completed"}
	case "Failed", "Aborted":
		st["startedAt"], st["completedAt"] = g.past(hoursAgo), g.past(hoursAgo-1)
		st["step"] = map[string]any{"state": step}
	case "Running":
		st["startedAt"] = g.recent(hoursAgo * 60)
		st["step"] = map[string]any{"state": step,
			"claim":    map[string]any{"state": step, "attempt": float64(1), "leaseUntil": g.future(1)},
			"deadline": g.future(30)}
	case "Pending":
		st["step"] = map[string]any{}
	}
	return st
}

// taskWindow is the cluster's status.tasks: the control plane's unfinished
// jobs, as the operator mirrors them.
func (g *genCtx) addTask(typ, status string, retry int) string {
	id := g.uuid()
	tasks, _ := getPath(g.cluster, "status.tasks").([]any)
	tasks = append(tasks, map[string]any{"id": id, "type": typ, "status": status, "retry": float64(retry)})
	setPath(g.cluster, "status.tasks", tasks)
	return id
}

// opsHistory seeds the operations a cluster this old has been through, plus,
// when the scenario says so, the ones in flight for the simulator to walk.
func (g *genCtx) opsHistory() {
	g.add(sbGroup, "storageclusterops", g.ns, g.clusterName+"-activate",
		map[string]any{"clusterRef": g.clusterName, "action": "Activate"},
		g.opsStatus("Succeeded", "", 200, "the control plane reports the cluster active"),
		map[string]any{"ownerReferences": ownerRef(g.deployConfig)})

	g.add(sbGroup, "operatorops", g.ns, "discover-initial",
		map[string]any{"action": "Discover", "discover": map[string]any{"configName": "sb-primary-config", "workers": toAny(g.workerNames)}},
		func() map[string]any {
			st := g.opsStatus("Succeeded", "", 240, "wrote sb-primary-config")
			st["configRef"], st["environment"], st["workers"] = "sb-primary-config", "Vanilla", toAny(g.workerNames)
			return st
		}(), nil)

	for i := 0; i < g.sc.FailedOps; i++ {
		n := g.nodes[g.rng.IntN(int(len(g.nodes)))]
		g.add(sbGroup, "storagenodeops", g.ns, fmt.Sprintf("%s-restart-%d", n.name, i),
			map[string]any{"nodeRef": n.name, "action": "Restart"},
			g.opsStatus("Failed", "Awaiting", 24+i*10, "step Awaiting expired: the node did not return to online within 30m"), nil)
	}

	// a device the control plane failed and migrated, through a Fail operation
	for _, n := range g.nodes {
		for di, d := range n.devices {
			if d.phase != "Degraded" || di == 0 {
				continue
			}
			setPath(d.obj, "status.phase", "Migrated")
			setPath(d.obj, "status.deviceStatus", "failed_and_migrated")
			setPath(d.obj, "status.message", "failed and migrated by "+d.name+"-fail")
			g.st.Update(g.def(sbGroup, "storagedevices"), g.ns, d.obj)
			st := g.opsStatus("Succeeded", "", 12, "the control plane reports the device failed_and_migrated")
			st["deviceStatusBefore"] = "unavailable"
			g.add(sbGroup, "storagedeviceops", g.ns, d.name+"-fail",
				map[string]any{"deviceRef": d.name, "action": "Fail"}, st, nil)
			break
		}
	}

	if !g.sc.RunningOps || len(g.nodes) < 3 {
		return
	}

	// a node restart in flight: the node has left online, the operation waits
	restarting := g.nodes[1]
	op := g.add(sbGroup, "storagenodeops", g.ns, restarting.name+"-restart",
		map[string]any{"nodeRef": restarting.name, "action": "Restart"},
		g.opsStatus("Running", "Departing", 1, "restart requested; waiting for the node to leave online"), nil)
	setPath(restarting.obj, "status.status", "in_restart")
	setPath(restarting.obj, "status.phase", "Provisioning")
	setPath(restarting.obj, "status.activeOpsRef", getStr(op, "metadata.name"))
	g.st.Update(g.def(sbGroup, "storagenodes"), g.ns, restarting.obj)
	g.addTask("node_restart", "running", 0)

	// a rolling restart, at the peer check before its second node
	var uuids []any
	for _, n := range g.nodes {
		uuids = append(uuids, n.uuid)
	}
	rr := g.opsStatus("Running", "CheckingPeers", 1, fmt.Sprintf("Node 2/%d (%s): CheckingPeers", len(g.nodes), g.nodes[1].uuid))
	rr["rollingRestart"] = map[string]any{"nodes": uuids, "nodeIndex": float64(1), "skipped": []any{}}
	rop := g.add(sbGroup, "storageclusterops", g.ns, g.clusterName+"-rollingrestart",
		map[string]any{"clusterRef": g.clusterName, "action": "RollingRestart"}, rr, nil)
	setPath(g.cluster, "status.activeOpsRef", getStr(rop, "metadata.name"))
	g.addTask("balancing_on_restart", "new", 0)

	// a node removal draining its volumes: devices already migrated, one
	// PersistentVolumeOps running and one waiting for the cluster's slot
	leaving := g.nodes[len(g.nodes)-1]
	target := g.nodes[0]
	devStates := map[string]any{}
	for _, d := range leaving.devices {
		devStates[d.name] = "failed_and_migrated"
	}
	rm := g.opsStatus("Running", "MigratingVolumes", 2, "migrating the node's volumes: 1 of 3 complete")
	rm["drain"] = map[string]any{"volumesTotal": float64(3), "volumesMigrated": float64(1)}
	rm["removal"] = map[string]any{"nodeStatus": "migrating_lvols", "devices": devStates,
		"prepareAttempts": float64(1), "lastPrepareTime": g.recent(90), "lastProgressTime": g.recent(10)}
	rmop := g.add(sbGroup, "storagenodeops", g.ns, leaving.name+"-remove",
		map[string]any{"nodeRef": leaving.name, "action": "Remove", "remove": map[string]any{"systemVolumeFilterRegex": "^sb-fio-baseline-.*"}}, rm, nil)
	setPath(leaving.obj, "status.status", "migrating_lvols")
	setPath(leaving.obj, "status.phase", "Removing")
	setPath(leaving.obj, "status.activeOpsRef", getStr(rmop, "metadata.name"))
	g.st.Update(g.def(sbGroup, "storagenodes"), g.ns, leaving.obj)
	for _, d := range leaving.devices {
		setPath(d.obj, "status.phase", "Migrated")
		setPath(d.obj, "status.deviceStatus", "failed_and_migrated")
		g.st.Update(g.def(sbGroup, "storagedevices"), g.ns, d.obj)
	}
	g.addTask("lvol_migration", "running", 0)
	creator := map[string]any{"kind": "StorageNodeOps", "namespace": g.ns, "name": getStr(rmop, "metadata.name"), "uid": getStr(rmop, "metadata.uid")}
	for i := 0; i < min(2, len(g.pvcRefs)); i++ {
		p := g.pvcRefs[i]
		phase, step := "Running", "Migrating"
		msg := "copying the volume to " + target.name
		if i == 1 {
			phase, step, msg = "Pending", "", "waiting for the cluster's migration slot held by drain-"+g.pvcRefs[0].VolumeID[:8]
		}
		st := g.opsStatus(phase, step, 1, msg)
		if phase == "Running" {
			st["migration"] = map[string]any{
				"migrationUUID": g.uuid(), "clusterUUID": g.clusterUUID, "poolUUID": g.poolUUIDs[0], "volumeUUID": p.VolumeID,
				"subsystemNQN": "nqn.2023-02.io.simplyblock:" + p.VolumeID, "sourceNodeUUID": leaving.uuid, "targetNodeUUID": target.uuid,
				"memberCount": float64(1), "continuedAt": g.recent(5),
			}
		} else {
			st["deferredSince"] = g.recent(20)
		}
		g.add(sbGroup, "persistentvolumeops", "", "drain-"+p.VolumeID[:8],
			map[string]any{
				"persistentVolumeName": "pvc-" + p.VolumeID, "action": "Migrate",
				"migrate":    map[string]any{"targetNodeRef": map[string]any{"namespace": g.ns, "name": target.name}},
				"creatorRef": creator,
			}, st,
			map[string]any{"labels": map[string]any{labDrainNode: leaving.uuid, labTargetNode: target.uuid, labSbManagedBy: "StorageNodeOps"}})
	}

	// the auto-rebalancer's own migration, queued behind the drain
	if len(g.pvcRefs) > 2 {
		p := g.pvcRefs[2]
		st := g.opsStatus("Pending", "", 1, "waiting for the cluster's migration slot")
		st["deferredSince"] = g.recent(8)
		g.add(sbGroup, "persistentvolumeops", "", "rebalance-"+p.VolumeID,
			map[string]any{
				"persistentVolumeName": "pvc-" + p.VolumeID, "action": "Migrate",
				"migrate":    map[string]any{"targetNodeRef": map[string]any{"namespace": g.ns, "name": g.nodes[2].name}},
				"creatorRef": map[string]any{"kind": "StorageCluster", "namespace": g.ns, "name": g.clusterName, "uid": getStr(g.cluster, "metadata.uid")},
			}, st,
			map[string]any{"labels": map[string]any{labRebalancer: "true", labSbCluster: g.clusterName, labSbManagedBy: "StorageCluster"}})
	}

	setPath(g.cluster, "status.phase", "Shrinking")
	g.st.Update(g.def(sbGroup, "storageclusters"), g.ns, g.cluster)
}

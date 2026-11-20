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
	ns      string   // sb-<tenant>  (primary tenant namespace = RBAC target)
	sysNs   string   // simplyblock-system (control plane)
	mcNs    string   // sb-mc-<managed>   (transport: projected intent + status)
	mcName  string   // managed cluster name
	tenant  string   // primary tenant (holds the generated cluster + DR)
	tenants []string // all tenants (extras are provisioned-but-empty)

	workerNames []string
	nodeUUIDs   []string
	poolNames   []string
	poolUUIDs   []string
	pvcRefs     []pvcRef // every generated PVC, for snapshots/backups/slots
	clusterUUID string
	clusterName string
	drCluster   string

	// the storage objects as created, for the ones that reference them
	cluster      map[string]any
	deployConfig map[string]any
	nodes        []genNode
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
	g.tenants = sc.Tenants
	if len(g.tenants) == 0 {
		g.tenants = []string{"tenant-a"}
	}
	g.tenant = g.tenants[0]
	g.ns = nsTenant(g.tenant)

	g.namespaces()
	g.k8sNodes()
	g.tenancyScaffold() // ManagedCluster, NodePoolAllocation, StorageClusterClass
	g.storageCluster()
	g.nodesAndDevices()
	g.poolsAndClasses()
	g.volumes()
	g.snapshots()
	g.backups()
	if sc.WithDR {
		g.replicationChain()
		g.drApplications()
		g.ramen()
	}
	g.workloads()
	g.platformPods()
	g.secrets()
	g.controlPlaneObjects()
	g.opsHistory()
	g.accessControl() // sb:* ClusterRoles, RoleBindings, AccessGrants
	g.events()
}

// ---- helpers ------------------------------------------------------------------

func (g *genCtx) def(group, resource string) ResourceDef {
	d, ok := g.st.LookupResource(group, resource)
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
	// status.observedGeneration is the one drift primitive every simplyblock
	// kind on main carries; nothing else is stamped onto a storage object.
	if group == sbGroup {
		g.stampParadigm(obj, resource)
	}
	created, err := g.st.Create(d, ns, obj)
	if err != nil {
		panic(fmt.Sprintf("generator conflict for %s/%s %s: %v", group, resource, name, err))
	}
	return created
}

// stampParadigm sets status.observedGeneration (= generation 1 at create) on a
// simplyblock object that has a status: observedGeneration == generation means
// that the reported status is current. The hub-only kinds of the tenancy
// scaffold (tenancy.go) additionally carry the managed-by label and a Ready
// condition, which no storage kind on main has.
func (g *genCtx) stampParadigm(obj map[string]any, resource string) {
	meta := obj["metadata"].(map[string]any)
	if isHubKind(resource) {
		stampActor(meta, "admin@simplyblock.io", g.uuid(), "sb-infra-admins,platform-eng", g.past(240))
		labels, _ := meta["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			meta["labels"] = labels
		}
		if _, ok := labels[labManagedBy]; !ok {
			labels[labManagedBy] = "hub"
		}
	}
	st, _ := obj["status"].(map[string]any)
	if st == nil {
		return
	}
	if _, ok := st["observedGeneration"]; !ok {
		st["observedGeneration"] = float64(1)
	}
	if _, has := st["conditions"]; !has && isHubKind(resource) {
		st["conditions"] = readyCondition("Ready", "True", "Reconciled", "resource reconciled", g.recent(30), 1)
	}
}

// isHubKind is the mock's hub-only scaffold: kinds with no CRD on main.
func isHubKind(resource string) bool {
	switch resource {
	case "managedclusters", "storageclusterclasses", "nodepoolallocations":
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

// future is a timestamp a few minutes ahead, for a claim lease or a deadline.
func (g *genCtx) future(minutes int) string {
	return g.now.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
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
	// application namespaces (Kubernetes workloads the recipe editor discovers)
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
	// Helm double-encodes: the Secret's data value is base64(gzip(JSON)), and
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
		node := g.nodes[g.rng.IntN(int(len(g.nodes)))]
		ts := g.recent(600)
		g.add("", "events", g.ns, fmt.Sprintf("%s.%s", node.name, randSuffixLocal(g.rng)),
			nil, nil, map[string]any{
				"type": ev.Type, "reason": ev.Reason,
				"message": ev.Message,
				"involvedObject": map[string]any{
					"kind": "StorageNode", "name": node.name,
					"namespace": g.ns, "apiVersion": sbGroup + "/v1alpha2",
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
		// aggregated role (rules filled by the controller from labeled children)
		g.add(rbacGroup, "clusterroles", "", r.Name, nil, nil, map[string]any{
			"aggregationRule": map[string]any{
				"clusterRoleSelectors": []any{map[string]any{"matchLabels": map[string]any{r.AggLabel: "true"}}},
			},
			"rules": []any{},
		})
		// rules-carrying child, labeled to aggregate into the role above
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

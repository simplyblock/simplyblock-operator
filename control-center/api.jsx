// ---------------------------------------------------------------------------
// API CLIENT — the only place that knows about control plane API v2 wire
// format. Everything below `normalize*` is the view model the UI renders,
// so re-pointing at a changed schema is a single-file edit.
// ---------------------------------------------------------------------------
// The console reads the Kubernetes API for CRDs and core objects, and the
// operator API for collections that have no CRD in v1alpha1 yet. It does not
// call the control plane REST API.
const API = window.SB_CONFIG.k8sBase;

// proposed: served by the operator until the kinds exist as CRDs
async function preq(path) {
  let res;
  try {
    res = await fetch(window.SB_CONFIG.operatorBase + "/proposed" + path,
      {headers: {Accept: "application/json", Authorization: `Bearer ${window.SB_CONFIG.token}`}});
  } catch (e) { throw new ApiError(0, "Cannot reach the operator API", path, "Unreachable"); }
  let body = null;
  try { body = await res.json(); } catch (e) {}
  if (!res.ok) throw new ApiError(res.status,
    (body && (body.message || body.error)) || "Request failed", path, (body && body.reason) || null);
  return body && body.results !== undefined ? body.results : body;
}
const scoped = (path, clusterId) => clusterId ? `${path}?cluster=${clusterId}` : path;

// ---- request router --------------------------------------------------------
// The endpoint table below speaks one path grammar:
//   /coll                     list
//   /coll/id                  one (returned as a one-element array)
//   /parent/id/child          child list scoped to that parent
//   /coll/id/verb   (POST)    an action
// This resolves each onto the surface that serves it: a CRD on the Kubernetes
// API, or an operator /proposed collection for kinds v1alpha1 does not model
// yet. Nothing here reaches the control plane REST API.

// collection -> CRD kind, for the five kinds that exist today
const CRD_COLL = {
  clusters: "StorageCluster", "storage-nodes": "StorageNode",
  devices: "StorageDevice", pools: "StoragePool", backups: "StorageBackup"
};
// how a child record points back at each kind of parent
const SCOPE_FIELD = {
  clusters: "cluster_id", pools: "pool_id", lvols: "lvol_id",
  "storage-nodes": "node_id", hosts: "host_id", devices: "device_id",
  "backup-policies": "policy_id", "replication-policies": "policy_id",
  tasks: "parent_id", pairs: "pair_id",
  "kubernetes-clusters": "k8s_cluster_id", "k8s-clusters": "k8s_cluster_id",
  zones: "zone_id", "storage-classes": "storage_class_id",
  "consistency-groups": "cg_id", "dr-clusters": "dr_cluster_id", snapshots: "snapshot_id"
};

// CRD reads carry their Kubernetes identity as __k8s so a view model can show
// activeOpsRef and conditions alongside the record.
const withK8s = o => Object.assign({}, (o.status && o.status.extras) || {}, {__k8s: {
  name: o.metadata.name, kind: o.kind, uid: o.metadata.uid,
  labels: o.metadata.labels || {}, generation: o.metadata.generation,
  observedGeneration: o.status.observedGeneration,
  phase: o.status.phase, conditions: o.status.conditions || [],
  activeOpsRef: o.status.activeOpsRef || null
}});
const crdList = kind => k8s.list(kind).then(r => r.map(withK8s));

async function req(path, opts) {
  const method = ((opts || {}).method || "GET").toUpperCase();
  const seg = path.split("?")[0].split("/").filter(Boolean);
  if (method !== "GET") return mutate(method, path, opts && opts.body ? JSON.parse(opts.body) : {});

  // /coll
  if (seg.length === 1) {
    return CRD_COLL[seg[0]] ? crdList(CRD_COLL[seg[0]]) : preq("/" + seg[0]);
  }
  // /coll/id  -> always an array, which is what the normalizers expect
  if (seg.length === 2) {
    const [coll, id] = seg;
    if (CRD_COLL[coll]) {
      const items = await crdList(CRD_COLL[coll]);
      const hit = items.find(x => x.uuid === id);
      if (!hit) throw new ApiError(404, `${coll}/${id} not found`, path, "NotFound");
      return [hit];
    }
    const r = await preq(`/${coll}/${id}`);
    return Array.isArray(r) ? r : [r];
  }
  // /parent/id/child
  if (seg.length === 3) {
    const [pColl, pid, child] = seg;
    const field = SCOPE_FIELD[pColl];
    if (CRD_COLL[child]) {
      const items = await crdList(CRD_COLL[child]);
      // a CRD child filtered by whichever parent asked for it
      return items.filter(x => x[field] === pid);
    }
    return preq(`/${child}?scope=${pColl}&scopeId=${pid}`);
  }
  return preq(path);
}
const send = (method, path, payload) => req(path, {method, body: JSON.stringify(payload || {})});

// ---- mutations -------------------------------------------------------------
// An action is not a verb against an entity: it creates an <Entity>Ops object
// whose spec.action names it. spec.abort is the only stop, so a cancel is a
// patch, never a DELETE.
const cap = s => s.charAt(0).toUpperCase() + s.slice(1).replace(/-(\w)/g, (_, c) => c.toUpperCase());
const OPS_VERBS = {
  clusters: {kind: "StorageCluster", verbs: {suspend: "Suspend", activate: "Activate",
    restart: "Activate", rebalance: "Rebalance", "storage-nodes": "Expand"}},
  "storage-nodes": {kind: "StorageNode", verbs: {restart: "Restart", shutdown: "Shutdown",
    migrate: "Migrate", devices: "AddDevice"}},
  devices: {kind: "StorageDevice", verbs: {restart: "Restart", fail: "Fail",
    "health-check": "HealthCheck", remove: "Remove"}},
  pools: {kind: "StoragePool", verbs: {enable: "Enable", disable: "Disable"}},
  lvols: {kind: "PersistentVolume", verbs: {resize: "Resize", snapshot: "Snapshot",
    clone: "Clone", migrate: "Migrate", backup: "Backup"}},
  backups: {kind: "StorageBackup", verbs: {merge: "Merge", restore: "Restore", export: "Export"}}
};
// DELETE on an entity is also an operation, not a raw delete
const DELETE_VERB = {"storage-nodes": ["StorageNode", "Remove"], devices: ["StorageDevice", "Remove"],
  backups: ["StorageBackup", "Delete"]};

// entities are addressed by uuid inside the console and by name on the API
async function k8sNameOf(kind, id) {
  const items = await k8s.list(kind);
  const hit = items.find(o => {
    const s = o.status || {};
    return [s.clusterId, s.nodeId, s.deviceId, s.poolId, s.backupId,
      s.simplyblock && s.simplyblock.volumeId, o.metadata.uid].includes(id);
  });
  if (!hit) throw new ApiError(404, `${kind} ${id} not found`, "/" + id, "NotFound");
  return hit.metadata.name;
}

async function mutate(method, path, payload) {
  const seg = path.split("?")[0].split("/").filter(Boolean);
  if (seg.length === 3 && (method === "POST" || method === "PUT")) {
    const m = OPS_VERBS[seg[0]];
    const action = m && m.verbs[seg[2]];
    if (action) return submitOps(m.kind, await k8sNameOf(m.kind, seg[1]), action, payload);
  }
  if (seg.length === 2 && method === "DELETE" && DELETE_VERB[seg[0]]) {
    const [kind, action] = DELETE_VERB[seg[0]];
    return submitOps(kind, await k8sNameOf(kind, seg[1]), action, null);
  }
  // Not an operation: a proposed collection the operator owns until it is a CRD.
  const res = await fetch(window.SB_CONFIG.operatorBase + "/proposed" + path, {
    method, headers: {"Content-Type": "application/json",
      Authorization: `Bearer ${window.SB_CONFIG.token}`},
    body: method === "DELETE" ? undefined : JSON.stringify(payload || {})
  });
  let b = null; try { b = await res.json(); } catch (e) {}
  if (!res.ok) throw new ApiError(res.status, (b && (b.message || b.error)) || "Request failed", path, (b && b.reason) || null);
  return b && b.results !== undefined ? b.results : b;
}

// ---- status vocabulary -----------------------------------------------------
const STATUS_META = {
  online: {c: "var(--ok)", rank: 0, label: "online"},
  active: {c: "var(--ok)", rank: 0, label: "active"},
  paired: {c: "var(--ok)", rank: 0, label: "paired"},
  pairing: {c: "var(--info)", rank: 1, label: "pairing", blink: true},
  healthy: {c: "var(--ok)", rank: 0, label: "healthy"},
  unhealthy: {c: "var(--bad)", rank: 4, label: "unhealthy"},
  available: {c: "var(--ok)", rank: 0, label: "available"},
  enabled: {c: "var(--ok)", rank: 0, label: "enabled"},
  disabled: {c: "var(--warn)", rank: 3, label: "disabled"},
  discovered: {c: "var(--dim2)", rank: 2, label: "discovered"},
  inspecting: {c: "var(--info)", rank: 1, label: "inspecting", blink: true},
  inspected: {c: "var(--accent)", rank: 3, label: "ready to configure"},
  degraded: {c: "var(--warn)", rank: 3, label: "degraded"},
  suspended: {c: "var(--bad)", rank: 4, label: "suspended"},
  unready: {c: "var(--idle)", rank: 2, label: "unready"},
  in_activation: {c: "var(--info)", rank: 1, label: "in activation", blink: true},
  offline: {c: "var(--bad)", rank: 4, label: "offline"},
  unreachable: {c: "var(--alert)", rank: 5, label: "unreachable"},
  down: {c: "var(--bad)", rank: 4, label: "down"},
  in_restart: {c: "var(--info)", rank: 1, label: "in restart", blink: true},
  in_removal: {c: "var(--alert)", rank: 2, label: "in removal", blink: true},
  in_shutdown: {c: "var(--warn)", rank: 3, label: "in shutdown", blink: true},
  in_creation: {c: "var(--info)", rank: 1, label: "in creation", blink: true},
  in_migration: {c: "var(--info)", rank: 1, label: "in migration", blink: true},
  // ReplicationSlot.status.state — a slot that is replicating is in its steady
  // state, so it reads green rather than as a transient.
  replicating: {c: "var(--ok)", rank: 0, label: "replicating"},
  converging: {c: "var(--info)", rank: 1, label: "converging", blink: true},
  cutover_pending: {c: "var(--warn)", rank: 5, label: "ready to cut over"},
  cutover_done: {c: "var(--info)", rank: 1, label: "cutover done"},
  attaching: {c: "var(--idle)", rank: 4, label: "attaching"},
  detaching: {c: "var(--idle)", rank: 4, label: "detaching"},
  failed_over: {c: "var(--ro)", rank: 6, label: "failed over"},
  error: {c: "var(--bad)", rank: 9, label: "error"},
  frozen: {c: "var(--alert)", rank: 3, label: "io frozen", blink: true},
  running: {c: "var(--info)", rank: 1, label: "running", blink: true},
  paused: {c: "var(--warn)", rank: 3, label: "paused"},
  completed: {c: "var(--ok)", rank: 0, label: "completed"},
  Bound: {c: "var(--ok)", rank: 0, label: "Bound"},
  Running: {c: "var(--info)", rank: 3, label: "running", blink: true},
  Succeeded: {c: "var(--ok)", rank: 0, label: "succeeded"},
  Pending: {c: "var(--warn)", rank: 3, label: "Pending"},
  Lost: {c: "var(--bad)", rank: 4, label: "Lost"},
  Draft: {c: "var(--dim2)", rank: 2, label: "draft — not approved"},
  Approved: {c: "var(--accent)", rank: 1, label: "approved"},
  Deploying: {c: "var(--info)", rank: 1, label: "deploying", blink: true},
  undiscovered: {c: "var(--dim2)", rank: 3, label: "not discovered"},
  Queued: {c: "var(--idle)", rank: 2, label: "queued"}, RestoringBackups: {c: "var(--info)", rank: 1, label: "restoring backups", blink: true}, PromotingVolumes: {c: "var(--info)", rank: 1, label: "promoting volumes", blink: true}, Replicating: {c: "var(--info)", rank: 1, label: "replicating", blink: true},
  Converged: {c: "var(--accent)", rank: 1, label: "converged"}, MovingWorkloads: {c: "var(--info)", rank: 1, label: "moving workloads", blink: true},
  MigratingVolumes: {c: "var(--info)", rank: 1, label: "migrating volumes", blink: true}, Cleanup: {c: "var(--info)", rank: 1, label: "cleanup", blink: true},
  Completed: {c: "var(--ok)", rank: 0, label: "completed"}, Paused: {c: "var(--warn)", rank: 3, label: "paused"},
  LiveMigrating: {c: "var(--info)", rank: 1, label: "live migrating", blink: true}, Restarting: {c: "var(--info)", rank: 1, label: "restarting", blink: true}, Moved: {c: "var(--ok)", rank: 0, label: "moved"},
  Configured: {c: "var(--ok)", rank: 0, label: "configured"}, Added: {c: "var(--ok)", rank: 0, label: "added"},
  Applying: {c: "var(--info)", rank: 1, label: "applying", blink: true}, Isolating: {c: "var(--info)", rank: 1, label: "isolating cores", blink: true},
  Rebooting: {c: "var(--warn)", rank: 1, label: "rebooting", blink: true}, Verifying: {c: "var(--info)", rank: 1, label: "verifying", blink: true},
  Scheduling: {c: "var(--info)", rank: 1, label: "scheduling pod", blink: true}, Joining: {c: "var(--info)", rank: 1, label: "joining cluster", blink: true},
  "Starting SPDK": {c: "var(--info)", rank: 1, label: "starting SPDK", blink: true},
  Failed: {c: "var(--bad)", rank: 4, label: "failed"},
  complete: {c: "var(--ok)", rank: 0, label: "complete"},
  Validated: {c: "var(--ok)", rank: 0, label: "Validated"},
  Validating: {c: "var(--info)", rank: 1, label: "Validating", blink: true},
  Deployed: {c: "var(--ok)", rank: 0, label: "Deployed"},
  FailedOver: {c: "var(--warn)", rank: 3, label: "Failed over"},
  FailingOver: {c: "var(--alert)", rank: 3, label: "Failing over", blink: true},
  Relocating: {c: "var(--info)", rank: 2, label: "Relocating", blink: true},
  Relocated: {c: "var(--ok)", rank: 0, label: "Relocated"},
  WaitForUser: {c: "var(--bad)", rank: 4, label: "Waiting for operator"},
  Fenced: {c: "var(--bad)", rank: 4, label: "Fenced"},
  ManuallyFenced: {c: "var(--bad)", rank: 4, label: "Manually fenced"},
  Unfenced: {c: "var(--ok)", rank: 0, label: "Unfenced"},
  failed: {c: "var(--bad)", rank: 4, label: "failed"},
  unavailable: {c: "var(--bad)", rank: 4, label: "unavailable"},
  new: {c: "var(--idle)", rank: 2, label: "new"},
  removed: {c: "var(--dim2)", rank: 3, label: "removed"},
  in_removal: {c: "var(--alert)", rank: 2, label: "in removal", blink: true},
  in_shutdown: {c: "var(--warn)", rank: 3, label: "in shutdown", blink: true},
  in_failure: {c: "var(--bad)", rank: 3, label: "failing", blink: true},
  read_only: {c: "var(--ro)", rank: 3, label: "read-only"},
  in_sync: {c: "var(--ok)", rank: 0, label: "in sync"},
  catching_up: {c: "var(--info)", rank: 1, label: "catching up", blink: true},
  paused: {c: "var(--warn)", rank: 3, label: "paused"},
  broken: {c: "var(--bad)", rank: 5, label: "broken"},
  good: {c: "var(--ok)", rank: 0, label: "healthy"},
  warn: {c: "var(--warn)", rank: 3, label: "warning"},
  critical: {c: "var(--bad)", rank: 4, label: "critical"}
};

// ---- normalizers: wire record -> view model --------------------------------
const REG = {};                       // uuid -> view model, warms breadcrumbs
const reg = vm => { REG[vm.id] = vm; return vm; };
const ioOf = s => ({r: (s || {}).read_io_ps || 0, w: (s || {}).write_io_ps || 0});
const bwOf = s => ({r: (s || {}).read_bytes_ps || 0, w: (s || {}).write_bytes_ps || 0});
const histOf = h => ({iops: (h || {}).iops || [], bw: (h || {}).bytes || []});

const normCluster = c => reg({
  kind: "cluster", id: c.uuid, name: c.name,
  mode: c.device_class === "nvme" ? "nvme" : "blockdev",
  siting: c.location_type === "edge" ? "edge" : "datacenter",
  status: c.status, rebalancing: !!c.rebalancing,
  capacity: {total: c.size_total, used: c.size_util},
  iops: ioOf(c.io_stats), bw: bwOf(c.io_stats), hist: histOf(c.io_history),
  counts: {hosts: c.hosts_count, hostsAvailable: c.hosts_available, nodes: c.storage_nodes_count,
    nodesOnline: c.storage_nodes_online, devices: c.devices_count, devicesOnline: c.devices_online,
    pools: c.pools_count, volumes: c.lvols_count, snapshots: c.snapshots_count,
    backups: c.backups_count, policies: c.backup_policies_count,
    cgroups: c.consistency_groups_count || 0, encrypted: c.encrypted_lvols_count || 0,
    migrations: c.migrations_count || 0, migrationTargets: c.migration_targets_count || 0,
    buckets: c.buckets_count || 0, rwxPvcs: c.rwx_pvcs_count || 0,
    k8sClusters: (c.k8s_cluster_ids || []).length, pvcs: c.pvcs_count || 0,
    reduced: c.reduced_lvols_count || 0,
    rpolicies: c.replication_policies_count || 0, pairsOut: c.pairs_out_count || 0,
    pairsIn: c.pairs_in_count || 0, replicated: c.replicated_lvols_count || 0,
    zones: (c.zone_ids || []).length},
  zoneIds: c.zone_ids || [], regions: c.regions || [], stretched: !!c.stretched, drEligible: !!c.dr_target_eligible,
  k8sClusterIds: c.k8s_cluster_ids || [],
  backupEnabled: !!c.backup_enabled, s3: c.s3 || null,
  syncReplication: !!c.sync_replication_enabled,
  kms: c.kms || null, logicalUsed: c.logical_used || 0,
  fileStorage: c.file_storage || {enabled: false},
  objectStorage: c.object_storage || {enabled: false},
  multipathing: !!c.multipathing_enabled, multipathNodes: c.multipath_nodes_count || 0,
  autoRebalance: {enabled: !!(c.auto_rebalance || {}).enabled,
    moved1h: (c.auto_rebalance || {}).moved_1h || 0, moved24h: (c.auto_rebalance || {}).moved_24h || 0},
  nodeAffinity: c.node_affinity || "none", podAffinity: !!c.pod_affinity_enabled,

  fd: {enabled: !!c.failure_domain_enabled, scope: c.failure_domain_scope || null,
    domains: c.failure_domains || [], unassigned: c.fd_unassigned || 0,
    min: c.fd_min || 0, max: c.fd_max || 0, balanced: c.fd_balanced !== false, thin: c.fd_thin || []},
  caps: c.capabilities || {},
  faultBudget: c.fault_budget || {kind: "nodes", tolerated: c.distr_npcs || 1, lost: (c.storage_nodes_count || 0) - (c.storage_nodes_online || 0)},
  haType: c.ha_type, distrNpcs: c.distr_npcs, distrNdcs: c.distr_ndcs, version: c.cluster_version, mgmt: c.mgmt_endpoint,
  mgmtKind: c.mgmt_endpoint_kind || "control plane API", createdAt: c.created_at
});
const normHost = h => reg({
  kind: "host", id: h.uuid, clusterId: h.cluster_id, hostname: h.hostname, mgmtIp: h.mgmt_ip,
  zoneId: h.zone_id || null, zone: h.zone || null, region: h.region || null,
  rack: h.rack_id || null, cabinet: h.cabinet_id || null, k8sCluster: h.k8s_cluster || null,
  migrationTaint: h.migration_taint || null,
  hostClass: h.host_class || null,
  status: h.status, source: h.source || "manual", sockets: h.numa_sockets, controlPlane: !!h.control_plane,
  kubelet: h.kubelet_version, roles: h.roles || [], k8sLabels: h.k8s_labels || {},
  inspection: h.inspection || null,
  vcpu: h.vcpu_count, memory: h.memory_total,
  memoryPerPod: h.memory_per_pod || null, socketsUsed: h.numa_sockets_used || null,
  mgmtNic: h.mgmt_nic || null, dataNics: h.data_nics || [],
  nics: (h.nics || []).map(n => ({name: n.name, mac: n.mac, speed: n.speed_gbps, address: n.address, socket: n.numa_socket, state: n.state})),
  hugepages: {reserved: h.hugepages_reserved, allocated: h.hugepages_allocated},
  devices: (h.devices || []).map(d => ({id: d.id, kind: d.kind, socket: d.numa_socket, pcie: d.pcie_address,
    blockdev: d.device_name, serial: d.serial_number, model: d.model_number, size: d.size,
    assignedNodeId: d.assigned_node_id, reserved: !!d.reserved, reservedFor: d.reserved_for_node_id})),
  nodeIds: h.storage_node_ids || [],
  counts: {devices: (h.devices || []).length, assigned: h.devices_assigned, free: h.devices_free,
    nvme: h.nvme_count, blockFree: h.block_free_count, nodes: (h.storage_node_ids || []).length},
  capacity: {total: h.size_total, used: h.size_assigned},
  preparedAt: h.prepared_at, labels: h.labels || {}
});
// A discovery run: the inventory pass, and the filter it ran with. The filter
// belongs here rather than in the wizard because it decides what was even
// reported — a boot device excluded at discovery never reaches a node set.
const normDiscovery = d => ({
  kind: "discovery", id: d.uuid, k8sClusterId: d.k8s_cluster_id,
  status: d.status, step: d.step || null, opName: d.op_name || null,
  startedAt: d.started_at, finishedAt: d.finished_at,
  nodeSelector: (d.node_selector || {}).matchLabels || {},
  filter: d.device_filter || {},
  hostIds: d.host_ids || [],
  counts: {nodes: d.node_count, devices: d.device_count, filtered: d.filtered_count}
});

// ClusterDeploymentConfig — the document that is reviewed and approved. One
// group per NUMA socket, because one storage node runs per socket.
const normDeployConfig = o => {
  const sp = o.spec || {}, st = o.status || {};
  const set = (sp.nodeSets || [])[0] || {};
  const groups = (set.groups || []).map(g => ({
    node: g.node, socket: g.numaSocket,
    nvme: (g.devices || {}).nvme || [], block: (g.devices || {}).block || [],
    mgmtNic: g.mgmtInterface, dataNics: g.dataInterfaces || [],
    maxSubsystems: (g.sizing || {}).maxSubsystemCount,
    vcpu: (g.sizing || {}).vcpuCount,
    hugepages: (g.sizing || {}).minHugePagesSize,
    systemMemory: (g.sizing || {}).systemMemory,
    coreIsolation: !!g.coreIsolation, failureDomain: g.failureDomain
  }));
  const steps = (st.steps || []).map(s => ({
    name: s.name, label: s.label, phase: s.phase, message: s.message,
    startedAt: s.startedAt, finishedAt: s.finishedAt, progress: s.progress
  }));
  const cl = sp.cluster || {};
  return reg({
    kind: "deployconfig", id: o.metadata.uid, name: o.metadata.name,
    createdAt: o.metadata.creationTimestamp,
    approved: !!sp.approved, status: st.phase || "Draft", message: st.message || null,
    environment: sp.environment || null,
    k8sClusterId: st.kubernetesClusterId || null, k8sClusterName: sp.kubernetesClusterRef || null,
    clusterId: st.clusterId || null, discoveryId: st.discoveryRef || null,
    hostIds: st.hostIds || [],
    nodeSelector: (sp.nodeSelector || {}).matchLabels || {},
    filter: cl.deviceFilter || sp.deviceFilter || {},
    cluster: sp.cluster || null, hosts: set.nodes || [], groups, steps,
    nodes: (st.nodes || []).map(n => ({host: n.host, hostId: n.hostId, phase: n.phase, message: n.message, progress: n.progress || 0, storageNodeIds: n.storageNodeIds || []})),
    log: (st.log || []).map(l => ({ts: l.ts, level: l.level, step: l.step, node: l.node, msg: l.msg})),
    counts: {hosts: (set.nodes || []).length, nodes: groups.length,
      devices: groups.reduce((n, g) => n + g.nvme.length + g.block.length, 0)},
    sizing: {hugepages: groups.reduce((n, g) => n + (g.hugepages || 0), 0),
      vcpu: groups.reduce((n, g) => n + (g.vcpu || 0), 0),
      coreIsolation: groups.some(g => g.coreIsolation),
      maxSubsystems: groups.length ? groups[0].maxSubsystems : null}
  });
};
const normNode = n => reg({
  kind: "node", id: n.uuid, clusterId: n.cluster_id, hostId: n.host_id, zoneId: n.zone_id || null, hostname: n.hostname,
  ip: (n.data_nics && n.data_nics[0] ? n.data_nics[0].ip : null), port: (n.data_nics && n.data_nics[0] ? n.data_nics[0].port : 4420),
  dataNics: (n.data_nics || []).map(x => ({name: x.name, ip: x.ip, port: x.port, socket: x.numa_socket, state: x.state})),
  multipath: (n.data_nics || []).length > 1,
  op: n.op ? {kind: n.op.kind, phase: n.op.phase, phaseIndex: n.op.phase_index || 0, phases: n.op.phases || [],
    volumesMoved: n.op.volumes_moved || 0, targetHostname: n.op.target_hostname || null, sourceHostname: n.op.source_hostname || null,
    startedAt: n.op.started_at, taskId: n.op.task_id} : null,
  mgmtIp: n.mgmt_ip, failureDomain: n.failure_domain, physicalLabel: n.physical_label, status: n.status,
  capacity: {total: n.size_total, used: n.size_util},
  iops: ioOf(n.io_stats), bw: bwOf(n.io_stats), hist: histOf(n.io_history),
  counts: {devices: n.devices_count, devicesOnline: n.devices_online},
  cpuCount: n.cpu_count, cpuReserved: n.vcpu_reserved, maxSubsystems: n.max_subsystem_count || null,
  memory: {total: n.memory_total, reserved: n.memory_reserved, used: n.memory_used},
  hugepages: {total: n.hugepages_total, used: n.hugepages_used},
  spdk: n.spdk_version
});
const normDevice = d => reg({
  kind: "device", id: d.uuid, nodeId: d.node_id, clusterId: d.cluster_id, hostId: d.host_id,
  mode: d.cluster_device_class === "nvme" ? "nvme" : "blockdev", socket: d.numa_socket,
  serial: d.serial_number, pcie: d.pcie_address, blockdev: d.device_name,
  model: d.model_number, firmware: d.firmware_revision,
  status: d.status, health: d.health_check, lastHealthCheck: d.last_health_check,
  capacity: {total: d.size_total, used: d.size_util},
  iops: ioOf(d.io_stats), bw: bwOf(d.io_stats), hist: histOf(d.io_history),
  temp: d.temperature_c, wear: d.percentage_used, poweronHours: d.power_on_hours
});
const normPool = p => reg({
  kind: "pool", id: p.uuid, clusterId: p.cluster_id, name: p.pool_name, dhchap: !!p.dhchap_bidirectional,
  // a pool is never offline: it is enabled or disabled. Disabled keeps serving I/O
  // but refuses new volume provisioning.
  status: p.enabled === false ? "disabled" : "enabled", enabled: p.enabled !== false, qos: p.qos,
  capacity: {total: p.size_prov, used: p.size_util},
  lvolBytes: p.lvols_bytes || 0, snapshotBytes: p.snapshots_bytes || 0,
  storageClasses: p.storage_classes || [], k8sClusterId: p.k8s_cluster_id || null,
  counts: {volumes: p.lvols_count, volumesOnline: p.lvols_online, snapshots: p.snapshots_count,
    backups: p.backups_count, storageClasses: p.storage_classes_count || 0}
});
const normVolume = v => reg({
  kind: "volume", id: v.uuid, poolId: v.pool_id, poolName: v.pool_name, clusterId: v.cluster_id,
  name: v.lvol_name, status: v.status, nodes: v.nodes || {},
  capacity: {total: v.size_prov, used: v.size_util},
  iops: ioOf(v.io_stats), bw: bwOf(v.io_stats), hist: histOf(v.io_history),
  crypto: !!v.crypto_enabled, qos: v.qos, nqn: v.nqn,
  dataReduction: !!v.compression_dedup_enabled,
  logicalUsed: v.logical_used || v.size_util,
  baseSnapshot: v.base_snapshot || null,
  // a volume can belong to several consistency groups at once
  consistencyGroups: v.consistency_groups || [],
  pvc: v.pvc || null, bucket: v.bucket || null,
  affinity: v.affinity || null,
  backupPolicy: v.backup_policy || null,
  replication: v.replication ? {
    policyId: v.replication.policy_id, policyName: v.replication.policy_name, mode: v.replication.mode,
    status: v.replication.status, lastAt: v.replication.last_replication_at,
    backlog: v.replication.backlog_bytes, consistencyGroup: v.replication.consistency_group,
    generations: v.replication.generations
  } : null,
  migration: v.migration || null,
  counts: {snapshots: v.snapshots_count || 0, backups: v.backups_count || 0,
    snapshotsBackedUp: v.snapshots_backed_up || 0, backupVersions: v.backup_versions_count || 0},
  backupChainId: v.backup_chain_id || null,
  createdAt: v.created_at
});
const normSnapshot = s => reg({
  kind: "snapshot", id: s.uuid, clusterId: s.cluster_id, poolId: s.pool_id, poolName: s.pool_name,
  volumeId: s.lvol_id, volumeName: s.lvol_name, name: s.snapshot_name, status: s.status || "online",
  seq: s.seq, parentId: s.parent_id || null, backupVersionId: s.backup_version_id || null,
  createdAt: s.created_at, capacity: {total: s.size, used: s.size}
});
const normBackup = b => reg({
  kind: "backup", id: b.uuid, clusterId: b.cluster_id, poolId: b.pool_id, poolName: b.pool_name,
  volumeId: b.lvol_id, volumeName: b.lvol_name, name: b.lvol_name, chainId: b.chain_id,
  policyId: b.policy_id || null, policyName: b.policy_name || null,
  status: b.status || "online", bucket: b.bucket,
  exportedTo: b.exported_to || null, exportedVersion: b.exported_version || null,
  createdAt: b.earliest_at || b.created_at, latestAt: b.latest_at, lastMergeAt: b.last_merge_at || null,
  versions: (b.versions || []).map(v => ({
    id: v.id, seq: v.seq, tier: v.tier, type: v.type, createdAt: v.created_at, size: v.size,
    snapshotId: v.source_snapshot_id, snapshotName: v.source_snapshot_name, merged: v.merged_count || 0
  })),
  counts: {versions: b.versions_count || 0, merged: b.merged_total || 0},
  fullBytes: b.full_bytes || 0, deltaBytes: b.delta_bytes || 0,
  capacity: {total: b.size || 0, used: b.size || 0}
});
const normPolicy = p => reg({
  kind: "policy", id: p.uuid, clusterId: p.cluster_id, name: p.policy_name, status: "active", consistencyGroup: !!p.consistency_group,
  schedule: (p.schedule || []).map(r => ({interval: r.interval, versions: r.versions, online: r.online || 0})),
  counts: {volumes: p.lvols_count || 0, chains: p.chains_count || 0,
    versions: p.versions_total || 0, online: p.online_snapshots || 0},
  finest: p.finest_interval || null,
  createdAt: p.created_at, capacity: {total: 0, used: 0}
});
// ReplicationPair — reusable {sourceCluster, targetCluster}. targetCluster is
// immutable after creation, so a pair is never edited, only replaced.
const normReplPair = (o, pols, slots) => {
  const s = o.spec || {}, st = o.status || {};
  const mine = (pols || []).filter(p => (p.spec || {}).pairRef === o.metadata.name);
  const names = mine.map(p => p.metadata.name);
  const mySlots = (slots || []).filter(x => names.includes((x.spec || {}).policyRef));
  return reg({
    kind: "pair", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationPair", shortName: "relpair",
    sourceCluster: s.sourceCluster, targetCluster: s.targetCluster,
    ready: !!st.ready, backendTargetId: st.backendTargetID || null,
    message: st.message || "", activeOpsRef: st.activeOpsRef || null,
    conditions: st.conditions || [],
    status: st.ready ? "online" : "degraded",
    counts: {policies: mine.length, slots: mySlots.length,
      failedOver: mySlots.filter(x => (x.status || {}).state === "failed_over").length,
      errored: mySlots.filter(x => (x.status || {}).state === "error").length},
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationPolicy — one interval, one snapshot count. mode is exactly
// failover | migration; there is no synchronous mode and no tiered retention.
const normReplPolicy = (o, slots, pairs) => {
  const s = o.spec || {}, st = o.status || {};
  const mine = (slots || []).filter(x => (x.spec || {}).policyRef === o.metadata.name)
    .map(x => x.status || {});
  const pair = (pairs || []).find(p => p.metadata.name === s.pairRef);
  // the fixtures' clock, not the wall clock — otherwise every slot reads stale
  const nowMs = window.SB_NOW || Date.now();
  const late = mine.filter(x => {
    if (x.state !== "replicating" || !x.lastReplicatedAt) return false;
    return (nowMs - Date.parse(x.lastReplicatedAt)) / 60000 > ivMinutes(s.interval) * 2;
  }).length;
  return reg({
    kind: "rpolicy", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationPolicy", shortName: "repl",
    pairRef: s.pairRef, pairId: pair ? pair.metadata.uid : null,
    sourceCluster: pair ? (pair.spec || {}).sourceCluster : null,
    targetCluster: pair ? (pair.spec || {}).targetCluster : null,
    mode: s.mode || "failover", interval: s.interval || "5m",
    snapshotRetention: s.snapshotRetention == null ? 3 : s.snapshotRetention,
    ready: !!st.ready, backendPolicyId: st.backendPolicyID || null,
    activeOpsRef: st.activeOpsRef || null, conditions: st.conditions || [],
    message: (st.conditions || []).map(c => c.message).filter(Boolean)[0] || "",
    counts: {slots: st.slotCount != null ? st.slotCount : mine.length,
      replicating: mine.filter(x => x.state === "replicating").length,
      failedOver: mine.filter(x => x.state === "failed_over").length,
      errored: mine.filter(x => x.state === "error").length,
      cutoverPending: mine.filter(x => x.state === "cutover_pending").length,
      late},
    lastAt: mine.map(x => x.lastReplicatedAt).filter(Boolean).sort().slice(-1)[0] || null,
    status: !st.ready ? "degraded"
      : mine.some(x => x.state === "error") ? "unhealthy"
      : late ? "degraded" : "online",
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationSlot — one per PVC, created by the operator from the annotation and
// owned by the PVC. Never created or deleted from here.
const normReplSlot = o => {
  const s = o.spec || {}, st = o.status || {};
  const own = (o.metadata.ownerReferences || [])[0] || {};
  return reg({
    kind: "slot", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationSlot", shortName: "relslot",
    policyRef: s.policyRef, pvcRef: s.pvcRef, volumeId: s.volumeID,
    // <clusterUUID>:<poolUUID>:<volumeUUID>
    volumeParts: String(s.volumeID || "").split(":"),
    state: st.state || "replicating", status: st.state || "replicating",
    direction: st.direction || "source",
    sourceLvolId: st.sourceLvolID || null, targetLvolId: st.targetLvolID || null,
    targetNqn: st.targetNQN || null, lastAt: st.lastReplicatedAt || null,
    message: st.message || "", conditions: st.conditions || [],
    ownedBy: own.kind ? `${own.kind}/${own.name}` : null,
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationOps — one-shot. A terminal op is never re-run; a correction needs
// a new one.
const normReplOps = o => {
  const s = o.spec || {}, st = o.status || {};
  return reg({
    kind: "replops", id: o.metadata.uid, name: o.metadata.name,
    crdKind: "ReplicationOps", shortName: "replops",
    action: s.action, scope: s.scope, ref: s.ref,
    sourceClusterId: s.sourceClusterID || null, deleteSource: !!s.deleteSource,
    phase: st.phase || "Pending", subphase: st.subphase || null,
    message: st.message || "", startedAt: st.startedAt || null, completedAt: st.completedAt || null,
    results: (st.results || []).map(r => ({slotRef: r.slotRef, status: r.status,
      detail: r.detail || null, targetLvolId: r.targetLvolID || null})),
    terminal: ["Succeeded", "Failed"].includes(st.phase),
    status: st.phase === "Succeeded" ? "online" : st.phase === "Failed" ? "unhealthy"
      : st.phase === "Running" ? "activating" : "idle",
    capacity: {total: 0, used: 0}, createdAt: o.metadata.creationTimestamp
  });
};
const ivMinutes = iv => {
  const m = /^(\d+(?:\.\d+)?)\s*([smhdw]?)$/.exec(String(iv || "").trim());
  if (!m) return 5;
  const n = parseFloat(m[1]), u = m[2] || "m";
  return Math.max(1, Math.round(u === "s" ? n / 60 : u === "h" ? n * 60 : u === "d" ? n * 1440 : u === "w" ? n * 10080 : n));
};

const normPair = p => reg({
  kind: "pair", id: p.uuid, sourceClusterId: p.source_cluster_id, targetClusterId: p.target_cluster_id,
  status: p.state, link: p.link || {},
  lastHandshakeAt: p.last_handshake_at, createdAt: p.created_at,
  counts: {policies: p.policies_count || 0}, capacity: {total: 0, used: 0},
  name: null
});
const normDrPolicy = p => reg({
  kind: "rpolicy", id: p.uuid, name: p.name, mode: p.mode, pairId: p.pair_id,
  sourceClusterId: p.source_cluster_id, targetClusterId: p.target_cluster_id, zoneIds: p.zone_ids || null,
  frequency: p.frequency_minutes, retention: p.retention || [],
  // An asynchronous DR policy names a consistency group and carries no schedule
  // of its own: the frequency and retention below are the group's.
  cgId: p.cg_id || null, cgName: p.cg_name || null,
  consistencyGroup: !!p.cg_id, failback: p.failback || {},
  status: p.state, lastAt: p.last_replication_at, backlog: p.backlog_bytes,
  generations: p.generations_kept, createdAt: p.created_at,
  lastFailoverAt: p.last_failover_at, failoverMode: p.failover_mode || null, lastTestAt: p.last_test_at,
  drClusterIds: p.dr_cluster_ids || [], replicationClass: p.replication_class || null,
  appDrStatus: p.app_dr_status || "Unavailable",
  counts: {volumes: p.lvols_count !== undefined ? p.lvols_count : (p.lvol_ids || []).length,
    apps: p.apps_count || 0, pvcs: p.pvcs_count || 0, unhealthy: p.unhealthy_apps || 0},
  capacity: {total: 0, used: 0}
});
const normCg = g => reg({
  kind: "cgroup", id: g.uuid, clusterId: g.cluster_id, name: g.name,
  status: g.status || "online", memberIds: g.lvol_ids || [],
  capacity: {total: g.size_prov || 0, used: g.size_util || 0},
  counts: {volumes: g.lvols_count || 0, snapshots: g.snapshots_count || 0, backedUp: g.backed_up_count || 0,
    apps: g.apps_count || 0},
  // A group can own its protection: attaching a policy here applies it to every
  // member, and members added later inherit it.
  // Once a group carries protection its membership is fixed: the retained
  // versions and the replica stream are defined against exactly this set.
  locked: !!g.locked,
  backupPolicy: g.backup_policy || null,
  // The group owns the replication cadence; DR policies just name the group.
  replicationConfig: g.replication_config
    ? {frequency: g.replication_config.frequency_minutes, retention: g.replication_config.retention || []} : null,
  drPolicyIds: g.dr_policy_ids || [],
  replication: g.replication_config ? {status: g.replication_status || "healthy",
    lastAt: g.replication_last_at, backlog: g.replication_backlog_bytes || 0} : null,
  createdAt: g.created_at
});
const normCgSnap = s => reg({
  kind: "cgsnapshot", id: s.uuid, clusterId: s.cluster_id, cgId: s.cg_id, cgName: s.cg_name,
  name: s.snapshot_name, status: s.status || "online", createdAt: s.created_at,
  members: (s.members || []).map(m => ({volumeId: m.lvol_id, volumeName: m.lvol_name, snapshotId: m.snapshot_id, size: m.size})),
  backupVersionId: s.backup_version_id || null, bucket: s.backup_bucket || null,
  capacity: {total: s.size || 0, used: s.size || 0},
  counts: {volumes: (s.members || []).length}
});
const normMigration = m => reg({
  kind: "migration", id: m.uuid, clusterId: m.cluster_id, name: m.name,
  mode: m.mode, scope: m.scope, status: m.state,
  sourceClusterId: m.source_cluster_id, targetClusterId: m.target_cluster_id, targetZoneId: m.target_zone_id,
  targetTaint: m.target_taint, followWorkload: !!m.follow_workload,
  memberIds: m.lvol_ids || [],
  counts: {volumes: m.lvols_count || 0, moved: m.moved_count || 0},
  progress: m.progress_pct || 0, readyToCutover: !!m.ready_to_cutover,
  iterations: m.iterations || 0, iterationLimit: m.iteration_limit || 0,
  firstSnapshot: m.first_snapshot_bytes || 0, lastSnapshot: m.last_snapshot_bytes || 0,
  freezeThreshold: m.freeze_threshold_bytes || 0, freezeMs: m.estimated_freeze_ms || 0,
  throughput: m.throughput_bytes_ps || 0,
  startedAt: m.started_at, completedAt: m.completed_at, frozenAt: m.frozen_at, error: m.error || null,
  capacity: {total: 0, used: 0}, createdAt: m.started_at
});
const normK8s = k => reg({
  kind: "k8sc", id: k.uuid, name: k.name, version: k.version, status: k.status,
  discovered: !!k.discovered, discoveredAt: k.discovered_at || null,
  endpoint: k.api_endpoint, environment: k.environment,
  csi: {version: k.csi_version, status: k.csi_status}, operatorNamespace: k.operator_namespace || null,
  zoneIds: k.zone_ids || [], storageClusterIds: k.storage_cluster_ids || [], namespaces: k.namespaces || [],
  counts: {storageClasses: k.storage_classes_count || 0, pvcs: k.pvcs_count || 0,
    bound: k.pvcs_bound || 0, workers: k.worker_nodes_count || 0, prepared: k.prepared_hosts_count || 0,
    zones: (k.zone_ids || []).length, storageClusters: (k.storage_cluster_ids || []).length,
    protectedApps: k.protected_apps_count || 0},
  drClusterId: k.dr_cluster_id || null,
  capacity: {total: k.provisioned_bytes || 0, used: k.provisioned_bytes || 0},
  createdAt: k.created_at
});
const normSc = s => reg({
  kind: "storageclass", id: s.uuid, k8sClusterId: s.k8s_cluster_id, name: s.name,
  provisioner: s.provisioner, clusterId: s.cluster_id, poolId: s.pool_id, poolName: s.pool_name,
  operatorNamespace: s.operator_namespace || null, storagePoolRef: s.storage_pool_ref || null,
  variant: s.variant || "default",
  parameters: s.parameters || {}, specParameters: s.spec_parameters || {},
  zoneClusterMap: s.zone_cluster_map || {}, regionClusterMap: s.region_cluster_map || {},
  dhchap: !!s.dhchap, allowedTopology: s.allowed_topology || null,
  reclaim: s.reclaim_policy, binding: s.volume_binding_mode,
  expansion: !!s.allow_volume_expansion, isDefault: !!s.is_default, status: "active",
  counts: {pvcs: s.pvcs_count || 0, bound: s.bound_count || 0},
  capacity: {total: s.provisioned_bytes || 0, used: s.provisioned_bytes || 0},
  createdAt: s.created_at
});
const normBucket = b => reg({
  kind: "bucket", id: b.uuid, clusterId: b.cluster_id, name: b.name,
  volumeId: b.lvol_id, volumeName: b.lvol_name, poolId: b.pool_id, poolName: b.pool_name,
  status: b.status || "online",
  versioning: !!b.versioning, objectLock: !!b.object_lock, encrypted: !!b.encrypted,
  quota: b.quota_bytes || 0, objects: b.objects || 0,
  access: b.access || {}, replication: b.replication || null,
  // S3 metadata — what a client sets, what the console searches by
  region: b.region || null, storageClass: b.storage_class || "standard", owner: b.owner || null,
  tags: b.tags || {}, lifecycle: b.lifecycle_rules || [], cors: !!b.cors_enabled,
  counts: {snapshots: b.snapshots_count || 0, backups: b.backups_count || 0, tags: Object.keys(b.tags || {}).length},
  capacity: {total: b.provisioned_bytes || 0, used: b.size_bytes || 0},
  createdAt: b.created_at
});
const normMPath = p => reg({
  kind: "mpath", id: p.uuid, name: p.name, status: p.status, k8sClusterId: p.k8s_cluster_id,
  sourceClusterId: p.source_cluster_id, targetClusterId: p.target_cluster_id,
  sourceZoneId: p.source_zone_id || null, targetZoneId: p.target_zone_id || null,
  queue: p.queue || [], log: (p.log || []).map(l => ({ts: l.ts, level: l.level, groupId: l.group_id, msg: l.msg})),
  createdAt: p.created_at, capacity: {total: 0, used: 0}, counts: {groups: (p.queue || []).length}
});
const normAppGroup = g => reg({
  kind: "appgroup", id: g.uuid, pathId: g.path_id, name: g.name, namespace: g.namespace,
  status: g.phase, phase: g.phase, message: g.message || null, approval: g.approval || "manual", order: g.order || 0,
  members: (g.members || []).map(m => ({kind: m.kind, name: m.name, state: m.state || "Pending", progress: m.progress || 0})),
  pvcIds: g.pvc_ids || [], memberIds: g.lvol_ids || [], rpolicyId: g.rpolicy_id || null,
  volumes: (g.volumes || []).map(v => ({id: v.lvol_id, name: v.lvol_name, size: v.size || 0, backlog: v.backlog_bytes || 0, lastAt: v.last_replication_at, progress: v.progress || 0, migrated: !!v.migrated})),
  backlog: (g.volumes || []).reduce((n, v) => n + (v.backlog_bytes || 0), 0),
  counts: {members: (g.members || []).length, vms: (g.members || []).filter(m => m.kind === "VirtualMachine").length, volumes: (g.lvol_ids || []).length,
    moved: (g.members || []).filter(m => m.state === "Moved").length, migrated: (g.volumes || []).filter(v => v.migrated).length},
  capacity: {total: (g.volumes || []).reduce((n, v) => n + (v.size || 0), 0), used: 0},
  createdAt: g.created_at, startedAt: g.started_at, finishedAt: g.finished_at
});
const normPvc = p => reg({
  kind: "pvc", id: p.uuid, k8sClusterId: p.k8s_cluster_id,
  namespace: p.namespace, name: p.pvc_name,
  storageClassId: p.storage_class_id, storageClass: p.storage_class,
  volumeId: p.lvol_id || null,
  status: p.status, accessMode: p.access_mode, volumeMode: p.volume_mode, filesystem: p.filesystem || null,
  workload: p.workload, workloadKind: p.workload_kind,
  annotations: p.annotations || {}, labels: p.labels || {},
  capacity: {total: p.actual_bytes || p.requested_bytes || 0, used: p.requested_bytes || 0},
  requested: p.requested_bytes || 0, createdAt: p.created_at
});
const normDrCluster = d => reg({
  kind: "drcluster", id: d.uuid, k8sClusterId: d.k8s_cluster_id, name: d.name, region: d.region,
  status: d.status, fencing: d.fencing_state, ramen: d.ramen_version,
  s3: {profile: d.s3_profile_name, endpoint: d.s3_endpoint, bucket: d.s3_bucket},
  lastHeartbeatAt: d.last_heartbeat_at, createdAt: d.created_at,
  counts: {apps: d.apps_count || 0, standby: d.standby_apps_count || 0, policies: d.policies_count || 0},
  capacity: {total: 0, used: 0}
});
// A protection plan: the sites it spans, the storage profile, and the methods
// it declares. Everything below it — DRCluster, DRPolicy, DRPC, the class
// matrix — is derived, never authored.
const CLS_NAME = m => m.type === "sync" ? "sb-sync-" + m.name
  : m.type === "async" ? "sb-async-" + m.interval : "sb-vault-" + m.interval;
const normMethod = (m, plan) => ({
  name: m.name, type: m.type, target: m.target,
  interval: m.interval, classInterval: m.class_interval,
  // one top-layer field written to two places: DRPolicy.spec.schedulingInterval
  // and the class's parameters.schedulingInterval. If they differ by even
  // formatting, no class resolves, the policy still validates cleanly, and the
  // application is protected by nothing.
  intervalConsistent: m.type === "sync" || m.class_interval === m.interval,
  retention: m.retention || null, immutable: !!m.immutable, bucket: m.bucket || null,
  cls: (plan.classes || []).find(c => c.name === CLS_NAME(m)) || null,
  rpo: m.type === "sync" ? "0s" : m.interval,
  failback: m.type !== "snapshot-s3",
  generationSelect: m.type === "snapshot-s3"
});
const normPlan = p => reg({
  kind: "plan", id: p.uuid, name: p.name, storageProfile: p.storage_profile,
  siteNames: p.site_names || [], siteIds: p.site_ids || [],
  methods: (p.methods || []).map(m => normMethod(m, p)),
  classes: (p.classes || []).map(c => ({
    name: c.name, crdKind: c.kind, replicationId: c.replication_id, storageId: c.storage_id,
    provisioner: c.provisioner, labels: c.labels || {}, parameters: c.parameters || {}
  })),
  policies: (p.policies || []).map(x => ({
    name: x.name, drClusters: x.dr_clusters, schedulingInterval: x.scheduling_interval,
    selector: x.replication_class_selector || {}, methodName: x.method_name,
    peerClass: x.peer_class || null, validated: x.validated
  })),
  status: p.health || "healthy",
  protectionGap: !!p.protection_gap,
  worstLagSeconds: p.worst_lag_seconds || 0,
  counts: {apps: p.apps_count || 0, pvcs: p.pvcs_count || 0, methods: p.methods_count || 0,
    sites: p.sites_count || 0, policies: p.policies_count || 0,
    unvalidated: p.unvalidated_count || 0, generations: p.generations_total || 0},
  capacity: {total: 0, used: 0}, createdAt: p.created_at
});
// A site is one managed cluster. Its name must equal the OCM ManagedCluster
// name, and its region is how sync versus async is declared: equal region means
// the pair can mirror synchronously, distinct region cannot.
const normSite = s => reg({
  kind: "site", id: s.uuid, name: s.name, k8sClusterId: s.k8s_cluster_id,
  region: s.region, s3Profile: s.s3_profile_name, s3Endpoint: s.s3_endpoint, s3Bucket: s.s3_bucket,
  cidrs: s.cidrs || [], fencing: s.fencing_state, status: s.status,
  ramen: s.ramen_version, discoveredClasses: s.discovered_classes || [],
  counts: {activeApps: s.active_apps_count || 0, standbyApps: s.standby_apps_count || 0,
    plans: s.plans_count || 0, classes: (s.discovered_classes || []).length},
  capacity: {total: 0, used: 0}, createdAt: s.onboarded_at
});
const normGeneration = g => ({
  generation: g.generation, tier: g.tier, at: g.at, size: g.size_bytes,
  integrity: g.integrity_state, lockedUntil: g.locked_until || null,
  consistencyGroup: g.consistency_group || null, kind: g.kind, verifiedAt: g.verified_at || null
});
const normLeg = l => ({
  id: l.leg_id, method: l.method, type: l.type, target: l.target,
  state: l.state, lastAt: l.last_sync_at, lagSeconds: l.lag_seconds,
  epoch: l.epoch, bytesLastCycle: l.bytes_last_cycle,
  status: l.health, orchestrated: !!l.orchestrated, note: l.note || null,
  generations: l.generations || 0,
  oldestAt: l.oldest_generation_at || null, newestAt: l.newest_generation_at || null,
  lockedUntil: l.locked_until || null
});
const normArbitration = t2 => ({
  holder: t2.token_holder, generation: t2.generation,
  quorumAck: (t2.quorum_ack || []).map(q => ({site: q.site, ackedAt: q.acked_at})),
  fencedSites: t2.fenced_sites || [], quorumSize: t2.quorum_size, updatedAt: t2.updated_at
});

const normApp = a2 => reg({
  kind: "protectedapp", id: a2.uuid, name: a2.app_name, namespace: a2.namespace, appKind: a2.app_kind,
  policyId: a2.policy_id, policyName: a2.policy_name,
  preferredClusterId: a2.preferred_cluster_id, failoverClusterId: a2.failover_cluster_id,
  selector: a2.pvc_selector || {}, phase: a2.phase, progression: a2.progression, action: a2.action,
  status: a2.health || "healthy", rpoMet: a2.rpo_met !== false, vrgState: a2.vrg_state,
  lastSyncAt: a2.last_group_sync_at, lastSyncDuration: a2.last_group_sync_duration_s,
  lastSyncBytes: a2.last_group_sync_bytes,
  kubeObjectProtection: !!a2.kube_object_protection,
  recipe: a2.recipe || null,
  planId: a2.plan_id || null, planName: a2.plan_name || null, storageProfile: a2.storage_profile || null,
  preferredSite: a2.preferred_site || null, activeSite: a2.active_site || null,
  preferredSiteId: a2.preferred_site_id || null, activeSiteId: a2.active_site_id || null,
  // Ramen drives exactly one method: a DRPC selects PVCs by label, so two over
  // the same PVCs would both claim them. The other methods run with identical
  // parameters in the data plane but report their lag out of band.
  orchestratedMethod: a2.orchestrated_method || null,
  legs: (a2.legs || []).map(normLeg),
  generations: (a2.generations || []).map(normGeneration),
  vaultMethod: a2.vault_method || null,
  pinnedGeneration: a2.pinned_generation != null ? a2.pinned_generation : null,
  restoredFromGeneration: a2.restored_from_generation != null ? a2.restored_from_generation : null,
  restoreTarget: a2.restore_target || null,
  needsReprotect: !!a2.needs_reprotect,
  failoverTargets: a2.failover_targets || [], restoreTargets: a2.restore_targets || [],
  backupPolicyId: a2.backup_policy_id || null, backupPolicyName: a2.backup_policy_name || null,
  cgId: a2.cg_id || null, cgName: a2.cg_name || null,
  // Protection lag rolled up over the group: the oldest member's last successful
  // cycle, and the data written since then.
  group: {lastAt: a2.group_last_at || null, writtenSince: a2.group_written_since || 0,
    lagSeconds: a2.group_lag_seconds, volumes: (a2.volumes || []).map(v => ({
      id: v.lvol_id, name: v.lvol_name, size: v.size, lastAt: v.last_at,
      writtenSince: v.written_since, status: v.status}))},
  counts: {pvcs: a2.pvcs_count || 0}, memberIds: a2.pvc_ids || [],
  capacity: {total: 0, used: 0}, createdAt: a2.created_at
});
const normZone = s => reg({
  kind: "zone", id: s.uuid, name: s.name, location: s.location, region: s.region,
  status: "active", region: s.region, label: s.label, regionLabel: s.region_label, createdAt: s.created_at,
  clusterIds: s.cluster_ids || [], racks: s.racks || [],
  k8sClusterIds: s.k8s_cluster_ids || [], k8sClusters: s.k8s_clusters || [],
  untaintedHosts: s.untainted_hosts || 0,
  counts: {hosts: s.hosts_count || 0, hostsPrepared: s.hosts_prepared || 0, nvme: s.nvme_hosts || 0,
    nodes: s.nodes_count || 0, clusters: (s.cluster_ids || []).length},
  capacity: {total: s.size_total || 0, used: 0}
});
const normTask = t => ({
  kind: "task", id: t.uuid, clusterId: t.cluster_id, parentId: t.parent_id,
  fn: t.function_name, target: t.target_id, nodeId: t.node_id, distrib: t.distrib,
  retry: t.retry, maxRetry: t.max_retry, status: t.status, result: t.result,
  canceled: !!t.canceled, subtaskTotal: t.subtask_total || 0,
  createdAt: t.created_at, updatedAt: t.updated_at
});
const normLog = l => ({
  id: l.uuid, ts: l.ts, level: l.level, event: l.event, message: l.message,
  nodeId: l.node_id, storageId: l.storage_id, vuid: l.vuid, recordStatus: l.record_status,
  objectKind: l.object_kind, objectId: l.object_id, objectName: l.object_name
});
// An alert is an indicator, not an event: it exists while its condition holds
// and vanishes when the condition clears. There is no acknowledged/closed
// lifecycle — only a silence, which dies with the condition.
const normAlert = a => ({
  id: a.uuid, clusterId: a.cluster_id, clusterName: a.cluster_name,
  rule: a.rule, severity: a.severity, scope: a.scope,
  title: a.title, detail: a.detail, remedy: a.remedy,
  nodeId: a.node_id, nodeName: a.node_name,
  nodeIds: a.node_ids || [], nodeNames: a.node_names || [],
  deviceIds: a.device_ids || [], deviceNames: a.device_names || [],
  container: a.container || null,
  since: a.since, silenced: !!a.silenced, silencedBy: a.silenced_by || null
});

// An operation is an Ops CRD: a named action on one object that walks a fixed
// list of phases and lands in Succeeded, Failed or Aborted. The phase list is
// the state machine — the console reads it rather than inventing its own.
const OPS_KINDS = ["StorageClusterOps", "StorageNodeOps", "StorageDeviceOps", "StoragePoolOps",
  "PersistentVolumeOps", "StorageBackupOps", "ControlPlaneOps", "OperatorOps"];
const OPS_TARGET_LABEL = {StorageCluster: "cluster", StorageNode: "node", StorageDevice: "device",
  StoragePool: "pool", PersistentVolume: "volume", StorageBackup: "backup", ControlPlane: "control plane", Operator: "operator"};
const normOperation = o => {
  const st = o.status || {}, step = st.step || {};
  return {
    id: o.metadata.uid, name: o.metadata.name, kind: o.kind,
    action: (o.spec || {}).action, clusterId: st.clusterId || null,
    targetKind: st.targetKind || (window.RESOURCES[o.kind] || {}).ops || null,
    targetName: ((o.spec || {}).targetRef || {}).name || null,
    phase: st.phase, message: st.message || "",
    step: step.label || step.state, stepKey: step.state,
    steps: step.labels || step.steps || [], stepIndex: step.index || 0,
    abortable: !!step.abortable, aborting: !!(o.spec || {}).abort,
    startedAt: st.startedAt, completedAt: st.completedAt || null,
    events: st.events || [],
    running: !["Succeeded", "Failed", "Aborted"].includes(st.phase)
  };
};

const normRole = r => ({kind: "role", id: r.uuid, uuid: r.uuid, name: r.name, builtin: !!r.builtin,
  description: r.description || "", rights: r.rights || [], createdBy: r.createdBy || null, createdAt: r.created_at});

// ---- endpoints -------------------------------------------------------------
const api = {
  clusters: () => req("/clusters").then(r => r.map(normCluster)),
  cluster: id => req(`/clusters/${id}`).then(r => normCluster(r[0])),
  hosts: cid => req(`/clusters/${cid}/hosts`).then(r => r.map(normHost)),
  host: id => req(`/hosts/${id}`).then(r => normHost(r[0])),
  nodes: cid => req(`/clusters/${cid}/storage-nodes`).then(r => r.map(normNode)),
  node: id => req(`/storage-nodes/${id}`).then(r => normNode(r[0])),
  devices: nid => req(`/storage-nodes/${nid}/devices`).then(r => r.map(normDevice)),
  device: id => req(`/devices/${id}`).then(r => normDevice(r[0])),
  pools: cid => req(`/clusters/${cid}/pools`).then(r => r.map(normPool)),
  pool: id => req(`/pools/${id}`).then(r => normPool(r[0])),
  poolVolumes: pid => req(`/pools/${pid}/lvols`).then(r => r.map(normVolume)),
  poolStorageClasses: pid => req(`/pools/${pid}/storage-classes`).then(r => r.map(normSc)),
  clusterVolumes: cid => req(`/clusters/${cid}/lvols`).then(r => r.map(normVolume)),
  volume: id => req(`/lvols/${id}`).then(r => normVolume(r[0])),
  snapshots: (scope, id) => req(`/${scope}/${id}/snapshots`).then(r => r.map(normSnapshot)),
  snapshot: id => req(`/snapshots/${id}`).then(r => normSnapshot(r[0])),
  backups: (scope, id) => req(`/${scope}/${id}/backups`).then(r => r.map(normBackup)),
  backup: id => req(`/backups/${id}`).then(r => normBackup(r[0])),
  policies: cid => req(`/clusters/${cid}/backup-policies`).then(r => r.map(normPolicy)),
  policy: id => req(`/backup-policies/${id}`).then(r => normPolicy(r[0])),
  cgroups: cid => req(`/clusters/${cid}/consistency-groups`).then(r => r.map(normCg)),
  cgroup: id => req(`/consistency-groups/${id}`).then(r => normCg(r[0])),
  cgroupVolumes: id => req(`/consistency-groups/${id}/lvols`).then(r => r.map(normVolume)),
  cgSnapshots: id => req(`/consistency-groups/${id}/snapshots`).then(r => r.map(normCgSnap)),
  cgSnapshot: id => req(`/cg-snapshots/${id}`).then(r => normCgSnap(r[0])),
  cgroupCreate: (cid, p) => send("POST", `/clusters/${cid}/consistency-groups`, p),
  cgroupAddVolumes: (id, ids) => send("POST", `/consistency-groups/${id}/lvols`, {lvol_ids: ids}),
  cgroupRemoveVolume: (id, vid) => send("DELETE", `/consistency-groups/${id}/lvols/${vid}`),
  cgroupSnapshot: (id, name) => send("POST", `/consistency-groups/${id}/snapshot`, {name}),
  cgroupDelete: id => send("DELETE", `/consistency-groups/${id}`),
  cgroupSetPolicy: (id, policyId) => send("PUT", `/consistency-groups/${id}/backup-policy`, {policy_id: policyId}),
  cgroupReplicate: (id, cfg) => send("POST", `/consistency-groups/${id}/replicate`, cfg),
  cgroupUnreplicate: id => send("POST", `/consistency-groups/${id}/unreplicate`),
  cgroupApps: id => req("/protected-apps").then(r => r.map(normApp).filter(a2 => a2.cgId === id)),
  cgSnapBackup: (id, bucket) => send("POST", `/cg-snapshots/${id}/backup`, {bucket}),
  cgSnapRestore: (id, p) => send("POST", `/cg-snapshots/${id}/restore`, p),
  cgSnapDelete: id => send("DELETE", `/cg-snapshots/${id}`),
  snapshotRestore: (id, p) => send("POST", `/snapshots/${id}/restore`, p),
  clusterSetKms: (cid, p) => send("PUT", `/clusters/${cid}/kms`, p),
  clusterTestKms: cid => send("POST", `/clusters/${cid}/kms/test`),
  tasks: cid => req(`/clusters/${cid}/tasks`).then(r => r.map(normTask)),
  subtasks: tid => req(`/tasks/${tid}/subtasks`).then(r => r.map(normTask)),
  taskCancel: tid => send("POST", `/tasks/${tid}/cancel`),
  clusterLogs: cid => req(`/clusters/${cid}/logs`).then(r => r.map(normLog)),
  alerts: cid => req(`/clusters/${cid}/alerts`).then(r => r.map(normAlert)),
  allAlerts: () => req("/alerts").then(r => r.map(normAlert)),
  cpAlerts: () => req("/control-plane/alerts").then(r => r.map(normAlert)),
  operations: cid => Promise.all(OPS_KINDS.map(k2 => k8s.list(k2).catch(() => [])))
    .then(rs => rs.flat().map(normOperation)
      .filter(o => !cid || o.clusterId === cid)
      .sort((x, y) => (y.running - x.running) || Date.parse(y.startedAt) - Date.parse(x.startedAt))),
  operationAbort: (kind, name) => abortOps(kind, name),
  alertRuleCount: scope => ((window.X && window.X.alertRuleCounts) || {})[scope] || null,
  alertSilence: id => send("POST", `/alerts/${encodeURIComponent(id)}/silence`),
  alertUnsilence: id => send("POST", `/alerts/${encodeURIComponent(id)}/unsilence`),
  unassignedHosts: () => req("/hosts/unassigned").then(r => r.map(normHost)),
  // ---- replication: the real v1alpha1 kinds, on the Kubernetes API ----
  // Pairs, policies and slots are three lists that have to be joined, because
  // the CRDs reference each other by name and carry no rollups.
  replTree: () => Promise.all([
    k8s.list("ReplicationPair"), k8s.list("ReplicationPolicy"), k8s.list("ReplicationSlot")
  ]).then(([pairs, pols, slots]) => ({pairs, pols, slots})),
  pairs: () => api.replTree().then(t => t.pairs.map(p => normReplPair(p, t.pols, t.slots))),
  pair: id => api.replTree().then(t => {
    const p = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!p) throw new ApiError(404, `ReplicationPair ${id} not found`, "", "NotFound");
    return normReplPair(p, t.pols, t.slots);
  }),
  allRPolicies: () => api.replTree().then(t => t.pols.map(p => normReplPolicy(p, t.slots, t.pairs))),
  // a policy belongs to a pair, and a pair to a source cluster — so a
  // cluster-scoped list is a filter on the pair's sourceCluster
  rpolicies: cid => api.replTree().then(t => {
    const name = (REG[cid] || {}).name;
    const mine = t.pairs.filter(p => (p.spec || {}).sourceCluster === name).map(p => p.metadata.name);
    return t.pols.filter(p => mine.includes((p.spec || {}).pairRef))
      .map(p => normReplPolicy(p, t.slots, t.pairs));
  }),
  rpolicy: id => api.replTree().then(t => {
    const p = t.pols.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!p) throw new ApiError(404, `ReplicationPolicy ${id} not found`, "", "NotFound");
    return normReplPolicy(p, t.slots, t.pairs);
  }),
  pairRPolicies: id => api.replTree().then(t => {
    const pair = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    return t.pols.filter(p => pair && (p.spec || {}).pairRef === pair.metadata.name)
      .map(p => normReplPolicy(p, t.slots, t.pairs));
  }),
  slots: () => k8s.list("ReplicationSlot").then(r => r.map(normReplSlot)),
  slot: id => k8s.list("ReplicationSlot").then(r => {
    const s = r.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!s) throw new ApiError(404, `ReplicationSlot ${id} not found`, "", "NotFound");
    return normReplSlot(s);
  }),
  policySlots: id => Promise.all([k8s.list("ReplicationPolicy"), k8s.list("ReplicationSlot")])
    .then(([pols, slots]) => {
      const p = pols.find(x => x.metadata.uid === id || x.metadata.name === id);
      return slots.filter(s => p && (s.spec || {}).policyRef === p.metadata.name).map(normReplSlot);
    }),
  pairSlots: id => api.replTree().then(t => {
    const pair = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    const names = t.pols.filter(p => pair && (p.spec || {}).pairRef === pair.metadata.name)
      .map(p => p.metadata.name);
    return t.slots.filter(s => names.includes((s.spec || {}).policyRef)).map(normReplSlot);
  }),
  replOps: () => k8s.list("ReplicationOps").then(r => r.map(normReplOps)),
  replOp: id => k8s.list("ReplicationOps").then(r => {
    const o = r.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!o) throw new ApiError(404, `ReplicationOps ${id} not found`, "", "NotFound");
    return normReplOps(o);
  }),
  // scope-filtered history, so a policy or pair can show its own operations
  refReplOps: ref => k8s.list("ReplicationOps").then(r =>
    r.filter(o => (o.spec || {}).ref === ref).map(normReplOps)),
  pairCreateCrd: (name, sourceCluster, targetCluster) => k8s.create("ReplicationPair", {
    apiVersion: window.API_GROUP, kind: "ReplicationPair",
    metadata: {name}, spec: {sourceCluster, targetCluster}
  }),
  rpolicyCreateCrd: (name, spec) => k8s.create("ReplicationPolicy", {
    apiVersion: window.API_GROUP, kind: "ReplicationPolicy", metadata: {name}, spec
  }),
  // every failover, failback and planned cutover is a ReplicationOps
  replOpsCreate: spec => k8s.create("ReplicationOps", {
    apiVersion: window.API_GROUP, kind: "ReplicationOps",
    metadata: {name: `${spec.action}-${spec.ref}-${Date.now().toString(36)}`.slice(0, 253)},
    spec
  }),
  pairDeleteCrd: name => k8s.remove("ReplicationPair", name),
  rpolicyDeleteCrd: name => k8s.remove("ReplicationPolicy", name),
  // membership is a PVC annotation, nothing else
  pvcSetReplPolicy: (pvcName, namespace, policyName) => k8s.patch("PersistentVolumeClaim", pvcName,
    {metadata: {annotations: {"storage.simplyblock.io/replication-policy": policyName || null}}},
    {namespace}),
  // ---- migration paths: A → B inside a stretched Kubernetes cluster ----
  mpaths: () => req("/migration-paths").then(r => r.map(normMPath)),
  mpath: id => req(`/migration-paths/${id}`).then(r => normMPath(r[0])),
  mpathGroups: id => req(`/migration-paths/${id}/app-groups`).then(r => r.map(normAppGroup).sort((x, y) => x.order - y.order)),
  mpathCreate: p => send("POST", "/migration-paths", p),
  mpathPause: id => send("POST", `/migration-paths/${id}/pause`),
  mpathResume: id => send("POST", `/migration-paths/${id}/resume`),
  mpathDelete: id => send("DELETE", `/migration-paths/${id}`),
  mpathReorder: (id, order) => send("PUT", `/migration-paths/${id}/queue`, {order}),
  appGroupCreate: (pathId, p) => send("POST", `/migration-paths/${pathId}/app-groups`, p),
  appGroup: id => req(`/app-groups/${id}`).then(r => normAppGroup(r[0])),
  appGroupVolumes: id => req(`/app-groups/${id}/lvols`).then(r => r.map(normVolume)),
  appGroupMove: id => send("POST", `/app-groups/${id}/move`),
  appGroupPause: id => send("POST", `/app-groups/${id}/pause`),
  appGroupResume: id => send("POST", `/app-groups/${id}/resume`),
  appGroupApproval: (id, approval) => send("PUT", `/app-groups/${id}/approval`, {approval}),
  appGroupDelete: id => send("DELETE", `/app-groups/${id}`),
  migrations: cid => req(`/clusters/${cid}/migrations`).then(r => r.map(normMigration)),
  allMigrations: () => req("/migrations").then(r => r.map(normMigration)),
  migration: id => req(`/migrations/${id}`).then(r => normMigration(r[0])),
  migrationVolumes: id => req(`/migrations/${id}/lvols`).then(r => r.map(normVolume)),
  migrationCreate: (cid, p) => send("POST", `/clusters/${cid}/migrations`, p),
  migrationCutover: id => send("POST", `/migrations/${id}/cutover`),
  migrationPause: id => send("POST", `/migrations/${id}/pause`),
  migrationResume: id => send("POST", `/migrations/${id}/resume`),
  migrationCancel: id => send("DELETE", `/migrations/${id}`),
  hostSetTaint: (id, taint) => send("PUT", `/hosts/${id}/migration-taint`, {taint}),
  // ---- DR top layer: plans and sites ----
  plans: () => req("/protection-plans").then(r => r.map(normPlan)),
  plan: id => req("/protection-plans/" + id).then(r => normPlan(r[0])),
  planApps: id => req("/protection-plans/" + id + "/protected-apps").then(r => r.map(normApp)),
  planSites: id => req("/protection-plans/" + id + "/sites").then(r => r.map(normSite)),
  planCreate: p => send("POST", "/protection-plans", p),
  planDelete: id => send("DELETE", "/protection-plans/" + id),
  planAddMethod: (id, m) => send("POST", "/protection-plans/" + id + "/methods", m),
  planRemoveMethod: (id, name) => send("DELETE", "/protection-plans/" + id + "/methods/" + name),
  planSetInterval: (id, name, interval) => send("PUT", "/protection-plans/" + id + "/methods/" + name + "/interval", {interval}),
  sites: () => req("/dr-sites").then(r => r.map(normSite)),
  site: id => req("/dr-sites/" + id).then(r => normSite(r[0])),
  siteApps: id => req("/dr-sites/" + id + "/protected-apps").then(r => r.map(normApp)),
  siteFence: id => send("POST", "/dr-sites/" + id + "/fence"),
  siteUnfence: id => send("POST", "/dr-sites/" + id + "/unfence"),
  sitePlans: id => req("/dr-sites/" + id + "/protection-plans").then(r => r.map(normPlan)),
  // bypass payload 4: no Kubernetes object represents a generation
  appGenerations: id => req("/protected-apps/" + id + "/generations").then(r => r.map(normGeneration)),
  appSetOrchestrated: (id, method, allowVault) => send("PUT", "/protected-apps/" + id + "/orchestrated-method", {method, allow_vault: !!allowVault}),
  // bypass payload 2: pin the generation, then rebind the DRPC
  appRestore: (id, generation) => send("POST", "/protected-apps/" + id + "/restore", {generation}),
  appVerifyGeneration: (id, generation) => send("POST", "/protected-apps/" + id + "/verify-generation", {generation}),
  // bypass payload 6: three sites need a quorum decision Ramen does not model
  arbitration: () => req("/dr-arbitration").then(r => normArbitration(r[0])),
  drClusters: () => req("/dr-clusters").then(r => r.map(normDrCluster)),
  drCluster: id => req(`/dr-clusters/${id}`).then(r => normDrCluster(r[0])),
  drClusterApps: id => req(`/dr-clusters/${id}/protected-apps`).then(r => r.map(normApp)),
  rpolicyApps: id => req(`/replication-policies/${id}/protected-apps`).then(r => r.map(normApp)),
  protectedApps: () => req("/protected-apps").then(r => r.map(normApp)),
  protectedApp: id => req(`/protected-apps/${id}`).then(r => normApp(r[0])),
  protectedAppPvcs: id => req(`/protected-apps/${id}/pvcs`).then(r => r.map(normPvc)),
  appProtect: p => send("POST", "/protected-apps", p),
  appUpdate: (id, p) => send("PUT", `/protected-apps/${id}`, p),
  appSetRecipe: (id, p) => send("PUT", `/protected-apps/${id}/recipe`, p),
  // application resources in a namespace, one Kubernetes list per kind — the
  // callback fires per kind as each list lands, so the form fills in progressively
  nsResourcesEach: (ns, cb) => Promise.all(["Deployment", "StatefulSet", "Service", "ConfigMap", "Secret", "Ingress", "PersistentVolumeClaim", "VirtualMachine"]
    .map(kind => k8s.list(kind, {namespace: ns}).then(items => cb(kind, items, null)).catch(e => cb(kind, [], e)))),
  appFailover: (id, p) => send("POST", `/protected-apps/${id}/failover`, p || {}),
  appSetProtection: (id, p) => send("PUT", `/protected-apps/${id}/protection`, p),
  appRelocate: id => send("POST", `/protected-apps/${id}/relocate`),
  appCleanup: id => send("POST", `/protected-apps/${id}/cleanup`),
  appUnprotect: id => send("DELETE", `/protected-apps/${id}`),
  drClusterFence: id => send("POST", `/dr-clusters/${id}/fence`),
  drClusterUnfence: id => send("POST", `/dr-clusters/${id}/unfence`),
  // access control — RBAC-DESIGN.md. Roles and bindings are proposed CRDs;
  // /access/self is the operator's "what may I do" read.
  accessSelf: () => req("/access/self").then(r => r[0]),
  accessRoles: () => req("/access-roles").then(r => r.map(normRole)),
  accessRoleCreate: b => send("POST", "/access-roles", b),
  accessRoleUpdate: (id, b) => send("PUT", `/access-roles/${id}`, b),
  accessRoleDelete: id => send("DELETE", `/access-roles/${id}`),
  accessBindings: () => req("/access-bindings"),
  accessBindingCreate: b => send("POST", "/access-bindings", b),
  accessBindingDelete: id => send("DELETE", `/access-bindings/${id}`),
  k8sClusters: () => req("/kubernetes-clusters").then(r => r.map(normK8s)),
  k8sCluster: id => req(`/kubernetes-clusters/${id}`).then(r => normK8s(r[0])),
  k8sStorageClasses: id => req(`/kubernetes-clusters/${id}/storage-classes`).then(r => r.map(normSc)),
  k8sPvcs: id => req(`/kubernetes-clusters/${id}/pvcs`).then(r => r.map(normPvc)),
  k8sHosts: id => req(`/kubernetes-clusters/${id}/hosts`).then(r => r.map(normHost)),
  k8sZones: id => req(`/kubernetes-clusters/${id}/zones`).then(r => r.map(normZone)),
  k8sStorageClusters: id => req(`/kubernetes-clusters/${id}/storage-clusters`).then(r => r.map(normCluster)),
  clusterK8s: id => req(`/clusters/${id}/kubernetes-clusters`).then(r => r.map(normK8s)),
  storageClass: id => req(`/storage-classes/${id}`).then(r => normSc(r[0])),
  storageClassPvcs: id => req(`/storage-classes/${id}/pvcs`).then(r => r.map(normPvc)),
  pvcs: () => req("/pvcs").then(r => r.map(normPvc)),
  pvcResize: (id, size) => send("POST", `/pvcs/${id}/resize`, {size}),
  buckets: cid => req(`/clusters/${cid}/buckets`).then(r => r.map(normBucket)),
  bucket: id => req(`/buckets/${id}`).then(r => normBucket(r[0])),
  bucketCreate: (cid, p) => send("POST", `/clusters/${cid}/buckets`, p),
  bucketUpdate: (id, p) => send("PUT", `/buckets/${id}`, p),
  bucketSetAccess: (id, p) => send("PUT", `/buckets/${id}/access`, p),
  bucketResize: (id, size) => send("POST", `/buckets/${id}/resize`, {size}),
  bucketDelete: id => send("DELETE", `/buckets/${id}`),
  bucketSetTags: (id, tags) => send("PUT", `/buckets/${id}/tags`, {tags}),
  bucketReplicate: (id, policyId) => send("POST", `/buckets/${id}/replicate`, {policy_id: policyId}),
  bucketUnreplicate: id => send("POST", `/buckets/${id}/unreplicate`),
  clusterSetFileStorage: (cid, p) => send("PUT", `/clusters/${cid}/file-storage`, p),
  clusterFailoverMds: cid => send("POST", `/clusters/${cid}/file-storage/failover`),
  clusterSetObjectStorage: (cid, p) => send("PUT", `/clusters/${cid}/object-storage`, p),
  pvc: id => req(`/pvcs/${id}`).then(r => normPvc(r[0])),
  zoneK8sClusters: id => req(`/zones/${id}/kubernetes-clusters`).then(r => r.map(normK8s)),
  zones: () => req("/zones").then(r => r.map(normZone)),
  zone: id => req(`/zones/${id}`).then(r => normZone(r[0])),
  zoneHosts: id => req(`/zones/${id}/hosts`).then(r => r.map(normHost)),
  zoneClusters: id => req(`/zones/${id}/clusters`).then(r => r.map(normCluster)),
  clusterZones: cid => req(`/clusters/${cid}/zones`).then(r => r.map(normZone)),

  pairCreate: p => send("POST", "/cluster-pairs", p),
  pairTest: id => send("POST", `/cluster-pairs/${id}/test`),
  pairDelete: id => send("DELETE", `/cluster-pairs/${id}`),
  rpolicyCreate: p => send("POST", "/replication-policies", p),
  rpolicyUpdate: (id, p) => send("PUT", `/replication-policies/${id}`, p),
  rpolicyAddVolumes: (id, ids) => send("POST", `/replication-policies/${id}/lvols`, {lvol_ids: ids}),
  rpolicyRemoveVolume: (id, vid) => send("DELETE", `/replication-policies/${id}/lvols/${vid}`),
  rpolicyFailover: (id, planned) => send("POST", `/replication-policies/${id}/failover`, {planned}),
  rpolicyFailback: id => send("POST", `/replication-policies/${id}/failback`),
  rpolicyTest: id => send("POST", `/replication-policies/${id}/test-failover`),
  rpolicyResync: id => send("POST", `/replication-policies/${id}/resync`),
  rpolicyDelete: id => send("DELETE", `/replication-policies/${id}`),
  clusterSetZones: (cid, ids) => send("PUT", `/clusters/${cid}/zones`, {zone_ids: ids}),
  hostSetPlacement: (id, p) => send("PUT", `/hosts/${id}/placement`, p),
  // ---- discovery + deployment ----
  // A cluster is not created by POSTing a cluster: discovery writes an
  // inventory, a ClusterDeploymentConfig is drafted against it, and approving
  // that document is what deploys.
  discoveries: kid => preq(`/discoveries?scope=kubernetes-clusters&scopeId=${kid}`)
    .then(r => r.map(normDiscovery)),
  discoveryRun: (kid, name, filter, nodeSelector) => k8s.create("OperatorOps", {
    apiVersion: window.API_GROUP, kind: "OperatorOps",
    metadata: {name: `discover-${(name || "cluster").slice(0, 30)}-${Date.now().toString(36)}`,
      namespace: window.SB_CONFIG.namespace},
    spec: {action: "Discover", kubernetesClusterRef: name,
      target: {kubernetesClusterId: kid},
      discover: {deviceFilter: filter, nodeSelector: nodeSelector || {matchLabels: {}}}}
  }),
  deployConfigs: () => k8s.list("ClusterDeploymentConfig").then(r => r.map(normDeployConfig)),
  k8sDeployConfigs: kid => k8s.list("ClusterDeploymentConfig")
    .then(r => r.map(normDeployConfig).filter(c => c.k8sClusterId === kid)),
  deployConfig: id => k8s.list("ClusterDeploymentConfig").then(r => {
    const hit = r.map(normDeployConfig).find(c => c.id === id || c.name === id);
    if (!hit) throw new ApiError(404, "deployment config not found", "/" + id, "NotFound");
    return hit;
  }),
  deployConfigCreate: (name, spec) => k8s.create("ClusterDeploymentConfig", {
    apiVersion: window.API_GROUP, kind: "ClusterDeploymentConfig",
    metadata: {name, namespace: window.SB_CONFIG.namespace},
    spec: Object.assign({approved: false}, spec)
  }),
  // approval is the only mutable field, and it is one-way
  deployConfigApprove: name => k8s.patch("ClusterDeploymentConfig", name, {spec: {approved: true}}),
  // step / node logs: the operator's job pod logs, keyed by step[/host]
  deployLog: (id, name) => preq(`/deployments/${id}/logs?name=${encodeURIComponent(name)}`),
  deployConfigDelete: name => k8s.remove("ClusterDeploymentConfig", name),
  clusterCreate: payload => send("POST", "/clusters", payload),

  // ---- mutations ----
  clusterSuspend: id => send("POST", `/clusters/${id}/suspend`),
  clusterActivate: id => send("POST", `/clusters/${id}/activate`),
  clusterAddNode: (id, hostId, failureDomain) => send("POST", `/clusters/${id}/storage-nodes`, {host_id: hostId, failure_domain: failureDomain || null}),
  nodeShutdown: (id, force) => send("POST", `/storage-nodes/${id}/shutdown`, {force: !!force}),
  nodeRestart: id => send("POST", `/storage-nodes/${id}/restart`),
  nodeRemove: id => send("DELETE", `/storage-nodes/${id}`),
  nodeMigrate: (id, hostId) => send("POST", `/storage-nodes/${id}/migrate`, {host_id: hostId}),
  nodeAddDevice: (id, payload) => send("POST", `/storage-nodes/${id}/devices`, payload),
  deviceRestart: id => send("POST", `/devices/${id}/restart`),
  deviceFail: id => send("POST", `/devices/${id}/fail`),
  deviceHealthCheck: id => send("POST", `/devices/${id}/health-check`),
  deviceRemove: id => send("DELETE", `/devices/${id}`),
  hostReserveDevice: (hid, did, nodeId) => send("POST", `/hosts/${hid}/devices/${did}/reserve`, {node_id: nodeId}),
  hostsPrepare: (cid, ids) => send("POST", `/clusters/${cid}/hosts/prepare`, {host_ids: ids}),
  hostConfigure: (id, p) => send("POST", `/hosts/${id}/configure`, p),
  volumeDelete: id => send("DELETE", `/lvols/${id}`),
  volumeResize: (id, size) => send("POST", `/lvols/${id}/resize`, {size}),
  volumeSnapshot: (id, name) => send("POST", `/lvols/${id}/snapshot`, {name}),
  volumeClone: (id, name) => send("POST", `/lvols/${id}/clone`, {name}),
  // instant migration: the primary moves without copying data
  volumeMigrate: (id, p) => send("POST", `/lvols/${id}/migrate`, p),
  volumeRebalance: id => send("POST", `/lvols/${id}/rebalance`),
  migrationTasks: cid => req(`/clusters/${cid}/tasks?function_name=lvol_migration`).then(r => r.map(normTask)),
  volumeSetAffinity: (id, p) => send("PUT", `/lvols/${id}/affinity`, p),
  clusterRebalance: id => send("POST", `/clusters/${id}/rebalance`),
  clusterSetRebalance: (id, enabled) => send("PUT", `/clusters/${id}/auto-rebalance`, {enabled}),
  volumeBackup: (id, payload) => send("POST", `/lvols/${id}/backup`, payload),
  snapshotBackup: (id, bucket) => send("POST", `/snapshots/${id}/backup`, {bucket}),
  backupMerge: id => send("POST", `/backups/${id}/merge`),
  backupMergeVersion: (id, vid) => send("POST", `/backups/${id}/versions/${vid}/merge`),
  policyCreate: (cid, p) => send("POST", `/clusters/${cid}/backup-policies`, p),
  policyUpdate: (id, p) => send("PUT", `/backup-policies/${id}`, p),
  policyDelete: id => send("DELETE", `/backup-policies/${id}`),
  volumeSetPolicy: (id, policyId) => send("PUT", `/lvols/${id}/backup-policy`, {policy_id: policyId}),
  volumeSetQos: (id, qos) => send("PUT", `/lvols/${id}/qos`, qos),
  volumeSetDataReduction: (id, p) => send("PUT", `/lvols/${id}/data-reduction`, p),
  poolSetQos: (id, qos) => send("PUT", `/pools/${id}/qos`, qos),
  poolCreate: (cid, p) => send("POST", `/clusters/${cid}/pools`, p),
  poolEnable: id => send("POST", `/pools/${id}/enable`),
  poolDisable: id => send("POST", `/pools/${id}/disable`),
  snapshotDelete: id => send("DELETE", `/snapshots/${id}`),
  snapshotClone: (id, name) => send("POST", `/snapshots/${id}/clone`, {name}),
  backupRestore: (id, name, versionId) => send("POST", `/backups/${id}/restore`, {name, version_id: versionId}),
  backupExport: (id, destination, versionId) => send("POST", `/backups/${id}/export`, {destination, version_id: versionId}),
  backupDelete: id => send("DELETE", `/backups/${id}`)
};
const GETTER = {plan: api.plan, site: api.site, cluster: api.cluster, host: api.host, node: api.node, device: api.device, pool: api.pool,
  volume: api.volume, snapshot: api.snapshot, backup: api.backup, policy: api.policy,
  pair: api.pair, rpolicy: api.rpolicy, slot: api.slot, replops: api.replOp, zone: api.zone, cgroup: api.cgroup, cgsnapshot: api.cgSnapshot,
  migration: api.migration, k8sc: api.k8sCluster, storageclass: api.storageClass, pvc: api.pvc,
  drcluster: api.drCluster, protectedapp: api.protectedApp,
  bucket: api.bucket, deployconfig: api.deployConfig, mpath: api.mpath, appgroup: api.appGroup};
const resolve = (kind, id) => REG[id] ? Promise.resolve(REG[id]) : (GETTER[kind] ? GETTER[kind](id).catch(() => null) : Promise.resolve(null));

// ---- data hooks ------------------------------------------------------------
function useResource(key, loader, pollMs) {
  const ref = React.useRef(loader); ref.current = loader;
  const [s, setS] = React.useState({loading: true, data: null, error: null});
  const load = React.useCallback(async quiet => {
    if (!quiet) setS(p => ({loading: true, data: null, error: null}));
    try { const d = await ref.current(); setS({loading: false, data: d, error: null}); }
    catch (e) { setS(p => quiet ? p : {loading: false, data: null, error: e}); }
  }, [key]);
  React.useEffect(() => { load(false); }, [key]);
  React.useEffect(() => {
    if (!pollMs) return;
    const i = setInterval(() => load(true), pollMs);
    return () => clearInterval(i);
  }, [key, pollMs]);
  return {...s, reload: () => load(false)};
}

Object.assign(window, {api, API, ApiError, STATUS_META, REG, GETTER, resolve, useResource,
  normCluster, normHost, normNode, normDevice, normPool, normVolume, normSnapshot, normBackup, normPolicy,
  normTask, normLog, normAlert, normOperation, OPS_TARGET_LABEL, normPlan, normSite, normMethod, normLeg, normGeneration,
  normReplPair, normReplPolicy, normReplSlot, normReplOps, ivMinutes, normPair, normDrPolicy, normZone, normCg, normCgSnap, normMigration, normK8s, normSc, normPvc, normBucket, normDrCluster, normApp});

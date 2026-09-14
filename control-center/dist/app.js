// generated from the *.jsx sources — do not edit; rebuild with build.md
// ---- api.jsx ----
(function(){
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
    res = await fetch(window.SB_CONFIG.operatorBase + "/proposed" + path, {
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${window.SB_CONFIG.token}`
      }
    });
  } catch (e) {
    throw new ApiError(0, "Cannot reach the operator API", path, "Unreachable");
  }
  let body = null;
  try {
    body = await res.json();
  } catch (e) {}
  if (!res.ok) throw new ApiError(res.status, body && (body.message || body.error) || "Request failed", path, body && body.reason || null);
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
  clusters: "StorageCluster",
  "storage-nodes": "StorageNode",
  devices: "StorageDevice",
  pools: "StoragePool",
  backups: "StorageBackup"
};
// how a child record points back at each kind of parent
const SCOPE_FIELD = {
  clusters: "cluster_id",
  pools: "pool_id",
  lvols: "lvol_id",
  "storage-nodes": "node_id",
  hosts: "host_id",
  devices: "device_id",
  "backup-policies": "policy_id",
  "replication-policies": "policy_id",
  tasks: "parent_id",
  pairs: "pair_id",
  "kubernetes-clusters": "k8s_cluster_id",
  "k8s-clusters": "k8s_cluster_id",
  zones: "zone_id",
  "storage-classes": "storage_class_id",
  "consistency-groups": "cg_id",
  "dr-clusters": "dr_cluster_id",
  snapshots: "snapshot_id"
};

// CRD reads carry their Kubernetes identity as __k8s so a view model can show
// activeOpsRef and conditions alongside the record.
const withK8s = o => Object.assign({}, o.status && o.status.extras || {}, {
  __k8s: {
    name: o.metadata.name,
    kind: o.kind,
    uid: o.metadata.uid,
    labels: o.metadata.labels || {},
    generation: o.metadata.generation,
    observedGeneration: o.status.observedGeneration,
    phase: o.status.phase,
    conditions: o.status.conditions || [],
    activeOpsRef: o.status.activeOpsRef || null
  }
});
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
const send = (method, path, payload) => req(path, {
  method,
  body: JSON.stringify(payload || {})
});

// ---- mutations -------------------------------------------------------------
// An action is not a verb against an entity: it creates an <Entity>Ops object
// whose spec.action names it. spec.abort is the only stop, so a cancel is a
// patch, never a DELETE.
const cap = s => s.charAt(0).toUpperCase() + s.slice(1).replace(/-(\w)/g, (_, c) => c.toUpperCase());
const OPS_VERBS = {
  clusters: {
    kind: "StorageCluster",
    verbs: {
      suspend: "Suspend",
      activate: "Activate",
      restart: "Activate",
      rebalance: "Rebalance",
      "storage-nodes": "Expand"
    }
  },
  "storage-nodes": {
    kind: "StorageNode",
    verbs: {
      restart: "Restart",
      shutdown: "Shutdown",
      migrate: "Migrate",
      devices: "AddDevice"
    }
  },
  devices: {
    kind: "StorageDevice",
    verbs: {
      restart: "Restart",
      fail: "Fail",
      "health-check": "HealthCheck",
      remove: "Remove"
    }
  },
  pools: {
    kind: "StoragePool",
    verbs: {
      enable: "Enable",
      disable: "Disable"
    }
  },
  lvols: {
    kind: "PersistentVolume",
    verbs: {
      resize: "Resize",
      snapshot: "Snapshot",
      clone: "Clone",
      migrate: "Migrate",
      backup: "Backup"
    }
  },
  backups: {
    kind: "StorageBackup",
    verbs: {
      merge: "Merge",
      restore: "Restore",
      export: "Export"
    }
  }
};
// DELETE on an entity is also an operation, not a raw delete
const DELETE_VERB = {
  "storage-nodes": ["StorageNode", "Remove"],
  devices: ["StorageDevice", "Remove"],
  backups: ["StorageBackup", "Delete"]
};

// entities are addressed by uuid inside the console and by name on the API
async function k8sNameOf(kind, id) {
  const items = await k8s.list(kind);
  const hit = items.find(o => {
    const s = o.status || {};
    return [s.clusterId, s.nodeId, s.deviceId, s.poolId, s.backupId, s.simplyblock && s.simplyblock.volumeId, o.metadata.uid].includes(id);
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
    method,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${window.SB_CONFIG.token}`
    },
    body: method === "DELETE" ? undefined : JSON.stringify(payload || {})
  });
  let b = null;
  try {
    b = await res.json();
  } catch (e) {}
  if (!res.ok) throw new ApiError(res.status, b && (b.message || b.error) || "Request failed", path, b && b.reason || null);
  return b && b.results !== undefined ? b.results : b;
}

// ---- status vocabulary -----------------------------------------------------
const STATUS_META = {
  online: {
    c: "var(--ok)",
    rank: 0,
    label: "online"
  },
  active: {
    c: "var(--ok)",
    rank: 0,
    label: "active"
  },
  paired: {
    c: "var(--ok)",
    rank: 0,
    label: "paired"
  },
  pairing: {
    c: "var(--info)",
    rank: 1,
    label: "pairing",
    blink: true
  },
  healthy: {
    c: "var(--ok)",
    rank: 0,
    label: "healthy"
  },
  unhealthy: {
    c: "var(--bad)",
    rank: 4,
    label: "unhealthy"
  },
  available: {
    c: "var(--ok)",
    rank: 0,
    label: "available"
  },
  enabled: {
    c: "var(--ok)",
    rank: 0,
    label: "enabled"
  },
  disabled: {
    c: "var(--warn)",
    rank: 3,
    label: "disabled"
  },
  discovered: {
    c: "var(--dim2)",
    rank: 2,
    label: "discovered"
  },
  inspecting: {
    c: "var(--info)",
    rank: 1,
    label: "inspecting",
    blink: true
  },
  inspected: {
    c: "var(--accent)",
    rank: 3,
    label: "ready to configure"
  },
  degraded: {
    c: "var(--warn)",
    rank: 3,
    label: "degraded"
  },
  suspended: {
    c: "var(--bad)",
    rank: 4,
    label: "suspended"
  },
  unready: {
    c: "var(--idle)",
    rank: 2,
    label: "unready"
  },
  in_activation: {
    c: "var(--info)",
    rank: 1,
    label: "in activation",
    blink: true
  },
  offline: {
    c: "var(--bad)",
    rank: 4,
    label: "offline"
  },
  unreachable: {
    c: "var(--alert)",
    rank: 5,
    label: "unreachable"
  },
  down: {
    c: "var(--bad)",
    rank: 4,
    label: "down"
  },
  in_restart: {
    c: "var(--info)",
    rank: 1,
    label: "in restart",
    blink: true
  },
  in_removal: {
    c: "var(--alert)",
    rank: 2,
    label: "in removal",
    blink: true
  },
  in_shutdown: {
    c: "var(--warn)",
    rank: 3,
    label: "in shutdown",
    blink: true
  },
  in_creation: {
    c: "var(--info)",
    rank: 1,
    label: "in creation",
    blink: true
  },
  in_migration: {
    c: "var(--info)",
    rank: 1,
    label: "in migration",
    blink: true
  },
  // ReplicationSlot.status.state — a slot that is replicating is in its steady
  // state, so it reads green rather than as a transient.
  replicating: {
    c: "var(--ok)",
    rank: 0,
    label: "replicating"
  },
  converging: {
    c: "var(--info)",
    rank: 1,
    label: "converging",
    blink: true
  },
  cutover_pending: {
    c: "var(--warn)",
    rank: 5,
    label: "ready to cut over"
  },
  cutover_done: {
    c: "var(--info)",
    rank: 1,
    label: "cutover done"
  },
  attaching: {
    c: "var(--idle)",
    rank: 4,
    label: "attaching"
  },
  detaching: {
    c: "var(--idle)",
    rank: 4,
    label: "detaching"
  },
  failed_over: {
    c: "var(--ro)",
    rank: 6,
    label: "failed over"
  },
  error: {
    c: "var(--bad)",
    rank: 9,
    label: "error"
  },
  frozen: {
    c: "var(--alert)",
    rank: 3,
    label: "io frozen",
    blink: true
  },
  running: {
    c: "var(--info)",
    rank: 1,
    label: "running",
    blink: true
  },
  paused: {
    c: "var(--warn)",
    rank: 3,
    label: "paused"
  },
  completed: {
    c: "var(--ok)",
    rank: 0,
    label: "completed"
  },
  Bound: {
    c: "var(--ok)",
    rank: 0,
    label: "Bound"
  },
  Running: {
    c: "var(--info)",
    rank: 3,
    label: "running",
    blink: true
  },
  Succeeded: {
    c: "var(--ok)",
    rank: 0,
    label: "succeeded"
  },
  Pending: {
    c: "var(--warn)",
    rank: 3,
    label: "Pending"
  },
  Lost: {
    c: "var(--bad)",
    rank: 4,
    label: "Lost"
  },
  Draft: {
    c: "var(--dim2)",
    rank: 2,
    label: "draft — not approved"
  },
  Approved: {
    c: "var(--accent)",
    rank: 1,
    label: "approved"
  },
  Deploying: {
    c: "var(--info)",
    rank: 1,
    label: "deploying",
    blink: true
  },
  undiscovered: {
    c: "var(--dim2)",
    rank: 3,
    label: "not discovered"
  },
  Queued: {
    c: "var(--idle)",
    rank: 2,
    label: "queued"
  },
  RestoringBackups: {
    c: "var(--info)",
    rank: 1,
    label: "restoring backups",
    blink: true
  },
  PromotingVolumes: {
    c: "var(--info)",
    rank: 1,
    label: "promoting volumes",
    blink: true
  },
  Replicating: {
    c: "var(--info)",
    rank: 1,
    label: "replicating",
    blink: true
  },
  Converged: {
    c: "var(--accent)",
    rank: 1,
    label: "converged"
  },
  MovingWorkloads: {
    c: "var(--info)",
    rank: 1,
    label: "moving workloads",
    blink: true
  },
  MigratingVolumes: {
    c: "var(--info)",
    rank: 1,
    label: "migrating volumes",
    blink: true
  },
  Cleanup: {
    c: "var(--info)",
    rank: 1,
    label: "cleanup",
    blink: true
  },
  Completed: {
    c: "var(--ok)",
    rank: 0,
    label: "completed"
  },
  Paused: {
    c: "var(--warn)",
    rank: 3,
    label: "paused"
  },
  LiveMigrating: {
    c: "var(--info)",
    rank: 1,
    label: "live migrating",
    blink: true
  },
  Restarting: {
    c: "var(--info)",
    rank: 1,
    label: "restarting",
    blink: true
  },
  Moved: {
    c: "var(--ok)",
    rank: 0,
    label: "moved"
  },
  Configured: {
    c: "var(--ok)",
    rank: 0,
    label: "configured"
  },
  Added: {
    c: "var(--ok)",
    rank: 0,
    label: "added"
  },
  Applying: {
    c: "var(--info)",
    rank: 1,
    label: "applying",
    blink: true
  },
  Isolating: {
    c: "var(--info)",
    rank: 1,
    label: "isolating cores",
    blink: true
  },
  Rebooting: {
    c: "var(--warn)",
    rank: 1,
    label: "rebooting",
    blink: true
  },
  Verifying: {
    c: "var(--info)",
    rank: 1,
    label: "verifying",
    blink: true
  },
  Scheduling: {
    c: "var(--info)",
    rank: 1,
    label: "scheduling pod",
    blink: true
  },
  Joining: {
    c: "var(--info)",
    rank: 1,
    label: "joining cluster",
    blink: true
  },
  "Starting SPDK": {
    c: "var(--info)",
    rank: 1,
    label: "starting SPDK",
    blink: true
  },
  Failed: {
    c: "var(--bad)",
    rank: 4,
    label: "failed"
  },
  complete: {
    c: "var(--ok)",
    rank: 0,
    label: "complete"
  },
  Validated: {
    c: "var(--ok)",
    rank: 0,
    label: "Validated"
  },
  Validating: {
    c: "var(--info)",
    rank: 1,
    label: "Validating",
    blink: true
  },
  Deployed: {
    c: "var(--ok)",
    rank: 0,
    label: "Deployed"
  },
  FailedOver: {
    c: "var(--warn)",
    rank: 3,
    label: "Failed over"
  },
  FailingOver: {
    c: "var(--alert)",
    rank: 3,
    label: "Failing over",
    blink: true
  },
  Relocating: {
    c: "var(--info)",
    rank: 2,
    label: "Relocating",
    blink: true
  },
  Relocated: {
    c: "var(--ok)",
    rank: 0,
    label: "Relocated"
  },
  WaitForUser: {
    c: "var(--bad)",
    rank: 4,
    label: "Waiting for operator"
  },
  Fenced: {
    c: "var(--bad)",
    rank: 4,
    label: "Fenced"
  },
  ManuallyFenced: {
    c: "var(--bad)",
    rank: 4,
    label: "Manually fenced"
  },
  Unfenced: {
    c: "var(--ok)",
    rank: 0,
    label: "Unfenced"
  },
  failed: {
    c: "var(--bad)",
    rank: 4,
    label: "failed"
  },
  unavailable: {
    c: "var(--bad)",
    rank: 4,
    label: "unavailable"
  },
  new: {
    c: "var(--idle)",
    rank: 2,
    label: "new"
  },
  removed: {
    c: "var(--dim2)",
    rank: 3,
    label: "removed"
  },
  in_removal: {
    c: "var(--alert)",
    rank: 2,
    label: "in removal",
    blink: true
  },
  in_shutdown: {
    c: "var(--warn)",
    rank: 3,
    label: "in shutdown",
    blink: true
  },
  in_failure: {
    c: "var(--bad)",
    rank: 3,
    label: "failing",
    blink: true
  },
  read_only: {
    c: "var(--ro)",
    rank: 3,
    label: "read-only"
  },
  in_sync: {
    c: "var(--ok)",
    rank: 0,
    label: "in sync"
  },
  catching_up: {
    c: "var(--info)",
    rank: 1,
    label: "catching up",
    blink: true
  },
  paused: {
    c: "var(--warn)",
    rank: 3,
    label: "paused"
  },
  broken: {
    c: "var(--bad)",
    rank: 5,
    label: "broken"
  },
  good: {
    c: "var(--ok)",
    rank: 0,
    label: "healthy"
  },
  warn: {
    c: "var(--warn)",
    rank: 3,
    label: "warning"
  },
  critical: {
    c: "var(--bad)",
    rank: 4,
    label: "critical"
  }
};

// ---- normalizers: wire record -> view model --------------------------------
const REG = {}; // uuid -> view model, warms breadcrumbs
const reg = vm => {
  REG[vm.id] = vm;
  return vm;
};
const ioOf = s => ({
  r: (s || {}).read_io_ps || 0,
  w: (s || {}).write_io_ps || 0
});
const bwOf = s => ({
  r: (s || {}).read_bytes_ps || 0,
  w: (s || {}).write_bytes_ps || 0
});
const histOf = h => ({
  iops: (h || {}).iops || [],
  bw: (h || {}).bytes || []
});
const normCluster = c => reg({
  kind: "cluster",
  id: c.uuid,
  name: c.name,
  mode: c.device_class === "nvme" ? "nvme" : "blockdev",
  siting: c.location_type === "edge" ? "edge" : "datacenter",
  status: c.status,
  rebalancing: !!c.rebalancing,
  capacity: {
    total: c.size_total,
    used: c.size_util
  },
  iops: ioOf(c.io_stats),
  bw: bwOf(c.io_stats),
  hist: histOf(c.io_history),
  counts: {
    hosts: c.hosts_count,
    hostsAvailable: c.hosts_available,
    nodes: c.storage_nodes_count,
    nodesOnline: c.storage_nodes_online,
    devices: c.devices_count,
    devicesOnline: c.devices_online,
    pools: c.pools_count,
    volumes: c.lvols_count,
    snapshots: c.snapshots_count,
    backups: c.backups_count,
    policies: c.backup_policies_count,
    cgroups: c.consistency_groups_count || 0,
    encrypted: c.encrypted_lvols_count || 0,
    migrations: c.migrations_count || 0,
    migrationTargets: c.migration_targets_count || 0,
    buckets: c.buckets_count || 0,
    rwxPvcs: c.rwx_pvcs_count || 0,
    k8sClusters: (c.k8s_cluster_ids || []).length,
    pvcs: c.pvcs_count || 0,
    reduced: c.reduced_lvols_count || 0,
    rpolicies: c.replication_policies_count || 0,
    pairsOut: c.pairs_out_count || 0,
    pairsIn: c.pairs_in_count || 0,
    replicated: c.replicated_lvols_count || 0,
    zones: (c.zone_ids || []).length
  },
  zoneIds: c.zone_ids || [],
  regions: c.regions || [],
  stretched: !!c.stretched,
  drEligible: !!c.dr_target_eligible,
  k8sClusterIds: c.k8s_cluster_ids || [],
  backupEnabled: !!c.backup_enabled,
  s3: c.s3 || null,
  syncReplication: !!c.sync_replication_enabled,
  kms: c.kms || null,
  logicalUsed: c.logical_used || 0,
  fileStorage: c.file_storage || {
    enabled: false
  },
  objectStorage: c.object_storage || {
    enabled: false
  },
  multipathing: !!c.multipathing_enabled,
  multipathNodes: c.multipath_nodes_count || 0,
  autoRebalance: {
    enabled: !!(c.auto_rebalance || {}).enabled,
    moved1h: (c.auto_rebalance || {}).moved_1h || 0,
    moved24h: (c.auto_rebalance || {}).moved_24h || 0
  },
  nodeAffinity: c.node_affinity || "none",
  podAffinity: !!c.pod_affinity_enabled,
  fd: {
    enabled: !!c.failure_domain_enabled,
    scope: c.failure_domain_scope || null,
    domains: c.failure_domains || [],
    unassigned: c.fd_unassigned || 0,
    min: c.fd_min || 0,
    max: c.fd_max || 0,
    balanced: c.fd_balanced !== false,
    thin: c.fd_thin || []
  },
  caps: c.capabilities || {},
  faultBudget: c.fault_budget || {
    kind: "nodes",
    tolerated: c.distr_npcs || 1,
    lost: (c.storage_nodes_count || 0) - (c.storage_nodes_online || 0)
  },
  haType: c.ha_type,
  distrNpcs: c.distr_npcs,
  distrNdcs: c.distr_ndcs,
  version: c.cluster_version,
  mgmt: c.mgmt_endpoint,
  mgmtKind: c.mgmt_endpoint_kind || "control plane API",
  createdAt: c.created_at
});
const normHost = h => reg({
  kind: "host",
  id: h.uuid,
  clusterId: h.cluster_id,
  hostname: h.hostname,
  mgmtIp: h.mgmt_ip,
  zoneId: h.zone_id || null,
  zone: h.zone || null,
  region: h.region || null,
  rack: h.rack_id || null,
  cabinet: h.cabinet_id || null,
  k8sCluster: h.k8s_cluster || null,
  migrationTaint: h.migration_taint || null,
  hostClass: h.host_class || null,
  status: h.status,
  source: h.source || "manual",
  sockets: h.numa_sockets,
  controlPlane: !!h.control_plane,
  kubelet: h.kubelet_version,
  roles: h.roles || [],
  k8sLabels: h.k8s_labels || {},
  inspection: h.inspection || null,
  vcpu: h.vcpu_count,
  memory: h.memory_total,
  memoryPerPod: h.memory_per_pod || null,
  socketsUsed: h.numa_sockets_used || null,
  mgmtNic: h.mgmt_nic || null,
  dataNics: h.data_nics || [],
  nics: (h.nics || []).map(n => ({
    name: n.name,
    mac: n.mac,
    speed: n.speed_gbps,
    address: n.address,
    socket: n.numa_socket,
    state: n.state
  })),
  hugepages: {
    reserved: h.hugepages_reserved,
    allocated: h.hugepages_allocated
  },
  devices: (h.devices || []).map(d => ({
    id: d.id,
    kind: d.kind,
    socket: d.numa_socket,
    pcie: d.pcie_address,
    blockdev: d.device_name,
    serial: d.serial_number,
    model: d.model_number,
    size: d.size,
    assignedNodeId: d.assigned_node_id,
    reserved: !!d.reserved,
    reservedFor: d.reserved_for_node_id
  })),
  nodeIds: h.storage_node_ids || [],
  counts: {
    devices: (h.devices || []).length,
    assigned: h.devices_assigned,
    free: h.devices_free,
    nvme: h.nvme_count,
    blockFree: h.block_free_count,
    nodes: (h.storage_node_ids || []).length
  },
  capacity: {
    total: h.size_total,
    used: h.size_assigned
  },
  preparedAt: h.prepared_at,
  labels: h.labels || {}
});
// A discovery run: the inventory pass, and the filter it ran with. The filter
// belongs here rather than in the wizard because it decides what was even
// reported — a boot device excluded at discovery never reaches a node set.
const normDiscovery = d => ({
  kind: "discovery",
  id: d.uuid,
  k8sClusterId: d.k8s_cluster_id,
  status: d.status,
  step: d.step || null,
  opName: d.op_name || null,
  startedAt: d.started_at,
  finishedAt: d.finished_at,
  nodeSelector: (d.node_selector || {}).matchLabels || {},
  filter: d.device_filter || {},
  hostIds: d.host_ids || [],
  counts: {
    nodes: d.node_count,
    devices: d.device_count,
    filtered: d.filtered_count
  }
});

// ClusterDeploymentConfig — the document that is reviewed and approved. One
// group per NUMA socket, because one storage node runs per socket.
const normDeployConfig = o => {
  const sp = o.spec || {},
    st = o.status || {};
  const set = (sp.nodeSets || [])[0] || {};
  const groups = (set.groups || []).map(g => ({
    node: g.node,
    socket: g.numaSocket,
    nvme: (g.devices || {}).nvme || [],
    block: (g.devices || {}).block || [],
    mgmtNic: g.mgmtInterface,
    dataNics: g.dataInterfaces || [],
    maxSubsystems: (g.sizing || {}).maxSubsystemCount,
    vcpu: (g.sizing || {}).vcpuCount,
    hugepages: (g.sizing || {}).minHugePagesSize,
    systemMemory: (g.sizing || {}).systemMemory,
    coreIsolation: !!g.coreIsolation,
    failureDomain: g.failureDomain
  }));
  const steps = (st.steps || []).map(s => ({
    name: s.name,
    label: s.label,
    phase: s.phase,
    message: s.message,
    startedAt: s.startedAt,
    finishedAt: s.finishedAt,
    progress: s.progress
  }));
  const cl = sp.cluster || {};
  return reg({
    kind: "deployconfig",
    id: o.metadata.uid,
    name: o.metadata.name,
    createdAt: o.metadata.creationTimestamp,
    approved: !!sp.approved,
    status: st.phase || "Draft",
    message: st.message || null,
    environment: sp.environment || null,
    k8sClusterId: st.kubernetesClusterId || null,
    k8sClusterName: sp.kubernetesClusterRef || null,
    clusterId: st.clusterId || null,
    discoveryId: st.discoveryRef || null,
    hostIds: st.hostIds || [],
    nodeSelector: (sp.nodeSelector || {}).matchLabels || {},
    filter: cl.deviceFilter || sp.deviceFilter || {},
    cluster: sp.cluster || null,
    hosts: set.nodes || [],
    groups,
    steps,
    nodes: (st.nodes || []).map(n => ({
      host: n.host,
      hostId: n.hostId,
      phase: n.phase,
      message: n.message,
      progress: n.progress || 0,
      storageNodeIds: n.storageNodeIds || []
    })),
    log: (st.log || []).map(l => ({
      ts: l.ts,
      level: l.level,
      step: l.step,
      node: l.node,
      msg: l.msg
    })),
    counts: {
      hosts: (set.nodes || []).length,
      nodes: groups.length,
      devices: groups.reduce((n, g) => n + g.nvme.length + g.block.length, 0)
    },
    sizing: {
      hugepages: groups.reduce((n, g) => n + (g.hugepages || 0), 0),
      vcpu: groups.reduce((n, g) => n + (g.vcpu || 0), 0),
      coreIsolation: groups.some(g => g.coreIsolation),
      maxSubsystems: groups.length ? groups[0].maxSubsystems : null
    }
  });
};
const normNode = n => reg({
  kind: "node",
  id: n.uuid,
  clusterId: n.cluster_id,
  hostId: n.host_id,
  zoneId: n.zone_id || null,
  hostname: n.hostname,
  ip: n.data_nics && n.data_nics[0] ? n.data_nics[0].ip : null,
  port: n.data_nics && n.data_nics[0] ? n.data_nics[0].port : 4420,
  dataNics: (n.data_nics || []).map(x => ({
    name: x.name,
    ip: x.ip,
    port: x.port,
    socket: x.numa_socket,
    state: x.state
  })),
  multipath: (n.data_nics || []).length > 1,
  op: n.op ? {
    kind: n.op.kind,
    phase: n.op.phase,
    phaseIndex: n.op.phase_index || 0,
    phases: n.op.phases || [],
    volumesMoved: n.op.volumes_moved || 0,
    targetHostname: n.op.target_hostname || null,
    sourceHostname: n.op.source_hostname || null,
    startedAt: n.op.started_at,
    taskId: n.op.task_id
  } : null,
  mgmtIp: n.mgmt_ip,
  failureDomain: n.failure_domain,
  physicalLabel: n.physical_label,
  status: n.status,
  capacity: {
    total: n.size_total,
    used: n.size_util
  },
  iops: ioOf(n.io_stats),
  bw: bwOf(n.io_stats),
  hist: histOf(n.io_history),
  counts: {
    devices: n.devices_count,
    devicesOnline: n.devices_online
  },
  cpuCount: n.cpu_count,
  cpuReserved: n.vcpu_reserved,
  maxSubsystems: n.max_subsystem_count || null,
  memory: {
    total: n.memory_total,
    reserved: n.memory_reserved,
    used: n.memory_used
  },
  hugepages: {
    total: n.hugepages_total,
    used: n.hugepages_used
  },
  spdk: n.spdk_version
});
const normDevice = d => reg({
  kind: "device",
  id: d.uuid,
  nodeId: d.node_id,
  clusterId: d.cluster_id,
  hostId: d.host_id,
  mode: d.cluster_device_class === "nvme" ? "nvme" : "blockdev",
  socket: d.numa_socket,
  serial: d.serial_number,
  pcie: d.pcie_address,
  blockdev: d.device_name,
  model: d.model_number,
  firmware: d.firmware_revision,
  status: d.status,
  health: d.health_check,
  lastHealthCheck: d.last_health_check,
  capacity: {
    total: d.size_total,
    used: d.size_util
  },
  iops: ioOf(d.io_stats),
  bw: bwOf(d.io_stats),
  hist: histOf(d.io_history),
  temp: d.temperature_c,
  wear: d.percentage_used,
  poweronHours: d.power_on_hours
});
const normPool = p => reg({
  kind: "pool",
  id: p.uuid,
  clusterId: p.cluster_id,
  name: p.pool_name,
  dhchap: !!p.dhchap_bidirectional,
  // a pool is never offline: it is enabled or disabled. Disabled keeps serving I/O
  // but refuses new volume provisioning.
  status: p.enabled === false ? "disabled" : "enabled",
  enabled: p.enabled !== false,
  qos: p.qos,
  capacity: {
    total: p.size_prov,
    used: p.size_util
  },
  lvolBytes: p.lvols_bytes || 0,
  snapshotBytes: p.snapshots_bytes || 0,
  storageClasses: p.storage_classes || [],
  k8sClusterId: p.k8s_cluster_id || null,
  counts: {
    volumes: p.lvols_count,
    volumesOnline: p.lvols_online,
    snapshots: p.snapshots_count,
    backups: p.backups_count,
    storageClasses: p.storage_classes_count || 0
  }
});
const normVolume = v => reg({
  kind: "volume",
  id: v.uuid,
  poolId: v.pool_id,
  poolName: v.pool_name,
  clusterId: v.cluster_id,
  name: v.lvol_name,
  status: v.status,
  nodes: v.nodes || {},
  capacity: {
    total: v.size_prov,
    used: v.size_util
  },
  iops: ioOf(v.io_stats),
  bw: bwOf(v.io_stats),
  hist: histOf(v.io_history),
  crypto: !!v.crypto_enabled,
  qos: v.qos,
  nqn: v.nqn,
  dataReduction: !!v.compression_dedup_enabled,
  logicalUsed: v.logical_used || v.size_util,
  baseSnapshot: v.base_snapshot || null,
  // a volume can belong to several consistency groups at once
  consistencyGroups: v.consistency_groups || [],
  pvc: v.pvc || null,
  bucket: v.bucket || null,
  affinity: v.affinity || null,
  backupPolicy: v.backup_policy || null,
  replication: v.replication ? {
    policyId: v.replication.policy_id,
    policyName: v.replication.policy_name,
    mode: v.replication.mode,
    status: v.replication.status,
    lastAt: v.replication.last_replication_at,
    backlog: v.replication.backlog_bytes,
    consistencyGroup: v.replication.consistency_group,
    generations: v.replication.generations
  } : null,
  migration: v.migration || null,
  counts: {
    snapshots: v.snapshots_count || 0,
    backups: v.backups_count || 0,
    snapshotsBackedUp: v.snapshots_backed_up || 0,
    backupVersions: v.backup_versions_count || 0
  },
  backupChainId: v.backup_chain_id || null,
  createdAt: v.created_at
});
const normSnapshot = s => reg({
  kind: "snapshot",
  id: s.uuid,
  clusterId: s.cluster_id,
  poolId: s.pool_id,
  poolName: s.pool_name,
  volumeId: s.lvol_id,
  volumeName: s.lvol_name,
  name: s.snapshot_name,
  status: s.status || "online",
  seq: s.seq,
  parentId: s.parent_id || null,
  backupVersionId: s.backup_version_id || null,
  createdAt: s.created_at,
  capacity: {
    total: s.size,
    used: s.size
  }
});
const normBackup = b => reg({
  kind: "backup",
  id: b.uuid,
  clusterId: b.cluster_id,
  poolId: b.pool_id,
  poolName: b.pool_name,
  volumeId: b.lvol_id,
  volumeName: b.lvol_name,
  name: b.lvol_name,
  chainId: b.chain_id,
  policyId: b.policy_id || null,
  policyName: b.policy_name || null,
  status: b.status || "online",
  bucket: b.bucket,
  exportedTo: b.exported_to || null,
  exportedVersion: b.exported_version || null,
  createdAt: b.earliest_at || b.created_at,
  latestAt: b.latest_at,
  lastMergeAt: b.last_merge_at || null,
  versions: (b.versions || []).map(v => ({
    id: v.id,
    seq: v.seq,
    tier: v.tier,
    type: v.type,
    createdAt: v.created_at,
    size: v.size,
    snapshotId: v.source_snapshot_id,
    snapshotName: v.source_snapshot_name,
    merged: v.merged_count || 0
  })),
  counts: {
    versions: b.versions_count || 0,
    merged: b.merged_total || 0
  },
  fullBytes: b.full_bytes || 0,
  deltaBytes: b.delta_bytes || 0,
  capacity: {
    total: b.size || 0,
    used: b.size || 0
  }
});
const normPolicy = p => reg({
  kind: "policy",
  id: p.uuid,
  clusterId: p.cluster_id,
  name: p.policy_name,
  status: "active",
  consistencyGroup: !!p.consistency_group,
  schedule: (p.schedule || []).map(r => ({
    interval: r.interval,
    versions: r.versions,
    online: r.online || 0
  })),
  counts: {
    volumes: p.lvols_count || 0,
    chains: p.chains_count || 0,
    versions: p.versions_total || 0,
    online: p.online_snapshots || 0
  },
  finest: p.finest_interval || null,
  createdAt: p.created_at,
  capacity: {
    total: 0,
    used: 0
  }
});
// ReplicationPair — reusable {sourceCluster, targetCluster}. targetCluster is
// immutable after creation, so a pair is never edited, only replaced.
const normReplPair = (o, pols, slots) => {
  const s = o.spec || {},
    st = o.status || {};
  const mine = (pols || []).filter(p => (p.spec || {}).pairRef === o.metadata.name);
  const names = mine.map(p => p.metadata.name);
  const mySlots = (slots || []).filter(x => names.includes((x.spec || {}).policyRef));
  return reg({
    kind: "pair",
    id: o.metadata.uid,
    name: o.metadata.name,
    crdKind: "ReplicationPair",
    shortName: "relpair",
    sourceCluster: s.sourceCluster,
    targetCluster: s.targetCluster,
    ready: !!st.ready,
    backendTargetId: st.backendTargetID || null,
    message: st.message || "",
    activeOpsRef: st.activeOpsRef || null,
    conditions: st.conditions || [],
    status: st.ready ? "online" : "degraded",
    counts: {
      policies: mine.length,
      slots: mySlots.length,
      failedOver: mySlots.filter(x => (x.status || {}).state === "failed_over").length,
      errored: mySlots.filter(x => (x.status || {}).state === "error").length
    },
    capacity: {
      total: 0,
      used: 0
    },
    createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationPolicy — one interval, one snapshot count. mode is exactly
// failover | migration; there is no synchronous mode and no tiered retention.
const normReplPolicy = (o, slots, pairs) => {
  const s = o.spec || {},
    st = o.status || {};
  const mine = (slots || []).filter(x => (x.spec || {}).policyRef === o.metadata.name).map(x => x.status || {});
  const pair = (pairs || []).find(p => p.metadata.name === s.pairRef);
  // the fixtures' clock, not the wall clock — otherwise every slot reads stale
  const nowMs = window.SB_NOW || Date.now();
  const late = mine.filter(x => {
    if (x.state !== "replicating" || !x.lastReplicatedAt) return false;
    return (nowMs - Date.parse(x.lastReplicatedAt)) / 60000 > ivMinutes(s.interval) * 2;
  }).length;
  return reg({
    kind: "rpolicy",
    id: o.metadata.uid,
    name: o.metadata.name,
    crdKind: "ReplicationPolicy",
    shortName: "repl",
    pairRef: s.pairRef,
    pairId: pair ? pair.metadata.uid : null,
    sourceCluster: pair ? (pair.spec || {}).sourceCluster : null,
    targetCluster: pair ? (pair.spec || {}).targetCluster : null,
    mode: s.mode || "failover",
    interval: s.interval || "5m",
    snapshotRetention: s.snapshotRetention == null ? 3 : s.snapshotRetention,
    ready: !!st.ready,
    backendPolicyId: st.backendPolicyID || null,
    activeOpsRef: st.activeOpsRef || null,
    conditions: st.conditions || [],
    message: (st.conditions || []).map(c => c.message).filter(Boolean)[0] || "",
    counts: {
      slots: st.slotCount != null ? st.slotCount : mine.length,
      replicating: mine.filter(x => x.state === "replicating").length,
      failedOver: mine.filter(x => x.state === "failed_over").length,
      errored: mine.filter(x => x.state === "error").length,
      cutoverPending: mine.filter(x => x.state === "cutover_pending").length,
      late
    },
    lastAt: mine.map(x => x.lastReplicatedAt).filter(Boolean).sort().slice(-1)[0] || null,
    status: !st.ready ? "degraded" : mine.some(x => x.state === "error") ? "unhealthy" : late ? "degraded" : "online",
    capacity: {
      total: 0,
      used: 0
    },
    createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationSlot — one per PVC, created by the operator from the annotation and
// owned by the PVC. Never created or deleted from here.
const normReplSlot = o => {
  const s = o.spec || {},
    st = o.status || {};
  const own = (o.metadata.ownerReferences || [])[0] || {};
  return reg({
    kind: "slot",
    id: o.metadata.uid,
    name: o.metadata.name,
    crdKind: "ReplicationSlot",
    shortName: "relslot",
    policyRef: s.policyRef,
    pvcRef: s.pvcRef,
    volumeId: s.volumeID,
    // <clusterUUID>:<poolUUID>:<volumeUUID>
    volumeParts: String(s.volumeID || "").split(":"),
    state: st.state || "replicating",
    status: st.state || "replicating",
    direction: st.direction || "source",
    sourceLvolId: st.sourceLvolID || null,
    targetLvolId: st.targetLvolID || null,
    targetNqn: st.targetNQN || null,
    lastAt: st.lastReplicatedAt || null,
    message: st.message || "",
    conditions: st.conditions || [],
    ownedBy: own.kind ? `${own.kind}/${own.name}` : null,
    capacity: {
      total: 0,
      used: 0
    },
    createdAt: o.metadata.creationTimestamp
  });
};
// ReplicationOps — one-shot. A terminal op is never re-run; a correction needs
// a new one.
const normReplOps = o => {
  const s = o.spec || {},
    st = o.status || {};
  return reg({
    kind: "replops",
    id: o.metadata.uid,
    name: o.metadata.name,
    crdKind: "ReplicationOps",
    shortName: "replops",
    action: s.action,
    scope: s.scope,
    ref: s.ref,
    sourceClusterId: s.sourceClusterID || null,
    deleteSource: !!s.deleteSource,
    phase: st.phase || "Pending",
    subphase: st.subphase || null,
    message: st.message || "",
    startedAt: st.startedAt || null,
    completedAt: st.completedAt || null,
    results: (st.results || []).map(r => ({
      slotRef: r.slotRef,
      status: r.status,
      detail: r.detail || null,
      targetLvolId: r.targetLvolID || null
    })),
    terminal: ["Succeeded", "Failed"].includes(st.phase),
    status: st.phase === "Succeeded" ? "online" : st.phase === "Failed" ? "unhealthy" : st.phase === "Running" ? "activating" : "idle",
    capacity: {
      total: 0,
      used: 0
    },
    createdAt: o.metadata.creationTimestamp
  });
};
const ivMinutes = iv => {
  const m = /^(\d+(?:\.\d+)?)\s*([smhdw]?)$/.exec(String(iv || "").trim());
  if (!m) return 5;
  const n = parseFloat(m[1]),
    u = m[2] || "m";
  return Math.max(1, Math.round(u === "s" ? n / 60 : u === "h" ? n * 60 : u === "d" ? n * 1440 : u === "w" ? n * 10080 : n));
};
const normPair = p => reg({
  kind: "pair",
  id: p.uuid,
  sourceClusterId: p.source_cluster_id,
  targetClusterId: p.target_cluster_id,
  status: p.state,
  link: p.link || {},
  lastHandshakeAt: p.last_handshake_at,
  createdAt: p.created_at,
  counts: {
    policies: p.policies_count || 0
  },
  capacity: {
    total: 0,
    used: 0
  },
  name: null
});
const normDrPolicy = p => reg({
  kind: "rpolicy",
  id: p.uuid,
  name: p.name,
  mode: p.mode,
  pairId: p.pair_id,
  sourceClusterId: p.source_cluster_id,
  targetClusterId: p.target_cluster_id,
  zoneIds: p.zone_ids || null,
  frequency: p.frequency_minutes,
  retention: p.retention || [],
  // An asynchronous DR policy names a consistency group and carries no schedule
  // of its own: the frequency and retention below are the group's.
  cgId: p.cg_id || null,
  cgName: p.cg_name || null,
  consistencyGroup: !!p.cg_id,
  failback: p.failback || {},
  status: p.state,
  lastAt: p.last_replication_at,
  backlog: p.backlog_bytes,
  generations: p.generations_kept,
  createdAt: p.created_at,
  lastFailoverAt: p.last_failover_at,
  failoverMode: p.failover_mode || null,
  lastTestAt: p.last_test_at,
  drClusterIds: p.dr_cluster_ids || [],
  replicationClass: p.replication_class || null,
  appDrStatus: p.app_dr_status || "Unavailable",
  counts: {
    volumes: p.lvols_count !== undefined ? p.lvols_count : (p.lvol_ids || []).length,
    apps: p.apps_count || 0,
    pvcs: p.pvcs_count || 0,
    unhealthy: p.unhealthy_apps || 0
  },
  capacity: {
    total: 0,
    used: 0
  }
});
const normCg = g => reg({
  kind: "cgroup",
  id: g.uuid,
  clusterId: g.cluster_id,
  name: g.name,
  status: g.status || "online",
  memberIds: g.lvol_ids || [],
  capacity: {
    total: g.size_prov || 0,
    used: g.size_util || 0
  },
  counts: {
    volumes: g.lvols_count || 0,
    snapshots: g.snapshots_count || 0,
    backedUp: g.backed_up_count || 0,
    apps: g.apps_count || 0
  },
  // A group can own its protection: attaching a policy here applies it to every
  // member, and members added later inherit it.
  // Once a group carries protection its membership is fixed: the retained
  // versions and the replica stream are defined against exactly this set.
  locked: !!g.locked,
  backupPolicy: g.backup_policy || null,
  // The group owns the replication cadence; DR policies just name the group.
  replicationConfig: g.replication_config ? {
    frequency: g.replication_config.frequency_minutes,
    retention: g.replication_config.retention || []
  } : null,
  drPolicyIds: g.dr_policy_ids || [],
  replication: g.replication_config ? {
    status: g.replication_status || "healthy",
    lastAt: g.replication_last_at,
    backlog: g.replication_backlog_bytes || 0
  } : null,
  createdAt: g.created_at
});
const normCgSnap = s => reg({
  kind: "cgsnapshot",
  id: s.uuid,
  clusterId: s.cluster_id,
  cgId: s.cg_id,
  cgName: s.cg_name,
  name: s.snapshot_name,
  status: s.status || "online",
  createdAt: s.created_at,
  members: (s.members || []).map(m => ({
    volumeId: m.lvol_id,
    volumeName: m.lvol_name,
    snapshotId: m.snapshot_id,
    size: m.size
  })),
  backupVersionId: s.backup_version_id || null,
  bucket: s.backup_bucket || null,
  capacity: {
    total: s.size || 0,
    used: s.size || 0
  },
  counts: {
    volumes: (s.members || []).length
  }
});
const normMigration = m => reg({
  kind: "migration",
  id: m.uuid,
  clusterId: m.cluster_id,
  name: m.name,
  mode: m.mode,
  scope: m.scope,
  status: m.state,
  sourceClusterId: m.source_cluster_id,
  targetClusterId: m.target_cluster_id,
  targetZoneId: m.target_zone_id,
  targetTaint: m.target_taint,
  followWorkload: !!m.follow_workload,
  memberIds: m.lvol_ids || [],
  counts: {
    volumes: m.lvols_count || 0,
    moved: m.moved_count || 0
  },
  progress: m.progress_pct || 0,
  readyToCutover: !!m.ready_to_cutover,
  iterations: m.iterations || 0,
  iterationLimit: m.iteration_limit || 0,
  firstSnapshot: m.first_snapshot_bytes || 0,
  lastSnapshot: m.last_snapshot_bytes || 0,
  freezeThreshold: m.freeze_threshold_bytes || 0,
  freezeMs: m.estimated_freeze_ms || 0,
  throughput: m.throughput_bytes_ps || 0,
  startedAt: m.started_at,
  completedAt: m.completed_at,
  frozenAt: m.frozen_at,
  error: m.error || null,
  capacity: {
    total: 0,
    used: 0
  },
  createdAt: m.started_at
});
const normK8s = k => reg({
  kind: "k8sc",
  id: k.uuid,
  name: k.name,
  version: k.version,
  status: k.status,
  discovered: !!k.discovered,
  discoveredAt: k.discovered_at || null,
  endpoint: k.api_endpoint,
  environment: k.environment,
  csi: {
    version: k.csi_version,
    status: k.csi_status
  },
  operatorNamespace: k.operator_namespace || null,
  zoneIds: k.zone_ids || [],
  storageClusterIds: k.storage_cluster_ids || [],
  namespaces: k.namespaces || [],
  counts: {
    storageClasses: k.storage_classes_count || 0,
    pvcs: k.pvcs_count || 0,
    bound: k.pvcs_bound || 0,
    workers: k.worker_nodes_count || 0,
    prepared: k.prepared_hosts_count || 0,
    zones: (k.zone_ids || []).length,
    storageClusters: (k.storage_cluster_ids || []).length,
    protectedApps: k.protected_apps_count || 0
  },
  drClusterId: k.dr_cluster_id || null,
  capacity: {
    total: k.provisioned_bytes || 0,
    used: k.provisioned_bytes || 0
  },
  createdAt: k.created_at
});
const normSc = s => reg({
  kind: "storageclass",
  id: s.uuid,
  k8sClusterId: s.k8s_cluster_id,
  name: s.name,
  provisioner: s.provisioner,
  clusterId: s.cluster_id,
  poolId: s.pool_id,
  poolName: s.pool_name,
  operatorNamespace: s.operator_namespace || null,
  storagePoolRef: s.storage_pool_ref || null,
  variant: s.variant || "default",
  parameters: s.parameters || {},
  specParameters: s.spec_parameters || {},
  zoneClusterMap: s.zone_cluster_map || {},
  regionClusterMap: s.region_cluster_map || {},
  dhchap: !!s.dhchap,
  allowedTopology: s.allowed_topology || null,
  reclaim: s.reclaim_policy,
  binding: s.volume_binding_mode,
  expansion: !!s.allow_volume_expansion,
  isDefault: !!s.is_default,
  status: "active",
  counts: {
    pvcs: s.pvcs_count || 0,
    bound: s.bound_count || 0
  },
  capacity: {
    total: s.provisioned_bytes || 0,
    used: s.provisioned_bytes || 0
  },
  createdAt: s.created_at
});
const normBucket = b => reg({
  kind: "bucket",
  id: b.uuid,
  clusterId: b.cluster_id,
  name: b.name,
  volumeId: b.lvol_id,
  volumeName: b.lvol_name,
  poolId: b.pool_id,
  poolName: b.pool_name,
  status: b.status || "online",
  versioning: !!b.versioning,
  objectLock: !!b.object_lock,
  encrypted: !!b.encrypted,
  quota: b.quota_bytes || 0,
  objects: b.objects || 0,
  access: b.access || {},
  replication: b.replication || null,
  // S3 metadata — what a client sets, what the console searches by
  region: b.region || null,
  storageClass: b.storage_class || "standard",
  owner: b.owner || null,
  tags: b.tags || {},
  lifecycle: b.lifecycle_rules || [],
  cors: !!b.cors_enabled,
  counts: {
    snapshots: b.snapshots_count || 0,
    backups: b.backups_count || 0,
    tags: Object.keys(b.tags || {}).length
  },
  capacity: {
    total: b.provisioned_bytes || 0,
    used: b.size_bytes || 0
  },
  createdAt: b.created_at
});
const normMPath = p => reg({
  kind: "mpath",
  id: p.uuid,
  name: p.name,
  status: p.status,
  k8sClusterId: p.k8s_cluster_id,
  sourceClusterId: p.source_cluster_id,
  targetClusterId: p.target_cluster_id,
  sourceZoneId: p.source_zone_id || null,
  targetZoneId: p.target_zone_id || null,
  queue: p.queue || [],
  log: (p.log || []).map(l => ({
    ts: l.ts,
    level: l.level,
    groupId: l.group_id,
    msg: l.msg
  })),
  createdAt: p.created_at,
  capacity: {
    total: 0,
    used: 0
  },
  counts: {
    groups: (p.queue || []).length
  }
});
const normAppGroup = g => reg({
  kind: "appgroup",
  id: g.uuid,
  pathId: g.path_id,
  name: g.name,
  namespace: g.namespace,
  status: g.phase,
  phase: g.phase,
  message: g.message || null,
  approval: g.approval || "manual",
  order: g.order || 0,
  members: (g.members || []).map(m => ({
    kind: m.kind,
    name: m.name,
    state: m.state || "Pending",
    progress: m.progress || 0
  })),
  pvcIds: g.pvc_ids || [],
  memberIds: g.lvol_ids || [],
  rpolicyId: g.rpolicy_id || null,
  volumes: (g.volumes || []).map(v => ({
    id: v.lvol_id,
    name: v.lvol_name,
    size: v.size || 0,
    backlog: v.backlog_bytes || 0,
    lastAt: v.last_replication_at,
    progress: v.progress || 0,
    migrated: !!v.migrated
  })),
  backlog: (g.volumes || []).reduce((n, v) => n + (v.backlog_bytes || 0), 0),
  counts: {
    members: (g.members || []).length,
    vms: (g.members || []).filter(m => m.kind === "VirtualMachine").length,
    volumes: (g.lvol_ids || []).length,
    moved: (g.members || []).filter(m => m.state === "Moved").length,
    migrated: (g.volumes || []).filter(v => v.migrated).length
  },
  capacity: {
    total: (g.volumes || []).reduce((n, v) => n + (v.size || 0), 0),
    used: 0
  },
  createdAt: g.created_at,
  startedAt: g.started_at,
  finishedAt: g.finished_at
});
const normPvc = p => reg({
  kind: "pvc",
  id: p.uuid,
  k8sClusterId: p.k8s_cluster_id,
  namespace: p.namespace,
  name: p.pvc_name,
  storageClassId: p.storage_class_id,
  storageClass: p.storage_class,
  volumeId: p.lvol_id || null,
  status: p.status,
  accessMode: p.access_mode,
  volumeMode: p.volume_mode,
  filesystem: p.filesystem || null,
  workload: p.workload,
  workloadKind: p.workload_kind,
  annotations: p.annotations || {},
  labels: p.labels || {},
  capacity: {
    total: p.actual_bytes || p.requested_bytes || 0,
    used: p.requested_bytes || 0
  },
  requested: p.requested_bytes || 0,
  createdAt: p.created_at
});
const normDrCluster = d => reg({
  kind: "drcluster",
  id: d.uuid,
  k8sClusterId: d.k8s_cluster_id,
  name: d.name,
  region: d.region,
  status: d.status,
  fencing: d.fencing_state,
  ramen: d.ramen_version,
  s3: {
    profile: d.s3_profile_name,
    endpoint: d.s3_endpoint,
    bucket: d.s3_bucket
  },
  lastHeartbeatAt: d.last_heartbeat_at,
  createdAt: d.created_at,
  counts: {
    apps: d.apps_count || 0,
    standby: d.standby_apps_count || 0,
    policies: d.policies_count || 0
  },
  capacity: {
    total: 0,
    used: 0
  }
});
// A protection plan: the sites it spans, the storage profile, and the methods
// it declares. Everything below it — DRCluster, DRPolicy, DRPC, the class
// matrix — is derived, never authored.
const CLS_NAME = m => m.type === "sync" ? "sb-sync-" + m.name : m.type === "async" ? "sb-async-" + m.interval : "sb-vault-" + m.interval;
const normMethod = (m, plan) => ({
  name: m.name,
  type: m.type,
  target: m.target,
  interval: m.interval,
  classInterval: m.class_interval,
  // one top-layer field written to two places: DRPolicy.spec.schedulingInterval
  // and the class's parameters.schedulingInterval. If they differ by even
  // formatting, no class resolves, the policy still validates cleanly, and the
  // application is protected by nothing.
  intervalConsistent: m.type === "sync" || m.class_interval === m.interval,
  retention: m.retention || null,
  immutable: !!m.immutable,
  bucket: m.bucket || null,
  cls: (plan.classes || []).find(c => c.name === CLS_NAME(m)) || null,
  rpo: m.type === "sync" ? "0s" : m.interval,
  failback: m.type !== "snapshot-s3",
  generationSelect: m.type === "snapshot-s3"
});
const normPlan = p => reg({
  kind: "plan",
  id: p.uuid,
  name: p.name,
  storageProfile: p.storage_profile,
  siteNames: p.site_names || [],
  siteIds: p.site_ids || [],
  methods: (p.methods || []).map(m => normMethod(m, p)),
  classes: (p.classes || []).map(c => ({
    name: c.name,
    crdKind: c.kind,
    replicationId: c.replication_id,
    storageId: c.storage_id,
    provisioner: c.provisioner,
    labels: c.labels || {},
    parameters: c.parameters || {}
  })),
  policies: (p.policies || []).map(x => ({
    name: x.name,
    drClusters: x.dr_clusters,
    schedulingInterval: x.scheduling_interval,
    selector: x.replication_class_selector || {},
    methodName: x.method_name,
    peerClass: x.peer_class || null,
    validated: x.validated
  })),
  status: p.health || "healthy",
  protectionGap: !!p.protection_gap,
  worstLagSeconds: p.worst_lag_seconds || 0,
  counts: {
    apps: p.apps_count || 0,
    pvcs: p.pvcs_count || 0,
    methods: p.methods_count || 0,
    sites: p.sites_count || 0,
    policies: p.policies_count || 0,
    unvalidated: p.unvalidated_count || 0,
    generations: p.generations_total || 0
  },
  capacity: {
    total: 0,
    used: 0
  },
  createdAt: p.created_at
});
// A site is one managed cluster. Its name must equal the OCM ManagedCluster
// name, and its region is how sync versus async is declared: equal region means
// the pair can mirror synchronously, distinct region cannot.
const normSite = s => reg({
  kind: "site",
  id: s.uuid,
  name: s.name,
  k8sClusterId: s.k8s_cluster_id,
  region: s.region,
  s3Profile: s.s3_profile_name,
  s3Endpoint: s.s3_endpoint,
  s3Bucket: s.s3_bucket,
  cidrs: s.cidrs || [],
  fencing: s.fencing_state,
  status: s.status,
  ramen: s.ramen_version,
  discoveredClasses: s.discovered_classes || [],
  counts: {
    activeApps: s.active_apps_count || 0,
    standbyApps: s.standby_apps_count || 0,
    plans: s.plans_count || 0,
    classes: (s.discovered_classes || []).length
  },
  capacity: {
    total: 0,
    used: 0
  },
  createdAt: s.onboarded_at
});
const normGeneration = g => ({
  generation: g.generation,
  tier: g.tier,
  at: g.at,
  size: g.size_bytes,
  integrity: g.integrity_state,
  lockedUntil: g.locked_until || null,
  consistencyGroup: g.consistency_group || null,
  kind: g.kind,
  verifiedAt: g.verified_at || null
});
const normLeg = l => ({
  id: l.leg_id,
  method: l.method,
  type: l.type,
  target: l.target,
  state: l.state,
  lastAt: l.last_sync_at,
  lagSeconds: l.lag_seconds,
  epoch: l.epoch,
  bytesLastCycle: l.bytes_last_cycle,
  status: l.health,
  orchestrated: !!l.orchestrated,
  note: l.note || null,
  generations: l.generations || 0,
  oldestAt: l.oldest_generation_at || null,
  newestAt: l.newest_generation_at || null,
  lockedUntil: l.locked_until || null
});
const normArbitration = t2 => ({
  holder: t2.token_holder,
  generation: t2.generation,
  quorumAck: (t2.quorum_ack || []).map(q => ({
    site: q.site,
    ackedAt: q.acked_at
  })),
  fencedSites: t2.fenced_sites || [],
  quorumSize: t2.quorum_size,
  updatedAt: t2.updated_at
});
const normApp = a2 => reg({
  kind: "protectedapp",
  id: a2.uuid,
  name: a2.app_name,
  namespace: a2.namespace,
  appKind: a2.app_kind,
  policyId: a2.policy_id,
  policyName: a2.policy_name,
  preferredClusterId: a2.preferred_cluster_id,
  failoverClusterId: a2.failover_cluster_id,
  selector: a2.pvc_selector || {},
  phase: a2.phase,
  progression: a2.progression,
  action: a2.action,
  status: a2.health || "healthy",
  rpoMet: a2.rpo_met !== false,
  vrgState: a2.vrg_state,
  lastSyncAt: a2.last_group_sync_at,
  lastSyncDuration: a2.last_group_sync_duration_s,
  lastSyncBytes: a2.last_group_sync_bytes,
  kubeObjectProtection: !!a2.kube_object_protection,
  recipe: a2.recipe || null,
  planId: a2.plan_id || null,
  planName: a2.plan_name || null,
  storageProfile: a2.storage_profile || null,
  preferredSite: a2.preferred_site || null,
  activeSite: a2.active_site || null,
  preferredSiteId: a2.preferred_site_id || null,
  activeSiteId: a2.active_site_id || null,
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
  failoverTargets: a2.failover_targets || [],
  restoreTargets: a2.restore_targets || [],
  backupPolicyId: a2.backup_policy_id || null,
  backupPolicyName: a2.backup_policy_name || null,
  cgId: a2.cg_id || null,
  cgName: a2.cg_name || null,
  // Protection lag rolled up over the group: the oldest member's last successful
  // cycle, and the data written since then.
  group: {
    lastAt: a2.group_last_at || null,
    writtenSince: a2.group_written_since || 0,
    lagSeconds: a2.group_lag_seconds,
    volumes: (a2.volumes || []).map(v => ({
      id: v.lvol_id,
      name: v.lvol_name,
      size: v.size,
      lastAt: v.last_at,
      writtenSince: v.written_since,
      status: v.status
    }))
  },
  counts: {
    pvcs: a2.pvcs_count || 0
  },
  memberIds: a2.pvc_ids || [],
  capacity: {
    total: 0,
    used: 0
  },
  createdAt: a2.created_at
});
const normZone = s => reg({
  kind: "zone",
  id: s.uuid,
  name: s.name,
  location: s.location,
  region: s.region,
  status: "active",
  region: s.region,
  label: s.label,
  regionLabel: s.region_label,
  createdAt: s.created_at,
  clusterIds: s.cluster_ids || [],
  racks: s.racks || [],
  k8sClusterIds: s.k8s_cluster_ids || [],
  k8sClusters: s.k8s_clusters || [],
  untaintedHosts: s.untainted_hosts || 0,
  counts: {
    hosts: s.hosts_count || 0,
    hostsPrepared: s.hosts_prepared || 0,
    nvme: s.nvme_hosts || 0,
    nodes: s.nodes_count || 0,
    clusters: (s.cluster_ids || []).length
  },
  capacity: {
    total: s.size_total || 0,
    used: 0
  }
});
const normTask = t => ({
  kind: "task",
  id: t.uuid,
  clusterId: t.cluster_id,
  parentId: t.parent_id,
  fn: t.function_name,
  target: t.target_id,
  nodeId: t.node_id,
  distrib: t.distrib,
  retry: t.retry,
  maxRetry: t.max_retry,
  status: t.status,
  result: t.result,
  canceled: !!t.canceled,
  subtaskTotal: t.subtask_total || 0,
  createdAt: t.created_at,
  updatedAt: t.updated_at
});
const normLog = l => ({
  id: l.uuid,
  ts: l.ts,
  level: l.level,
  event: l.event,
  message: l.message,
  nodeId: l.node_id,
  storageId: l.storage_id,
  vuid: l.vuid,
  recordStatus: l.record_status,
  objectKind: l.object_kind,
  objectId: l.object_id,
  objectName: l.object_name
});
// An alert is an indicator, not an event: it exists while its condition holds
// and vanishes when the condition clears. There is no acknowledged/closed
// lifecycle — only a silence, which dies with the condition.
const normAlert = a => ({
  id: a.uuid,
  clusterId: a.cluster_id,
  clusterName: a.cluster_name,
  rule: a.rule,
  severity: a.severity,
  scope: a.scope,
  title: a.title,
  detail: a.detail,
  remedy: a.remedy,
  nodeId: a.node_id,
  nodeName: a.node_name,
  nodeIds: a.node_ids || [],
  nodeNames: a.node_names || [],
  deviceIds: a.device_ids || [],
  deviceNames: a.device_names || [],
  container: a.container || null,
  since: a.since,
  silenced: !!a.silenced,
  silencedBy: a.silenced_by || null
});

// An operation is an Ops CRD: a named action on one object that walks a fixed
// list of phases and lands in Succeeded, Failed or Aborted. The phase list is
// the state machine — the console reads it rather than inventing its own.
const OPS_KINDS = ["StorageClusterOps", "StorageNodeOps", "StorageDeviceOps", "StoragePoolOps", "PersistentVolumeOps", "StorageBackupOps", "ControlPlaneOps", "OperatorOps"];
const OPS_TARGET_LABEL = {
  StorageCluster: "cluster",
  StorageNode: "node",
  StorageDevice: "device",
  StoragePool: "pool",
  PersistentVolume: "volume",
  StorageBackup: "backup",
  ControlPlane: "control plane",
  Operator: "operator"
};
const normOperation = o => {
  const st = o.status || {},
    step = st.step || {};
  return {
    id: o.metadata.uid,
    name: o.metadata.name,
    kind: o.kind,
    action: (o.spec || {}).action,
    clusterId: st.clusterId || null,
    targetKind: st.targetKind || (window.RESOURCES[o.kind] || {}).ops || null,
    targetName: ((o.spec || {}).targetRef || {}).name || null,
    phase: st.phase,
    message: st.message || "",
    step: step.label || step.state,
    stepKey: step.state,
    steps: step.labels || step.steps || [],
    stepIndex: step.index || 0,
    abortable: !!step.abortable,
    aborting: !!(o.spec || {}).abort,
    startedAt: st.startedAt,
    completedAt: st.completedAt || null,
    events: st.events || [],
    running: !["Succeeded", "Failed", "Aborted"].includes(st.phase)
  };
};
const normRole = r => ({
  kind: "role",
  id: r.uuid || r.name,
  name: r.name,
  boundAt: r.boundAt || "",
  description: r.description || "",
  parts: r.parts || [],
  rules: r.rules || (r.parts || []).flatMap(p => p.rules)
});

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
  cgroupAddVolumes: (id, ids) => send("POST", `/consistency-groups/${id}/lvols`, {
    lvol_ids: ids
  }),
  cgroupRemoveVolume: (id, vid) => send("DELETE", `/consistency-groups/${id}/lvols/${vid}`),
  cgroupSnapshot: (id, name) => send("POST", `/consistency-groups/${id}/snapshot`, {
    name
  }),
  cgroupDelete: id => send("DELETE", `/consistency-groups/${id}`),
  cgroupSetPolicy: (id, policyId) => send("PUT", `/consistency-groups/${id}/backup-policy`, {
    policy_id: policyId
  }),
  cgroupReplicate: (id, cfg) => send("POST", `/consistency-groups/${id}/replicate`, cfg),
  cgroupUnreplicate: id => send("POST", `/consistency-groups/${id}/unreplicate`),
  cgroupApps: id => req("/protected-apps").then(r => r.map(normApp).filter(a2 => a2.cgId === id)),
  cgSnapBackup: (id, bucket) => send("POST", `/cg-snapshots/${id}/backup`, {
    bucket
  }),
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
  operations: cid => Promise.all(OPS_KINDS.map(k2 => k8s.list(k2).catch(() => []))).then(rs => rs.flat().map(normOperation).filter(o => !cid || o.clusterId === cid).sort((x, y) => y.running - x.running || Date.parse(y.startedAt) - Date.parse(x.startedAt))),
  operationAbort: (kind, name) => abortOps(kind, name),
  alertRuleCount: scope => (window.X && window.X.alertRuleCounts || {})[scope] || null,
  alertSilence: id => send("POST", `/alerts/${encodeURIComponent(id)}/silence`),
  alertUnsilence: id => send("POST", `/alerts/${encodeURIComponent(id)}/unsilence`),
  unassignedHosts: () => req("/hosts/unassigned").then(r => r.map(normHost)),
  // ---- replication: the real v1alpha1 kinds, on the Kubernetes API ----
  // Pairs, policies and slots are three lists that have to be joined, because
  // the CRDs reference each other by name and carry no rollups.
  replTree: () => Promise.all([k8s.list("ReplicationPair"), k8s.list("ReplicationPolicy"), k8s.list("ReplicationSlot")]).then(([pairs, pols, slots]) => ({
    pairs,
    pols,
    slots
  })),
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
    return t.pols.filter(p => mine.includes((p.spec || {}).pairRef)).map(p => normReplPolicy(p, t.slots, t.pairs));
  }),
  rpolicy: id => api.replTree().then(t => {
    const p = t.pols.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!p) throw new ApiError(404, `ReplicationPolicy ${id} not found`, "", "NotFound");
    return normReplPolicy(p, t.slots, t.pairs);
  }),
  pairRPolicies: id => api.replTree().then(t => {
    const pair = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    return t.pols.filter(p => pair && (p.spec || {}).pairRef === pair.metadata.name).map(p => normReplPolicy(p, t.slots, t.pairs));
  }),
  slots: () => k8s.list("ReplicationSlot").then(r => r.map(normReplSlot)),
  slot: id => k8s.list("ReplicationSlot").then(r => {
    const s = r.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!s) throw new ApiError(404, `ReplicationSlot ${id} not found`, "", "NotFound");
    return normReplSlot(s);
  }),
  policySlots: id => Promise.all([k8s.list("ReplicationPolicy"), k8s.list("ReplicationSlot")]).then(([pols, slots]) => {
    const p = pols.find(x => x.metadata.uid === id || x.metadata.name === id);
    return slots.filter(s => p && (s.spec || {}).policyRef === p.metadata.name).map(normReplSlot);
  }),
  pairSlots: id => api.replTree().then(t => {
    const pair = t.pairs.find(x => x.metadata.uid === id || x.metadata.name === id);
    const names = t.pols.filter(p => pair && (p.spec || {}).pairRef === pair.metadata.name).map(p => p.metadata.name);
    return t.slots.filter(s => names.includes((s.spec || {}).policyRef)).map(normReplSlot);
  }),
  replOps: () => k8s.list("ReplicationOps").then(r => r.map(normReplOps)),
  replOp: id => k8s.list("ReplicationOps").then(r => {
    const o = r.find(x => x.metadata.uid === id || x.metadata.name === id);
    if (!o) throw new ApiError(404, `ReplicationOps ${id} not found`, "", "NotFound");
    return normReplOps(o);
  }),
  // scope-filtered history, so a policy or pair can show its own operations
  refReplOps: ref => k8s.list("ReplicationOps").then(r => r.filter(o => (o.spec || {}).ref === ref).map(normReplOps)),
  pairCreateCrd: (name, sourceCluster, targetCluster) => k8s.create("ReplicationPair", {
    apiVersion: window.API_GROUP,
    kind: "ReplicationPair",
    metadata: {
      name
    },
    spec: {
      sourceCluster,
      targetCluster
    }
  }),
  rpolicyCreateCrd: (name, spec) => k8s.create("ReplicationPolicy", {
    apiVersion: window.API_GROUP,
    kind: "ReplicationPolicy",
    metadata: {
      name
    },
    spec
  }),
  // every failover, failback and planned cutover is a ReplicationOps
  replOpsCreate: spec => k8s.create("ReplicationOps", {
    apiVersion: window.API_GROUP,
    kind: "ReplicationOps",
    metadata: {
      name: `${spec.action}-${spec.ref}-${Date.now().toString(36)}`.slice(0, 253)
    },
    spec
  }),
  pairDeleteCrd: name => k8s.remove("ReplicationPair", name),
  rpolicyDeleteCrd: name => k8s.remove("ReplicationPolicy", name),
  // membership is a PVC annotation, nothing else
  pvcSetReplPolicy: (pvcName, namespace, policyName) => k8s.patch("PersistentVolumeClaim", pvcName, {
    metadata: {
      annotations: {
        "storage.simplyblock.io/replication-policy": policyName || null
      }
    }
  }, {
    namespace
  }),
  // ---- migration paths: A → B inside a stretched Kubernetes cluster ----
  mpaths: () => req("/migration-paths").then(r => r.map(normMPath)),
  mpath: id => req(`/migration-paths/${id}`).then(r => normMPath(r[0])),
  mpathGroups: id => req(`/migration-paths/${id}/app-groups`).then(r => r.map(normAppGroup).sort((x, y) => x.order - y.order)),
  mpathCreate: p => send("POST", "/migration-paths", p),
  mpathPause: id => send("POST", `/migration-paths/${id}/pause`),
  mpathResume: id => send("POST", `/migration-paths/${id}/resume`),
  mpathDelete: id => send("DELETE", `/migration-paths/${id}`),
  mpathReorder: (id, order) => send("PUT", `/migration-paths/${id}/queue`, {
    order
  }),
  appGroupCreate: (pathId, p) => send("POST", `/migration-paths/${pathId}/app-groups`, p),
  appGroup: id => req(`/app-groups/${id}`).then(r => normAppGroup(r[0])),
  appGroupVolumes: id => req(`/app-groups/${id}/lvols`).then(r => r.map(normVolume)),
  appGroupMove: id => send("POST", `/app-groups/${id}/move`),
  appGroupPause: id => send("POST", `/app-groups/${id}/pause`),
  appGroupResume: id => send("POST", `/app-groups/${id}/resume`),
  appGroupApproval: (id, approval) => send("PUT", `/app-groups/${id}/approval`, {
    approval
  }),
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
  hostSetTaint: (id, taint) => send("PUT", `/hosts/${id}/migration-taint`, {
    taint
  }),
  // ---- DR top layer: plans and sites ----
  plans: () => req("/protection-plans").then(r => r.map(normPlan)),
  plan: id => req("/protection-plans/" + id).then(r => normPlan(r[0])),
  planApps: id => req("/protection-plans/" + id + "/protected-apps").then(r => r.map(normApp)),
  planSites: id => req("/protection-plans/" + id + "/sites").then(r => r.map(normSite)),
  planCreate: p => send("POST", "/protection-plans", p),
  planDelete: id => send("DELETE", "/protection-plans/" + id),
  planAddMethod: (id, m) => send("POST", "/protection-plans/" + id + "/methods", m),
  planRemoveMethod: (id, name) => send("DELETE", "/protection-plans/" + id + "/methods/" + name),
  planSetInterval: (id, name, interval) => send("PUT", "/protection-plans/" + id + "/methods/" + name + "/interval", {
    interval
  }),
  sites: () => req("/dr-sites").then(r => r.map(normSite)),
  site: id => req("/dr-sites/" + id).then(r => normSite(r[0])),
  siteApps: id => req("/dr-sites/" + id + "/protected-apps").then(r => r.map(normApp)),
  siteFence: id => send("POST", "/dr-sites/" + id + "/fence"),
  siteUnfence: id => send("POST", "/dr-sites/" + id + "/unfence"),
  sitePlans: id => req("/dr-sites/" + id + "/protection-plans").then(r => r.map(normPlan)),
  // bypass payload 4: no Kubernetes object represents a generation
  appGenerations: id => req("/protected-apps/" + id + "/generations").then(r => r.map(normGeneration)),
  appSetOrchestrated: (id, method, allowVault) => send("PUT", "/protected-apps/" + id + "/orchestrated-method", {
    method,
    allow_vault: !!allowVault
  }),
  // bypass payload 2: pin the generation, then rebind the DRPC
  appRestore: (id, generation) => send("POST", "/protected-apps/" + id + "/restore", {
    generation
  }),
  appVerifyGeneration: (id, generation) => send("POST", "/protected-apps/" + id + "/verify-generation", {
    generation
  }),
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
  nsResourcesEach: (ns, cb) => Promise.all(["Deployment", "StatefulSet", "Service", "ConfigMap", "Secret", "Ingress", "PersistentVolumeClaim", "VirtualMachine"].map(kind => k8s.list(kind, {
    namespace: ns
  }).then(items => cb(kind, items, null)).catch(e => cb(kind, [], e)))),
  appFailover: (id, p) => send("POST", `/protected-apps/${id}/failover`, p || {}),
  appSetProtection: (id, p) => send("PUT", `/protected-apps/${id}/protection`, p),
  appRelocate: id => send("POST", `/protected-apps/${id}/relocate`),
  appCleanup: id => send("POST", `/protected-apps/${id}/cleanup`),
  appUnprotect: id => send("DELETE", `/protected-apps/${id}`),
  drClusterFence: id => send("POST", `/dr-clusters/${id}/fence`),
  drClusterUnfence: id => send("POST", `/dr-clusters/${id}/unfence`),
  // access control — RBAC-DESIGN.md. Kubernetes RBAC is the store; the operator
  // serves the aggregated view: one SelfSubjectRulesReview per namespace (/self),
  // the permitted scope tree (/scopes), AccessGrants, and SubjectAccessReview.
  accessSelf: () => req("/access/self").then(r => r[0]),
  accessScopes: () => req("/access/scopes").then(r => r[0]),
  accessRoles: () => req("/access-roles").then(r => r.map(normRole)),
  accessGrants: () => req("/access-grants"),
  accessGrantCreate: b => send("POST", "/access-grants", b),
  accessGrantDelete: id => send("DELETE", `/access-grants/${id}`),
  accessSubjects: () => req("/access/subjects"),
  accessEffective: subject => req("/access/effective?subject=" + encodeURIComponent(subject)).then(r => r[0]),
  accessReview: b => send("POST", "/access/review", b),
  accessInvariants: () => req("/access/invariants"),
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
  pvcResize: (id, size) => send("POST", `/pvcs/${id}/resize`, {
    size
  }),
  buckets: cid => req(`/clusters/${cid}/buckets`).then(r => r.map(normBucket)),
  bucket: id => req(`/buckets/${id}`).then(r => normBucket(r[0])),
  bucketCreate: (cid, p) => send("POST", `/clusters/${cid}/buckets`, p),
  bucketUpdate: (id, p) => send("PUT", `/buckets/${id}`, p),
  bucketSetAccess: (id, p) => send("PUT", `/buckets/${id}/access`, p),
  bucketResize: (id, size) => send("POST", `/buckets/${id}/resize`, {
    size
  }),
  bucketDelete: id => send("DELETE", `/buckets/${id}`),
  bucketSetTags: (id, tags) => send("PUT", `/buckets/${id}/tags`, {
    tags
  }),
  bucketReplicate: (id, policyId) => send("POST", `/buckets/${id}/replicate`, {
    policy_id: policyId
  }),
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
  rpolicyAddVolumes: (id, ids) => send("POST", `/replication-policies/${id}/lvols`, {
    lvol_ids: ids
  }),
  rpolicyRemoveVolume: (id, vid) => send("DELETE", `/replication-policies/${id}/lvols/${vid}`),
  rpolicyFailover: (id, planned) => send("POST", `/replication-policies/${id}/failover`, {
    planned
  }),
  rpolicyFailback: id => send("POST", `/replication-policies/${id}/failback`),
  rpolicyTest: id => send("POST", `/replication-policies/${id}/test-failover`),
  rpolicyResync: id => send("POST", `/replication-policies/${id}/resync`),
  rpolicyDelete: id => send("DELETE", `/replication-policies/${id}`),
  clusterSetZones: (cid, ids) => send("PUT", `/clusters/${cid}/zones`, {
    zone_ids: ids
  }),
  hostSetPlacement: (id, p) => send("PUT", `/hosts/${id}/placement`, p),
  // ---- discovery + deployment ----
  // A cluster is not created by POSTing a cluster: discovery writes an
  // inventory, a ClusterDeploymentConfig is drafted against it, and approving
  // that document is what deploys.
  discoveries: kid => preq(`/discoveries?scope=kubernetes-clusters&scopeId=${kid}`).then(r => r.map(normDiscovery)),
  discoveryRun: (kid, name, filter, nodeSelector) => k8s.create("OperatorOps", {
    apiVersion: window.API_GROUP,
    kind: "OperatorOps",
    metadata: {
      name: `discover-${(name || "cluster").slice(0, 30)}-${Date.now().toString(36)}`,
      namespace: window.SB_CONFIG.namespace
    },
    spec: {
      action: "Discover",
      kubernetesClusterRef: name,
      target: {
        kubernetesClusterId: kid
      },
      discover: {
        deviceFilter: filter,
        nodeSelector: nodeSelector || {
          matchLabels: {}
        }
      }
    }
  }),
  deployConfigs: () => k8s.list("ClusterDeploymentConfig").then(r => r.map(normDeployConfig)),
  k8sDeployConfigs: kid => k8s.list("ClusterDeploymentConfig").then(r => r.map(normDeployConfig).filter(c => c.k8sClusterId === kid)),
  deployConfig: id => k8s.list("ClusterDeploymentConfig").then(r => {
    const hit = r.map(normDeployConfig).find(c => c.id === id || c.name === id);
    if (!hit) throw new ApiError(404, "deployment config not found", "/" + id, "NotFound");
    return hit;
  }),
  deployConfigCreate: (name, spec) => k8s.create("ClusterDeploymentConfig", {
    apiVersion: window.API_GROUP,
    kind: "ClusterDeploymentConfig",
    metadata: {
      name,
      namespace: window.SB_CONFIG.namespace
    },
    spec: Object.assign({
      approved: false
    }, spec)
  }),
  // approval is the only mutable field, and it is one-way
  deployConfigApprove: name => k8s.patch("ClusterDeploymentConfig", name, {
    spec: {
      approved: true
    }
  }),
  // step / node logs: the operator's job pod logs, keyed by step[/host]
  deployLog: (id, name) => preq(`/deployments/${id}/logs?name=${encodeURIComponent(name)}`),
  deployConfigDelete: name => k8s.remove("ClusterDeploymentConfig", name),
  clusterCreate: payload => send("POST", "/clusters", payload),
  // ---- mutations ----
  clusterSuspend: id => send("POST", `/clusters/${id}/suspend`),
  clusterActivate: id => send("POST", `/clusters/${id}/activate`),
  clusterAddNode: (id, hostId, failureDomain) => send("POST", `/clusters/${id}/storage-nodes`, {
    host_id: hostId,
    failure_domain: failureDomain || null
  }),
  nodeShutdown: (id, force) => send("POST", `/storage-nodes/${id}/shutdown`, {
    force: !!force
  }),
  nodeRestart: id => send("POST", `/storage-nodes/${id}/restart`),
  nodeRemove: id => send("DELETE", `/storage-nodes/${id}`),
  nodeMigrate: (id, hostId) => send("POST", `/storage-nodes/${id}/migrate`, {
    host_id: hostId
  }),
  nodeAddDevice: (id, payload) => send("POST", `/storage-nodes/${id}/devices`, payload),
  deviceRestart: id => send("POST", `/devices/${id}/restart`),
  deviceFail: id => send("POST", `/devices/${id}/fail`),
  deviceHealthCheck: id => send("POST", `/devices/${id}/health-check`),
  deviceRemove: id => send("DELETE", `/devices/${id}`),
  hostReserveDevice: (hid, did, nodeId) => send("POST", `/hosts/${hid}/devices/${did}/reserve`, {
    node_id: nodeId
  }),
  hostsPrepare: (cid, ids) => send("POST", `/clusters/${cid}/hosts/prepare`, {
    host_ids: ids
  }),
  hostConfigure: (id, p) => send("POST", `/hosts/${id}/configure`, p),
  volumeDelete: id => send("DELETE", `/lvols/${id}`),
  volumeResize: (id, size) => send("POST", `/lvols/${id}/resize`, {
    size
  }),
  volumeSnapshot: (id, name) => send("POST", `/lvols/${id}/snapshot`, {
    name
  }),
  volumeClone: (id, name) => send("POST", `/lvols/${id}/clone`, {
    name
  }),
  // instant migration: the primary moves without copying data
  volumeMigrate: (id, p) => send("POST", `/lvols/${id}/migrate`, p),
  volumeRebalance: id => send("POST", `/lvols/${id}/rebalance`),
  migrationTasks: cid => req(`/clusters/${cid}/tasks?function_name=lvol_migration`).then(r => r.map(normTask)),
  volumeSetAffinity: (id, p) => send("PUT", `/lvols/${id}/affinity`, p),
  clusterRebalance: id => send("POST", `/clusters/${id}/rebalance`),
  clusterSetRebalance: (id, enabled) => send("PUT", `/clusters/${id}/auto-rebalance`, {
    enabled
  }),
  volumeBackup: (id, payload) => send("POST", `/lvols/${id}/backup`, payload),
  snapshotBackup: (id, bucket) => send("POST", `/snapshots/${id}/backup`, {
    bucket
  }),
  backupMerge: id => send("POST", `/backups/${id}/merge`),
  backupMergeVersion: (id, vid) => send("POST", `/backups/${id}/versions/${vid}/merge`),
  policyCreate: (cid, p) => send("POST", `/clusters/${cid}/backup-policies`, p),
  policyUpdate: (id, p) => send("PUT", `/backup-policies/${id}`, p),
  policyDelete: id => send("DELETE", `/backup-policies/${id}`),
  volumeSetPolicy: (id, policyId) => send("PUT", `/lvols/${id}/backup-policy`, {
    policy_id: policyId
  }),
  volumeSetQos: (id, qos) => send("PUT", `/lvols/${id}/qos`, qos),
  volumeSetDataReduction: (id, p) => send("PUT", `/lvols/${id}/data-reduction`, p),
  poolSetQos: (id, qos) => send("PUT", `/pools/${id}/qos`, qos),
  poolCreate: (cid, p) => send("POST", `/clusters/${cid}/pools`, p),
  poolEnable: id => send("POST", `/pools/${id}/enable`),
  poolDisable: id => send("POST", `/pools/${id}/disable`),
  snapshotDelete: id => send("DELETE", `/snapshots/${id}`),
  snapshotClone: (id, name) => send("POST", `/snapshots/${id}/clone`, {
    name
  }),
  backupRestore: (id, name, versionId) => send("POST", `/backups/${id}/restore`, {
    name,
    version_id: versionId
  }),
  backupExport: (id, destination, versionId) => send("POST", `/backups/${id}/export`, {
    destination,
    version_id: versionId
  }),
  backupDelete: id => send("DELETE", `/backups/${id}`)
};
const GETTER = {
  plan: api.plan,
  site: api.site,
  cluster: api.cluster,
  host: api.host,
  node: api.node,
  device: api.device,
  pool: api.pool,
  volume: api.volume,
  snapshot: api.snapshot,
  backup: api.backup,
  policy: api.policy,
  pair: api.pair,
  rpolicy: api.rpolicy,
  slot: api.slot,
  replops: api.replOp,
  zone: api.zone,
  cgroup: api.cgroup,
  cgsnapshot: api.cgSnapshot,
  migration: api.migration,
  k8sc: api.k8sCluster,
  storageclass: api.storageClass,
  pvc: api.pvc,
  drcluster: api.drCluster,
  protectedapp: api.protectedApp,
  bucket: api.bucket,
  deployconfig: api.deployConfig,
  mpath: api.mpath,
  appgroup: api.appGroup
};
const resolve = (kind, id) => REG[id] ? Promise.resolve(REG[id]) : GETTER[kind] ? GETTER[kind](id).catch(() => null) : Promise.resolve(null);

// ---- data hooks ------------------------------------------------------------
function useResource(key, loader, pollMs) {
  const ref = React.useRef(loader);
  ref.current = loader;
  const [s, setS] = React.useState({
    loading: true,
    data: null,
    error: null
  });
  const load = React.useCallback(async quiet => {
    if (!quiet) setS(p => ({
      loading: true,
      data: null,
      error: null
    }));
    try {
      const d = await ref.current();
      setS({
        loading: false,
        data: d,
        error: null
      });
    } catch (e) {
      setS(p => quiet ? p : {
        loading: false,
        data: null,
        error: e
      });
    }
  }, [key]);
  React.useEffect(() => {
    load(false);
  }, [key]);
  React.useEffect(() => {
    if (!pollMs) return;
    const i = setInterval(() => load(true), pollMs);
    return () => clearInterval(i);
  }, [key, pollMs]);
  return {
    ...s,
    reload: () => load(false)
  };
}
Object.assign(window, {
  api,
  API,
  ApiError,
  STATUS_META,
  REG,
  GETTER,
  resolve,
  useResource,
  normCluster,
  normHost,
  normNode,
  normDevice,
  normPool,
  normVolume,
  normSnapshot,
  normBackup,
  normPolicy,
  normTask,
  normLog,
  normAlert,
  normOperation,
  OPS_TARGET_LABEL,
  normPlan,
  normSite,
  normMethod,
  normLeg,
  normGeneration,
  normReplPair,
  normReplPolicy,
  normReplSlot,
  normReplOps,
  ivMinutes,
  normPair,
  normDrPolicy,
  normZone,
  normCg,
  normCgSnap,
  normMigration,
  normK8s,
  normSc,
  normPvc,
  normBucket,
  normDrCluster,
  normApp
});
})();
// ---- agent.jsx ----
(function(){
// ---------------------------------------------------------------------------
// OPERATOR-AGENT / PROMETHEUS CLIENT
// These are served by the operator, not by the control plane. Container logs
// should move to the Kubernetes pod-log subresource (k8s.logs); the rest —
// SMART, SPDK streams, FoundationDB backups — have no CRD and no core object,
// so they stay on the operator API until the model covers them. — data that the control plane API v2 does NOT
// expose. Read straight from the control plane containers, the node agent and
// Prometheus. Kept apart from api.jsx on purpose: different base URL,
// different auth, different availability guarantees.
// ---------------------------------------------------------------------------
const AGENT_BASE = window.SB_CONFIG.agentBase;
const PROM_BASE = window.SB_CONFIG.promBase;
async function areq(base, path) {
  let res;
  try {
    res = await fetch(base + path, {
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${window.SB_CONFIG.token}`
      }
    });
  } catch (e) {
    throw new ApiError(0, "Agent unreachable — this data is read directly from the containers", path);
  }
  let body = null;
  try {
    body = await res.json();
  } catch (e) {}
  if (!res.ok || body && body.status === false) throw new ApiError(res.status, body && body.error || "Request failed", path);
  return body && body.results !== undefined ? body.results : body;
}
const asend = (base, method, path, payload) => fetch(base + path, {
  method,
  headers: {
    "Content-Type": "application/json",
    Authorization: `Bearer ${window.SB_CONFIG.token}`
  },
  body: JSON.stringify(payload || {})
}).then(async res => {
  let b = null;
  try {
    b = await res.json();
  } catch (e) {}
  if (!res.ok || b && b.status === false) throw new ApiError(res.status, b && b.error || "Request failed", path);
  return b;
});
const normContainer = c => ({
  name: c.name,
  group: c.group,
  image: c.image,
  state: c.state,
  cpu: {
    alloc: c.cpu_cores_alloc,
    pct: c.cpu_pct
  },
  mem: {
    used: c.mem_used,
    limit: c.mem_limit
  },
  disk: {
    used: c.disk_used,
    limit: c.disk_limit
  },
  restarts: c.restarts,
  uptimeH: c.uptime_h
});
const normFdb = b => ({
  id: b.id,
  version: b.version,
  createdAt: b.created_at,
  size: b.size,
  type: b.type,
  status: b.status,
  restoreRequestedAt: b.restore_requested_at || null
});
const agent = {
  // The control plane is one deployment across every cluster, so none of these
  // are scoped by cluster.
  containers: () => areq(AGENT_BASE, "/control-plane/containers").then(r => r.map(normContainer)),
  containerLogs: name => areq(AGENT_BASE, `/control-plane/containers/${name}/logs`),
  fdbBackups: () => areq(AGENT_BASE, "/control-plane/fdb/backups").then(r => r.map(normFdb)),
  fdbRestore: id => asend(AGENT_BASE, "POST", `/control-plane/fdb/backups/${id}/restore`),
  nodeLogs: (nid, stream) => areq(AGENT_BASE, `/nodes/${nid}/logs/${stream}`),
  smart: did => areq(AGENT_BASE, `/devices/${did}/smart`).then(r => r[0]),
  smartRefresh: did => asend(AGENT_BASE, "POST", `/devices/${did}/smart/refresh`),
  // Prometheus: spdk_thread_busy_percent{node="<uuid>"}
  spdkThreads: nid => areq(PROM_BASE, `/query?query=${encodeURIComponent(`spdk_thread_busy_percent{node="${nid}"}`)}`).then(d => (d.data.result || []).map(r => ({
    name: r.metric.thread,
    core: Number(r.metric.core),
    busy: Number(r.value[1])
  })).sort((a, b) => a.core - b.core || a.name.localeCompare(b.name)))
};
const SourceTag = ({
  what
}) => /*#__PURE__*/React.createElement("span", {
  className: "srctag",
  title: `Not part of control plane API v2 — read from ${what}`
}, /*#__PURE__*/React.createElement(Icon, {
  n: "alert",
  s: 10
}), what);
Object.assign(window, {
  agent,
  AGENT_BASE,
  PROM_BASE,
  SourceTag,
  normContainer,
  normFdb
});
})();
// ---- ui.jsx ----
(function(){
const {
  useState,
  useEffect,
  useRef,
  useMemo,
  useCallback
} = React;
const fmtBytes = (n, d) => {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0,
    v = n;
  while (v >= 1000 && i < u.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v.toFixed(d === undefined ? v < 10 ? 2 : v < 100 ? 1 : 0 : d)} ${u[i]}`;
};
const fmtNum = n => !n ? "0" : n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(Math.round(n));
// Always returns gigabytes so the fixed "GB/s" label stays truthful.
const fmtBW = n => {
  const g = (n || 0) / 1e9;
  return !g ? "0" : g < 1 ? g.toFixed(3) : g < 10 ? g.toFixed(2) : g < 100 ? g.toFixed(1) : g.toFixed(0);
};
const pct = (a, b) => b ? Math.min(100, a / b * 100) : 0;
const shortId = s => s.slice(0, 8) + "…" + s.slice(-4);
const ICONS = {
  chev: "M6 3.5 10.5 8 6 12.5",
  chevd: "M3.5 6 8 10.5 12.5 6",
  chevu: "M3.5 10 8 5.5 12.5 10",
  pause: "M5 3.5v9M11 3.5v9",
  play: "M5 3.5 12 8 5 12.5Z",
  list: "M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.01M2.5 8h.01M2.5 12h.01",
  search: "M7.2 12.4a5.2 5.2 0 1 0 0-10.4 5.2 5.2 0 0 0 0 10.4ZM11 11l3 3",
  copy: "M5.5 5.5h7v7h-7zM3.5 10.5v-7h7",
  check: "M3.5 8.5 6.5 11.5 12.5 4.5",
  cluster: "M2.5 2.5h5v5h-5zM8.5 2.5h5v5h-5zM2.5 8.5h5v5h-5zM8.5 8.5h5v5h-5z",
  node: "M2.5 3.5h11v4h-11zM2.5 8.5h11v4h-11zM4.5 5.5h.01M4.5 10.5h.01",
  device: "M2.5 4.5h11v7h-11zM10.5 8h1.5",
  pool: "M2.5 5.5c2-1.6 3.5 1.6 5.5 0s3.5 1.6 5.5 0M2.5 9.5c2-1.6 3.5 1.6 5.5 0s3.5 1.6 5.5 0",
  volume: "M3.5 4.5h9v7h-9zM3.5 7h9M6 9.5h.01",
  lock: "M4.5 7.5h7v5h-7zM6 7.5V5.5a2 2 0 0 1 4 0v2",
  ext: "M9 3.5h3.5V7M12.5 3.5 7.5 8.5M11 9.5v3h-8v-8h3",
  sun: "M8 5a3 3 0 1 0 0 6 3 3 0 0 0 0-6M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.5 3.5l1 1M11.5 11.5l1 1M12.5 3.5l-1 1M4.5 11.5l-1 1",
  moon: "M13 9.5A5.5 5.5 0 0 1 6.5 3a5.5 5.5 0 1 0 6.5 6.5Z",
  alert: "M8 2.5 14.5 13.5h-13zM8 6.5v3.5M8 11.8h.01",
  refresh: "M13 8a5 5 0 1 1-1.6-3.7M13 2.5V5h-2.5",
  x: "M4 4l8 8M12 4l-8 8",
  bell: "M8 2.2a4 4 0 0 0-4 4v3l-1.2 2h10.4L12 9.2v-3a4 4 0 0 0-4-4M6.4 13a1.7 1.7 0 0 0 3.2 0",
  filter: "M2.5 3.5h11l-4.3 5v4l-2.4 1.3v-5.3z",
  gauge: "M3 11.5a5.5 5.5 0 1 1 10 0M8 8.5 10.5 6",
  host: "M2.5 2.5h11v11h-11zM5.5 5.5h5v5h-5zM8 2.5v3M8 10.5v3M2.5 8h3M10.5 8h3",
  power: "M8 2.5v5M11.5 4.4a5 5 0 1 1-7 0",
  plus: "M8 3.5v9M3.5 8h9",
  move: "M2.5 8h11M10.5 5l3 3-3 3M5.5 5l-3 3 3 3",
  trash: "M3 4.5h10M6 4.5V3h4v1.5M4.5 4.5l.7 9h5.6l.7-9M6.8 7v4M9.2 7v4",
  camera: "M2.5 5h2.6l1-1.5h3.8l1 1.5h2.6v8h-11zM8 11a2.4 2.4 0 1 0 0-4.8 2.4 2.4 0 0 0 0 4.8",
  cloud: "M4.6 12.5a3 3 0 0 1-.3-6 4 4 0 0 1 7.6.6 2.7 2.7 0 0 1-.4 5.4z",
  clock: "M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11M8 5v3.2l2.2 1.3",
  link: "M6.8 9.2a2.6 2.6 0 0 0 3.7 0l2-2a2.6 2.6 0 0 0-3.7-3.7l-.9.9M9.2 6.8a2.6 2.6 0 0 0-3.7 0l-2 2a2.6 2.6 0 0 0 3.7 3.7l.9-.9",
  dots: "M8 4.2h.01M8 8h.01M8 11.8h.01",
  shield: "M8 2.2 13 4v4.2c0 3-2.1 4.9-5 5.6-2.9-.7-5-2.6-5-5.6V4z",
  swap: "M3 5.5h9L9.5 3M13 10.5H4l2.5 2.5",
  arrow: "M3 8h10M9.5 4.5 13 8l-3.5 3.5",
  zone: "M2.5 13.5h11M4 13.5V6l4-3.5L12 6v7.5M6.5 13.5v-3h3v3",
  k8s: "M8 1.8 13.4 4.6v6.8L8 14.2 2.6 11.4V4.6zM8 5.4 10.8 6.9v3.2L8 11.6 5.2 10.1V6.9z",
  folder: "M2.5 12.5v-9h4l1.5 2h5.5v7z"
};
const Icon = ({
  n,
  s = 14,
  c,
  sw = 1.4,
  style
}) => /*#__PURE__*/React.createElement("svg", {
  className: "ic",
  width: s,
  height: s,
  viewBox: "0 0 16 16",
  fill: "none",
  stroke: c || "currentColor",
  strokeWidth: n === "dots" ? 2.4 : sw,
  strokeLinecap: "round",
  strokeLinejoin: "round",
  style: style,
  "aria-hidden": "true"
}, /*#__PURE__*/React.createElement("path", {
  d: ICONS[n] || ICONS.chev
}));
const fmtDate = s => {
  if (!s) return "—";
  const d = new Date(s);
  return isNaN(d) ? s : d.toISOString().slice(0, 16).replace("T", " ") + " UTC";
};
const fmtDur = s => {
  s = Math.round(s || 0);
  return s < 60 ? s + "s" : s < 3600 ? Math.round(s / 60) + "m" : s < 86400 ? +(s / 3600).toFixed(1) + "h" : +(s / 86400).toFixed(1) + "d";
};
const fmtAgo = s => {
  const d = Date.parse(s);
  if (isNaN(d)) return "";
  const m = Math.max(0, Math.round(((window.SB_NOW || Date.now()) - d) / 60000));
  return m < 60 ? m + "m ago" : m < 1440 ? Math.round(m / 60) + "h ago" : Math.round(m / 1440) + "d ago";
};
function TrafficLight({
  status,
  sm,
  label
}) {
  const m = STATUS_META[status] || {
    c: "var(--idle)",
    label: status
  };
  return /*#__PURE__*/React.createElement("span", {
    className: "tl" + (m.blink ? " blink" : "") + (sm ? " sm" : ""),
    style: {
      "--c": m.c,
      color: m.c
    }
  }, /*#__PURE__*/React.createElement("i", {
    className: "d"
  }), label !== false && (m.label || status));
}
function Uuid({
  value,
  short = true
}) {
  const [done, setDone] = useState(false);
  return /*#__PURE__*/React.createElement("div", {
    className: "uuid",
    title: value
  }, /*#__PURE__*/React.createElement("span", null, short ? shortId(value) : value), /*#__PURE__*/React.createElement("button", {
    className: done ? "done" : "",
    title: "Copy UUID",
    onClick: e => {
      e.stopPropagation();
      navigator.clipboard && navigator.clipboard.writeText(value);
      setDone(true);
      setTimeout(() => setDone(false), 1200);
      window.__toast && window.__toast("UUID copied to clipboard");
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: done ? "check" : "copy",
    s: 11,
    c: done ? "var(--ok)" : undefined
  })));
}
function Capacity({
  label = "Capacity",
  total,
  used,
  unit
}) {
  const p = pct(used, total);
  const c = p > 90 ? "var(--bad)" : p > 75 ? "var(--warn)" : "var(--accent)";
  return /*#__PURE__*/React.createElement("div", {
    className: "capwrap"
  }, /*#__PURE__*/React.createElement("div", {
    className: "caprow"
  }, /*#__PURE__*/React.createElement("span", null, label, " ", /*#__PURE__*/React.createElement("b", {
    className: "mono",
    style: {
      color: c,
      fontWeight: 600
    }
  }, p.toFixed(0), "%")), /*#__PURE__*/React.createElement("span", {
    className: "capval"
  }, fmtBytes(used), " ", /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "/ ", fmtBytes(total)))), /*#__PURE__*/React.createElement("div", {
    className: "bar"
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: p + "%",
      "--bc": c
    }
  })));
}
function Sparkline({
  data,
  color = "var(--dim2)",
  w = 52,
  h = 15
}) {
  if (!data || !data.length) return null;
  const max = Math.max(...data, 1),
    min = Math.min(...data);
  const rng = max - min || max || 1;
  const pts = data.map((v, i) => `${i / (data.length - 1) * w},${h - (v - min) / rng * (h - 2) - 1}`).join(" ");
  return /*#__PURE__*/React.createElement("svg", {
    className: "spark",
    width: w,
    height: h,
    viewBox: `0 0 ${w} ${h}`,
    fill: "none",
    "aria-hidden": "true"
  }, /*#__PURE__*/React.createElement("polyline", {
    points: pts,
    stroke: color,
    strokeWidth: "1.2",
    strokeLinejoin: "round",
    strokeLinecap: "round"
  }));
}
function Metric({
  label,
  unit,
  r,
  w,
  fmt,
  hist,
  color
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "met"
  }, /*#__PURE__*/React.createElement("div", {
    className: "k"
  }, /*#__PURE__*/React.createElement("span", null, label, unit && /*#__PURE__*/React.createElement("span", {
    style: {
      opacity: .8
    }
  }, " ", unit)), /*#__PURE__*/React.createElement(Sparkline, {
    data: hist,
    color: color
  })), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "READ"), /*#__PURE__*/React.createElement("b", null, fmt(r))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "WRITE"), /*#__PURE__*/React.createElement("b", null, fmt(w)))));
}
const QosChips = ({
  qos
}) => {
  if (!qos) return /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "No QoS limits \u2014 pool default applies");
  const rows = [["rw iops", qos.rw_ios_per_sec && fmtNum(qos.rw_ios_per_sec)], ["rw", qos.rw_mbytes_per_sec && qos.rw_mbytes_per_sec + " MB/s"], ["r", qos.r_mbytes_per_sec && qos.r_mbytes_per_sec + " MB/s"], ["w", qos.w_mbytes_per_sec && qos.w_mbytes_per_sec + " MB/s"]].filter(x => x[1]);
  if (!rows.length) return /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "QoS profile set, all limits unlimited");
  return /*#__PURE__*/React.createElement("div", {
    className: "qos"
  }, rows.map(([k, v]) => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: k
  }, /*#__PURE__*/React.createElement("i", null, k), v)));
};
function useLocal(key, init) {
  const [v, setV] = useState(() => {
    try {
      const s = localStorage.getItem(key);
      return s === null ? init : JSON.parse(s);
    } catch (e) {
      return init;
    }
  });
  useEffect(() => {
    try {
      localStorage.setItem(key, JSON.stringify(v));
    } catch (e) {}
  }, [key, v]);
  return [v, setV];
}

// Backup policy schedule: interval · retained backup versions · online snapshots.
// The interval is both the snapshot/backup periodicity and the merge cadence
// for that tier once its version count is exceeded.
const BackupSchedule = ({
  rows,
  showTotals = true
}) => !rows || !rows.length ? /*#__PURE__*/React.createElement("div", {
  className: "nolim"
}, "No schedule rows \u2014 this policy triggers nothing.") : /*#__PURE__*/React.createElement("div", {
  className: "rettable"
}, /*#__PURE__*/React.createElement("div", {
  className: "rethead"
}, /*#__PURE__*/React.createElement("span", null, "Every"), /*#__PURE__*/React.createElement("span", null, "Versions"), /*#__PURE__*/React.createElement("span", null, "Online")), rows.map((r, i) => /*#__PURE__*/React.createElement("div", {
  className: "retrow",
  key: i
}, /*#__PURE__*/React.createElement("span", {
  className: "mono"
}, r.interval), /*#__PURE__*/React.createElement("b", null, r.versions, "\xD7"), /*#__PURE__*/React.createElement("b", {
  style: r.online ? {
    color: "var(--accent)"
  } : {
    color: "var(--dim2)",
    fontWeight: 400
  }
}, r.online ? r.online + "×" : "—"))), showTotals && /*#__PURE__*/React.createElement("div", {
  className: "retrow tot"
}, /*#__PURE__*/React.createElement("span", null, "retained"), /*#__PURE__*/React.createElement("b", null, rows.reduce((a, r) => a + (Number(r.versions) || 0), 0), "\xD7"), /*#__PURE__*/React.createElement("b", null, rows.reduce((a, r) => a + (Number(r.online) || 0), 0), "\xD7")));
Object.assign(window, {
  fmtBytes,
  fmtNum,
  fmtBW,
  fmtDate,
  fmtAgo,
  fmtDur,
  pct,
  shortId,
  Icon,
  ICONS,
  TrafficLight,
  Uuid,
  Capacity,
  Sparkline,
  Metric,
  QosChips,
  BackupSchedule,
  useLocal,
  useState,
  useEffect,
  useRef,
  useMemo,
  useCallback
});
})();
// ---- rbac.jsx ----
(function(){
// ---------------------------------------------------------------------------
// ACCESS CONTROL — the client side of §5 of the RBAC design.
//
// The API server is the only policy decision point. This file never enforces:
// it asks one SelfSubjectRulesReview per namespace (aggregated in /access/self),
// evaluates locally, and renders. Denied controls are DISABLED with the missing
// permission in the tooltip; only whole scopes the caller cannot see are hidden.
// incomplete:true renders optimistically — the real call fails with a clear 403.
// ---------------------------------------------------------------------------
const OPS_LABEL = {
  create: "C",
  read: "R",
  update: "U",
  delete: "D",
  restoresource: "src",
  backup: "bak",
  restore: "rst",
  failover: "f/o",
  failback: "f/b",
  fence: "fence"
};
const ENTITY_LABEL = {
  k8scluster: "Managed cluster",
  storagecluster: "Storage cluster",
  storagepool: "Storage pool",
  backupop: "Backup / restore",
  replicationpolicy: "Replication policy",
  backuppolicy: "Backup policy",
  drpolicy: "DR policy",
  application: "Application",
  role: "ClusterRole",
  binding: "AccessGrant"
};
const ENTITY_COVERS = {
  k8scluster: "worker nodes, discovery, allocations",
  storagecluster: "hosts, nodes, devices, tasks, logs, migrations, S3 target, KMS endpoint",
  storagepool: "volumes, snapshots, clones, PVCs, backups, buckets, KEKs",
  backupop: "per-volume backup and restore",
  replicationpolicy: "pairs, policies, slots",
  backuppolicy: "schedules and retention",
  drpolicy: "protection plans, methods, migration paths",
  application: "protected applications, recipes, failovers",
  role: "aggregated sb:* ClusterRole",
  binding: "RoleBinding or AccessGrant"
};
const ENTITY_OPS = {
  k8scluster: ["create", "read", "update", "delete"],
  storagecluster: ["create", "read", "update", "delete"],
  storagepool: ["create", "read", "update", "delete", "restoresource"],
  backupop: ["backup", "restore"],
  replicationpolicy: ["create", "read", "update", "delete"],
  backuppolicy: ["create", "read", "update", "delete"],
  drpolicy: ["create", "read", "update", "delete"],
  application: ["create", "read", "update", "delete", "failover", "failback", "fence"],
  role: ["read"],
  binding: ["create", "read", "delete"]
};
// UI kind -> the main entity whose namespace governs it
const KIND_ENTITY = {
  cluster: "storagecluster",
  host: "storagecluster",
  node: "storagecluster",
  device: "storagecluster",
  task: "storagecluster",
  migration: "storagecluster",
  deployconfig: "k8scluster",
  k8sc: "k8scluster",
  zone: "k8scluster",
  pool: "storagepool",
  volume: "storagepool",
  snapshot: "storagepool",
  backup: "storagepool",
  pvc: "storagepool",
  storageclass: "storagepool",
  bucket: "storagepool",
  cgroup: "storagepool",
  cgsnapshot: "storagepool",
  policy: "backuppolicy",
  pair: "replicationpolicy",
  rpolicy: "replicationpolicy",
  slot: "replicationpolicy",
  replop: "replicationpolicy",
  plan: "drpolicy",
  method: "drpolicy",
  site: "drpolicy",
  mpath: "drpolicy",
  appgroup: "drpolicy",
  protectedapp: "application",
  role: "role",
  binding: "binding",
  grant: "binding",
  replops: "replicationpolicy"
};
// UI kind -> the CRD resource the API server checks (§3.5, the console's column)
const KIND_RESOURCE = {
  cluster: "storageclusters",
  host: "storagenodes",
  node: "storagenodes",
  device: "devices",
  task: "storageclusters",
  migration: "storageclusters",
  k8sc: "managedclusters",
  zone: "managedclusters",
  deployconfig: "nodepoolallocations",
  pool: "storagepools",
  volume: "volumes",
  pvc: "volumes",
  storageclass: "storagepools",
  bucket: "buckets",
  cgroup: "consistencygroups",
  cgsnapshot: "snapshots",
  snapshot: "snapshots",
  backup: "backups",
  policy: "backuppolicies",
  pair: "clusterpairs",
  rpolicy: "replicationpolicies",
  slot: "replicationpolicies",
  replop: "replicationpolicies",
  replops: "replicationpolicies",
  plan: "protectionplans",
  method: "protectionplans",
  site: "drclusters",
  mpath: "protectionplans",
  appgroup: "protectionplans",
  protectedapp: "protectedapplications",
  role: "clusterroles",
  binding: "accessgrants",
  grant: "accessgrants"
};
const ENTITY_RESOURCE = {
  k8scluster: "managedclusters",
  storagecluster: "storageclusters",
  storagepool: "storagepools",
  backupop: "backups",
  replicationpolicy: "replicationpolicies",
  backuppolicy: "backuppolicies",
  drpolicy: "drpolicies",
  application: "protectedapplications",
  role: "clusterroles",
  binding: "accessgrants"
};
const VERB_OF = {
  read: "get",
  create: "create",
  update: "update",
  delete: "delete",
  backup: "create",
  restoresource: "get"
};

// ---- the store ---------------------------------------------------------------
const AC_STATE = {
  ready: false,
  user: null,
  groups: [],
  initials: "??",
  label: "",
  rules: {
    cluster: [],
    ns: {}
  },
  incomplete: false,
  grants: [],
  scopes: null,
  ns: {
    clusters: {},
    pools: {},
    managed: {},
    apps: {},
    dr: "sb-dr-system"
  },
  demoUsers: [],
  error: null
};
const acListeners = new Set();
const acNotify = () => acListeners.forEach(f => f());
async function loadAccess() {
  try {
    const r = await api.accessSelf();
    Object.assign(AC_STATE, {
      ready: true,
      error: null,
      user: r.user,
      groups: r.groups || [],
      initials: r.initials || "??",
      label: r.label || r.user,
      rules: r.rules || {
        cluster: [],
        ns: {}
      },
      incomplete: !!r.incomplete,
      grants: r.grants || [],
      scopes: r.scopes || null,
      ns: r.scopes && r.scopes.ns || AC_STATE.ns,
      demoUsers: r.demo_users || []
    });
  } catch (e) {
    Object.assign(AC_STATE, {
      ready: true,
      error: e,
      user: null,
      rules: {
        cluster: [],
        ns: {}
      },
      grants: [],
      scopes: null
    });
  }
  acNotify();
}

// ---- object -> namespaces ------------------------------------------------------
const clusterIdsOf = o => [...new Set([o.kind === "cluster" ? o.id : null, o.clusterId, o.sourceClusterId, o.targetClusterId, ...(o.clusterIds || []), ...(o.involvedClusterIds || [])].filter(Boolean))];
function nsOf(entity, o, op) {
  const N = AC_STATE.ns;
  if (!o) return entity === "k8scluster" || entity === "binding" ? ["*"] : [];
  if (entity === "k8scluster") {
    if (op === "create") return ["*"]; // create nodepoolallocations is cluster-scoped
    const ids = o.kind === "k8sc" ? [o.id] : o.k8sClusterIds || (o.k8sClusterId ? [o.k8sClusterId] : []);
    return ids.map(id => N.managed[id]).filter(Boolean);
  }
  if (entity === "storagepool" || entity === "backupop") {
    const pid = o.kind === "pool" ? o.id : o.poolId;
    if (pid && N.pools[pid]) return [N.pools[pid]];
    return clusterIdsOf(o).map(id => N.clusters[id]).filter(Boolean); // pool create: the cluster namespace
  }
  if (entity === "storagecluster" || entity === "backuppolicy") return clusterIdsOf(o).map(id => N.clusters[id]).filter(Boolean);
  if (entity === "replicationpolicy" || entity === "drpolicy") return [N.dr];
  if (entity === "application") {
    const aid = o.kind === "protectedapp" ? o.id : o.appId;
    if (aid && N.apps[aid]) return [N.apps[aid]];
    return clusterIdsOf(o).map(id => N.clusters[id]).filter(Boolean); // app create: source cluster
  }
  if (entity === "binding") return o.namespaces || (o.scope ? scopeNamespaces(o.scope) : ["*"]);
  return [];
}
function scopeNamespaces(sc) {
  const N = AC_STATE.ns;
  return sc.kind === "cluster-scope" ? ["*"] : sc.kind === "managed-cluster" ? [N.managed[sc.id]] : sc.kind === "storage-cluster" ? [N.clusters[sc.id]] : sc.kind === "storage-pool" ? [N.pools[sc.id]] : sc.kind === "dr-pair" ? [N.dr] : sc.kind === "application" ? [N.apps[sc.id]] : [];
}

// ---- rule evaluation (SSRR, local) --------------------------------------------
const ruleAllows = (r, verb, resource, group, name) => (r.apiGroups.includes(group || "simplyblock.io") || r.apiGroups.includes("*")) && (r.resources.includes(resource) || r.resources.includes("*")) && (r.verbs.includes(verb) || r.verbs.includes("*")) && (!r.resourceNames || !name || r.resourceNames.includes(name));
function allowedIn(ns, verb, resource, group, name) {
  if (AC_STATE.rules.cluster.some(r => ruleAllows(r, verb, resource, group, name))) return true;
  if (!ns || ns === "*") return false;
  return (AC_STATE.rules.ns[ns] || []).some(r => ruleAllows(r, verb, resource, group, name));
}
const anyNs = (verb, resource, group, name) => allowedIn("*", verb, resource, group, name) || Object.keys(AC_STATE.rules.ns).some(n => allowedIn(n, verb, resource, group, name));

// what (verb, resource) does (op, entity) become?
function target(op, entity, obj) {
  if (entity === "application" && (op === "failover" || op === "failback")) return {
    verb: "create",
    resource: "applicationfailovers"
  };
  if (entity === "application" && op === "fence") return {
    verb: "update",
    resource: "drclusters",
    nsOverride: [AC_STATE.ns.dr]
  };
  if (entity === "k8scluster" && op === "create") return {
    verb: "create",
    resource: "nodepoolallocations"
  };
  const resource = obj && KIND_RESOURCE[obj.kind] && KIND_ENTITY[obj.kind] === entity ? KIND_RESOURCE[obj.kind] : ENTITY_RESOURCE[entity] || entity;
  return {
    verb: VERB_OF[op] || op,
    resource
  };
}
// Is `op` on `entity` permitted for `obj`? Cross-cluster policies must pass on
// every namespace they touch. Returns true when the rules review is incomplete.
function acCan(op, entity, obj) {
  if (!AC_STATE.ready) return false;
  if (AC_STATE.incomplete) return true;
  const t = target(op, entity, obj);
  const nss = t.nsOverride || nsOf(entity, obj, op);
  if (!nss.length) return allowedIn("*", t.verb, t.resource);
  return nss.every(ns => allowedIn(ns, t.verb, t.resource));
}
// why not? — the tooltip text for a disabled control (§5.5)
function acWhy(op, entity, obj) {
  const t = target(op, entity, obj);
  const nss = t.nsOverride || nsOf(entity, obj, op);
  const missing = nss.filter(ns => !allowedIn(ns, t.verb, t.resource));
  return `Needs ${t.verb} on ${t.resource}${missing.length ? " in " + missing.map(n => n === "*" ? "cluster scope" : n).join(", ") : nss.length ? "" : " (cluster scope)"}`;
}
const canAnywhere = (op, entity) => AC_STATE.ready && (AC_STATE.incomplete || anyNs(target(op, entity, null).verb, target(op, entity, null).resource));
// scope discovery (§5.2): scopes the server says are visible; other kinds are the server's job
const SCOPE_KINDS = {
  cluster: "clusters",
  k8sc: "managed",
  pool: "pools",
  protectedapp: "apps"
};
function canRead(o) {
  if (!AC_STATE.ready) return false;
  const coll = SCOPE_KINDS[o.kind];
  if (coll && AC_STATE.scopes) {
    const e = AC_STATE.scopes[coll].find(x => x.id === o.id);
    if (e) return !!e.visible;
  }
  return acCan("read", KIND_ENTITY[o.kind] || "storagecluster", o);
}
const canCreateIn = (kind, parent) => acCan("create", KIND_ENTITY[kind] || "storagecluster", parent);
const whyCreateIn = (kind, parent) => acWhy("create", KIND_ENTITY[kind] || "storagecluster", parent);
// restore = get backups in the source pool + create volumes in the target pool
const canRestore = (backup, targetPool) => acCan("read", "backupop", backup) && (!targetPool || allowedIn(AC_STATE.ns.pools[targetPool.id] || "", "create", "volumes"));
// §4.3 on the client: may the caller bind `role` in `ns`? (server decides; this only disables the option)
function mayBind(role, ns) {
  if (!allowedIn(ns, "create", "accessgrants")) return false;
  if (allowedIn(ns, "bind", "clusterroles", "rbac.authorization.k8s.io", role.name)) return true;
  return role.rules.length > 0 && role.rules.every(r => r.resources.every(res => r.verbs.every(v => allowedIn(ns, v, res, r.apiGroups[0]))));
}
const rulesIn = ns => [...AC_STATE.rules.cluster, ...(AC_STATE.rules.ns[ns] || [])];
const grantsFor = o => {
  const nss = new Set(["*", ...(Object.keys(KIND_ENTITY).includes(o.kind) ? nsOf(KIND_ENTITY[o.kind], o) : [])]);
  return AC_STATE.grants.filter(g => g.namespaces.some(n => nss.has(n)));
};
const access = {
  can: acCan,
  why: acWhy,
  canAnywhere,
  canRead,
  canCreateIn,
  whyCreateIn,
  canRestore,
  mayBind,
  nsOf,
  scopeNamespaces,
  rulesIn,
  grantsFor,
  allowedIn,
  load: loadAccess,
  state: AC_STATE,
  KIND_ENTITY,
  KIND_RESOURCE
};
window.access = access;
function useAccess() {
  const [, tick] = useState(0);
  useEffect(() => {
    const f = () => tick(t => t + 1);
    acListeners.add(f);
    return () => acListeners.delete(f);
  }, []);
  return access;
}
// <Can op="update" entity="storagepool" obj={pool}>…</Can> — hides; prefer disabled controls with access.why()
const Can = ({
  op,
  entity,
  obj,
  children,
  fallback
}) => {
  useAccess();
  return acCan(op, entity, obj) ? children : fallback || null;
};

// ---- identity in the top bar ------------------------------------------------
function IdentityMenu({
  here
}) {
  const a = useAccess();
  const [open, setOpen] = useState(false);
  const ref = useRef(null);
  useEffect(() => {
    if (!open) return;
    const h = e => {
      if (ref.current && !ref.current.contains(e.target)) setOpen(false);
    };
    document.addEventListener("mousedown", h);
    return () => document.removeEventListener("mousedown", h);
  }, [open]);
  const s = a.state;
  const nss = here ? nsOf(KIND_ENTITY[here.kind] || "storagecluster", here) : [];
  const applying = here ? a.grantsFor(here) : s.grants.filter(g => g.namespaces.includes("*"));
  return /*#__PURE__*/React.createElement("div", {
    ref: ref,
    style: {
      position: "relative"
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "avatar",
    title: s.user || "not signed in",
    onClick: () => setOpen(o => !o),
    style: {
      border: 0,
      cursor: "pointer"
    }
  }, s.initials), open && /*#__PURE__*/React.createElement("div", {
    className: "idmenu"
  }, /*#__PURE__*/React.createElement("div", {
    className: "idhead"
  }, /*#__PURE__*/React.createElement("b", null, s.label || s.user || "No identity"), /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, s.user), s.error && /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--bad)",
      fontSize: 11.5
    }
  }, s.error.message, " \u2014 nothing is permitted without an identity."), s.incomplete && /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--warn)",
      fontSize: 11.5
    }
  }, "The rules review is incomplete \u2014 controls are shown optimistically and may fail with 403.")), !!s.groups.length && /*#__PURE__*/React.createElement("div", {
    className: "idsec"
  }, /*#__PURE__*/React.createElement("h4", null, "Groups"), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, s.groups.map(g => /*#__PURE__*/React.createElement("span", {
    key: g,
    className: "lab mono"
  }, g)))), /*#__PURE__*/React.createElement("div", {
    className: "idsec"
  }, /*#__PURE__*/React.createElement("h4", null, here ? `Roles in ${nss.map(n => n === "*" ? "cluster scope" : n).join(", ") || "this scope"}` : "Roles at cluster scope"), applying.length ? applying.map(g => /*#__PURE__*/React.createElement("div", {
    key: g.uuid,
    className: "idrole"
  }, /*#__PURE__*/React.createElement("b", null, g.role), /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, g.namespaces.map(n => n === "*" ? "cluster scope" : n).join(", ")), /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "via ", g.subject.kind.toLowerCase(), " ", g.subject.name.replace(/^oidc:/, "")))) : /*#__PURE__*/React.createElement("div", {
    style: {
      color: "var(--dim)",
      fontSize: 12
    }
  }, here ? "No RoleBinding reaches this namespace — if you can see the object, you are reading it through a wider scope." : "No cluster-scoped binding.")), !!s.demoUsers.length && /*#__PURE__*/React.createElement("div", {
    className: "idsec"
  }, /*#__PURE__*/React.createElement("h4", null, "View as ", /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)",
      fontWeight: 400
    }
  }, "fixture backend \u2014 stands in for impersonation")), s.demoUsers.map(u => /*#__PURE__*/React.createElement("button", {
    key: u.name,
    className: "idpick" + (u.name === s.user ? " on" : ""),
    onClick: () => {
      localStorage.setItem("sb.viewas", u.name);
      setOpen(false);
      a.load().then(() => window.dispatchEvent(new Event("sb:access")));
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "avatar sm"
  }, u.initials), /*#__PURE__*/React.createElement("span", null, u.label))))));
}
Object.assign(window, {
  access,
  useAccess,
  Can,
  IdentityMenu,
  KIND_ENTITY,
  KIND_RESOURCE,
  ENTITY_LABEL,
  ENTITY_COVERS,
  ENTITY_OPS,
  OPS_LABEL,
  ACCESS_OPS_LABEL: OPS_LABEL
});
})();
// ---- actions.jsx ----
(function(){
// ---------------------------------------------------------------------------
// ACTION LAYER — per-object command registry, kebab menus and the
// parameterised confirm dialogs they open. Every command maps 1:1 to a
// mutation endpoint in api.jsx.
// ---------------------------------------------------------------------------
const GBn = 1e9;
const gb = n => Math.round((n || 0) / GBn);
const qosFields = q => [{
  k: "rw_ios_per_sec",
  label: "Max read+write IOPS",
  type: "number",
  min: 0,
  def: q && q.rw_ios_per_sec || 0,
  placeholder: "0 = unlimited"
}, {
  k: "rw_mbytes_per_sec",
  label: "Max read+write throughput",
  unit: "MB/s",
  type: "number",
  min: 0,
  def: q && q.rw_mbytes_per_sec || 0
}, {
  k: "r_mbytes_per_sec",
  label: "Max read throughput",
  unit: "MB/s",
  type: "number",
  min: 0,
  def: q && q.r_mbytes_per_sec || 0
}, {
  k: "w_mbytes_per_sec",
  label: "Max write throughput",
  unit: "MB/s",
  type: "number",
  min: 0,
  def: q && q.w_mbytes_per_sec || 0
}, {
  k: "n1",
  type: "note",
  label: "0 means unlimited. Setting every field to 0 removes the QoS profile."
}];
const configureHostDialog = h => {
  const sockets = Array.from({
    length: h.sockets || 1
  }, (_, s) => s);
  const mp = !!(REG[h.clusterId] && REG[h.clusterId].multipathing);
  return {
    title: `Configure ${h.hostname} as a storage host`,
    confirm: "Apply configuration",
    desc: "Runs the storage-plane configuration on the inspected node. Only the resources you pick here are handed to simplyblock; everything else stays with the kubelet.",
    fields: [{
      k: "numa_sockets",
      label: "NUMA socket(s) to use",
      type: "multiselect",
      required: true,
      options: sockets.map(s => ({
        v: String(s),
        l: `socket ${s} · ${h.devices.filter(d => d.socket === s).length} device(s) · ${h.nics.filter(n => n.socket === s).length} NIC(s)`
      }))
    }, {
      k: "memory_per_pod",
      label: "System memory per storage-plane pod",
      unit: "GB",
      type: "number",
      min: 8,
      def: Math.max(16, Math.round(h.memory / 1e9 / 4)),
      required: true
    }, {
      k: "hugepages_per_pod",
      label: "Hugepages per pod",
      unit: "GB",
      type: "number",
      min: 4,
      def: Math.max(8, Math.round(h.memory / 1e9 / 8)),
      required: true
    }, {
      k: "device_ids",
      label: "Devices to assign",
      type: "multiselect",
      required: true,
      options: h.devices.map(d => ({
        v: d.id,
        l: `${d.kind === "nvme" ? d.pcie : d.blockdev} · socket ${d.socket} · ${fmtBytes(d.size)} · ${d.model}`
      })),
      empty: "The inspection pod found no usable device on this node."
    }, {
      k: "mgmt_nic",
      label: "Management NIC",
      type: "select",
      required: true,
      options: h.nics.map(n => ({
        v: n.name,
        l: `${n.name} · ${n.address} · ${n.speed}G · ${n.state}`
      }))
    }, {
      k: "data_nics",
      label: mp ? "Data NICs — exactly two required" : "Data NIC",
      type: "multiselect",
      required: true,
      options: h.nics.map(n => ({
        v: n.name,
        l: `${n.name} · ${n.address} · ${n.speed}G · socket ${n.socket} · ${n.state}`
      }))
    }, {
      k: "n22",
      type: "note",
      label: mp ? "This cluster uses multipathing, so every storage node needs two data NICs. Pick two on different NUMA sockets where possible." : "This cluster does not use multipathing, so a single data NIC is expected."
    }, {
      k: "n4",
      type: "note",
      label: "After configuration the host is labelled and appears as available — you can then start a storage node on it."
    }],
    run: v => api.hostConfigure(h.id, v)
  };
};
const newPoolDialog = cluster => ({
  title: `New pool on ${cluster.name}`,
  confirm: "Create pool",
  done: "Pool created",
  desc: "A pool groups volumes and carries their QoS ceiling. Bi-directional DH-CHAP is decided here and cannot be changed afterwards.",
  fields: [{
    k: "name",
    label: "Pool name",
    type: "text",
    required: true,
    placeholder: "prod-oltp"
  }, {
    k: "dhchap_bidirectional",
    label: "Bi-directional DH-CHAP",
    type: "checkbox",
    def: false
  }, {
    k: "n70",
    type: "note",
    label: "With DH-CHAP the host and the subsystem authenticate each other on every connection. It applies to every volume in the pool, is fixed for the pool's lifetime, and cannot be added later — create a second pool if you need both."
  }, {
    k: "max_rw_iops",
    label: "Max IOPS",
    type: "number",
    min: 0,
    def: 0,
    sub: "0 = unlimited"
  }, {
    k: "max_rw_mbytes",
    label: "Max throughput",
    type: "number",
    min: 0,
    def: 0,
    sub: "MB/s, 0 = unlimited"
  }],
  run: v => api.poolCreate(cluster.id, v)
});

// A plan needs its sites up front: DRCluster and DRPolicy fields are immutable,
// so every pair a method could ever use is derived at creation.
const newPlanDialog = sites => ({
  title: "New protection plan",
  confirm: "Create plan",
  done: "Plan created",
  desc: "A plan names the sites it spans and the storage profile its volumes come from. Methods are added afterwards — each one declares a protection relationship and derives its own policy and replication class.",
  fields: [{
    k: "name",
    label: "Plan name",
    type: "text",
    required: true,
    placeholder: "gold",
    sub: "becomes the storageID suffix and the vault bucket name"
  }, {
    k: "site_names",
    label: "Sites",
    type: "multiselect",
    required: true,
    options: (sites || []).map(s => ({
      v: s.name,
      l: `${s.name} · ${s.region}`
    })),
    sub: "at least two. Two sites in one region can mirror synchronously; a site in another region cannot"
  }, {
    k: "storage_profile",
    label: "Storage profile",
    type: "select",
    def: "sb-nvme-gold",
    options: [{
      v: "sb-nvme-gold",
      l: "sb-nvme-gold"
    }, {
      v: "sb-nvme-standard",
      l: "sb-nvme-standard"
    }],
    sub: "the StorageClass, and the storageID Ramen intersects across sites"
  }, {
    k: "n98",
    type: "note",
    label: "The storageID granularity has to be at least as coarse as an application's volume set — Ramen derives the consistency-group boundary from it, so volumes on different storageIDs cannot be restored crash-consistently together."
  }],
  run: v => api.planCreate(v)
});
const addMethodDialog = plan => ({
  title: `Add a method to ${plan.name}`,
  confirm: "Add method",
  done: "Method added",
  desc: "A method is one declared protection relationship. All three types are configured identically and travel the same path to the driver; what differs is the parameters the class carries and therefore what the driver does with the data.",
  fields: v => [{
    k: "name",
    label: "Method name",
    type: "text",
    required: true,
    placeholder: "regional"
  }, {
    k: "type",
    label: "Type",
    type: "select",
    required: true,
    def: "async",
    options: [{
      v: "sync",
      l: "synchronous — inline mirror to a peer, RPO 0, failback yes"
    }, {
      v: "async",
      l: "asynchronous — block delta per epoch to a peer, RPO = interval, failback yes"
    }, {
      v: "snapshot-s3",
      l: "generation vault — immutable snapshots to S3, generation select, no failback"
    }]
  }, {
    k: "target",
    label: "Target site",
    type: "select",
    required: true,
    options: plan.siteNames.slice(1).map(s => ({
      v: s,
      l: s
    })),
    sub: v.type === "sync" ? "must be in the same region as the source site" : v.type === "snapshot-s3" ? "the site a restore materialises on — the bucket itself is not a site" : "the peer that holds the replica"
  }, v.type !== "sync" ? {
    k: "interval",
    label: "Interval",
    type: "text",
    required: true,
    def: v.type === "snapshot-s3" ? "1h" : "5m",
    placeholder: "5m",
    sub: "the target RPO. Emitted to the policy and to the class parameters as one value"
  } : null, v.type === "snapshot-s3" ? {
    k: "bucket",
    label: "Bucket",
    type: "text",
    def: `sb-vault-${plan.name}`
  } : null, v.type === "snapshot-s3" ? {
    k: "retention_hourly",
    label: "Hourly generations",
    type: "number",
    def: 24,
    min: 0
  } : null, v.type === "snapshot-s3" ? {
    k: "retention_daily",
    label: "Daily generations",
    type: "number",
    def: 14,
    min: 0
  } : null, v.type === "snapshot-s3" ? {
    k: "retention_weekly",
    label: "Weekly generations",
    type: "number",
    def: 8,
    min: 0
  } : null, v.type === "snapshot-s3" ? {
    k: "immutable",
    label: "Object Lock (compliance)",
    type: "checkbox",
    def: true,
    sub: "generations cannot be deleted before their lock expires — the guarantee the vault exists for"
  } : null, v.type === "sync" ? {
    k: "n99",
    type: "note",
    label: "Synchronous protection needs both sites in one region: it is declared by an equal region on the two DRClusters, and the mirror is inline so every write waits for the peer."
  } : null, v.type === "snapshot-s3" ? {
    k: "n100",
    type: "note",
    label: "There is no failback from a vault restore. The source volume is gone or untrusted and the vault holds generations rather than a live peer, so the original site's pre-compromise state cannot be reconstructed — the restored application is re-protected as a new plan with a full baseline."
  } : null].filter(Boolean),
  run: v => api.planAddMethod(plan.id, v)
});
const restoreGenerationDialog = (a, g) => ({
  title: `Restore ${a.namespace}/${a.name} from generation ${g.generation}`,
  danger: true,
  desc: `Taken ${fmtDate(g.at)} · ${g.tier} tier · ${fmtBytes(g.size)}${g.kind === "full" ? " · full baseline" : " · delta"}. The volumes are materialised from this generation's objects at ${a.restoreTargets.join(", ") || "the restore target"}, and the application is brought up from them.`,
  fields: [{
    k: "n101",
    type: "note",
    label: "The generation is pinned out of band immediately before the placement rebind, because PromoteVolume carries no point-in-time argument. The driver resolves the pin when the promote arrives, and the pin is cleared afterwards so a later ordinary failover cannot resolve to an old generation."
  }, g.consistencyGroup ? {
    k: "n102",
    type: "note",
    label: `This generation belongs to consistency group ${g.consistencyGroup}, so its volumes are crash-consistent with each other as of that moment.`
  } : null, {
    k: "n103",
    type: "note",
    label: "Failback will not be available afterwards. Re-protect the restored application as a new plan; it will take a full baseline."
  }].filter(Boolean),
  confirm: "Pin and restore",
  run: () => api.appRestore(a.id, g.generation),
  done: "Generation pinned — the placement control is rebinding"
});

// Both clusters must be StorageClusters in this namespace with status.uuid
// populated. Cross-namespace references are not supported.
const newPairDialog = () => ({
  title: "New replication pair",
  confirm: "Create pair",
  done: "ReplicationPair created",
  desc: "A pair names a source and a target cluster and is reusable: several policies can replicate between the same two clusters on different schedules. Both clusters must be attached to this control plane — cross-namespace references are not supported.",
  fields: [{
    k: "name",
    label: "Name",
    type: "text",
    required: true,
    placeholder: "prod-to-dr",
    sub: "metadata.name, DNS-1123"
  }, {
    k: "sourceCluster",
    label: "Source cluster",
    type: "select",
    required: true,
    load: () => api.clusters().then(cs => cs.filter(c => c.status !== "unready").map(c => ({
      v: c.name,
      l: `${c.name} · ${c.status}`
    }))),
    sub: "the local StorageCluster; its status.uuid must be populated"
  }, {
    k: "targetCluster",
    label: "Target cluster",
    type: "select",
    required: true,
    load: () => api.clusters().then(cs => cs.map(c => ({
      v: c.name,
      l: `${c.name} · ${c.status}`
    }))),
    sub: "immutable after creation — changing a target means a new pair"
  }, {
    k: "n70",
    type: "note",
    label: "The operator creates the backend replication target and records its ID in status.backendTargetID. No policy on the pair replicates until status.ready is true."
  }],
  run: v => api.pairCreateCrd(v.name, v.sourceCluster, v.targetCluster)
});
const newReplPolicyDialog = pair => ({
  title: pair ? `New policy on ${pair.name}` : "New replication policy",
  confirm: "Create policy",
  done: "ReplicationPolicy created",
  desc: "One interval and one snapshot count. There is no tiered retention schedule and no synchronous mode — mode is exactly failover or migration.",
  fields: v => [{
    k: "name",
    label: "Name",
    type: "text",
    required: true,
    placeholder: "prod-to-dr-5m"
  }, pair ? null : {
    k: "pairRef",
    label: "Replication pair",
    type: "select",
    required: true,
    load: () => api.pairs().then(ps => ps.map(p => ({
      v: p.name,
      l: `${p.name} · ${p.sourceCluster} → ${p.targetCluster}${p.ready ? "" : " · not ready"}`
    }))),
    empty: "No ReplicationPair exists yet. Create one first."
  }, {
    k: "mode",
    label: "Mode",
    type: "select",
    def: "failover",
    options: [{
      v: "failover",
      l: "failover — target is a read-only DR standby"
    }, {
      v: "migration",
      l: "migration — planned online cutover to the target"
    }]
  }, {
    k: "interval",
    label: "Interval",
    type: "text",
    def: "5m",
    required: true,
    sub: "rounded to whole minutes, minimum 1m; an unparseable value falls back to 5m"
  }, {
    k: "snapshotRetention",
    label: "Snapshot retention",
    type: "number",
    def: 3,
    min: 2,
    sub: "minimum snapshots kept on the target — the CRD floor is 2"
  }, v.mode === "migration" ? {
    k: "n71",
    type: "note",
    label: "A migration policy cuts over per volume: replicating → cutover_pending → cutover_done, with both clusters up throughout."
  } : {
    k: "n71",
    type: "note",
    label: "Failover is never automatic. Promoting the target is always an explicit ReplicationOps."
  }, {
    k: "n72",
    type: "note",
    label: "Volumes are not added to a policy. A PVC opts in through the storage.simplyblock.io/replication-policy annotation, and the operator creates one ReplicationSlot per bound PVC."
  }].filter(Boolean),
  run: v => api.rpolicyCreateCrd(v.name, {
    pairRef: pair ? pair.name : v.pairRef,
    mode: v.mode,
    interval: v.interval,
    snapshotRetention: Number(v.snapshotRetention)
  })
});

// Membership is an annotation write. Nothing else.
const attachPvcDialog = pol => ({
  title: `Attach a PVC to ${pol.name}`,
  confirm: "Write annotation",
  done: "Annotation written — the operator creates the slot",
  desc: `Sets ${REPL_ANNOTATION} on the PVC. The operator creates a ReplicationSlot named <policy>-<pvc> once the claim is Bound, and owns it from then on.`,
  fields: [{
    k: "pvc",
    label: "PVC",
    type: "select",
    required: true,
    load: () => api.pvcs().then(ps => ps.filter(p => p.status === "Bound").map(p => ({
      v: `${p.name}|${p.namespace}`,
      l: `${p.namespace}/${p.name}${p.replicationPolicy ? ` · already on ${p.replicationPolicy}` : ""}`
    }))),
    sub: "only Bound PVCs — a slot is created once the claim is bound"
  }, {
    k: "n73",
    type: "note",
    label: "Annotating the StorageClass instead replicates every volume it provisions. Where both carry the annotation, the PVC wins."
  }, {
    k: "n74",
    type: "note",
    label: "Repointing a PVC that is already replicating is a detach followed by a fresh attach, so the new target takes a full copy."
  }],
  run: v => {
    const [name, namespace] = v.pvc.split("|");
    return api.pvcSetReplPolicy(name, namespace, pol.name);
  }
});
const detachPvcDialog = slot => ({
  title: `Detach ${slot.pvcRef} from ${slot.policyRef}?`,
  danger: true,
  desc: `Clears ${REPL_ANNOTATION} on the PVC. The operator deletes the replication snapshots on both sides and then removes the slot. The source volume itself is untouched.`,
  confirm: "Clear annotation",
  done: "Annotation cleared",
  run: () => api.pvcSetReplPolicy(slot.pvcRef, slot.pvcNamespace, null)
});

// Every failover, failback and cutover is a ReplicationOps. Scope decides how
// much it covers; failback cannot be target-scoped.
const replOpsDialog = ({
  action,
  scope,
  ref,
  obj
}) => {
  const meta = OPS_ACTION_META[action] || {};
  const n = obj && obj.counts ? obj.counts.slots : null;
  return {
    title: `${meta.label || action} — ${scope} ${ref}`,
    danger: !!meta.danger,
    desc: `${meta.desc} Scope ${scope}: ${SCOPE_HINT[scope]}${n != null ? ` (${n} volume(s))` : ""}.`,
    fields: [action === "migration" ? {
      k: "deleteSource",
      label: "Delete the source volume after the cutover",
      type: "checkbox",
      def: false,
      sub: "spec.deleteSource — only meaningful for a migration"
    } : null, action === "failback" ? {
      k: "sourceClusterID",
      label: "Recover into a different cluster",
      type: "text",
      placeholder: "leave empty for the original source",
      sub: "spec.sourceClusterID — omit to recover to the original source"
    } : null, action === "failback" ? {
      k: "n75",
      type: "note",
      label: "A short write freeze holds while the final delta transfers. Plan a maintenance window."
    } : null, {
      k: "n76",
      type: "note",
      label: "Per-volume outcomes are independent: one volume can fail while the rest succeed, and the operation as a whole then ends Failed."
    }, {
      k: "n77",
      type: "note",
      label: "The operation is one-shot. Once it reaches Succeeded or Failed it is never re-run — a repeat needs a new ReplicationOps."
    }, obj && obj.activeOpsRef ? {
      k: "n78",
      type: "note",
      label: `${obj.activeOpsRef} currently holds the lock. This operation waits for it rather than failing.`
    } : null].filter(Boolean),
    confirm: meta.label || action,
    done: "ReplicationOps created — it runs to a terminal phase",
    run: v => api.replOpsCreate(Object.assign({
      action,
      scope,
      ref
    }, action === "migration" && v.deleteSource ? {
      deleteSource: true
    } : {}, action === "failback" && v.sourceClusterID ? {
      sourceClusterID: v.sourceClusterID
    } : {}))
  };
};
const volumeOpsDialog = slot => ({
  title: `Operate on ${slot.pvcRef}`,
  confirm: "Create operation",
  desc: "A volume-scoped ReplicationOps affects exactly this slot. Which actions apply depends on the state it is in.",
  fields: v => [{
    k: "action",
    label: "Action",
    type: "select",
    required: true,
    def: slot.state === "failed_over" ? "failback" : slot.state === "cutover_pending" ? "migration" : "failover",
    options: [{
      v: "failover",
      l: "fail over — promote the target, unplanned"
    }, {
      v: "failback",
      l: "fail back — restore the source as primary"
    }, {
      v: "migration",
      l: "cut over — commit the planned migration"
    }]
  }, v.action === "migration" ? {
    k: "deleteSource",
    label: "Delete the source volume after the cutover",
    type: "checkbox",
    def: false
  } : null, slot.state === "error" ? {
    k: "n79",
    type: "note",
    label: `This slot is in error: ${slot.message} The backend is likely to refuse the operation for it.`
  } : null, {
    k: "n80",
    type: "note",
    label: `Current state: ${slot.state}. ${smeta(slot.state).hint}`
  }].filter(Boolean),
  run: v => api.replOpsCreate(Object.assign({
    action: v.action,
    scope: "volume",
    ref: slot.name
  }, v.action === "migration" && v.deleteSource ? {
    deleteSource: true
  } : {}))
});
const newPairDialogLegacy = () => ({
  title: "Pair two clusters",
  confirm: "Create pair",
  desc: "A pair is a one-way link. Replication policies attach to pairs, so replicating both ways means creating both a→b and b→a. Fan-out is the same: one pair per route.",
  fields: [{
    k: "source_cluster_id",
    label: "Source cluster",
    type: "select",
    required: true,
    load: () => api.clusters().then(cs => cs.filter(c => c.caps.async_replication).map(c => ({
      v: c.id,
      l: `${c.name} · ${c.siting}`
    }))),
    empty: "No cluster available as a replication source."
  }, {
    k: "target_cluster_id",
    label: "Target cluster",
    type: "select",
    required: true,
    load: () => api.clusters().then(cs => cs.filter(c => c.drEligible).map(c => ({
      v: c.id,
      l: `${c.name} · ${c.siting} · ${fmtBytes(c.capacity.total - c.capacity.used)} free`
    }))),
    empty: "No cluster is qualified as a DR target."
  }, {
    k: "bandwidth_mbit",
    label: "Provisioned link bandwidth",
    unit: "Mbit/s",
    type: "number",
    min: 100,
    def: 10000
  }, {
    k: "n6",
    type: "note",
    label: "Only clusters flagged as DR-eligible can be targets. The pair starts in the pairing state until the first handshake succeeds."
  }],
  run: v => api.pairCreate(v)
});
const RET_INTERVALS = ["5m", "15m", "30m", "1h", "6h", "12h", "1d", "7d"];
const newRPolicyDialog = ctx => ({
  title: "Create a replication policy",
  confirm: "Create policy",
  desc: "An asynchronous policy is a cluster pair plus a consistency group — the group already carries the frequency and retention, so the policy needs nothing else. A synchronous policy is a stretched cluster and its zones, and has no schedule at all.",
  fields: v => {
    const sync = v.mode === "synchronous";
    return [{
      k: "name",
      label: "Policy name",
      type: "text",
      placeholder: "dr-prod-eu-to-us-east",
      required: true
    }, {
      k: "mode",
      label: "Mode",
      type: "select",
      def: ctx && ctx.mode ? ctx.mode : "asynchronous",
      options: [{
        v: "asynchronous",
        l: "Asynchronous — between two clusters"
      }, {
        v: "synchronous",
        l: "Synchronous — across zones of one cluster"
      }]
    }, sync ? {
      k: "source_cluster_id",
      label: "Stretched cluster",
      type: "select",
      required: true,
      load: () => api.clusters().then(cs => cs.filter(c => c.counts.zones > 1).map(c => ({
        v: c.id,
        l: `${c.name} · ${c.counts.zones} zones`
      }))),
      empty: "No cluster spans two or more zones. Assign a cluster to several zones first."
    } : null, sync ? {
      k: "zone_ids",
      label: "Zones in the write quorum",
      type: "multiselect",
      required: true,
      load: () => api.zones().then(ss => ss.map(s => ({
        v: s.id,
        l: `${s.name} · ${s.location}`
      }))),
      empty: "No zones defined."
    } : null, sync ? null : {
      k: "pair_id",
      label: "Cluster pair",
      type: "select",
      required: true,
      load: () => api.pairs().then(ps => ps.filter(p => p.status !== "unreachable").map(p => ({
        v: p.id,
        l: `${regName(p.sourceClusterId)} → ${regName(p.targetClusterId)} · ${p.link.rtt_ms} ms`
      }))),
      empty: "No usable cluster pair. Pair two clusters first."
    }, sync ? null : {
      k: "cg_id",
      label: "Consistency group",
      type: "select",
      required: true,
      load: () => api.pairs().then(ps => {
        const p2 = ps.find(x => x.id === v.pair_id);
        return p2 ? api.cgroups(p2.sourceClusterId) : Promise.resolve([]);
      }).then(gs => gs.filter(g => g.replicationConfig).map(g => ({
        v: g.id,
        l: `${g.name} · ${g.counts.volumes} volumes · every ${g.replicationConfig.frequency} min · ${g.replicationConfig.retention.reduce((a2, r) => a2 + r.keep, 0)} generations`
      }))).catch(() => []),
      empty: "No consistency group on the pair's source cluster has a replication cadence. Attach replication to a group first — the group owns the frequency and retention."
    }, sync ? null : {
      k: "n84",
      type: "note",
      label: "The policy replicates exactly that group's volumes, on the cadence the group defines. Change the frequency or the retention on the group, not here."
    }, sync ? null : {
      k: "retention",
      label: "Retained snapshot generations",
      type: "schedule",
      def: [{
        interval: "5m",
        keep: 10
      }, {
        interval: "15m",
        keep: 4
      }, {
        interval: "1h",
        keep: 11
      }, {
        interval: "1d",
        keep: 6
      }]
    }, sync ? null : {
      k: "consistency_group",
      label: "Create a consistency group for the member volumes",
      type: "checkbox",
      def: true
    }, sync ? null : {
      k: "failback_mode",
      label: "Failback policy",
      type: "select",
      def: "manual",
      options: [{
        v: "manual",
        l: "Manual — operator initiates failback"
      }, {
        v: "automatic",
        l: "Automatic — on source recovery"
      }]
    }, sync ? null : {
      k: "resync_full",
      label: "Full resync on failback (instead of delta)",
      type: "checkbox"
    }, {
      k: "n7",
      type: "note",
      label: sync ? "Writes are acknowledged only when every listed zone has them — no frequency, no backlog and no consistency group. Volumes can be added to and removed from the policy at any time." : "Volumes are attached after the policy exists. A policy can only be deleted once no volumes are attached."
    }].filter(Boolean);
  },
  run: v => api.rpolicyCreate(v)
});
const newMigrationDialog = cluster => ({
  title: "Migrate storage",
  confirm: "Start migration",
  desc: "Within a cluster, volumes move between nodes by instant migration. Between clusters or zones, asynchronous replication ships the data and a brief IO freeze rolls the NVMe paths over at the end.",
  fields: v => {
    const cross = v.mode === "cross_cluster";
    return [{
      k: "name",
      label: "Migration name",
      type: "text",
      placeholder: "move-prod-to-dc2",
      required: true
    }, {
      k: "mode",
      label: "Mechanism",
      type: "select",
      def: "intra_cluster",
      options: [{
        v: "intra_cluster",
        l: "Within the cluster — instant volume migration"
      }, {
        v: "cross_cluster",
        l: "Between clusters / zones — replicate, then roll over"
      }]
    }, {
      k: "scope",
      label: "Scope",
      type: "select",
      def: "volumes",
      options: [{
        v: "volumes",
        l: "Selected volumes"
      }, {
        v: "cluster",
        l: "The whole cluster"
      }]
    }, v.scope !== "cluster" ? {
      k: "lvol_ids",
      label: "Volumes to migrate",
      type: "multiselect",
      required: true,
      load: () => api.clusterVolumes(cluster.id).then(vs => vs.filter(x => x.status === "online").map(x => ({
        v: x.id,
        l: `${x.name} · ${x.poolName} · ${fmtBytes(x.capacity.used)} used`
      }))),
      empty: "No online volume in this cluster."
    } : null, cross ? {
      k: "target_cluster_id",
      label: "Target cluster",
      type: "select",
      required: true,
      load: () => api.pairs().then(ps => ps.filter(p => p.sourceClusterId === cluster.id && p.status !== "unreachable").map(p => ({
        v: p.targetClusterId,
        l: `${regName(p.targetClusterId)} · ${p.link.rtt_ms} ms`
      }))),
      empty: "No usable cluster pair from this cluster. Pair it with the target first."
    } : null, cross ? null : {
      k: "target_zone_id",
      label: "Target zone",
      type: "select",
      load: () => api.clusterZones(cluster.id).then(ss => ss.map(x => ({
        v: x.id,
        l: `${x.name} · ${x.location}`
      }))),
      empty: "This cluster spans a single zone — targets come from the taint alone."
    }, {
      k: "target_taint",
      label: "Target host taint",
      type: "text",
      def: "simplyblock.io/migration-target=true"
    }, cross ? {
      k: "freeze_threshold_mb",
      label: "Freeze threshold",
      unit: "MB",
      type: "number",
      min: 16,
      def: 256
    } : null, cross ? {
      k: "iteration_limit",
      label: "Max snapshot iterations",
      type: "number",
      min: 2,
      def: 12
    } : null, {
      k: "follow_workload",
      label: "Follow the workload",
      type: "checkbox",
      def: true
    }, {
      k: "n27",
      type: "note",
      label: cross ? "Snapshots are taken iteratively until the outstanding delta is under the freeze threshold. You then trigger the cutover, which freezes IO for a moment and rolls the NVMe paths over to the target." : "Taint the destination hosts first — only tainted hosts that already run a storage node can receive volumes."
    }].filter(Boolean);
  },
  run: v => api.migrationCreate(cluster.id, v)
});
const protectAppDialog = () => ({
  title: "Protect an application",
  confirm: "Protect",
  desc: "An application binds a workload to a protection plan. The plan's methods become its legs; Ramen drives one of them, and the rest replicate in the data plane with identical parameters.",
  fields: v => [{
    k: "plan_id",
    label: "Protection plan",
    type: "select",
    required: true,
    load: () => api.plans().then(ps => ps.filter(p => p.methods.length).map(p => ({
      v: p.id,
      l: `${p.name} · ${p.methods.map(m => `${m.name} (${mmeta(m.type).short})`).join(", ")} · ${p.siteNames.length} sites`
    }))),
    empty: "No plan declares a method yet. Create a plan under Disaster recovery and add at least one method."
  }, {
    k: "preferred_site",
    label: "Preferred site",
    type: "select",
    required: true,
    load: () => v.plan_id ? api.plan(v.plan_id).then(p => p.siteNames.map(s => ({
      v: s,
      l: s
    }))) : Promise.resolve([]),
    sub: "where the workload normally runs — becomes DRPC.spec.preferredCluster",
    empty: "Choose a plan first."
  }, {
    k: "orchestrated_method",
    label: "Orchestrated method",
    type: "select",
    required: true,
    load: () => v.plan_id ? api.plan(v.plan_id).then(p => p.methods.filter(m => m.type !== "snapshot-s3").map(m => ({
      v: m.name,
      l: `${m.name} · ${mmeta(m.type).label} → ${m.target}`
    }))) : Promise.resolve([]),
    sub: "the one method Ramen drives through this application's single placement control",
    empty: "This plan declares only a vault method, so that is what Ramen will drive."
  }, {
    k: "n89",
    type: "note",
    label: "A placement control selects PVCs by label, so two over the same PVCs would both claim them — the documented outcome is data corruption. One application therefore names one orchestrated method; the others still replicate, and report their lag out of band."
  }, {
    k: "app_name",
    label: "Application name",
    type: "text",
    placeholder: "postgres-ha",
    required: true
  }, {
    k: "namespace",
    label: "Namespace",
    type: "text",
    placeholder: "db-prod",
    required: true
  }, {
    k: "app_kind",
    label: "Deployed as",
    type: "select",
    def: "ApplicationSet",
    options: [{
      v: "ApplicationSet",
      l: "ApplicationSet"
    }, {
      v: "Subscription",
      l: "Subscription"
    }, {
      v: "VirtualMachine",
      l: "VirtualMachine"
    }]
  }, {
    k: "pvc_selector_key",
    label: "PVC selector label",
    type: "text",
    def: "app.kubernetes.io/name",
    required: true
  }, {
    k: "pvc_selector_value",
    label: "Selector value",
    type: "text",
    placeholder: "postgres",
    required: true,
    sub: "never leave the selector empty — an empty selector claims every PVC in the namespace"
  }, {
    k: "kube_object_protection",
    label: "Also protect Kubernetes objects",
    type: "checkbox",
    def: true
  }, {
    k: "n28",
    type: "note",
    label: "Without Kubernetes object protection a failover moves the volumes only: the workload has to be recreated by hand and the application parks in WaitForUser."
  }],
  run: v => api.appProtect(v)
});
const newCgroupDialog = cluster => ({
  title: "Create a consistency group",
  confirm: "Create group",
  desc: "A consistency group is a named set of volumes. Group snapshots capture every member at the same instant so they restore to one common point in time.",
  fields: [{
    k: "name",
    label: "Group name",
    type: "text",
    placeholder: "sap-hana-prod",
    required: true
  }, {
    k: "lvol_ids",
    label: "Member volumes",
    type: "multiselect",
    required: true,
    load: () => api.clusterVolumes(cluster.id).then(vs => vs.filter(v => v.status === "online").map(v => ({
      v: v.id,
      l: `${v.name} · ${v.poolName} · ${fmtBytes(v.capacity.total)}` + ((v.consistencyGroups || []).length ? ` · also in ${v.consistencyGroups.map(g => g.name).join(", ")}` : "")
    }))),
    empty: "No online volume in this cluster."
  }, {
    k: "n82",
    type: "note",
    label: "Groups may overlap: a volume can belong to several consistency groups. That is how a differently-scoped crash-consistent set gets its own protection without disturbing an existing group."
  }, {
    k: "n18",
    type: "note",
    label: "At least two volumes. A volume can belong to only one consistency group. Groups exist independently of replication."
  }],
  run: v => api.cgroupCreate(cluster.id, v)
});
const kmsDialog = c => {
  const k = c.kms || {};
  return {
    title: `External key management for ${c.name}`,
    confirm: "Save KMS configuration",
    desc: "The cluster never stores a data encryption key in the clear: each encrypted volume's key is wrapped by a key held in your KMS. Saving does not rekey anything — existing volumes keep referencing their current key.",
    fields: v => {
      const p = v.provider || k.provider || "hashicorp_vault";
      const base = [{
        k: "provider",
        label: "Provider",
        type: "select",
        def: p,
        options: [{
          v: "hashicorp_vault",
          l: "HashiCorp Vault"
        }, {
          v: "aws_kms",
          l: "AWS KMS"
        }, {
          v: "azure_key_vault",
          l: "Azure Key Vault"
        }, {
          v: "gcp_kms",
          l: "Google Cloud KMS"
        }, {
          v: "kmip",
          l: "KMIP appliance"
        }]
      }];
      const perProvider = p === "hashicorp_vault" ? [{
        k: "address",
        label: "Vault address",
        type: "text",
        def: k.address || "",
        placeholder: "https://vault.internal:8200",
        required: true
      }, {
        k: "namespace",
        label: "Namespace",
        type: "text",
        def: k.namespace || "",
        placeholder: "admin/storage (Enterprise)"
      }, {
        k: "auth_method",
        label: "Auth method",
        type: "select",
        def: k.auth_method || "kubernetes",
        options: [{
          v: "kubernetes",
          l: "Kubernetes service account"
        }, {
          v: "approle",
          l: "AppRole"
        }, {
          v: "token",
          l: "Token"
        }]
      }, {
        k: "auth_role",
        label: "Role",
        type: "text",
        def: k.auth_role || "simplyblock-storage"
      }, {
        k: "mount_path",
        label: "Transit mount",
        type: "text",
        def: k.mount_path || "transit",
        required: true
      }] : p === "aws_kms" ? [{
        k: "address",
        label: "Endpoint",
        type: "text",
        def: k.address || "https://kms.eu-central-1.amazonaws.com",
        required: true
      }, {
        k: "region",
        label: "Region",
        type: "text",
        def: k.region || "eu-central-1",
        required: true
      }, {
        k: "auth_method",
        label: "Auth method",
        type: "select",
        def: "irsa",
        options: [{
          v: "irsa",
          l: "IRSA — service account role"
        }, {
          v: "access_key",
          l: "Access key"
        }]
      }, {
        k: "auth_role",
        label: "Role ARN",
        type: "text",
        def: k.auth_role || "arn:aws:iam::…:role/simplyblock-kms"
      }] : p === "azure_key_vault" ? [{
        k: "address",
        label: "Vault URI",
        type: "text",
        def: k.address || "https://sb-kv.vault.azure.net",
        required: true
      }, {
        k: "auth_method",
        label: "Auth method",
        type: "select",
        def: "workload_identity",
        options: [{
          v: "workload_identity",
          l: "Workload identity"
        }, {
          v: "client_secret",
          l: "Client secret"
        }]
      }, {
        k: "auth_role",
        label: "Client / tenant id",
        type: "text",
        def: k.auth_role || ""
      }] : p === "gcp_kms" ? [{
        k: "address",
        label: "Key ring resource",
        type: "text",
        def: k.address || "projects/…/locations/…/keyRings/simplyblock",
        required: true
      }, {
        k: "auth_method",
        label: "Auth method",
        type: "select",
        def: "workload_identity",
        options: [{
          v: "workload_identity",
          l: "Workload identity"
        }, {
          v: "service_account_key",
          l: "Service account key"
        }]
      }] : [{
        k: "address",
        label: "KMIP endpoint",
        type: "text",
        def: k.address || "kmip.internal:5696",
        required: true
      }, {
        k: "auth_method",
        label: "Auth method",
        type: "select",
        def: "certificate",
        options: [{
          v: "certificate",
          l: "Client certificate"
        }]
      }, {
        k: "auth_role",
        label: "Client certificate secret",
        type: "text",
        def: k.auth_role || "sb-kmip-client"
      }];
      return base.concat(perProvider, [{
        k: "key_name",
        label: "Key name",
        type: "text",
        def: k.key_name || `sb-${c.name}-dek`,
        required: true
      }, {
        k: "key_type",
        label: "Key type",
        type: "select",
        def: k.key_type || "aes256-gcm96",
        options: [{
          v: "aes256-gcm96",
          l: "aes256-gcm96"
        }, {
          v: "chacha20-poly1305",
          l: "chacha20-poly1305"
        }]
      }, {
        k: "rotation_days",
        label: "Rotate every",
        unit: "days, 0 = never",
        type: "number",
        min: 0,
        def: k.rotation_days || 0
      }, {
        k: "verify_tls",
        label: "Verify TLS certificate",
        type: "checkbox",
        def: k.verify_tls !== false
      }, {
        k: "n19",
        type: "note",
        label: "Applies to every encrypted volume and bucket in this cluster. Unencrypted volumes are unaffected. This is separate from the S3 target the cluster writes backups to."
      }]);
    },
    run: v => api.clusterSetKms(c.id, v)
  };
};
const fileStorageDialog = c => {
  const f = c.fileStorage || {};
  return {
    title: `File storage for ${c.name}`,
    confirm: "Apply",
    desc: "A pNFS filesystem over this cluster's capacity. Every worker becomes a pNFS client and reads and writes directly against the storage nodes; the kernel NFS server on one control-plane worker serves metadata only.",
    fields: v => [{
      k: "enabled",
      label: "Enable file storage (RWX)",
      type: "checkbox",
      def: f.enabled !== false
    }, v.enabled === false ? {
      k: "n30",
      type: "note",
      label: "Disabling is refused while any ReadWriteMany claim still exists."
    } : null, v.enabled === false ? null : {
      k: "mds_host_id",
      label: "NFS metadata server",
      type: "select",
      load: () => api.hosts(c.id).then(hs => hs.filter(h => h.controlPlane && h.status === "available").map(h => ({
        v: h.id,
        l: `${h.hostname}${f.mds_host && f.mds_host.uuid === h.id ? " (current)" : ""}`
      }))),
      empty: "No control-plane worker available. File storage needs one to run the metadata service."
    }, v.enabled === false ? null : {
      k: "export_root",
      label: "Export root",
      type: "text",
      def: f.export_root || "/export/simplyblock",
      required: true
    }, v.enabled === false ? null : {
      k: "layout_type",
      label: "pNFS layout",
      type: "select",
      def: f.layout_type || "flexfile",
      options: [{
        v: "flexfile",
        l: "Flexible file layout"
      }, {
        v: "block",
        l: "Block layout"
      }]
    }, v.enabled === false ? null : {
      k: "lease_seconds",
      label: "Lease",
      unit: "s",
      type: "number",
      min: 5,
      def: f.lease_seconds || 20
    }, v.enabled === false ? null : {
      k: "grace_seconds",
      label: "Grace period",
      unit: "s",
      type: "number",
      min: 10,
      def: f.grace_seconds || 45
    }, v.enabled === false ? null : {
      k: "failover_budget_seconds",
      label: "Failover budget",
      unit: "s",
      type: "number",
      min: 3,
      def: f.failover_budget_seconds || 8
    }, v.enabled === false ? null : {
      k: "max_exports",
      label: "Max exports",
      type: "number",
      min: 8,
      def: f.max_exports || 128
    }, v.enabled === false ? null : {
      k: "n31",
      type: "note",
      label: "pNFS on the Linux kernel NFS server supports XFS only, so every ReadWriteMany claim is formatted XFS."
    }].filter(Boolean),
    run: v => api.clusterSetFileStorage(c.id, v)
  };
};
const objectStorageDialog = c => {
  const o = c.objectStorage || {};
  return {
    title: `Object storage for ${c.name}`,
    confirm: "Apply",
    desc: "S3 on this cluster's own capacity: object data lands on cluster volumes, object metadata in FoundationDB. One bucket is one filesystem is one logical volume.",
    fields: v => [{
      k: "enabled",
      label: "Enable object storage (S3)",
      type: "checkbox",
      def: o.enabled !== false
    }, v.enabled === false ? {
      k: "n32",
      type: "note",
      label: "Disabling is refused while any bucket still exists."
    } : null, v.enabled === false ? null : {
      k: "endpoint",
      label: "S3 endpoint",
      type: "text",
      def: o.endpoint || `https://s3.${c.name}.simplyblock.internal`,
      required: true
    }, v.enabled === false ? null : {
      k: "region",
      label: "Region",
      type: "text",
      def: o.region || "eu-central-1",
      required: true
    }, v.enabled === false ? null : {
      k: "addressing",
      label: "Addressing style",
      type: "select",
      def: o.addressing || "virtual-hosted",
      options: [{
        v: "virtual-hosted",
        l: "Virtual-hosted"
      }, {
        v: "path",
        l: "Path"
      }]
    }, v.enabled === false ? null : {
      k: "versioning_default",
      label: "Version new buckets by default",
      type: "checkbox",
      def: !!o.versioning_default
    }, v.enabled === false ? null : {
      k: "max_buckets",
      label: "Max buckets",
      type: "number",
      min: 1,
      def: o.max_buckets || 500
    }, v.enabled === false ? null : {
      k: "n33",
      type: "note",
      label: "This is the S3 service the cluster serves. It is separate from the S3 target the cluster writes its own backups to."
    }].filter(Boolean),
    run: v => api.clusterSetObjectStorage(c.id, v)
  };
};
const newBucketDialog = c => ({
  title: "Create a bucket",
  confirm: "Create bucket",
  desc: "A bucket is provisioned as one logical volume. Access is Kubernetes-native: a service account and a secret holding the access key.",
  fields: [{
    k: "name",
    label: "Bucket name",
    type: "text",
    placeholder: "media-assets",
    required: true
  }, {
    k: "pool_id",
    label: "Pool",
    type: "select",
    required: true,
    load: () => api.pools(c.id).then(ps => ps.filter(p => p.enabled).map(p => ({
      v: p.id,
      l: p.name
    }))),
    empty: "No enabled pool in this cluster."
  }, {
    k: "size",
    label: "Filesystem size",
    unit: "GB",
    type: "number",
    min: 10,
    def: 1000,
    required: true
  }, {
    k: "quota",
    label: "Quota",
    unit: "GB, 0 = none",
    type: "number",
    min: 0,
    def: 0
  }, {
    k: "namespace",
    label: "Kubernetes namespace",
    type: "text",
    def: "default",
    required: true
  }, {
    k: "service_account",
    label: "Service account",
    type: "text",
    placeholder: "sb-s3-media-assets"
  }, {
    k: "policy",
    label: "Policy",
    type: "select",
    def: "read-write",
    options: [{
      v: "read-write",
      l: "Read / write"
    }, {
      v: "read-only",
      l: "Read only"
    }, {
      v: "write-only",
      l: "Write only"
    }]
  }, {
    k: "versioning",
    label: "Enable versioning",
    type: "checkbox",
    def: !!(c.objectStorage || {}).versioning_default
  }, {
    k: "object_lock",
    label: "Enable object lock",
    type: "checkbox"
  }, {
    k: "n40",
    type: "note",
    label: "Object lock cannot be turned off again once enabled, and a locked bucket cannot be deleted."
  }, {
    k: "encryption",
    label: "Encrypt with the cluster KMS key",
    type: "checkbox",
    def: true
  }, {
    k: "storage_class",
    label: "Default storage class",
    type: "select",
    def: "standard",
    options: [{
      v: "standard",
      l: "Standard"
    }, {
      v: "infrequent-access",
      l: "Infrequent access"
    }]
  }, {
    k: "owner",
    label: "Owner",
    type: "text",
    placeholder: "platform-team"
  }, {
    k: "tags",
    label: "Bucket tags",
    type: "kv",
    max: 50,
    def: [{
      k: "env",
      v: "prod"
    }, {
      k: "team",
      v: ""
    }],
    hint: "S3 bucket tagging — searchable in the bucket list as key or key=value."
  }],
  run: v => api.bucketCreate(c.id, Object.assign({}, v, {
    size: Number(v.size) * 1e9,
    quota: Number(v.quota) * 1e9,
    tags: Object.fromEntries((v.tags || []).filter(t => t.k).map(t => [t.k.trim(), (t.v || "").trim()]))
  }))
});
const newClusterDialog = () => ({
  title: "Create a storage cluster",
  confirm: "Create cluster",
  desc: "The cluster is bootstrapped on prepared, labelled hosts. A storage node is started on each selected host and their unassigned devices are claimed.",
  fields: v => {
    const edge = v.location_type === "edge";
    return [{
      k: "name",
      label: "Cluster label",
      type: "text",
      placeholder: "prod-eu-central-2",
      required: true
    }, {
      k: "location_type",
      label: "Siting",
      type: "select",
      def: "datacenter",
      options: [{
        v: "datacenter",
        l: "Data center"
      }, {
        v: "edge",
        l: "Edge"
      }]
    }, {
      k: "device_class",
      label: "Device class",
      type: "select",
      def: "nvme",
      options: edge ? [{
        v: "block",
        l: "Block devices"
      }] : [{
        v: "nvme",
        l: "NVMe (PCIe address)"
      }, {
        v: "block",
        l: "Block devices"
      }]
    }, edge ? {
      k: "n5",
      type: "note",
      label: "Edge clusters are block-device only, do not rebalance, run no task engine, and are managed through the Kubernetes API rather than a separate control plane endpoint."
    } : null, {
      k: "ec",
      label: "Stripe — data + parity chunks",
      type: "select",
      def: "2+1",
      options: ["1+1", "2+1", "2+2", "4+1", "4+2", "8+2"].map(x => ({
        v: x,
        l: `${x} (data+parity chunks)`
      }))
    }, {
      k: "zone_ids",
      label: "Zones the cluster spans",
      type: "multiselect",
      required: true,
      load: () => api.zones().then(ss => ss.map(x => ({
        v: x.id,
        l: `${x.name} · ${x.location}`
      }))),
      empty: "No zones defined. Add a zone under Disaster recovery first."
    }, {
      k: "n10",
      type: "note",
      label: "Zones are permanent: they cannot be added or changed after creation, and storage nodes can only ever be started on hosts in these zones."
    }, {
      k: "node_affinity",
      label: "Node affinity",
      type: "select",
      def: "soft",
      options: [{
        v: "none",
        l: "None — free placement"
      }, {
        v: "soft",
        l: "Soft — prefer to keep volumes in place"
      }, {
        v: "strict",
        l: "Strict — volumes can be pinned to a node"
      }]
    }, {
      k: "pod_affinity_enabled",
      label: "Pod affinity — front storage follows the workload",
      type: "checkbox",
      def: true
    }, {
      k: "n25",
      type: "note",
      label: "With pod affinity a volume's primary is moved by instant migration whenever its workload pod is rescheduled to another node."
    }, {
      k: "multipathing_enabled",
      label: "Enable multipathing",
      type: "checkbox",
      def: true
    }, v.multipathing_enabled ? {
      k: "n21",
      type: "note",
      label: "Every storage node must then be given two data NICs, and clients use both paths automatically."
    } : {
      k: "n21",
      type: "note",
      label: "Each storage node gets a single data NIC and clients have one path to it."
    }, {
      k: "sync_replication_enabled",
      label: "Enable synchronous replication",
      type: "checkbox"
    }, v.sync_replication_enabled && (v.zone_ids || []).length < 2 ? {
      k: "n14",
      type: "note",
      label: "Synchronous replication needs at least two zones — pick another zone above."
    } : null, {
      k: "failure_domain_enabled",
      label: "Enable failure domains",
      type: "checkbox",
      def: true
    }, v.failure_domain_enabled ? {
      k: "failure_domain_scope",
      label: "Failure domain granularity",
      type: "select",
      def: "rack",
      options: [{
        v: "rack",
        l: "Rack — node taint"
      }, {
        v: "cabinet",
        l: "Cabinet — node taint"
      }, {
        v: "zone",
        l: "Zone — topology.kubernetes.io/zone"
      }]
    } : null, v.failure_domain_enabled ? {
      k: "n15",
      type: "note",
      label: "Every node must sit in a failure domain, and node counts per domain may differ by at most one. Hosts without the matching taint cannot take a node."
    } : null, {
      k: "backup_enabled",
      label: "Enable backups",
      type: "checkbox",
      def: true
    }, v.backup_enabled ? {
      k: "s3_endpoint",
      label: "S3 endpoint",
      type: "text",
      placeholder: "https://s3.eu-central-1.amazonaws.com",
      required: true
    } : null, v.backup_enabled ? {
      k: "s3_region",
      label: "S3 region",
      type: "text",
      placeholder: "eu-central-1",
      required: true
    } : null, v.backup_enabled ? {
      k: "s3_bucket",
      label: "Bucket",
      type: "text",
      placeholder: "sb-backup-prod",
      required: true
    } : null, v.backup_enabled ? {
      k: "s3_path_prefix",
      label: "Path prefix",
      type: "text",
      placeholder: "clusters/prod/"
    } : null, v.backup_enabled ? {
      k: "s3_access_key_id",
      label: "Access key id",
      type: "text",
      placeholder: "AKIA…",
      required: true
    } : null, v.backup_enabled ? {
      k: "s3_secret_access_key",
      label: "Secret access key",
      type: "text",
      placeholder: "••••",
      required: true
    } : null, v.backup_enabled ? {
      k: "s3_addressing",
      label: "Addressing style",
      type: "select",
      def: "virtual-hosted",
      options: [{
        v: "virtual-hosted",
        l: "Virtual-hosted"
      }, {
        v: "path",
        l: "Path"
      }]
    } : null, v.backup_enabled ? {
      k: "s3_verify_tls",
      label: "Verify TLS certificate",
      type: "checkbox",
      def: true
    } : null, {
      k: "n16",
      type: "note",
      label: "Zones, failure domains and synchronous replication are all fixed at creation and cannot be changed later."
    }, {
      k: "host_ids",
      label: "Prepared hosts to build on",
      type: "multiselect",
      required: true,
      load: () => api.unassignedHosts().then(hs => hs.map(h => ({
        v: h.id,
        l: `${h.hostname} · ${h.sockets} socket(s) · ${h.counts.free} free devices · ${h.vcpu} vCPU`
      }))),
      empty: "No unassigned prepared host. Prepare and label a host first — it then appears here automatically."
    }, {
      k: "n2",
      type: "note",
      label: "A default storage pool is created with the cluster. The cluster reports in_activation until every node is online."
    }].filter(Boolean);
  },
  run: v => api.clusterCreate(Object.assign({}, v, {
    device_class: v.location_type === "edge" ? "block" : v.device_class,
    distr_ndcs: Number(String(v.ec).split("+")[0]),
    distr_npcs: Number(String(v.ec).split("+")[1])
  }))
});
const ACTIONS = {
  deployconfig: o => [o.status === "Draft" ? {
    label: "Approve and deploy",
    icon: "check",
    op: "create",
    dialog: window.approveDialog(o)
  } : null, o.status === "Draft" || o.status === "Failed" ? {
    label: "Delete document",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: o.status === "Draft" ? "The document has never been applied, so deleting it changes nothing on any node." : "The deployment failed. Deleting the document leaves whatever the failed steps already applied — check the cluster before re-drafting.",
      confirm: "Delete document",
      run: () => api.deployConfigDelete(o.name)
    }
  } : {
    label: "Deployed — document is immutable",
    icon: "lock",
    disabled: true,
    hint: "approval is one-way"
  }].filter(Boolean),
  cluster: o => [o.caps.rebalancing ? {
    label: "Rebalance front storage now",
    icon: "gauge",
    dialog: {
      title: `Rebalance ${o.name}?`,
      desc: "Evens out volume placement across the online storage nodes using instant migration — the primary role is handed over without copying data. Each move files an lvol_migration task, which is where it can be followed. Volumes pinned by node affinity are left where they are.",
      confirm: "Rebalance now",
      run: () => api.clusterRebalance(o.id)
    }
  } : null, o.caps.rebalancing ? {
    label: o.autoRebalance.enabled ? "Turn off automatic rebalancing" : "Turn on automatic rebalancing",
    icon: "clock",
    dialog: {
      title: `${o.autoRebalance.enabled ? "Turn off" : "Turn on"} automatic rebalancing for ${o.name}?`,
      desc: o.autoRebalance.enabled ? "Volumes stay where they are until you rebalance the cluster by hand or move one yourself." : "The control plane evens out volume placement across the online storage nodes on its own, by instant migration. Every move files an lvol_migration task.",
      fields: [{
        k: "n74",
        type: "note",
        label: `${o.autoRebalance.moved24h} volume(s) moved in the last 24 hours, ${o.autoRebalance.moved1h} in the last hour.`
      }],
      confirm: o.autoRebalance.enabled ? "Turn off" : "Turn on",
      run: () => api.clusterSetRebalance(o.id, !o.autoRebalance.enabled)
    }
  } : null, o.type === "kubernetes" ? {
    label: "File storage (RWX)…",
    icon: "folder",
    dialog: fileStorageDialog(o)
  } : {
    label: "File storage (RWX)…",
    icon: "folder",
    disabled: true,
    hint: "Kubernetes deployments only"
  }, {
    label: "Object storage (S3)…",
    icon: "cloud",
    dialog: objectStorageDialog(o)
  }, o.objectStorage && o.objectStorage.enabled ? {
    label: "Create bucket…",
    icon: "plus",
    dialog: newBucketDialog(o)
  } : null, {
    label: "KMS — external key management…",
    icon: "lock",
    dialog: kmsDialog(o)
  }, o.kms ? {
    label: "Test KMS connection",
    icon: "gauge",
    run: () => api.clusterTestKms(o.id),
    toast: "Vault connection tested"
  } : null, o.status !== "suspended" && {
    label: "Shut down cluster",
    icon: "power",
    danger: true,
    dialog: {
      title: `Shut down ${o.name}?`,
      danger: true,
      desc: "All storage nodes are stopped and the cluster is suspended. Connected volumes lose their NVMe-oF targets until the cluster is restarted.",
      confirm: "Suspend cluster",
      run: () => api.clusterSuspend(o.id)
    }
  }, o.status === "suspended" && {
    label: "Restart cluster",
    icon: "refresh",
    dialog: {
      title: `Restart ${o.name}?`,
      desc: "The control plane re-activates every storage node in sequence. The cluster reports in_activation until all nodes are online.",
      confirm: "Restart",
      run: () => api.clusterActivate(o.id)
    }
  }, {
    label: "Expand — add storage node",
    icon: "plus",
    dialog: {
      title: "Add a storage node",
      desc: "Pick a prepared host. Its unassigned devices are claimed by the new node, then existing data is rebalanced onto it — the expansion runs asynchronously through adding node, rebalancing data and complete.",
      fields: [{
        k: "host_id",
        label: "Target host",
        type: "select",
        required: true,
        load: () => api.hosts(o.id).then(hs => {
          const ok = hs.filter(h => h.status === "available" && h.counts.nodes < 2 && h.counts.free > 0 && (!(o.zoneIds || []).length || (o.zoneIds || []).includes(h.zoneId)));
          if (!o.fd || !o.fd.enabled) return ok.map(h => ({
            v: h.id,
            l: `${h.hostname} · ${regName(h.zoneId, "zone")} · ${h.counts.free} free devices`
          }));
          const fdOf = h => o.fd.scope === "zone" ? regName(h.zoneId, null) : o.fd.scope === "cabinet" ? h.cabinet : h.rack;
          const counts = {};
          (o.fd.domains || []).forEach(f => counts[f.name] = f.nodes);
          const min = o.fd.domains.length ? Math.min(...o.fd.domains.map(f => f.nodes)) : 0;
          return ok.filter(h => {
            const f = fdOf(h);
            return f && (counts[f] === undefined || counts[f] <= min);
          }).map(h => ({
            v: h.id,
            l: `${h.hostname} · ${o.fd.scope} ${fdOf(h)} · ${h.counts.free} free devices`
          }));
        }),
        empty: "No eligible host. A host must be in one of the cluster's zones, carry a failure-domain taint, and sit in a domain that is not already ahead of the others."
      }, o.fd && o.fd.enabled ? {
        k: "failure_domain",
        label: `Failure domain (${o.fd.scope})`,
        type: "text",
        required: true,
        placeholder: o.fd.domains && o.fd.domains[0] && o.fd.domains[0].name || "rack-1",
        sub: (o.fd.domains || []).length ? "existing: " + o.fd.domains.map(f => f.name).join(", ") : null
      } : null, o.fd && o.fd.enabled ? {
        k: "n71",
        type: "note",
        label: "The failure domain is assigned once, now. It is fixed for the node's lifetime — moving a node between domains means removing it and adding it again."
      } : null, {
        k: "n13",
        type: "note",
        label: o.fd && o.fd.enabled ? `Nodes can only be started on hosts in the cluster's zones. Each ${o.fd.scope} must carry at least two storage nodes and counts may differ by at most one, so expand in pairs` + ((o.fd.domains || []).length ? ` — currently ${o.fd.domains.filter(f => f.name !== "unassigned").map(f => f.name + ": " + f.nodes).join(", ")}.` : ".") : "Storage nodes can only be started on hosts in the zones assigned when the cluster was created."
      }].filter(Boolean),
      confirm: "Add node",
      run: v => api.clusterAddNode(o.id, v.host_id, v.failure_domain)
    }
  }].filter(Boolean),
  host: o => o.status === "discovered" ? [{
    label: "Prepare this worker node",
    icon: "plus",
    dialog: {
      title: `Prepare ${o.hostname}?`,
      desc: "Schedules the simplyblock inspection pod on this Kubernetes worker node. It collects the NUMA topology, the NVMe and block devices and the available NICs. Nothing is configured yet — you pick the resources afterwards.",
      confirm: "Deploy inspection pod",
      run: () => api.hostsPrepare(o.clusterId, [o.id]),
      done: "Inspection pod scheduled"
    }
  }] : o.status === "inspecting" ? [{
    label: "Inspection in progress…",
    icon: "clock",
    disabled: true,
    hint: "waiting for the inspection pod"
  }] : o.status === "inspected" ? [{
    label: "Configure storage plane…",
    icon: "gauge",
    dialog: configureHostDialog(o)
  }] : [{
    label: "Add storage node on host",
    icon: "plus",
    disabled: o.status !== "available" || o.counts.nodes >= 2 || !o.counts.free,
    hint: o.counts.nodes >= 2 ? "host already runs two nodes" : !o.counts.free ? "no unassigned devices" : null,
    dialog: {
      title: `Add a storage node on ${o.hostname}`,
      desc: `${o.counts.free} unassigned device(s) will be claimed by the new node, then existing data is rebalanced onto it.`,
      confirm: "Add node",
      run: () => api.clusterAddNode(o.clusterId, o.id, o.rack || o.zone)
    }
  }, {
    label: "Reserve device for a node",
    icon: "device",
    disabled: !o.counts.free,
    hint: !o.counts.free ? "no unassigned devices" : null,
    dialog: {
      title: "Reserve a host device",
      desc: "The device is earmarked for a storage node. It is picked up on the next node restart.",
      fields: [{
        k: "device_id",
        label: "Unassigned device",
        type: "select",
        required: true,
        options: o.devices.filter(d => !d.assignedNodeId).map(d => ({
          v: d.id,
          l: `${d.kind === "nvme" ? d.pcie : d.blockdev} · socket ${d.socket} · ${fmtBytes(d.size)}${d.reserved ? " (reserved)" : ""}`
        }))
      }, {
        k: "node_id",
        label: "Storage node",
        type: "select",
        required: true,
        load: () => api.nodes(o.clusterId).then(ns => ns.filter(n => o.nodeIds.includes(n.id)).map(n => ({
          v: n.id,
          l: n.hostname
        }))),
        empty: "This host runs no storage node yet."
      }, {
        k: "note",
        type: "note",
        label: "Requires a node restart to take effect."
      }],
      confirm: "Reserve",
      run: v => api.hostReserveDevice(o.id, v.device_id, v.node_id)
    }
  }, {
    label: o.migrationTaint ? "Clear migration target taint" : "Taint as migration target",
    icon: "move",
    dialog: {
      title: o.migrationTaint ? `Clear the taint on ${o.hostname}?` : `Taint ${o.hostname} as a migration target`,
      desc: o.migrationTaint ? "The host stops being offered as a destination for cluster migrations." : "Tainted hosts are the destinations a cluster migration moves front storage onto. The host must already run a storage node to receive volumes.",
      fields: o.migrationTaint ? [] : [{
        k: "taint",
        label: "Taint",
        type: "text",
        def: "simplyblock.io/migration-target=true",
        required: true
      }],
      confirm: o.migrationTaint ? "Clear taint" : "Apply taint",
      run: v => api.hostSetTaint(o.id, o.migrationTaint ? null : v.taint)
    }
  }, {
    label: "Rack & cabinet taints…",
    icon: "zone",
    dialog: {
      title: `Placement of ${o.hostname}`,
      desc: "A host is racked in exactly one zone. The rack and cabinet labels are how operators find it on the floor.",
      fields: [{
        k: "n28",
        type: "note",
        label: `Zone and region come from the node labels topology.kubernetes.io/zone and /region — this host reports ${o.zone || "no zone"}${o.region ? " in " + o.region : ""}. They cannot be set here.`
      }, {
        k: "rack_id",
        label: "Rack",
        type: "text",
        def: o.rack || "",
        placeholder: "r14 (optional)"
      }, {
        k: "cabinet_id",
        label: "Cabinet",
        type: "text",
        def: o.cabinet || "",
        placeholder: "c03 (optional)"
      }, {
        k: "n9",
        type: "note",
        label: `Rack and cabinet come from the worker node's taints and may be absent. Device class is derived from the installed hardware: this host is ${o.hostClass || "not yet inspected"}.`
      }],
      confirm: "Apply",
      run: v => api.hostSetPlacement(o.id, v)
    }
  }],
  node: o => o.op ? [{
    label: `${OP_LABEL[o.op.kind]} in progress…`,
    icon: "clock",
    disabled: true,
    hint: `${o.op.phase} · phase ${o.op.phaseIndex + 1} of ${o.op.phases.length}`
  }] : [o.status === "online" || o.status === "in_restart" ? {
    label: "Shut down node",
    icon: "power",
    danger: true,
    dialog: {
      title: `Shut down ${o.hostname}?`,
      danger: true,
      desc: "The node reports in shutdown and then offline. Nothing is migrated off it — volumes whose primary sits here fail over to their secondaries, and the cluster runs at reduced redundancy until the node is back. A forced shutdown stops the node without waiting for it to close cleanly.",
      fields: [{
        k: "force",
        label: "Force (do not wait for a clean stop)",
        type: "checkbox"
      }],
      confirm: "Shut down",
      run: v => api.nodeShutdown(o.id, v.force)
    }
  } : null, o.status !== "online" && o.status !== "in_restart" ? {
    label: "Restart node",
    icon: "refresh",
    dialog: {
      title: `Restart ${o.hostname}?`,
      desc: "The node reports in restart and then online. It re-registers with the control plane, re-attaches its devices, and its chunks resynchronise from the surviving copies.",
      confirm: "Restart",
      run: () => api.nodeRestart(o.id)
    }
  } : null, {
    label: "Migrate to another host",
    icon: "move",
    dialog: {
      title: `Migrate ${o.hostname}`,
      desc: "The node is restarted on a different, already prepared host. Prepare and label the target host first.",
      fields: [{
        k: "host_id",
        label: "Target host",
        type: "select",
        required: true,
        load: () => Promise.all([api.hosts(o.clusterId), api.cluster(o.clusterId)]).then(([hs, c]) => hs.filter(h => h.status === "available" && h.id !== o.hostId && h.counts.nodes === 0 && (!(c.zoneIds || []).length || (c.zoneIds || []).includes(h.zoneId))).map(h => ({
          v: h.id,
          l: `${h.hostname} · ${regName(h.zoneId, "zone")} · ${h.counts.free} free devices`
        }))),
        empty: "No prepared, empty host in this cluster's zones."
      }],
      confirm: "Queue migration",
      run: v => api.nodeMigrate(o.id, v.host_id)
    }
  }, {
    label: "Add device",
    icon: "plus",
    dialog: {
      title: "Add a device to this node",
      desc: (REG[o.clusterId] || {}).mode === "nvme" ? "NVMe cluster — identify the drive by PCIe address." : "Block device cluster — identify the drive by block device name.",
      fields: (REG[o.clusterId] || {}).mode === "nvme" ? [{
        k: "pcie_address",
        label: "PCIe address",
        type: "text",
        placeholder: "0000:5e:00.0",
        required: true
      }] : [{
        k: "device_name",
        label: "Block device",
        type: "text",
        placeholder: "/dev/sdb",
        required: true
      }],
      confirm: "Add device",
      run: v => api.nodeAddDevice(o.id, v)
    }
  }, {
    label: "Remove node",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Remove ${o.hostname}?`,
      danger: true,
      desc: "The node is drained first: every volume whose primary sits here is moved off by instant migration, without copying data. The node and its device records are then deleted and its host devices released back to the host pool.",
      fields: [{
        k: "n26",
        type: "note",
        label: "Blocked if a volume is pinned to this node by affinity, if there is no other online node to move to, or if the removal would leave failure domains more than one node apart."
      }, {
        k: "confirmName",
        label: "Type the node hostname to confirm",
        type: "text",
        match: o.hostname,
        required: true
      }],
      confirm: "Drain & remove",
      run: () => api.nodeRemove(o.id),
      done: "Node removal started — this runs asynchronously and may take a while"
    }
  }].filter(Boolean),
  // Two different things can happen to a bad device, and the difference matters:
  //   Remove — takes it out of service. Reversible: the cluster still knows the
  //            device and a restart adds it back.
  //   Fail   — excludes it permanently and rebuilds its chunks onto the
  //            remaining devices, restoring fault tolerance without it. Only
  //            from removed or unavailable, and there is no way back.
  device: o => {
    const stopped = o.status === "unavailable" || o.status === "removed";
    const live = o.status === "online" || o.status === "read_only";
    const busy = o.status === "in_removal" || o.status === "in_failure" || o.status === "in_restart";
    const name = o.mode === "nvme" ? o.serial : o.blockdev || o.serial;
    if (busy) return [{
      label: `${STATUS_META[o.status].label}…`,
      icon: "clock",
      disabled: true,
      hint: "follow it under the cluster's Operations tab"
    }];
    if (o.status === "failed") return [{
      label: "Permanently failed",
      icon: "alert",
      disabled: true,
      hint: "its chunks were rebuilt onto the remaining devices; this device cannot be re-added"
    }];
    return [stopped && {
      label: o.status === "removed" ? "Add device back" : "Restart device",
      icon: "refresh",
      dialog: {
        title: o.status === "removed" ? `Add ${name} back?` : `Restart ${name}?`,
        desc: o.status === "removed" ? "Removal is reversible. The node re-attaches the drive, re-admits it to the distribution layer, and its chunks are resynchronised from the surviving copies." : "The node re-attaches the drive and re-admits it to the distribution layer. Its chunks are resynchronised from the surviving copies.",
        confirm: o.status === "removed" ? "Add back" : "Restart",
        run: () => api.deviceRestart(o.id)
      }
    }, {
      label: "Run health check",
      icon: "gauge",
      disabled: !live,
      hint: !live ? "the drive is not attached, so its SMART log cannot be read" : null,
      dialog: {
        title: `Run a health check on ${name}?`,
        desc: "nvme-cli re-reads the drive's SMART log on the node. The report is stored on the device and the health traffic light is rewritten from its verdict — available spare against the 10% threshold, media and integrity errors, and the critical warning bit.",
        confirm: "Run check",
        run: () => api.deviceHealthCheck(o.id),
        done: "Health check running — the report and the health status update when it finishes"
      }
    }, live && {
      label: "Remove device",
      icon: "power",
      danger: true,
      dialog: {
        title: `Remove ${name}?`,
        danger: true,
        desc: "The device is taken out of service. This is a temporary state — the cluster keeps the device and you can add it back at any time. Redundancy is reduced while it is out.",
        fields: [{
          k: "n73",
          type: "note",
          label: "Use this to pull a drive for inspection or replacement. If the drive is not coming back, fail it afterwards so its chunks are rebuilt and fault tolerance is restored without it."
        }],
        confirm: "Remove",
        run: () => api.deviceRemove(o.id),
        done: "Device removal started — follow it under the cluster's Operations tab"
      }
    }, stopped && {
      label: "Fail device (permanent)",
      icon: "alert",
      danger: true,
      removes: true,
      dialog: {
        title: `Permanently fail ${name}?`,
        danger: true,
        desc: "The device is excluded from the cluster for good and every chunk that lived on it is rebuilt onto the remaining devices, which restores fault tolerance without this drive. It cannot be added back afterwards.",
        fields: [{
          k: "n72",
          type: "note",
          label: `Redundancy is currently reduced because this device is ${STATUS_META[o.status].label}. The rebuild moves ${fmtBytes(o.capacity.used)} and restores it. Only fail the device once you are sure the drive will not come back — a removed device can simply be added again.`
        }, {
          k: "confirmName",
          label: "Type the device name to confirm",
          type: "text",
          match: name,
          required: true
        }],
        confirm: "Fail permanently",
        run: () => api.deviceFail(o.id),
        done: "Failure migration started — follow the rebuild under the cluster's Operations tab"
      }
    }].filter(Boolean);
  },
  volume: o => [{
    label: "Expand…",
    icon: "move",
    dialog: {
      title: `Expand ${o.name}`,
      desc: o.pvc ? "Logical volumes can only grow. Expanding here also raises the request on the PVC bound to this volume; the filesystem is extended on the next mount." : "Logical volumes can only grow — shrinking is refused. The filesystem must be extended inside the guest afterwards.",
      fields: [{
        k: "size",
        label: "Provisioned size",
        type: "number",
        unit: "GB",
        def: gb(o.capacity.total),
        min: gb(o.capacity.total),
        required: true
      }, {
        k: "n36",
        type: "note",
        label: `Currently ${fmtBytes(o.capacity.total)}.` + (o.pvc ? ` Bound to PVC ${o.pvc.namespace}/${o.pvc.name}.` : "")
      }],
      confirm: "Expand",
      run: v => api.volumeResize(o.id, Number(v.size) * GBn)
    }
  }, {
    label: "Take snapshot",
    icon: "camera",
    dialog: {
      title: `Snapshot ${o.name}`,
      desc: "A copy-on-write snapshot is taken immediately and chained onto the existing snapshot chain. It consumes space only as the volume diverges.",
      fields: [{
        k: "name",
        label: "Snapshot name",
        type: "text",
        def: `${o.name}-snap`,
        required: true
      }],
      confirm: "Create snapshot",
      run: v => api.volumeSnapshot(o.id, v.name)
    }
  }, {
    label: "Clone",
    icon: "copy",
    dialog: {
      title: `Clone ${o.name}`,
      desc: "Creates a new thin volume in the same pool, backed by a snapshot of this one.",
      fields: [{
        k: "name",
        label: "New volume name",
        type: "text",
        def: `${o.name}-clone`,
        required: true
      }],
      confirm: "Clone",
      run: v => api.volumeClone(o.id, v.name)
    }
  }, {
    label: "Migrate instantly…",
    icon: "move",
    dialog: {
      title: `Move ${o.name} to another node`,
      desc: "Instant volume migration moves the primary role to another storage node without copying data, so the move completes immediately. Use it to follow a workload or to relieve a hot node.",
      fields: [{
        k: "node_id",
        label: "Target primary node",
        type: "select",
        required: true,
        load: () => api.nodes(o.clusterId).then(ns => ns.filter(n => n.status === "online" && (!o.nodes.primary || n.id !== o.nodes.primary.uuid)).map(n => ({
          v: n.id,
          l: `${n.hostname} · ${n.ip}`
        }))),
        empty: "No other online node in this cluster."
      }, {
        k: "reason",
        label: "Reason",
        type: "select",
        def: "manual",
        options: [{
          v: "manual",
          l: "Manual move"
        }, {
          v: "follow_workload",
          l: "Follow the workload"
        }, {
          v: "rebalance",
          l: "Relieve a hot node"
        }]
      }, o.affinity && o.affinity.mode === "node" ? {
        k: "n23",
        type: "note",
        label: `This volume is pinned to ${o.affinity.pinned_node}. Moving it re-pins the affinity to the new node.`
      } : null].filter(Boolean),
      confirm: "Move now",
      run: v => api.volumeMigrate(o.id, v)
    }
  }, {
    label: "Rebalance this volume",
    icon: "gauge",
    dialog: {
      title: `Rebalance ${o.name}?`,
      desc: "Moves the volume to the least loaded online node in the cluster using instant migration. Nothing is copied.",
      confirm: "Rebalance",
      run: () => api.volumeRebalance(o.id)
    }
  }, {
    label: "Affinity…",
    icon: "link",
    dialog: {
      title: `Affinity for ${o.name}`,
      desc: "Node affinity pins the volume's primary to one node. Pod affinity keeps front storage on whichever node the workload pod runs on, moving it instantly when the pod is rescheduled.",
      fields: v2 => [{
        k: "mode",
        label: "Affinity",
        type: "select",
        def: o.affinity ? o.affinity.mode : "none",
        options: [{
          v: "none",
          l: "None — the control plane places it freely"
        }, {
          v: "node",
          l: "Node affinity — pin to a storage node"
        }, {
          v: "pod",
          l: "Pod affinity — follow the workload"
        }]
      }, v2.mode === "node" ? {
        k: "node_id",
        label: "Pin to node",
        type: "select",
        load: () => api.nodes(o.clusterId).then(ns => ns.filter(n => n.status === "online").map(n => ({
          v: n.id,
          l: `${n.hostname}${o.nodes.primary && n.id === o.nodes.primary.uuid ? " (current primary)" : ""}`
        })))
      } : null, v2.mode === "pod" ? {
        k: "workload",
        label: "Workload pod",
        type: "text",
        def: o.affinity && o.affinity.workload ? o.affinity.workload : `${o.name}-0`
      } : null, v2.mode === "pod" ? {
        k: "n24",
        type: "note",
        label: "Requires pod affinity to be enabled on the cluster."
      } : null].filter(Boolean),
      confirm: "Apply",
      run: v => api.volumeSetAffinity(o.id, v)
    }
  }, {
    label: "Snapshot & back up now",
    icon: "cloud",
    dialog: {
      title: `Back up ${o.name}`,
      desc: "Backups are always taken from a snapshot. This takes a snapshot now and appends a version to the volume\u2019s backup chain — the first version is a full copy, every later one a delta.",
      fields: [{
        k: "bucket",
        label: "Bucket location",
        type: "text",
        def: `s3://sb-backup-eu/${o.name}/`,
        required: true
      }, {
        k: "n11",
        type: "note",
        label: "Snapshot and backup version become independent objects: deleting the online snapshot later leaves the backup version untouched."
      }],
      confirm: "Snapshot & back up",
      run: v => api.volumeBackup(o.id, v)
    }
  }, {
    label: "Compression-dedup…",
    icon: "move",
    dialog: {
      title: `Compression-dedup for ${o.name}`,
      desc: "Compression-dedup is a single per-volume switch. Turning it on or off only affects data written from now on — blocks already on disk keep their current form.",
      fields: [{
        k: "compression_dedup_enabled",
        label: "Compression-dedup",
        type: "checkbox",
        def: o.dataReduction
      }, {
        k: "n20",
        type: "note",
        label: "Costs CPU on the storage node and hugepage memory for the fingerprint table."
      }],
      confirm: "Apply",
      run: v => api.volumeSetDataReduction(o.id, v)
    }
  }, {
    label: "QoS limits…",
    icon: "gauge",
    dialog: {
      title: `QoS limits for ${o.name}`,
      desc: "Caps applied by the storage node to this volume. They override the pool defaults.",
      fields: qosFields(o.qos),
      confirm: "Apply limits",
      run: v => api.volumeSetQos(o.id, v)
    }
  }, {
    label: "Backup policy…",
    icon: "clock",
    dialog: {
      title: `Backup policy for ${o.name}`,
      desc: "Link this volume to a cluster backup policy, or detach it.",
      fields: [{
        k: "policy_id",
        label: "Policy",
        type: "select",
        def: o.backupPolicy ? o.backupPolicy.uuid : "",
        load: () => api.policies(o.clusterId).then(ps => [{
          v: "",
          l: "— none —"
        }].concat(ps.map(p => ({
          v: p.id,
          l: `${p.name} · window ${p.window}`
        }))))
      }],
      confirm: "Apply",
      run: v => api.volumeSetPolicy(o.id, v.policy_id)
    }
  }, o.replication ? {
    label: "Detach from replication",
    icon: "link",
    danger: true,
    dialog: {
      title: `Stop replicating ${o.name}?`,
      desc: `The volume leaves ${o.replication.policyName}. Data already replicated to the target is kept, but no further generations are shipped.`,
      confirm: "Detach",
      run: () => api.rpolicyRemoveVolume(o.replication.policyId, o.id)
    }
  } : {
    label: "Attach to replication policy…",
    icon: "shield",
    dialog: {
      title: `Replicate ${o.name}`,
      desc: "Pick a replication policy in this cluster. Asynchronous policies ship deltas over a cluster pair; synchronous policies acknowledge writes at every zone of a stretched cluster.",
      fields: [{
        k: "policy",
        label: "Replication policy",
        type: "select",
        required: true,
        load: () => api.rpolicies(o.clusterId).then(ps => ps.map(p => ({
          v: p.id,
          l: p.mode === "synchronous" ? `${p.name} · synchronous · ${(p.zoneIds || []).length} zones` : `${p.name} · every ${p.frequency} min → ${regName(p.targetClusterId)}`
        }))).catch(() => []),
        empty: "No replication policy in this cluster. Create one under Disaster recovery."
      }],
      confirm: "Attach",
      run: v => api.rpolicyAddVolumes(v.policy, [o.id])
    }
  }, {
    label: "Consistency group…",
    icon: "link",
    hint: (o.consistencyGroups || []).length ? `in ${o.consistencyGroups.map(g => g.name).join(", ")}` : null,
    dialog: {
      title: `Add ${o.name} to a consistency group`,
      desc: "Members of a consistency group are snapshotted together, at one common point in time. A volume can belong to several groups; a group whose protection is already active cannot take new members.",
      fields: [{
        k: "cg",
        label: "Consistency group",
        type: "select",
        required: true,
        load: () => api.cgroups(o.clusterId).then(gs => gs.filter(g => !g.locked && !(o.consistencyGroups || []).some(x => x.uuid === g.id)).map(g => ({
          v: g.id,
          l: `${g.name} · ${g.counts.volumes} volumes`
        }))),
        empty: "No group in this cluster can take this volume — every one either already has it or carries an active policy."
      }],
      confirm: "Add",
      run: v => api.cgroupAddVolumes(v.cg, [o.id])
    }
  }, {
    label: "Delete volume",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: "The volume and all of its snapshots are destroyed. Backups in object storage are kept.",
      fields: [{
        k: "confirmName",
        label: `Type the volume name to confirm`,
        type: "text",
        match: o.name,
        required: true
      }],
      confirm: "Delete",
      run: () => api.volumeDelete(o.id)
    }
  }],
  snapshot: o => [o.backupVersionId ? {
    label: "Already backed up",
    icon: "cloud",
    disabled: true,
    hint: `version ${o.backupVersionId} was taken from this snapshot`
  } : {
    label: "Back up this snapshot",
    icon: "cloud",
    dialog: {
      title: `Back up ${o.name}`,
      desc: "Appends a version to the volume\u2019s backup chain from this snapshot. Afterwards the snapshot and the backup version are independent — deleting the snapshot leaves the version in the bucket.",
      fields: [{
        k: "bucket",
        label: "Bucket location",
        type: "text",
        def: `s3://sb-backup-eu/${o.volumeName}/`,
        required: true
      }],
      confirm: "Back up",
      run: v => api.snapshotBackup(o.id, v.bucket)
    }
  }, {
    label: "Restore to new volume…",
    icon: "refresh",
    op: "create",
    dialog: {
      title: `Restore ${o.name}`,
      desc: "Creates a new volume from this snapshot. The target can be this cluster or any other cluster the control plane manages.",
      fields: [{
        k: "cluster_id",
        label: "Target cluster",
        type: "select",
        required: true,
        def: o.clusterId,
        load: () => api.clusters().then(cs => cs.map(c => ({
          v: c.id,
          l: `${c.name}${c.id === o.clusterId ? " (source)" : ""} · ${c.siting}`
        })))
      }, {
        k: "name",
        label: "New volume name",
        type: "text",
        def: `${o.volumeName}-restored`,
        required: true
      }],
      confirm: "Restore",
      run: v => api.snapshotRestore(o.id, v)
    }
  }, {
    label: "Clone to new volume",
    icon: "copy",
    dialog: {
      title: `Clone ${o.name}`,
      desc: "Creates a thin volume from this snapshot in the same pool.",
      fields: [{
        k: "name",
        label: "New volume name",
        type: "text",
        def: `${o.volumeName}-from-snap`,
        required: true
      }],
      confirm: "Clone",
      run: v => api.snapshotClone(o.id, v.name)
    }
  }, {
    label: "Delete snapshot",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: o.backupVersionId ? `Only the online snapshot is removed. Backup version ${o.backupVersionId} stays in the bucket unchanged, and volumes cloned from this snapshot keep their data.` : "No backup was taken from this snapshot, so this point in time is lost. Volumes cloned from it keep their data.",
      confirm: "Delete",
      run: () => api.snapshotDelete(o.id)
    }
  }],
  backup: o => {
    const vs = o.versions || [];
    const opts = vs.slice().reverse().map(v => ({
      v: v.id,
      l: `${v.id} · ${v.type} · ${fmtDate(v.createdAt)}`
    }));
    return [{
      label: "Restore to new volume…",
      icon: "refresh",
      op: "restore",
      dialog: {
        title: `Restore ${o.chainId}`,
        desc: "Rebuilds a new logical volume from the full version plus every delta up to the version you pick — that version is the point in time you get back.",
        fields: [{
          k: "version_id",
          label: "Restore to version",
          type: "select",
          required: true,
          options: opts,
          empty: "This chain holds no versions."
        }, {
          k: "name",
          label: "New volume name",
          type: "text",
          def: `${o.volumeName}-restored`,
          required: true
        }],
        confirm: "Restore",
        run: v => api.backupRestore(o.id, v.name, v.version_id)
      }
    }, {
      label: "Merge oldest delta into full",
      icon: "move",
      disabled: vs.length < 2,
      hint: vs.length < 2 ? "chain holds a single full version" : null,
      dialog: {
        title: `Merge the earliest delta of ${o.chainId}?`,
        danger: true,
        desc: "The second-earliest version is folded into the full version, which then covers both, and the delta is removed. This is how older retention is aged out — points in time before the merged version become unrecoverable.",
        confirm: "Merge",
        run: () => api.backupMerge(o.id)
      }
    }, {
      label: "Export a version…",
      icon: "ext",
      dialog: {
        title: `Export from ${o.chainId}`,
        desc: "Copies the chain up to the chosen version to another bucket or an external target.",
        fields: [{
          k: "version_id",
          label: "Version",
          type: "select",
          required: true,
          options: opts
        }, {
          k: "destination",
          label: "Destination",
          type: "text",
          def: "s3://sb-archive-cold/",
          required: true
        }],
        confirm: "Export",
        run: v => api.backupExport(o.id, v.destination, v.version_id)
      }
    }, {
      label: "Delete backup chain",
      icon: "trash",
      danger: true,
      removes: true,
      dialog: {
        title: `Delete ${o.chainId}?`,
        danger: true,
        desc: `Deleting a volume backup deletes the entire chain — all ${vs.length} version(s), full and deltas. Online snapshots on the cluster are not touched.`,
        fields: [{
          k: "confirmName",
          label: "Type the chain id to confirm",
          type: "text",
          match: o.chainId,
          required: true
        }],
        confirm: "Delete chain",
        run: () => api.backupDelete(o.id)
      }
    }];
  },
  pool: o => [o.enabled ? {
    label: "Disable pool",
    icon: "power",
    dialog: {
      title: `Disable ${o.name}?`,
      desc: "The pool keeps serving its existing volumes — no I/O is interrupted. New volumes can no longer be provisioned into it until it is enabled again.",
      confirm: "Disable",
      run: () => api.poolDisable(o.id)
    }
  } : {
    label: "Enable pool",
    icon: "refresh",
    dialog: {
      title: `Enable ${o.name}?`,
      desc: "Provisioning of new volumes into this pool is allowed again.",
      confirm: "Enable",
      run: () => api.poolEnable(o.id)
    }
  }, {
    label: "QoS limits…",
    icon: "gauge",
    dialog: {
      title: `QoS limits for pool ${o.name}`,
      desc: "Pool-wide caps. Volumes without their own QoS profile inherit these.",
      fields: qosFields(o.qos),
      confirm: "Apply limits",
      run: v => api.poolSetQos(o.id, v)
    }
  }],
  // A pair is never edited: spec.targetCluster is immutable. It can only be
  // replaced, and the delete is refused while a policy references it.
  pair: o => [{
    label: "Add a policy…",
    icon: "plus",
    op: "create",
    dialog: newReplPolicyDialog(o)
  }, o.counts.slots ? {
    label: "Fail over the whole pair…",
    icon: "move",
    danger: true,
    op: "failover",
    entity: "application",
    dialog: replOpsDialog({
      action: "failover",
      scope: "target",
      ref: o.name,
      obj: o
    })
  } : null, {
    label: "Delete pair",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: o.counts.policies ? `${o.counts.policies} ReplicationPolicy resource(s) still reference this pair. The operator refuses the delete until they are gone.` : "The backend replication target is deleted with the pair.",
      confirm: "Delete",
      run: () => api.pairDeleteCrd(o.name)
    }
  }].filter(Boolean),
  rpolicy: o => [{
    label: "Attach a PVC…",
    icon: "plus",
    dialog: attachPvcDialog(o)
  }, o.counts.slots ? {
    label: o.mode === "migration" ? "Commit the cutover…" : "Fail over this policy…",
    icon: "move",
    danger: o.mode !== "migration",
    dialog: replOpsDialog({
      action: o.mode === "migration" ? "migration" : "failover",
      scope: "policy",
      ref: o.name,
      obj: o
    })
  } : null, o.counts.failedOver ? {
    label: "Fail back this policy…",
    icon: "refresh",
    op: "failback",
    entity: "application",
    dialog: replOpsDialog({
      action: "failback",
      scope: "policy",
      ref: o.name,
      obj: o
    })
  } : null, {
    label: "Delete policy",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: o.counts.slots ? `${o.counts.slots} ReplicationSlot resource(s) still reference this policy. Detach their PVCs first — the operator refuses the delete otherwise.` : "The backend replication policy is deleted with it.",
      confirm: "Delete",
      run: () => api.rpolicyDeleteCrd(o.name)
    }
  }].filter(Boolean),
  slot: o => [{
    label: "Operate on this volume…",
    icon: "move",
    dialog: volumeOpsDialog(o)
  }, {
    label: "Detach from replication",
    icon: "x",
    danger: true,
    removes: true,
    dialog: detachPvcDialog(o)
  }],
  // A terminal operation is spent: there is nothing left to act on.
  replops: o => o.terminal ? [] : [{
    label: `${o.subphase || o.phase} — in progress`,
    icon: "clock",
    disabled: true,
    hint: "a ReplicationOps has no abort: it runs to a terminal phase"
  }],
  zone: () => [],
  migration: o => [o.mode === "cross_cluster" && o.status !== "completed" ? {
    label: "Cut over now",
    icon: "move",
    danger: true,
    disabled: !o.readyToCutover,
    hint: !o.readyToCutover ? `outstanding snapshot ${fmtBytes(o.lastSnapshot)} is above the threshold` : null,
    dialog: {
      title: `Cut over ${o.name}?`,
      danger: true,
      desc: `IO is frozen for roughly ${o.freezeMs} ms while the last ${fmtBytes(o.lastSnapshot)} snapshot is applied and the NVMe paths roll over to ${regName(o.targetClusterId)}. Clients reconnect to the target.`,
      fields: [{
        k: "confirmName",
        label: "Type the migration name to confirm",
        type: "text",
        match: o.name,
        required: true
      }],
      confirm: "Freeze & roll over",
      run: () => api.migrationCutover(o.id)
    }
  } : null, o.status === "paused" ? {
    label: "Resume migration",
    icon: "refresh",
    run: () => api.migrationResume(o.id),
    toast: "Migration resumed"
  } : o.status !== "completed" ? {
    label: "Pause migration",
    icon: "power",
    dialog: {
      title: `Pause ${o.name}?`,
      desc: "Shipping stops and the outstanding delta grows again. Volumes keep serving from the source.",
      confirm: "Pause",
      run: () => api.migrationPause(o.id)
    }
  } : null, o.status !== "completed" ? {
    label: "Cancel migration",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Cancel ${o.name}?`,
      danger: true,
      desc: "Volumes already moved stay where they are. Anything still replicating is abandoned and its target data discarded.",
      confirm: "Cancel migration",
      run: () => api.migrationCancel(o.id)
    }
  } : null].filter(Boolean),
  protectedapp: o => {
    const busy = ["FailingOver", "Relocating"].includes(o.phase);
    const legs = o.legs || [];
    const live = legs.filter(l => l.type !== "snapshot-s3" && l.state !== "Unprotected");
    const canFailover = !!o.failoverTargets.length;
    return [!busy && o.phase !== "FailedOver" && o.phase !== "WaitForUser" ? {
      label: "Fail over to a peer site",
      icon: "move",
      danger: true,
      op: "failover",
      disabled: !canFailover,
      hint: !canFailover ? "no live peer leg — a vault restore leaves no site to fail over to" : null,
      dialog: {
        title: `Fail over ${o.namespace}/${o.name}?`,
        danger: true,
        desc: "The application is brought up at a peer site from the last synced PVC group. This is the unplanned move: anything written after that sync is lost. Fence the old site afterwards if it is still reachable.",
        fields: [{
          k: "target",
          label: "Failover target",
          type: "select",
          required: true,
          options: o.failoverTargets.map(t => {
            const leg = legs.find(l => l.target === t);
            return {
              v: t,
              l: leg ? `${t} · via ${leg.method} · lag ${fmtLag(leg.lagSeconds)}` : t
            };
          })
        }, {
          k: "n90",
          type: "note",
          label: o.group && o.group.lastAt ? `The group was last made safe at ${clockOf(o.group.lastAt)} with ${fmtBytes(o.group.writtenSince)} written since. That is what a failover now would lose.` : "The group has never completed a cycle, so a failover now would bring up empty volumes."
        }, !o.kubeObjectProtection ? {
          k: "n91",
          type: "note",
          label: "Kubernetes object protection is off, so only the volumes travel. The workload has to be recreated by hand and the application parks in WaitForUser."
        } : null].filter(Boolean),
        confirm: "Fail over",
        run: v => api.appFailover(o.id, v.target),
        done: "Failover started — this runs asynchronously"
      }
    } : null, !busy && o.phase === "WaitForUser" ? {
      label: "Confirm cleanup",
      icon: "check",
      dialog: {
        title: "Confirm the stale workload is gone?",
        desc: "Ramen waits for the operator here because it cannot tell a deleted workload from an unreachable one. Confirm only once the old site's workload really is deleted — two live copies writing to one volume set is the failure this guard exists to prevent.",
        confirm: "Confirm",
        run: () => api.appConfirmCleanup(o.id)
      }
    } : null, !busy && o.phase === "FailedOver" && o.failoverTargets.length ? {
      label: "Fail back to the preferred site",
      icon: "refresh",
      op: "failback",
      disabled: !o.rpoMet,
      hint: !o.rpoMet ? "a leg is outside its scheduling interval" : null,
      dialog: {
        title: `Fail back ${o.namespace}/${o.name}?`,
        desc: `Planned move back to ${o.preferredSite}. Replication has been running in reverse since the failover, and the move only proceeds once the group is inside its interval, so nothing is lost.`,
        fields: [{
          k: "n88",
          type: "note",
          label: o.group && o.group.lastAt ? `The group last synced at ${clockOf(o.group.lastAt)} with ${fmtBytes(o.group.writtenSince)} written since. That is what a fail back has to ship before the cutover.` : "The group's replication status decides when the fail back can proceed."
        }],
        confirm: "Fail back",
        run: () => api.appRelocate(o.id),
        done: "Relocation started — this runs asynchronously"
      }
    } : null, busy ? {
      label: `${o.phase} in progress…`,
      icon: "clock",
      disabled: true,
      hint: o.progression
    } : null,
    // Ramen drives one DRPC per application, so switching which method it
    // drives is a rebind: delete and create, because DRPolicy is immutable.
    !busy && live.length > 1 ? {
      label: "Switch orchestrated method…",
      icon: "swap",
      dialog: {
        title: `Orchestrated method for ${o.name}`,
        desc: "A placement control selects PVCs by label, so two over the same PVCs would both claim them — the documented outcome is data corruption. Exactly one method is therefore driven by Ramen; the rest keep replicating in the data plane with identical parameters and report their lag out of band.",
        fields: [{
          k: "method",
          label: "Method Ramen drives",
          type: "select",
          required: true,
          def: o.orchestratedMethod,
          options: live.map(l => ({
            v: l.method,
            l: `${l.method} · ${mmeta(l.type).label} → ${l.target} · lag ${fmtLag(l.lagSeconds)}`
          }))
        }, {
          k: "n92",
          type: "note",
          label: "Switching deletes and recreates the placement control. The data plane is untouched: no baseline is retaken and no generation is lost."
        }, {
          k: "n93",
          type: "note",
          label: "The vault method is never chosen here — it becomes the orchestrated one only for the duration of a restore, from the generation catalogue."
        }],
        confirm: "Rebind",
        run: v => api.appSetOrchestrated(o.id, v.method),
        done: "Placement control rebinding — this runs asynchronously"
      }
    } : null, !busy && o.vaultMethod && o.generations.length ? {
      label: "Restore from a generation…",
      icon: "camera",
      danger: true,
      op: "failover",
      dialog: {
        title: `Restore ${o.namespace}/${o.name} from a generation`,
        danger: true,
        desc: `Materialises the application's volumes from one immutable generation in the vault and brings it up at ${o.restoreTargets.join(", ") || "the restore target"}. This is the ransomware path: recovery takes minutes rather than seconds because volumes are built from object storage, and afterwards the application has to be re-protected from scratch.`,
        fields: [{
          k: "generation",
          label: "Generation",
          type: "select",
          required: true,
          options: o.generations.filter(g => g.integrity === "Verified").slice(0, 40).map(g => ({
            v: String(g.generation),
            l: `${g.generation} · ${fmtDate(g.at)} · ${g.tier} · ${fmtBytes(g.size)}${g.kind === "full" ? " · full" : ""}`
          }))
        }, {
          k: "n94",
          type: "note",
          label: "The generation is pinned out of band immediately before the placement rebind, because PromoteVolume carries no point-in-time argument. The driver reads the pin when the promote arrives."
        }, {
          k: "n95",
          type: "note",
          label: "Failback will not be available afterwards: the source volume is gone or untrusted and the vault holds generations rather than a live peer, so the original site's pre-compromise state cannot be reconstructed."
        }],
        confirm: "Pin and restore",
        run: v => api.appRestore(o.id, v.generation),
        done: "Generation pinned — the placement control is rebinding"
      }
    } : null, !busy ? {
      label: "Edit recipe…",
      icon: "list",
      dialog: {
        title: `Recipe for ${o.namespace}/${o.name}`,
        desc: "The Ramen Recipe this application's placement control references. Groups select Kubernetes objects; hooks run a command in a pod or wait for a condition; the recover workflow is the boot sequence at the standby site, the capture workflow runs before objects are backed up. Discover the namespace's resources to start from what is actually deployed.",
        wide: true,
        fields: [{
          k: "recipe",
          type: "recipe",
          namespace: o.namespace,
          def: o.recipe || {
            name: o.name.toLowerCase().replace(/[^a-z0-9-]/g, "-") + "-recipe",
            namespace: o.namespace,
            appType: "",
            groups: [],
            hooks: [],
            captureWorkflow: {
              failOn: "any-error",
              sequence: []
            },
            recoverWorkflow: {
              failOn: "any-error",
              sequence: []
            }
          }
        }, !o.kubeObjectProtection ? {
          k: "n50",
          type: "note",
          label: "Kubernetes object protection is off on this application — the recipe is stored but not executed until it is enabled."
        } : null].filter(Boolean),
        confirm: "Save recipe",
        run: v => api.appSetRecipe(o.id, v.recipe)
      }
    } : null, !busy && o.recipe ? {
      label: "Remove recipe",
      icon: "x",
      dialog: {
        title: `Remove the recipe from ${o.name}?`,
        desc: "Without a recipe Ramen captures and restores every object in the namespace in one pass — no ordering, no hooks.",
        confirm: "Remove",
        run: () => api.appSetRecipe(o.id, {
          remove: true
        })
      }
    } : null, !busy ? {
      label: "Edit protection…",
      icon: "gauge",
      dialog: {
        title: `Protection for ${o.name}`,
        desc: "The plan decides which methods exist and what they cost. Kubernetes object protection decides whether the workload itself travels or only its volumes.",
        fields: [{
          k: "kube_object_protection",
          label: "Protect Kubernetes objects",
          type: "checkbox",
          def: o.kubeObjectProtection
        }, {
          k: "n96",
          type: "note",
          label: "Without object protection a failover moves the volumes only: the workload has to be recreated by hand and the application parks in WaitForUser."
        }],
        confirm: "Apply",
        run: v => api.appUpdate(o.id, {
          kube_object_protection: v.kube_object_protection
        })
      }
    } : null, !busy ? {
      label: "Stop protecting",
      icon: "trash",
      danger: true,
      removes: true,
      dialog: {
        title: `Stop protecting ${o.namespace}/${o.name}?`,
        danger: true,
        desc: "The application is removed from DR. Its PVCs keep their data and storage-level replication is unaffected, but no failover and no restore is possible.",
        confirm: "Stop protecting",
        run: () => api.appUnprotect(o.id)
      }
    } : null].filter(Boolean);
  },
  // A plan is authored; everything under it is derived, so the only things to
  // act on are its methods.
  plan: o => [{
    label: "Add method…",
    icon: "plus",
    dialog: addMethodDialog(o)
  }, o.protectionGap ? {
    label: "Fix the interval mismatch…",
    icon: "alert",
    dialog: {
      title: `Fix the interval on ${o.name}`,
      desc: "The interval is written to the policy and to the class parameters. When they differ no class resolves, no peerClass appears, and every application on that method is protected by nothing — while the policy still validates cleanly. Setting it here emits one value to both places.",
      fields: [{
        k: "method",
        label: "Method",
        type: "select",
        required: true,
        options: o.methods.filter(m => !m.intervalConsistent).map(m => ({
          v: m.name,
          l: `${m.name} · policy ${m.interval} vs class ${m.classInterval}`
        }))
      }, {
        k: "interval",
        label: "Interval",
        type: "text",
        required: true,
        placeholder: "5m",
        sub: "written to DRPolicy.spec.schedulingInterval and parameters.schedulingInterval at once"
      }],
      confirm: "Set interval",
      run: v => api.planSetInterval(o.id, v.method, v.interval),
      done: "Interval corrected — the class should now resolve"
    }
  } : null, {
    label: "Delete plan",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: o.counts.apps ? `${o.counts.apps} application(s) are still bound to this plan. Unbind them first.` : "The plan and its derived policies and classes are removed. Nothing is deleted in the data plane and no vault generation is touched.",
      confirm: "Delete",
      run: () => api.planDelete(o.id)
    }
  }].filter(Boolean),
  method: o => [{
    label: "Set interval…",
    icon: "clock",
    disabled: o.type === "sync",
    hint: o.type === "sync" ? "a synchronous method has no interval — every write is mirrored before it is acknowledged" : null,
    dialog: {
      title: `Interval for ${o.name}`,
      desc: "One field, written to two places: the policy's schedulingInterval and the class's parameters.schedulingInterval. They must agree exactly — a difference of formatting alone leaves the application protected by nothing.",
      fields: [{
        k: "interval",
        label: "Interval",
        type: "text",
        required: true,
        def: o.interval,
        placeholder: "5m"
      }, !o.intervalConsistent ? {
        k: "n97",
        type: "note",
        label: `Currently the policy carries ${o.interval} and the class carries ${o.classInterval}, so no class resolves for this method.`
      } : null].filter(Boolean),
      confirm: "Set interval",
      run: v => api.planSetInterval(o.planId, o.name, v.interval)
    }
  }, {
    label: "Remove method",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Remove ${o.name} from ${o.planName}?`,
      danger: true,
      desc: o.type === "snapshot-s3" ? "The schedule stops and no further generation is uploaded. Existing generations are not deleted — locked objects cannot be removed before their lock expires, which is the guarantee the vault exists for." : "The relationship is torn down and the peer copy is released per policy. Every application on this plan loses the leg.",
      confirm: "Remove",
      run: () => api.planRemoveMethod(o.planId, o.name)
    }
  }],
  site: o => [o.fencing === "Unfenced" ? {
    label: "Fence this site",
    icon: "power",
    danger: true,
    op: "fence",
    entity: "application",
    dialog: {
      title: `Fence ${o.name}?`,
      danger: true,
      desc: "Blocks the site's access to storage so an application failed over elsewhere cannot be written to from two places. Fencing in Ramen is pair-scoped, so with three or more sites the decision is taken by quorum through the arbitration token.",
      confirm: "Fence",
      run: () => api.siteFence(o.id)
    }
  } : {
    label: "Unfence this site",
    icon: "power",
    op: "fence",
    entity: "application",
    dialog: {
      title: `Unfence ${o.name}?`,
      desc: "Restores the site's access to storage. Do this only once the site is known healthy and its stale workloads are gone.",
      confirm: "Unfence",
      run: () => api.siteUnfence(o.id)
    }
  }],
  mpath: o => [o.status === "active" ? {
    label: "Pause path",
    icon: "pause",
    dialog: {
      title: `Pause ${o.name}?`,
      desc: "A group already moving finishes its current step. No new step and no new group starts until the path is resumed.",
      confirm: "Pause",
      run: () => api.mpathPause(o.id)
    }
  } : o.status === "paused" ? {
    label: "Resume path",
    icon: "play",
    dialog: {
      title: `Resume ${o.name}?`,
      desc: "The queue continues with the next pending step.",
      confirm: "Resume",
      run: () => api.mpathResume(o.id)
    }
  } : null, {
    label: "Add application group…",
    icon: "plus",
    dialog: window.newAppGroupDialog(o)
  }, {
    label: "Delete path",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: "Only possible once every application group has completed. Completed groups are removed with the path; the migrated volumes stay at site B.",
      confirm: "Delete",
      run: () => api.mpathDelete(o.id)
    }
  }].filter(Boolean),
  appgroup: o => [o.phase === "Converged" ? {
    label: "Move workloads now",
    icon: "move",
    dialog: {
      title: `Move ${o.name} to site B?`,
      desc: `Replication has converged (backlog zero). ${o.counts.vms} VM(s) are live-migrated with KubeVirt and ${o.counts.members - o.counts.vms} container workload(s) are restarted at the target site. Afterwards the ${o.counts.volumes} volume(s) follow by online migration.`,
      confirm: "Move now",
      run: () => api.appGroupMove(o.id)
    }
  } : null, ["Replicating", "Converged"].includes(o.phase) ? {
    label: "Pause group",
    icon: "pause",
    dialog: {
      title: `Pause ${o.name}?`,
      desc: "The replication policy stays in place; nothing moves until resumed.",
      confirm: "Pause",
      run: () => api.appGroupPause(o.id)
    }
  } : null, o.phase === "Paused" ? {
    label: "Resume group",
    icon: "play",
    dialog: {
      title: `Resume ${o.name}?`,
      confirm: "Resume",
      run: () => api.appGroupResume(o.id)
    }
  } : null, !["Completed", "Failed"].includes(o.phase) ? {
    label: o.approval === "auto" ? "Require approval before moving" : "Move automatically when converged",
    icon: "check",
    dialog: {
      title: "Change approval",
      desc: o.approval === "auto" ? "The group will wait at Converged until an operator approves the move." : "The group moves its workloads as soon as every volume's backlog is zero.",
      confirm: "Apply",
      run: () => api.appGroupApproval(o.id, o.approval === "auto" ? "manual" : "auto")
    }
  } : null, ["Queued", "Completed", "Failed"].includes(o.phase) ? {
    label: "Remove group",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Remove ${o.name}?`,
      danger: true,
      desc: o.phase === "Completed" ? "Removes the record; the workloads and volumes stay at site B." : "The group leaves the queue. Nothing has been replicated or moved.",
      confirm: "Remove",
      run: () => api.appGroupDelete(o.id)
    }
  } : null].filter(Boolean),
  bucket: o => [{
    label: "Resize filesystem…",
    icon: "move",
    dialog: {
      title: `Resize ${o.name}`,
      desc: "The bucket's filesystem can only grow — S3 has no shrink, and neither does the volume beneath it.",
      fields: [{
        k: "size",
        label: "Filesystem size",
        unit: "GB",
        type: "number",
        min: gb(o.capacity.total),
        def: gb(o.capacity.total),
        required: true
      }],
      confirm: "Resize",
      run: v => api.bucketResize(o.id, Number(v.size) * GBn)
    }
  }, {
    label: "Bucket settings…",
    icon: "gauge",
    dialog: {
      title: `Settings for ${o.name}`,
      desc: "Versioning and object lock apply to objects written from now on.",
      fields: [{
        k: "versioning",
        label: "Versioning",
        type: "checkbox",
        def: o.versioning
      }, {
        k: "object_lock",
        label: "Object lock",
        type: "checkbox",
        def: o.objectLock
      }, {
        k: "quota",
        label: "Quota",
        unit: "GB, 0 = none",
        type: "number",
        min: 0,
        def: gb(o.quota)
      }],
      confirm: "Apply",
      run: v => api.bucketUpdate(o.id, Object.assign({}, v, {
        quota: Number(v.quota) * GBn
      }))
    }
  }, {
    label: "Tags…",
    icon: "filter",
    dialog: {
      title: `Tags on ${o.name}`,
      desc: "S3 bucket tagging. Tags are the metadata the bucket list searches and filters by — cost center, team, environment, retention class.",
      fields: [{
        k: "tags",
        label: "Bucket tags",
        type: "kv",
        max: 50,
        def: Object.entries(o.tags || {}).map(([k, v]) => ({
          k,
          v
        }))
      }],
      confirm: "Save tags",
      run: v => api.bucketSetTags(o.id, Object.fromEntries((v.tags || []).filter(t => t.k).map(t => [t.k.trim(), (t.v || "").trim()])))
    }
  }, o.replication ? {
    label: `Detach from ${o.replication.policy_name}`,
    icon: "shield",
    dialog: {
      title: `Stop replicating ${o.name}?`,
      desc: `The bucket leaves the ${o.replication.mode} policy ${o.replication.policy_name}. The replica on the target keeps the last generation it received; nothing is deleted there.`,
      confirm: "Detach",
      run: () => api.bucketUnreplicate(o.id)
    }
  } : {
    label: "Replicate — attach to a policy…",
    icon: "shield",
    dialog: {
      title: `Replicate ${o.name}`,
      desc: "A bucket is its volume, so it joins a replication policy the way a volume does. Only policies whose source is this bucket's cluster qualify — synchronous ones across zones, asynchronous ones to a paired cluster.",
      fields: [{
        k: "policy_id",
        label: "Replication policy",
        type: "select",
        required: true,
        load: () => api.rpolicies(o.clusterId).then(ps => ps.map(p => ({
          v: p.id,
          l: `${p.name} · ${p.mode}${p.mode === "asynchronous" ? ` · every ${p.frequency} min → ${regName(p.targetClusterId)}` : " · across zones"}`
        }))),
        empty: "No replication policy has this cluster as its source. Create one under Disaster recovery first."
      }],
      confirm: "Attach",
      run: v => api.bucketReplicate(o.id, v.policy_id)
    }
  }, {
    label: "Access & credentials…",
    icon: "lock",
    dialog: {
      title: `Access for ${o.name}`,
      desc: "Bucket security rides on Kubernetes: a service account in a namespace, and a secret holding the access key.",
      fields: [{
        k: "namespace",
        label: "Namespace",
        type: "text",
        def: o.access.namespace,
        required: true
      }, {
        k: "service_account",
        label: "Service account",
        type: "text",
        def: o.access.service_account,
        required: true
      }, {
        k: "policy",
        label: "Policy",
        type: "select",
        def: o.access.policy,
        options: [{
          v: "read-write",
          l: "Read / write"
        }, {
          v: "read-only",
          l: "Read only"
        }, {
          v: "write-only",
          l: "Write only"
        }]
      }, {
        k: "public",
        label: "Allow anonymous access",
        type: "checkbox",
        def: o.access.public
      }, {
        k: "rotate_key",
        label: "Rotate the access key",
        type: "checkbox"
      }, {
        k: "n34",
        type: "note",
        label: "Rotating the key rewrites the secret. Clients holding the old key stop working immediately."
      }],
      confirm: "Apply",
      run: v => api.bucketSetAccess(o.id, v)
    }
  }, {
    label: "Delete bucket",
    icon: "trash",
    danger: true,
    removes: true,
    disabled: o.objects > 0 || o.objectLock,
    hint: o.objects > 0 ? `holds ${fmtNum(o.objects)} object(s)` : o.objectLock ? "object lock is on" : null,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: "The bucket and the logical volume beneath it are destroyed, along with its snapshots. Backups already in object storage are kept.",
      fields: [{
        k: "confirmName",
        label: "Type the bucket name to confirm",
        type: "text",
        match: o.name,
        required: true
      }],
      confirm: "Delete bucket",
      run: () => api.bucketDelete(o.id)
    }
  }],
  pvc: o => [{
    label: "Expand claim…",
    icon: "move",
    dialog: {
      title: `Expand ${o.namespace}/${o.name}`,
      desc: "Kubernetes only supports growing a claim. The new size is applied to the logical volume behind it; the filesystem is extended on the next mount.",
      fields: [{
        k: "size",
        label: "Requested size",
        unit: "GB",
        type: "number",
        min: gb(o.requested),
        def: gb(o.requested),
        required: true
      }, {
        k: "n35",
        type: "note",
        label: `Currently ${fmtBytes(o.requested)}. Shrinking a claim is refused.`
      }],
      confirm: "Expand",
      run: v => api.pvcResize(o.id, Number(v.size) * GBn)
    }
  }],
  cgroup: o => [{
    label: "Take group snapshot",
    icon: "camera",
    dialog: {
      title: `Snapshot ${o.name}`,
      desc: "Every member volume is snapshotted at the same instant, so the group restores to one common point in time.",
      fields: [{
        k: "name",
        label: "Group snapshot name",
        type: "text",
        def: `${o.name}-cgsnap`,
        required: true
      }],
      confirm: "Take snapshot",
      run: v => api.cgroupSnapshot(o.id, v.name)
    }
  },
  // Protection lives on the group, not on the individual volumes: attaching a
  // policy here applies it to every member and to members added later.
  o.backupPolicy ? {
    label: "Detach backup policy",
    icon: "cloud",
    dialog: {
      title: `Detach ${o.backupPolicy.policy_name} from ${o.name}?`,
      desc: `The ${o.counts.volumes} member volumes stop being snapshotted and backed up together. Existing backup chains and their retained versions are untouched.`,
      confirm: "Detach",
      run: () => api.cgroupSetPolicy(o.id, null)
    }
  } : {
    label: "Attach backup policy…",
    icon: "cloud",
    dialog: {
      title: `Back up ${o.name}`,
      desc: "Every member volume is snapshotted and backed up on the policy's schedule, all at the same instant, so any retained version is a point in time the whole group can be restored to.",
      fields: [{
        k: "policy",
        label: "Backup policy",
        type: "select",
        required: true,
        load: () => api.policies(o.clusterId).then(ps => ps.map(p => ({
          v: p.id,
          l: `${p.name} · ${p.schedule.map(r => r.interval + "×" + r.versions).join(" ")}${p.consistencyGroup ? " · group-consistent" : ""}`
        }))),
        empty: "No backup policy in this cluster. Create one from the cluster's Backup policies layer."
      }, {
        k: "n80",
        type: "note",
        label: "The policy is switched to group-consistent if it is not already: a policy driving a group has to snapshot every member at the same instant, otherwise the group means nothing. Member volumes lose any policy of their own."
      }],
      confirm: "Attach",
      run: v => api.cgroupSetPolicy(o.id, v.policy)
    }
  }, o.replicationConfig ? {
    label: "Replication cadence…",
    icon: "shield",
    dialog: {
      title: `Replication cadence for ${o.name}`,
      desc: "How often the group snapshot is taken and shipped, and how many older generations the target keeps. Every DR policy naming this group replicates on exactly this cadence.",
      fields: [{
        k: "frequency_minutes",
        label: "Replication frequency",
        unit: "minutes",
        type: "number",
        min: 1,
        required: true,
        def: o.replicationConfig.frequency
      }, {
        k: "retention",
        label: "Retained generations at the target",
        type: "schedule",
        def: o.replicationConfig.retention
      }, {
        k: "n86",
        type: "note",
        label: `${o.drPolicyIds.length} DR polic${o.drPolicyIds.length === 1 ? "y" : "ies"} currently replicate${o.drPolicyIds.length === 1 ? "s" : ""} this group.`
      }],
      confirm: "Apply",
      run: v => api.cgroupReplicate(o.id, v)
    }
  } : {
    label: "Enable replication…",
    icon: "shield",
    dialog: {
      title: `Replicate ${o.name}`,
      desc: "Give the group a replication cadence: every cycle a group snapshot is taken and shipped, so the target always holds one common point in time across all members. A DR policy can then name this group to replicate it to a paired cluster.",
      fields: [{
        k: "frequency_minutes",
        label: "Replication frequency",
        unit: "minutes",
        type: "number",
        min: 1,
        def: 15,
        required: true
      }, {
        k: "retention",
        label: "Retained generations at the target",
        type: "schedule",
        def: [{
          interval: "1h",
          keep: 12
        }, {
          interval: "1d",
          keep: 7
        }]
      }, {
        k: "n81",
        type: "note",
        label: "Enabling replication fixes the group's membership: the replica stream is defined against exactly this set of volumes."
      }],
      confirm: "Enable",
      run: v => api.cgroupReplicate(o.id, v)
    }
  }, o.replicationConfig ? {
    label: "Disable replication",
    icon: "x",
    danger: true,
    disabled: !!o.drPolicyIds.length,
    hint: o.drPolicyIds.length ? `${o.drPolicyIds.length} DR policy/policies replicate this group` : null,
    dialog: {
      title: `Stop replicating ${o.name}?`,
      desc: "The group loses its cadence and its members stop replicating. Replicas already at the target are kept.",
      confirm: "Disable",
      run: () => api.cgroupUnreplicate(o.id)
    }
  } : null, {
    label: "Add volumes…",
    icon: "plus",
    disabled: o.locked,
    hint: o.locked ? "membership is fixed while the group carries a policy" : null,
    dialog: {
      title: `Add volumes to ${o.name}`,
      desc: o.backupPolicy || o.replicationPolicy ? `Only volumes of the same cluster that are not already in a consistency group can join. A volume joining inherits the group's ${[o.backupPolicy && "backup policy", o.replicationPolicy && "replication policy"].filter(Boolean).join(" and ")}.` : "Only volumes of the same cluster that are not already in a consistency group can join.",
      fields: [{
        k: "lvol_ids",
        label: "Volumes",
        type: "multiselect",
        required: true,
        load: () => api.clusterVolumes(o.clusterId).then(vs => vs.filter(v => !v.consistencyGroup && v.status === "online").map(v => ({
          v: v.id,
          l: `${v.name} · ${v.poolName} · ${fmtBytes(v.capacity.total)}`
        }))),
        empty: "Every online volume in this cluster is already in a consistency group."
      }],
      confirm: "Add",
      run: v => api.cgroupAddVolumes(o.id, v.lvol_ids)
    }
  }, {
    label: "Delete group",
    icon: "trash",
    danger: true,
    removes: true,
    disabled: o.counts.snapshots > 0 || !!o.counts.apps || !!o.backupPolicy || !!o.replicationPolicy,
    hint: o.counts.apps ? `${o.counts.apps} protected application(s) are based on this group` : o.backupPolicy || o.replicationPolicy ? "detach the group's policies first" : o.counts.snapshots > 0 ? `${o.counts.snapshots} group snapshot(s) exist` : null,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: "The member volumes are released and keep their data. Delete the group's snapshots first.",
      confirm: "Delete group",
      run: () => api.cgroupDelete(o.id)
    }
  }],
  cgsnapshot: o => [{
    label: "Restore into new volumes…",
    icon: "refresh",
    op: "create",
    dialog: {
      title: `Restore ${o.name}`,
      desc: `Creates one new volume per member (${o.counts.volumes} in total), all at the same point in time. The target can be this cluster or any other.`,
      fields: [{
        k: "cluster_id",
        label: "Target cluster",
        type: "select",
        required: true,
        def: o.clusterId,
        load: () => api.clusters().then(cs => cs.map(c => ({
          v: c.id,
          l: `${c.name}${c.id === o.clusterId ? " (source)" : ""} · ${c.siting}`
        })))
      }, {
        k: "prefix",
        label: "Name prefix for the new volumes",
        type: "text",
        def: "restored",
        required: true
      }, {
        k: "n17",
        type: "note",
        label: "The volumes land in the first enabled pool of the target cluster and are not placed in a consistency group."
      }],
      confirm: "Restore",
      run: v => api.cgSnapRestore(o.id, v)
    }
  }, o.backupVersionId ? {
    label: "Already backed up",
    icon: "cloud",
    disabled: true,
    hint: `version ${o.backupVersionId}`
  } : {
    label: "Back up group snapshot",
    icon: "cloud",
    dialog: {
      title: `Back up ${o.name}`,
      desc: "Writes every member snapshot of this group snapshot to object storage as one backup version.",
      fields: [{
        k: "bucket",
        label: "Bucket location",
        type: "text",
        def: `s3://sb-backup-eu/cg/${o.cgName}/`,
        required: true
      }],
      confirm: "Back up",
      run: v => api.cgSnapBackup(o.id, v.bucket)
    }
  }, {
    label: "Delete group snapshot",
    icon: "trash",
    danger: true,
    removes: true,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: o.backupVersionId ? `The ${o.counts.volumes} member snapshots are removed from the cluster. Backup version ${o.backupVersionId} stays in the bucket.` : `The ${o.counts.volumes} member snapshots are removed and this point in time is lost.`,
      confirm: "Delete",
      run: () => api.cgSnapDelete(o.id)
    }
  }],
  policy: o => [{
    label: "Edit schedule…",
    icon: "clock",
    dialog: {
      title: `Schedule for ${o.name}`,
      desc: "Each row is a tier: how often a snapshot is taken and backed up, how many backup versions of that tier are retained, and how many of those snapshots stay online on the cluster.",
      fields: [{
        k: "schedule",
        label: "Schedule",
        type: "bschedule",
        def: o.schedule
      }, {
        k: "consistency_group",
        label: "Group-consistent — all linked volumes in one consistency group",
        type: "checkbox",
        def: o.consistencyGroup
      }, {
        k: "n60",
        type: "note",
        label: "Group-consistent policies are what Ramen applications link for ransomware recovery: any retained version is a point-in-time the application can be restored to."
      }],
      confirm: "Apply",
      run: v => api.policyUpdate(o.id, v)
    }
  }, {
    label: "Delete policy",
    icon: "trash",
    danger: true,
    removes: true,
    disabled: o.counts.volumes > 0,
    hint: o.counts.volumes > 0 ? `${o.counts.volumes} volume(s) still use it` : null,
    dialog: {
      title: `Delete ${o.name}?`,
      danger: true,
      desc: "Existing backup chains and their versions are kept — only the schedule that would extend them is removed.",
      confirm: "Delete policy",
      run: () => api.policyDelete(o.id)
    }
  }]
};
const newBackupPolicyDialog = cluster => ({
  title: "Create a backup policy",
  confirm: "Create policy",
  desc: "A policy triggers background snapshot-and-backup cycles. Each schedule row carries the cadence, how many backup versions are retained, and how many snapshots stay online.",
  fields: [{
    k: "name",
    label: "Policy name",
    type: "text",
    placeholder: "5m-tiered",
    required: true
  }, {
    k: "schedule",
    label: "Schedule",
    type: "bschedule",
    def: [{
      interval: "5m",
      versions: 12,
      online: 3
    }, {
      interval: "1h",
      versions: 11,
      online: 0
    }, {
      interval: "1d",
      versions: 6,
      online: 0
    }]
  }, {
    k: "consistency_group",
    label: "Group-consistent — all linked volumes in one consistency group",
    type: "checkbox",
    def: false
  }, {
    k: "n12",
    type: "note",
    label: "Once a tier holds more versions than it retains, the oldest is merged into its predecessor — so the interval is also the merge cadence. Group-consistent policies snapshot every linked volume atomically per cycle, which is what an application needs to recover from ransomware to one point in time."
  }],
  run: v => api.policyCreate(cluster.id, v)
});

// ---- menu + dialog host ----------------------------------------------------
// The operation an action needs. Default: a removal is delete, everything else
// is update. Items override with op: "create" | "failover" | "restore" | …;
// restore is the three-part check from RBAC-DESIGN.md §11.
const actionOp = it => it.op || (it.removes ? "delete" : "update");
// §5.5: a denied action is DISABLED with the missing permission as its tooltip.
// Enforcement is the API server rejecting the real call; this is display only.
const permitted = (obj, items) => items.map(it => {
  if (it.disabled) return it;
  const entity = it.entity || KIND_ENTITY[obj.kind] || "storagecluster";
  const ok = it.op === "restore" ? window.access.canRestore(obj, it.targetPool || null) : window.access.can(actionOp(it), entity, obj);
  if (ok) return it;
  const why = it.op === "restore" ? "Needs get on backups in the source pool and create on volumes in the target pool" : window.access.why(actionOp(it), entity, obj);
  return Object.assign({}, it, {
    disabled: true,
    hint: why,
    denied: true
  });
});
function ActionBtn({
  obj,
  big
}) {
  useAccess();
  const items = permitted(obj, (ACTIONS[obj.kind] || (() => []))(obj));
  if (!items.length) return null;
  return /*#__PURE__*/React.createElement("button", {
    className: big ? "chip" : "kebab",
    title: "Actions",
    onClick: e => {
      e.stopPropagation();
      window.__ui.menu(e.currentTarget.getBoundingClientRect(), items, obj);
    }
  }, big ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Icon, {
    n: "dots",
    s: 13
  }), "Actions") : /*#__PURE__*/React.createElement(Icon, {
    n: "dots",
    s: 14
  }));
}
function Field({
  f,
  val,
  setVal
}) {
  const [opts, setOpts] = useState(f.options || null);
  const [loading, setLoading] = useState(!!f.load);
  useEffect(() => {
    if (f.load) f.load().then(o => {
      setOpts(o);
      setLoading(false);
      if (o.length && f.type !== "multiselect" && (val === undefined || val === "")) setVal(o[0].v);
    }).catch(() => setLoading(false));else if (f.options && f.options.length && f.type !== "multiselect" && val === undefined) setVal(f.options[0].v);
  }, []);
  // conditional forms can swap the option set out from under a chosen value
  useEffect(() => {
    if (!f.options || f.type === "multiselect") return;
    setOpts(f.options);
    if (f.options.length && !f.options.some(o => o.v === val)) setVal(f.options[0].v);
  }, [f.options && f.options.map(o => o.v).join("|")]);
  if (f.type === "note") return /*#__PURE__*/React.createElement("div", {
    className: "fnote"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), f.label);
  if (f.type === "recipe") return /*#__PURE__*/React.createElement(RecipeField, {
    f: f,
    val: val,
    setVal: setVal
  });
  if (f.type === "members") return /*#__PURE__*/React.createElement(MembersField, {
    f: f,
    val: val,
    setVal: setVal
  });
  if (f.type === "kv") {
    const rows = val || [];
    const set = (i, k, x) => setVal(rows.map((r, j) => j === i ? Object.assign({}, r, {
      [k]: x
    }) : r));
    return /*#__PURE__*/React.createElement("label", {
      className: "field"
    }, /*#__PURE__*/React.createElement("span", {
      className: "flabel"
    }, f.label, " ", /*#__PURE__*/React.createElement("em", null, "(", rows.length, " of ", f.max || 50, ")")), /*#__PURE__*/React.createElement("div", {
      className: "schedbox"
    }, rows.map((r, i) => /*#__PURE__*/React.createElement("div", {
      className: "schedrow",
      key: i
    }, /*#__PURE__*/React.createElement("input", {
      className: "finput sm",
      placeholder: "key",
      value: r.k,
      onChange: e => set(i, "k", e.target.value)
    }), /*#__PURE__*/React.createElement("span", {
      className: "sl"
    }, "="), /*#__PURE__*/React.createElement("input", {
      className: "finput sm",
      placeholder: "value",
      value: r.v,
      onChange: e => set(i, "v", e.target.value)
    }), /*#__PURE__*/React.createElement("button", {
      type: "button",
      className: "kebab",
      title: "Remove tag",
      onClick: () => setVal(rows.filter((_, j) => j !== i))
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "x",
      s: 11
    })))), /*#__PURE__*/React.createElement("button", {
      type: "button",
      className: "schedadd",
      disabled: rows.length >= (f.max || 50),
      onClick: () => setVal(rows.concat({
        k: "",
        v: ""
      }))
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 11
    }), "Add tag")), f.hint && /*#__PURE__*/React.createElement("span", {
      className: "fhint"
    }, f.hint));
  }
  if (f.type === "bschedule") {
    const rows = val || [];
    const set = (i, k, x) => setVal(rows.map((r, j) => j === i ? Object.assign({}, r, {
      [k]: x
    }) : r));
    return /*#__PURE__*/React.createElement("label", {
      className: "field"
    }, /*#__PURE__*/React.createElement("span", {
      className: "flabel"
    }, f.label, " ", /*#__PURE__*/React.createElement("em", null, "(", rows.reduce((a, r) => a + (Number(r.versions) || 0), 0), " versions, ", rows.reduce((a, r) => a + (Number(r.online) || 0), 0), " online)")), /*#__PURE__*/React.createElement("div", {
      className: "schedbox"
    }, /*#__PURE__*/React.createElement("div", {
      className: "schedrow head"
    }, /*#__PURE__*/React.createElement("span", {
      className: "sl"
    }, "every"), /*#__PURE__*/React.createElement("span", {
      className: "sl"
    }, "versions"), /*#__PURE__*/React.createElement("span", {
      className: "sl"
    }, "online"), /*#__PURE__*/React.createElement("span", {
      style: {
        width: 24
      }
    })), rows.map((r, i) => /*#__PURE__*/React.createElement("div", {
      className: "schedrow",
      key: i
    }, /*#__PURE__*/React.createElement("select", {
      className: "finput sm",
      value: r.interval,
      onChange: e => set(i, "interval", e.target.value)
    }, RET_INTERVALS.map(x => /*#__PURE__*/React.createElement("option", {
      key: x,
      value: x
    }, x))), /*#__PURE__*/React.createElement("input", {
      className: "finput sm",
      type: "number",
      min: "1",
      value: r.versions,
      onChange: e => set(i, "versions", e.target.value)
    }), /*#__PURE__*/React.createElement("input", {
      className: "finput sm",
      type: "number",
      min: "0",
      value: r.online || 0,
      onChange: e => set(i, "online", e.target.value)
    }), /*#__PURE__*/React.createElement("button", {
      type: "button",
      className: "kebab",
      title: "Remove tier",
      onClick: () => setVal(rows.filter((_, j) => j !== i))
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "x",
      s: 11
    })))), /*#__PURE__*/React.createElement("button", {
      type: "button",
      className: "schedadd",
      onClick: () => setVal(rows.concat({
        interval: "1d",
        versions: 6,
        online: 0
      }))
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 11
    }), "Add tier")), /*#__PURE__*/React.createElement("span", {
      className: "fhint"
    }, "Written short: ", rows.map(r => `${r.interval} ${r.versions}x${r.online ? ` ${r.online}x online` : ""}`).join(" · ") || "—"));
  }
  if (f.type === "schedule") {
    const rows = val || [];
    const set = (i, k, x) => setVal(rows.map((r, j) => j === i ? Object.assign({}, r, {
      [k]: x
    }) : r));
    return /*#__PURE__*/React.createElement("label", {
      className: "field"
    }, /*#__PURE__*/React.createElement("span", {
      className: "flabel"
    }, f.label, " ", /*#__PURE__*/React.createElement("em", null, "(", rows.reduce((a, r) => a + (Number(r.keep) || 0), 0), " generations total)")), /*#__PURE__*/React.createElement("div", {
      className: "schedbox"
    }, rows.map((r, i) => /*#__PURE__*/React.createElement("div", {
      className: "schedrow",
      key: i
    }, /*#__PURE__*/React.createElement("span", {
      className: "sl"
    }, "every"), /*#__PURE__*/React.createElement("select", {
      className: "finput sm",
      value: r.interval,
      onChange: e => set(i, "interval", e.target.value)
    }, RET_INTERVALS.map(x => /*#__PURE__*/React.createElement("option", {
      key: x,
      value: x
    }, x))), /*#__PURE__*/React.createElement("input", {
      className: "finput sm",
      type: "number",
      min: "1",
      value: r.keep,
      onChange: e => set(i, "keep", e.target.value)
    }), /*#__PURE__*/React.createElement("span", {
      className: "sl"
    }, "kept"), /*#__PURE__*/React.createElement("button", {
      type: "button",
      className: "kebab",
      title: "Remove rule",
      onClick: () => setVal(rows.filter((_, j) => j !== i))
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "x",
      s: 11
    })))), /*#__PURE__*/React.createElement("button", {
      type: "button",
      className: "schedadd",
      onClick: () => setVal(rows.concat({
        interval: "1h",
        keep: 6
      }))
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 11
    }), "Add retention rule")), /*#__PURE__*/React.createElement("span", {
      className: "fhint"
    }, "Each rule keeps its own number of older snapshot generations at its own interval."));
  }
  if (f.type === "multiselect") {
    const sel = val || [];
    return /*#__PURE__*/React.createElement("label", {
      className: "field"
    }, /*#__PURE__*/React.createElement("span", {
      className: "flabel"
    }, f.label, " ", /*#__PURE__*/React.createElement("em", null, "(", sel.length, " selected)")), loading ? /*#__PURE__*/React.createElement("div", {
      className: "fskel"
    }) : !opts || !opts.length ? /*#__PURE__*/React.createElement("div", {
      className: "fempty"
    }, f.empty || "No options available") : /*#__PURE__*/React.createElement("div", {
      className: "msbox"
    }, opts.map(o => /*#__PURE__*/React.createElement("button", {
      type: "button",
      key: o.v,
      className: "msrow" + (sel.includes(o.v) ? " on" : ""),
      onClick: () => setVal(sel.includes(o.v) ? sel.filter(x => x !== o.v) : sel.concat(o.v))
    }, /*#__PURE__*/React.createElement("span", {
      className: "msbox-i"
    }, sel.includes(o.v) && /*#__PURE__*/React.createElement(Icon, {
      n: "check",
      s: 10
    })), /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, o.l)))));
  }
  const empty = f.type === "select" && !loading && (!opts || !opts.length);
  return /*#__PURE__*/React.createElement("label", {
    className: "field"
  }, /*#__PURE__*/React.createElement("span", {
    className: "flabel"
  }, f.label, f.unit && /*#__PURE__*/React.createElement("em", null, " (", f.unit, ")")), f.type === "select" ? loading ? /*#__PURE__*/React.createElement("div", {
    className: "fskel"
  }) : empty ? /*#__PURE__*/React.createElement("div", {
    className: "fempty"
  }, f.empty || "No options available") : /*#__PURE__*/React.createElement("select", {
    className: "finput",
    value: val === undefined ? "" : val,
    onChange: e => setVal(e.target.value)
  }, opts.map(o => /*#__PURE__*/React.createElement("option", {
    key: o.v,
    value: o.v
  }, o.l))) : f.type === "checkbox" ? /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "toggle" + (val ? " on" : ""),
    onClick: () => setVal(!val)
  }, /*#__PURE__*/React.createElement("i", null)) : /*#__PURE__*/React.createElement("input", {
    className: "finput",
    type: f.type === "number" ? "number" : "text",
    min: f.min,
    value: val === undefined ? "" : val,
    placeholder: f.placeholder,
    onChange: e => setVal(e.target.value)
  }), f.match && val && val !== f.match && /*#__PURE__*/React.createElement("span", {
    className: "fhint",
    style: {
      color: "var(--bad)"
    }
  }, "must match \u201C", f.match, "\u201D"));
}
function Dialog({
  spec,
  obj,
  removes,
  onClose
}) {
  const resolve = v => typeof spec.fields === "function" ? spec.fields(v) : spec.fields || [];
  const [vals, setVals] = useState(() => {
    const v = {};
    resolve({}).forEach(f => {
      if (f.def !== undefined) v[f.k] = f.def;
      if (f.type === "checkbox") v[f.k] = !!f.def;
    });
    return v;
  });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const allFields = resolve(vals);
  const fields = allFields.filter(f => f.type !== "note");
  const invalid = fields.some(f => f.required && (vals[f.k] === undefined || vals[f.k] === "" || Array.isArray(vals[f.k]) && !vals[f.k].length) || f.match && vals[f.k] !== f.match);
  const submit = async () => {
    setBusy(true);
    setErr(null);
    try {
      await spec.run(vals);
      window.__toast(spec.done || `${spec.confirm} — accepted by the control plane`);
      if (removes) window.__removed(obj);else window.__refresh();
      onClose();
    } catch (e) {
      setErr(e.message || "Request failed");
      setBusy(false);
    }
  };
  useEffect(() => {
    const h = e => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", h, true);
    return () => window.removeEventListener("keydown", h, true);
  }, []);
  return /*#__PURE__*/React.createElement("div", {
    className: "ovl",
    onClick: onClose
  }, /*#__PURE__*/React.createElement("div", {
    className: "modal",
    onClick: e => e.stopPropagation()
  }, /*#__PURE__*/React.createElement("div", {
    className: "mhead"
  }, /*#__PURE__*/React.createElement("h3", null, spec.title), /*#__PURE__*/React.createElement("button", {
    className: "tbtn",
    style: {
      color: "var(--dim)"
    },
    onClick: onClose
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "x",
    s: 13
  }))), /*#__PURE__*/React.createElement("div", {
    className: "mbody"
  }, spec.desc && /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, spec.desc), allFields.map(f => f.type === "note" ? /*#__PURE__*/React.createElement(Field, {
    key: f.k || f.label,
    f: f
  }) : /*#__PURE__*/React.createElement(Field, {
    key: f.k,
    f: f,
    val: vals[f.k],
    setVal: v => setVals(s => Object.assign({}, s, {
      [f.k]: v
    }))
  })), err && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      marginTop: 10,
      marginBottom: 0
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 14
  }), err)), /*#__PURE__*/React.createElement("div", {
    className: "mfoot"
  }, /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      fontSize: 10.5,
      color: "var(--dim2)",
      marginRight: "auto"
    }
  }, obj.id && obj.id !== "new" ? `${obj.kind} · ${shortId(obj.id)}` : obj.kind), /*#__PURE__*/React.createElement("button", {
    className: "btn",
    onClick: onClose
  }, "Cancel"), /*#__PURE__*/React.createElement("button", {
    className: "btn primary" + (spec.danger ? " danger" : ""),
    disabled: invalid || busy,
    onClick: submit
  }, busy ? "Working…" : spec.confirm || "Confirm"))));
}
function UiLayer({
  routeKey
}) {
  const [menu, setMenu] = useState(null);
  const [dialog, setDialog] = useState(null);
  const menuRef = useRef(null),
    dialogRef = useRef(null);
  menuRef.current = menu;
  dialogRef.current = dialog;
  useEffect(() => {
    setMenu(null);
    setDialog(null);
  }, [routeKey]);
  useEffect(() => {
    window.__ui = {
      menu: (rect, items, obj) => setMenu({
        rect,
        items,
        obj
      }),
      dialog: (spec, obj) => setDialog({
        spec,
        obj
      }),
      isOpen: () => !!(menuRef.current || dialogRef.current),
      close: () => {
        setMenu(null);
        setDialog(null);
      }
    };
  }, []);
  const runItem = async it => {
    setMenu(null);
    if (it.dialog) return setDialog({
      spec: it.dialog,
      obj: menu.obj,
      removes: it.removes
    });
    try {
      await it.run();
      window.__toast(it.toast || "Done");
      window.__refresh();
    } catch (e) {
      window.__toast(e.message || "Request failed");
    }
  };
  const pos = menu ? {
    top: Math.min(menu.rect.bottom + 6, window.innerHeight - 280),
    left: Math.max(8, Math.min(menu.rect.right - 232, window.innerWidth - 240))
  } : null;
  return /*#__PURE__*/React.createElement(React.Fragment, null, menu && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    style: {
      position: "fixed",
      inset: 0,
      zIndex: 80
    },
    onClick: () => setMenu(null)
  }), /*#__PURE__*/React.createElement("div", {
    className: "fmenu",
    style: pos
  }, menu.items.map((it, i) => /*#__PURE__*/React.createElement("button", {
    key: i,
    className: "menu-item" + (it.danger ? " danger" : ""),
    disabled: it.disabled,
    title: it.hint || "",
    onClick: () => !it.disabled && runItem(it)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: it.icon,
    s: 13
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, it.label), it.disabled && it.hint && /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 11
  }))))), dialog && /*#__PURE__*/React.createElement(Dialog, {
    spec: dialog.spec,
    obj: dialog.obj,
    removes: dialog.removes,
    onClose: () => setDialog(null)
  }));
}
Object.assign(window, {
  ACTIONS,
  ActionBtn,
  Dialog,
  UiLayer,
  newClusterDialog,
  newPairDialog,
  newRPolicyDialog,
  newPoolDialog,
  newReplPolicyDialog,
  attachPvcDialog,
  detachPvcDialog,
  replOpsDialog,
  volumeOpsDialog,
  newPlanDialog,
  addMethodDialog,
  restoreGenerationDialog,
  fileStorageDialog,
  objectStorageDialog,
  newBucketDialog,
  newBackupPolicyDialog,
  newCgroupDialog,
  newMigrationDialog,
  protectAppDialog,
  kmsDialog,
  configureHostDialog,
  qosFields,
  RET_INTERVALS
});
})();
// ---- panels.jsx ----
(function(){
// Detail-page panels: tasks, event log, control-plane containers, FDB backups,
// SPDK threads & logs, SMART. Several read from the agent, not from API v2.
const Tabs = ({
  items,
  active,
  onChange
}) => /*#__PURE__*/React.createElement("div", {
  className: "tabs"
}, items.map(t => /*#__PURE__*/React.createElement("button", {
  key: t.k,
  className: "tab" + (active === t.k ? " on" : ""),
  onClick: () => onChange(t.k)
}, /*#__PURE__*/React.createElement(Icon, {
  n: t.icon,
  s: 13
}), t.label, t.n !== undefined && /*#__PURE__*/React.createElement("span", {
  className: "tn"
}, t.n))));
const LEVEL_C = {
  ERROR: "var(--bad)",
  error: "var(--bad)",
  WARN: "var(--warn)",
  WARNING: "var(--warn)",
  warn: "var(--warn)",
  NOTICE: "var(--info)",
  INFO: "var(--dim)",
  info: "var(--dim)",
  DEBUG: "var(--dim2)"
};
const shortTs = s => (s || "").replace("T", " ").replace(/(\.\d+)?Z$/, "");
function LogStream({
  lines,
  loading,
  error,
  onRetry,
  empty,
  tools,
  height = 340
}) {
  if (error) return /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: onRetry
  }));
  return /*#__PURE__*/React.createElement(React.Fragment, null, tools, /*#__PURE__*/React.createElement("div", {
    className: "logstream",
    style: {
      maxHeight: height
    }
  }, loading && !lines.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "loading\u2026") : !lines.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, empty || "No lines match the current filter.") : lines.map((l, i) => /*#__PURE__*/React.createElement("div", {
    className: "lrow",
    key: i
  }, /*#__PURE__*/React.createElement("span", {
    className: "lts"
  }, shortTs(l.ts)), /*#__PURE__*/React.createElement("span", {
    className: "llvl",
    style: {
      color: LEVEL_C[l.level] || "var(--dim)"
    }
  }, l.level), /*#__PURE__*/React.createElement("span", {
    className: "lmsgtxt"
  }, l.msg)))));
}
const CopyBtn = ({
  get,
  label = "Copy"
}) => {
  const [done, setDone] = useState(false);
  return /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => {
      navigator.clipboard && navigator.clipboard.writeText(get());
      setDone(true);
      setTimeout(() => setDone(false), 1300);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: done ? "check" : "copy",
    s: 12
  }), done ? "Copied" : label);
};

// ---- tasks ----------------------------------------------------------------
const TASK_ST = ["running", "new", "suspended", "done"];
const TASK_SM = {
  running: "var(--info)",
  new: "var(--idle)",
  suspended: "var(--warn)",
  failed: "var(--bad)",
  done: "var(--ok)"
};
const resultColor = r => !r ? "var(--dim2)" : /^canceled/.test(r) ? "var(--warn)" : /fail|error/i.test(r) ? "var(--bad)" : "var(--dim)";
const retryTxt = t => t.maxRetry ? `${t.retry}/${t.maxRetry}` : String(t.retry);
function SubTasks({
  id,
  rev,
  cols
}) {
  const {
    data,
    loading,
    error
  } = useResource("sub|" + id + "|" + rev, () => api.subtasks(id), 4000);
  if (loading) return /*#__PURE__*/React.createElement("tr", {
    className: "subrow"
  }, /*#__PURE__*/React.createElement("td", {
    colSpan: cols,
    className: "lmsg"
  }, "loading subtasks\u2026"));
  if (error) return /*#__PURE__*/React.createElement("tr", {
    className: "subrow"
  }, /*#__PURE__*/React.createElement("td", {
    colSpan: cols,
    className: "lmsg"
  }, "could not load subtasks"));
  if (!data.length) return /*#__PURE__*/React.createElement("tr", {
    className: "subrow"
  }, /*#__PURE__*/React.createElement("td", {
    colSpan: cols,
    className: "lmsg"
  }, "no subtasks"));
  return data.map(s => /*#__PURE__*/React.createElement("tr", {
    className: "subrow",
    key: s.id
  }, /*#__PURE__*/React.createElement("td", null), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      paddingLeft: 20,
      color: "var(--dim)"
    }
  }, "\u21B3 ", shortId(s.id)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, s.distrib || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, s.fn), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: s.retry ? "var(--warn)" : "var(--dim2)"
    }
  }, retryTxt(s)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "tstat",
    style: {
      "--c": TASK_SM[s.status]
    }
  }, /*#__PURE__*/React.createElement("i", null), s.status)), /*#__PURE__*/React.createElement("td", {
    style: {
      color: resultColor(s.result)
    }
  }, s.result || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, fmtAgo(s.updatedAt)), /*#__PURE__*/React.createElement("td", null)));
}
function TasksPanel({
  cluster
}) {
  const [rev, setRev] = useState(0);
  const [q, setQ] = useState("");
  const [fn, setFn] = useState("");
  const [sts, setSts] = useState(["running", "new", "suspended"]);
  const [showDone, setShowDone] = useState(false);
  const [open, setOpen] = useState({});
  const {
    data,
    loading,
    error,
    reload
  } = useResource("tasks|" + cluster.id + "|" + rev, () => api.tasks(cluster.id), 3000);
  const tasks = data || [];
  const fns = [...new Set(tasks.map(t => t.fn))].sort();
  const active = showDone ? sts.concat("done") : sts;
  const rows = tasks.filter(t => active.includes(t.status) && (!fn || t.fn === fn) && (!q || `${t.id} ${t.fn} ${t.target || ""} ${t.result || ""}`.toLowerCase().includes(q.toLowerCase())));
  const counts = {};
  tasks.forEach(t => counts[t.status] = (counts[t.status] || 0) + 1);
  const cancel = async t => {
    try {
      await api.taskCancel(t.id);
      window.__toast(`Task ${shortId(t.id)} cancelled`);
      setRev(r => r + 1);
    } catch (e) {
      window.__toast(e.message);
    }
  };
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  });
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "ptools"
  }, /*#__PURE__*/React.createElement("div", {
    className: "search",
    style: {
      minWidth: 200
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 13,
    c: "var(--dim2)"
  }), /*#__PURE__*/React.createElement("input", {
    value: q,
    placeholder: "Search task id, target, result\u2026",
    onChange: e => setQ(e.target.value)
  })), /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: fn,
    onChange: e => setFn(e.target.value)
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "All functions"), fns.map(f => /*#__PURE__*/React.createElement("option", {
    key: f,
    value: f
  }, f))), /*#__PURE__*/React.createElement("div", {
    className: "chips"
  }, TASK_ST.filter(s => s !== "done").map(s => /*#__PURE__*/React.createElement("button", {
    key: s,
    className: "chip" + (sts.includes(s) ? " on" : ""),
    onClick: () => setSts(sts.includes(s) ? sts.filter(x => x !== s) : [...sts, s])
  }, /*#__PURE__*/React.createElement(Dot, {
    c: TASK_SM[s]
  }), s, /*#__PURE__*/React.createElement("span", {
    className: "n"
  }, counts[s] || 0))), /*#__PURE__*/React.createElement("button", {
    className: "chip" + (showDone ? " on" : ""),
    onClick: () => setShowDone(!showDone)
  }, /*#__PURE__*/React.createElement(Dot, {
    c: TASK_SM.done
  }), "done", /*#__PURE__*/React.createElement("span", {
    className: "n"
  }, counts.done || 0))), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, rows.length), /*#__PURE__*/React.createElement("span", {
    className: "live"
  }, /*#__PURE__*/React.createElement("i", null), "live")), /*#__PURE__*/React.createElement("table", {
    className: "dt tasks"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", {
    style: {
      width: 26
    }
  }), /*#__PURE__*/React.createElement("th", null, "Task ID"), /*#__PURE__*/React.createElement("th", null, "Target"), /*#__PURE__*/React.createElement("th", null, "Function"), /*#__PURE__*/React.createElement("th", null, "Retry"), /*#__PURE__*/React.createElement("th", null, "Status"), /*#__PURE__*/React.createElement("th", null, "Result"), /*#__PURE__*/React.createElement("th", null, "Updated"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, loading && !tasks.length ? /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", {
    colSpan: "9",
    className: "lmsg"
  }, "loading\u2026")) : !rows.length ? /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", {
    colSpan: "9",
    className: "lmsg"
  }, "No tasks match. Completed tasks are hidden unless you opt in.")) : rows.map(t => /*#__PURE__*/React.createElement(React.Fragment, {
    key: t.id
  }, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", null, t.subtaskTotal > 0 && /*#__PURE__*/React.createElement("button", {
    className: "xpand",
    onClick: () => setOpen(o => Object.assign({}, o, {
      [t.id]: !o[t.id]
    }))
  }, /*#__PURE__*/React.createElement(Icon, {
    n: open[t.id] ? "chevd" : "chev",
    s: 11
  }))), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600
    }
  }, shortId(t.id)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, t.subtaskTotal > 0 ? /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--accent)"
    }
  }, "master \xB7 ", t.subtaskTotal, " subtasks") : (t.target || "—").replace(/^NodeID:/, "node ").replace(/^ClusterID:/, "cluster ")), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, t.fn), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: t.retry ? "var(--warn)" : "var(--dim2)"
    }
  }, retryTxt(t)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "tstat",
    style: {
      "--c": TASK_SM[t.status]
    }
  }, /*#__PURE__*/React.createElement("i", null), t.status)), /*#__PURE__*/React.createElement("td", {
    style: {
      color: resultColor(t.result)
    }
  }, t.result || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, fmtAgo(t.updatedAt)), /*#__PURE__*/React.createElement("td", {
    style: {
      textAlign: "right"
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "chip",
    disabled: t.status === "done" || t.canceled,
    onClick: () => cancel(t)
  }, "Cancel"))), open[t.id] && /*#__PURE__*/React.createElement(SubTasks, {
    id: t.id,
    rev: rev,
    cols: 9
  }))))));
}

// ---- alerts ---------------------------------------------------------------
const SEV = {
  critical: {
    c: "var(--bad)",
    label: "critical",
    rank: 2
  },
  warning: {
    c: "var(--warn)",
    label: "warning",
    rank: 1
  },
  info: {
    c: "var(--info)",
    label: "info",
    rank: 0
  }
};

// Alerts are indicators, not events. Each row is a condition that is true right
// now; when the condition clears the row disappears on the next poll. Nothing
// is dismissed or closed — a lamp can only be silenced while it is lit.
function AlertsPanel({
  cluster,
  nav
}) {
  const [rev, setRev] = useState(0);
  const [q, setQ] = useState("");
  const [sev, setSev] = useState([]);
  const [showSilenced, setShowSilenced] = useState(true);
  const {
    data,
    loading,
    error,
    reload
  } = useResource("alerts|" + (cluster ? cluster.id : "cp") + "|" + rev, () => cluster ? api.alerts(cluster.id) : api.cpAlerts(), 5000);
  const all = data || [];
  const rows = all.filter(a => (showSilenced || !a.silenced) && (!sev.length || sev.includes(a.severity)) && (!q || `${a.title} ${a.detail} ${a.rule} ${(a.nodeNames || []).join(" ")} ${(a.deviceNames || []).join(" ")}`.toLowerCase().includes(q.toLowerCase()))).sort((x, y) => x.silenced - y.silenced || SEV[y.severity].rank - SEV[x.severity].rank || x.title.localeCompare(y.title));
  const lit = all.filter(a => !a.silenced);
  const counts = {
    critical: lit.filter(a => a.severity === "critical").length,
    warning: lit.filter(a => a.severity === "warning").length
  };
  const act = async (a, f, msg) => {
    try {
      await f(a.id);
      window.__toast(msg);
      setRev(r => r + 1);
    } catch (e) {
      window.__toast(e.message);
    }
  };
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  });
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "stats",
    style: {
      marginBottom: 12
    }
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Critical",
    v: counts.critical,
    c: counts.critical ? "var(--bad)" : "var(--ok)",
    s: counts.critical ? "conditions holding now" : "nothing lit"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Warning",
    v: counts.warning,
    c: counts.warning ? "var(--warn)" : "var(--ok)",
    s: counts.warning ? "conditions holding now" : "nothing lit"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Silenced",
    v: all.filter(a => a.silenced).length,
    s: "cleared when the condition clears"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Rules watched",
    v: api.alertRuleCount(cluster ? "cluster" : "control-plane") || "—",
    s: cluster ? "cluster scope" : "control plane scope"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "ptools"
  }, /*#__PURE__*/React.createElement("div", {
    className: "search",
    style: {
      minWidth: 210
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 13,
    c: "var(--dim2)"
  }), /*#__PURE__*/React.createElement("input", {
    value: q,
    placeholder: "Search condition, node, device\u2026",
    onChange: e => setQ(e.target.value)
  })), /*#__PURE__*/React.createElement("div", {
    className: "chips"
  }, ["critical", "warning"].map(s => /*#__PURE__*/React.createElement("button", {
    key: s,
    className: "chip" + (sev.includes(s) ? " on" : ""),
    onClick: () => setSev(sev.includes(s) ? sev.filter(v => v !== s) : [...sev, s])
  }, /*#__PURE__*/React.createElement(Dot, {
    c: SEV[s].c
  }), s, /*#__PURE__*/React.createElement("span", {
    className: "n"
  }, counts[s]))), /*#__PURE__*/React.createElement("button", {
    className: "chip" + (showSilenced ? " on" : ""),
    onClick: () => setShowSilenced(!showSilenced)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 10
  }), "include silenced")), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, rows.length), /*#__PURE__*/React.createElement("span", {
    className: "live"
  }, /*#__PURE__*/React.createElement("i", null), "live")), loading && !all.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "loading\u2026") : !rows.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, all.length ? "No condition matches this filter." : "Nothing lit. Every watched condition is currently false.") : /*#__PURE__*/React.createElement("div", {
    className: "alertlist"
  }, rows.map(a => /*#__PURE__*/React.createElement("div", {
    className: "alertrow " + a.severity + (a.silenced ? " ack" : ""),
    key: a.id
  }, /*#__PURE__*/React.createElement("span", {
    className: "asev",
    title: a.severity
  }, /*#__PURE__*/React.createElement("i", null)), /*#__PURE__*/React.createElement("div", {
    style: {
      minWidth: 0,
      flex: 1
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "atitle"
  }, a.title, a.silenced && /*#__PURE__*/React.createElement("span", {
    className: "badge"
  }, "silenced")), /*#__PURE__*/React.createElement("div", {
    className: "adetail"
  }, a.detail), /*#__PURE__*/React.createElement("div", {
    className: "ameta"
  }, !cluster && a.clusterName && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "cluster"), a.clusterName), (a.nodeNames || []).map((nm, i) => /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    key: a.nodeIds[i],
    onClick: () => nav.openNode(a.clusterId, a.nodeIds[i])
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "node",
    s: 10
  }), nm)), (a.deviceNames || []).slice(0, 4).map((nm, i) => /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    key: a.deviceIds[i],
    onClick: () => nav.openDevice(a.clusterId, a.nodeId || a.nodeIds[0], a.deviceIds[i])
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "device",
    s: 10
  }), nm)), (a.deviceNames || []).length > 4 && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "+"), a.deviceNames.length - 4, " more devices"), a.container && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "container"), a.container), !a.nodeNames.length && !a.deviceNames.length && !a.container && cluster && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: () => nav.openCluster(a.clusterId)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 10
  }), cluster.name), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "rule"), a.rule), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "lit"), fmtAgo(a.since))), /*#__PURE__*/React.createElement("div", {
    className: "aremedy"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "chev",
    s: 10
  }), a.remedy)), /*#__PURE__*/React.createElement("div", {
    className: "aacts"
  }, a.silenced ? /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => act(a, api.alertUnsilence, "Alert unsilenced")
  }, "Unsilence") : /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => act(a, api.alertSilence, "Alert silenced until the condition clears")
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 11
  }), "Silence"))))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "10px 12px 12px"
    }
  }, "These are indicators, not a history: a row is here because its condition is true right now, and it disappears by itself once the condition resolves. There is no reconciliation event and nothing to close. Silencing keeps a lamp out of the counts while it stays lit; the silence is forgotten when the condition clears.")));
}

// ---- operations -----------------------------------------------------------
// Every mutation the operator performs is an Ops object with a fixed phase
// list: expansion walks adding node → rebalancing data → complete, removal
// walks data migration → volume migration → removed, migration walks
// restarting node → rebalancing → removing node → migrated. This panel shows
// those state machines while they run and after they land.
const OPS_PHASE_C = {
  Running: "var(--info)",
  Succeeded: "var(--ok)",
  Failed: "var(--bad)",
  Aborted: "var(--warn)"
};
function OpMachine({
  o,
  onAbort,
  nav
}) {
  const [open, setOpen] = useState(o.running);
  const c = OPS_PHASE_C[o.phase] || "var(--dim)";
  return /*#__PURE__*/React.createElement("div", {
    className: "opcard" + (o.running ? " on" : "")
  }, /*#__PURE__*/React.createElement("div", {
    className: "oph",
    onClick: () => setOpen(!open)
  }, /*#__PURE__*/React.createElement("span", {
    className: "opdot",
    style: {
      background: c,
      boxShadow: o.running ? `0 0 0 3px color-mix(in srgb,${c} 22%,transparent)` : "none"
    }
  }), /*#__PURE__*/React.createElement("b", null, o.action), /*#__PURE__*/React.createElement("span", {
    className: "opt"
  }, OPS_TARGET_LABEL[o.targetKind] || "object"), o.targetName && /*#__PURE__*/React.createElement("span", {
    className: "mono opn"
  }, o.targetName), /*#__PURE__*/React.createElement("span", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "opph2",
    style: {
      color: c
    }
  }, o.running ? o.step : o.phase), /*#__PURE__*/React.createElement("span", {
    className: "opstepn"
  }, o.stepIndex + 1, "/", o.steps.length), /*#__PURE__*/React.createElement(Icon, {
    n: "chev",
    s: 11,
    c: "var(--dim2)"
  })), /*#__PURE__*/React.createElement("div", {
    className: "opbars"
  }, o.steps.map((s, i) => /*#__PURE__*/React.createElement("span", {
    key: s + i,
    className: "opp" + (o.phase === "Succeeded" || i < o.stepIndex ? " done" : i === o.stepIndex && o.running ? " on" : ""),
    title: s
  }))), open && /*#__PURE__*/React.createElement("div", {
    className: "opbody"
  }, /*#__PURE__*/React.createElement("ol", {
    className: "opsteps"
  }, o.steps.map((s, i) => {
    const state = o.phase === "Succeeded" || i < o.stepIndex ? "done" : i === o.stepIndex ? o.running ? "on" : o.phase.toLowerCase() : "todo";
    return /*#__PURE__*/React.createElement("li", {
      key: s + i,
      className: state
    }, /*#__PURE__*/React.createElement("i", null), s, state === "on" && /*#__PURE__*/React.createElement("span", {
      className: "dots"
    }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)));
  })), o.message && /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "6px 0 0"
    }
  }, o.message), /*#__PURE__*/React.createElement("div", {
    className: "opev"
  }, o.events.slice(-6).reverse().map((e, i) => /*#__PURE__*/React.createElement("div", {
    key: i
  }, /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, shortTs(e.at)), /*#__PURE__*/React.createElement("b", null, e.reason), /*#__PURE__*/React.createElement("span", null, e.message)))), /*#__PURE__*/React.createElement("div", {
    className: "opfoot"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "started"), fmtAgo(o.startedAt)), o.completedAt && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "finished"), fmtAgo(o.completedAt)), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "object"), o.name), /*#__PURE__*/React.createElement("span", {
    className: "spacer"
  }), o.running && (o.abortable ? /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => onAbort(o)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "x",
    s: 11
  }), "Abort") : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "this phase cannot be unwound"
  }, /*#__PURE__*/React.createElement("i", null, "abort"), "not from ", o.step)))));
}
function OperationsPanel({
  cluster,
  nav
}) {
  const [rev, setRev] = useState(0);
  const [q, setQ] = useState("");
  const [onlyRunning, setOnlyRunning] = useState(false);
  const {
    data,
    loading,
    error,
    reload
  } = useResource("ops|" + (cluster ? cluster.id : "all") + "|" + rev, () => api.operations(cluster ? cluster.id : null), 1500);
  const all = data || [];
  const rows = all.filter(o => (!onlyRunning || o.running) && (!q || `${o.action} ${o.targetName || ""} ${o.step} ${o.phase}`.toLowerCase().includes(q.toLowerCase())));
  const running = all.filter(o => o.running);
  const abort = async o => {
    try {
      await api.operationAbort(o.kind, o.name);
      window.__toast("Abort requested");
      setRev(r => r + 1);
    } catch (e) {
      window.__toast(e.message);
    }
  };
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  });
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "stats",
    style: {
      marginBottom: 12
    }
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Running",
    v: running.length,
    c: running.length ? "var(--info)" : "var(--dim)",
    s: running.length ? running.map(o => o.action).join(", ") : "nothing in flight"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Succeeded",
    v: all.filter(o => o.phase === "Succeeded").length
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Failed",
    v: all.filter(o => o.phase === "Failed").length,
    c: all.some(o => o.phase === "Failed") ? "var(--bad)" : null
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Aborted",
    v: all.filter(o => o.phase === "Aborted").length
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "ptools"
  }, /*#__PURE__*/React.createElement("div", {
    className: "search",
    style: {
      minWidth: 210
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 13,
    c: "var(--dim2)"
  }), /*#__PURE__*/React.createElement("input", {
    value: q,
    placeholder: "Search action, target, phase\u2026",
    onChange: e => setQ(e.target.value)
  })), /*#__PURE__*/React.createElement("button", {
    className: "chip" + (onlyRunning ? " on" : ""),
    onClick: () => setOnlyRunning(!onlyRunning)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 10
  }), "running only"), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, rows.length), /*#__PURE__*/React.createElement("span", {
    className: "live"
  }, /*#__PURE__*/React.createElement("i", null), "live")), loading && !all.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "loading\u2026") : !rows.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, all.length ? "No operation matches this filter." : "No operation has run on this cluster yet. Shutting down, restarting, expanding, migrating or removing a node all appear here.") : /*#__PURE__*/React.createElement("div", {
    className: "oplist"
  }, rows.map(o => /*#__PURE__*/React.createElement(OpMachine, {
    key: o.id,
    o: o,
    onAbort: abort,
    nav: nav
  }))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "10px 12px 12px"
    }
  }, "Each row is one operator operation and its state machine. A phase can only be unwound while the operation is still in an abortable phase \u2014 once data has moved, the operation runs to its end.")));
}

// ---- cluster event log ----------------------------------------------------
const LOG_PRESETS = [{
  k: "status_change",
  label: "All object status changes"
}, {
  k: "errors",
  label: "Error event reporting"
}, {
  k: "device_status",
  label: "Device I/O errors"
}, {
  k: "obj_created",
  label: "Object lifecycle"
}, {
  k: "all",
  label: "Everything"
}];
const EVENT_C = {
  OBJ_CREATED: "var(--accent)",
  STATUS_CHANGE: "var(--dim)",
  device_status: "var(--bad)",
  jm_compression: "var(--ro)"
};
function ClusterLogPanel({
  cluster
}) {
  const [preset, setPreset] = useState("status_change");
  const [q, setQ] = useState("");
  const {
    data,
    loading,
    error,
    reload
  } = useResource("logs|" + cluster.id, () => api.clusterLogs(cluster.id), 5000);
  const all = data || [];
  const rows = all.filter(l => {
    if (preset === "errors") return l.level === "Error";
    if (preset === "status_change") return l.event === "STATUS_CHANGE";
    if (preset === "device_status") return l.event === "device_status";
    if (preset === "obj_created") return l.event === "OBJ_CREATED";
    return true;
  }).filter(l => !q || `${l.message} ${l.objectName || ""} ${l.nodeId || ""} ${l.recordStatus}`.toLowerCase().includes(q.toLowerCase())).slice(0, 400);
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  });
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "ptools"
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: preset,
    onChange: e => setPreset(e.target.value)
  }, LOG_PRESETS.map(p => /*#__PURE__*/React.createElement("option", {
    key: p.k,
    value: p.k
  }, p.label))), /*#__PURE__*/React.createElement("div", {
    className: "search",
    style: {
      minWidth: 220
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 13,
    c: "var(--dim2)"
  }), /*#__PURE__*/React.createElement("input", {
    value: q,
    placeholder: "Search message, node, status\u2026",
    onChange: e => setQ(e.target.value)
  })), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, rows.length, " / ", all.length), /*#__PURE__*/React.createElement(CopyBtn, {
    get: () => rows.map(l => [l.ts, l.nodeId || "None", l.event, l.level, l.message, l.storageId === null || l.storageId === undefined ? "None" : l.storageId, l.vuid || "None", l.recordStatus].join(" | ")).join("\n")
  }), /*#__PURE__*/React.createElement("span", {
    className: "live"
  }, /*#__PURE__*/React.createElement("i", null), "live")), /*#__PURE__*/React.createElement("div", {
    style: {
      overflow: "auto",
      maxHeight: 520
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt logs"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Date"), /*#__PURE__*/React.createElement("th", null, "NodeId"), /*#__PURE__*/React.createElement("th", null, "Event"), /*#__PURE__*/React.createElement("th", null, "Level"), /*#__PURE__*/React.createElement("th", null, "Message"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Storage_ID"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "VUID"), /*#__PURE__*/React.createElement("th", null, "Status"))), /*#__PURE__*/React.createElement("tbody", null, loading && !all.length ? /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", {
    colSpan: "8",
    className: "lmsg"
  }, "loading\u2026")) : !rows.length ? /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", {
    colSpan: "8",
    className: "lmsg"
  }, "No entries match this filter.")) : rows.map(l => /*#__PURE__*/React.createElement("tr", {
    key: l.id
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)",
      whiteSpace: "nowrap"
    }
  }, shortTs(l.ts)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: l.nodeId ? "var(--text)" : "var(--dim2)"
    },
    title: l.objectName ? `${l.objectKind} ${l.objectName}` : ""
  }, l.nodeId ? shortId(l.nodeId) : "None"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: EVENT_C[l.event] || "var(--dim)"
    }
  }, l.event), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: LEVEL_C[l.level] || "var(--dim)",
      fontWeight: l.level === "Info" ? 400 : 600
    }
  }, l.level), /*#__PURE__*/React.createElement("td", {
    className: "logmsg"
  }, l.message), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right",
      color: l.storageId === null || l.storageId === undefined ? "var(--dim2)" : "var(--text)"
    }
  }, l.storageId === null || l.storageId === undefined ? "None" : l.storageId), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right",
      color: l.vuid ? "var(--text)" : "var(--dim2)"
    }
  }, l.vuid || "None"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: l.recordStatus === "None" ? "var(--dim2)" : /^skipped|^late|forced/.test(l.recordStatus) ? "var(--warn)" : "var(--dim)"
    }
  }, l.recordStatus)))))));
}

// ---- control plane containers ---------------------------------------------
const AllocBar = ({
  label,
  used,
  total,
  unit,
  color
}) => {
  const p = pct(used, total);
  return /*#__PURE__*/React.createElement("div", {
    className: "alloc"
  }, /*#__PURE__*/React.createElement("div", {
    className: "ah"
  }, /*#__PURE__*/React.createElement("span", null, label), /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, unit === "cores" ? `${used.toFixed(1)} / ${total} ${unit}` : `${fmtBytes(used)} / ${fmtBytes(total)}`)), /*#__PURE__*/React.createElement("div", {
    className: "bar"
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: Math.min(100, p) + "%",
      "--bc": p > 88 ? "var(--bad)" : p > 70 ? "var(--warn)" : color
    }
  })));
};
function ControlPlanePanel() {
  const {
    data,
    loading,
    error,
    reload
  } = useResource("cont", () => agent.containers(), 4000);
  const [sel, setSel] = useState(null);
  const list = data || [];
  const groups = [...new Set(list.map(c => c.group))];
  const name = sel || list[0] && list[0].name;
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  });
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Live allocation"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement(SourceTag, {
    what: "container runtime"
  })), loading && !list.length ? /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, Array.from({
    length: 6
  }).map((_, i) => /*#__PURE__*/React.createElement("div", {
    className: "skel",
    key: i,
    style: {
      height: 150
    }
  }))) : groups.map(g => /*#__PURE__*/React.createElement("div", {
    key: g
  }, /*#__PURE__*/React.createElement("div", {
    className: "grouplbl"
  }, g, g === "observability" && /*#__PURE__*/React.createElement("em", null, " \xB7 optional deployment")), /*#__PURE__*/React.createElement("div", {
    className: "grid",
    style: {
      marginBottom: 12
    }
  }, list.filter(c => c.group === g).map(c => /*#__PURE__*/React.createElement("div", {
    className: "tile static",
    key: c.name,
    style: {
      "--sc": c.state === "running" ? "var(--ok)" : "var(--dim2)"
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "th"
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      minWidth: 0,
      flex: 1
    }
  }, /*#__PURE__*/React.createElement(TrafficLight, {
    status: c.state === "running" ? "online" : "offline"
  }), /*#__PURE__*/React.createElement("div", {
    className: "tname"
  }, c.name), /*#__PURE__*/React.createElement("div", {
    className: "tsub"
  }, c.image))), /*#__PURE__*/React.createElement("div", {
    style: {
      marginTop: 10
    }
  }, /*#__PURE__*/React.createElement(AllocBar, {
    label: "vCPU",
    used: c.cpu.pct / 100,
    total: c.cpu.alloc,
    unit: "cores",
    color: "var(--accent)"
  }), /*#__PURE__*/React.createElement(AllocBar, {
    label: "RAM",
    used: c.mem.used,
    total: c.mem.limit,
    color: "var(--ok)"
  }), /*#__PURE__*/React.createElement(AllocBar, {
    label: "Disk",
    used: c.disk.used,
    total: c.disk.limit,
    color: "var(--ro)"
  })), /*#__PURE__*/React.createElement("div", {
    className: "kv",
    style: {
      marginTop: 9
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Restarts"), /*#__PURE__*/React.createElement("b", null, c.restarts)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Uptime"), /*#__PURE__*/React.createElement("b", null, c.uptimeH, "h"))), /*#__PURE__*/React.createElement("div", {
    className: "tfoot"
  }, /*#__PURE__*/React.createElement("button", {
    className: "fbtn det",
    onClick: () => setSel(c.name)
  }, "Logs", /*#__PURE__*/React.createElement(Icon, {
    n: "chev",
    s: 11
  })))))))), name && /*#__PURE__*/React.createElement(ContainerLogs, {
    names: list.map(c => c.name),
    name: name,
    setName: setSel
  }));
}
function ContainerLogs({
  names,
  name,
  setName
}) {
  const [q, setQ] = useState("");
  const [lvl, setLvl] = useState("");
  const {
    data,
    loading,
    error,
    reload
  } = useResource("clog|" + name, () => agent.containerLogs(name), 4000);
  const lines = (data || []).filter(l => (!lvl || l.level === lvl) && (!q || l.msg.toLowerCase().includes(q.toLowerCase())));
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Container logs"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement(SourceTag, {
    what: "kubectl logs"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement(LogStream, {
    lines: lines,
    loading: loading,
    error: error,
    onRetry: reload,
    tools: /*#__PURE__*/React.createElement("div", {
      className: "ptools"
    }, /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: name,
      onChange: e => setName(e.target.value)
    }, names.map(n => /*#__PURE__*/React.createElement("option", {
      key: n,
      value: n
    }, n))), /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: lvl,
      onChange: e => setLvl(e.target.value)
    }, /*#__PURE__*/React.createElement("option", {
      value: ""
    }, "All levels"), ["DEBUG", "INFO", "WARN", "ERROR"].map(l => /*#__PURE__*/React.createElement("option", {
      key: l,
      value: l
    }, l))), /*#__PURE__*/React.createElement("div", {
      className: "search",
      style: {
        minWidth: 200
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "search",
      s: 13,
      c: "var(--dim2)"
    }), /*#__PURE__*/React.createElement("input", {
      value: q,
      placeholder: "Filter lines\u2026",
      onChange: e => setQ(e.target.value)
    })), /*#__PURE__*/React.createElement("div", {
      className: "spacer"
    }), /*#__PURE__*/React.createElement("span", {
      className: "count"
    }, lines.length), /*#__PURE__*/React.createElement(CopyBtn, {
      get: () => lines.map(l => `${shortTs(l.ts)} ${l.level} ${l.msg}`).join("\n")
    }), /*#__PURE__*/React.createElement("span", {
      className: "live"
    }, /*#__PURE__*/React.createElement("i", null), "live"))
  })));
}

// ---- FoundationDB backups -------------------------------------------------
function FdbPanel() {
  const [rev, setRev] = useState(0);
  const {
    data,
    loading,
    error,
    reload
  } = useResource("fdb|" + rev, () => agent.fdbBackups());
  const list = data || [];
  const restore = b => window.__ui.dialog({
    title: `Restore state database to ${b.version}?`,
    danger: true,
    desc: "The control plane is stopped, FoundationDB is rolled back to this backup and the services are restarted. Cluster data is untouched, but any control-plane change made after this point is lost.",
    fields: [{
      k: "confirm",
      label: "Type RESTORE to confirm",
      type: "text",
      match: "RESTORE",
      required: true
    }],
    confirm: "Restore state DB",
    run: () => agent.fdbRestore(b.id).then(() => setRev(r => r + 1))
  }, {
    kind: "fdb backup",
    id: b.id
  });
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  });
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "State database backups"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, list.length, " versions"), /*#__PURE__*/React.createElement(SourceTag, {
    what: "fdbbackup agent"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Version"), /*#__PURE__*/React.createElement("th", null, "Backup id"), /*#__PURE__*/React.createElement("th", null, "Type"), /*#__PURE__*/React.createElement("th", null, "Created"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Size"), /*#__PURE__*/React.createElement("th", null, "Status"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, loading && !list.length ? /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", {
    colSpan: "7",
    className: "lmsg"
  }, "loading\u2026")) : list.map(b => /*#__PURE__*/React.createElement("tr", {
    key: b.id
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600
    }
  }, b.version), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, b.id), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "badge " + (b.type === "full" ? "k8s" : "")
  }, b.type)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, fmtDate(b.createdAt), " ", /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "\xB7 ", fmtAgo(b.createdAt))), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(b.size)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
    status: b.status === "complete" ? "online" : "in_activation",
    label: b.status === "complete" ? undefined : false
  }), b.status !== "complete" && /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 11,
      color: "var(--info)"
    }
  }, " writing\u2026"), b.restoreRequestedAt && /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 11,
      color: "var(--warn)"
    }
  }, " \xB7 restore queued")), /*#__PURE__*/React.createElement("td", {
    style: {
      textAlign: "right"
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "chip",
    disabled: b.status !== "complete",
    onClick: () => restore(b)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "refresh",
    s: 11
  }), "Restore")))))))));
}

// ---- SPDK threads + node logs ---------------------------------------------
function SpdkThreadsPanel({
  node
}) {
  const {
    data,
    loading,
    error,
    reload
  } = useResource("spdk|" + node.id, () => agent.spdkThreads(node.id), 2500);
  const threads = data || [];
  const cores = [...new Set(threads.map(t => t.core))].sort((a, b) => a - b);
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  });
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "SPDK thread utilization"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, threads.length, " threads \xB7 ", cores.length, " cores"), /*#__PURE__*/React.createElement(SourceTag, {
    what: "Prometheus"
  })), loading && !threads.length ? /*#__PURE__*/React.createElement("div", {
    className: "skel",
    style: {
      height: 200
    }
  }) : !threads.length ? /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement("b", null, "No thread metrics"), /*#__PURE__*/React.createElement("span", null, "Prometheus has no samples for this node.")) : /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, cores.map(c => /*#__PURE__*/React.createElement("div", {
    className: "corerow",
    key: c
  }, /*#__PURE__*/React.createElement("div", {
    className: "corelbl mono"
  }, "core ", c), /*#__PURE__*/React.createElement("div", {
    className: "corethreads"
  }, threads.filter(t => t.core === c).map(t => /*#__PURE__*/React.createElement("div", {
    className: "thread",
    key: t.name
  }, /*#__PURE__*/React.createElement("div", {
    className: "ah"
  }, /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, t.name), /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, t.busy.toFixed(1), "%")), /*#__PURE__*/React.createElement("div", {
    className: "bar"
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: t.busy + "%",
      "--bc": t.busy > 85 ? "var(--bad)" : t.busy > 65 ? "var(--warn)" : "var(--accent)"
    }
  }))))))))));
}

// Storage-plane logs for the whole cluster: pick a node, pick a stream. Same
// live source as the node's own log tab — the node agent, not the API.
function StoragePlaneLogPanel({
  cluster
}) {
  const {
    data: nodes
  } = useResource("splnodes|" + cluster.id, () => api.nodes(cluster.id));
  const [nodeId, setNodeId] = useState("");
  const [stream, setStream] = useState("spdk");
  const [q, setQ] = useState("");
  const [lvl, setLvl] = useState("");
  const list = nodes || [];
  const active = list.find(n => n.id === nodeId) || list[0];
  const key = active ? active.id : "none";
  const {
    data,
    loading,
    error,
    reload
  } = useResource("splog|" + key + "|" + stream, () => active ? agent.nodeLogs(active.id, stream) : Promise.resolve([]), 2500);
  const lines = (data || []).filter(l => (!lvl || l.level === lvl) && (!q || l.msg.toLowerCase().includes(q.toLowerCase())));
  if (!list.length) return /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "node",
    s: 22
  }), /*#__PURE__*/React.createElement("b", null, "No storage nodes"), /*#__PURE__*/React.createElement("span", null, "Nothing is running on the storage plane yet."));
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Storage plane logs"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement(SourceTag, {
    what: "node agent"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement(LogStream, {
    lines: lines,
    loading: loading,
    error: error,
    onRetry: reload,
    height: 480,
    tools: /*#__PURE__*/React.createElement("div", {
      className: "ptools"
    }, /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: active ? active.id : "",
      onChange: e => setNodeId(e.target.value),
      style: {
        minWidth: 170
      }
    }, list.map(n => /*#__PURE__*/React.createElement("option", {
      key: n.id,
      value: n.id
    }, n.hostname, n.status === "online" ? "" : " · " + n.status))), /*#__PURE__*/React.createElement("div", {
      className: "seg"
    }, ["spdk", "spdk-proxy"].map(s => /*#__PURE__*/React.createElement("button", {
      key: s,
      className: stream === s ? "on" : "",
      onClick: () => setStream(s)
    }, s))), /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: lvl,
      onChange: e => setLvl(e.target.value)
    }, /*#__PURE__*/React.createElement("option", {
      value: ""
    }, "All levels"), ["INFO", "NOTICE", "WARNING", "ERROR"].map(l => /*#__PURE__*/React.createElement("option", {
      key: l,
      value: l
    }, l))), /*#__PURE__*/React.createElement("div", {
      className: "search",
      style: {
        minWidth: 180
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "search",
      s: 13,
      c: "var(--dim2)"
    }), /*#__PURE__*/React.createElement("input", {
      value: q,
      placeholder: "Filter lines\u2026",
      onChange: e => setQ(e.target.value)
    })), /*#__PURE__*/React.createElement("div", {
      className: "spacer"
    }), /*#__PURE__*/React.createElement("span", {
      className: "count"
    }, lines.length), /*#__PURE__*/React.createElement(CopyBtn, {
      get: () => lines.map(l => `${shortTs(l.ts)} ${l.level} ${l.msg}`).join("\n"),
      label: "Copy log"
    }))
  })), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Streamed from the node agent on ", active ? active.hostname : "—", " \u2014 the SPDK reactor and its proxy write to the pod's stdout, which the control plane API does not expose. Open the node itself for its thread utilization."));
}
function NodeLogPanel({
  node
}) {
  const [stream, setStream] = useState("spdk");
  const [q, setQ] = useState("");
  const [lvl, setLvl] = useState("");
  const {
    data,
    loading,
    error,
    reload
  } = useResource("nlog|" + node.id + "|" + stream, () => agent.nodeLogs(node.id, stream), 2500);
  const lines = (data || []).filter(l => (!lvl || l.level === lvl) && (!q || l.msg.toLowerCase().includes(q.toLowerCase())));
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Live log stream"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement(SourceTag, {
    what: "node agent"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement(LogStream, {
    lines: lines,
    loading: loading,
    error: error,
    onRetry: reload,
    height: 420,
    tools: /*#__PURE__*/React.createElement("div", {
      className: "ptools"
    }, /*#__PURE__*/React.createElement("div", {
      className: "seg"
    }, ["spdk", "spdk-proxy"].map(s => /*#__PURE__*/React.createElement("button", {
      key: s,
      className: stream === s ? "on" : "",
      onClick: () => setStream(s)
    }, s))), /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: lvl,
      onChange: e => setLvl(e.target.value)
    }, /*#__PURE__*/React.createElement("option", {
      value: ""
    }, "All levels"), ["INFO", "NOTICE", "WARNING", "ERROR"].map(l => /*#__PURE__*/React.createElement("option", {
      key: l,
      value: l
    }, l))), /*#__PURE__*/React.createElement("div", {
      className: "search",
      style: {
        minWidth: 200
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "search",
      s: 13,
      c: "var(--dim2)"
    }), /*#__PURE__*/React.createElement("input", {
      value: q,
      placeholder: "Filter lines\u2026",
      onChange: e => setQ(e.target.value)
    })), /*#__PURE__*/React.createElement("div", {
      className: "spacer"
    }), /*#__PURE__*/React.createElement("span", {
      className: "count"
    }, lines.length), /*#__PURE__*/React.createElement(CopyBtn, {
      get: () => lines.map(l => `${shortTs(l.ts)} ${l.level} ${l.msg}`).join("\n"),
      label: "Copy log"
    }), /*#__PURE__*/React.createElement("span", {
      className: "live"
    }, /*#__PURE__*/React.createElement("i", null), "live"))
  })));
}

// ---- SMART ----------------------------------------------------------------
function SmartCard({
  device
}) {
  const [rev, setRev] = useState(0);
  const [busy, setBusy] = useState(false);
  // polled, so the report and the traffic light update when a health check
  // started from the actions menu finishes
  const {
    data,
    loading,
    error,
    reload
  } = useResource("smart|" + device.id + "|" + rev, () => agent.smart(device.id), 4000);
  const refresh = async () => {
    setBusy(true);
    try {
      await agent.smartRefresh(device.id);
      window.__toast("SMART re-read — health status updated from the report");
      setRev(r => r + 1);
    } catch (e) {
      window.__toast(e.message);
    }
    setBusy(false);
  };
  const gone = data && data.unavailable;
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 10
    }
  }, "SMART health check", /*#__PURE__*/React.createElement(SourceTag, {
    what: "nvme-cli on the node"
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    disabled: busy || gone,
    onClick: refresh,
    title: gone ? "the drive is not attached, so nvme-cli cannot reach it" : null
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "refresh",
    s: 11
  }), busy ? "Reading…" : "Run health check")), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, error ? /*#__PURE__*/React.createElement("div", {
    style: {
      padding: 14
    }
  }, /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload
  })) : loading && !data ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "loading\u2026") : gone ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "The drive is not attached, so nvme-cli cannot read its SMART log. The last check was ", fmtAgo(data.checked_at), "; the health traffic light stays blank until the device is back online.") : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "smarthead"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Overall"), /*#__PURE__*/React.createElement("b", {
    style: {
      color: data.overall === "PASSED" ? "var(--ok)" : data.overall.indexOf("warn") > 0 ? "var(--warn)" : "var(--bad)"
    }
  }, data.overall)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Model"), /*#__PURE__*/React.createElement("b", null, data.model)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Firmware"), /*#__PURE__*/React.createElement("b", null, data.firmware)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Last check"), /*#__PURE__*/React.createElement("b", null, fmtAgo(data.checked_at))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Health status"), /*#__PURE__*/React.createElement("b", {
    style: {
      color: STATUS_META[data.verdict || "good"].c
    }
  }, STATUS_META[data.verdict || "good"].label))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "0 12px",
      padding: "9px 0 0"
    }
  }, "The device's health traffic light is this verdict: it is derived from the counters below \u2014 available spare against the 10% threshold, media and integrity errors, and the critical warning bit \u2014 and is rewritten every time a health check runs."), /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("tbody", null, data.attributes.map(a => /*#__PURE__*/React.createElement("tr", {
    key: a.name
  }, /*#__PURE__*/React.createElement("td", {
    style: {
      color: "var(--dim)"
    }
  }, a.name), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right",
      fontWeight: a.note ? 600 : 400,
      color: a.note ? "var(--warn)" : undefined
    }
  }, a.value), /*#__PURE__*/React.createElement("td", {
    style: {
      color: "var(--warn)",
      fontSize: 11,
      width: "40%"
    }
  }, a.note || ""))))))));
}

// ---------------------------------------------------------------------------
// CONTROL PLANE — one deployment, cross-cluster. Not a child of any cluster:
// it is the thing that manages them all, so it sits at the top level.
function ControlPlaneView({
  nav
}) {
  const [tab, setTab] = useState("services");
  const acc = useAccess();
  const showAccess = acc.canAnywhere("read", "binding") || acc.canAnywhere("read", "role");
  return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "dhead"
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("h1", null, "Control plane"), /*#__PURE__*/React.createElement("div", {
    className: "mdesc",
    style: {
      marginTop: 4
    }
  }, "One deployment manages every storage cluster. Its services, logs and state database are shared, so they live here rather than under any single cluster."))), /*#__PURE__*/React.createElement("div", {
    className: "tabs"
  }, [["services", "Services"], ["alerts", "Alerts"], ["fdb", "State DB"], ["clusters", "Managed clusters"], showAccess && ["access", "Access"]].filter(Boolean).map(function (t) {
    return /*#__PURE__*/React.createElement("button", {
      key: t[0],
      className: "tab" + (tab === t[0] ? " on" : ""),
      onClick: () => setTab(t[0])
    }, t[1]);
  })), tab === "services" && /*#__PURE__*/React.createElement(ControlPlanePanel, null), tab === "alerts" && /*#__PURE__*/React.createElement(AlertsPanel, {
    cluster: null,
    nav: nav
  }), tab === "fdb" && /*#__PURE__*/React.createElement(FdbPanel, null), tab === "clusters" && /*#__PURE__*/React.createElement(CpClusters, {
    nav: nav
  }), tab === "access" && showAccess && /*#__PURE__*/React.createElement(AccessView, {
    nav: nav
  }));
}
function CpClusters({
  nav
}) {
  const r = useResource("cp-clusters", () => api.clusters(), 8000);
  const cs = r.data || [];
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Clusters managed by this control plane"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Cluster"), /*#__PURE__*/React.createElement("th", null, "Status"), /*#__PURE__*/React.createElement("th", null, "Nodes"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, cs.map(function (c) {
    return /*#__PURE__*/React.createElement("tr", {
      key: c.id
    }, /*#__PURE__*/React.createElement("td", null, c.name), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: c.status
    })), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, c.counts.nodesOnline, "/", c.counts.nodes), /*#__PURE__*/React.createElement("td", {
      style: {
        textAlign: "right"
      }
    }, /*#__PURE__*/React.createElement("button", {
      className: "chip",
      onClick: () => nav.openCluster(c.id)
    }, "Open")));
  })))));
}
Object.assign(window, {
  StoragePlaneLogPanel,
  OperationsPanel,
  OpMachine,
  Tabs,
  LogStream,
  CopyBtn,
  TasksPanel,
  AlertsPanel,
  ClusterLogPanel,
  ControlPlanePanel,
  FdbPanel,
  SpdkThreadsPanel,
  NodeLogPanel,
  SmartCard,
  AllocBar,
  ControlPlaneView,
  CpClusters
});
})();
// ---- rbac-admin.jsx ----
(function(){
// ---------------------------------------------------------------------------
// ACCESS MANAGEMENT — Control plane → Access. §4.5 of the RBAC design:
// scope tree, per-scope grant list with a source column, add grant (disabled
// for roles the caller cannot bind, with the reason), effective access for a
// subject with a SubjectAccessReview spot check, and the §6 invariants.
// ---------------------------------------------------------------------------
const RBA_RBAC = "rbac.authorization.k8s.io";
const SCOPE_KIND_LABEL = {
  "cluster-scope": "cluster scope",
  "managed-cluster": "managed cluster",
  "storage-cluster": "storage cluster",
  "storage-pool": "pool",
  "dr-pair": "DR",
  application: "application"
};
const SCOPE_ICON = {
  "cluster-scope": "shield",
  "managed-cluster": "k8s",
  "storage-cluster": "cluster",
  "storage-pool": "pool",
  "dr-pair": "link",
  application: "shield"
};
const scopeLabel = s => s.kind === "cluster-scope" ? "cluster scope" : `${SCOPE_KIND_LABEL[s.kind] || s.kind} · ${s.name || s.id}`;
const nsLabel = n => n === "*" ? "cluster scope" : n;
const fmtExpiry = e => !e ? "—" : Date.parse(e) < Date.now() ? "expired " + relAge(e) : "in " + Math.ceil((Date.parse(e) - Date.now()) / 86400e3) + " d";

// ---- rules of one aggregated role, read-only ----------------------------------
function RulesTable({
  rules
}) {
  return /*#__PURE__*/React.createElement("table", {
    className: "dt rules"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "API group"), /*#__PURE__*/React.createElement("th", null, "Resources"), /*#__PURE__*/React.createElement("th", null, "Verbs"), /*#__PURE__*/React.createElement("th", null, "Names"))), /*#__PURE__*/React.createElement("tbody", null, rules.map((r, i) => /*#__PURE__*/React.createElement("tr", {
    key: i
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, r.apiGroups.join(", ")), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, r.resources.map(x => /*#__PURE__*/React.createElement("span", {
    key: x,
    className: "lab mono"
  }, x)))), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: r.verbs.length > 3 ? "var(--text)" : "var(--dim)"
    }
  }, r.verbs.join(" ")), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontSize: 10.5,
      color: "var(--dim)"
    }
  }, (r.resourceNames || []).join(", ") || "—")))));
}
function RolesPanel() {
  const {
    data,
    loading,
    error,
    reload
  } = useResource("access.roles", () => api.accessRoles(), 60000);
  const [open, setOpen] = useState(null);
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload,
    kind: "roles"
  });
  const roles = data || [];
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Roles"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, roles.length)), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "The eight ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "sb:*"), " ClusterRoles are ", /*#__PURE__*/React.createElement("b", null, "aggregated"), ": the chart owns the name, the controller fills the rules from every ClusterRole labelled ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "simplyblock.io/aggregate-to-<role>: \"true\""), ". Adding a CRD never edits a role, and there is no role editor here \u2014 a rule set that is not in the chart is added with kubectl and shows up on the next load. Actions are resources: failover is ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "create"), " on ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "applicationfailovers"), ", not a field on the policy."), loading && !data ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "Loading roles\u2026") : /*#__PURE__*/React.createElement("div", {
    className: "rolelist"
  }, roles.map(r => {
    const isOpen = open === r.name;
    return /*#__PURE__*/React.createElement("div", {
      key: r.name,
      className: "card role" + (isOpen ? " open" : "")
    }, /*#__PURE__*/React.createElement("button", {
      className: "rolehead",
      onClick: () => setOpen(isOpen ? null : r.name)
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "shield",
      s: 14
    }), /*#__PURE__*/React.createElement("b", {
      className: "mono"
    }, r.name), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "bound at ", r.boundAt), /*#__PURE__*/React.createElement("span", {
      className: "sub",
      style: {
        flex: 1,
        textAlign: "left"
      }
    }, r.description), /*#__PURE__*/React.createElement("span", {
      className: "count"
    }, r.rules.length, " rule", r.rules.length === 1 ? "" : "s"), /*#__PURE__*/React.createElement(Icon, {
      n: isOpen ? "chevron-up" : "chevron-down",
      s: 12
    })), isOpen && /*#__PURE__*/React.createElement("div", {
      className: "bd",
      style: {
        padding: 0
      }
    }, /*#__PURE__*/React.createElement(RulesTable, {
      rules: r.rules
    }), /*#__PURE__*/React.createElement("div", {
      className: "roleacts",
      style: {
        color: "var(--dim2)",
        fontSize: 11.5
      }
    }, "Aggregated from ", r.parts.map(p => /*#__PURE__*/React.createElement("span", {
      key: p.name,
      className: "lab mono",
      style: {
        marginLeft: 6
      }
    }, p.name)))));
  })));
}

// ---- scope tree (§4.5) ---------------------------------------------------------
function ScopeTree({
  scopes,
  sel,
  onSel
}) {
  const Node = ({
    k,
    label,
    icon,
    ns,
    depth,
    count,
    children,
    muted
  }) => {
    const on = sel === k;
    return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("button", {
      className: "stnode" + (on ? " on" : "") + (muted ? " muted" : ""),
      style: {
        paddingLeft: 8 + depth * 14
      },
      onClick: () => onSel(on ? null : k)
    }, /*#__PURE__*/React.createElement(Icon, {
      n: icon,
      s: 12
    }), /*#__PURE__*/React.createElement("span", {
      className: "stl"
    }, label), ns && /*#__PURE__*/React.createElement("span", {
      className: "mono stns"
    }, ns), count !== undefined && /*#__PURE__*/React.createElement("span", {
      className: "count"
    }, count)), children);
  };
  const counts = scopes.counts || {};
  return /*#__PURE__*/React.createElement("div", {
    className: "scopetree"
  }, /*#__PURE__*/React.createElement(Node, {
    k: "*",
    label: "Cluster scope",
    icon: "shield",
    depth: 0,
    count: counts["*"]
  }), scopes.managed.filter(m => m.visible).map(m => /*#__PURE__*/React.createElement(Node, {
    key: m.id,
    k: m.namespace,
    label: m.name,
    icon: "k8s",
    ns: m.namespace,
    depth: 0,
    count: counts[m.namespace]
  }, scopes.clusters.filter(c => m.clusters.includes(c.id) && c.visible).map(c => /*#__PURE__*/React.createElement(Node, {
    key: c.id,
    k: c.namespace,
    label: c.name,
    icon: "cluster",
    ns: c.namespace,
    depth: 1,
    count: counts[c.namespace],
    muted: !c.full
  }, c.pools.filter(p => p.visible && p.isolated).map(p => /*#__PURE__*/React.createElement(Node, {
    key: p.id,
    k: p.namespace,
    label: p.name,
    icon: "pool",
    ns: p.namespace,
    depth: 2,
    count: counts[p.namespace]
  })), c.sharedPools > 0 && /*#__PURE__*/React.createElement("div", {
    className: "stshared",
    style: {
      paddingLeft: 8 + 2 * 14
    }
  }, c.sharedPools, " pool", c.sharedPools === 1 ? "" : "s", " share ", c.namespace))))), scopes.dr.visible && /*#__PURE__*/React.createElement(Node, {
    k: scopes.dr.namespace,
    label: "Disaster recovery",
    icon: "link",
    ns: scopes.dr.namespace,
    depth: 0,
    count: counts[scopes.dr.namespace]
  }), scopes.apps.some(a => a.visible) && /*#__PURE__*/React.createElement("div", {
    className: "stgroup"
  }, "Protected applications"), scopes.apps.filter(a => a.visible).map(a => /*#__PURE__*/React.createElement(Node, {
    key: a.id,
    k: a.namespace,
    label: a.name,
    icon: "shield",
    ns: a.namespace,
    depth: 0,
    count: counts[a.namespace]
  })));
}

// ---- grants ----------------------------------------------------------------------
function GrantsPanel({
  nav
}) {
  const a = useAccess();
  const [rev, setRev] = useState(0);
  const {
    data,
    loading,
    error,
    reload
  } = useResource("access.grants|" + rev, () => api.accessGrants(), 10000);
  const roles = useResource("access.roles.list", () => api.accessRoles(), 60000);
  const [sel, setSel] = useState(null);
  const [adding, setAdding] = useState(false);
  const scopes = a.state.scopes;
  const gs = data || [];
  const counts = {};
  gs.forEach(g => g.namespaces.forEach(n => {
    counts[n] = (counts[n] || 0) + 1;
  }));
  const shown = sel ? gs.filter(g => g.namespaces.includes(sel)) : gs;
  const mayAdd = a.canAnywhere("create", "binding");
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload,
    kind: "grants"
  });
  return /*#__PURE__*/React.createElement("div", {
    className: "grantlayout"
  }, scopes && /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      alignSelf: "start"
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Scopes"), /*#__PURE__*/React.createElement(ScopeTree, {
    scopes: Object.assign({
      counts
    }, scopes),
    sel: sel,
    onSel: setSel
  })), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Grants", sel ? /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      fontWeight: 400,
      color: "var(--dim)",
      marginLeft: 8
    }
  }, nsLabel(sel)) : null), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, shown.length), !adding && /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    disabled: !mayAdd,
    title: mayAdd ? "" : a.why("create", "binding", null),
    onClick: () => setAdding(true)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Add grant")), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "A grant is ", /*#__PURE__*/React.createElement("b", null, "(subject, role, scope)"), " and becomes a RoleBinding in the scope's namespace \u2014 Kubernetes RBAC is the store, so a grant made here and one made with ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "kubectl"), " are the same object. Grants with an expiry, a reason, or DR fan-out are ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "AccessGrant"), "s the controller reconciles. ", /*#__PURE__*/React.createElement("b", null, "Source"), " tells them apart: ", /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, "control-center"), " is owned here, ", /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, "external"), " is a RoleBinding someone made directly \u2014 shown, never hidden."), adding && /*#__PURE__*/React.createElement(GrantForm, {
    roles: roles.data || [],
    scopes: scopes,
    preNs: sel,
    onDone: () => {
      setAdding(false);
      setRev(r => r + 1);
      a.load();
    },
    onCancel: () => setAdding(false)
  }), loading && !data ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "Loading grants\u2026") : !shown.length ? /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, sel ? `No grant lands in ${nsLabel(sel)}.` : "No grants in the namespaces you can read.")) : /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd grantwrap",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt grants"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Subject"), /*#__PURE__*/React.createElement("th", null, "Role"), /*#__PURE__*/React.createElement("th", null, "Scope"), /*#__PURE__*/React.createElement("th", null, "Source"), /*#__PURE__*/React.createElement("th", null, "Expires"), /*#__PURE__*/React.createElement("th", null, "Reason"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, shown.map(g => {
    const mayDel = g.source !== "external" && g.namespaces.every(n => a.allowedIn(n, "delete", "accessgrants"));
    return /*#__PURE__*/React.createElement("tr", {
      key: g.uuid,
      className: g.status === "Expired" ? "expired" : ""
    }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
      className: "lab",
      title: g.subject.kind === "User" ? "Bound to a user — offboarding needs a Kubernetes action. Prefer groups." : ""
    }, /*#__PURE__*/React.createElement(Icon, {
      n: g.subject.kind === "Group" ? "users" : "user",
      s: 10
    }), /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, g.subject.name), g.subject.kind === "User" && /*#__PURE__*/React.createElement(Icon, {
      n: "alert",
      s: 10,
      c: "var(--warn)"
    }))), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("b", {
      className: "mono"
    }, g.role)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(ScopeRef, {
      scope: g.scope,
      namespaces: g.namespaces,
      nav: nav
    })), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
      className: "lab" + (g.source === "external" ? " ext" : "")
    }, g.source)), /*#__PURE__*/React.createElement("td", {
      className: "mono",
      style: {
        color: g.status === "Expired" ? "var(--bad)" : "var(--dim)"
      }
    }, fmtExpiry(g.expires_at)), /*#__PURE__*/React.createElement("td", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, g.reason || "—", /*#__PURE__*/React.createElement("div", {
      className: "sub mono"
    }, "by ", (g.created_by || "").replace(/^oidc:/, ""), " \xB7 ", relAge(g.created_at))), /*#__PURE__*/React.createElement("td", {
      style: {
        textAlign: "right"
      }
    }, /*#__PURE__*/React.createElement("button", {
      className: "chip",
      disabled: !mayDel,
      title: g.source === "external" ? `Not owned by an AccessGrant — remove the RoleBinding with kubectl in ${g.namespaces.join(", ")}` : mayDel ? "" : `Needs delete on accessgrants in ${g.namespaces.map(nsLabel).join(", ")}`,
      onClick: () => window.__ui.dialog({
        title: `Revoke ${g.role} from ${g.subject.name.replace(/^oidc:/, "")}?`,
        danger: true,
        confirm: "Revoke grant",
        desc: `Removes the RoleBinding${g.namespaces.length > 1 ? "s" : ""} in ${g.namespaces.map(nsLabel).join(", ")}. Takes effect on the subject's next request; the console may stay optimistic for up to a minute.`,
        run: () => api.accessGrantDelete(g.uuid).then(() => {
          setRev(x => x + 1);
          a.load();
        })
      }, {
        kind: "grant",
        id: g.uuid
      })
    }, "Revoke")));
  })))))));
}
const ScopeRef = ({
  scope,
  namespaces,
  nav
}) => /*#__PURE__*/React.createElement("div", null, scope.kind === "cluster-scope" ? /*#__PURE__*/React.createElement("span", {
  className: "lab"
}, "cluster scope") : /*#__PURE__*/React.createElement("button", {
  className: "lab link",
  onClick: () => {
    if (scope.kind === "storage-cluster") nav.openCluster(scope.id);else if (scope.kind === "managed-cluster") nav.openK8s(scope.id);else if (scope.kind === "storage-pool" && nav.openPool) nav.openPool((REG[scope.id] || {}).clusterId, scope.id);else if (scope.kind === "application" && nav.openApp) nav.openApp(scope.id);
  }
}, /*#__PURE__*/React.createElement(Icon, {
  n: SCOPE_ICON[scope.kind] || "cluster",
  s: 10
}), scope.name || scope.id), scope.drPolicyName && /*#__PURE__*/React.createElement("span", {
  className: "lab",
  style: {
    marginLeft: 4
  },
  title: "Fans out to both members of the DR pair"
}, /*#__PURE__*/React.createElement(Icon, {
  n: "link",
  s: 10
}), scope.drPolicyName), /*#__PURE__*/React.createElement("div", {
  className: "sub mono",
  style: {
    fontSize: 10.5,
    color: "var(--dim2)",
    marginTop: 2
  }
}, (namespaces || []).map(nsLabel).join(" · ")));
function GrantForm({
  roles,
  scopes,
  preNs,
  onDone,
  onCancel
}) {
  const a = useAccess();
  const pre = preNs && scopes ? preNs === "*" ? {
    kind: "cluster-scope"
  } : scopes.clusters.find(c => c.namespace === preNs) ? {
    kind: "storage-cluster",
    id: scopes.clusters.find(c => c.namespace === preNs).id
  } : scopes.managed.find(m => m.namespace === preNs) ? {
    kind: "managed-cluster",
    id: scopes.managed.find(m => m.namespace === preNs).id
  } : scopes.pools.find(p => p.namespace === preNs && p.isolated) ? {
    kind: "storage-pool",
    id: scopes.pools.find(p => p.namespace === preNs).id
  } : preNs === scopes.dr.namespace ? {
    kind: "dr-pair"
  } : scopes.apps.find(x => x.namespace === preNs) ? {
    kind: "application",
    id: scopes.apps.find(x => x.namespace === preNs).id
  } : null : null;
  const [f, setF] = useState({
    kind: "Group",
    name: "",
    role: "sb:cluster-reader",
    scopeKind: pre ? pre.kind : "storage-cluster",
    scopeId: pre ? pre.id || "" : "",
    fanout: false,
    expires: "",
    reason: ""
  });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  if (!scopes) return null;
  const targets = f.scopeKind === "storage-cluster" ? scopes.clusters.filter(c => c.visible) : f.scopeKind === "managed-cluster" ? scopes.managed.filter(m => m.visible) : f.scopeKind === "storage-pool" ? scopes.pools.filter(p => p.visible) : f.scopeKind === "application" ? scopes.apps.filter(x => x.visible) : [];
  const chosen = targets.find(t => t.id === f.scopeId);
  const app = f.scopeKind === "application" ? chosen : null;
  const drPol = app && app.drPolicy ? scopes.dr.pairs.find(p => p.id === app.drPolicy) : null;
  const scope = f.scopeKind === "cluster-scope" ? {
    kind: "cluster-scope"
  } : f.scopeKind === "dr-pair" ? {
    kind: "dr-pair",
    name: "all pairs"
  } : chosen ? Object.assign({
    kind: f.scopeKind,
    id: chosen.id,
    name: chosen.name
  }, f.fanout && drPol ? {
    drPolicy: drPol.id,
    drPolicyName: drPol.name
  } : {}) : null;
  // the namespaces this grant lands in, and which roles the caller may bind there
  let nss = scope ? a.scopeNamespaces(scope).filter(Boolean) : [];
  if (scope && scope.drPolicy) {
    const other = [drPol.sourceClusterId, drPol.targetClusterId].filter(id => id !== app.clusterId)[0];
    const oc = scopes.clusters.find(c => c.id === other);
    if (oc) nss = [...nss, `${app.namespace.split("@")[0]}@${oc.namespace.replace(/^sb-sc-/, "")}`];
  }
  const bindable = r => nss.length > 0 && nss.every(n => a.mayBind(r, n));
  const role = roles.find(r => r.name === f.role);
  const roleOk = role ? bindable(role) : false;
  const pool = f.scopeKind === "storage-pool" ? chosen : null;
  const sharedWith = pool && !pool.isolated ? scopes.pools.filter(p => p.namespace === pool.namespace).length - 1 : 0;
  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      const r = await api.accessGrantCreate({
        subject: {
          kind: f.kind,
          name: f.name.trim()
        },
        role: f.role,
        scope,
        expires_at: f.expires ? new Date(f.expires).toISOString() : null,
        reason: f.reason.trim()
      });
      window.__toast && window.__toast(r && r[0] && r[0].warning ? r[0].warning : `${f.name.trim()} granted ${f.role}`);
      onDone();
    } catch (e) {
      setErr(e.message);
      setBusy(false);
    }
  };
  return /*#__PURE__*/React.createElement("div", {
    className: "card editor"
  }, /*#__PURE__*/React.createElement("h3", null, "Add grant"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement("label", {
    className: "fl"
  }, "Subject", /*#__PURE__*/React.createElement("div", {
    className: "seg"
  }, ["Group", "User"].map(k => /*#__PURE__*/React.createElement("button", {
    key: k,
    className: f.kind === k ? "on" : "",
    onClick: () => setF({
      ...f,
      kind: k
    })
  }, k)))), /*#__PURE__*/React.createElement("label", {
    className: "fl",
    style: {
      flex: 2
    }
  }, `${f.kind} name`, /*#__PURE__*/React.createElement("input", {
    className: "finput mono",
    value: f.name,
    placeholder: f.kind === "User" ? "oidc:alice@corp.example" : "oidc:team-a-oncall",
    onChange: e => setF({
      ...f,
      name: e.target.value
    })
  }), /*#__PURE__*/React.createElement("span", {
    className: "sub"
  }, f.kind === "User" ? /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--warn)"
    }
  }, "Bind to IdP groups, not users \u2014 offboarding a user-bound grant needs a Kubernetes action.") : "An IdP group as the API server presents it, prefix included. Membership changes need no Kubernetes action."))), /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement("label", {
    className: "fl"
  }, "Scope", /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: f.scopeKind,
    onChange: e => setF({
      ...f,
      scopeKind: e.target.value,
      scopeId: "",
      fanout: false,
      role: e.target.value === "cluster-scope" ? "sb:infra-admin" : e.target.value === "dr-pair" ? "sb:dr-reader" : e.target.value === "application" ? "sb:app-admin" : e.target.value === "storage-pool" ? "sb:pool-reader" : "sb:cluster-reader"
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: "cluster-scope"
  }, "cluster scope"), /*#__PURE__*/React.createElement("option", {
    value: "managed-cluster"
  }, "a managed cluster"), /*#__PURE__*/React.createElement("option", {
    value: "storage-cluster"
  }, "a storage cluster"), /*#__PURE__*/React.createElement("option", {
    value: "storage-pool"
  }, "a storage pool"), /*#__PURE__*/React.createElement("option", {
    value: "dr-pair"
  }, "disaster recovery"), /*#__PURE__*/React.createElement("option", {
    value: "application"
  }, "a protected application"))), targets.length > 0 || f.scopeKind !== "cluster-scope" && f.scopeKind !== "dr-pair" ? /*#__PURE__*/React.createElement("label", {
    className: "fl",
    style: {
      flex: 2
    }
  }, SCOPE_KIND_LABEL[f.scopeKind], /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: f.scopeId,
    onChange: e => setF({
      ...f,
      scopeId: e.target.value,
      fanout: false
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "\u2014"), targets.map(t => /*#__PURE__*/React.createElement("option", {
    key: t.id,
    value: t.id
  }, t.name, t.namespace ? ` · ${t.namespace}` : ""))), !targets.length && /*#__PURE__*/React.createElement("span", {
    className: "sub",
    style: {
      color: "var(--warn)"
    }
  }, "No ", SCOPE_KIND_LABEL[f.scopeKind], " is visible to you.")) : null, /*#__PURE__*/React.createElement("label", {
    className: "fl",
    style: {
      flex: 2
    }
  }, "Role", /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: f.role,
    onChange: e => setF({
      ...f,
      role: e.target.value
    })
  }, roles.map(r => {
    const ok = bindable(r);
    return /*#__PURE__*/React.createElement("option", {
      key: r.name,
      value: r.name,
      disabled: !ok
    }, r.name, ok ? "" : " — cannot bind here");
  })), role && /*#__PURE__*/React.createElement("span", {
    className: "sub"
  }, roleOk ? role.description : nss.length ? /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--warn)"
    }
  }, "You hold neither ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "bind"), " on ", role.name, " nor every right it grants in ", nss.map(nsLabel).join(", "), ".") : "Pick a scope first."))), app && drPol && /*#__PURE__*/React.createElement("label", {
    className: "fl chk"
  }, /*#__PURE__*/React.createElement("input", {
    type: "checkbox",
    checked: f.fanout,
    onChange: e => setF({
      ...f,
      fanout: e.target.checked
    })
  }), " Fan out across DR pair ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, drPol.name), " \u2014 bindings on both members or neither (\xA74.4)"), app && !app.drPolicy && /*#__PURE__*/React.createElement("div", {
    className: "fnote"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "This application has no DR policy, so the grant lands on its source cluster only. During a failover, the standby side would be unreachable for this subject."), sharedWith > 0 && /*#__PURE__*/React.createElement("div", {
    className: "fnote"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), pool.name, " shares namespace ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, pool.namespace), " with ", sharedWith, " other pool", sharedWith === 1 ? "" : "s", ". RoleBindings are namespace-wide, so this grant covers them all. Per-pool isolation needs the pool in its own ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "sb-sp-*"), " namespace."), /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement("label", {
    className: "fl"
  }, "Expires", /*#__PURE__*/React.createElement("input", {
    className: "finput",
    type: "date",
    value: f.expires,
    onChange: e => setF({
      ...f,
      expires: e.target.value
    })
  }), /*#__PURE__*/React.createElement("span", {
    className: "sub"
  }, "Optional. The controller removes the bindings when it passes.")), /*#__PURE__*/React.createElement("label", {
    className: "fl",
    style: {
      flex: 2
    }
  }, "Reason", /*#__PURE__*/React.createElement("input", {
    className: "finput",
    value: f.reason,
    placeholder: "OPS-4821 on-call rotation",
    onChange: e => setF({
      ...f,
      reason: e.target.value
    })
  }))), nss.length > 0 && /*#__PURE__*/React.createElement("div", {
    className: "sub mono",
    style: {
      color: "var(--dim2)"
    }
  }, "Lands in: ", nss.map(nsLabel).join(" · ")), err && /*#__PURE__*/React.createElement("div", {
    className: "ferr"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), err), /*#__PURE__*/React.createElement("div", {
    className: "facts"
  }, /*#__PURE__*/React.createElement("span", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("button", {
    className: "btn",
    onClick: onCancel,
    disabled: busy
  }, "Cancel"), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    onClick: save,
    disabled: busy || !f.name.trim() || !scope || !roleOk,
    title: !roleOk && role && nss.length ? `You cannot bind ${role.name} in ${nss.map(nsLabel).join(", ")}` : ""
  }, busy ? "Granting…" : "Grant"))));
}

// ---- effective access for a subject (§4.5) --------------------------------------
function EffectivePanel() {
  const subjects = useResource("access.subjects", () => api.accessSubjects(), 30000);
  const [subject, setSubject] = useState("");
  const eff = useResource("access.eff|" + subject, () => subject ? api.accessEffective(subject) : Promise.resolve(null), 30000);
  const [chk, setChk] = useState({
    verb: "get",
    resource: "storageclusters",
    namespace: ""
  });
  const [res, setRes] = useState(null);
  const e = eff.data;
  const nss = e ? [...new Set(e.grants.flatMap(g => g.namespaces))] : [];
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Effective access"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Kubernetes has no reverse lookup for \"what may this subject do everywhere\" \u2014 ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "SelfSubjectRulesReview"), " works only for the caller. What follows is assembled from ", /*#__PURE__*/React.createElement("b", null, "grants issued through simplyblock"), ". ClusterRoles the platform team created independently are not visible here; spot-check a specific right with a ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "SubjectAccessReview"), " below, which the API server answers completely."), /*#__PURE__*/React.createElement("div", {
    className: "frow",
    style: {
      maxWidth: 560
    }
  }, /*#__PURE__*/React.createElement("label", {
    className: "fl",
    style: {
      flex: 1
    }
  }, "Subject", /*#__PURE__*/React.createElement("select", {
    className: "sel mono",
    value: subject,
    onChange: ev => {
      setSubject(ev.target.value);
      setRes(null);
    }
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "\u2014"), (subjects.data || []).map(s => /*#__PURE__*/React.createElement("option", {
    key: s.kind + s.name,
    value: `${s.kind}:${s.name}`
  }, s.kind, " \xB7 ", s.name))))), e && subject && /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Grants issued through simplyblock ", /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, e.grants.length)), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, !!e.groups.length && /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "8px 14px",
      borderBottom: "1px solid var(--line)",
      fontSize: 11.5,
      color: "var(--dim)"
    }
  }, "Member of ", /*#__PURE__*/React.createElement("span", {
    className: "labels",
    style: {
      display: "inline-flex",
      gap: 4,
      marginLeft: 4
    }
  }, e.groups.map(g => /*#__PURE__*/React.createElement("span", {
    key: g,
    className: "lab mono"
  }, g)))), e.grants.length ? /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Role"), /*#__PURE__*/React.createElement("th", null, "Namespaces"), /*#__PURE__*/React.createElement("th", null, "Via"), /*#__PURE__*/React.createElement("th", null, "Source"), /*#__PURE__*/React.createElement("th", null, "Expires"))), /*#__PURE__*/React.createElement("tbody", null, e.grants.map(g => /*#__PURE__*/React.createElement("tr", {
    key: g.uuid,
    className: g.expires_at && Date.parse(g.expires_at) < Date.now() ? "expired" : ""
  }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("b", {
    className: "mono"
  }, g.role)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontSize: 11
    }
  }, g.namespaces.map(nsLabel).join(", ")), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontSize: 11,
      color: "var(--dim)"
    }
  }, g.subject.kind.toLowerCase(), " ", g.subject.name), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "lab" + (g.source === "external" ? " ext" : "")
  }, g.source)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, fmtExpiry(g.expires_at)))))) : /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "No grant names this subject or any of its groups."), /*#__PURE__*/React.createElement("div", {
    style: {
      padding: "8px 14px",
      fontSize: 11,
      color: "var(--dim2)",
      borderTop: "1px solid var(--line)"
    }
  }, e.note))), subject && /*#__PURE__*/React.createElement("div", {
    className: "card editor",
    style: {
      marginTop: 10
    }
  }, /*#__PURE__*/React.createElement("h3", null, "SubjectAccessReview"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement("label", {
    className: "fl"
  }, "Verb", /*#__PURE__*/React.createElement("select", {
    className: "sel mono",
    value: chk.verb,
    onChange: ev => setChk({
      ...chk,
      verb: ev.target.value
    })
  }, ["get", "list", "watch", "create", "update", "patch", "delete", "bind"].map(v => /*#__PURE__*/React.createElement("option", {
    key: v
  }, v)))), /*#__PURE__*/React.createElement("label", {
    className: "fl",
    style: {
      flex: 1
    }
  }, "Resource", /*#__PURE__*/React.createElement("input", {
    className: "finput mono",
    value: chk.resource,
    onChange: ev => setChk({
      ...chk,
      resource: ev.target.value
    })
  })), /*#__PURE__*/React.createElement("label", {
    className: "fl",
    style: {
      flex: 1
    }
  }, "Namespace", /*#__PURE__*/React.createElement("input", {
    className: "finput mono",
    list: "ac-ns",
    value: chk.namespace,
    placeholder: "empty = cluster scope",
    onChange: ev => setChk({
      ...chk,
      namespace: ev.target.value
    })
  }), /*#__PURE__*/React.createElement("datalist", {
    id: "ac-ns"
  }, nss.filter(n => n !== "*").map(n => /*#__PURE__*/React.createElement("option", {
    key: n,
    value: n
  })))), /*#__PURE__*/React.createElement("div", {
    className: "fl",
    style: {
      justifyContent: "flex-end"
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "btn",
    onClick: () => api.accessReview({
      subject: {
        kind: subject.split(":")[0],
        name: subject.split(":").slice(1).join(":")
      },
      verb: chk.verb,
      resource: chk.resource,
      namespace: chk.namespace || undefined
    }).then(r => setRes(r[0]))
  }, "Check"))), res && /*#__PURE__*/React.createElement("div", {
    className: "ferr" + (res.allowed ? " ok" : ""),
    style: res.allowed ? {
      color: "var(--ok)"
    } : {}
  }, /*#__PURE__*/React.createElement(Icon, {
    n: res.allowed ? "check" : "x",
    s: 12
  }), res.allowed ? "Allowed" : "Denied", " \u2014 ", res.reason))));
}

// ---- §6 invariants --------------------------------------------------------------
function InvariantsPanel() {
  const [rev, setRev] = useState(0);
  const {
    data,
    loading,
    error,
    reload
  } = useResource("access.inv|" + rev, () => api.accessInvariants());
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload,
    kind: "invariants"
  });
  const rows = data || [];
  const fails = rows.filter(r => r.ok === false).length;
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Invariants"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), rows.length > 0 && /*#__PURE__*/React.createElement("span", {
    className: "badge",
    style: {
      color: fails ? "var(--bad)" : "var(--ok)"
    }
  }, fails ? `${fails} violated` : "all hold"), /*#__PURE__*/React.createElement("button", {
    className: "btn",
    onClick: () => setRev(r => r + 1)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "refresh",
    s: 12
  }), "Re-run")), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "The properties whose violation is a security bug rather than a defect (\xA76). Those the console can exercise run against the authorizer behind this page on every load; the rest are named so nobody assumes they are covered here."), loading && !data ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "Running\u2026") : /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, rows.map(r => /*#__PURE__*/React.createElement("div", {
    key: r.n,
    className: "inv"
  }, /*#__PURE__*/React.createElement("span", {
    className: "invdot " + (r.ok === true ? "ok" : r.ok === false ? "bad" : "na")
  }, /*#__PURE__*/React.createElement(Icon, {
    n: r.ok === true ? "check" : r.ok === false ? "x" : "dots",
    s: 11
  })), /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      color: "var(--dim2)"
    }
  }, r.n), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("b", null, r.title), /*#__PURE__*/React.createElement("div", {
    className: "sub"
  }, r.detail)), /*#__PURE__*/React.createElement("span", {
    className: "badge"
  }, r.ok === null ? "backend-only" : r.ok ? "holds" : "violated"))))));
}
function AccessView({
  nav
}) {
  const a = useAccess();
  const [tab, setTab] = useState("grants");
  const canGrants = a.canAnywhere("read", "binding");
  const tabs = [canGrants && ["grants", "Grants"], ["roles", "Roles"], canGrants && ["effective", "Effective access"], canGrants && ["invariants", "Invariants"]].filter(Boolean);
  const cur = tabs.some(t => t[0] === tab) ? tab : tabs[0][0];
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "tabs",
    style: {
      marginTop: 0
    }
  }, tabs.map(t => /*#__PURE__*/React.createElement("button", {
    key: t[0],
    className: "tab" + (cur === t[0] ? " on" : ""),
    onClick: () => setTab(t[0])
  }, t[1]))), cur === "grants" && /*#__PURE__*/React.createElement(GrantsPanel, {
    nav: nav
  }), cur === "roles" && /*#__PURE__*/React.createElement(RolesPanel, null), cur === "effective" && /*#__PURE__*/React.createElement(EffectivePanel, null), cur === "invariants" && /*#__PURE__*/React.createElement(InvariantsPanel, null));
}
Object.assign(window, {
  RulesTable,
  RolesPanel,
  GrantsPanel,
  EffectivePanel,
  InvariantsPanel,
  AccessView,
  scopeLabel
});
})();
// ---- tiles.jsx ----
(function(){
const Dot = ({
  c
}) => /*#__PURE__*/React.createElement("i", {
  style: {
    width: 8,
    height: 8,
    borderRadius: "50%",
    background: c,
    boxShadow: `0 0 0 3px color-mix(in srgb,${c} 20%,transparent)`,
    flex: "none",
    display: "block"
  }
});
const TileHead = ({
  obj,
  left,
  right
}) => /*#__PURE__*/React.createElement("div", {
  className: "th"
}, /*#__PURE__*/React.createElement("div", {
  style: {
    minWidth: 0,
    flex: 1
  }
}, left), /*#__PURE__*/React.createElement("div", {
  style: {
    display: "flex",
    alignItems: "center",
    gap: 6,
    flex: "none"
  }
}, right, /*#__PURE__*/React.createElement(ActionBtn, {
  obj: obj
})));
const Name = ({
  children
}) => /*#__PURE__*/React.createElement("div", {
  className: "tname",
  title: children
}, children);
const Foot = ({
  items
}) => /*#__PURE__*/React.createElement("div", {
  className: "tfoot"
}, items.filter(Boolean).map((it, i) => /*#__PURE__*/React.createElement("button", {
  key: i,
  className: "fbtn" + (it.right ? " det" : ""),
  onClick: e => {
    e.stopPropagation();
    it.onClick();
  }
}, it.icon && /*#__PURE__*/React.createElement(Icon, {
  n: it.icon,
  s: 13
}), /*#__PURE__*/React.createElement("span", null, it.label), it.count !== undefined && /*#__PURE__*/React.createElement("b", null, it.count), !it.right && /*#__PURE__*/React.createElement(Icon, {
  n: "chev",
  s: 11
}))));
const IoMetrics = ({
  o
}) => /*#__PURE__*/React.createElement("div", {
  className: "mets"
}, /*#__PURE__*/React.createElement(Metric, {
  label: "IOPS",
  r: o.iops.r,
  w: o.iops.w,
  fmt: fmtNum,
  hist: o.hist.iops,
  color: "var(--accent)"
}), /*#__PURE__*/React.createElement(Metric, {
  label: "Throughput",
  unit: "GB/s",
  r: o.bw.r,
  w: o.bw.w,
  fmt: fmtBW,
  hist: o.hist.bw,
  color: "var(--ok)"
}));
function ClusterTile({
  c,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[c.status].c
    },
    onDoubleClick: () => nav.detail(c)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: c,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: c.status
    }), /*#__PURE__*/React.createElement(Name, null, c.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, c.mode === "nvme" ? "NVMe" : "block device")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: c.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "siting"), c.siting === "edge" ? "edge" : "data center"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "devices"), c.mode === "nvme" ? "NVMe" : "block dev"), c.caps.rebalancing ? /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "rebalancing"), /*#__PURE__*/React.createElement(Dot, {
    c: c.rebalancing ? "var(--warn)" : "var(--ok)"
  }), c.rebalancing ? "yes" : "no") : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      opacity: .6
    }
  }, /*#__PURE__*/React.createElement("i", null, "rebalancing"), "n/a"), c.counts.nodes > 0 && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: `Fault budget: ${c.faultBudget.kind === "failure_domain" ? "one failure domain" : c.faultBudget.tolerated + " node(s)"} may be lost before the cluster suspends`,
    style: c.faultBudget.lost > c.faultBudget.tolerated ? {
      color: "var(--bad)",
      borderColor: "color-mix(in srgb,var(--bad) 40%,transparent)"
    } : c.faultBudget.lost ? {
      color: "var(--warn)",
      borderColor: "color-mix(in srgb,var(--warn) 40%,transparent)"
    } : undefined
  }, /*#__PURE__*/React.createElement("i", null, "nodes online"), c.counts.nodesOnline, "/", c.counts.nodes, c.faultBudget.lost > 0 && ` · ${c.faultBudget.lost}/${c.faultBudget.tolerated} ${c.faultBudget.kind === "failure_domain" ? "FD" : ""} lost`)), /*#__PURE__*/React.createElement(Capacity, {
    total: c.capacity.total,
    used: c.capacity.used
  }), /*#__PURE__*/React.createElement(IoMetrics, {
    o: c
  }), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Hosts",
      count: c.counts.hosts,
      icon: "host",
      onClick: () => nav.layer(c, "hosts")
    }, {
      label: "Nodes",
      count: c.counts.nodes,
      icon: "node",
      onClick: () => nav.layer(c, "nodes")
    }, {
      label: "Pools",
      count: c.counts.pools,
      icon: "pool",
      onClick: () => nav.layer(c, "pools")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(c)
    }]
  }));
}
const HOST_CANDIDATE = {
  discovered: 1,
  inspecting: 1,
  inspected: 1
};
function HostTile({
  h,
  nav,
  select
}) {
  if (HOST_CANDIDATE[h.status]) return /*#__PURE__*/React.createElement(HostCandidateTile, {
    h: h,
    nav: nav,
    select: select
  });
  // a configured host only puts the sockets it was given into service
  const inUse = h.socketsUsed && h.socketsUsed.length ? h.socketsUsed : Array.from({
    length: h.sockets || 0
  }, (_, s) => s);
  const bySocket = inUse.map(s => {
    const d = h.devices.filter(x => x.kind === "nvme" && x.socket === s);
    return {
      s,
      total: d.length,
      free: d.filter(x => !x.assignedNodeId).length
    };
  }).filter(x => x.total > 0);
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[h.status].c
    },
    onDoubleClick: () => nav.detail(h)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: h,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: h.status
    }), /*#__PURE__*/React.createElement(Name, null, h.hostname)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "tsub"
    }, h.mgmtIp)
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: h.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, h.controlPlane && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--accent)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 10
  }), "control plane"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "nodes"), h.counts.nodes || "none"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "sockets"), h.socketsUsed && h.socketsUsed.length ? `${h.socketsUsed.length} of ${h.sockets}` : h.sockets), h.hostClass && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "class"), h.hostClass)), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, h.zoneId && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openZone(h.zoneId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "zone",
    s: 10
  }), regName(h.zoneId, "zone")), h.rack && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "rack"), h.rack), h.cabinet && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "cabinet"), h.cabinet), h.migrationTaint && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    },
    title: h.migrationTaint
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "move",
    s: 10
  }), "migration target"), h.mgmtNic && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "mgmt"), h.mgmtNic), h.dataNics && h.dataNics.length > 0 && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "data"), h.dataNics.join(", "))), /*#__PURE__*/React.createElement("div", {
    className: "sockets"
  }, bySocket.map(s => /*#__PURE__*/React.createElement("div", {
    className: "sock",
    key: s.s
  }, /*#__PURE__*/React.createElement("span", {
    className: "sk"
  }, "NUMA ", s.s), /*#__PURE__*/React.createElement("span", {
    className: "sv"
  }, s.total - s.free, /*#__PURE__*/React.createElement("em", null, "/", s.total), " nvme"), /*#__PURE__*/React.createElement("div", {
    className: "pips"
  }, Array.from({
    length: s.total
  }).map((_, i) => /*#__PURE__*/React.createElement("i", {
    key: i,
    className: i < s.total - s.free ? "on" : ""
  }))))), h.counts.blockFree > 0 && /*#__PURE__*/React.createElement("div", {
    className: "sock"
  }, /*#__PURE__*/React.createElement("span", {
    className: "sk"
  }, "unused"), /*#__PURE__*/React.createElement("span", {
    className: "sv"
  }, h.counts.blockFree, " free block dev"))), /*#__PURE__*/React.createElement("div", {
    className: "mets"
  }, /*#__PURE__*/React.createElement("div", {
    className: "met"
  }, /*#__PURE__*/React.createElement("div", {
    className: "k"
  }, /*#__PURE__*/React.createElement("span", null, "Compute")), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "VCPU"), /*#__PURE__*/React.createElement("b", null, h.vcpu)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "RAM"), /*#__PURE__*/React.createElement("b", null, fmtBytes(h.memory, 0))))), /*#__PURE__*/React.createElement("div", {
    className: "met"
  }, /*#__PURE__*/React.createElement("div", {
    className: "k"
  }, /*#__PURE__*/React.createElement("span", null, "Hugepages")), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "ALLOC"), /*#__PURE__*/React.createElement("b", null, fmtBytes(h.hugepages.allocated, 0))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "RESERVED"), /*#__PURE__*/React.createElement("b", null, fmtBytes(h.hugepages.reserved, 0)))))), h.counts.nodes === 0 && /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: "var(--accent)"
    }
  }, "Prepared & labelled \u2014 no storage node yet"), /*#__PURE__*/React.createElement(Foot, {
    items: [h.counts.nodes > 0 && {
      label: "Nodes",
      count: h.counts.nodes,
      icon: "node",
      onClick: () => nav.hostNodes(h)
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(h)
    }]
  }));
}
function HostCandidateTile({
  h,
  nav,
  select
}) {
  const selectable = h.status === "discovered" && select;
  const on = selectable && select.has(h.id);
  const nvme = h.devices.filter(d => d.kind === "nvme").length;
  return /*#__PURE__*/React.createElement("div", {
    className: "tile candidate" + (on ? " sel" : ""),
    style: {
      "--sc": STATUS_META[h.status].c
    },
    onDoubleClick: () => nav.detail(h)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: h,
    left: /*#__PURE__*/React.createElement("div", {
      style: {
        display: "flex",
        alignItems: "flex-start",
        gap: 9
      }
    }, selectable && /*#__PURE__*/React.createElement("button", {
      className: "selbox" + (on ? " on" : ""),
      title: "Select for preparation",
      onClick: e => {
        e.stopPropagation();
        select.toggle(h.id);
      }
    }, on && /*#__PURE__*/React.createElement(Icon, {
      n: "check",
      s: 10
    })), /*#__PURE__*/React.createElement("div", {
      style: {
        minWidth: 0
      }
    }, /*#__PURE__*/React.createElement(TrafficLight, {
      status: h.status
    }), /*#__PURE__*/React.createElement(Name, null, h.hostname))),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, "k8s worker")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: h.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "kubelet"), h.kubelet), h.zoneId && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openZone(h.zoneId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "zone",
    s: 10
  }), regName(h.zoneId, "zone")), h.rack && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "rack"), h.rack), h.k8sLabels["node.kubernetes.io/instance-type"] && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "type"), h.k8sLabels["node.kubernetes.io/instance-type"]), h.k8sLabels["topology.kubernetes.io/zone"] && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "zone"), h.k8sLabels["topology.kubernetes.io/zone"])), h.status === "discovered" && /*#__PURE__*/React.createElement("div", {
    className: "prepbox"
  }, "Not prepared. Deploying the inspection pod collects the NUMA topology, devices and NICs."), h.status === "inspecting" && /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), h.inspection ? h.inspection.pod : "inspection pod", " \u2014 collecting device and NUMA inventory\u2026"), h.status === "inspected" && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sockets"
  }, Array.from({
    length: h.sockets
  }, (_, s) => /*#__PURE__*/React.createElement("div", {
    className: "sock",
    key: s
  }, /*#__PURE__*/React.createElement("span", {
    className: "sk"
  }, "NUMA ", s), /*#__PURE__*/React.createElement("span", {
    className: "sv"
  }, h.devices.filter(d => d.socket === s).length, " dev \xB7 ", h.nics.filter(n => n.socket === s).length, " nic")))), /*#__PURE__*/React.createElement("div", {
    className: "prepbox ready"
  }, "Inventory collected \u2014 ", nvme, " NVMe, ", h.devices.length - nvme, " block, ", h.nics.length, " NIC(s). Choose sockets, memory, devices and NICs to finish.")), /*#__PURE__*/React.createElement("div", {
    className: "mets"
  }, /*#__PURE__*/React.createElement("div", {
    className: "met"
  }, /*#__PURE__*/React.createElement("div", {
    className: "k"
  }, /*#__PURE__*/React.createElement("span", null, "Node capacity")), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "VCPU"), /*#__PURE__*/React.createElement("b", null, h.vcpu)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "RAM"), /*#__PURE__*/React.createElement("b", null, fmtBytes(h.memory, 0))))), /*#__PURE__*/React.createElement("div", {
    className: "met"
  }, /*#__PURE__*/React.createElement("div", {
    className: "k"
  }, /*#__PURE__*/React.createElement("span", null, "Inventory")), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "DEVICES"), /*#__PURE__*/React.createElement("b", null, h.sockets ? h.devices.length : "—")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "NICS"), /*#__PURE__*/React.createElement("b", null, h.nics.length || "—"))))), /*#__PURE__*/React.createElement(Foot, {
    items: [h.status === "discovered" ? {
      label: "Prepare",
      icon: "plus",
      right: true,
      onClick: () => window.__ui.dialog(ACTIONS.host(h)[0].dialog, h)
    } : null, h.status === "inspected" ? {
      label: "Configure",
      icon: "gauge",
      right: true,
      onClick: () => window.__ui.dialog(configureHostDialog(h), h)
    } : null, h.status === "inspecting" ? {
      label: "Details",
      right: true,
      onClick: () => nav.detail(h)
    } : null]
  }));
}

// phase tracker for a running node operation
const OP_LABEL = {
  removal: "Removal",
  expansion: "Expansion",
  migration: "Migration"
};
const OP_HINT = {
  removal: "Data is rebalanced onto the remaining nodes, then the volumes whose primary sits here are moved off; the node and its device records are deleted at the end.",
  expansion: "The storage node is deployed and joins the cluster, then existing data is rebalanced onto it.",
  migration: "The node restarts on the prepared target host, data is rebalanced, and the record on the old host is removed."
};
function NodeOpBox({
  op,
  wide
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("div", {
    className: "opl"
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), /*#__PURE__*/React.createElement("b", null, OP_LABEL[op.kind] || op.kind), /*#__PURE__*/React.createElement("span", null, op.phase), op.kind === "migration" && op.targetHostname && /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "\u2192 ", op.targetHostname), op.kind === "removal" && !!op.volumesMoved && /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, op.volumesMoved, " volume(s) moved off")), /*#__PURE__*/React.createElement("div", {
    className: "opph"
  }, op.phases.map((p, i) => /*#__PURE__*/React.createElement("span", {
    key: p,
    className: "opp" + (i < op.phaseIndex ? " done" : i === op.phaseIndex ? " on" : ""),
    title: p
  }, wide && /*#__PURE__*/React.createElement("i", null, p)))));
}
function NodeTile({
  n,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[n.status].c
    },
    onDoubleClick: () => nav.detail(n)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: n,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: n.status
    }), /*#__PURE__*/React.createElement(Name, null, n.hostname)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "tsub",
      title: n.multipath ? (n.dataNics || []).map(x => x.ip).join(", ") : n.ip
    }, n.ip, n.multipath && /*#__PURE__*/React.createElement("em", {
      style: {
        fontStyle: "normal",
        color: "var(--accent)"
      }
    }, " +1"))
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: n.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: n.failureDomain ? null : {
      opacity: .55
    }
  }, /*#__PURE__*/React.createElement("i", null, "fd"), n.failureDomain || "—"), n.physicalLabel && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "phys"), n.physicalLabel), n.hostId && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openHost(n.clusterId, n.hostId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "host",
    s: 10
  }), "host")), n.op && /*#__PURE__*/React.createElement(NodeOpBox, {
    op: n.op
  }), /*#__PURE__*/React.createElement(Capacity, {
    total: n.capacity.total,
    used: n.capacity.used
  }), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "vCPU res"), /*#__PURE__*/React.createElement("b", null, n.cpuReserved)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "RAM used"), /*#__PURE__*/React.createElement("b", null, fmtBytes(n.memory.used, 0), /*#__PURE__*/React.createElement("em", {
    style: {
      color: "var(--dim2)",
      fontStyle: "normal"
    }
  }, "/", fmtBytes(n.memory.total, 0)))), /*#__PURE__*/React.createElement("div", {
    title: "Kubernetes request/limit"
  }, /*#__PURE__*/React.createElement("span", null, "RAM res"), /*#__PURE__*/React.createElement("b", null, fmtBytes(n.memory.reserved, 0))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Hugepages"), /*#__PURE__*/React.createElement("b", null, fmtBytes(n.hugepages.used, 0), /*#__PURE__*/React.createElement("em", {
    style: {
      color: "var(--dim2)",
      fontStyle: "normal"
    }
  }, "/", fmtBytes(n.hugepages.total, 0))))), /*#__PURE__*/React.createElement(IoMetrics, {
    o: n
  }), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Devices",
      count: n.counts.devices,
      icon: "device",
      onClick: () => nav.layer(n, "devices")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(n)
    }]
  }));
}
function DeviceTile({
  d,
  nav
}) {
  const primary = d.mode === "nvme" ? d.serial : d.blockdev;
  const showHealth = (d.status === "online" || d.status === "read_only") && d.health;
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[d.status].c
    },
    onDoubleClick: () => nav.detail(d)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: d,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: d.status
    }), /*#__PURE__*/React.createElement(Name, null, primary)),
    right: showHealth ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      title: "Media health"
    }, /*#__PURE__*/React.createElement("i", null, "health"), /*#__PURE__*/React.createElement(Dot, {
      c: STATUS_META[d.health].c
    })) : null
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: d.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, d.mode === "nvme" ? "pcie" : "serial"), (d.mode === "nvme" ? d.pcie : d.serial) || "—"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "dev"), d.blockdev || "—")), /*#__PURE__*/React.createElement(Capacity, {
    total: d.capacity.total,
    used: d.capacity.used
  }), /*#__PURE__*/React.createElement(IoMetrics, {
    o: d
  }), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(d)
    }]
  }));
}
Object.assign(window, {
  NodeOpBox,
  OP_HINT,
  Dot,
  TileHead,
  Name,
  Foot,
  IoMetrics,
  ClusterTile,
  HostTile,
  HostCandidateTile,
  NodeTile,
  DeviceTile
});
})();
// ---- tiles-data.jsx ----
(function(){
function PoolTile({
  p,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[p.status].c
    },
    onDoubleClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: p,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    }), /*#__PURE__*/React.createElement(Name, null, p.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, p.enabled ? "pool" : "no new volumes")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: p.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, p.dhchap && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    },
    title: "bi-directional DH-CHAP \u2014 set at creation, immutable"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "lock",
    s: 10
  }), "dhchap bi-dir"), p.storageClasses.length ? p.storageClasses.map(sc => /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    key: sc.uuid,
    title: "StorageClass " + sc.name,
    onClick: e => {
      e.stopPropagation();
      nav.openStorageClass(sc.uuid);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 10
  }), /*#__PURE__*/React.createElement("i", null, sc.k8s_cluster || "class"), sc.variant)) : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      opacity: .6
    }
  }, /*#__PURE__*/React.createElement("i", null, "storage class"), "none")), /*#__PURE__*/React.createElement(QosChips, {
    qos: p.qos
  }), /*#__PURE__*/React.createElement(Capacity, {
    label: "Provisioned",
    total: p.capacity.total,
    used: p.capacity.used
  }), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Volume data"), /*#__PURE__*/React.createElement("b", null, fmtBytes(p.lvolBytes))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Snapshots"), /*#__PURE__*/React.createElement("b", null, fmtBytes(p.snapshotBytes))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Utilized"), /*#__PURE__*/React.createElement("b", null, fmtBytes(p.capacity.used)))), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Volumes",
      count: p.counts.volumes,
      icon: "volume",
      onClick: () => nav.layer(p, "volumes")
    }, {
      label: "Snapshots",
      count: p.counts.snapshots,
      icon: "camera",
      onClick: () => nav.layer(p, "snapshots")
    }, p.counts.storageClasses > 0 && {
      label: "Classes",
      count: p.counts.storageClasses,
      icon: "k8s",
      onClick: () => nav.layer(p, "storageclasses")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(p)
    }]
  }));
}
function VolumeTile({
  v,
  nav
}) {
  const link = (ref, role) => ref ? /*#__PURE__*/React.createElement("button", {
    key: role,
    className: "lab link",
    title: ref.hostname,
    onClick: e => {
      e.stopPropagation();
      nav.openNode(v.clusterId, ref.uuid);
    }
  }, /*#__PURE__*/React.createElement("i", null, role), ref.hostname.split("-").slice(-2).join("-")) : null;
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[v.status].c
    },
    onDoubleClick: () => nav.detail(v)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: v,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: v.status
    }), /*#__PURE__*/React.createElement(Name, null, v.name)),
    right: /*#__PURE__*/React.createElement(React.Fragment, null, v.dataReduction && /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--accent)",
        borderColor: "var(--accent-line)"
      },
      title: "Compression-dedup enabled"
    }, "comp-dedup"), v.crypto ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "lock",
      s: 11
    }), "enc") : /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        opacity: .6
      }
    }, "no enc"))
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: v.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, link(v.nodes.primary, "P"), link(v.nodes.secondary, "S"), link(v.nodes.tertiary, "T"), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openPool(v.clusterId, v.poolId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "pool",
    s: 11
  }), v.poolName), v.bucket && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    title: "This volume is the filesystem behind an S3 bucket",
    onClick: e => {
      e.stopPropagation();
      nav.openBucket(v.bucket.uuid);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cloud",
    s: 10
  }), v.bucket.name), v.pvc && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    title: "PVC " + v.pvc.namespace + "/" + v.pvc.name + " · " + v.pvc.k8s_cluster,
    onClick: e => {
      e.stopPropagation();
      nav.openPvc(v.pvc.uuid);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 10
  }), v.pvc.namespace, "/", v.pvc.name)), (v.baseSnapshot || v.backupPolicy || v.replication || v.migration) && /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, v.baseSnapshot && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    title: `Cloned from ${v.baseSnapshot.snapshot_name}`,
    onClick: e => {
      e.stopPropagation();
      nav.openSnapshot(v.clusterId, v.baseSnapshot.uuid);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "camera",
    s: 10
  }), v.baseSnapshot.snapshot_name), v.backupPolicy && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.layerRef(v.clusterId, "policies");
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 10
  }), v.backupPolicy.policy_name), v.replication && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    title: `${v.replication.mode} replication · ${v.replication.policyName}`,
    onClick: e => {
      e.stopPropagation();
      nav.openRPolicy(v.replication.policyId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "shield",
    s: 10
  }), v.replication.policyName), (v.consistencyGroups || []).map(g => /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    key: g.uuid,
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    },
    onClick: e => {
      e.stopPropagation();
      nav.openCgroup(g.uuid);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), g.name)), v.affinity && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: v.affinity.satisfied === false ? {
      color: "var(--warn)",
      borderColor: "color-mix(in srgb,var(--warn) 40%,transparent)"
    } : {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    },
    title: v.affinity.mode === "pod" ? `follows workload ${v.affinity.workload}` : `pinned to ${v.affinity.pinned_node}`
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), v.affinity.mode === "pod" ? "pod affinity" : "pinned", v.affinity.satisfied === false ? " · off-node" : ""), v.migration && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--info)",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    },
    title: v.migration.instant ? `moved instantly from ${v.migration.from} · ${v.migration.reason}` : ""
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "move",
    s: 10
  }), v.migration.state === "completed" ? "moved → " : "migrating → ", v.migration.target)), v.replication && /*#__PURE__*/React.createElement("div", {
    className: "replbar" + (v.replication.status === "healthy" ? "" : " bad")
  }, /*#__PURE__*/React.createElement(TrafficLight, {
    status: v.replication.status,
    sm: true
  }), /*#__PURE__*/React.createElement("span", {
    className: "rl"
  }, v.replication.mode === "synchronous" ? "sync" : "async"), /*#__PURE__*/React.createElement("span", {
    className: "rv",
    title: "Last replication"
  }, clockOf(v.replication.lastAt)), /*#__PURE__*/React.createElement("span", {
    className: "rv",
    title: "Backlog"
  }, v.replication.mode === "synchronous" ? "0 backlog" : fmtBytes(v.replication.backlog))), /*#__PURE__*/React.createElement(Capacity, {
    label: "Provisioned",
    total: v.capacity.total,
    used: v.capacity.used
  }), v.dataReduction && v.logicalUsed > v.capacity.used && /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Logical"), /*#__PURE__*/React.createElement("b", null, fmtBytes(v.logicalUsed))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "On disk"), /*#__PURE__*/React.createElement("b", null, fmtBytes(v.capacity.used))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Saving"), /*#__PURE__*/React.createElement("b", {
    style: {
      color: "var(--ok)"
    }
  }, (v.logicalUsed / Math.max(1, v.capacity.used)).toFixed(2), "\xD7"))), /*#__PURE__*/React.createElement(IoMetrics, {
    o: v
  }), /*#__PURE__*/React.createElement(QosChips, {
    qos: v.qos
  }), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Snapshots",
      count: v.counts.snapshots,
      icon: "camera",
      onClick: () => nav.layer(v, "snapshots")
    }, {
      label: "Backup",
      count: v.counts.backupVersions || 0,
      icon: "cloud",
      onClick: () => nav.layer(v, "backups")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(v)
    }]
  }));
}
function SnapshotTile({
  s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[s.status].c
    },
    onDoubleClick: () => nav.detail(s)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: s,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: s.status
    }), /*#__PURE__*/React.createElement(Name, null, s.name)),
    right: s.backupVersionId ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"
      },
      title: `Backup version ${s.backupVersionId} was taken from this snapshot`
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "cloud",
      s: 10
    }), s.backupVersionId) : /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        opacity: .6
      }
    }, "not backed up")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: s.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openVolume(s.clusterId, s.poolId, s.volumeId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "volume",
    s: 10
  }), s.volumeName), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openPool(s.clusterId, s.poolId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "pool",
    s: 10
  }), s.poolName), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "gen"), s.seq || "—")), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Taken"), /*#__PURE__*/React.createElement("b", null, fmtDate(s.createdAt))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Age"), /*#__PURE__*/React.createElement("b", null, fmtAgo(s.createdAt))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Delta size"), /*#__PURE__*/React.createElement("b", null, fmtBytes(s.capacity.total)))), !s.parentId && s.seq > 1 && /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "Predecessor deleted \u2014 this snapshot now chains to the volume"), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(s)
    }]
  }));
}
function BackupTile({
  b,
  nav
}) {
  const vs = b.versions || [];
  const latest = vs[vs.length - 1];
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[b.status].c
    },
    onDoubleClick: () => nav.detail(b)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: b,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: b.status
    }), /*#__PURE__*/React.createElement(Name, null, b.volumeName)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, b.counts.versions, " version", b.counts.versions === 1 ? "" : "s")
  }), /*#__PURE__*/React.createElement("div", {
    className: "uuid"
  }, /*#__PURE__*/React.createElement("span", null, b.chainId)), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openVolume(b.clusterId, b.poolId, b.volumeId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "volume",
    s: 10
  }), "volume"), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openPool(b.clusterId, b.poolId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "pool",
    s: 10
  }), b.poolName), b.policyName ? /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.layerRef(b.clusterId, "policies");
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 10
  }), b.policyName) : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      opacity: .65
    }
  }, /*#__PURE__*/React.createElement("i", null, "policy"), "manual")), /*#__PURE__*/React.createElement("div", {
    className: "chainbar",
    title: `${vs.length} versions: 1 full + ${Math.max(0, vs.length - 1)} deltas`
  }, vs.map(v => /*#__PURE__*/React.createElement("i", {
    key: v.id,
    className: v.type,
    style: {
      flex: v.type === "full" ? 3 : 1
    }
  }))), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Latest version"), /*#__PURE__*/React.createElement("b", null, latest ? latest.id : "—")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Taken"), /*#__PURE__*/React.createElement("b", null, fmtDate(b.latestAt))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Full"), /*#__PURE__*/React.createElement("b", null, fmtBytes(b.fullBytes))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Deltas"), /*#__PURE__*/React.createElement("b", null, fmtBytes(b.deltaBytes)))), b.lastMergeAt && /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "Last merge ", fmtAgo(b.lastMergeAt), " \xB7 ", b.counts.merged, " version(s) merged into the full so far"), /*#__PURE__*/React.createElement("div", {
    className: "bucket",
    title: b.bucket
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cloud",
    s: 11
  }), b.bucket), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Versions",
      count: b.counts.versions,
      icon: "cloud",
      onClick: () => nav.detail(b)
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(b)
    }]
  }));
}
function PolicyTile({
  p,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": "var(--ok)"
    },
    onDoubleClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: p,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: "active"
    }), /*#__PURE__*/React.createElement(Name, null, p.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, p.consistencyGroup ? "group-consistent" : "backup policy")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: p.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "every"), p.finest || "—"), p.consistencyGroup && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), "one consistency group"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "volumes"), p.counts.volumes), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "chains"), p.counts.chains)), /*#__PURE__*/React.createElement(BackupSchedule, {
    rows: p.schedule
  }), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(p)
    }]
  }));
}
Object.assign(window, {
  PoolTile,
  VolumeTile,
  SnapshotTile,
  BackupTile,
  PolicyTile
});
})();
// ---- details.jsx ----
(function(){
const Props = ({
  rows
}) => /*#__PURE__*/React.createElement("dl", {
  className: "props"
}, rows.filter(Boolean).map(([k, v], i) => /*#__PURE__*/React.createElement(React.Fragment, {
  key: i
}, /*#__PURE__*/React.createElement("dt", null, k), /*#__PURE__*/React.createElement("dd", null, v === null || v === undefined || v === "" ? /*#__PURE__*/React.createElement("span", {
  style: {
    color: "var(--dim2)"
  }
}, "\u2014") : v))));
const Stat = ({
  k,
  v,
  s,
  c
}) => {
  const n = typeof v === "string" || typeof v === "number" ? String(v).length : 0;
  return /*#__PURE__*/React.createElement("div", {
    className: "stat"
  }, /*#__PURE__*/React.createElement("div", {
    className: "k",
    title: k
  }, k), /*#__PURE__*/React.createElement("div", {
    className: "v" + (n > 22 ? " xs" : n > 15 ? " sm" : n > 10 ? " md" : ""),
    style: {
      color: c
    },
    title: n > 10 ? String(v) : null
  }, v), s && /*#__PURE__*/React.createElement("div", {
    className: "s"
  }, s));
};
const Ref = ({
  onClick,
  label
}) => /*#__PURE__*/React.createElement("button", {
  onClick: onClick,
  style: {
    color: "var(--accent)",
    fontFamily: "inherit"
  }
}, label);
const regName = (id, fb) => {
  const o = REG[id];
  return o ? o.hostname || o.name : fb || (id ? shortId(id) : "—");
};
const NavCard = ({
  icon,
  title,
  sub,
  count,
  onClick
}) => /*#__PURE__*/React.createElement("button", {
  className: "navcard",
  onClick: onClick
}, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
  className: "t"
}, /*#__PURE__*/React.createElement(Icon, {
  n: icon,
  s: 14,
  c: "var(--accent)"
}), title), /*#__PURE__*/React.createElement("div", {
  className: "sb"
}, sub)), /*#__PURE__*/React.createElement("span", {
  className: "c"
}, count));
const NicTable = ({
  nics,
  mgmt,
  data
}) => /*#__PURE__*/React.createElement("div", {
  className: "card"
}, /*#__PURE__*/React.createElement("div", {
  className: "bd",
  style: {
    padding: 0
  }
}, /*#__PURE__*/React.createElement("table", {
  className: "dt"
}, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Interface"), /*#__PURE__*/React.createElement("th", null, "Address"), /*#__PURE__*/React.createElement("th", null, "MAC"), /*#__PURE__*/React.createElement("th", null, "Speed"), /*#__PURE__*/React.createElement("th", null, "Socket"), /*#__PURE__*/React.createElement("th", null, "State"), /*#__PURE__*/React.createElement("th", null, "Role"))), /*#__PURE__*/React.createElement("tbody", null, (nics || []).map(n => /*#__PURE__*/React.createElement("tr", {
  key: n.name
}, /*#__PURE__*/React.createElement("td", {
  className: "mono",
  style: {
    fontWeight: 600
  }
}, n.name), /*#__PURE__*/React.createElement("td", {
  className: "mono"
}, n.address), /*#__PURE__*/React.createElement("td", {
  className: "mono",
  style: {
    color: "var(--dim)"
  }
}, n.mac), /*#__PURE__*/React.createElement("td", {
  className: "mono"
}, n.speed, " GbE"), /*#__PURE__*/React.createElement("td", {
  className: "mono"
}, n.socket), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
  status: n.state === "up" ? "online" : "offline"
})), /*#__PURE__*/React.createElement("td", null, mgmt === n.name ? /*#__PURE__*/React.createElement("span", {
  className: "badge k8s"
}, "management") : (data || []).includes(n.name) ? /*#__PURE__*/React.createElement("span", {
  className: "badge"
}, "data") : /*#__PURE__*/React.createElement("span", {
  style: {
    color: "var(--dim2)"
  }
}, "\u2014"))))))));
function Chart({
  title,
  data,
  color,
  fmt,
  unit
}) {
  if (!data || data.length < 2) return null;
  const w = 300,
    h = 62;
  const max = Math.max(...data, 1),
    min = Math.min(...data);
  const rng = max - min || max || 1;
  const line = data.map((v, i) => `${i / (data.length - 1) * w},${h - (v - min) / rng * (h - 8) - 4}`).join(" ");
  const gid = "g" + title.replace(/\W/g, "");
  return /*#__PURE__*/React.createElement("div", {
    style: {
      marginBottom: 14
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      justifyContent: "space-between",
      alignItems: "baseline",
      marginBottom: 5
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 11,
      color: "var(--dim)",
      letterSpacing: ".05em",
      textTransform: "uppercase"
    }
  }, title), /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      fontSize: 12
    }
  }, fmt(data[data.length - 1]), unit && /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, " ", unit))), /*#__PURE__*/React.createElement("svg", {
    width: "100%",
    height: h,
    viewBox: `0 0 ${w} ${h}`,
    preserveAspectRatio: "none",
    style: {
      display: "block",
      overflow: "visible"
    }
  }, /*#__PURE__*/React.createElement("defs", null, /*#__PURE__*/React.createElement("linearGradient", {
    id: gid,
    x1: "0",
    y1: "0",
    x2: "0",
    y2: "1"
  }, /*#__PURE__*/React.createElement("stop", {
    offset: "0%",
    stopColor: color,
    stopOpacity: ".22"
  }), /*#__PURE__*/React.createElement("stop", {
    offset: "100%",
    stopColor: color,
    stopOpacity: "0"
  }))), /*#__PURE__*/React.createElement("polygon", {
    points: `0,${h} ${line} ${w},${h}`,
    fill: `url(#${gid})`
  }), /*#__PURE__*/React.createElement("polyline", {
    points: line,
    fill: "none",
    stroke: color,
    strokeWidth: "1.5",
    vectorEffect: "non-scaling-stroke",
    strokeLinejoin: "round"
  })));
}
const IOCards = ({
  o
}) => /*#__PURE__*/React.createElement("div", {
  className: "card"
}, /*#__PURE__*/React.createElement("h3", null, "Live I/O \xB7 /iostats, last 28 samples"), /*#__PURE__*/React.createElement("div", {
  className: "bd"
}, /*#__PURE__*/React.createElement(Chart, {
  title: "IOPS (read + write)",
  data: o.hist.iops,
  color: "var(--accent)",
  fmt: fmtNum
}), /*#__PURE__*/React.createElement(Chart, {
  title: "Throughput",
  data: o.hist.bw,
  color: "var(--ok)",
  fmt: fmtBW,
  unit: "GB/s"
}), /*#__PURE__*/React.createElement("div", {
  className: "stats",
  style: {
    marginTop: 4
  }
}, /*#__PURE__*/React.createElement(Stat, {
  k: "IOPS read",
  v: fmtNum(o.iops.r)
}), /*#__PURE__*/React.createElement(Stat, {
  k: "IOPS write",
  v: fmtNum(o.iops.w)
}), /*#__PURE__*/React.createElement(Stat, {
  k: "Read",
  v: fmtBW(o.bw.r),
  s: "GB/s"
}), /*#__PURE__*/React.createElement(Stat, {
  k: "Write",
  v: fmtBW(o.bw.w),
  s: "GB/s"
}))));
const DetailHead = ({
  obj,
  title,
  sub,
  badge
}) => /*#__PURE__*/React.createElement("div", {
  className: "dhead"
}, /*#__PURE__*/React.createElement("div", {
  style: {
    minWidth: 0,
    flex: 1
  }
}, /*#__PURE__*/React.createElement("div", {
  style: {
    display: "flex",
    alignItems: "center",
    gap: 11,
    flexWrap: "wrap"
  }
}, /*#__PURE__*/React.createElement("h1", null, title), /*#__PURE__*/React.createElement(TrafficLight, {
  status: obj.status
}), badge), /*#__PURE__*/React.createElement("div", {
  style: {
    display: "flex",
    alignItems: "center",
    gap: 14,
    marginTop: 5,
    flexWrap: "wrap"
  }
}, /*#__PURE__*/React.createElement(Uuid, {
  value: obj.id,
  short: false
}), sub)), /*#__PURE__*/React.createElement(ActionBtn, {
  obj: obj,
  big: true
}));
const QosProps = ({
  qos,
  fallback
}) => qos ? /*#__PURE__*/React.createElement(Props, {
  rows: [["Max R/W IOPS", qos.rw_ios_per_sec ? fmtNum(qos.rw_ios_per_sec) : "unlimited"], ["Max R/W throughput", qos.rw_mbytes_per_sec ? qos.rw_mbytes_per_sec + " MB/s" : "unlimited"], ["Max read throughput", qos.r_mbytes_per_sec ? qos.r_mbytes_per_sec + " MB/s" : "unlimited"], ["Max write throughput", qos.w_mbytes_per_sec ? qos.w_mbytes_per_sec + " MB/s" : "unlimited"]]
}) : /*#__PURE__*/React.createElement("div", {
  className: "nolim"
}, fallback);

// The connection fields differ per provider — a Vault transit mount means
// nothing to AWS KMS, so only emit the rows that provider actually has.
const KMS_LABEL = {
  hashicorp_vault: "HashiCorp Vault",
  aws_kms: "AWS KMS",
  azure_key_vault: "Azure Key Vault",
  gcp_kms: "Google Cloud KMS",
  kmip: "KMIP appliance"
};
const KMS_ADDR = {
  hashicorp_vault: "Vault address",
  aws_kms: "Endpoint",
  azure_key_vault: "Vault URI",
  gcp_kms: "Key ring",
  kmip: "KMIP endpoint"
};
const KMS_ROLE = {
  hashicorp_vault: "Role",
  aws_kms: "Role ARN",
  azure_key_vault: "Client / tenant id",
  gcp_kms: "Role",
  kmip: "Client certificate secret"
};
function KMS_ROWS(k) {
  const p = k.provider;
  return [["Provider", KMS_LABEL[p] || p], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
    status: k.status === "connected" ? "online" : k.status === "sealed" ? "suspended" : "unreachable"
  })], [KMS_ADDR[p] || "Address", k.address], p === "aws_kms" ? ["Region", k.region] : null, p === "hashicorp_vault" ? ["Namespace", k.namespace] : null, ["Auth method", k.auth_method], k.auth_role ? [KMS_ROLE[p] || "Role", k.auth_role] : null, p === "hashicorp_vault" ? ["Transit mount", k.mount_path + "/"] : null, ["Key", k.key_name], ["Key type", k.key_type], ["Rotation", k.rotation_days ? "every " + k.rotation_days + " days" : "manual"], ["Verify TLS", k.verify_tls ? "yes" : "no"], ["Keys in use", k.keys_in_use + " encrypted volume(s)"], ["Scope", "every encrypted volume and bucket in this cluster"], ["Last check", fmtDate(k.last_check_at) + " · " + fmtAgo(k.last_check_at)]].filter(Boolean);
}
function ClusterDetail({
  o: c,
  nav
}) {
  const [tab, setTab] = useState("overview");
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: c,
    title: c.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, c.siting === "edge" ? "edge" : "data center"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, c.mode === "nvme" ? "NVMe" : "block device")),
    sub: c.caps.rebalancing ? /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)",
        display: "flex",
        alignItems: "center",
        gap: 6
      }
    }, "rebalancing", /*#__PURE__*/React.createElement(Dot, {
      c: c.rebalancing ? "var(--warn)" : "var(--ok)"
    }), c.rebalancing ? "in progress" : "idle") : /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim2)"
      }
    }, "edge deployment \u2014 no rebalancing, no task engine")
  }), /*#__PURE__*/React.createElement(Tabs, {
    active: tab,
    onChange: setTab,
    items: [{
      k: "overview",
      label: "Overview",
      icon: "cluster"
    }, {
      k: "alerts",
      label: "Alerts",
      icon: "bell"
    }, {
      k: "ops",
      label: "Operations",
      icon: "refresh"
    }, {
      k: "file",
      label: "File storage",
      icon: "folder"
    }, c.caps.tasks && {
      k: "tasks",
      label: "Tasks",
      icon: "gauge"
    }, {
      k: "logs",
      label: "Cluster log",
      icon: "filter"
    }, {
      k: "splane",
      label: "Storage plane logs",
      icon: "node"
    }].filter(Boolean)
  }), tab === "alerts" && /*#__PURE__*/React.createElement(AlertsPanel, {
    cluster: c,
    nav: nav
  }), tab === "ops" && /*#__PURE__*/React.createElement(OperationsPanel, {
    cluster: c,
    nav: nav
  }), tab === "file" && /*#__PURE__*/React.createElement(FileStoragePanel, {
    cluster: c,
    nav: nav
  }), tab === "tasks" && c.caps.tasks && /*#__PURE__*/React.createElement(TasksPanel, {
    cluster: c
  }), tab === "logs" && /*#__PURE__*/React.createElement(ClusterLogPanel, {
    cluster: c
  }), tab === "splane" && /*#__PURE__*/React.createElement(StoragePlaneLogPanel, {
    cluster: c
  }), tab === "overview" && /*#__PURE__*/React.createElement(React.Fragment, null, c.status === "degraded" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Cluster degraded."), " ", c.counts.nodes - c.counts.nodesOnline, " of ", c.counts.nodes, " storage nodes are not online", c.faultBudget.kind === "failure_domain" ? ` in ${c.faultBudget.lost} failure domain` : "", " \u2014 inside the fault budget of ", c.faultBudget.kind === "failure_domain" ? "one failure domain" : `${c.faultBudget.tolerated} node${c.faultBudget.tolerated === 1 ? "" : "s"}`, ", capacity is served with reduced redundancy. Losing ", c.faultBudget.kind === "failure_domain" ? "a second domain" : "one more node", " suspends the cluster.")), c.status === "suspended" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Cluster suspended."), " ", c.counts.nodesOnline === 0 ? "All storage nodes are stopped. Restart the cluster to resume serving volumes." : `${c.counts.nodes - c.counts.nodesOnline} of ${c.counts.nodes} storage nodes are not online${c.faultBudget.kind === "failure_domain" ? ` across ${c.faultBudget.lost} failure domains` : ""} — more than the fault budget of ${c.faultBudget.kind === "failure_domain" ? "one failure domain" : c.faultBudget.tolerated + " node" + (c.faultBudget.tolerated === 1 ? "" : "s")}. I/O is halted until nodes return; the cluster resumes as degraded once the loss is back inside the budget.`)), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Capacity used",
    v: pct(c.capacity.used, c.capacity.total).toFixed(0) + "%",
    s: `${fmtBytes(c.capacity.used)} of ${fmtBytes(c.capacity.total)}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Total IOPS",
    v: fmtNum(c.iops.r + c.iops.w),
    s: `${fmtNum(c.iops.r)} r · ${fmtNum(c.iops.w)} w`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Throughput",
    v: fmtBW(c.bw.r + c.bw.w),
    s: "GB/s combined"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Nodes online",
    v: `${c.counts.nodesOnline}/${c.counts.nodes}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Hosts",
    v: `${c.counts.hostsAvailable}/${c.counts.hosts}`,
    s: "available"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Devices",
    v: c.counts.devices,
    s: `${c.counts.devicesOnline} online`
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "host",
    title: "Hosts",
    sub: "prepared machines",
    count: c.counts.hosts,
    onClick: () => nav.layer(c, "hosts")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "node",
    title: "Storage nodes",
    sub: "devices & I/O",
    count: c.counts.nodes,
    onClick: () => nav.layer(c, "nodes")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "pool",
    title: "Storage pools",
    sub: "QoS & tenancy",
    count: c.counts.pools,
    onClick: () => nav.layer(c, "pools")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Logical volumes",
    sub: "across all pools",
    count: c.counts.volumes,
    onClick: () => nav.layer(c, "volumes")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "camera",
    title: "Snapshots",
    sub: "all pools",
    count: c.counts.snapshots,
    onClick: () => nav.layer(c, "snapshots")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "cloud",
    title: "Backups",
    sub: "object storage",
    count: c.counts.backups,
    onClick: () => nav.layer(c, "backups")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "clock",
    title: "Backup policies",
    sub: "schedules & retention",
    count: c.counts.policies,
    onClick: () => nav.layer(c, "policies")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "shield",
    title: "Replication policies",
    sub: c.caps.async_replication ? "DR & synchronous" : "Kubernetes only",
    count: c.caps.async_replication ? c.counts.rpolicies : "n/a",
    onClick: () => nav.layer(c, "rpolicies")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "link",
    title: "Consistency groups",
    sub: "grouped volumes & group snapshots",
    count: c.counts.cgroups,
    onClick: () => nav.layer(c, "cgroups")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "move",
    title: "Migrations",
    sub: "move storage to other nodes or clusters",
    count: c.counts.migrations,
    onClick: () => nav.layer(c, "migrations")
  }), c.objectStorage.enabled && /*#__PURE__*/React.createElement(NavCard, {
    icon: "cloud",
    title: "Buckets",
    sub: "S3 on cluster capacity",
    count: c.counts.buckets,
    onClick: () => nav.layer(c, "buckets")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "zone",
    title: "Zones",
    sub: c.stretched ? "stretched cluster" : "single zone",
    count: c.counts.zones,
    onClick: () => nav.layer(c, "zones")
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Cluster configuration"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Label", c.name], ["Siting", c.siting === "edge" ? "edge" : "data center"], ["Device class", c.mode === "nvme" ? "NVMe" : "block device"], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: c.status
    })], ["Rebalancing", c.caps.rebalancing ? c.rebalancing ? "yes" : "no" : "not available on edge clusters"], ["Task engine", c.caps.tasks ? "yes" : "not available on edge clusters"], ["Stripe", `${c.distrNdcs} data + ${c.distrNpcs} parity chunks`], ["Software version", `${c.version} · set by the operator release`], ["Regions", (c.regions || []).join(", ") || null], ["Zones", (c.zoneIds || []).length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        gap: 8,
        justifyContent: "flex-end",
        flexWrap: "wrap"
      }
    }, c.zoneIds.map(id => /*#__PURE__*/React.createElement(Ref, {
      key: id,
      onClick: () => nav.openZone(id),
      label: regName(id, "zone")
    }))) : null], ["Stretched", c.stretched ? `yes — ${(c.zoneIds || []).length} zones` : "no (single zone)"], ["DR target eligible", c.drEligible ? "yes" : "no"], [c.mgmtKind === "Kubernetes API" ? "Kubernetes API" : "Management endpoint", c.mgmt], ["Created", fmtDate(c.createdAt)]]
  }))), /*#__PURE__*/React.createElement(IOCards, {
    o: c
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Feature flags \xB7 fixed at creation"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Multipathing", c.multipathing ? `enabled · ${c.multipathNodes}/${c.counts.nodes} nodes on two paths` : "disabled · single data NIC per node"], ["Node affinity", c.nodeAffinity === "none" ? "none — free placement" : c.nodeAffinity === "strict" ? "strict — volumes can be pinned" : "soft — prefer to keep in place"], ["Pod affinity", c.podAffinity ? "enabled — front storage follows the workload" : "disabled"], ["Failure domains", c.fd.enabled ? `enabled · per ${c.fd.scope}` : "disabled"], ["Synchronous replication", c.syncReplication ? "enabled" : "disabled"], ["Backups", c.backupEnabled ? "enabled" : "disabled"], ["File storage (RWX)", c.fileStorage.enabled ? "pNFS · " + c.counts.rwxPvcs + " claim(s) · MDS " + c.fileStorage.mds_state : "disabled"], ["Object storage (S3)", c.objectStorage.enabled ? c.counts.buckets + " bucket(s) · metadata in FoundationDB" : "disabled"], ["Zones", (c.zoneIds || []).length]]
  }), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginTop: 10,
      marginBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "Zones, failure domains and synchronous replication cannot be changed after the cluster is created."))), c.fd.enabled && /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Failure domains \xB7 per ", c.fd.scope), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, c.fd.scope), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Nodes"), /*#__PURE__*/React.createElement("th", null, "Balance"))), /*#__PURE__*/React.createElement("tbody", null, c.fd.domains.map(d => /*#__PURE__*/React.createElement("tr", {
    key: d.name
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600,
      color: d.name === "unassigned" ? "var(--bad)" : undefined
    }
  }, d.name), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, d.nodes), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("div", {
    className: "bar",
    style: {
      maxWidth: 140
    }
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: d.nodes / Math.max(1, c.fd.max) * 100 + "%",
      "--bc": d.name === "unassigned" ? "var(--bad)" : d.nodes === c.fd.max ? "var(--accent)" : "var(--ok)"
    }
  }))))))), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      margin: 12
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: c.fd.balanced ? "check" : "alert",
    s: 12
  }), c.fd.unassigned ? `${c.fd.unassigned} node(s) sit in no failure domain — their host carries no ${c.fd.scope} taint.` : (c.fd.thin || []).length ? `${(c.fd.thin || []).join(", ")} carries a single node. Each ${c.fd.scope} must hold at least two storage nodes, so a domain can lose one without losing its share of the data.` : c.fd.domains.length < 2 ? `Only one ${c.fd.scope} is in use. Failure domains need at least two, each with at least two nodes.` : c.fd.balanced ? `Balanced: ${c.fd.min}–${c.fd.max} nodes per ${c.fd.scope}, at least two each. Nodes are added and removed in pairs, and the spread must stay within one.` : `Unbalanced: ${c.fd.min}–${c.fd.max} nodes per ${c.fd.scope}. Node counts may differ by at most one.`))), c.caps.rebalancing && /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Volume rebalancing"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Automatic rebalancing", c.autoRebalance.enabled ? "on" : "off — rebalance manually"], ["Volumes moved, last hour", c.autoRebalance.moved1h], ["Volumes moved, last 24 hours", c.autoRebalance.moved24h]]
  }), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginTop: 8,
      marginBottom: 0
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "move",
    s: 12
  }), "A volume moves by instant migration: the primary role is handed to another node without copying data. Every move files an ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "lvol_migration"), " task \u2014 the Tasks tab is where an individual move is followed.")))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Encryption \xB7 KMS"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, c.kms ? /*#__PURE__*/React.createElement(Props, {
    rows: KMS_ROWS(c.kms)
  }) : /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "No KMS configured. Encrypted volumes and buckets cannot be created in this cluster until an external key manager is set up."))), c.objectStorage.enabled && /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Object storage service \xB7 S3"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Endpoint", c.objectStorage.endpoint], ["Region", c.objectStorage.region], ["Addressing", c.objectStorage.addressing], ["Metadata backend", "FoundationDB — the control plane state database"], ["Buckets", c.counts.buckets + " of " + c.objectStorage.max_buckets], ["Default versioning", c.objectStorage.versioning_default ? "on" : "off"]]
  }), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginTop: 10,
      marginBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "This is the S3 service the cluster serves to its tenants. The S3 target the cluster writes its own backups to is configured separately below."))), c.backupEnabled && /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Backup target \xB7 S3"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, c.s3 ? /*#__PURE__*/React.createElement(Props, {
    rows: [["Endpoint", c.s3.endpoint], ["Region", c.s3.region], ["Bucket", c.s3.bucket], ["Path prefix", c.s3.path_prefix], ["Access key id", c.s3.access_key_id], ["Secret", c.s3.secret_access_key], ["Addressing", c.s3.addressing], ["Verify TLS", c.s3.verify_tls ? "yes" : "no"]]
  }) : /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "Backups are enabled but no S3 endpoint is configured \u2014 backups will fail until one is set.")))))));
}
function HostDetail({
  o: h,
  nav
}) {
  const candidate = h.status === "discovered" || h.status === "inspecting" || h.status === "inspected";
  const sockets = h.socketsUsed && h.socketsUsed.length ? h.socketsUsed : Array.from({
    length: h.sockets || 0
  }, (_, s) => s);
  if (candidate) return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: h,
    title: h.hostname,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, "kubernetes worker"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, h.kubelet)),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, h.mgmtIp)
  }), h.status === "discovered" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "host",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Not prepared."), " This worker node is visible to the operator but simplyblock knows nothing about its devices yet. Deploy the inspection pod to collect the NUMA topology, devices and NICs.")), h.status === "inspecting" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Inspection running."), " ", h.inspection ? h.inspection.pod : "The inspection pod", " is collecting the device and NUMA inventory.")), h.status === "inspected" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Inventory collected."), " Choose the NUMA sockets, the memory per storage-plane pod, the devices and the NICs to finish preparing this host.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "vCPU / cores",
    v: h.vcpu
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "System RAM",
    v: fmtBytes(h.memory, 0)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "NUMA sockets",
    v: h.sockets || "unknown"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Devices found",
    v: h.sockets ? h.devices.length : "—"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "NICs found",
    v: h.nics.length || "—"
  })), h.status === "inspected" && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Discovered devices"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Socket"), /*#__PURE__*/React.createElement("th", null, "Kind"), /*#__PURE__*/React.createElement("th", null, "PCIe"), /*#__PURE__*/React.createElement("th", null, "Block device"), /*#__PURE__*/React.createElement("th", null, "Model"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Size"))), /*#__PURE__*/React.createElement("tbody", null, h.devices.map(d => /*#__PURE__*/React.createElement("tr", {
    key: d.id
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.socket), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "badge"
  }, d.kind)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.pcie || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.blockdev || "—"), /*#__PURE__*/React.createElement("td", {
    style: {
      color: "var(--dim)"
    }
  }, d.model), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(d.size)))))))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Discovered NICs"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement(NicTable, {
    nics: h.nics
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Kubernetes node"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Node name", h.hostname], ["Kubelet", h.kubelet], ["Roles", h.roles.join(", ")], ["Zone", h.zoneId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openZone(h.zoneId),
      label: h.zone || regName(h.zoneId, "zone")
    }) : null], ["Region", h.region], ["Rack", h.rack], ["Cabinet", h.cabinet], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: h.status
    })], ["Address", h.mgmtIp], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(h.clusterId),
      label: regName(h.clusterId)
    })]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Node labels"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, Object.entries(h.k8sLabels).map(([k, v]) => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: k
  }, /*#__PURE__*/React.createElement("i", null, k), v)))))));
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: h,
    title: h.hostname,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, h.controlPlane && /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, "control plane services"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, h.counts.nodes ? `${h.counts.nodes} storage node(s)` : "no storage node")),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, h.mgmtIp)
  }), h.status === "unreachable" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Host unreachable."), " The agent has stopped reporting. Storage nodes on this host cannot be restarted until it returns.")), h.counts.nodes === 0 && h.status === "available" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Prepared and labelled."), " ", h.counts.free, " unassigned device(s) \u2014 this host can take a new storage node or receive a migrated one.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "NUMA sockets",
    v: h.socketsUsed && h.socketsUsed.length ? `${h.socketsUsed.length} of ${h.sockets}` : h.sockets,
    s: h.socketsUsed && h.socketsUsed.length ? `socket ${h.socketsUsed.join(", ")} in use` : "all in use"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "vCPU / cores",
    v: h.vcpu
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "System RAM",
    v: fmtBytes(h.memory, 0)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Hugepages",
    v: fmtBytes(h.hugepages.allocated, 0),
    s: `of ${fmtBytes(h.hugepages.reserved, 0)} reserved`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Devices",
    v: `${h.counts.assigned}/${h.counts.devices}`,
    s: "assigned"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Raw capacity",
    v: fmtBytes(h.capacity.total),
    s: `${fmtBytes(h.capacity.used)} claimed`
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Devices by NUMA socket"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, h.counts.free, " unassigned")), "      ", /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Socket"), /*#__PURE__*/React.createElement("th", null, "Kind"), /*#__PURE__*/React.createElement("th", null, "PCIe"), /*#__PURE__*/React.createElement("th", null, "Block device"), /*#__PURE__*/React.createElement("th", null, "Model"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Size"), /*#__PURE__*/React.createElement("th", null, "Assignment"))), /*#__PURE__*/React.createElement("tbody", null, sockets.map(s => h.devices.filter(d => d.socket === s).map(d => /*#__PURE__*/React.createElement("tr", {
    key: d.id
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.socket), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "badge"
  }, d.kind)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.pcie || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.blockdev || "—"), /*#__PURE__*/React.createElement("td", {
    style: {
      color: "var(--dim)"
    }
  }, d.model), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(d.size)), /*#__PURE__*/React.createElement("td", null, d.assignedNodeId ? /*#__PURE__*/React.createElement(Ref, {
    onClick: () => nav.openNode(h.clusterId, d.assignedNodeId),
    label: regName(d.assignedNodeId, "storage node")
  }) : d.reserved ? /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--warn)"
    }
  }, "reserved \xB7 awaiting node restart") : /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "unassigned"))))), h.devices.filter(d => d.kind === "block" && !d.assignedNodeId).length === 0 && null)))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Network interfaces"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), h.mgmtNic && /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, "mgmt ", h.mgmtNic, " \xB7 data ", (h.dataNics || []).join(", ") || "—")), /*#__PURE__*/React.createElement(NicTable, {
    nics: h.nics,
    mgmt: h.mgmtNic,
    data: h.dataNics
  }), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Host properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Hostname", h.hostname], ["Management IP", h.mgmtIp], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: h.status
    })], ["Zone", h.zoneId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openZone(h.zoneId),
      label: h.zone || regName(h.zoneId, "zone")
    }) : null], ["Region", h.region], ["Rack", h.rack], ["Cabinet", h.cabinet], ["Device class", h.hostClass], ["Control plane services", h.controlPlane ? "yes" : "no"], ["NUMA sockets in use", h.socketsUsed ? h.socketsUsed.join(", ") : "all"], ["Memory per storage-plane pod", h.memoryPerPod ? fmtBytes(h.memoryPerPod, 0) : null], ["Management NIC", h.mgmtNic], ["Data NICs", (h.dataNics || []).join(", ") || null], ["Storage nodes", h.nodeIds.length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        gap: 8,
        justifyContent: "flex-end",
        flexWrap: "wrap"
      }
    }, h.nodeIds.map(id => /*#__PURE__*/React.createElement(Ref, {
      key: id,
      onClick: () => nav.openNode(h.clusterId, id),
      label: regName(id, shortId(id))
    }))) : null], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(h.clusterId),
      label: regName(h.clusterId)
    })], ["Prepared", fmtDate(h.preparedAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Labels"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, Object.entries(h.labels).map(([k, v]) => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: k
  }, /*#__PURE__*/React.createElement("i", null, k), v)))))));
}

// The devices attached to one node, with the per-device actions in place —
// restart, health check, fail (failure migration) and remove — so a drive can
// be dealt with from the node it hangs off, not only from the devices layer.
function NodeDeviceTable({
  node,
  nav
}) {
  const [rev, setRev] = useState(0);
  const {
    data,
    loading,
    error,
    reload
  } = useResource("ndev|" + node.id + "|" + rev, () => api.devices(node.id), 6000);
  const ds = data || [];
  const nvme = ((REG[node.clusterId] || {}).mode || (ds[0] || {}).mode) === "nvme";
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Devices on this node"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, ds.filter(x => x.status === "online").length, " of ", ds.length, " online")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, error ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, error.message) : loading && !ds.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "loading\u2026") : !ds.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "This node has no devices attached.") : /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Status"), /*#__PURE__*/React.createElement("th", null, nvme ? "Serial" : "Block device"), /*#__PURE__*/React.createElement("th", null, nvme ? "PCIe" : "Serial"), /*#__PURE__*/React.createElement("th", null, "Block device"), /*#__PURE__*/React.createElement("th", null, "Model"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Capacity"), /*#__PURE__*/React.createElement("th", null, "Health"), /*#__PURE__*/React.createElement("th", {
    style: {
      width: 34
    }
  }))), /*#__PURE__*/React.createElement("tbody", null, ds.map(x => /*#__PURE__*/React.createElement("tr", {
    key: x.id
  }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
    status: x.status,
    sm: true,
    label: true
  })), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("button", {
    className: "tlink",
    onClick: () => nav.detail(x)
  }, nvme ? x.serial : x.blockdev || x.serial)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, (nvme ? x.pcie : x.serial) || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, x.blockdev || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, x.model || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(x.capacity.used), " / ", fmtBytes(x.capacity.total)), /*#__PURE__*/React.createElement("td", null, (x.status === "online" || x.status === "read_only") && x.health ? /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement(Dot, {
    c: STATUS_META[x.health].c
  }), STATUS_META[x.health].label) : /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "\u2014")), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(ActionBtn, {
    obj: x
  })))))))));
}
function NodeDetail({
  o: n,
  nav
}) {
  const [tab, setTab] = useState("overview");
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: n,
    title: n.hostname,
    badge: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "storage node"),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, n.ip, ":", n.port)
  }), /*#__PURE__*/React.createElement(Tabs, {
    active: tab,
    onChange: setTab,
    items: [{
      k: "overview",
      label: "Overview",
      icon: "node"
    }, {
      k: "threads",
      label: "SPDK threads",
      icon: "gauge"
    }, {
      k: "logs",
      label: "SPDK logs",
      icon: "filter"
    }]
  }), tab === "threads" && /*#__PURE__*/React.createElement(SpdkThreadsPanel, {
    node: n
  }), tab === "logs" && /*#__PURE__*/React.createElement(NodeLogPanel, {
    node: n
  }), tab === "overview" && /*#__PURE__*/React.createElement(React.Fragment, null, n.status === "unreachable" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Node unreachable."), " No heartbeat received. Volumes served by this node have failed over to their secondaries.")), n.op && /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginBottom: 12
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(NodeOpBox, {
    op: n.op,
    wide: true
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "8px 0 0"
    }
  }, OP_HINT[n.op.kind], " Started ", fmtAgo(n.op.startedAt), "; it finishes on its own \u2014 follow it under the cluster's Tasks tab."))), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Capacity used",
    v: pct(n.capacity.used, n.capacity.total).toFixed(0) + "%",
    s: `${fmtBytes(n.capacity.used)} of ${fmtBytes(n.capacity.total)}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Devices",
    v: n.counts.devices,
    s: `${n.counts.devicesOnline} online`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "vCPU reserved",
    v: n.cpuReserved,
    s: `of ${n.cpuCount} cores on host`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "System RAM",
    v: fmtBytes(n.memory.used, 0),
    s: `used of ${fmtBytes(n.memory.total, 0)}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "RAM reserved",
    v: fmtBytes(n.memory.reserved, 0),
    s: "requests/limits"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Hugepages",
    v: fmtBytes(n.hugepages.used, 0),
    s: `of ${fmtBytes(n.hugepages.total, 0)} allocated`
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Resource reservation"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(AllocBar, {
    label: "System memory in use",
    used: n.memory.used,
    total: n.memory.total,
    color: "var(--ok)"
  }), /*#__PURE__*/React.createElement(AllocBar, {
    label: "Reserved for this node",
    used: n.memory.reserved,
    total: n.memory.total,
    color: "var(--accent)"
  }), /*#__PURE__*/React.createElement(AllocBar, {
    label: "Hugepages in use",
    used: n.hugepages.used,
    total: n.hugepages.total,
    color: "var(--ro)"
  }), /*#__PURE__*/React.createElement("div", {
    className: "kv",
    style: {
      marginTop: 10
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "vCPU reserved"), /*#__PURE__*/React.createElement("b", null, n.cpuReserved)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Cores on host"), /*#__PURE__*/React.createElement("b", null, n.cpuCount)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "SPDK"), /*#__PURE__*/React.createElement("b", null, n.spdk))))), /*#__PURE__*/React.createElement(IOCards, {
    o: n
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "device",
    title: "Devices",
    sub: "attached storage media",
    count: n.counts.devices,
    onClick: () => nav.layer(n, "devices")
  }), n.hostId && /*#__PURE__*/React.createElement(NavCard, {
    icon: "host",
    title: "Host",
    sub: "machine running this node",
    count: "\u2192",
    onClick: () => nav.openHost(n.clusterId, n.hostId)
  })), /*#__PURE__*/React.createElement(NodeDeviceTable, {
    node: n,
    nav: nav
  }), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Node properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Hostname", n.hostname], ["Management IP", n.mgmtIp], ["Data paths", (n.dataNics || []).length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        flexDirection: "column",
        gap: 2,
        alignItems: "flex-end"
      }
    }, n.dataNics.map(x => /*#__PURE__*/React.createElement("span", {
      key: x.name + x.ip
    }, x.name, " \xB7 ", x.ip, ":", x.port))) : n.ip], ["Multipathing", n.multipath ? "yes — two paths" : "no — single path"], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: n.status
    })], ["Failure domain", n.failureDomain], ["Physical label", n.physicalLabel], ["Host", n.hostId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openHost(n.clusterId, n.hostId),
      label: regName(n.hostId, "host")
    }) : null], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(n.clusterId),
      label: regName(n.clusterId)
    })], ["vCPU reserved", n.cpuReserved], ["Max subsystems", n.maxSubsystems], ["CPU cores", n.cpuCount], ["System memory", fmtBytes(n.memory.total, 0)], ["Memory reserved", fmtBytes(n.memory.reserved, 0)], ["Memory in use", fmtBytes(n.memory.used, 0)], ["Hugepages", `${fmtBytes(n.hugepages.used, 0)} / ${fmtBytes(n.hugepages.total, 0)}`], ["SPDK", n.spdk]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "I/O totals"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "IOPS read",
    v: fmtNum(n.iops.r)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "IOPS write",
    v: fmtNum(n.iops.w)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Read",
    v: fmtBW(n.bw.r),
    s: "GB/s"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Write",
    v: fmtBW(n.bw.w),
    s: "GB/s"
  })))))));
}
function DeviceDetail({
  o: d,
  nav
}) {
  const showHealth = (d.status === "online" || d.status === "read_only") && d.health;
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: d,
    title: d.mode === "nvme" ? d.serial : d.blockdev,
    badge: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, d.model),
    sub: showHealth ? /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)",
        display: "flex",
        alignItems: "center",
        gap: 6
      }
    }, "health", /*#__PURE__*/React.createElement(TrafficLight, {
      status: d.health
    })) : null
  }), showHealth && d.health === "critical" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Media health critical."), " Elevated error counters reported. Schedule replacement and let the cluster rebalance.")), d.status === "removed" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "power",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Out of service \u2014 this is reversible."), " The cluster still knows this device and it can be added back at any time; its chunks resynchronise from the surviving copies. Redundancy stays reduced until it returns. If the drive is not coming back, fail it so its chunks are rebuilt and fault tolerance is restored without it.")), d.status === "failed" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--bad)",
      background: "color-mix(in srgb,var(--bad) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--bad) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Permanently failed."), " This device is excluded from the cluster and every chunk that lived on it has been rebuilt onto the remaining devices, so fault tolerance is restored without it. It cannot be added back \u2014 replace the drive and add the new one to the node.")), (d.status === "in_removal" || d.status === "in_failure") && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, d.status === "in_removal" ? "Removal in progress." : "Failure migration in progress."), " ", d.status === "in_removal" ? "The device is being detached from the distribution layer." : "Chunks are being rebuilt onto the remaining devices.", " Follow it under the cluster's Operations tab.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Capacity used",
    v: pct(d.capacity.used, d.capacity.total).toFixed(0) + "%",
    s: `${fmtBytes(d.capacity.used)} of ${fmtBytes(d.capacity.total)}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "IOPS read",
    v: fmtNum(d.iops.r)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "IOPS write",
    v: fmtNum(d.iops.w)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Temperature",
    v: d.temp + "°C",
    c: d.temp > 60 ? "var(--warn)" : undefined
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Wear level",
    v: d.wear + "%",
    c: d.wear > 25 ? "var(--warn)" : undefined
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Power-on",
    v: (d.poweronHours / 1000).toFixed(1) + "k",
    s: "hours"
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Device properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Serial number", d.serial], ["PCIe address", d.pcie], ["Block device", d.blockdev], ["NUMA socket", d.socket], ["Model", d.model], ["Firmware", d.firmware], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: d.status
    })], showHealth ? ["Health", /*#__PURE__*/React.createElement(TrafficLight, {
      status: d.health
    })] : null, d.lastHealthCheck ? ["Last health check", fmtDate(d.lastHealthCheck)] : null, ["Storage node", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openNode(d.clusterId, d.nodeId),
      label: regName(d.nodeId)
    })], d.hostId ? ["Host", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openHost(d.clusterId, d.hostId),
      label: regName(d.hostId, "host")
    })] : null, ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(d.clusterId),
      label: regName(d.clusterId)
    })], ["Raw capacity", fmtBytes(d.capacity.total)]]
  }))), /*#__PURE__*/React.createElement(IOCards, {
    o: d
  })), /*#__PURE__*/React.createElement("div", {
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement(SmartCard, {
    device: d
  })));
}
Object.assign(window, {
  NodeDeviceTable,
  Props,
  Stat,
  Ref,
  NavCard,
  NicTable,
  Chart,
  IOCards,
  DetailHead,
  QosProps,
  regName,
  ClusterDetail,
  HostDetail,
  NodeDetail,
  DeviceDetail
});
})();
// ---- dr.jsx ----
(function(){
// ---------------------------------------------------------------------------
// CROSS-CLUSTER CONFIGURATION — cluster pairs, DR replication policies, zones.
// Pairing is directional: a→b and b→a are two pairs, so bidirectional and
// fan-out topologies (a→b, b→a, a→c, b→c) are just several pairs.
// ---------------------------------------------------------------------------
const fmtMin = m => !m ? "—" : m < 60 ? `${m} min` : m % 60 === 0 ? `${m / 60} h` : `${Math.floor(m / 60)} h ${m % 60} min`;
const clockOf = s => {
  const d = new Date(s);
  return isNaN(d) ? "—" : d.toISOString().slice(11, 19);
};
const MODE_BADGE = m => m === "synchronous" ? /*#__PURE__*/React.createElement("span", {
  className: "badge sync"
}, "synchronous") : /*#__PURE__*/React.createElement("span", {
  className: "badge k8s"
}, "asynchronous");
const ScheduleTable = ({
  rows,
  compact
}) => !rows || !rows.length ? /*#__PURE__*/React.createElement("div", {
  className: "nolim"
}, "No older generations retained \u2014 only the latest snapshot is kept.") : /*#__PURE__*/React.createElement("div", {
  className: "rettable two"
}, /*#__PURE__*/React.createElement("div", {
  className: "rethead"
}, /*#__PURE__*/React.createElement("span", null, "Every"), /*#__PURE__*/React.createElement("span", null, "Generations kept")), rows.map((r, i) => /*#__PURE__*/React.createElement("div", {
  className: "retrow",
  key: i
}, /*#__PURE__*/React.createElement("span", null, r.interval), /*#__PURE__*/React.createElement("b", null, r.keep, "\xD7"))), !compact && /*#__PURE__*/React.createElement("div", {
  className: "retrow",
  style: {
    background: "var(--panel2)"
  }
}, /*#__PURE__*/React.createElement("span", null, "total"), /*#__PURE__*/React.createElement("b", null, rows.reduce((a, r) => a + r.keep, 0), "\xD7")));

// ---- tiles -----------------------------------------------------------------
function ZoneTile({
  s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": "var(--ok)"
    },
    onDoubleClick: () => nav.detail(s)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: s,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: "active"
    }), /*#__PURE__*/React.createElement(Name, null, s.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, s.region)
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, s.location), /*#__PURE__*/React.createElement(Uuid, {
    value: s.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab mono",
    title: "topology.kubernetes.io/zone"
  }, /*#__PURE__*/React.createElement("i", null, "zone"), s.name), /*#__PURE__*/React.createElement("span", {
    className: "lab mono",
    title: "topology.kubernetes.io/region"
  }, /*#__PURE__*/React.createElement("i", null, "region"), s.region), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "racks"), s.racks.length || "—")), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Hosts"), /*#__PURE__*/React.createElement("b", null, s.counts.hosts)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Prepared"), /*#__PURE__*/React.createElement("b", null, s.counts.hostsPrepared)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "NVMe hosts"), /*#__PURE__*/React.createElement("b", null, s.counts.nvme)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Storage nodes"), /*#__PURE__*/React.createElement("b", null, s.counts.nodes))), /*#__PURE__*/React.createElement("div", {
    className: "zonestrip"
  }, (s.k8sClusters || []).map(kc => /*#__PURE__*/React.createElement("button", {
    className: "zonechip",
    key: kc.uuid,
    onClick: e => {
      e.stopPropagation();
      nav.openK8s(kc.uuid);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 10
  }), kc.name))), s.capacity.total > 0 && /*#__PURE__*/React.createElement("div", {
    className: "lab wide"
  }, /*#__PURE__*/React.createElement("i", null, "raw capacity"), fmtBytes(s.capacity.total)), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Hosts",
      count: s.counts.hosts,
      icon: "host",
      onClick: () => nav.layer(s, "hosts")
    }, {
      label: "Clusters",
      count: s.counts.clusters,
      icon: "cluster",
      onClick: () => nav.layer(s, "clusters")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(s)
    }]
  }));
}

// ---- details ---------------------------------------------------------------
function ZoneDetail({
  o: s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: s,
    title: s.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "zone"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, s.region)),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, s.location)
  }), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Hosts",
    v: s.counts.hosts,
    s: `${s.counts.hostsPrepared} prepared`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "NVMe hosts",
    v: s.counts.nvme,
    s: `${s.counts.hosts - s.counts.nvme} non-NVMe`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Storage nodes",
    v: s.counts.nodes
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Clusters present",
    v: s.counts.clusters
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Racks",
    v: s.racks.length,
    s: s.untaintedHosts ? s.untaintedHosts + " host(s) untainted" : s.racks.join(", ") || "—"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Raw capacity",
    v: fmtBytes(s.capacity.total)
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "host",
    title: "Hosts",
    sub: "racked in this zone",
    count: s.counts.hosts,
    onClick: () => nav.layer(s, "hosts")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "cluster",
    title: "Clusters",
    sub: "present in this zone",
    count: s.counts.clusters,
    onClick: () => nav.layer(s, "clusters")
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Zone properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Zone label", s.label], ["Region label", s.regionLabel], ["Location", s.location], ["Racks", s.racks.join(", ") || null], ["Kubernetes clusters", (s.k8sClusters || []).length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        gap: 8,
        justifyContent: "flex-end",
        flexWrap: "wrap"
      }
    }, s.k8sClusters.map(kc => /*#__PURE__*/React.createElement(Ref, {
      key: kc.uuid,
      onClick: () => nav.openK8s(kc.uuid),
      label: kc.name
    }))) : null], ["Hosts", s.counts.hosts], ["Storage nodes", s.counts.nodes], ["Clusters", s.clusterIds.length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        gap: 8,
        justifyContent: "flex-end",
        flexWrap: "wrap"
      }
    }, s.clusterIds.map(id => /*#__PURE__*/React.createElement(Ref, {
      key: id,
      onClick: () => nav.openCluster(id),
      label: regName(id)
    }))) : null], ["Created", fmtDate(s.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Zones, topology and replication"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "A zone is the standard Kubernetes failure domain, read from the ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "topology.kubernetes.io/zone"), " node label and grouped under ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "topology.kubernetes.io/region"), " \u2014 not a bespoke object. Worker nodes may additionally be tainted with a rack and cabinet; untainted hosts show no rack."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Storage classes map these labels onto storage clusters through ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "zone_cluster_map"), " and ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "region_cluster_map"), ", so a pod scheduled here is provisioned from storage in the same zone."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Zones are assigned to a storage cluster ", /*#__PURE__*/React.createElement("b", null, "at creation time only"), " and cannot be changed afterwards. Storage nodes can only ever be started on hosts inside those zones."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Two or more zones is the precondition for ", /*#__PURE__*/React.createElement("b", null, "synchronous"), " replication. Asynchronous replication does not use zones \u2014 it runs between two clusters over a cluster pair.")))));
}

// ---- DR landing ------------------------------------------------------------
function DrHome({
  nav
}) {
  const pairs = useResource("dr.pairs", () => api.pairs(), 8000);
  const pols = useResource("dr.pols", () => api.allRPolicies(), 6000);
  const zones = useResource("dr.zones", () => api.zones(), 20000);
  const clusters = useResource("dr.clusters", () => api.clusters(), 20000);
  const P = pols.data || [],
    PR = pairs.data || [],
    S = zones.data || [],
    C = clusters.data || [];
  const unhealthy = P.filter(p => p.status !== "healthy");
  const backlog = P.reduce((a, p) => a + (p.backlog || 0), 0);
  const attached = P.reduce((a, p) => a + p.counts.volumes, 0);
  const eligible = C.filter(c => c.drEligible).length;
  return /*#__PURE__*/React.createElement("div", {
    className: "scroll",
    style: {
      paddingTop: 14
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "dhead",
    style: {
      paddingTop: 0
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      minWidth: 0,
      flex: 1
    }
  }, /*#__PURE__*/React.createElement("h1", null, "Cross-cluster configuration"), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 12.5,
      color: "var(--dim)",
      marginTop: 4,
      maxWidth: 720
    }
  }, "Pair clusters, then attach replication and failback policies to those pairs. Synchronous replication is configured inside a single cluster that is stretched across zones.")), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    onClick: () => window.__ui.dialog(newPairDialog(), {
      kind: "cluster pair",
      id: "new"
    })
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Pair clusters")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Cluster pairs",
    v: PR.length,
    s: `${PR.filter(p => p.status === "paired").length} healthy links`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Policies",
    v: P.length,
    s: `${P.filter(p => p.mode === "synchronous").length} synchronous`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Volumes",
    v: attached,
    s: "protected by a policy"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Total backlog",
    v: fmtBytes(backlog),
    c: unhealthy.length ? "var(--bad)" : undefined
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Unhealthy",
    v: unhealthy.length,
    c: unhealthy.length ? "var(--bad)" : "var(--ok)"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "DR-eligible",
    v: `${eligible}/${C.length}`,
    s: "qualified as targets"
  })), unhealthy.length > 0 && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Needs attention"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Policy"), /*#__PURE__*/React.createElement("th", null, "Mode"), /*#__PURE__*/React.createElement("th", null, "Route"), /*#__PURE__*/React.createElement("th", null, "Last replication"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Backlog"), /*#__PURE__*/React.createElement("th", null, "State"))), /*#__PURE__*/React.createElement("tbody", null, unhealthy.map(p => /*#__PURE__*/React.createElement("tr", {
    key: p.id,
    style: {
      cursor: "pointer"
    },
    onClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600
    }
  }, p.name), /*#__PURE__*/React.createElement("td", null, MODE_BADGE(p.mode)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, regName(p.sourceClusterId), p.mode === "synchronous" ? "" : ` → ${regName(p.targetClusterId)}`), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, clockOf(p.lastAt), " ", /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "\xB7 ", fmtAgo(p.lastAt))), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right",
      color: "var(--bad)"
    }
  }, fmtBytes(p.backlog)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
    status: p.status
  }))))))))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Configure"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "swap",
    title: "Cluster pairs",
    sub: "links between clusters",
    count: PR.length,
    onClick: () => nav.drLayer("pairs")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "shield",
    title: "DR policies",
    sub: "a pair and a consistency group, or a stretched cluster \u2014 volumes and applications",
    count: P.length,
    onClick: () => nav.drLayer("rpolicies")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "zone",
    title: "Zones",
    sub: "hosts, racks, stretched clusters",
    count: S.length,
    onClick: () => nav.drLayer("zones")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "move",
    title: "Migration paths",
    sub: "online site-to-site migration of VMs, containers and their volumes",
    count: "\u2192",
    onClick: () => nav.drLayer("mpaths")
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Application DR \xB7 Ramen"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, "the workload, not just the blocks")), /*#__PURE__*/React.createElement(AppDrSummary, {
    nav: nav
  }));
}

// Replication pairs and policies live in repl.jsx now, on the real CRDs. What
// remains here is the zone/topology layer and the shared schedule helpers.
Object.assign(window, {
  fmtMin,
  clockOf,
  MODE_BADGE,
  ScheduleTable,
  ZoneTile,
  ZoneDetail
});
})();
// ---- dr-plan.jsx ----
(function(){
// ---------------------------------------------------------------------------
// DR TOP LAYER — protection plans and sites
//
// The plan is the only thing authored: sites, a storage profile, and the
// methods it declares. DRCluster, DRPolicy, DRPlacementControl and the
// replication class matrix are all derived from it, and are shown read-only so
// an operator can see what the orchestrator produced without being invited to
// edit objects whose fields are immutable.
// ---------------------------------------------------------------------------

// The one table that puts all three methods side by side. Everything else in
// the control path is identical between them.
const METHOD_META = {
  sync: {
    label: "synchronous",
    short: "sync",
    c: "var(--ok)",
    rpo: "0",
    rto: "seconds",
    genSelect: false,
    failback: true,
    vrMode: "sync",
    target: "Peer cluster storage, inline write mirror",
    how: "Every write is mirrored to the peer before it is acknowledged. Both sites must be in one region."
  },
  async: {
    label: "asynchronous",
    short: "async",
    c: "var(--info)",
    rpo: "= interval",
    rto: "seconds",
    genSelect: false,
    failback: true,
    vrMode: "async",
    target: "Peer cluster storage, block delta per epoch",
    how: "A block delta is shipped to the peer once per interval. The peer holds a whole volume, one epoch behind."
  },
  "snapshot-s3": {
    label: "generation vault",
    short: "vault",
    c: "var(--ro)",
    rpo: "= interval",
    rto: "minutes — materialisation from object store",
    genSelect: true,
    failback: false,
    vrMode: "snapshot-s3",
    target: "S3 bucket, immutable snapshot objects + manifest",
    how: "Each interval uploads an immutable generation to an object store. A restore materialises a volume set from a chosen generation."
  }
};
const mmeta = t => METHOD_META[t] || METHOD_META.async;
const MethodBadge = ({
  type,
  sm
}) => {
  const m = mmeta(type);
  return /*#__PURE__*/React.createElement("span", {
    className: "badge",
    style: {
      color: m.c,
      borderColor: `color-mix(in srgb,${m.c} 45%,transparent)`
    }
  }, sm ? m.short : m.label);
};
const fmtLag = s => s == null ? "—" : s === 0 ? "0s" : s < 60 ? `${s}s` : s < 3600 ? `${Math.floor(s / 60)}m ${s % 60}s` : `${Math.floor(s / 3600)}h ${Math.floor(s % 3600 / 60)}m`;

// ---- tiles -----------------------------------------------------------------
function PlanTile({
  p,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[p.status].c
    },
    onDoubleClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: p,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    }), /*#__PURE__*/React.createElement(Name, null, p.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "plan")
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, p.counts.methods, " method", p.counts.methods === 1 ? "" : "s", " \xB7 ", p.counts.sites, " sites"), /*#__PURE__*/React.createElement(Uuid, {
    value: p.id
  }), p.protectionGap && /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: "var(--bad)"
    }
  }, "Interval mismatch \u2014 no replication class resolves, so a declared method is protecting nothing"), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "profile"), p.storageProfile), p.siteNames.map(s => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: s
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 10
  }), s))), /*#__PURE__*/React.createElement("div", {
    className: "mlist"
  }, p.methods.map(m => /*#__PURE__*/React.createElement("div", {
    className: "mrow",
    key: m.name
  }, /*#__PURE__*/React.createElement(MethodBadge, {
    type: m.type,
    sm: true
  }), /*#__PURE__*/React.createElement("b", null, m.name), /*#__PURE__*/React.createElement("span", {
    className: "ar"
  }, "\u2192"), /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, m.target), /*#__PURE__*/React.createElement("span", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "mono rpo"
  }, m.type === "sync" ? "RPO 0" : "RPO " + m.interval), !m.intervalConsistent && /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 11,
    c: "var(--bad)"
  }))), !p.methods.length && /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "No method declared \u2014 this plan protects nothing yet.")), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Apps",
      count: p.counts.apps,
      icon: "cluster",
      onClick: () => nav.layer(p, "protectedapps")
    }, {
      label: "Sites",
      count: p.counts.sites,
      icon: "k8s",
      onClick: () => nav.layer(p, "sites")
    }, {
      label: "Gens",
      count: p.counts.generations,
      icon: "camera"
    }],
    onDetail: () => nav.detail(p)
  }));
}
function SiteTile({
  s,
  nav
}) {
  const fenced = s.fencing !== "Unfenced";
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[s.status] ? STATUS_META[s.status].c : "var(--ok)"
    },
    onDoubleClick: () => nav.detail(s)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: s,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: s.status
    }), /*#__PURE__*/React.createElement(Name, null, s.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "site")
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, s.region), /*#__PURE__*/React.createElement(Uuid, {
    value: s.id
  }), fenced && /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: "var(--bad)"
    }
  }, s.fencing, " \u2014 no I/O is accepted from this site"), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openK8s(s.k8sClusterId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 10
  }), "managed cluster"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "s3"), s.s3Profile), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "ramen"), s.ramen)), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Active here"), /*#__PURE__*/React.createElement("b", null, s.counts.activeApps)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Standby for"), /*#__PURE__*/React.createElement("b", null, s.counts.standbyApps))), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Apps",
      count: s.counts.activeApps + s.counts.standbyApps,
      icon: "cluster",
      onClick: () => nav.layer(s, "protectedapps")
    }, {
      label: "Plans",
      count: s.counts.plans,
      icon: "shield",
      onClick: () => nav.layer(s, "plans")
    }, {
      label: "Classes",
      count: s.counts.classes,
      icon: "link"
    }],
    onDetail: () => nav.detail(s)
  }));
}

// ---- the method matrix on the plan detail ----------------------------------
function MethodMatrix({
  p,
  nav
}) {
  const act = async (fn, msg) => {
    try {
      await fn();
      window.__toast(msg);
    } catch (e) {
      window.__toast(e.message);
    }
  };
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Method"), /*#__PURE__*/React.createElement("th", null, "Type"), /*#__PURE__*/React.createElement("th", null, "Target"), /*#__PURE__*/React.createElement("th", null, "RPO"), /*#__PURE__*/React.createElement("th", null, "RTO"), /*#__PURE__*/React.createElement("th", null, "Generations"), /*#__PURE__*/React.createElement("th", null, "Failback"), /*#__PURE__*/React.createElement("th", null, "Class"), /*#__PURE__*/React.createElement("th", {
    style: {
      width: 34
    }
  }))), /*#__PURE__*/React.createElement("tbody", null, p.methods.map(m => {
    const mm = mmeta(m.type);
    return /*#__PURE__*/React.createElement("tr", {
      key: m.name,
      className: m.intervalConsistent ? "" : "bad"
    }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("b", null, m.name)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(MethodBadge, {
      type: m.type,
      sm: true
    })), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, m.target), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, m.type === "sync" ? "0" : m.interval), /*#__PURE__*/React.createElement("td", null, mm.rto), /*#__PURE__*/React.createElement("td", null, mm.genSelect ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ro)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "camera",
      s: 10
    }), "selectable") : /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)"
      }
    }, "\u2014")), /*#__PURE__*/React.createElement("td", null, mm.failback ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ok)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "check",
      s: 10
    }), "yes") : /*#__PURE__*/React.createElement("span", {
      className: "lab",
      title: "the source is gone or untrusted after a vault restore, and the vault holds generations rather than a live peer",
      style: {
        color: "var(--dim2)"
      }
    }, "no")), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, m.intervalConsistent ? m.cls ? m.cls.name : "—" : /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--bad)"
      }
    }, "unresolved")), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(ActionBtn, {
      obj: Object.assign({
        kind: "method",
        id: p.id + "/" + m.name,
        planId: p.id,
        planName: p.name
      }, m)
    })));
  }))), !p.methods.length && /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "No method declared. A plan without methods derives no policy and protects nothing.")));
}

// ---- the derivation view ---------------------------------------------------
// Ramen object fields are immutable, so every pair a method could ever use is
// created at onboarding. This is what the orchestrator produced; none of it is
// editable here on purpose.
function DerivationCard({
  p,
  nav
}) {
  const [open, setOpen] = useState("policies");
  const bad = p.policies.filter(x => !x.validated);
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Derived Ramen objects"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, p.counts.sites, " DRCluster \xB7 ", p.counts.policies, " DRPolicy \xB7 ", p.counts.apps, " DRPC \xB7 ", p.methods.length, " class")), !!bad.length && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, bad.length, " derived policy has no peerClass."), " The policy validates cleanly and reports no error, but no replication class resolves for it \u2014 so any application bound to that method is protected by nothing. Fix the method's interval to emit it to both places at once.")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "ptools"
  }, /*#__PURE__*/React.createElement("div", {
    className: "seg"
  }, [["policies", "DRPolicy"], ["classes", "Replication classes"], ["cardinality", "Cardinality"]].map(([k, l]) => /*#__PURE__*/React.createElement("button", {
    key: k,
    className: open === k ? "on" : "",
    onClick: () => setOpen(k)
  }, l))), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "live"
  }, /*#__PURE__*/React.createElement("i", null), "read-only \u2014 every field is immutable")), open === "policies" && /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Name"), /*#__PURE__*/React.createElement("th", null, "DR clusters"), /*#__PURE__*/React.createElement("th", null, "Interval"), /*#__PURE__*/React.createElement("th", null, "Selector"), /*#__PURE__*/React.createElement("th", null, "Method"), /*#__PURE__*/React.createElement("th", null, "peerClass"))), /*#__PURE__*/React.createElement("tbody", null, p.policies.map(x => /*#__PURE__*/React.createElement("tr", {
    key: x.name,
    className: x.validated ? "" : "bad"
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, x.name), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, x.drClusters.join(" → ")), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, x.schedulingInterval || /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "unset (sync)")), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, Object.entries(x.selector).map(([k, v]) => k + "=" + v).join(", ")), /*#__PURE__*/React.createElement("td", null, x.methodName || /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "unused pair")), /*#__PURE__*/React.createElement("td", null, x.peerClass ? /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ok)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 10
  }), x.peerClass.replicationId) : x.methodName ? /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--bad)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 10
  }), "absent \u2014 no protection") : /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "\u2014")))))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "0 12px",
      padding: "9px 0 12px"
    }
  }, "An absent peerClass is the only reliable signal that a method is not protecting anything. Ramen computes one per pair by intersecting the StorageClass storageID and the replication class replicationID across both clusters; if either fails to match, the policy still reports healthy.")), open === "classes" && /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Class"), /*#__PURE__*/React.createElement("th", null, "Kind"), /*#__PURE__*/React.createElement("th", null, "replicationID"), /*#__PURE__*/React.createElement("th", null, "Labels"), /*#__PURE__*/React.createElement("th", null, "Parameters"))), /*#__PURE__*/React.createElement("tbody", null, p.classes.map(c => /*#__PURE__*/React.createElement("tr", {
    key: c.name
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, c.name), /*#__PURE__*/React.createElement("td", {
    style: {
      fontSize: 11
    }
  }, c.crdKind), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, c.replicationId), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontSize: 10.5
    }
  }, Object.entries(c.labels).map(([k, v]) => k + "=" + v).join(" ")), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontSize: 10.5
    }
  }, Object.entries(c.parameters).map(([k, v]) => k + "=" + v).join(" ")))))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "0 12px",
      padding: "9px 0 12px"
    }
  }, "The parameters map is the only channel that reaches the driver: mode, interval, bucket, object lock and retention all arrive here. Ramen itself ignores everything except the interval and the selector labels. A single shared replicationID across the async family pre-validates every cluster pair at once, because Ramen matches replicationID pairwise; the synchronous class carries a second, pair-scoped identifier.")), open === "cardinality" && /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "cardin"
  }, [["DRCluster", p.counts.sites, "one per site, created once at onboarding", "region, s3ProfileName, cidrs, clusterFence"], ["DRPolicy", p.counts.policies, "one per pair × interval — all fields immutable, so every pair a method could ever use is created up front", "drClusters[2], schedulingInterval, replicationClassSelector"], ["DRPlacementControl", p.counts.apps, "one per application — the only object rewritten during operation; a rebind is a delete plus a create", "drPolicyRef, placementRef, pvcSelector, preferredCluster, action"], ["VRClass / VGRClass", p.methods.length, "one per method × interval — not a Ramen object, but the switch Ramen reads", "parameters (mode, schedulingInterval, retention, bucket)"]].map(([k, n, why, fields]) => /*#__PURE__*/React.createElement("div", {
    className: "cardrow",
    key: k
  }, /*#__PURE__*/React.createElement("div", {
    className: "cn"
  }, /*#__PURE__*/React.createElement("b", null, k), /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "\xD7", n)), /*#__PURE__*/React.createElement("div", {
    className: "cw"
  }, why), /*#__PURE__*/React.createElement("div", {
    className: "cf mono"
  }, fields)))))));
}

// ---- the bypass channel ----------------------------------------------------
// Everything expressible in a Ramen resource goes through Ramen. These six
// payloads have no field anywhere in its API, and two of them are reads the
// console cannot be truthful without.
const BYPASS = [{
  n: 1,
  name: "Peer and topology binding",
  dir: "down",
  why: "VR, VGR and the gRPC calls carry no peer identity at all — Ramen assumes the driver already knows its topology.",
  params: "sourceSite, targetSite | bucket, transport, credentialsRef, replicationID"
}, {
  n: 2,
  name: "Generation selection",
  dir: "down",
  why: "DRPC failover has no point-in-time parameter; PromoteVolume means promote to current.",
  params: "recoveryPointRef | timestamp, consistencyGroup, storageClassMapping"
}, {
  n: 3,
  name: "Retention and lock enforcement",
  dir: "both",
  why: "Class parameters carry the intent, but enforcement state is object-store state with no CR.",
  params: "lockedUntil per object, complianceState, pendingExpiry, actual count vs policy"
}, {
  n: 4,
  name: "Generation catalogue",
  dir: "up",
  console: true,
  why: "No Kubernetes object represents a generation. VRG status describes the current relationship only.",
  params: "generation, timestamp, sizeBytes, integrityState, lockedUntil"
}, {
  n: 5,
  name: "Per-leg lag",
  dir: "up",
  console: true,
  why: "Only the orchestrated method has a VRG, so only its RPO appears in DRPC.status.lastGroupSyncTime.",
  params: "legID, lastSyncTime, lag, epoch, health — for every declared method"
}, {
  n: 6,
  name: "Arbitration token",
  dir: "both",
  why: "clusterFence and NetworkFence are pair-scoped; three sites need a quorum decision Ramen does not model.",
  params: "tokenHolder, generation, quorumAck[], fencedSites[]"
}];
function BypassCard() {
  const {
    data
  } = useResource("dr.arb", () => api.arbitration(), 8000);
  const t = data || {};
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Side channel"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement(SourceTag, {
    what: "simplyblock control plane"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", {
    style: {
      width: 26
    }
  }, "#"), /*#__PURE__*/React.createElement("th", null, "Payload"), /*#__PURE__*/React.createElement("th", null, "Direction"), /*#__PURE__*/React.createElement("th", null, "Why no Ramen field"), /*#__PURE__*/React.createElement("th", null, "Parameters"))), /*#__PURE__*/React.createElement("tbody", null, BYPASS.map(b => /*#__PURE__*/React.createElement("tr", {
    key: b.n
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, b.n), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("b", null, b.name), b.console && /*#__PURE__*/React.createElement("span", {
    className: "badge",
    style: {
      marginLeft: 6,
      color: "var(--accent)",
      borderColor: "var(--accent-line)"
    }
  }, "this console reads it")), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, b.dir), /*#__PURE__*/React.createElement("td", {
    style: {
      fontSize: 11.5,
      color: "var(--dim)"
    }
  }, b.why), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontSize: 10.5
    }
  }, b.params))))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "0 12px",
      padding: "9px 0 0"
    }
  }, "Payloads 4 and 5 are why an application reports every declared method while Ramen reports one. Read from Kubernetes alone, a plan with three methods shows a healthy single-leg posture and no generations at all."), /*#__PURE__*/React.createElement("div", {
    className: "arb"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "token holder"), t.holder || "—"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "generation"), t.generation != null ? t.generation : "—"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "quorum"), (t.quorumAck || []).length, "/", t.quorumSize || "—"), (t.fencedSites || []).length ? /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--bad)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "fenced"), t.fencedSites.join(", ")) : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ok)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "fenced"), "none"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "updated"), fmtAgo(t.updatedAt))))));
}

// ---- details ---------------------------------------------------------------
function PlanDetail({
  o: p,
  nav
}) {
  const sync = p.methods.filter(m => m.type === "sync");
  const vault = p.methods.find(m => m.type === "snapshot-s3");
  const broken = p.methods.filter(m => !m.intervalConsistent);
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: p,
    title: p.name,
    sub: p.storageProfile,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "ProtectionPlan"), p.protectionGap && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--bad)",
        borderColor: "color-mix(in srgb,var(--bad) 45%,transparent)"
      }
    }, "protection gap"))
  }), broken.map(m => /*#__PURE__*/React.createElement("div", {
    className: "banner",
    key: m.name
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, m.name, ": the interval is written to two places and they disagree."), " The policy carries ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, m.interval), " and the class parameters carry ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, m.classInterval), ". Ramen resolves a class by matching the provisioner, that interval string and the selector labels \u2014 so no class resolves, no peerClass appears, the policy still validates, and every application on this method is protected by nothing. Both values must be emitted from one field."))), p.methods.some(m => m.type === "sync") && p.methods.some(m => m.type !== "sync") && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "One DRPC per application."), " A DRPC selects PVCs by label, so two over the same PVCs would both claim them \u2014 the documented outcome is data corruption. Each application therefore names one orchestrated method; the others run with identical parameters in the data plane and report their lag out of band.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Methods",
    v: p.counts.methods,
    s: p.methods.map(m => mmeta(m.type).short).join(" · ")
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Sites",
    v: p.counts.sites,
    s: p.siteNames.join(", ")
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Applications",
    v: p.counts.apps,
    s: `${p.counts.pvcs} PVCs`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Worst leg lag",
    v: fmtLag(p.worstLagSeconds),
    c: p.status === "healthy" ? null : "var(--warn)"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Generations",
    v: p.counts.generations,
    s: vault ? `vault · ${vault.interval}` : "no vault method"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Declared methods"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("button", {
    className: "btn",
    onClick: () => window.__ui.dialog(addMethodDialog(p), p)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Add method")), /*#__PURE__*/React.createElement(MethodMatrix, {
    p: p,
    nav: nav
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "All three methods are configured identically and travel the same control path down to the driver. What differs is the driver's implementation of the relationship: ", sync.length ? "an inline write mirror to the peer, " : "", "a block delta per epoch to the peer, and an immutable snapshot stream into an object store. Method selection happens entirely at class resolution \u2014 the policy's selector and interval pick exactly one class, and that class's parameters are the whole difference."), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "cluster",
    title: "Applications",
    sub: "workloads bound to this plan",
    count: p.counts.apps,
    onClick: () => nav.layer(p, "protectedapps")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "k8s",
    title: "Sites",
    sub: "managed clusters this plan spans",
    count: p.counts.sites,
    onClick: () => nav.layer(p, "sites")
  })), /*#__PURE__*/React.createElement(DerivationCard, {
    p: p,
    nav: nav
  }), /*#__PURE__*/React.createElement(BypassCard, null), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Plan"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", p.name], ["Storage profile", p.storageProfile], ["storageID", `sb-${p.name}`], ["Sites", p.siteNames.join(", ")], ["Vault bucket", vault ? vault.bucket : "—"], ["Object lock", vault ? vault.immutable ? "compliance — generations cannot be deleted early" : "none" : "—"], ["Created", fmtDate(p.createdAt)]]
  }));
}
function SiteDetail({
  o: s,
  nav
}) {
  const fenced = s.fencing !== "Unfenced";
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: s,
    title: s.name,
    sub: s.region,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "DRCluster"), fenced && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--bad)",
        borderColor: "color-mix(in srgb,var(--bad) 45%,transparent)"
      }
    }, s.fencing))
  }), fenced && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, s.fencing, "."), " No I/O is accepted from this site. Fencing in Ramen is pair-scoped, so with three or more sites the decision is taken by quorum through the arbitration token rather than by the DRCluster alone.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Active applications",
    v: s.counts.activeApps,
    s: "running here now"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Standby for",
    v: s.counts.standbyApps,
    s: "failover or restore target"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Plans",
    v: s.counts.plans
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Discovered classes",
    v: s.counts.classes,
    s: "reported by DRClusterConfig"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "cluster",
    title: "Applications",
    sub: "active or standby here",
    count: s.counts.activeApps + s.counts.standbyApps,
    onClick: () => nav.layer(s, "protectedapps")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "shield",
    title: "Plans",
    sub: "plans spanning this site",
    count: s.counts.plans,
    onClick: () => nav.layer(s, "plans")
  }), s.k8sClusterId && /*#__PURE__*/React.createElement(NavCard, {
    icon: "k8s",
    title: "Managed cluster",
    sub: "the Kubernetes cluster itself",
    count: "\u2192",
    onClick: () => nav.openK8s(s.k8sClusterId)
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "DRCluster"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, s.name)], ["Region", /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, s.region)], ["s3ProfileName", /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, s.s3Profile)], ["S3 endpoint", /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, s.s3Endpoint)], ["Metadata bucket", /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, s.s3Bucket)], ["CIDRs", /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, s.cidrs.join(", "))], ["clusterFence", s.fencing], ["Ramen", s.ramen], ["Onboarded", fmtDate(s.createdAt)]]
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "The site name must equal the OCM ManagedCluster name \u2014 it is the identity Ramen keys DRCluster on. The region is how synchronous and asynchronous protection are declared: an equal region on both sides of a pair permits a synchronous mirror, a distinct region does not."), !!s.discoveredClasses.length && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Discovered replication classes"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, s.discoveredClasses.map(c => /*#__PURE__*/React.createElement("span", {
    className: "lab mono",
    key: c
  }, c))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "9px 0 0"
    }
  }, "Reported up by DRClusterConfig on the managed cluster. A class present on one side of a pair and absent on the other produces no peerClass, and therefore no protection.")))));
}

// ---- DR landing ------------------------------------------------------------
function DrHome({
  nav
}) {
  const plans = useResource("dr.plans", () => api.plans(), 8000);
  const sites = useResource("dr.sites", () => api.sites(), 12000);
  const apps = useResource("dr.apps", () => api.protectedApps(), 6000);
  const ps = plans.data || [],
    ss = sites.data || [],
    as = apps.data || [];
  const legs = as.flatMap(a => a.legs || []);
  const gaps = ps.filter(p => p.protectionGap);
  const worst = legs.reduce((n, l) => Math.max(n, l.lagSeconds || 0), 0);
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "dhead"
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      minWidth: 0,
      flex: 1
    }
  }, /*#__PURE__*/React.createElement("h1", null, "Disaster recovery"), /*#__PURE__*/React.createElement("div", {
    className: "dsub"
  }, "One plan per protection posture, one application per workload. Ramen objects are derived.")), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    onClick: () => window.__ui.dialog(newPlanDialog(ss), {
      kind: "plan",
      id: "new"
    })
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "New plan")), !!gaps.length && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, gaps.length, " plan", gaps.length === 1 ? " has" : "s have", " a protection gap."), " A declared method's interval is written to the policy and to the class parameters and the two disagree, so no class resolves and the applications on that method are protected by nothing \u2014 while every object still reports healthy.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Plans",
    v: ps.length,
    s: `${ps.reduce((n, p) => n + p.counts.methods, 0)} declared methods`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Sites",
    v: ss.length,
    s: [...new Set(ss.map(s => s.region))].join(", ")
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Applications",
    v: as.length,
    s: `${as.reduce((n, a) => n + a.counts.pvcs, 0)} PVCs`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Legs",
    v: legs.length,
    c: legs.some(l => l.status === "unhealthy") ? "var(--bad)" : null,
    s: `${legs.filter(l => l.orchestrated).length} orchestrated`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Worst leg lag",
    v: fmtLag(worst)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Generations",
    v: as.reduce((n, a) => n + a.generations.length, 0),
    s: "restorable points"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Protection plans"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => nav.drLayer("plans")
  }, "Open all")), /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, ps.slice(0, 6).map(p => /*#__PURE__*/React.createElement(PlanTile, {
    key: p.id,
    p: p,
    nav: nav
  }))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Sites"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => nav.drLayer("sites")
  }, "Open all")), /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, ss.slice(0, 6).map(s => /*#__PURE__*/React.createElement(SiteTile, {
    key: s.id,
    s: s,
    nav: nav
  }))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Applications"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => nav.drLayer("protectedapps")
  }, "Open all")), /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, as.slice(0, 6).map(a => /*#__PURE__*/React.createElement(ProtectedAppTile, {
    key: a.id,
    a: a,
    nav: nav
  }))));
}
Object.assign(window, {
  METHOD_META,
  mmeta,
  MethodBadge,
  fmtLag,
  BYPASS,
  PlanTile,
  SiteTile,
  PlanDetail,
  SiteDetail,
  MethodMatrix,
  DerivationCard,
  BypassCard,
  DrHome
});
})();
// ---- repl.jsx ----
(function(){
// ---------------------------------------------------------------------------
// REPLICATION — UI for the real v1alpha1 kinds
//
// Four kinds, and the shape matters:
//   ReplicationPair    reusable {sourceCluster, targetCluster}
//   ReplicationPolicy  {pairRef, mode, interval, snapshotRetention}
//   ReplicationSlot    one per PVC, created by the operator, owned by the PVC
//   ReplicationOps     one-shot {action, scope, ref}
//
// Two things the console must not imply: that a policy has a volume list (it
// does not — a PVC annotation is the whole membership model), and that
// replication can be synchronous (mode is exactly failover | migration).
// ---------------------------------------------------------------------------
const REPL_ANNOTATION = "storage.simplyblock.io/replication-policy";
// The house convention is className="mono"; this is just that span, so the CRD
// field values below read as the literals they are.
const Mono = ({
  children
}) => /*#__PURE__*/React.createElement("span", {
  className: "mono"
}, children);

// Colour and label for a slot state come from STATUS_META, which is the one
// vocabulary the traffic light and the tile stripe both read. This carries the
// explanation and nothing else, so the two can never disagree.
const SLOT_HINT = {
  replicating: "shipping a delta once per interval",
  cutover_pending: "final delta transferred, waiting for the commit",
  cutover_done: "the target is authoritative; the migration is complete",
  failed_over: "the target was promoted; the source is no longer authoritative",
  attaching: "legacy state — an attach is synchronous now, so a new slot reaches replicating directly",
  detaching: "removing the replication snapshots on both sides",
  error: "the backend refused the last call"
};
const smeta = s => Object.assign({
  label: s,
  c: "var(--idle)"
}, STATUS_META[s] || {}, {
  hint: SLOT_HINT[s] || ""
});
const MODE_META = {
  failover: {
    label: "failover",
    c: "var(--ro)",
    desc: "The target is a DR standby and its volumes are read-only. Promoting it is a deliberate ReplicationOps, never automatic."
  },
  migration: {
    label: "migration",
    c: "var(--info)",
    desc: "A planned online cutover. Both clusters stay up and the commit runs per volume: replicating → cutover_pending → cutover_done."
  }
};
const ModeBadge = ({
  mode
}) => {
  const m = MODE_META[mode] || MODE_META.failover;
  return /*#__PURE__*/React.createElement("span", {
    className: "badge",
    style: {
      color: m.c,
      borderColor: `color-mix(in srgb,${m.c} 45%,transparent)`
    },
    title: m.desc
  }, m.label);
};
const OPS_ACTION_META = {
  failover: {
    label: "fail over",
    danger: true,
    desc: "Unplanned. The target clone is promoted and the source may be down. Work written after the last replication snapshot is lost."
  },
  failback: {
    label: "fail back",
    danger: false,
    desc: "Restores the source as primary after a failover. A short write freeze holds while the final delta transfers, so plan a window."
  },
  migration: {
    label: "cut over",
    danger: false,
    desc: "Planned. Commits the cutover per volume with both clusters up, optionally deleting the source volume afterwards."
  }
};
const SCOPE_HINT = {
  target: "every volume of every policy on the pair",
  policy: "every volume attached to this policy",
  volume: "one volume — exactly one slot"
};
// One reference clock, the fixtures': measuring their timestamps against the
// wall clock is what made every slot read days stale and every policy degraded.
const REPL_NOW = () => window.SB_NOW || Date.now();
const minsSince = at => at ? (REPL_NOW() - Date.parse(at)) / 60000 : Infinity;
const relAge = at => {
  if (!at) return "never";
  const m = Math.round(minsSince(at));
  return m < 1 ? "just now" : m < 60 ? m + "m ago" : m < 1440 ? Math.round(m / 60) + "h ago" : Math.round(m / 1440) + "d ago";
};

// ---- tiles -----------------------------------------------------------------
function PairTile({
  p,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[p.status].c
    },
    onDoubleClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: p,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    }), /*#__PURE__*/React.createElement(Name, null, p.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge",
      title: "ReplicationPair"
    }, "relpair")
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, p.sourceCluster, " \u2192 ", p.targetCluster), /*#__PURE__*/React.createElement(Uuid, {
    value: p.id
  }), !p.ready && /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: "var(--bad)"
    }
  }, p.message || "Backend replication target not available"), p.activeOpsRef && /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), p.activeOpsRef, " holds the pair lock \u2014 a second operation waits for it"), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "target"), p.backendTargetId ? /*#__PURE__*/React.createElement(Mono, null, p.backendTargetId.slice(0, 8)) : "—"), /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "targetCluster is immutable after creation"
  }, /*#__PURE__*/React.createElement("i", null, "immutable"), "targetCluster")), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Policies"), /*#__PURE__*/React.createElement("b", null, p.counts.policies)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Slots"), /*#__PURE__*/React.createElement("b", null, p.counts.slots))), p.counts.failedOver || p.counts.errored ? /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, !!p.counts.failedOver && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "failed over"), p.counts.failedOver), !!p.counts.errored && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--bad)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "error"), p.counts.errored)) : null, /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Policies",
      count: p.counts.policies,
      icon: "clock",
      onClick: () => nav.layer(p, "rpolicies")
    }, {
      label: "Slots",
      count: p.counts.slots,
      icon: "volume",
      onClick: () => nav.layer(p, "slots")
    }],
    onDetail: () => nav.detail(p)
  }));
}
function RPolicyTile({
  p,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[p.status].c
    },
    onDoubleClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: p,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    }), /*#__PURE__*/React.createElement(Name, null, p.name)),
    right: /*#__PURE__*/React.createElement(ModeBadge, {
      mode: p.mode
    })
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, p.sourceCluster || "?", " \u2192 ", p.targetCluster || "?"), /*#__PURE__*/React.createElement(Uuid, {
    value: p.id
  }), !p.ready && /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: "var(--bad)"
    }
  }, p.message || "Backend policy not created"), p.activeOpsRef && /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), p.activeOpsRef, " in flight"), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openPairByName(p.pairRef);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), p.pairRef), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "interval"), p.interval), /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "minimum snapshots kept on the target (minimum 2)"
  }, /*#__PURE__*/React.createElement("i", null, "retain"), p.snapshotRetention)), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Slots"), /*#__PURE__*/React.createElement("b", null, p.counts.slots)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Last snapshot"), /*#__PURE__*/React.createElement("b", null, relAge(p.lastAt)))), p.counts.errored || p.counts.late || p.counts.failedOver || p.counts.cutoverPending ? /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, !!p.counts.errored && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--bad)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "error"), p.counts.errored), !!p.counts.late && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--warn)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "late"), p.counts.late), !!p.counts.cutoverPending && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--warn)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "cutover"), p.counts.cutoverPending), !!p.counts.failedOver && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "failed over"), p.counts.failedOver)) : null, /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Slots",
      count: p.counts.slots,
      icon: "volume",
      onClick: () => nav.layer(p, "slots")
    }, {
      label: "Operations",
      icon: "clock",
      onClick: () => nav.layer(p, "replops")
    }],
    onDetail: () => nav.detail(p)
  }));
}
function SlotTile({
  s,
  nav
}) {
  const m = smeta(s.state);
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": m.c
    },
    onDoubleClick: () => nav.detail(s)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: s,
    left: /*#__PURE__*/React.createElement(TrafficLight, {
      status: s.state
    }),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge",
      title: "ReplicationSlot"
    }, "relslot")
  }), /*#__PURE__*/React.createElement("div", {
    className: "nm",
    style: {
      marginTop: 4
    }
  }, /*#__PURE__*/React.createElement("b", null, s.pvcRef)), /*#__PURE__*/React.createElement("div", {
    className: "tsub"
  }, s.name), /*#__PURE__*/React.createElement(Uuid, {
    value: s.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: s.state === "error" ? "var(--bad)" : "var(--dim)"
    }
  }, s.message || m.hint), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openRPolicyByName(s.policyRef);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 10
  }), s.policyRef), /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "which side of the relationship this cluster holds"
  }, /*#__PURE__*/React.createElement("i", null, "direction"), s.direction), s.ownedBy && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "the slot is owned by its PVC, so deleting the PVC cascades"
  }, /*#__PURE__*/React.createElement("i", null, "owned by"), s.ownedBy)), /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Last replicated"), /*#__PURE__*/React.createElement("b", null, relAge(s.lastAt))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Target volume"), /*#__PURE__*/React.createElement("b", null, s.targetLvolId ? /*#__PURE__*/React.createElement(Mono, null, s.targetLvolId.slice(0, 8)) : "—"))), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(s)
    }]
  }));
}
function ReplOpsTile({
  o,
  nav
}) {
  const m = OPS_ACTION_META[o.action] || {};
  const failed = o.results.filter(r => r.status === "failed").length;
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[o.status].c
    },
    onDoubleClick: () => nav.detail(o)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: o,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: o.status
    }), /*#__PURE__*/React.createElement(Name, null, m.label || o.action)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge",
      title: "ReplicationOps"
    }, "replops")
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, o.scope, " \xB7 ", o.ref), /*#__PURE__*/React.createElement(Uuid, {
    value: o.id
  }), o.phase === "Running" && /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), o.subphase || "Running"), /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: failed ? "var(--bad)" : "var(--dim)"
    }
  }, o.message), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "phase"), o.phase), o.deleteSource && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--bad)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "deleteSource"), "yes"), o.terminal && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "a terminal operation is never re-run \u2014 a repeat needs a new ReplicationOps"
  }, /*#__PURE__*/React.createElement("i", null, "one-shot"), "spent")), !!o.results.length && /*#__PURE__*/React.createElement("div", {
    className: "rw"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Succeeded"), /*#__PURE__*/React.createElement("b", {
    style: {
      color: "var(--ok)"
    }
  }, o.results.filter(r => r.status === "succeeded").length)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Skipped"), /*#__PURE__*/React.createElement("b", null, o.results.filter(r => r.status === "skipped").length)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Failed"), /*#__PURE__*/React.createElement("b", {
    style: failed ? {
      color: "var(--bad)"
    } : null
  }, failed))), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(o)
    }]
  }));
}

// ---- details ---------------------------------------------------------------
function PairDetail({
  o: p,
  nav
}) {
  const {
    data: ops
  } = useResource("pair.ops|" + p.name, () => api.refReplOps(p.name), 5000);
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: p,
    title: p.name,
    sub: `${p.sourceCluster} → ${p.targetCluster}`,
    badge: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "ReplicationPair")
  }), !p.ready && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "The backend replication target is not available."), " ", p.message, " No policy on this pair can replicate until it is.")), p.activeOpsRef && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, p.activeOpsRef, " holds the pair lock."), " Only one target-scoped operation runs per pair; a second one waits for the lock rather than failing.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Ready",
    v: p.ready ? "yes" : "no",
    c: p.ready ? "var(--ok)" : "var(--bad)"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Policies",
    v: p.counts.policies,
    s: "each with its own interval"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Slots",
    v: p.counts.slots,
    s: "one per replicated PVC"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Failed over",
    v: p.counts.failedOver,
    c: p.counts.failedOver ? "var(--ro)" : null
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "clock",
    title: "Policies",
    sub: "schedules on this pair",
    count: p.counts.policies,
    onClick: () => nav.layer(p, "rpolicies")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Slots",
    sub: "volumes replicating across it",
    count: p.counts.slots,
    onClick: () => nav.layer(p, "slots")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "cluster",
    title: "Source cluster",
    sub: p.sourceCluster,
    count: "\u2192",
    onClick: () => nav.openClusterByName(p.sourceCluster)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "cluster",
    title: "Target cluster",
    sub: p.targetCluster,
    count: "\u2192",
    onClick: () => nav.openClusterByName(p.targetCluster)
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "ReplicationPair"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement(Props, {
    rows: [["metadata.name", /*#__PURE__*/React.createElement(Mono, null, p.name)], ["spec.sourceCluster", /*#__PURE__*/React.createElement(Mono, null, p.sourceCluster)], ["spec.targetCluster", /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(Mono, null, p.targetCluster), " ", /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)",
        fontSize: 11
      }
    }, "immutable after creation"))], ["status.ready", String(p.ready)], ["status.backendTargetID", p.backendTargetId ? /*#__PURE__*/React.createElement(Mono, null, p.backendTargetId) : "—"], ["status.activeOpsRef", p.activeOpsRef || "none"], ["status.message", p.message || "—"], ["Created", fmtDate(p.createdAt)]]
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "A pair is reusable configuration: several policies may replicate between the same two clusters on different schedules. Both clusters must be StorageCluster resources in this namespace with ", /*#__PURE__*/React.createElement(Mono, null, "status.uuid"), " populated \u2014 cross-namespace references are not supported. Because ", /*#__PURE__*/React.createElement(Mono, null, "spec.targetCluster"), " is immutable, changing a target means a new pair. Deleting this one is refused while any policy references it, and it deletes the backend replication target."), /*#__PURE__*/React.createElement(ConditionsCard, {
    conditions: p.conditions
  }), /*#__PURE__*/React.createElement(OpsHistory, {
    ops: ops,
    nav: nav,
    title: "Operations on this pair"
  }));
}
function RPolicyDetail({
  o: p,
  nav
}) {
  const {
    data: slots
  } = useResource("pol.slots|" + p.id, () => api.policySlots(p.id), 6000);
  const {
    data: ops
  } = useResource("pol.ops|" + p.name, () => api.refReplOps(p.name), 5000);
  const mode = MODE_META[p.mode] || MODE_META.failover;
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: p,
    title: p.name,
    sub: `${p.sourceCluster || "?"} → ${p.targetCluster || "?"}`,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "ReplicationPolicy"), /*#__PURE__*/React.createElement(ModeBadge, {
      mode: p.mode
    }))
  }), !!p.counts.errored && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, p.counts.errored, " slot", p.counts.errored === 1 ? "" : "s", " in error."), " The backend refused the last replication call for those volumes. The rest of the policy keeps replicating \u2014 slot state is per volume.")), !!p.counts.late && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, p.counts.late, " slot", p.counts.late === 1 ? " is" : "s are", " behind the ", p.interval, " interval."), " A failover now would lose more than one interval of work for them.")), p.activeOpsRef && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, p.activeOpsRef, " is in flight."), " One operation runs per policy; anything else queues on the lock.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Mode",
    v: mode.label,
    c: mode.c
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Interval",
    v: p.interval,
    s: "target RPO"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Snapshot retention",
    v: p.snapshotRetention,
    s: "minimum kept on the target"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Slots",
    v: p.counts.slots,
    s: `${p.counts.replicating} replicating`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Last snapshot",
    v: relAge(p.lastAt),
    s: p.lastAt ? fmtDate(p.lastAt) : ""
  })), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, mode.desc), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Attached volumes"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, (slots || []).length, " slots"), /*#__PURE__*/React.createElement("button", {
    className: "btn",
    onClick: () => window.__ui.dialog(attachPvcDialog(p), p)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Attach a PVC")), /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "A policy has no volume list."), " Membership is the annotation ", /*#__PURE__*/React.createElement(Mono, null, REPL_ANNOTATION), " on the PVC, or on its StorageClass \u2014 the PVC wins. The operator creates one ReplicationSlot per bound PVC and owns it, so attaching and detaching here writes that annotation and nothing else.")), /*#__PURE__*/React.createElement(SlotTable, {
    slots: slots,
    nav: nav,
    interval: p.interval
  }), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Slots",
    sub: "one per replicated PVC",
    count: p.counts.slots,
    onClick: () => nav.layer(p, "slots")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "link",
    title: "Pair",
    sub: p.pairRef,
    count: "\u2192",
    onClick: () => nav.openPairByName(p.pairRef)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "clock",
    title: "Operations",
    sub: "failover, failback, cutover",
    count: (ops || []).length,
    onClick: () => nav.layer(p, "replops")
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "ReplicationPolicy"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement(Props, {
    rows: [["metadata.name", /*#__PURE__*/React.createElement(Mono, null, p.name)], ["spec.pairRef", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPairByName(p.pairRef),
      label: p.pairRef
    })], ["spec.mode", /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(Mono, null, p.mode), " ", /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)",
        fontSize: 11
      }
    }, "failover | migration"))], ["spec.interval", /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(Mono, null, p.interval), " ", /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)",
        fontSize: 11
      }
    }, "rounded to whole minutes, minimum 1m"))], ["spec.snapshotRetention", /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(Mono, null, p.snapshotRetention), " ", /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)",
        fontSize: 11
      }
    }, "minimum 2"))], ["status.ready", String(p.ready)], ["status.backendPolicyID", p.backendPolicyId ? /*#__PURE__*/React.createElement(Mono, null, p.backendPolicyId) : "—"], ["status.slotCount", p.counts.slots], ["status.activeOpsRef", p.activeOpsRef || "none"], ["Created", fmtDate(p.createdAt)]]
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "There is no tiered retention schedule here and no consistency-group field: one interval and one snapshot count is the whole model. Deletion is refused while any slot references the policy \u2014 detach the PVCs first. Repointing a PVC at a different policy is a detach followed by a fresh attach, so the new target takes a full copy."), /*#__PURE__*/React.createElement(ConditionsCard, {
    conditions: p.conditions
  }), /*#__PURE__*/React.createElement(OpsHistory, {
    ops: ops,
    nav: nav,
    title: "Operations on this policy"
  }));
}
function SlotDetail({
  o: s,
  nav
}) {
  const m = smeta(s.state);
  const [cid, pid, vid] = s.volumeParts || [];
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: s,
    title: s.pvcRef,
    sub: s.name,
    badge: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "ReplicationSlot")
  }), s.state === "error" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, s.message), " The operator backs off to a 30-second poll while a slot is in error, instead of the usual 60.")), s.state === "cutover_pending" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Waiting for the cutover commit."), " The final delta has transferred. A ", /*#__PURE__*/React.createElement(Mono, null, "migration"), "-scoped ReplicationOps commits it and the slot moves to ", /*#__PURE__*/React.createElement(Mono, null, "cutover_done"), ".")), s.state === "attaching" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--dim)",
      background: "var(--panel2)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, m.hint)), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "State",
    v: m.label,
    c: m.c,
    s: m.hint
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Direction",
    v: s.direction,
    s: s.direction === "source" ? "this cluster holds the source" : "this cluster holds the replica"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Last replicated",
    v: relAge(s.lastAt),
    s: s.lastAt ? fmtDate(s.lastAt) : "no snapshot yet — taking the full copy"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Target volume",
    v: s.targetLvolId ? s.targetLvolId.slice(0, 8) : "—"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "clock",
    title: "Policy",
    sub: s.policyRef,
    count: "\u2192",
    onClick: () => nav.openRPolicyByName(s.policyRef)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "PVC",
    sub: s.pvcRef,
    count: "\u2192",
    onClick: () => nav.openPvcByName(s.pvcRef)
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "ReplicationSlot"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement(Props, {
    rows: [["metadata.name", /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(Mono, null, s.name), " ", /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)",
        fontSize: 11
      }
    }, "<policy>-<pvc>"))], ["ownerReferences", s.ownedBy ? /*#__PURE__*/React.createElement(Mono, null, s.ownedBy) : "—"], ["spec.policyRef", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openRPolicyByName(s.policyRef),
      label: s.policyRef
    })], ["spec.pvcRef", /*#__PURE__*/React.createElement(Mono, null, s.pvcRef)], ["spec.volumeID", /*#__PURE__*/React.createElement(Mono, null, s.volumeId)], ["— cluster UUID", cid ? /*#__PURE__*/React.createElement(Mono, null, cid) : "—"], ["— pool UUID", pid ? /*#__PURE__*/React.createElement(Mono, null, pid) : "—"], ["— volume UUID", vid ? /*#__PURE__*/React.createElement(Mono, null, vid) : "—"], ["status.sourceLvolID", s.sourceLvolId ? /*#__PURE__*/React.createElement(Mono, null, s.sourceLvolId) : "—"], ["status.targetLvolID", s.targetLvolId ? /*#__PURE__*/React.createElement(Mono, null, s.targetLvolId) : "—"], ["status.targetNQN", s.targetNqn ? /*#__PURE__*/React.createElement(Mono, null, s.targetNqn) : "— populated after failover"], ["status.message", s.message || "—"], ["Created", fmtDate(s.createdAt)]]
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "All three spec fields are immutable. The slot is owned by its PVC, so deleting the PVC cascades to the slot and the finalizer detaches it in the backend. Clearing the annotation deletes the replication snapshots on both sides before the slot is removed."), /*#__PURE__*/React.createElement(ConditionsCard, {
    conditions: s.conditions
  }));
}
function ReplOpsDetail({
  o,
  nav
}) {
  const m = OPS_ACTION_META[o.action] || {};
  const failed = o.results.filter(r => r.status === "failed");
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: o,
    title: `${m.label || o.action} · ${o.ref}`,
    sub: o.name,
    badge: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "ReplicationOps")
  }), o.phase === "Running" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, o.subphase), " \u2014 ", o.message)), o.phase === "Failed" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, failed.length, " volume", failed.length === 1 ? "" : "s", " failed."), " Per-volume outcomes are independent: the rest continued, and the operation as a whole is Failed. A retry needs a ", /*#__PURE__*/React.createElement("b", null, "new"), " ReplicationOps \u2014 this one is spent.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Action",
    v: m.label || o.action
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Scope",
    v: o.scope,
    s: SCOPE_HINT[o.scope]
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Phase",
    v: o.phase,
    c: STATUS_META[o.status].c
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Started",
    v: o.startedAt ? fmtDate(o.startedAt) : "—"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Completed",
    v: o.completedAt ? fmtDate(o.completedAt) : "—"
  })), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, m.desc), !!o.results.length && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Per-volume results"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, o.results.length)), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Slot"), /*#__PURE__*/React.createElement("th", null, "Status"), /*#__PURE__*/React.createElement("th", null, "Detail"), /*#__PURE__*/React.createElement("th", null, "Target volume"))), /*#__PURE__*/React.createElement("tbody", null, o.results.map(r => /*#__PURE__*/React.createElement("tr", {
    key: r.slotRef,
    className: r.status === "failed" ? "bad" : ""
  }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("button", {
    className: "tlink",
    onClick: () => nav.openSlotByName(r.slotRef)
  }, r.slotRef)), /*#__PURE__*/React.createElement("td", null, r.status === "succeeded" ? /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ok)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 10
  }), "succeeded") : r.status === "skipped" ? /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, "skipped") : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--bad)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 10
  }), "failed")), /*#__PURE__*/React.createElement("td", {
    style: {
      fontSize: 11.5,
      color: "var(--dim)"
    }
  }, r.detail || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, r.targetLvolId ? r.targetLvolId.slice(0, 8) : "—")))))))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "ReplicationOps"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement(Props, {
    rows: [["metadata.name", /*#__PURE__*/React.createElement(Mono, null, o.name)], ["spec.action", /*#__PURE__*/React.createElement(Mono, null, o.action)], ["spec.scope", /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(Mono, null, o.scope), " \u2014 ", SCOPE_HINT[o.scope])], ["spec.ref", /*#__PURE__*/React.createElement(Mono, null, o.ref)], o.sourceClusterId ? ["spec.sourceClusterID", /*#__PURE__*/React.createElement(Mono, null, o.sourceClusterId)] : null, o.action === "migration" ? ["spec.deleteSource", String(o.deleteSource)] : null, ["status.phase", o.phase], ["status.subphase", o.subphase || "—"], ["status.message", o.message || "—"]].filter(Boolean)
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Every spec field is immutable and the operation is one-shot: once it reaches Succeeded or Failed it is never re-run, so a repeat or a correction is a new resource. The lock it held \u2014 the policy's ", /*#__PURE__*/React.createElement(Mono, null, "activeOpsRef"), ", or the pair's for a target-scoped operation \u2014 is released when it completes."));
}

// ---- shared bits -----------------------------------------------------------
function SlotTable({
  slots,
  nav,
  interval
}) {
  const rows = slots || [];
  const budget = ivMinutes(interval) * 2;
  const detach = s => window.__ui.dialog(detachPvcDialog(s), s);
  if (!rows.length) return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "No PVC carries this policy's annotation yet, so nothing is replicating."));
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "PVC"), /*#__PURE__*/React.createElement("th", null, "Slot"), /*#__PURE__*/React.createElement("th", null, "State"), /*#__PURE__*/React.createElement("th", null, "Direction"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Last replicated"), /*#__PURE__*/React.createElement("th", {
    style: {
      width: 150
    }
  }))), /*#__PURE__*/React.createElement("tbody", null, rows.map(s => {
    const m = smeta(s.state);
    const late = s.state === "replicating" && s.lastAt && minsSince(s.lastAt) > budget;
    return /*#__PURE__*/React.createElement("tr", {
      key: s.id,
      className: s.state === "error" ? "bad" : ""
    }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("button", {
      className: "tlink",
      onClick: () => nav.openPvcByName(s.pvcRef)
    }, s.pvcRef)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("button", {
      className: "tlink",
      onClick: () => nav.detail(s)
    }, s.name)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: s.state,
      sm: true,
      label: true
    })), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, s.direction), /*#__PURE__*/React.createElement("td", {
      className: "mono",
      style: Object.assign({
        textAlign: "right"
      }, late ? {
        color: "var(--warn)"
      } : {})
    }, relAge(s.lastAt)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("div", {
      style: {
        display: "flex",
        gap: 5,
        justifyContent: "flex-end"
      }
    }, /*#__PURE__*/React.createElement("button", {
      className: "chip",
      onClick: () => window.__ui.dialog(volumeOpsDialog(s), s)
    }, "Operate\u2026"), /*#__PURE__*/React.createElement("button", {
      className: "chip",
      onClick: () => detach(s)
    }, "Detach"))));
  })))));
}
const ConditionsCard = ({
  conditions
}) => !(conditions || []).length ? null : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
  className: "sech"
}, /*#__PURE__*/React.createElement("h2", null, "Conditions"), /*#__PURE__*/React.createElement("span", {
  className: "ln"
})), /*#__PURE__*/React.createElement("div", {
  className: "card"
}, /*#__PURE__*/React.createElement("div", {
  className: "bd",
  style: {
    padding: 0
  }
}, /*#__PURE__*/React.createElement("table", {
  className: "dt"
}, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Type"), /*#__PURE__*/React.createElement("th", null, "Status"), /*#__PURE__*/React.createElement("th", null, "Reason"), /*#__PURE__*/React.createElement("th", null, "Message"), /*#__PURE__*/React.createElement("th", null, "Since"))), /*#__PURE__*/React.createElement("tbody", null, conditions.map((c, i) => /*#__PURE__*/React.createElement("tr", {
  key: i
}, /*#__PURE__*/React.createElement("td", {
  className: "mono"
}, c.type), /*#__PURE__*/React.createElement("td", {
  style: {
    color: c.status === "True" ? "var(--ok)" : "var(--dim)"
  }
}, c.status), /*#__PURE__*/React.createElement("td", {
  className: "mono"
}, c.reason || "—"), /*#__PURE__*/React.createElement("td", {
  style: {
    fontSize: 11.5,
    color: "var(--dim)"
  }
}, c.message || "—"), /*#__PURE__*/React.createElement("td", {
  className: "mono"
}, c.lastTransitionTime ? relAge(c.lastTransitionTime) : "—"))))))));
const OpsHistory = ({
  ops,
  nav,
  title
}) => {
  const rows = ops || [];
  if (!rows.length) return null;
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, title), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, rows.length)), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Operation"), /*#__PURE__*/React.createElement("th", null, "Action"), /*#__PURE__*/React.createElement("th", null, "Scope"), /*#__PURE__*/React.createElement("th", null, "Phase"), /*#__PURE__*/React.createElement("th", null, "Outcome"), /*#__PURE__*/React.createElement("th", null, "Started"))), /*#__PURE__*/React.createElement("tbody", null, rows.map(o => {
    const failed = o.results.filter(r => r.status === "failed").length;
    return /*#__PURE__*/React.createElement("tr", {
      key: o.id,
      className: o.phase === "Failed" ? "bad" : ""
    }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("button", {
      className: "tlink",
      onClick: () => nav.detail(o)
    }, o.name)), /*#__PURE__*/React.createElement("td", null, (OPS_ACTION_META[o.action] || {}).label || o.action), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, o.scope), /*#__PURE__*/React.createElement("td", null, o.phase, o.subphase ? /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)"
      }
    }, " \xB7 ", o.subphase) : null), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, o.results.length ? `${o.results.length - failed}/${o.results.length} ok` : "—"), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, o.startedAt ? relAge(o.startedAt) : "—"));
  }))))));
};
Object.assign(window, {
  REPL_ANNOTATION,
  Mono,
  SLOT_HINT,
  smeta,
  MODE_META,
  ModeBadge,
  OPS_ACTION_META,
  SCOPE_HINT,
  relAge,
  minsSince,
  REPL_NOW,
  PairTile,
  RPolicyTile,
  SlotTile,
  ReplOpsTile,
  PairDetail,
  RPolicyDetail,
  SlotDetail,
  ReplOpsDetail,
  SlotTable,
  ConditionsCard,
  OpsHistory
});
})();
// ---- cgroups.jsx ----
(function(){
// ---------------------------------------------------------------------------
// CONSISTENCY GROUPS — a named set of volumes, independent of replication.
// Group snapshots are crash-consistent across every member, can be backed up,
// and can be restored into new volumes in this or any other cluster.
// ---------------------------------------------------------------------------
function CgroupTile({
  g,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[g.status].c
    },
    onDoubleClick: () => nav.detail(g)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: g,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: g.status
    }), /*#__PURE__*/React.createElement(Name, null, g.name)),
    right: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, g.counts.volumes, " volumes"), g.locked && /*#__PURE__*/React.createElement("span", {
      className: "lab",
      title: "membership is fixed while a policy is attached",
      style: {
        color: "var(--ro)",
        borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "lock",
      s: 10
    })))
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: g.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openCluster(g.clusterId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 10
  }), regName(g.clusterId)), g.backupPolicy ? /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    style: {
      color: "var(--ok)",
      borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"
    },
    onClick: e => {
      e.stopPropagation();
      nav.openPolicy(g.clusterId, g.backupPolicy.uuid);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cloud",
    s: 10
  }), g.backupPolicy.policy_name) : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      opacity: .65
    }
  }, /*#__PURE__*/React.createElement("i", null, "backup"), "none"), g.replicationConfig ? /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--accent)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "shield",
    s: 10
  }), "every ", g.replicationConfig.frequency, " min") : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      opacity: .65
    }
  }, /*#__PURE__*/React.createElement("i", null, "replication"), "none"), !!(g.drPolicyIds || []).length && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "dr policies"), g.drPolicyIds.length), !!g.counts.apps && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "dr apps"), g.counts.apps)), /*#__PURE__*/React.createElement(Capacity, {
    label: "Provisioned",
    total: g.capacity.total,
    used: g.capacity.used
  }), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Volumes"), /*#__PURE__*/React.createElement("b", null, g.counts.volumes)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Group snapshots"), /*#__PURE__*/React.createElement("b", null, g.counts.snapshots)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Backed up"), /*#__PURE__*/React.createElement("b", null, g.counts.backedUp))), g.replication && /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Replication"), /*#__PURE__*/React.createElement("b", {
    style: {
      color: STATUS_META[g.replication.status].c
    }
  }, g.replication.status)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Last cycle"), /*#__PURE__*/React.createElement("b", null, g.replication.lastAt ? clockOf(g.replication.lastAt) : "—")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Backlog"), /*#__PURE__*/React.createElement("b", null, fmtBytes(g.replication.backlog)))), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Volumes",
      count: g.counts.volumes,
      icon: "volume",
      onClick: () => nav.layer(g, "volumes")
    }, {
      label: "Snapshots",
      count: g.counts.snapshots,
      icon: "camera",
      onClick: () => nav.layer(g, "cgsnapshots")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(g)
    }]
  }));
}
function CgSnapshotTile({
  s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[s.status].c
    },
    onDoubleClick: () => nav.detail(s)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: s,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: s.status
    }), /*#__PURE__*/React.createElement(Name, null, s.name)),
    right: s.backupVersionId ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "cloud",
      s: 10
    }), s.backupVersionId) : /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        opacity: .6
      }
    }, "not backed up")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: s.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openCgroup(s.cgId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), s.cgName), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "members"), s.counts.volumes)), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Taken"), /*#__PURE__*/React.createElement("b", null, fmtDate(s.createdAt))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Age"), /*#__PURE__*/React.createElement("b", null, fmtAgo(s.createdAt))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Size"), /*#__PURE__*/React.createElement("b", null, fmtBytes(s.capacity.total)))), /*#__PURE__*/React.createElement("div", {
    className: "cgstrip"
  }, s.members.slice(0, 6).map(m => /*#__PURE__*/React.createElement("span", {
    className: "zonechip",
    key: m.snapshotId,
    title: `${m.volumeName} · ${fmtBytes(m.size)}`
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "volume",
    s: 10
  }), m.volumeName.split("-").slice(0, 2).join("-"))), s.members.length > 6 && /*#__PURE__*/React.createElement("span", {
    className: "zonechip"
  }, "+", s.members.length - 6)), s.bucket && /*#__PURE__*/React.createElement("div", {
    className: "bucket",
    title: s.bucket
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cloud",
    s: 11
  }), s.bucket), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(s)
    }]
  }));
}

// The protection the group owns, and what it means for the members.
function CgProtection({
  g,
  nav
}) {
  const none = !g.backupPolicy && !g.replicationConfig;
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Group protection"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, none ? "not protected as a group" : `applies to all ${g.counts.volumes} members`)), none ? /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "This group has no policy of its own, so its members are protected individually \u2014 or not at all. Attach a backup policy to snapshot and back every member up at the same instant, or a replication policy to ship them as one crash-consistent set. Both are on the group's actions menu."))) : /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, g.backupPolicy && /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Backup policy", /*#__PURE__*/React.createElement("span", {
    className: "cnt",
    style: {
      marginLeft: 8,
      fontSize: 11,
      color: "var(--dim2)"
    }
  }, "group-consistent")), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "labels",
    style: {
      marginBottom: 8
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    style: {
      color: "var(--ok)",
      borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"
    },
    onClick: () => nav.openPolicy(g.clusterId, g.backupPolicy.uuid)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cloud",
    s: 10
  }), g.backupPolicy.policy_name), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "members"), g.counts.volumes), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "versions"), g.counts.backedUp)), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "Every cycle snapshots all ", g.counts.volumes, " members at one instant and backs the set up as a single version. Any retained version is a point in time the whole group can be restored to \u2014 this is what a DR application based on this group recovers from."))), g.replicationConfig && /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Replication cadence", /*#__PURE__*/React.createElement("span", {
    className: "cnt",
    style: {
      marginLeft: 8,
      fontSize: 11,
      color: "var(--dim2)"
    }
  }, "owned by this group")), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "labels",
    style: {
      marginBottom: 8
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--accent)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "every"), g.replicationConfig.frequency, " min"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "generations"), g.replicationConfig.retention.reduce((a, r) => a + r.keep, 0)), g.replication && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: STATUS_META[g.replication.status].c,
      borderColor: `color-mix(in srgb,${STATUS_META[g.replication.status].c} 40%,transparent)`
    }
  }, /*#__PURE__*/React.createElement("i", null, "status"), g.replication.status), g.replication && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "backlog"), fmtBytes(g.replication.backlog))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "All ", g.counts.volumes, " members replicate as one set: a group snapshot is taken every ", g.replicationConfig.frequency, " minutes, so the target always holds a common point in time across the group rather than a mixture. ", (g.drPolicyIds || []).length ? `${g.drPolicyIds.length} DR polic${g.drPolicyIds.length === 1 ? "y ships" : "ies ship"} this group to ${g.drPolicyIds.length === 1 ? "its" : "their"} paired cluster${g.drPolicyIds.length === 1 ? "" : "s"} on this cadence.` : "No DR policy names this group yet, so nothing is being shipped — create one under Disaster recovery.", " The group's status is the worst of its members'", g.replication && g.replication.lastAt ? `; the oldest member last synced at ${clockOf(g.replication.lastAt)}` : "", ".")))));
}

// DR applications whose PVC set is this group.
function CgAppsCard({
  g,
  nav
}) {
  const {
    data
  } = useResource("cgapps|" + g.id, () => api.cgroupApps(g.id), 8000);
  const apps = data || [];
  if (!apps.length) return null;
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Protected applications"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, "based on this group")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Application"), /*#__PURE__*/React.createElement("th", null, "Namespace"), /*#__PURE__*/React.createElement("th", null, "Phase"), /*#__PURE__*/React.createElement("th", null, "Plan"), /*#__PURE__*/React.createElement("th", null, "Orchestrated"), /*#__PURE__*/React.createElement("th", null, "Active site"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, apps.map(a => /*#__PURE__*/React.createElement("tr", {
    key: a.id
  }, /*#__PURE__*/React.createElement("td", {
    style: {
      fontWeight: 600
    }
  }, a.name), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, a.namespace), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
    status: a.phase,
    sm: true,
    label: true
  })), /*#__PURE__*/React.createElement("td", null, a.planName || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, a.orchestratedMethod || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, a.activeSite || "—"), /*#__PURE__*/React.createElement("td", {
    style: {
      textAlign: "right"
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => nav.openProtectedApp(a.id)
  }, "Open"))))))), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      margin: 0
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "These applications take their crash-consistent boundary from this group: every member volume fails over together. The group cannot be deleted or have members removed while an application is based on it."))));
}
function CgroupDetail({
  o: g,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: g,
    title: g.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "consistency group"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, g.counts.volumes, " volumes"), g.locked && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ro)",
        borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"
      }
    }, "membership fixed")),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "in ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(g.clusterId),
      label: regName(g.clusterId)
    }))
  }), g.status !== "online" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Group not fully online."), " A group snapshot is only crash-consistent if every member volume is online at the moment it is taken.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Volumes",
    v: g.counts.volumes
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Provisioned",
    v: fmtBytes(g.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Utilized",
    v: fmtBytes(g.capacity.used),
    s: pct(g.capacity.used, g.capacity.total).toFixed(0) + "%"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Group snapshots",
    v: g.counts.snapshots
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backup policy",
    v: g.backupPolicy ? "yes" : "no",
    c: g.backupPolicy ? "var(--ok)" : undefined,
    s: g.backupPolicy ? g.backupPolicy.policy_name : "members not backed up as a group"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Replication",
    v: g.replicationConfig ? "yes" : "no",
    c: g.replication ? STATUS_META[g.replication.status].c : undefined,
    s: g.replicationConfig ? `every ${g.replicationConfig.frequency} min` : "no cadence set"
  })), g.locked && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--ro)",
      background: "color-mix(in srgb,var(--ro) 7%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "lock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Membership is fixed."), " This group carries ", [g.backupPolicy && "a backup policy", g.replicationConfig && "a replication cadence"].filter(Boolean).join(" and "), ", and every retained version and replica stream is defined against exactly these ", g.counts.volumes, " volumes. Volumes cannot be added or removed while ", g.backupPolicy && g.replicationConfig ? "they are" : "it is", " attached \u2014 detach ", g.backupPolicy && g.replicationConfig ? "them" : "it", " to change the members, or create a second group with the volumes you want. A volume can belong to more than one group.")), /*#__PURE__*/React.createElement(CgProtection, {
    g: g,
    nav: nav
  }), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Member volumes",
    sub: g.locked ? "fixed — a policy is attached" : "add or remove members",
    count: g.counts.volumes,
    onClick: () => nav.layer(g, "volumes")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "camera",
    title: "Group snapshots",
    sub: "crash-consistent across members",
    count: g.counts.snapshots,
    onClick: () => nav.layer(g, "cgsnapshots")
  }), g.backupPolicy && /*#__PURE__*/React.createElement(NavCard, {
    icon: "cloud",
    title: "Backup policy",
    sub: g.backupPolicy.policy_name,
    count: "\u2192",
    onClick: () => nav.openPolicy(g.clusterId, g.backupPolicy.uuid)
  }), !!(g.drPolicyIds || []).length && /*#__PURE__*/React.createElement(NavCard, {
    icon: "shield",
    title: "DR policies",
    sub: "ship this group to a paired cluster",
    count: g.drPolicyIds.length,
    onClick: () => nav.openRPolicy(g.drPolicyIds[0])
  })), /*#__PURE__*/React.createElement(CgAppsCard, {
    g: g,
    nav: nav
  }), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Group properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", g.name], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: g.status
    })], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(g.clusterId),
      label: regName(g.clusterId)
    })], ["Volumes", g.counts.volumes + (g.locked ? " · fixed while a policy is attached" : "")], ["Provisioned", fmtBytes(g.capacity.total)], ["Utilized", fmtBytes(g.capacity.used)], ["Backup policy", g.backupPolicy ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPolicy(g.clusterId, g.backupPolicy.uuid),
      label: g.backupPolicy.policy_name
    }) : null], ["Replication frequency", g.replicationConfig ? `every ${g.replicationConfig.frequency} min` : null], ["Retained generations", g.replicationConfig ? g.replicationConfig.retention.map(r => `${r.interval}×${r.keep}`).join(" ") || "latest only" : null], g.replication ? ["Replication status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: g.replication.status,
      label: true
    })] : null, g.replication ? ["Last group cycle", g.replication.lastAt ? `${clockOf(g.replication.lastAt)} · ${fmtAgo(g.replication.lastAt)}` : "—"] : null, g.replication ? ["Backlog", fmtBytes(g.replication.backlog)] : null, ["Created", fmtDate(g.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "What a consistency group is"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "A named set of volumes. Taking a ", /*#__PURE__*/React.createElement("b", null, "group snapshot"), " snapshots every member at the same instant, so they restore to one common point in time."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "A group also ", /*#__PURE__*/React.createElement("b", null, "owns its protection"), ". Attach a backup policy and every member is snapshotted and backed up together on that schedule, so any retained version is a point in time the whole group returns to. Give it a replication cadence and every member ships as one crash-consistent set \u2014 a DR policy then simply names this group, and takes its frequency and retention from here."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Because the group owns the policy, a volume joining it inherits both, and a volume leaving it gives them up. A DR application based on this group inherits the same boundary.")))));
}
function CgSnapshotDetail({
  o: s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: s,
    title: s.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "group snapshot"), s.backupVersionId ? /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"
      }
    }, "backed up \xB7 ", s.backupVersionId) : /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "not backed up")),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "of ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCgroup(s.cgId),
      label: s.cgName
    }), " \xB7 ", fmtDate(s.createdAt), " \xB7 ", fmtAgo(s.createdAt))
  }), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Members",
    v: s.counts.volumes,
    s: "one snapshot per volume"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Total size",
    v: fmtBytes(s.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Taken",
    v: fmtAgo(s.createdAt),
    s: fmtDate(s.createdAt)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backup",
    v: s.backupVersionId || "none",
    c: s.backupVersionId ? "var(--ok)" : undefined
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Member snapshots"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, "all taken at the same instant")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Volume"), /*#__PURE__*/React.createElement("th", null, "Snapshot id"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Delta size"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, s.members.map(m => /*#__PURE__*/React.createElement("tr", {
    key: m.snapshotId
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600
    }
  }, m.volumeName), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, shortId(m.snapshotId)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(m.size)), /*#__PURE__*/React.createElement("td", {
    style: {
      textAlign: "right"
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => nav.openSnapshot(s.clusterId, m.snapshotId)
  }, "Open snapshot")))))))), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Snapshot properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", s.name], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: s.status
    })], ["Consistency group", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCgroup(s.cgId),
      label: s.cgName
    })], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(s.clusterId),
      label: regName(s.clusterId)
    })], ["Created", fmtDate(s.createdAt)], ["Members", s.counts.volumes], ["Total size", fmtBytes(s.capacity.total)], ["Backup version", s.backupVersionId || null], ["Bucket", s.bucket || null]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Restoring"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Restoring this group snapshot creates ", /*#__PURE__*/React.createElement("b", null, "one new volume per member"), ", all at the same point in time. The target can be this cluster or any other cluster the control plane manages."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Individual member snapshots can also be restored on their own from the snapshot page \u2014 a single volume, same choice of target cluster.")))));
}
Object.assign(window, {
  CgroupTile,
  CgSnapshotTile,
  CgroupDetail,
  CgSnapshotDetail,
  CgProtection,
  CgAppsCard
});
})();
// ---- migrations.jsx ----
(function(){
// ---------------------------------------------------------------------------
// CLUSTER / VOLUME MIGRATION
// Two mechanisms behind one object:
//  · intra_cluster — volumes move between nodes on a zone by instant migration
//  · cross_cluster — asynchronous replication ships the data, then a short IO
//    freeze applies the last small snapshot and rolls the NVMe paths over
// Operators taint the destination hosts and switch on "follow the workload".
// ---------------------------------------------------------------------------
const MIG_MODE = m => m === "cross_cluster" ? /*#__PURE__*/React.createElement("span", {
  className: "badge k8s"
}, "between clusters") : /*#__PURE__*/React.createElement("span", {
  className: "badge"
}, "within cluster");
function MigrationTile({
  m,
  nav
}) {
  const cross = m.mode === "cross_cluster";
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[m.status].c
    },
    onDoubleClick: () => nav.detail(m)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: m,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: m.status
    }), /*#__PURE__*/React.createElement(Name, null, m.name)),
    right: MIG_MODE(m.mode)
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: m.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openCluster(m.sourceClusterId);
    }
  }, /*#__PURE__*/React.createElement("i", null, "from"), regName(m.sourceClusterId)), cross ? /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openCluster(m.targetClusterId);
    }
  }, /*#__PURE__*/React.createElement("i", null, "to"), regName(m.targetClusterId)) : /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "to zone"), m.targetZoneId ? regName(m.targetZoneId, "zone") : "tainted hosts"), m.followWorkload && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), "follows workload")), /*#__PURE__*/React.createElement("div", {
    className: "capwrap"
  }, /*#__PURE__*/React.createElement("div", {
    className: "caprow"
  }, /*#__PURE__*/React.createElement("span", null, "Progress ", /*#__PURE__*/React.createElement("b", {
    className: "mono",
    style: {
      color: "var(--accent)",
      fontWeight: 600
    }
  }, m.progress, "%")), /*#__PURE__*/React.createElement("span", {
    className: "capval"
  }, m.counts.moved, " / ", m.counts.volumes, " volumes")), /*#__PURE__*/React.createElement("div", {
    className: "bar"
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: m.progress + "%",
      "--bc": STATUS_META[m.status].c
    }
  }))), cross ? /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Iteration"), /*#__PURE__*/React.createElement("b", null, m.iterations, " / ", m.iterationLimit)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Outstanding"), /*#__PURE__*/React.createElement("b", null, fmtBytes(m.lastSnapshot))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Freeze est."), /*#__PURE__*/React.createElement("b", null, m.freezeMs, " ms")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Shipping"), /*#__PURE__*/React.createElement("b", null, fmtBW(m.throughput), " GB/s"))) : /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Mechanism"), /*#__PURE__*/React.createElement("b", null, "instant")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Moved"), /*#__PURE__*/React.createElement("b", null, m.counts.moved)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Started"), /*#__PURE__*/React.createElement("b", null, fmtAgo(m.startedAt)))), m.readyToCutover && /*#__PURE__*/React.createElement("div", {
    className: "prepbox ready"
  }, "Outstanding snapshot is under the freeze threshold \u2014 ready to cut over."), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Volumes",
      count: m.counts.volumes,
      icon: "volume",
      onClick: () => nav.layer(m, "volumes")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(m)
    }]
  }));
}
function MigrationDetail({
  o: m,
  nav
}) {
  const cross = m.mode === "cross_cluster";
  const shrink = m.firstSnapshot && m.lastSnapshot ? (m.firstSnapshot / m.lastSnapshot).toFixed(0) : null;
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: m,
    title: m.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, MIG_MODE(m.mode), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, m.scope === "cluster" ? "whole cluster" : "selected volumes"), m.followWorkload && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ro)",
        borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"
      }
    }, "follows workload")),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(m.sourceClusterId),
      label: regName(m.sourceClusterId)
    }), " → ", cross ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(m.targetClusterId),
      label: regName(m.targetClusterId)
    }) : m.targetZoneId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openZone(m.targetZoneId),
      label: regName(m.targetZoneId, "zone")
    }) : "tainted hosts")
  }), m.readyToCutover && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Ready to cut over."), " The outstanding snapshot is ", fmtBytes(m.lastSnapshot), ", inside the ", fmtBytes(m.freezeThreshold), " freeze threshold. Cutting over freezes IO for roughly ", m.freezeMs, " ms while the last delta is applied and the NVMe paths roll over.")), m.status === "completed" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--ok)",
      background: "color-mix(in srgb,var(--ok) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Migration complete."), " ", m.counts.moved, " volume(s) now serve from the target.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Progress",
    v: m.progress + "%"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Volumes",
    v: `${m.counts.moved}/${m.counts.volumes}`,
    s: "moved"
  }), cross ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Stat, {
    k: "Iteration",
    v: `${m.iterations}/${m.iterationLimit}`,
    s: "snapshot rounds"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Outstanding",
    v: fmtBytes(m.lastSnapshot),
    s: `threshold ${fmtBytes(m.freezeThreshold)}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Freeze estimate",
    v: m.freezeMs + " ms",
    c: m.freezeMs > 1000 ? "var(--warn)" : "var(--ok)"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Shipping",
    v: fmtBW(m.throughput),
    s: "GB/s"
  })) : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Stat, {
    k: "Mechanism",
    v: "instant",
    s: "no data copied"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Started",
    v: fmtAgo(m.startedAt),
    s: fmtDate(m.startedAt)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Follow workload",
    v: m.followWorkload ? "on" : "off"
  }))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Volumes in this migration",
    sub: "per-volume placement",
    count: m.counts.volumes,
    onClick: () => nav.layer(m, "volumes")
  }), cross && /*#__PURE__*/React.createElement(NavCard, {
    icon: "swap",
    title: "Cluster pair",
    sub: "the link data ships over",
    count: "\u2192",
    onClick: () => nav.drLayer("pairs")
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Migration properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", m.name], ["State", /*#__PURE__*/React.createElement(TrafficLight, {
      status: m.status
    })], ["Mode", cross ? "between clusters / zones" : "within the cluster"], ["Scope", m.scope === "cluster" ? "every online volume" : `${m.counts.volumes} selected volume(s)`], ["Source cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(m.sourceClusterId),
      label: regName(m.sourceClusterId)
    })], cross ? ["Target cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(m.targetClusterId),
      label: regName(m.targetClusterId)
    })] : null, m.targetZoneId ? ["Target zone", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openZone(m.targetZoneId),
      label: regName(m.targetZoneId, "zone")
    })] : null, ["Target taint", m.targetTaint], ["Follow the workload", m.followWorkload ? "on — storage rolls over when the workload moves" : "off"], ["Started", fmtDate(m.startedAt)], m.completedAt ? ["Completed", `${fmtDate(m.completedAt)} · ${fmtAgo(m.completedAt)}`] : null, m.frozenAt ? ["IO frozen at", fmtDate(m.frozenAt)] : null, m.error ? ["Error", m.error] : null]
  }))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "How this migration works"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, cross ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Data ships over the cluster pair by asynchronous replication. Snapshots are taken ", /*#__PURE__*/React.createElement("b", null, "iteratively"), ", each one covering less change than the last", shrink ? ` — ${fmtBytes(m.firstSnapshot)} down to ${fmtBytes(m.lastSnapshot)}, a factor of ${shrink}` : "", "."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Once the outstanding snapshot is small enough, cutting over ", /*#__PURE__*/React.createElement("b", null, "freezes IO briefly"), " (", m.freezeMs, " ms at the current size), applies that last delta, and rolls the NVMe paths over to the target. Clients reconnect to the new target."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Taint the destination hosts and switch on ", /*#__PURE__*/React.createElement("b", null, "follow the workload"), " to have storage roll over as the workload is rescheduled.")) : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Volumes move between nodes with ", /*#__PURE__*/React.createElement("b", null, "instant migration"), ": the primary role is handed to another node without copying data, so each volume moves in one step."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Targets are the hosts you tainted (", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, m.targetTaint), ")", m.targetZoneId ? /*#__PURE__*/React.createElement(React.Fragment, null, " in zone ", /*#__PURE__*/React.createElement("b", null, regName(m.targetZoneId, "zone"))) : null, ". With ", /*#__PURE__*/React.createElement("b", null, "follow the workload"), " on, front storage keeps moving as pods are rescheduled.")))), cross && /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Convergence"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(AllocBar, {
    label: "Outstanding snapshot vs freeze threshold",
    used: Math.min(m.lastSnapshot, m.freezeThreshold * 4),
    total: m.freezeThreshold * 4,
    color: "var(--accent)"
  }), /*#__PURE__*/React.createElement("div", {
    className: "kv",
    style: {
      marginTop: 6
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "First snapshot"), /*#__PURE__*/React.createElement("b", null, fmtBytes(m.firstSnapshot))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Latest"), /*#__PURE__*/React.createElement("b", null, fmtBytes(m.lastSnapshot))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Threshold"), /*#__PURE__*/React.createElement("b", null, fmtBytes(m.freezeThreshold)))))))));
}
Object.assign(window, {
  MigrationTile,
  MigrationDetail,
  MIG_MODE
});
})();
// ---- k8s.jsx ----
(function(){
// ---------------------------------------------------------------------------
// KUBERNETES — cluster, storage classes, PVCs.
// A Kubernetes cluster spans one or more zones and provides the worker nodes
// that become simplyblock hosts. Its storage classes point at pools in storage
// clusters, so k8s ↔ storage cluster is many-to-many. Each PVC is provisioned
// as one logical volume, which carries a back-reference to the PVC.
// ---------------------------------------------------------------------------
const annList = a => Object.entries(a || []);
// StoragePool.spec.storageClassParameters (camelCase) -> CSI StorageClass parameter
const SCP_FIELD = {
  qosRwIops: "qos_rw_iops",
  qosRwMbytes: "qos_rw_mbytes",
  qosRMbytes: "qos_r_mbytes",
  qosWMbytes: "qos_w_mbytes",
  compression: "compression",
  encryption: "encryption",
  fabric: "fabric",
  maxNamespacePerSubsys: "max_namespace_per_subsys",
  tune2fsReservedBlocks: "tune2fs_reserved_blocks",
  filesystem: "csi.storage.k8s.io/fstype"
};
function K8sTile({
  k,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[k.status].c
    },
    onDoubleClick: () => nav.detail(k)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: k,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: k.status
    }), /*#__PURE__*/React.createElement(Name, null, k.name)),
    right: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, k.environment), k.counts.workers ? /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: k.discovered ? {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"
      } : {
        color: "var(--dim2)"
      }
    }, k.discovered ? "discovered" : "not discovered") : null)
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, k.endpoint), /*#__PURE__*/React.createElement(Uuid, {
    value: k.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "k8s"), k.version), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "csi"), k.csi.version, /*#__PURE__*/React.createElement(Dot, {
    c: STATUS_META[k.csi.status === "online" ? "online" : k.csi.status === "degraded" ? "degraded" : "unreachable"].c
  })), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "namespaces"), k.namespaces.length || "—")), /*#__PURE__*/React.createElement("div", {
    className: "zonestrip"
  }, k.zoneIds.map(id => /*#__PURE__*/React.createElement("button", {
    className: "zonechip",
    key: id,
    onClick: e => {
      e.stopPropagation();
      nav.openZone(id);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "zone",
    s: 10
  }), regName(id, "zone")))), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Worker nodes"), /*#__PURE__*/React.createElement("b", null, k.counts.workers || "—")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Prepared"), /*#__PURE__*/React.createElement("b", null, k.counts.prepared || "—")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Storage clusters"), /*#__PURE__*/React.createElement("b", null, k.counts.storageClusters)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Provisioned"), /*#__PURE__*/React.createElement("b", null, fmtBytes(k.capacity.total)))), !k.counts.workers && /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "Disaggregated \u2014 consumes storage over NVMe/TCP, runs no simplyblock storage node"), !!k.counts.workers && !k.discovered && /*#__PURE__*/React.createElement("div", {
    className: "prepbox"
  }, "Worker nodes are listed, but their hardware is unknown. Run discovery before a storage cluster can be deployed here."), /*#__PURE__*/React.createElement(Foot, {
    items: [k.counts.workers && !k.discovered ? {
      label: "Discover",
      icon: "search",
      onClick: () => window.__ui.dialog(discoveryDialog(k), k)
    } : null, k.discovered ? {
      label: "Deploy",
      icon: "plus",
      onClick: () => nav.deployWizard(k.id)
    } : null, {
      label: "PVCs",
      count: k.counts.pvcs,
      icon: "volume",
      onClick: () => nav.layer(k, "pvcs")
    }, {
      label: "Classes",
      count: k.counts.storageClasses,
      icon: "pool",
      onClick: () => nav.layer(k, "storageclasses")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(k)
    }]
  }));
}
function StorageClassTile({
  s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": "var(--ok)"
    },
    onDoubleClick: () => nav.detail(s)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: s,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: "active"
    }), /*#__PURE__*/React.createElement(Name, null, s.name)),
    right: /*#__PURE__*/React.createElement(React.Fragment, null, s.variant !== "default" && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, s.variant), s.isDefault ? /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, "default class") : /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "generated"))
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, s.provisioner), /*#__PURE__*/React.createElement(Uuid, {
    value: s.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openCluster(s.clusterId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 10
  }), regName(s.clusterId)), s.poolId && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openPool(s.clusterId, s.poolId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "pool",
    s: 10
  }), "StoragePool ", s.poolName), s.dhchap && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "lock",
    s: 10
  }), "dhchap")), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, annList(s.parameters).filter(([k2]) => k2 !== "cluster_id" && k2 !== "pool_name").slice(0, 6).map(([k2, v2]) => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: k2,
    title: k2 + "=" + v2
  }, /*#__PURE__*/React.createElement("i", null, k2.replace("csi.storage.k8s.io/", "")), v2))), /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "Generated by the operator from the StoragePool \u2014 parameters are immutable"), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "PVCs"), /*#__PURE__*/React.createElement("b", null, s.counts.pvcs)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Bound"), /*#__PURE__*/React.createElement("b", null, s.counts.bound)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Provisioned"), /*#__PURE__*/React.createElement("b", null, fmtBytes(s.capacity.total))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Reclaim"), /*#__PURE__*/React.createElement("b", null, s.reclaim))), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "PVCs",
      count: s.counts.pvcs,
      icon: "volume",
      onClick: () => nav.layer(s, "pvcs")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(s)
    }]
  }));
}
function PvcTile({
  p,
  nav
}) {
  const anns = annList(p.annotations).filter(([k2]) => !k2.startsWith("volume.kubernetes.io"));
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[p.status].c
    },
    onDoubleClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: p,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    }), /*#__PURE__*/React.createElement(Name, null, p.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, p.namespace)
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: p.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openStorageClass(p.storageClassId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "pool",
    s: 10
  }), p.storageClass), p.volumeId ? /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openVolumeById(p.volumeId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "volume",
    s: 10
  }), "volume") : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--warn)",
      borderColor: "color-mix(in srgb,var(--warn) 40%,transparent)"
    }
  }, "no volume bound"), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, p.workloadKind.toLowerCase()), p.workload), p.accessMode === "ReadWriteMany" && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    },
    title: "Served by pNFS \u2014 XFS only"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "folder",
    s: 10
  }), "RWX \xB7 pNFS")), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Requested"), /*#__PURE__*/React.createElement("b", null, fmtBytes(p.requested))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Access"), /*#__PURE__*/React.createElement("b", null, p.accessMode)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Filesystem"), /*#__PURE__*/React.createElement("b", null, p.filesystem || p.volumeMode.toLowerCase())), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Age"), /*#__PURE__*/React.createElement("b", null, fmtAgo(p.createdAt)))), anns.length > 0 && /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, anns.slice(0, 3).map(([k2, v2]) => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: k2,
    title: `${k2}=${v2}`
  }, /*#__PURE__*/React.createElement("i", null, k2.split("/").pop()), v2)), anns.length > 3 && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, "+", anns.length - 3)), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(p)
    }]
  }));
}
function K8sDetail({
  o: k,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: k,
    title: k.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, "kubernetes ", k.version), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, k.environment)),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, k.endpoint)
  }), k.csi.status !== "online" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "CSI driver ", k.csi.status, "."), " Provisioning and attach operations in this cluster will fail until the driver recovers.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Worker nodes",
    v: k.counts.workers || "—",
    s: k.counts.workers ? `${k.counts.prepared} prepared as hosts` : "disaggregated deployment"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "PVCs",
    v: k.counts.pvcs,
    s: `${k.counts.bound} bound`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Storage classes",
    v: k.counts.storageClasses
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Storage clusters",
    v: k.counts.storageClusters,
    s: "consumed"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Zones",
    v: k.counts.zones
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Provisioned",
    v: fmtBytes(k.capacity.total)
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Persistent volume claims",
    sub: "filter by class or annotation",
    count: k.counts.pvcs,
    onClick: () => nav.layer(k, "pvcs")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "pool",
    title: "Storage classes",
    sub: "provisioner parameters",
    count: k.counts.storageClasses,
    onClick: () => nav.layer(k, "storageclasses")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "host",
    title: "Worker nodes",
    sub: k.counts.workers ? "hosts provided to simplyblock" : "none — storage is disaggregated",
    count: k.counts.workers,
    onClick: () => nav.layer(k, "hosts")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "search",
    title: "Discovery & deployment",
    sub: k.discovered ? `discovered ${fmtAgo(k.discoveredAt)} · deploy a cluster` : "not discovered yet",
    count: "\u2192",
    onClick: () => nav.discovery(k.id)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "zone",
    title: "Zones",
    sub: "where this cluster runs",
    count: k.counts.zones,
    onClick: () => nav.layer(k, "zones")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "k8s",
    title: "Protected applications",
    sub: "Ramen DR",
    count: k.counts.protectedApps,
    onClick: () => nav.drLayer("protectedapps")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "cluster",
    title: "Storage clusters",
    sub: "serving this k8s cluster",
    count: k.counts.storageClusters,
    onClick: () => nav.layer(k, "clusters")
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Cluster properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", k.name], ["Kubernetes", k.version], ["Environment", k.environment], ["API endpoint", k.endpoint], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: k.status
    })], ["CSI driver", `${k.csi.version} · ${k.csi.status}`], ["Operator namespace", k.operatorNamespace], ["Zones", k.zoneIds.length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        gap: 8,
        justifyContent: "flex-end",
        flexWrap: "wrap"
      }
    }, k.zoneIds.map(id => /*#__PURE__*/React.createElement(Ref, {
      key: id,
      onClick: () => nav.openZone(id),
      label: regName(id, "zone")
    }))) : null], ["Storage clusters", k.storageClusterIds.length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        gap: 8,
        justifyContent: "flex-end",
        flexWrap: "wrap"
      }
    }, k.storageClusterIds.map(id => /*#__PURE__*/React.createElement(Ref, {
      key: id,
      onClick: () => nav.openCluster(id),
      label: regName(id)
    }))) : null], ["Namespaces with PVCs", k.namespaces.join(", ") || null], ["Registered", fmtDate(k.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "How this fits together"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "The Kubernetes cluster runs in one or more ", /*#__PURE__*/React.createElement("b", null, "zones"), " and its worker nodes are the machines simplyblock prepares as ", /*#__PURE__*/React.createElement("b", null, "hosts"), ". A cluster with no worker nodes of its own is ", /*#__PURE__*/React.createElement("b", null, "disaggregated"), ": it consumes storage over NVMe/TCP from a storage cluster running elsewhere."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Each ", /*#__PURE__*/React.createElement("b", null, "storage class"), " points at a pool in a storage cluster, so this cluster can consume several storage clusters and one storage cluster can serve several Kubernetes clusters."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Every ", /*#__PURE__*/React.createElement("b", null, "PVC"), " is provisioned as one logical volume; the volume carries a back-reference to its claim.")))));
}
function StorageClassDetail({
  o: s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: s,
    title: s.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "storage class"), s.isDefault && /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, "default")),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, s.provisioner)
  }), /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--dim)",
      background: "var(--panel2)",
      borderColor: "var(--line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Generated by the operator."), " This StorageClass was created automatically when the StoragePool ", /*#__PURE__*/React.createElement("b", null, s.storagePoolRef || s.poolName), " became active. A class always belongs to exactly one pool, and a pool can back several classes offering different defaults over the same capacity. Kubernetes does not allow StorageClass parameters to change after creation, so they are immutable: to provision with different defaults, add another class or a new storage pool.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "PVCs",
    v: s.counts.pvcs,
    s: `${s.counts.bound} bound`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Provisioned",
    v: fmtBytes(s.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Reclaim policy",
    v: s.reclaim
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Binding",
    v: s.binding === "Immediate" ? "Immediate" : "WaitForConsumer"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Expansion",
    v: s.expansion ? "allowed" : "blocked"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Fabric",
    v: (s.parameters || {}).fabric || "tcp",
    s: (s.parameters || {})["csi.storage.k8s.io/fstype"] || "raw block"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "PVCs using this class",
    sub: "all namespaces",
    count: s.counts.pvcs,
    onClick: () => nav.layer(s, "pvcs")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "pool",
    title: "Source StoragePool",
    sub: regName(s.clusterId),
    count: "\u2192",
    onClick: () => nav.openPool(s.clusterId, s.poolId)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "k8s",
    title: "Kubernetes cluster",
    sub: regName(s.k8sClusterId, "cluster"),
    count: "\u2192",
    onClick: () => nav.openK8s(s.k8sClusterId)
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Class properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", s.name], ["Provisioner", s.provisioner], ["Source StoragePool", s.storagePoolRef], ["Variant", s.variant], ["Kubernetes cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openK8s(s.k8sClusterId),
      label: regName(s.k8sClusterId, "cluster")
    })], ["Storage cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(s.clusterId),
      label: regName(s.clusterId)
    })], ["Pool", s.poolId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPool(s.clusterId, s.poolId),
      label: s.poolName
    }) : null], ["Reclaim policy", s.reclaim], ["Volume binding mode", s.binding], ["Allow expansion", s.expansion ? "yes" : "no"], ["Default class", s.isDefault ? "yes" : "no"], ["DHCHAP", s.dhchap ? "enabled — restricted to the pool's allowed nodes" : "no"], s.allowedTopology ? ["Allowed topology", s.allowedTopology] : null, ["Created", fmtDate(s.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Topology-aware provisioning"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, Object.keys(s.zoneClusterMap || {}).length || Object.keys(s.regionClusterMap || {}).length ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "A PVC using this class is provisioned from the storage cluster mapped to the pod's ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "topology.kubernetes.io/zone"), " label, falling back to its region."), /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Label"), /*#__PURE__*/React.createElement("th", null, "Value"), /*#__PURE__*/React.createElement("th", null, "Storage cluster"))), /*#__PURE__*/React.createElement("tbody", null, Object.entries(s.zoneClusterMap || {}).map(([z, cid]) => /*#__PURE__*/React.createElement("tr", {
    key: "z" + z
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, "zone"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, z), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(Ref, {
    onClick: () => nav.openCluster(cid),
    label: regName(cid)
  })))), Object.entries(s.regionClusterMap || {}).map(([r, cid]) => /*#__PURE__*/React.createElement("tr", {
    key: "r" + r
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, "region"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, r), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(Ref, {
    onClick: () => nav.openCluster(cid),
    label: regName(cid)
  }))))))) : /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "No zone or region map \u2014 every PVC lands on ", /*#__PURE__*/React.createElement("b", null, regName(s.clusterId)), " regardless of where the pod is scheduled."))), /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "StorageClass parameters \xB7 immutable"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "CSI parameter"), /*#__PURE__*/React.createElement("th", null, "Value"), /*#__PURE__*/React.createElement("th", null, "StoragePool field"))), /*#__PURE__*/React.createElement("tbody", null, annList(s.parameters).map(([k2, v2]) => {
    const crd = Object.entries(SCP_FIELD).find(([, csi]) => csi === k2);
    const fixed = k2 === "cluster_id" || k2 === "pool_name";
    return /*#__PURE__*/React.createElement("tr", {
      key: k2
    }, /*#__PURE__*/React.createElement("td", {
      className: "mono",
      style: {
        color: "var(--dim)",
        overflowWrap: "anywhere",
        whiteSpace: "normal"
      }
    }, k2), /*#__PURE__*/React.createElement("td", {
      className: "mono",
      style: {
        overflowWrap: "anywhere",
        whiteSpace: "normal"
      }
    }, v2), /*#__PURE__*/React.createElement("td", {
      className: "mono",
      style: {
        color: fixed ? "var(--warn)" : "var(--dim2)",
        fontSize: 10.5
      }
    }, fixed ? "set from the pool" : crd ? crd[0] : "operator"));
  })))))));
}
function PvcDetail({
  o: p,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: p,
    title: `${p.namespace}/${p.name}`,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "PVC"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, p.accessMode), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, p.volumeMode)),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "class ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openStorageClass(p.storageClassId),
      label: p.storageClass
    }))
  }), p.status === "Lost" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Claim lost."), " The logical volume backing this PVC no longer exists, so the workload cannot mount it.")), p.status === "Pending" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Pending."), " No volume has been provisioned yet \u2014 with WaitForFirstConsumer binding this is normal until the pod is scheduled.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Requested",
    v: fmtBytes(p.requested)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Provisioned",
    v: p.volumeId ? fmtBytes(p.capacity.total) : "—"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Namespace",
    v: p.namespace
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Access mode",
    v: p.accessMode === "ReadWriteMany" ? "RWX" : p.accessMode === "ReadWriteOnce" ? "RWO" : "RWOP",
    s: p.accessMode === "ReadWriteMany" ? "pNFS, XFS only" : p.accessMode
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Age",
    v: fmtAgo(p.createdAt),
    s: fmtDate(p.createdAt)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Status",
    v: p.status,
    c: STATUS_META[p.status].c
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, p.volumeId && /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Logical volume",
    sub: "the volume behind this claim",
    count: "\u2192",
    onClick: () => nav.openVolumeById(p.volumeId)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "pool",
    title: "Storage class",
    sub: p.storageClass,
    count: "\u2192",
    onClick: () => nav.openStorageClass(p.storageClassId)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "k8s",
    title: "Kubernetes cluster",
    sub: regName(p.k8sClusterId, "cluster"),
    count: "\u2192",
    onClick: () => nav.openK8s(p.k8sClusterId)
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Claim properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", p.name], ["Namespace", p.namespace], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    })], ["Storage class", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openStorageClass(p.storageClassId),
      label: p.storageClass
    })], ["Logical volume", p.volumeId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openVolumeById(p.volumeId),
      label: regName(p.volumeId, shortId(p.volumeId))
    }) : null], ["Kubernetes cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openK8s(p.k8sClusterId),
      label: regName(p.k8sClusterId, "cluster")
    })], ["Requested", fmtBytes(p.requested)], ["Access mode", p.accessMode], ["Volume mode", p.volumeMode], ["Filesystem", p.filesystem], p.accessMode === "ReadWriteMany" ? ["Served by", "pNFS — the cluster's file storage. XFS is the only filesystem the Linux NFS server supports for pNFS."] : null, ["Workload", `${p.workload} (${p.workloadKind})`], ["Created", fmtDate(p.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Annotations"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("tbody", null, annList(p.annotations).map(([k2, v2]) => /*#__PURE__*/React.createElement("tr", {
    key: k2
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)",
      overflowWrap: "anywhere",
      whiteSpace: "normal"
    }
  }, k2), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, v2))))))), /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Labels"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, annList(p.labels).map(([k2, v2]) => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: k2
  }, /*#__PURE__*/React.createElement("i", null, k2.split("/").pop()), v2))))))));
}
Object.assign(window, {
  K8sTile,
  StorageClassTile,
  PvcTile,
  K8sDetail,
  StorageClassDetail,
  PvcDetail,
  SCP_FIELD
});
})();
// ---- deploy.jsx ----
(function(){
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
// ---------------------------------------------------------------------------
// DISCOVERY AND CLUSTER DEPLOYMENT — discovery panel and the wizard.
// Control plane (Helm) and operator install happen outside this console.
// Discovery makes a Kubernetes cluster "discovered"; only then can a storage
// cluster be deployed on it. The wizard writes a draft document; approval
// (on the document) starts the three asynchronous steps.
// ---------------------------------------------------------------------------
const GBn = 1e9;
const gb = n => Math.round((n || 0) / GBn);
const devLabel = d => d.kind === "nvme" ? d.pcie || d.blockdev : d.blockdev;
const csv = s => String(s || "").split(",").map(x => x.trim()).filter(Boolean);
const up = (set, patch) => set(s => Object.assign({}, s, patch));

// ---- discovery -------------------------------------------------------------
const discoveryDialog = (k, prev) => ({
  title: `Discover ${k.name}`,
  confirm: "Run discovery",
  desc: "The operator deploys an inspection pod on every matching worker node and reports NUMA topology, vCPU, memory, network interfaces and every unmounted, unused device. Filters apply during discovery — an excluded device is never reported. Runs asynchronously; the cluster is marked discovered when it completes.",
  fields: v => [{
    k: "tag_key",
    label: "Only nodes with this label",
    type: "text",
    def: prev && Object.keys(prev.nodeSelector || {})[0] || "",
    placeholder: "simplyblock.io/storage-node-candidate"
  }, v.tag_key ? {
    k: "tag_value",
    label: "Label value",
    type: "text",
    def: "true"
  } : null, {
    k: "pcie_allow",
    label: "PCIe addresses — allow",
    type: "text",
    def: (prev && prev.filter.pcieAllowList || []).join(", "),
    placeholder: "0000:5e:, 0000:5f:  (prefixes, comma-separated)"
  }, {
    k: "pcie_deny",
    label: "PCIe addresses — deny",
    type: "text",
    def: (prev && prev.filter.pcieDenyList || []).join(", "),
    placeholder: "0000:00:04.0"
  }, {
    k: "blockdev",
    label: "Block device name pattern",
    type: "text",
    def: (prev && prev.filter.blockDeviceNames || []).join(", "),
    placeholder: "nvme*, sd[b-f]  (glob, comma-separated)"
  }, {
    k: "models",
    label: "Device model contains",
    type: "text",
    def: (prev && prev.filter.models || []).join(", "),
    placeholder: "PM9A3, CD8"
  }, {
    k: "size_min",
    label: "Capacity — smallest",
    unit: "GB",
    type: "number",
    min: 0,
    def: prev && prev.filter.driveSizeRange ? gb(prev.filter.driveSizeRange.min) : 400
  }, {
    k: "size_max",
    label: "Capacity — largest (0 = no limit)",
    unit: "GB",
    type: "number",
    min: 0,
    def: prev && prev.filter.driveSizeRange ? gb(prev.filter.driveSizeRange.max) : 0
  }, {
    k: "n2",
    type: "note",
    label: "Leave a filter empty to skip it. All filters combine; a device must pass every one to be reported."
  }].filter(Boolean),
  run: v => api.discoveryRun(k.id, k.name, {
    pcieAllowList: csv(v.pcie_allow),
    pcieDenyList: csv(v.pcie_deny),
    blockDeviceNames: csv(v.blockdev),
    models: csv(v.models),
    driveSizeRange: {
      min: Number(v.size_min || 0) * GBn,
      max: Number(v.size_max || 0) * GBn
    },
    enableLogicalBlockDevices: true
  }, v.tag_key ? {
    matchLabels: {
      [v.tag_key]: v.tag_value || "true"
    }
  } : {
    matchLabels: {}
  })
});

// "Deploy cluster" from the clusters overview: pick a discovered Kubernetes cluster
const deployFromDialog = nav => ({
  title: "Deploy a storage cluster",
  confirm: "Continue",
  desc: "A storage cluster is deployed onto the worker nodes of a discovered Kubernetes cluster. Undiscovered clusters are not offered — run discovery on them first.",
  fields: [{
    k: "kid",
    label: "Kubernetes cluster",
    type: "select",
    required: true,
    load: () => api.k8sClusters().then(ks => ks.filter(k => k.discovered).map(k => ({
      v: k.id,
      l: `${k.name} · discovered ${fmtAgo(k.discoveredAt)}`
    }))),
    empty: "No Kubernetes cluster has been discovered yet."
  }],
  run: v => {
    nav.deployWizard(v.kid);
    return Promise.resolve({});
  }
});
const FilterProps = ({
  f
}) => /*#__PURE__*/React.createElement(Props, {
  rows: [["PCIe allow", (f.pcieAllowList || []).length ? f.pcieAllowList.join(", ") : "—"], ["PCIe deny", (f.pcieDenyList || []).length ? f.pcieDenyList.join(", ") : "—"], ["Block device pattern", (f.blockDeviceNames || []).length ? f.blockDeviceNames.join(", ") : "—"], ["Model contains", (f.models || []).length ? f.models.join(", ") : "—"], ["Capacity", `${(f.driveSizeRange || {}).min ? fmtBytes(f.driveSizeRange.min, 0) : "any"} – ${(f.driveSizeRange || {}).max ? fmtBytes(f.driveSizeRange.max, 0) : "any"}`]]
});
function DiscoveryPanel({
  k,
  nav
}) {
  const {
    data: ds,
    loading,
    error,
    reload
  } = useResource("disc|" + k.id, () => api.discoveries(k.id), 2500);
  const {
    data: hosts
  } = useResource("dhosts|" + k.id, () => api.k8sHosts(k.id), 4000);
  const {
    data: cfgs
  } = useResource("dcfg|" + k.id, () => api.k8sDeployConfigs(k.id), 3000);
  const cur = (ds || [])[0];
  const running = cur && cur.status === "running";
  const inv = (hosts || []).filter(h => cur && cur.status === "complete" && cur.hostIds.includes(h.id));
  if (error) return /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload,
    kind: "discovery"
  });
  return /*#__PURE__*/React.createElement(React.Fragment, null, !cur && !loading && /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 24,
    c: "var(--accent)"
  }), /*#__PURE__*/React.createElement("b", {
    style: {
      color: "var(--text)"
    }
  }, k.name, " has not been discovered"), /*#__PURE__*/React.createElement("span", {
    style: {
      maxWidth: 520
    }
  }, "The operator is connected and lists ", k.counts.workers, " worker node(s), but nothing is known about their hardware. Discovery inspects them; a storage cluster can only be deployed on a discovered cluster."), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    style: {
      marginTop: 10
    },
    onClick: () => window.__ui.dialog(discoveryDialog(k), k)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 12
  }), "Run discovery")), running && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)"
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Discovery running \u2014 ", cur.step, "."), " Inspection pods are collecting the inventory; this page updates as it lands.")), cur && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Discovery",
    v: running ? "running" : "complete",
    s: running ? cur.step : fmtAgo(cur.finishedAt || cur.startedAt),
    c: running ? "var(--info)" : "var(--ok)"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Worker nodes",
    v: cur.counts.nodes,
    s: "inspected"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Usable devices",
    v: cur.counts.devices,
    s: cur.counts.filtered ? `${cur.counts.filtered} excluded by the filter` : "nothing excluded"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Deployment documents",
    v: (cfgs || []).length,
    s: (cfgs || []).filter(c => !c.approved).length + " awaiting approval"
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Discovery filter"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(FilterProps, {
    f: cur.filter
  }), /*#__PURE__*/React.createElement("div", {
    className: "btnrow",
    style: {
      marginTop: 10
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "chip",
    disabled: running,
    onClick: () => window.__ui.dialog(discoveryDialog(k, cur), k)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "refresh",
    s: 12
  }), "Re-run with different filters")))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Node selector"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, Object.keys(cur.nodeSelector).length ? /*#__PURE__*/React.createElement(Props, {
    rows: Object.entries(cur.nodeSelector).map(([a, b]) => [a, b])
  }) : /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "No selector \u2014 every worker node was inspected."), cur.opName && /*#__PURE__*/React.createElement("div", {
    className: "uuid",
    style: {
      marginTop: 8
    }
  }, /*#__PURE__*/React.createElement("span", null, "OperatorOps/", cur.opName))))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Inventory"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), !running && /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    onClick: () => nav.deployWizard(k.id)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Deploy a cluster")), inv.map(h => /*#__PURE__*/React.createElement(HostInventory, {
    key: h.id,
    h: h
  })), !inv.length && !running && /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "The inventory is empty \u2014 no worker node matched the selector."), !!(cfgs || []).length && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Deployment documents"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, cfgs.map(c => /*#__PURE__*/React.createElement(DeployConfigTile, {
    key: c.id,
    d: c,
    nav: nav
  }))))));
}
function HostInventory({
  h
}) {
  const sockets = Array.from({
    length: h.sockets || 1
  }, (_, i) => i);
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, h.hostname, /*#__PURE__*/React.createElement("span", {
    className: "labels",
    style: {
      marginLeft: 8
    }
  }, h.zone && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "zone"), h.zone), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "vcpu"), h.vcpu), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "ram"), fmtBytes(h.memory, 0)), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "sockets"), h.sockets), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "nics"), h.nics.map(n => n.name).join(", ")))), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Socket"), /*#__PURE__*/React.createElement("th", null, "Kind"), /*#__PURE__*/React.createElement("th", null, "PCIe address"), /*#__PURE__*/React.createElement("th", null, "Block device"), /*#__PURE__*/React.createElement("th", null, "Serial"), /*#__PURE__*/React.createElement("th", null, "Model"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Capacity"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, sockets.flatMap(s => h.devices.filter(d => d.socket === s).map(d => /*#__PURE__*/React.createElement("tr", {
    key: d.id,
    style: d.assignedNodeId ? {
      opacity: .55
    } : null
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, s), /*#__PURE__*/React.createElement("td", null, d.kind === "nvme" ? "NVMe" : "block"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.pcie || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.blockdev), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, d.serial || "—"), /*#__PURE__*/React.createElement("td", null, d.model), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(d.size)), /*#__PURE__*/React.createElement("td", null, d.assignedNodeId ? /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, "assigned") : /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ok)",
      borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"
    }
  }, "free")))))))));
}

// ---- the wizard ------------------------------------------------------------
const EC_OPTS = ["1+1", "2+1", "2+2", "4+1", "4+2", "8+2"];
const Tog = ({
  on,
  set,
  label,
  hint
}) => /*#__PURE__*/React.createElement("label", {
  className: "fc",
  onClick: () => set(!on)
}, /*#__PURE__*/React.createElement("span", {
  className: "selbox" + (on ? " on" : "")
}, on && /*#__PURE__*/React.createElement(Icon, {
  n: "check",
  s: 11
})), /*#__PURE__*/React.createElement("span", null, label, hint && /*#__PURE__*/React.createElement("span", {
  className: "sub",
  style: {
    display: "block",
    fontSize: 10.5,
    color: "var(--dim2)"
  }
}, hint)));
const Fl = ({
  l,
  sub,
  children,
  sm
}) => /*#__PURE__*/React.createElement("label", {
  className: "fl" + (sm ? " sm" : "")
}, l, sub && /*#__PURE__*/React.createElement("span", {
  className: "sub"
}, sub), children);
const Inp = ({
  v,
  set,
  ...p
}) => /*#__PURE__*/React.createElement("input", _extends({
  className: "inp",
  value: v,
  onChange: e => set(e.target.value)
}, p));

// filter object from the wizard's cluster form
const clusterFilter = c => ({
  deviceClass: c.deviceClass,
  pcieAllowList: c.deviceClass === "nvme" ? csv(c.pcieAllow) : [],
  pcieDenyList: c.deviceClass === "nvme" ? csv(c.pcieDeny) : [],
  models: c.deviceClass === "nvme" ? csv(c.models) : [],
  blockDeviceNames: c.deviceClass === "block" ? csv(c.namePattern) : [],
  driveSizeRange: {
    min: Number(c.sizeMin || 0) * GBn,
    max: Number(c.sizeMax || 0) * GBn
  }
});
const toMock = d => ({
  kind: d.kind,
  pcie_address: d.pcie,
  device_name: d.blockdev,
  model_number: d.model,
  size: d.size
});
const matches = (d, f) => !d.assignedNodeId && window.deviceMatches(toMock(d), f);
const quick = (d, q) => !q || [d.pcie, d.blockdev, d.model, d.serial].some(x => (x || "").toLowerCase().includes(q.toLowerCase()));
function DeployWizard({
  kid,
  nav
}) {
  const {
    data: k
  } = useResource("wk|" + kid, () => api.k8sCluster(kid));
  const {
    data: ds
  } = useResource("wd|" + kid, () => api.discoveries(kid));
  const {
    data: hosts
  } = useResource("wh|" + kid, () => api.k8sHosts(kid));
  const [c, setC] = useState({
    name: "",
    deviceClass: "nvme",
    ec: "2+1",
    numa: "both",
    vcpu: 12,
    maxSubsystems: 128,
    hugepagesGb: "",
    memoryGb: 32,
    backups: true,
    objectStorage: false,
    failureDomains: true,
    coreIsolation: true,
    mgmtNic: "",
    dataNic1: "",
    dataNic2: "",
    pcieAllow: "",
    pcieDeny: "",
    models: "",
    sizeMin: 400,
    sizeMax: 0,
    namePattern: "",
    s3Endpoint: "",
    s3Bucket: "",
    s3Region: "",
    s3AccessKey: "",
    s3SecretKey: "",
    kmsEnabled: false,
    kmsProvider: "vault",
    kmsAddress: "",
    kmsKeyName: "",
    kmsAuth: "token",
    kmsToken: "",
    kmsVerifyTls: true
  });
  const [fd, setFd] = useState({}); // host id → failure domain label, fixed at deployment
  const [mode, setMode] = useState("pick");
  const [tag, setTag] = useState({
    key: "",
    value: "true"
  });
  const [zone, setZone] = useState("");
  const [picked, setPicked] = useState({});
  const [ov, setOv] = useState({}); // per host: {sockets, q, drop:{}, add:{}}
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const disc = (ds || []).find(d => d.status === "complete");
  const pool = useMemo(() => (hosts || []).filter(h => disc && disc.hostIds.includes(h.id)), [hosts, disc]);
  const zones = useMemo(() => [...new Set(pool.map(h => h.zone).filter(Boolean))].sort(), [pool]);
  const nicNames = useMemo(() => [...new Set(pool.flatMap(h => h.nics.map(n => n.name)))].sort(), [pool]);
  const filter = useMemo(() => clusterFilter(c), [c]);
  const numaDefault = c.numa === "both" ? [0, 1] : [Number(c.numa)];
  const listed = pool.filter(h => !zone || h.zone === zone);
  const selected = useMemo(() => mode === "tag" ? pool.filter(h => tag.key && (h.k8sLabels[tag.key] || h.labels[tag.key]) === tag.value) : pool.filter(h => picked[h.id]), [pool, mode, tag, picked]);
  const ovOf = h => ov[h.id] || {};
  const setOvOf = (h, patch) => setOv(o => Object.assign({}, o, {
    [h.id]: Object.assign({}, o[h.id] || {}, patch)
  }));
  const socketsOf = h => (ovOf(h).sockets || numaDefault).filter(s => s < (h.sockets || 1));
  const devSel = (h, d) => {
    const o = ovOf(h);
    if (o.drop && o.drop[d.id]) return false;
    if (o.add && o.add[d.id]) return true;
    return matches(d, filter) && quick(d, o.q);
  };
  const devicesOf = (h, s) => h.devices.filter(d => d.socket === s && !d.assignedNodeId && devSel(h, d));
  const hpPerNode = c.hugepagesGb ? Number(c.hugepagesGb) * GBn : hugepagesFor(Number(c.maxSubsystems) || 0);
  const plan = useMemo(() => {
    const groups = selected.flatMap(h => socketsOf(h).map(s => ({
      h,
      s,
      devs: devicesOf(h, s)
    })));
    return {
      groups,
      nodes: groups.length,
      devices: groups.reduce((n, g) => n + g.devs.length, 0),
      raw: groups.reduce((n, g) => n + g.devs.reduce((a, d) => a + d.size, 0), 0)
    };
  }, [selected, ov, filter, c.numa]);
  const [nd, np] = c.ec.split("+").map(Number);
  const usable = plan.raw * (nd / (nd + np));
  const nicMissing = selected.filter(h => [c.mgmtNic, c.dataNic1].filter(Boolean).some(n => !h.nics.some(x => x.name === n)));
  const fdOf = h => (fd[h.id] !== undefined ? fd[h.id] : h.rack || h.zone || "").trim();
  const fdMissing = c.failureDomains ? selected.filter(h => !fdOf(h)) : [];
  // a domain must hold at least two nodes, and domains stay within one node of
  // each other — so nodes are chosen in pairs per domain
  const fdTally = {};
  selected.forEach(h => {
    const f = fdOf(h);
    if (f) fdTally[f] = (fdTally[f] || 0) + 1;
  });
  const fdNames = Object.keys(fdTally).sort();
  const fdCount = fdNames.length;
  const fdThin = fdNames.filter(f => fdTally[f] < 2);
  const fdVals = fdNames.map(f => fdTally[f]);
  const fdSpread = fdVals.length ? Math.max(...fdVals) - Math.min(...fdVals) : 0;
  const fdTooFew = c.failureDomains && fdCount < 2;
  const fdBad = c.failureDomains && (fdThin.length > 0 || fdSpread > 1);
  const s3Missing = c.backups && !(c.s3Endpoint && c.s3Bucket && c.s3AccessKey && c.s3SecretKey);
  const kmsMissing = c.kmsEnabled && !(c.kmsAddress && c.kmsKeyName);
  const ready = !!c.name && !!c.mgmtNic && !!c.dataNic1 && plan.nodes > 0 && plan.devices > 0 && !nicMissing.length && !fdMissing.length && !fdTooFew && !fdBad && !s3Missing && !kmsMissing;
  const create = async () => {
    setBusy(true);
    setErr(null);
    try {
      const cfg = await api.deployConfigCreate(c.name + "-deployment", {
        environment: k && k.environment || "Vanilla",
        kubernetesClusterRef: k ? k.name : null,
        nodeSelector: mode === "tag" && tag.key ? {
          matchLabels: {
            [tag.key]: tag.value
          }
        } : {
          matchLabels: {}
        },
        cluster: {
          name: c.name,
          deviceClass: c.deviceClass,
          ec: c.ec,
          numaSockets: numaDefault,
          vcpu: Number(c.vcpu),
          maxSubsystems: Number(c.maxSubsystems),
          hugepagesOverride: c.hugepagesGb ? Number(c.hugepagesGb) * GBn : null,
          systemMemoryGb: Number(c.memoryGb),
          backups: c.backups,
          objectStorage: c.objectStorage,
          failureDomains: c.failureDomains,
          coreIsolation: c.coreIsolation,
          mgmtNic: c.mgmtNic,
          dataNics: [c.dataNic1, c.dataNic2].filter(Boolean),
          deviceFilter: filter,
          backupTarget: c.backups ? {
            endpoint: c.s3Endpoint,
            bucket: c.s3Bucket,
            region: c.s3Region || null,
            accessKeyId: c.s3AccessKey,
            secretAccessKeyRef: c.s3SecretKey ? "sb-s3-backup-credentials" : null
          } : null,
          kms: c.kmsEnabled ? {
            provider: c.kmsProvider,
            address: c.kmsAddress,
            keyName: c.kmsKeyName,
            auth: c.kmsAuth,
            verifyTls: c.kmsVerifyTls,
            secretRef: "sb-kms-credentials"
          } : null,
          failureDomainLabels: c.failureDomains ? Object.fromEntries(selected.map(h => [h.hostname, fdOf(h)])) : null
        },
        __kubernetesClusterId: kid,
        __hosts: selected.map(h => ({
          id: h.id,
          sockets: socketsOf(h),
          failureDomain: c.failureDomains ? fdOf(h) : null,
          deviceIds: socketsOf(h).flatMap(s => devicesOf(h, s).map(d => d.id))
        }))
      });
      nav.deployConfig(kid, cfg.metadata.uid);
    } catch (e) {
      setErr(e);
      setBusy(false);
    }
  };
  if (!k || !hosts || !ds) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, Array.from({
    length: 4
  }).map((_, i) => /*#__PURE__*/React.createElement("div", {
    className: "skel",
    key: i,
    style: {
      height: 62
    }
  }))));
  if (!disc) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 24,
    c: "var(--warn)"
  }), /*#__PURE__*/React.createElement("b", {
    style: {
      color: "var(--text)"
    }
  }, k.name, " is not discovered"), /*#__PURE__*/React.createElement("span", {
    style: {
      maxWidth: 480
    }
  }, "A storage cluster can only be deployed on a discovered Kubernetes cluster. Run discovery first."), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    style: {
      marginTop: 10
    },
    onClick: () => nav.discovery(kid)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 12
  }), "Go to discovery")));
  const nvme = c.deviceClass === "nvme";
  return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "dhead"
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("h1", null, "Deploy a storage cluster"), /*#__PURE__*/React.createElement("div", {
    className: "mdesc",
    style: {
      marginTop: 4
    }
  }, "On the discovered hardware of ", /*#__PURE__*/React.createElement("b", null, k.name), " (", pool.length, " candidate node(s)). This writes a deployment document for review \u2014 nothing is applied until it is approved."))), /*#__PURE__*/React.createElement("div", {
    className: "wzstep"
  }, /*#__PURE__*/React.createElement("b", null, "1"), /*#__PURE__*/React.createElement("h2", null, "Cluster parameters"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "Cluster name"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.name,
    set: v => up(setC, {
      name: v
    }),
    placeholder: "prod-eu-central-2"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Device type",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.deviceClass,
    onChange: e => up(setC, {
      deviceClass: e.target.value
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: "nvme"
  }, "NVMe (PCIe)"), /*#__PURE__*/React.createElement("option", {
    value: "block"
  }, "Linux block devices"))), /*#__PURE__*/React.createElement(Fl, {
    l: "Erasure coding",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.ec,
    onChange: e => up(setC, {
      ec: e.target.value
    })
  }, EC_OPTS.map(x => /*#__PURE__*/React.createElement("option", {
    key: x,
    value: x
  }, x, " data+parity")))), /*#__PURE__*/React.createElement(Fl, {
    l: "NUMA sockets",
    sub: "one storage node per socket",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.numa,
    onChange: e => up(setC, {
      numa: e.target.value
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: "0"
  }, "socket 0"), /*#__PURE__*/React.createElement("option", {
    value: "1"
  }, "socket 1"), /*#__PURE__*/React.createElement("option", {
    value: "both"
  }, "both")))), /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "vCPU per storage node",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "number",
    min: "4",
    v: c.vcpu,
    set: v => up(setC, {
      vcpu: v
    })
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Max subsystems",
    sm: true,
    sub: `→ ${fmtBytes(hugepagesFor(Number(c.maxSubsystems) || 0), 0)} hugepages`
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "number",
    min: "8",
    v: c.maxSubsystems,
    set: v => up(setC, {
      maxSubsystems: v
    })
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Hugepage memory override",
    sub: "GB \u2014 empty = derived",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "number",
    min: "2",
    step: "2",
    v: c.hugepagesGb,
    set: v => up(setC, {
      hugepagesGb: v
    }),
    placeholder: String(gb(hugepagesFor(Number(c.maxSubsystems) || 0)))
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "System memory per node",
    sub: "GB",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "number",
    min: "4",
    v: c.memoryGb,
    set: v => up(setC, {
      memoryGb: v
    })
  }))), /*#__PURE__*/React.createElement("div", {
    className: "togs"
  }, /*#__PURE__*/React.createElement(Tog, {
    on: c.coreIsolation,
    set: v => up(setC, {
      coreIsolation: v
    }),
    label: "Core isolation",
    hint: "CPU topology is enforced either way"
  }), /*#__PURE__*/React.createElement(Tog, {
    on: c.failureDomains,
    set: v => up(setC, {
      failureDomains: v
    }),
    label: "Failure domains",
    hint: "from rack / zone labels"
  }), /*#__PURE__*/React.createElement(Tog, {
    on: c.backups,
    set: v => up(setC, {
      backups: v
    }),
    label: "Backups",
    hint: "S3 backup target, set after deployment"
  }), /*#__PURE__*/React.createElement(Tog, {
    on: c.objectStorage,
    set: v => up(setC, {
      objectStorage: v
    }),
    label: "S3 object storage",
    hint: "buckets on this cluster"
  })), /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "Management NIC",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.mgmtNic,
    onChange: e => up(setC, {
      mgmtNic: e.target.value
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "choose\u2026"), nicNames.map(n => /*#__PURE__*/React.createElement("option", {
    key: n
  }, n)))), /*#__PURE__*/React.createElement(Fl, {
    l: "Data NIC 1",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.dataNic1,
    onChange: e => up(setC, {
      dataNic1: e.target.value
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "choose\u2026"), nicNames.filter(n => n !== c.mgmtNic).map(n => /*#__PURE__*/React.createElement("option", {
    key: n
  }, n)))), /*#__PURE__*/React.createElement(Fl, {
    l: "Data NIC 2",
    sub: "optional \u2014 multipath",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.dataNic2,
    onChange: e => up(setC, {
      dataNic2: e.target.value
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "none"), nicNames.filter(n => n !== c.mgmtNic && n !== c.dataNic1).map(n => /*#__PURE__*/React.createElement("option", {
    key: n
  }, n))))), /*#__PURE__*/React.createElement("div", {
    className: "sech",
    style: {
      margin: "6px 0 8px"
    }
  }, /*#__PURE__*/React.createElement("h2", null, "Device filter"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "cnt"
  }, nvme ? "NVMe" : "block devices")), /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, nvme ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Fl, {
    l: "PCIe allow",
    sub: "prefixes, comma-separated"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.pcieAllow,
    set: v => up(setC, {
      pcieAllow: v
    }),
    placeholder: "0000:5e:, 0000:5f:"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "PCIe deny"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.pcieDeny,
    set: v => up(setC, {
      pcieDeny: v
    }),
    placeholder: "0000:00:04.0"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "SSD model contains"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.models,
    set: v => up(setC, {
      models: v
    }),
    placeholder: "PM9A3, CD8"
  }))) : /*#__PURE__*/React.createElement(Fl, {
    l: "Block device name pattern",
    sub: "glob, comma-separated"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.namePattern,
    set: v => up(setC, {
      namePattern: v
    }),
    placeholder: "/dev/sd*, /dev/nvme?n1"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Capacity min",
    sub: "GB",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "number",
    min: "0",
    v: c.sizeMin,
    set: v => up(setC, {
      sizeMin: v
    })
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Capacity max",
    sub: "GB, 0 = any",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "number",
    min: "0",
    v: c.sizeMax,
    set: v => up(setC, {
      sizeMax: v
    })
  }))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "This filter picks the default device set on every node; step 3 can narrow it further per node."))), /*#__PURE__*/React.createElement("div", {
    className: "wzstep"
  }, /*#__PURE__*/React.createElement("b", null, "2"), /*#__PURE__*/React.createElement("h2", null, "Nodes"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), zones.length > 1 && /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: zone,
    onChange: e => setZone(e.target.value)
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "all sites"), zones.map(z => /*#__PURE__*/React.createElement("option", {
    key: z
  }, z))), /*#__PURE__*/React.createElement("div", {
    className: "seg"
  }, /*#__PURE__*/React.createElement("button", {
    className: mode === "pick" ? "on" : "",
    onClick: () => setMode("pick")
  }, "Pick nodes"), /*#__PURE__*/React.createElement("button", {
    className: mode === "tag" ? "on" : "",
    onClick: () => setMode("tag")
  }, "By node label"))), mode === "tag" ? /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "Label key"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: tag.key,
    set: v => up(setTag, {
      key: v
    }),
    placeholder: "simplyblock.io/storage-node-candidate"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Value",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    v: tag.value,
    set: v => up(setTag, {
      value: v
    })
  }))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, tag.key ? `${selected.length} of ${pool.length} discovered node(s) carry this label.` : "Enter a label key to match nodes by."))) : /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, listed.map(h => {
    const n = h.devices.filter(d => matches(d, filter)).length;
    return /*#__PURE__*/React.createElement("div", {
      key: h.id,
      className: "tile" + (picked[h.id] ? " sel" : ""),
      style: {
        "--sc": "var(--accent)"
      },
      onClick: () => setPicked(p => Object.assign({}, p, {
        [h.id]: !p[h.id]
      }))
    }, /*#__PURE__*/React.createElement("div", {
      className: "th"
    }, /*#__PURE__*/React.createElement("div", {
      style: {
        minWidth: 0,
        flex: 1,
        display: "flex",
        gap: 8,
        alignItems: "flex-start"
      }
    }, /*#__PURE__*/React.createElement("span", {
      className: "selbox" + (picked[h.id] ? " on" : "")
    }, picked[h.id] && /*#__PURE__*/React.createElement(Icon, {
      n: "check",
      s: 11
    })), /*#__PURE__*/React.createElement(Name, null, h.hostname)), h.zone && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, h.zone)), /*#__PURE__*/React.createElement("div", {
      className: "kv"
    }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Sockets"), /*#__PURE__*/React.createElement("b", null, h.sockets)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Matching devices"), /*#__PURE__*/React.createElement("b", {
      style: n ? null : {
        color: "var(--warn)"
      }
    }, n)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "vCPU"), /*#__PURE__*/React.createElement("b", null, h.vcpu)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "RAM"), /*#__PURE__*/React.createElement("b", null, fmtBytes(h.memory, 0)))));
  })), !listed.length && /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "No discovered node", zone ? ` in ${zone}` : "", "."), /*#__PURE__*/React.createElement("div", {
    className: "wzstep" + (selected.length ? "" : " dis")
  }, /*#__PURE__*/React.createElement("b", null, "3"), /*#__PURE__*/React.createElement("h2", null, "Per-node devices and sockets"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "cnt"
  }, plan.nodes, " storage node(s)")), !!selected.length && /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, selected.map(h => {
    const o = ovOf(h);
    return /*#__PURE__*/React.createElement("div", {
      key: h.id,
      className: "nset"
    }, /*#__PURE__*/React.createElement("div", {
      className: "nsh"
    }, /*#__PURE__*/React.createElement("b", null, h.hostname), h.zone && /*#__PURE__*/React.createElement("span", {
      className: "lab"
    }, /*#__PURE__*/React.createElement("i", null, "site"), h.zone), c.failureDomains && /*#__PURE__*/React.createElement("label", {
      className: "fdin",
      title: "fixed for the node's lifetime"
    }, /*#__PURE__*/React.createElement("i", null, "failure domain"), /*#__PURE__*/React.createElement("input", {
      className: "inp",
      list: "fdlist",
      value: fdOf(h),
      placeholder: "rack-1",
      onChange: e => setFd(f => Object.assign({}, f, {
        [h.id]: e.target.value
      }))
    })), /*#__PURE__*/React.createElement("div", {
      className: "socks"
    }, Array.from({
      length: h.sockets || 1
    }, (_, s) => {
      const on = socketsOf(h).includes(s);
      return /*#__PURE__*/React.createElement("button", {
        key: s,
        className: "chip" + (on ? " on" : ""),
        onClick: () => setOvOf(h, {
          sockets: on ? socketsOf(h).filter(y => y !== s) : [...socketsOf(h), s].sort()
        })
      }, "socket ", s, /*#__PURE__*/React.createElement("b", {
        className: "n"
      }, h.devices.filter(d => d.socket === s && matches(d, filter)).length));
    })), /*#__PURE__*/React.createElement("input", {
      className: "inp",
      style: {
        width: 220,
        marginLeft: "auto",
        height: 26
      },
      placeholder: "extra filter: pcie / model / name",
      value: o.q || "",
      onChange: e => setOvOf(h, {
        q: e.target.value,
        drop: {},
        add: {}
      })
    }), nicMissing.includes(h) && /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--bad)"
      }
    }, "missing NIC ", [c.mgmtNic, c.dataNic1].filter(n => n && !h.nics.some(x => x.name === n)).join(", "))), socketsOf(h).map(s => /*#__PURE__*/React.createElement("div", {
      key: s,
      className: "devrow"
    }, /*#__PURE__*/React.createElement("span", {
      className: "tlabel"
    }, "socket ", s), /*#__PURE__*/React.createElement("div", {
      className: "devs"
    }, h.devices.filter(d => d.socket === s && !d.assignedNodeId && d.kind === c.deviceClass).map(d => {
      const on = devSel(h, d);
      return /*#__PURE__*/React.createElement("button", {
        key: d.id,
        className: "chip" + (on ? " on" : ""),
        title: `${d.model} · ${fmtBytes(d.size)}${d.serial ? " · " + d.serial : ""}`,
        onClick: () => setOvOf(h, on ? {
          drop: Object.assign({}, o.drop, {
            [d.id]: true
          }),
          add: Object.assign({}, o.add, {
            [d.id]: false
          })
        } : {
          add: Object.assign({}, o.add, {
            [d.id]: true
          }),
          drop: Object.assign({}, o.drop, {
            [d.id]: false
          })
        })
      }, devLabel(d), /*#__PURE__*/React.createElement("b", {
        className: "n"
      }, fmtBytes(d.size, 0)));
    }), !h.devices.some(d => d.socket === s && !d.assignedNodeId && d.kind === c.deviceClass) && /*#__PURE__*/React.createElement("span", {
      className: "mdesc",
      style: {
        margin: 0
      }
    }, "no free ", nvme ? "NVMe" : "block", " device on this socket")))));
  }))), c.failureDomains && !!selected.length && /*#__PURE__*/React.createElement("div", {
    className: "fdsum"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "host",
    s: 13
  }), /*#__PURE__*/React.createElement("span", null, fdCount || "no", " failure domain", fdCount === 1 ? "" : "s", " across ", selected.length, " node(s)", fdMissing.length ? ` · ${fdMissing.length} node(s) unlabelled` : ""), !!fdCount && /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, fdNames.map(f => `${f}: ${fdTally[f]}`).join(" · ")), /*#__PURE__*/React.createElement("datalist", {
    id: "fdlist"
  }, [...new Set(pool.map(h => h.rack || h.zone).filter(Boolean))].map(x => /*#__PURE__*/React.createElement("option", {
    key: x,
    value: x
  }))), /*#__PURE__*/React.createElement("span", {
    className: "note"
  }, "at least two nodes per domain, domains within one node of each other \xB7 a node's domain is set once, at deployment, and cannot be changed afterwards")), /*#__PURE__*/React.createElement("div", {
    className: "wzstep"
  }, /*#__PURE__*/React.createElement("b", null, "4"), /*#__PURE__*/React.createElement("h2", null, "Backup target and key management"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "cnt"
  }, "correctable later")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, c.backups ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "S3 endpoint",
    sub: "backups of snapshot chains are written here"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.s3Endpoint,
    set: v => up(setC, {
      s3Endpoint: v
    }),
    placeholder: "https://s3.eu-central-1.amazonaws.com"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Bucket",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.s3Bucket,
    set: v => up(setC, {
      s3Bucket: v
    }),
    placeholder: "sb-backups-prod"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Region",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.s3Region,
    set: v => up(setC, {
      s3Region: v
    }),
    placeholder: "eu-central-1"
  }))), /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "Access key ID",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.s3AccessKey,
    set: v => up(setC, {
      s3AccessKey: v
    })
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Secret access key",
    sub: "stored in a Kubernetes secret",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "password",
    v: c.s3SecretKey,
    set: v => up(setC, {
      s3SecretKey: v
    })
  }))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "The credentials are written to ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "sb-s3-backup-credentials"), " in the operator namespace, never into the deployment document. The endpoint can be corrected after deployment from the cluster's actions.")) : /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "Backups are off for this cluster, so no S3 target is needed. Turning them on later also asks for the endpoint."), /*#__PURE__*/React.createElement("div", {
    className: "sech",
    style: {
      margin: "12px 0 8px"
    }
  }, /*#__PURE__*/React.createElement("h2", null, "External key management"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement(Tog, {
    on: c.kmsEnabled,
    set: v => up(setC, {
      kmsEnabled: v
    }),
    label: "Use an external KMS",
    hint: "encryption keys are fetched per volume"
  })), c.kmsEnabled ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "Provider",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.kmsProvider,
    onChange: e => up(setC, {
      kmsProvider: e.target.value
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: "vault"
  }, "HashiCorp Vault"), /*#__PURE__*/React.createElement("option", {
    value: "aws-kms"
  }, "AWS KMS"), /*#__PURE__*/React.createElement("option", {
    value: "azure-keyvault"
  }, "Azure Key Vault"), /*#__PURE__*/React.createElement("option", {
    value: "gcp-kms"
  }, "Google Cloud KMS"))), /*#__PURE__*/React.createElement(Fl, {
    l: "Address",
    sub: "API endpoint"
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.kmsAddress,
    set: v => up(setC, {
      kmsAddress: v
    }),
    placeholder: "https://vault.internal:8200"
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Key name",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    v: c.kmsKeyName,
    set: v => up(setC, {
      kmsKeyName: v
    }),
    placeholder: "simplyblock-prod"
  }))), /*#__PURE__*/React.createElement("div", {
    className: "frow"
  }, /*#__PURE__*/React.createElement(Fl, {
    l: "Authentication",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.kmsAuth,
    onChange: e => up(setC, {
      kmsAuth: e.target.value
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: "token"
  }, "token"), /*#__PURE__*/React.createElement("option", {
    value: "approle"
  }, "AppRole"), /*#__PURE__*/React.createElement("option", {
    value: "kubernetes"
  }, "Kubernetes service account"), /*#__PURE__*/React.createElement("option", {
    value: "iam"
  }, "IAM role"))), /*#__PURE__*/React.createElement(Fl, {
    l: "Token / secret",
    sub: "stored in a Kubernetes secret",
    sm: true
  }, /*#__PURE__*/React.createElement(Inp, {
    type: "password",
    v: c.kmsToken,
    set: v => up(setC, {
      kmsToken: v
    })
  })), /*#__PURE__*/React.createElement(Fl, {
    l: "Verify TLS",
    sm: true
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: c.kmsVerifyTls ? "1" : "0",
    onChange: e => up(setC, {
      kmsVerifyTls: e.target.value === "1"
    })
  }, /*#__PURE__*/React.createElement("option", {
    value: "1"
  }, "yes"), /*#__PURE__*/React.createElement("option", {
    value: "0"
  }, "no")))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "Encrypted volumes then hold no key material: the node fetches the data-encryption key from the KMS at attach time. Without a KMS, keys are generated and kept by the control plane.")) : /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "Keys for encrypted volumes are generated and stored by the control plane. An external KMS can also be configured after deployment."))), /*#__PURE__*/React.createElement("div", {
    className: "wzsum"
  }, /*#__PURE__*/React.createElement("div", {
    className: "stats",
    style: {
      margin: 0
    }
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Nodes",
    v: selected.length,
    s: mode === "tag" ? "matched by label" : "picked"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Storage nodes",
    v: plan.nodes,
    s: "one per NUMA socket"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Devices",
    v: plan.devices,
    s: "claimed at deployment"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Raw capacity",
    v: fmtBytes(plan.raw),
    s: `${fmtBytes(usable)} usable at ${c.ec}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Hugepages",
    v: fmtBytes(hpPerNode * plan.nodes, 0),
    s: `${fmtBytes(hpPerNode, 0)} per node${c.hugepagesGb ? " (override)" : ""}`
  })), err && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--bad)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, err.message)), /*#__PURE__*/React.createElement("div", {
    className: "btnrow"
  }, /*#__PURE__*/React.createElement("button", {
    className: "btn",
    onClick: () => nav.discovery(kid)
  }, "Cancel"), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    disabled: !ready || busy,
    onClick: create
  }, busy ? "Writing document…" : "Write deployment document")), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, ready ? "The document is written unapproved. Review it, then approve it to start the deployment." : !c.name ? "Name the cluster." : !c.mgmtNic || !c.dataNic1 ? "Choose the management and at least one data NIC." : nicMissing.length ? `${nicMissing.length} node(s) lack the chosen NIC names.` : plan.nodes === 0 || plan.devices === 0 ? "Select at least one node with one device." : fdMissing.length ? `Give every node a failure domain label — ${fdMissing.length} still unlabelled.` : fdTooFew ? "Failure domains need at least two distinct labels across the selected nodes." : fdThin.length ? `${fdThin.join(", ")} would carry a single node. Each failure domain needs at least two storage nodes — select them in pairs.` : fdSpread > 1 ? `The domains are unbalanced (${fdNames.map(f => f + ": " + fdTally[f]).join(", ")}). Node counts may differ by at most one.` : s3Missing ? "Backups are on: give the S3 endpoint, bucket and credentials." : kmsMissing ? "The KMS needs an address and a key name." : "")));
}
Object.assign(window, {
  DiscoveryPanel,
  HostInventory,
  DeployWizard,
  discoveryDialog,
  deployFromDialog,
  FilterProps
});
})();
// ---- deploy-doc.jsx ----
(function(){
// ---------------------------------------------------------------------------
// DEPLOYMENT DOCUMENT — tile, detail, per-node progress, general log and
// drill-in to the step / node logs.
// ---------------------------------------------------------------------------
const STEP_PH = {
  Succeeded: "var(--ok)",
  Running: "var(--info)",
  Failed: "var(--bad)",
  Pending: "var(--idle)"
};
const NODE_DONE = {
  Configured: 1,
  Added: 1
};
const nodePhaseColor = p => NODE_DONE[p] ? "var(--ok)" : p === "Pending" ? "var(--dim2)" : p === "Rebooting" ? "var(--warn)" : "var(--info)";
const StepTracker = ({
  steps,
  onLog
}) => /*#__PURE__*/React.createElement("div", {
  className: "steps"
}, steps.map((s, i) => /*#__PURE__*/React.createElement("div", {
  key: s.name,
  className: "stp " + s.phase.toLowerCase()
}, /*#__PURE__*/React.createElement("div", {
  className: "sn",
  style: {
    "--c": STEP_PH[s.phase]
  }
}, s.phase === "Succeeded" ? /*#__PURE__*/React.createElement(Icon, {
  n: "check",
  s: 11
}) : i + 1), /*#__PURE__*/React.createElement("div", {
  style: {
    minWidth: 0,
    flex: 1
  }
}, /*#__PURE__*/React.createElement("div", {
  className: "sl",
  style: {
    display: "flex",
    gap: 8,
    alignItems: "center"
  }
}, s.label, onLog && s.phase !== "Pending" && s.name === "ActivateCluster" && /*#__PURE__*/React.createElement("button", {
  className: "chip",
  style: {
    marginLeft: "auto"
  },
  onClick: () => onLog(s.name)
}, /*#__PURE__*/React.createElement(Icon, {
  n: "list",
  s: 11
}), "Log")), /*#__PURE__*/React.createElement("div", {
  className: "sm"
}, s.message || (s.phase === "Pending" ? "waiting" : s.phase.toLowerCase()), s.finishedAt ? ` · ${fmtAgo(s.finishedAt)}` : ""), s.phase === "Running" && /*#__PURE__*/React.createElement("div", {
  className: "sbar"
}, /*#__PURE__*/React.createElement("i", {
  style: {
    width: (s.progress || 3) + "%"
  }
}))))));
const approveDialog = d => ({
  title: `Approve and deploy ${d.name}?`,
  confirm: "Approve and deploy",
  desc: `Approval is one-way. The cluster ${d.cluster.name} is created in the control plane, then three asynchronous steps run: the ${d.counts.hosts} worker node(s) are configured (persistent hugepages, core isolation — this reboots them), ${d.counts.nodes} storage node(s) are added in parallel, and the cluster is activated.`,
  run: () => api.deployConfigApprove(d.name)
});
function DeployConfigTile({
  d,
  nav
}) {
  const running = d.steps.find(s => s.phase === "Running");
  const done = d.steps.filter(s => s.phase === "Succeeded").length;
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[d.status].c
    },
    onDoubleClick: () => nav.detail(d)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: d,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: d.status
    }), /*#__PURE__*/React.createElement(Name, null, d.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, d.cluster ? d.cluster.deviceClass === "nvme" ? "NVMe" : "block" : "")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: d.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "cluster"), d.cluster && d.cluster.name || "—"), d.cluster && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "ec"), d.cluster.ec), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "core isolation"), d.sizing.coreIsolation ? "yes" : "no")), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Nodes"), /*#__PURE__*/React.createElement("b", null, d.counts.hosts)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Storage nodes"), /*#__PURE__*/React.createElement("b", null, d.counts.nodes)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Devices"), /*#__PURE__*/React.createElement("b", null, d.counts.devices)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Hugepages"), /*#__PURE__*/React.createElement("b", null, fmtBytes(d.sizing.hugepages, 0)))), d.status === "Deploying" && /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, "Step ", done + 1, " of ", d.steps.length, " \u2014 ", running ? running.message || running.label : "starting"), /*#__PURE__*/React.createElement("div", {
    className: "sbar",
    style: {
      width: 80
    }
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: (done + (running ? (running.progress || 3) / 100 : 0)) / d.steps.length * 100 + "%"
    }
  }))), d.status === "Draft" && /*#__PURE__*/React.createElement("div", {
    className: "prepbox"
  }, "Awaiting approval. Nothing has been applied to any node yet."), /*#__PURE__*/React.createElement(Foot, {
    items: [d.status === "Draft" ? {
      label: "Approve",
      icon: "check",
      onClick: () => window.__ui.dialog(approveDialog(d), d)
    } : null, d.clusterId ? {
      label: "Cluster",
      icon: "cluster",
      onClick: () => nav.openCluster(d.clusterId)
    } : null, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(d)
    }]
  }));
}

// ---- logs ------------------------------------------------------------------
function DeployLog({
  d,
  onOpen
}) {
  const [step, setStep] = useState("");
  const [node, setNode] = useState("");
  const [q, setQ] = useState("");
  const lines = d.log.filter(l => (!step || l.step === step) && (!node || l.node === node) && (!q || l.msg.toLowerCase().includes(q.toLowerCase()))).slice().reverse();
  const steps = [...new Set(d.log.map(l => l.step))];
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Deployment log", /*#__PURE__*/React.createElement("span", {
    className: "live",
    style: {
      marginLeft: 8
    }
  }, /*#__PURE__*/React.createElement("i", null), "live")), /*#__PURE__*/React.createElement("div", {
    className: "logtools"
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: step,
    onChange: e => setStep(e.target.value)
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "all steps"), steps.map(s => /*#__PURE__*/React.createElement("option", {
    key: s
  }, s))), /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: node,
    onChange: e => setNode(e.target.value)
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "all nodes"), d.nodes.map(n => /*#__PURE__*/React.createElement("option", {
    key: n.host
  }, n.host))), /*#__PURE__*/React.createElement("input", {
    className: "inp",
    style: {
      width: 200
    },
    placeholder: "search",
    value: q,
    onChange: e => setQ(e.target.value)
  }), /*#__PURE__*/React.createElement("span", {
    className: "cnt",
    style: {
      marginLeft: "auto",
      fontSize: 11,
      color: "var(--dim2)"
    }
  }, lines.length, " of ", d.log.length), /*#__PURE__*/React.createElement(CopyBtn, {
    get: () => lines.map(l => `${l.ts} ${l.level} ${l.step}${l.node ? " " + l.node : ""} ${l.msg}`).join("\n")
  })), /*#__PURE__*/React.createElement("div", {
    className: "logstream",
    style: {
      maxHeight: 320
    }
  }, !lines.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, d.status === "Draft" ? "The log starts when the document is approved." : "No lines match.") : lines.map((l, i) => /*#__PURE__*/React.createElement("div", {
    className: "lrow",
    key: i
  }, /*#__PURE__*/React.createElement("span", {
    className: "lts"
  }, (l.ts || "").replace("T", " ").replace(/(\.\d+)?Z$/, "")), /*#__PURE__*/React.createElement("span", {
    className: "lstep"
  }, l.step), /*#__PURE__*/React.createElement("span", {
    className: "lnode"
  }, l.node ? /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: () => onOpen(`${l.step}/${l.node}`)
  }, l.node) : "—"), /*#__PURE__*/React.createElement("span", {
    className: "lmsgtxt"
  }, l.msg)))));
}

// the actual log of one step, or of one node within a step
function StepLogViewer({
  d,
  name,
  setName
}) {
  const [q, setQ] = useState("");
  const {
    data,
    loading,
    error,
    reload
  } = useResource("dlog|" + d.id + "|" + name, () => api.deployLog(d.id, name), 2000);
  const lines = (data || []).filter(l => !q || l.msg.toLowerCase().includes(q.toLowerCase()));
  const options = d.steps.filter(s => s.phase !== "Pending").flatMap(s => s.name === "ActivateCluster" ? [s.name] : d.nodes.map(n => `${s.name}/${n.host}`));
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Log \xB7 ", /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      fontWeight: 400
    }
  }, name), /*#__PURE__*/React.createElement("button", {
    className: "kebab",
    style: {
      marginLeft: "auto"
    },
    onClick: () => setName(null),
    title: "Close"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "x",
    s: 12
  }))), /*#__PURE__*/React.createElement("div", {
    className: "logtools"
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: name,
    onChange: e => setName(e.target.value)
  }, options.map(o => /*#__PURE__*/React.createElement("option", {
    key: o
  }, o))), /*#__PURE__*/React.createElement("input", {
    className: "inp",
    style: {
      width: 200
    },
    placeholder: "filter",
    value: q,
    onChange: e => setQ(e.target.value)
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: "auto"
    }
  }), /*#__PURE__*/React.createElement(CopyBtn, {
    get: () => lines.map(l => `${l.ts} ${l.level} ${l.msg}`).join("\n")
  })), /*#__PURE__*/React.createElement(LogStream, {
    lines: lines,
    loading: loading,
    error: error,
    onRetry: reload,
    height: 300,
    empty: "No output yet."
  }));
}
function NodeProgress({
  d,
  step,
  onOpen
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, step === "ConfigureNodes" ? "Worker node configuration" : "Storage nodes", /*#__PURE__*/React.createElement("span", {
    className: "cnt",
    style: {
      marginLeft: 8,
      fontSize: 11,
      color: "var(--dim2)"
    }
  }, d.nodes.filter(n => NODE_DONE[n.phase]).length, "/", d.nodes.length, " done")), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, d.nodes.map(n => /*#__PURE__*/React.createElement("div", {
    key: n.host,
    className: "nodest"
  }, /*#__PURE__*/React.createElement("span", {
    className: "nm"
  }, /*#__PURE__*/React.createElement("b", null, n.host), " ", /*#__PURE__*/React.createElement("span", {
    className: "mdesc",
    style: {
      margin: 0,
      display: "inline"
    }
  }, n.message || "")), /*#__PURE__*/React.createElement("span", {
    className: "ph",
    style: {
      color: nodePhaseColor(n.phase),
      display: "flex",
      gap: 6,
      alignItems: "center"
    }
  }, /*#__PURE__*/React.createElement(Dot, {
    c: nodePhaseColor(n.phase)
  }), n.phase, n.phase !== "Pending" && /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => onOpen(`${step}/${n.host}`)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "list",
    s: 10
  }), "log"), n.storageNodeIds.map(id => /*#__PURE__*/React.createElement("button", {
    key: id,
    className: "chip",
    onClick: () => window.__nav && window.__nav.openNode(d.clusterId, id)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "node",
    s: 10
  }), "node"))), !NODE_DONE[n.phase] && n.phase !== "Pending" && /*#__PURE__*/React.createElement("div", {
    className: "sbar"
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: n.progress + "%"
    }
  }))))));
}
function DeployConfigDetail({
  o: d,
  nav
}) {
  const [logName, setLogName] = useState(null);
  const running = d.steps.find(s => s.phase === "Running");
  const perNodeStep = running && running.name !== "ActivateCluster" ? running : d.steps.filter(s => s.name !== "ActivateCluster" && s.phase === "Succeeded").slice(-1)[0];
  const byHost = {};
  d.groups.forEach(g => {
    (byHost[g.node] = byHost[g.node] || []).push(g);
  });
  const cl = d.cluster || {};
  window.__nav = nav;
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: d,
    title: d.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, cl.deviceClass === "nvme" ? "NVMe" : "block devices"), d.environment && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, d.environment)),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "ClusterDeploymentConfig/", d.name)
  }), d.status === "Draft" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Awaiting approval."), " Nothing has been applied to any node \u2014 approving is what starts the deployment."), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    style: {
      marginLeft: "auto",
      flex: "none"
    },
    onClick: () => window.__ui.dialog(approveDialog(d), d)
  }, "Approve and deploy")), d.status === "Deploying" && running && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)"
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Step ", d.steps.indexOf(running) + 1, " of ", d.steps.length, ": ", running.label, "."), " ", running.message || "")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Phase",
    v: /*#__PURE__*/React.createElement(TrafficLight, {
      status: d.status
    }),
    s: d.message || "—"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Nodes",
    v: d.counts.hosts,
    s: perNodeStep ? `${d.nodes.filter(n => NODE_DONE[n.phase]).length}/${d.nodes.length} ${perNodeStep.name === "ConfigureNodes" ? "configured" : "added"}` : "worker nodes"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Storage nodes",
    v: d.counts.nodes,
    s: "one per NUMA socket"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Devices",
    v: d.counts.devices
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Hugepages",
    v: fmtBytes(d.sizing.hugepages, 0),
    s: cl.hugepagesOverride ? "override" : `from ${cl.maxSubsystems} subsystems/node`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "vCPU",
    v: d.sizing.vcpu,
    s: cl.coreIsolation ? "core isolation on" : "no core isolation"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Deployment"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), d.clusterId && /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => nav.openCluster(d.clusterId)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 11
  }), "Open cluster")), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Steps"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, d.status === "Draft" && /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "These run asynchronously once approved. The cluster record exists from approval on (status unready); it is usable after the last step."), /*#__PURE__*/React.createElement(StepTracker, {
    steps: d.steps,
    onLog: setLogName
  }))), d.status !== "Draft" && perNodeStep ? /*#__PURE__*/React.createElement(NodeProgress, {
    d: d,
    step: perNodeStep.name,
    onOpen: setLogName
  }) : /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Cluster"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(ClusterProps, {
    cl: cl,
    d: d,
    nav: nav
  })))), d.status !== "Draft" && /*#__PURE__*/React.createElement(React.Fragment, null, logName && /*#__PURE__*/React.createElement(StepLogViewer, {
    d: d,
    name: logName,
    setName: setLogName
  }), /*#__PURE__*/React.createElement(DeployLog, {
    d: d,
    onOpen: setLogName
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, d.status !== "Draft" && /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Cluster"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(ClusterProps, {
    cl: cl,
    d: d,
    nav: nav
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Device filter"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(FilterProps, {
    f: d.filter
  }), /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "Applied on top of the discovery filter; per-node picks in the node sets below are the final selection.")))), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Node sets"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), Object.entries(byHost).map(([host, gs]) => /*#__PURE__*/React.createElement("div", {
    key: host,
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, host, /*#__PURE__*/React.createElement("span", {
    className: "labels",
    style: {
      marginLeft: 8
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "storage nodes"), gs.length), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "mgmt nic"), gs[0].mgmtNic), gs[0].failureDomain && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "failure domain"), gs[0].failureDomain))), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Socket"), /*#__PURE__*/React.createElement("th", null, "Devices"), /*#__PURE__*/React.createElement("th", null, "Data NICs"), /*#__PURE__*/React.createElement("th", null, "vCPU"), /*#__PURE__*/React.createElement("th", null, "Hugepages"), /*#__PURE__*/React.createElement("th", null, "System RAM"), /*#__PURE__*/React.createElement("th", null, "Max subsystems"), /*#__PURE__*/React.createElement("th", null, "Core isolation"))), /*#__PURE__*/React.createElement("tbody", null, gs.map(g => /*#__PURE__*/React.createElement("tr", {
    key: g.socket
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, g.socket), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      maxWidth: 280
    }
  }, [].concat(g.nvme, g.block).join(", ") || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, g.dataNics.join(", ") || "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, g.vcpu), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, fmtBytes(g.hugepages, 0)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, fmtBytes(g.systemMemory, 0)), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, g.maxSubsystems), /*#__PURE__*/React.createElement("td", null, g.coreIsolation ? "yes" : "no")))))))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Provenance"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Kubernetes cluster", d.k8sClusterId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.k8sDetail(d.k8sClusterId),
      label: d.k8sClusterName || regName(d.k8sClusterId)
    }) : d.k8sClusterName || "—"], ["Node selector", Object.keys(d.nodeSelector).length ? Object.entries(d.nodeSelector).map(([a, b]) => `${a}=${b}`).join(", ") : "picked individually"], ["Approved", d.approved ? "yes" : "no — not applied"], ["Created", fmtDate(d.createdAt)]]
  }))));
}
const ClusterProps = ({
  cl,
  d,
  nav
}) => /*#__PURE__*/React.createElement(Props, {
  rows: [["Name", cl.name], ["Device type", cl.deviceClass === "nvme" ? "NVMe (PCIe)" : "Linux block devices"], ["Erasure coding", cl.ec], ["NUMA sockets", (cl.numaSockets || []).map(s => "socket " + s).join(", ")], ["Per storage node", `${cl.vcpu} vCPU · ${cl.systemMemoryGb} GB RAM · ${cl.maxSubsystems} subsystems`], ["Hugepages", cl.hugepagesOverride ? `${fmtBytes(cl.hugepagesOverride, 0)} (override)` : `derived from subsystems`], ["NICs", `${cl.mgmtNic} (mgmt) · ${(cl.dataNics || []).join(", ")} (data)`], ["Options", [cl.coreIsolation && "core isolation", cl.failureDomains && "failure domains", cl.backups && "backups", cl.objectStorage && "S3 object storage"].filter(Boolean).join(" · ") || "—"], d.clusterId ? ["Cluster", /*#__PURE__*/React.createElement(Ref, {
    onClick: () => nav.openCluster(d.clusterId),
    label: regName(d.clusterId) || cl.name
  })] : null].filter(Boolean)
});
Object.assign(window, {
  DeployConfigTile,
  DeployConfigDetail,
  StepTracker,
  approveDialog,
  DeployLog,
  StepLogViewer
});
})();
// ---- storage-types.jsx ----
(function(){
// ---------------------------------------------------------------------------
// FILE (pNFS) AND OBJECT (S3) STORAGE
// File: one kernel NFS server on a control-plane worker serves metadata only;
// every worker is a pNFS data client, so data paths bypass it entirely. The
// metadata service keeps no local state, so it restarts on another worker in
// seconds. pNFS on the Linux NFS server supports XFS only, so RWX is XFS-only.
// Object: one bucket = one filesystem = one logical volume — blobs on cluster
// capacity, metadata in FoundationDB — so buckets inherit snapshots, backups
// and both replication modes from the volume beneath them.
// ---------------------------------------------------------------------------
const MDS_STATE = {
  active: "online",
  restarting: "in_restart",
  electing: "in_activation"
};
function BucketTile({
  b,
  nav
}) {
  const quota = b.quota || b.capacity.total;
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[b.status].c
    },
    onDoubleClick: () => nav.detail(b)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: b,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: b.status
    }), /*#__PURE__*/React.createElement(Name, null, b.name)),
    right: /*#__PURE__*/React.createElement(React.Fragment, null, b.versioning && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "versioned"), b.objectLock && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "locked"), b.encrypted && /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "lock",
      s: 11
    }), "enc"))
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: b.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    title: "One bucket is one filesystem is one logical volume",
    onClick: e => {
      e.stopPropagation();
      nav.openVolumeById(b.volumeId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "volume",
    s: 10
  }), b.volumeName), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openPool(b.clusterId, b.poolId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "pool",
    s: 10
  }), b.poolName), b.access.public && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--warn)",
      borderColor: "color-mix(in srgb,var(--warn) 40%,transparent)"
    }
  }, "public")), /*#__PURE__*/React.createElement(Capacity, {
    label: b.quota ? "Quota" : "Provisioned",
    total: quota,
    used: b.capacity.used
  }), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Objects"), /*#__PURE__*/React.createElement("b", null, fmtNum(b.objects))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Stored"), /*#__PURE__*/React.createElement("b", null, fmtBytes(b.capacity.used))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Snapshots"), /*#__PURE__*/React.createElement("b", null, b.counts.snapshots)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Backups"), /*#__PURE__*/React.createElement("b", null, b.counts.backups))), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "Kubernetes service account holding the bucket credentials"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 10
  }), b.access.namespace, "/", b.access.service_account), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "policy"), b.access.policy), b.region && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "region"), b.region), b.storageClass !== "standard" && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "class"), b.storageClass)), !!b.counts.tags && /*#__PURE__*/React.createElement("div", {
    className: "labels tags",
    title: "S3 bucket tags"
  }, Object.entries(b.tags).slice(0, 5).map(([k, v]) => /*#__PURE__*/React.createElement("span", {
    key: k,
    className: "lab tag"
  }, /*#__PURE__*/React.createElement("i", null, k), v || "—")), b.counts.tags > 5 && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, "+", b.counts.tags - 5)), b.replication && /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "shield",
    s: 10
  }), b.replication.mode === "synchronous" ? "sync" : "async", " replicated")), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Details",
      right: true,
      onClick: () => nav.detail(b)
    }]
  }));
}
function BucketDetail({
  o: b,
  nav
}) {
  const a = b.access || {};
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: b,
    title: b.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "bucket"), b.versioning && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "versioning on"), b.objectLock && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "object lock")),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "on volume ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openVolumeById(b.volumeId),
      label: b.volumeName
    }))
  }), a.public && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Bucket is public."), " Anonymous requests can read objects without presenting the service account credentials.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Objects",
    v: fmtNum(b.objects)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Stored",
    v: fmtBytes(b.capacity.used),
    s: b.quota ? `of ${fmtBytes(b.quota)} quota` : "no quota"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Provisioned",
    v: fmtBytes(b.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Snapshots",
    v: b.counts.snapshots
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backups",
    v: b.counts.backups
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Encryption",
    v: b.encrypted ? "on" : "off",
    c: b.encrypted ? "var(--ok)" : undefined
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Backing volume",
    sub: "the filesystem behind the bucket",
    count: "\u2192",
    onClick: () => nav.openVolumeById(b.volumeId)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "pool",
    title: "Pool",
    sub: b.poolName,
    count: "\u2192",
    onClick: () => nav.openPool(b.clusterId, b.poolId)
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Bucket properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", b.name], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: b.status
    })], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(b.clusterId),
      label: regName(b.clusterId)
    })], ["Backing volume", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openVolumeById(b.volumeId),
      label: b.volumeName
    })], ["Pool", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPool(b.clusterId, b.poolId),
      label: b.poolName
    })], ["Versioning", b.versioning ? "enabled" : "disabled"], ["Object lock", b.objectLock ? "enabled" : "disabled"], ["Quota", b.quota ? fmtBytes(b.quota) : "none"], ["Objects", fmtNum(b.objects)], ["Encryption", b.encrypted ? "enabled — keys from the cluster KMS" : "disabled"], ["Replication", b.replication ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openRPolicy && nav.openRPolicy(b.replication.policy_id),
      label: `${b.replication.mode} · ${b.replication.policy_name}`
    }) : "none — attach to a policy from Actions"], ["Created", fmtDate(b.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "S3 metadata"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Region", b.region || "—"], ["Default storage class", b.storageClass], ["Owner", b.owner || "—"], ["CORS", b.cors ? "configured" : "none"], ["Tags", b.counts.tags ? /*#__PURE__*/React.createElement("div", {
      className: "labels tags",
      style: {
        marginTop: 2
      }
    }, Object.entries(b.tags).map(([k, v]) => /*#__PURE__*/React.createElement("span", {
      key: k,
      className: "lab tag"
    }, /*#__PURE__*/React.createElement("i", null, k), v || "—"))) : "none"], ["Lifecycle rules", b.lifecycle.length ? /*#__PURE__*/React.createElement("div", {
      className: "code",
      style: {
        marginTop: 2
      }
    }, b.lifecycle.map(r => `${r.id}\t${r.prefix ? "prefix " + r.prefix : "all objects"}\t${r.expire_days ? "expire after " + r.expire_days + "d" : r.transition_days ? "→ " + r.transition_class + " after " + r.transition_days + "d" : "noncurrent versions expire after " + r.noncurrent_expire_days + "d"}\t${r.status}`).join("\n")) : "none"]]
  }))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Access \xB7 Kubernetes-native"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Service account", `${a.namespace}/${a.service_account}`], ["Credentials secret", a.secret_name], ["Access key id", a.access_key_id], ["Secret access key", "•••••••••••••••• (in the secret)"], ["Policy", a.policy], ["Anonymous access", a.public ? "allowed" : "denied"]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "How this bucket is built"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "One bucket is one filesystem is one ", /*#__PURE__*/React.createElement("b", null, "logical volume"), ". Object data lives on the cluster's own capacity; the object metadata lives in ", /*#__PURE__*/React.createElement("b", null, "FoundationDB"), ", the same state database the control plane uses."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Because the bucket is a volume, everything a volume can do applies to it unchanged \u2014 snapshots, backups to object storage, and both synchronous and asynchronous replication."))))));
}

// ---- cluster panel: file storage -------------------------------------------
function FileStoragePanel({
  cluster,
  nav
}) {
  const f = cluster.fileStorage || {
    enabled: false
  };
  const [busy, setBusy] = useState(false);
  const failover = async () => {
    setBusy(true);
    try {
      await api.clusterFailoverMds(cluster.id);
      window.__toast("Metadata server moving to the next worker");
      window.__refresh();
    } catch (e) {
      window.__toast(e.message);
    }
    setBusy(false);
  };
  if (!f.enabled) return /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "folder",
    s: 24
  }), /*#__PURE__*/React.createElement("b", {
    style: {
      color: "var(--text)"
    }
  }, supported ? "File storage is off" : "File storage is not available here"), /*#__PURE__*/React.createElement("span", {
    style: {
      maxWidth: 520
    }
  }, "Turning it on creates a pNFS filesystem over this cluster's capacity and makes every worker a pNFS client, so pods can take ReadWriteMany claims."), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    style: {
      marginTop: 8
    },
    onClick: () => window.__ui.dialog(fileStorageDialog(cluster), cluster)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Configure file storage"));
  return /*#__PURE__*/React.createElement(React.Fragment, null, f.mds_state === "restarting" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Metadata server restarting on ", f.mds_host ? f.mds_host.hostname : "another worker", "."), " Expected back within ", f.failover_budget_seconds, "s. Clients hold their layouts and keep reading and writing through the data paths \u2014 only new metadata operations block.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Metadata server",
    v: f.mds_state,
    c: f.mds_state === "active" ? "var(--ok)" : "var(--info)",
    s: f.mds_host ? f.mds_host.hostname : "—"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "pNFS clients",
    v: f.client_count || 0,
    s: "every prepared worker"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "RWX claims",
    v: cluster.counts.rwxPvcs,
    s: `of ${f.max_exports} exports`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Filesystem",
    v: f.filesystem,
    s: "pNFS supports XFS only"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Failover budget",
    v: f.failover_budget_seconds + "s"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Standbys",
    v: (f.mds_candidates || []).length
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Metadata service"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    disabled: busy || !(f.mds_candidates || []).length,
    onClick: failover
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "move",
    s: 11
  }), busy ? "Moving…" : "Fail over now")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Role"), /*#__PURE__*/React.createElement("th", null, "Worker"), /*#__PURE__*/React.createElement("th", null, "State"))), /*#__PURE__*/React.createElement("tbody", null, f.mds_host && /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "badge k8s"
  }, "active")), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600
    }
  }, f.mds_host.hostname), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
    status: MDS_STATE[f.mds_state] || "online"
  }))), (f.mds_candidates || []).map(h => /*#__PURE__*/React.createElement("tr", {
    key: h.uuid
  }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "badge"
  }, "standby")), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, h.hostname), /*#__PURE__*/React.createElement("td", {
    style: {
      color: "var(--dim2)"
    }
  }, "ready"))))))), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Export configuration"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["NFS version", f.nfs_version], ["Layout type", f.layout_type], ["Export root", f.export_root], ["Filesystem", f.filesystem], ["Lease", f.lease_seconds + "s"], ["Grace period", f.grace_seconds + "s"], ["Failover budget", f.failover_budget_seconds + "s"], ["Max exports", f.max_exports], ["Active exports", cluster.counts.rwxPvcs]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "How RWX is served"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "A pNFS filesystem sits over the cluster's block capacity. Each worker mounts it as a ", /*#__PURE__*/React.createElement("b", null, "pNFS client"), " and reads and writes directly against the storage nodes \u2014 file data never passes through the NFS server."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "The ", /*#__PURE__*/React.createElement("b", null, "kernel NFS server"), " on one control-plane worker serves metadata only. Its configuration and backend are shared, so it can restart on any standby worker; clients keep their layouts and see a pause of seconds at most."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "The Linux NFS server only supports pNFS on ", /*#__PURE__*/React.createElement("b", null, "XFS"), ", so every ReadWriteMany claim is XFS \u2014 the UI enforces this rather than letting a claim fail at mount time.")))));
}
Object.assign(window, {
  BucketTile,
  BucketDetail,
  FileStoragePanel
});
})();
// ---- recipe.jsx ----
(function(){
// ---------------------------------------------------------------------------
// RAMEN RECIPE — the common subset of ramendr.openshift.io/v1alpha1 Recipe:
//   groups            resource groups (kinds + label selector, in the app namespace)
//   hooks             exec (command in a pod) or check (condition on a resource)
//   captureWorkflow   ordered groups/hooks run when Kubernetes objects are captured
//   recoverWorkflow   ordered groups/hooks run on the standby cluster — the boot sequence
//   failOn            any-error | essential-error | full-error
// The DRPlacementControl references it via kubeObjectProtection.recipeRef.
// Advanced fields (includeClusterResources, inverseOp, essential flags per
// step, recipeParameters) are intentionally left out.
// ---------------------------------------------------------------------------
const FAIL_ON = [{
  v: "any-error",
  l: "any-error — stop at the first failing step"
}, {
  v: "essential-error",
  l: "essential-error — stop only when an essential step fails"
}, {
  v: "full-error",
  l: "full-error — run everything, fail at the end"
}];
const HOOK_RES = ["pod", "deployment", "statefulset"];
const NS_KINDS = ["Deployment", "StatefulSet", "Service", "ConfigMap", "Secret", "Ingress", "PersistentVolumeClaim", "VirtualMachine"];
const selText = ml => Object.entries(ml || {}).map(([k, v]) => `${k}=${v}`).join(", ");
const parseSel = s => String(s || "").split(",").map(x => x.trim()).filter(Boolean).reduce((o, kv) => {
  const [k, ...r] = kv.split("=");
  if (k) o[k.trim()] = r.join("=").trim();
  return o;
}, {});
const stepLabel = s => s.group ? `group · ${s.group}` : `hook · ${s.hook}`;

// ---- read-only card on the protected application ----------------------------
function RecipeCard({
  a
}) {
  const r = a.recipe;
  const hookOf = ref => {
    const [hn, on] = String(ref).split("/");
    const h = (r.hooks || []).find(x => x.name === hn);
    const op = h && [].concat(h.ops || [], h.chks || []).find(o => o.name === on);
    return {
      h,
      op
    };
  };
  const Seq = ({
    wf,
    title
  }) => /*#__PURE__*/React.createElement("div", {
    style: {
      marginBottom: 10
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "mdesc",
    style: {
      marginBottom: 6
    }
  }, /*#__PURE__*/React.createElement("b", null, title), " \xB7 fail on ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, wf.failOn || "any-error")), !(wf.sequence || []).length ? /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: 0
    }
  }, "empty \u2014 all objects in one pass") : /*#__PURE__*/React.createElement("ol", {
    className: "bootseq"
  }, wf.sequence.map((s, i) => {
    const g = s.group && (r.groups || []).find(x => x.name === s.group);
    const {
      h,
      op
    } = s.hook ? hookOf(s.hook) : {};
    return /*#__PURE__*/React.createElement("li", {
      key: i
    }, /*#__PURE__*/React.createElement("span", {
      className: "bootn"
    }, i + 1), /*#__PURE__*/React.createElement("div", {
      className: "bootb"
    }, g ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
      className: "bootl"
    }, /*#__PURE__*/React.createElement("b", null, g.name), /*#__PURE__*/React.createElement("span", {
      className: "bootk"
    }, (g.includedResourceTypes || []).join(" · ") || "all kinds")), /*#__PURE__*/React.createElement("div", {
      className: "bootw"
    }, g.labelSelector && Object.keys(g.labelSelector.matchLabels || {}).length ? selText(g.labelSelector.matchLabels) : "every object in the namespace")) : h ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
      className: "bootl"
    }, /*#__PURE__*/React.createElement("b", null, h.name, "/", op ? op.name : "?"), /*#__PURE__*/React.createElement("span", {
      className: "bootk"
    }, h.type, " hook \xB7 ", h.selectResource, " ", selText(h.labelSelector && h.labelSelector.matchLabels))), /*#__PURE__*/React.createElement("div", {
      className: "boothook"
    }, /*#__PURE__*/React.createElement("i", null, h.type), op ? h.type === "exec" ? op.command : op.condition : "", op && op.timeout ? ` · ≤${op.timeout}s` : "", op && op.onError ? ` · on error: ${op.onError}` : "")) : /*#__PURE__*/React.createElement("div", {
      className: "bootl"
    }, /*#__PURE__*/React.createElement("b", {
      style: {
        color: "var(--bad)"
      }
    }, stepLabel(s), " \u2014 not defined"))));
  })));
  return /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Recipe \xB7 recover and capture workflows"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 6
    }
  }, !a.kubeObjectProtection && /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginBottom: 8
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "Kubernetes object protection is off \u2014 only the PVCs fail over; the recipe is stored but not executed."), !r ? /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "No Recipe referenced: Ramen captures and restores all namespace objects in one pass, without ordering or hooks. Edit the recipe from Actions.") : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "labels",
    style: {
      marginBottom: 10
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "recipeRef"), r.namespace, "/", r.name), r.appType && /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "appType"), r.appType), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "groups"), (r.groups || []).length), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "hooks"), (r.hooks || []).length)), /*#__PURE__*/React.createElement(Seq, {
    wf: r.recoverWorkflow || {},
    title: "Recover workflow \u2014 boot sequence on the standby cluster"
  }), /*#__PURE__*/React.createElement(Seq, {
    wf: r.captureWorkflow || {},
    title: "Capture workflow \u2014 before Kubernetes objects are backed up"
  }), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginBottom: 0
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "Objects of kinds not covered by any group are restored after the last step. Ramen reads the Recipe from the application namespace on both managed clusters."))));
}

// ---- the editor (a form field type) ----------------------------------------
function RecipeField({
  f,
  val,
  setVal
}) {
  const r = val || {};
  const set = patch => setVal(Object.assign({}, r, patch));
  const groups = r.groups || [],
    hooks = r.hooks || [];
  const [disc, setDisc] = useState(null); // {kind: {items|error|loading}}
  const [tab, setTab] = useState("recover");
  const discover = () => {
    const init = {};
    NS_KINDS.forEach(k => {
      init[k] = {
        loading: true
      };
    });
    setDisc(init);
    api.nsResourcesEach(f.namespace, (kind, items, error) => setDisc(d => Object.assign({}, d, {
      [kind]: {
        items: items || [],
        error
      }
    })));
  };
  const commonLabels = items => {
    const keys = ["app.kubernetes.io/name", "app.kubernetes.io/instance", "app"];
    for (const k of keys) {
      const vals = [...new Set(items.map(i => (i.metadata.labels || {})[k]).filter(Boolean))];
      if (vals.length === 1) return {
        [k]: vals[0]
      };
    }
    return {};
  };
  const addGroupFromKind = (kind, items) => {
    const name = kind.toLowerCase() + "s";
    if (groups.some(g => g.name === name)) return;
    set({
      groups: groups.concat({
        name,
        type: "resource",
        includedResourceTypes: [kind],
        labelSelector: {
          matchLabels: commonLabels(items)
        }
      }),
      recoverWorkflow: Object.assign({
        failOn: "any-error"
      }, r.recoverWorkflow, {
        sequence: ((r.recoverWorkflow || {}).sequence || []).concat({
          group: name
        })
      })
    });
  };
  const setG = (i, patch) => set({
    groups: groups.map((g, j) => j === i ? Object.assign({}, g, patch) : g)
  });
  const setH = (i, patch) => set({
    hooks: hooks.map((h, j) => j === i ? Object.assign({}, h, patch) : h)
  });
  const opOf = h => (h.type === "check" ? (h.chks || [])[0] : (h.ops || [])[0]) || {};
  const setOp = (i, patch) => {
    const h = hooks[i];
    const cur = opOf(h);
    const op = Object.assign({
      name: cur.name || "run",
      timeout: 300,
      onError: "fail"
    }, cur, patch);
    setH(i, h.type === "check" ? {
      chks: [op],
      ops: []
    } : {
      ops: [op],
      chks: []
    });
  };
  const wf = r[tab + "Workflow"] || {
    failOn: "any-error",
    sequence: []
  };
  const setWf = patch => set({
    [tab + "Workflow"]: Object.assign({}, wf, patch)
  });
  const seq = wf.sequence || [];
  const stepOptions = [...groups.map(g => ({
    v: "group:" + g.name,
    l: `group · ${g.name}`
  })), ...hooks.flatMap(h => {
    const ops = [].concat(h.ops || [], h.chks || []);
    return (ops.length ? ops : [{
      name: "run"
    }]).map(o => ({
      v: "hook:" + h.name + "/" + (o.name || "run"),
      l: `hook · ${h.name}/${o.name || "run"}`
    }));
  })];
  const stepKey = s => s.group ? "group:" + s.group : "hook:" + s.hook;
  const fromKey = k => k.startsWith("group:") ? {
    group: k.slice(6)
  } : {
    hook: k.slice(5)
  };
  const move = (i, d) => {
    const j = i + d;
    if (j < 0 || j >= seq.length) return;
    const n = seq.slice();
    [n[i], n[j]] = [n[j], n[i]];
    setWf({
      sequence: n
    });
  };
  const Row = ({
    children,
    className
  }) => /*#__PURE__*/React.createElement("div", {
    className: "schedrow rcp" + (className ? " " + className : "")
  }, children);
  const Del = ({
    onClick
  }) => /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "kebab",
    title: "Remove",
    onClick: onClick
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "x",
    s: 11
  }));
  return /*#__PURE__*/React.createElement("div", {
    className: "field"
  }, /*#__PURE__*/React.createElement("div", {
    className: "frow",
    style: {
      marginBottom: 6
    }
  }, /*#__PURE__*/React.createElement("label", {
    className: "fl sm"
  }, "Recipe name", /*#__PURE__*/React.createElement("input", {
    className: "finput sm",
    value: r.name || "",
    onChange: e => set({
      name: e.target.value
    }),
    placeholder: "postgres-recipe"
  })), /*#__PURE__*/React.createElement("label", {
    className: "fl sm"
  }, "Namespace", /*#__PURE__*/React.createElement("input", {
    className: "finput sm",
    value: f.namespace,
    disabled: true
  })), /*#__PURE__*/React.createElement("label", {
    className: "fl sm"
  }, "appType", /*#__PURE__*/React.createElement("input", {
    className: "finput sm",
    value: r.appType || "",
    onChange: e => set({
      appType: e.target.value
    }),
    placeholder: "postgres"
  }))), /*#__PURE__*/React.createElement("div", {
    className: "rcph"
  }, /*#__PURE__*/React.createElement("span", {
    className: "flabel"
  }, "Resources in ", f.namespace), /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "chip",
    onClick: discover
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 11
  }), disc ? "Re-discover" : "Discover via Kubernetes API")), disc && /*#__PURE__*/React.createElement("div", {
    className: "rcpdisc"
  }, NS_KINDS.map(k => {
    const d = disc[k] || {};
    return /*#__PURE__*/React.createElement("div", {
      key: k,
      className: "rcpkind"
    }, /*#__PURE__*/React.createElement("b", null, k), d.loading ? /*#__PURE__*/React.createElement("span", {
      className: "dots"
    }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)) : d.error ? /*#__PURE__*/React.createElement("span", {
      className: "mdesc",
      style: {
        margin: 0,
        color: "var(--warn)"
      }
    }, "not available") : !d.items.length ? /*#__PURE__*/React.createElement("span", {
      className: "mdesc",
      style: {
        margin: 0
      }
    }, "none") : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11,
        color: "var(--dim)"
      }
    }, d.items.map(i => i.metadata.name).join(", ")), k !== "PersistentVolumeClaim" && /*#__PURE__*/React.createElement("button", {
      type: "button",
      className: "chip",
      disabled: groups.some(g => g.name === k.toLowerCase() + "s"),
      onClick: () => addGroupFromKind(k, d.items)
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 10
    }), "group")));
  }), /*#__PURE__*/React.createElement("span", {
    className: "fhint"
  }, "PVCs are protected through the VRG's PVC selector, not a recipe group. Adding a group appends it to the recover workflow.")), /*#__PURE__*/React.createElement("div", {
    className: "rcph"
  }, /*#__PURE__*/React.createElement("span", {
    className: "flabel"
  }, "Groups ", /*#__PURE__*/React.createElement("em", null, "(", groups.length, ")")), /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "chip",
    onClick: () => set({
      groups: groups.concat({
        name: "",
        type: "resource",
        includedResourceTypes: [],
        labelSelector: {
          matchLabels: {}
        }
      })
    })
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 11
  }), "Add group")), /*#__PURE__*/React.createElement("div", {
    className: "schedbox"
  }, /*#__PURE__*/React.createElement(Row, null, /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "name"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "resource kinds"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "label selector"), /*#__PURE__*/React.createElement("span", null)), groups.map((g, i) => /*#__PURE__*/React.createElement(Row, {
    key: i
  }, /*#__PURE__*/React.createElement("input", {
    className: "finput sm",
    placeholder: "config",
    value: g.name,
    onChange: e => setG(i, {
      name: e.target.value
    })
  }), /*#__PURE__*/React.createElement("input", {
    className: "finput sm",
    placeholder: "Secret, ConfigMap",
    value: (g.includedResourceTypes || []).join(", "),
    onChange: e => setG(i, {
      includedResourceTypes: e.target.value.split(",").map(x => x.trim()).filter(Boolean)
    })
  }), /*#__PURE__*/React.createElement("input", {
    className: "finput sm mono",
    placeholder: "app.kubernetes.io/name=postgres",
    value: selText((g.labelSelector || {}).matchLabels),
    onChange: e => setG(i, {
      labelSelector: {
        matchLabels: parseSel(e.target.value)
      }
    })
  }), /*#__PURE__*/React.createElement(Del, {
    onClick: () => set({
      groups: groups.filter((_, j) => j !== i)
    })
  }))), !groups.length && /*#__PURE__*/React.createElement("span", {
    className: "fhint"
  }, "No groups: every object in the namespace is one implicit group.")), /*#__PURE__*/React.createElement("div", {
    className: "rcph"
  }, /*#__PURE__*/React.createElement("span", {
    className: "flabel"
  }, "Hooks ", /*#__PURE__*/React.createElement("em", null, "(", hooks.length, ")")), /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "chip",
    onClick: () => set({
      hooks: hooks.concat({
        name: "",
        type: "exec",
        selectResource: "pod",
        labelSelector: {
          matchLabels: {}
        },
        ops: [{
          name: "run",
          command: "",
          timeout: 300,
          onError: "fail"
        }],
        chks: []
      })
    })
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 11
  }), "Add hook")), /*#__PURE__*/React.createElement("div", {
    className: "schedbox"
  }, /*#__PURE__*/React.createElement(Row, {
    className: "hk"
  }, /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "name"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "type"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "on"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "label selector"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "command / condition"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "timeout s"), /*#__PURE__*/React.createElement("span", {
    className: "sl"
  }, "on error"), /*#__PURE__*/React.createElement("span", null)), hooks.map((h, i) => {
    const op = opOf(h);
    return /*#__PURE__*/React.createElement("div", {
      className: "schedrow rcp hk",
      key: i
    }, /*#__PURE__*/React.createElement("input", {
      className: "finput sm",
      placeholder: "quiesce",
      value: h.name,
      onChange: e => setH(i, {
        name: e.target.value
      })
    }), /*#__PURE__*/React.createElement("select", {
      className: "finput sm",
      value: h.type,
      onChange: e => {
        const t = e.target.value;
        setH(i, {
          type: t,
          ops: t === "exec" ? [Object.assign({
            name: "run",
            timeout: 300,
            onError: "fail"
          }, op, {
            condition: undefined
          })] : [],
          chks: t === "check" ? [Object.assign({
            name: "ready",
            timeout: 300,
            onError: "fail"
          }, op, {
            command: undefined
          })] : []
        });
      }
    }, /*#__PURE__*/React.createElement("option", {
      value: "exec"
    }, "exec"), /*#__PURE__*/React.createElement("option", {
      value: "check"
    }, "check")), /*#__PURE__*/React.createElement("select", {
      className: "finput sm",
      value: h.selectResource || "pod",
      onChange: e => setH(i, {
        selectResource: e.target.value
      })
    }, HOOK_RES.map(x => /*#__PURE__*/React.createElement("option", {
      key: x
    }, x))), /*#__PURE__*/React.createElement("input", {
      className: "finput sm mono",
      placeholder: "app=postgres",
      value: selText((h.labelSelector || {}).matchLabels),
      onChange: e => setH(i, {
        labelSelector: {
          matchLabels: parseSel(e.target.value)
        }
      })
    }), /*#__PURE__*/React.createElement("input", {
      className: "finput sm mono",
      placeholder: h.type === "exec" ? "psql -c CHECKPOINT" : "{$.status.readyReplicas} == {$.spec.replicas}",
      value: h.type === "exec" ? op.command || "" : op.condition || "",
      onChange: e => setOp(i, h.type === "exec" ? {
        command: e.target.value
      } : {
        condition: e.target.value
      })
    }), /*#__PURE__*/React.createElement("input", {
      className: "finput sm",
      type: "number",
      min: "1",
      value: op.timeout || 300,
      onChange: e => setOp(i, {
        timeout: Number(e.target.value)
      })
    }), /*#__PURE__*/React.createElement("select", {
      className: "finput sm",
      value: op.onError || "fail",
      onChange: e => setOp(i, {
        onError: e.target.value
      })
    }, /*#__PURE__*/React.createElement("option", {
      value: "fail"
    }, "fail"), /*#__PURE__*/React.createElement("option", {
      value: "continue"
    }, "continue")), /*#__PURE__*/React.createElement(Del, {
      onClick: () => set({
        hooks: hooks.filter((_, j) => j !== i)
      })
    }));
  }), !hooks.length && /*#__PURE__*/React.createElement("span", {
    className: "fhint"
  }, "exec runs a command in the selected pods; check waits for a condition on the selected resource.")), /*#__PURE__*/React.createElement("div", {
    className: "rcph"
  }, /*#__PURE__*/React.createElement("div", {
    className: "seg",
    style: {
      height: 26
    }
  }, /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: tab === "recover" ? "on" : "",
    onClick: () => setTab("recover")
  }, "Recover workflow"), /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: tab === "capture" ? "on" : "",
    onClick: () => setTab("capture")
  }, "Capture workflow")), /*#__PURE__*/React.createElement("select", {
    className: "finput sm",
    style: {
      width: "auto",
      flex: "none"
    },
    value: wf.failOn || "any-error",
    onChange: e => setWf({
      failOn: e.target.value
    })
  }, FAIL_ON.map(o => /*#__PURE__*/React.createElement("option", {
    key: o.v,
    value: o.v
  }, o.l))), /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "chip",
    disabled: !stepOptions.length,
    onClick: () => setWf({
      sequence: seq.concat(fromKey(stepOptions[0].v))
    })
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 11
  }), "Add step")), /*#__PURE__*/React.createElement("div", {
    className: "schedbox"
  }, seq.map((s, i) => /*#__PURE__*/React.createElement("div", {
    className: "schedrow rcp sq",
    key: i
  }, /*#__PURE__*/React.createElement("span", {
    className: "sl mono"
  }, i + 1), /*#__PURE__*/React.createElement("select", {
    className: "finput sm",
    value: stepKey(s),
    onChange: e => setWf({
      sequence: seq.map((x, j) => j === i ? fromKey(e.target.value) : x)
    })
  }, !stepOptions.some(o => o.v === stepKey(s)) && /*#__PURE__*/React.createElement("option", {
    value: stepKey(s)
  }, stepLabel(s), " (undefined)"), stepOptions.map(o => /*#__PURE__*/React.createElement("option", {
    key: o.v,
    value: o.v
  }, o.l))), /*#__PURE__*/React.createElement("span", {
    style: {
      display: "flex",
      gap: 2
    }
  }, /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "kebab",
    disabled: i === 0,
    onClick: () => move(i, -1)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "chevu",
    s: 11
  })), /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "kebab",
    disabled: i === seq.length - 1,
    onClick: () => move(i, 1)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "chevd",
    s: 11
  })), /*#__PURE__*/React.createElement(Del, {
    onClick: () => setWf({
      sequence: seq.filter((_, j) => j !== i)
    })
  })))), !seq.length && /*#__PURE__*/React.createElement("span", {
    className: "fhint"
  }, tab === "recover" ? "Empty: objects are restored in one pass, no ordering." : "Empty: objects are captured as they are, no quiesce.")));
}
Object.assign(window, {
  RecipeCard,
  RecipeField,
  FAIL_ON
});
})();
// ---- migrate.jsx ----
(function(){
// ---------------------------------------------------------------------------
// MIGRATION PATHS — online migration of VMs, containers and their volumes from
// site A to site B inside a stretched Kubernetes cluster.
// ---------------------------------------------------------------------------
const clockOfM = s => {
  const d = new Date(s);
  return isNaN(d) ? "—" : d.toISOString().slice(11, 19);
};
const MPH = window.MIG_PHASES || ["Queued", "Replicating", "Converged", "MovingWorkloads", "MigratingVolumes", "Cleanup", "Completed"];
const PH_LABEL = {
  Queued: "queued",
  Replicating: "replicating",
  Converged: "converged",
  MovingWorkloads: "moving workloads",
  MigratingVolumes: "migrating volumes",
  Cleanup: "cleanup",
  Completed: "completed"
};
const PH_HINT = {
  Queued: "waits for the groups ahead of it in the queue",
  Replicating: "an asynchronous replication policy copies the group's volumes to the target site; the backlog per volume shrinks until it is zero",
  Converged: "every volume's backlog is zero — the workloads can move without losing data",
  MovingWorkloads: "VMs are live-migrated with KubeVirt, containers are rescheduled to the target site; storage still comes from site A",
  MigratingVolumes: "instant volume migration moves each volume's primary to the target nodes while it is in use",
  Cleanup: "the source copies and the replication policy are deleted",
  Completed: "the group runs entirely at the target site"
};
const phaseIdx = ph => MPH.indexOf(ph);
const PhaseStrip = ({
  phase,
  compact
}) => {
  const i = phaseIdx(phase);
  return /*#__PURE__*/React.createElement("div", {
    className: "phases"
  }, MPH.filter(p => p !== "Queued" || !compact).map((p, k) => {
    const j = phaseIdx(p);
    const cls = "ph" + (phase === "Paused" ? "" : j < i ? " done" : j === i ? " cur" : "");
    return /*#__PURE__*/React.createElement(React.Fragment, {
      key: p
    }, k > 0 && /*#__PURE__*/React.createElement("span", {
      className: "arr"
    }, "\u203A"), /*#__PURE__*/React.createElement("span", {
      className: cls
    }, j < i && phase !== "Paused" ? /*#__PURE__*/React.createElement(Icon, {
      n: "check",
      s: 9
    }) : null, PH_LABEL[p]));
  }));
};

// ---- dialogs -----------------------------------------------------------------
const newMPathDialog = () => ({
  title: "New migration path",
  confirm: "Create path",
  desc: "A path moves application groups from a source storage cluster to a target storage cluster inside one Kubernetes cluster that spans both sites. The target site's nodes must already be part of that Kubernetes cluster and have a storage class on the target storage cluster.",
  fields: v => [{
    k: "name",
    label: "Name",
    type: "text",
    required: true,
    placeholder: "site-a-to-site-b"
  }, {
    k: "k8s_cluster_id",
    label: "Stretched Kubernetes cluster",
    type: "select",
    required: true,
    load: () => api.k8sClusters().then(ks => ks.filter(k => k.storageClusterIds.length >= 2).map(k => ({
      v: k.id,
      l: `${k.name} · ${k.storageClusterIds.length} storage clusters`
    }))),
    empty: "No Kubernetes cluster consumes two storage clusters yet — add nodes at the target site first."
  }, {
    k: "source_cluster_id",
    label: "Source storage cluster (site A)",
    type: "select",
    required: true,
    load: () => api.clusters().then(cs => cs.map(c => ({
      v: c.id,
      l: c.name
    })))
  }, {
    k: "target_cluster_id",
    label: "Target storage cluster (site B)",
    type: "select",
    required: true,
    load: () => api.clusters().then(cs => cs.map(c => ({
      v: c.id,
      l: c.name
    })))
  }, {
    k: "n1",
    type: "note",
    label: "Volumes replicate asynchronously to B first; the move itself happens only once a group's backlog is zero."
  }],
  run: v => api.mpathCreate({
    name: v.name,
    k8s_cluster_id: v.k8s_cluster_id,
    source_cluster_id: v.source_cluster_id,
    target_cluster_id: v.target_cluster_id
  })
});
const newAppGroupDialog = p => ({
  title: `Add an application group to ${p.name || "the path"}`,
  confirm: "Queue group",
  wide: true,
  desc: "An application group is one unit of migration: its VMs and containers, and — identified from them — their PVCs and volumes. Groups run in queue order.",
  fields: [{
    k: "name",
    label: "Group name",
    type: "text",
    required: true,
    placeholder: "payments"
  }, {
    k: "members",
    type: "members",
    k8sClusterId: p.k8sClusterId,
    label: "Workloads"
  }, {
    k: "approval",
    label: "When replication has converged",
    type: "select",
    def: "manual",
    options: [{
      v: "manual",
      l: "Wait for approval before moving the workloads"
    }, {
      v: "auto",
      l: "Move the workloads automatically"
    }]
  }],
  run: v => api.appGroupCreate(p.id, {
    name: v.name,
    namespace: (v.members || {}).namespace,
    members: (v.members || {}).members || [],
    approval: v.approval
  })
});

// namespace + discovered workloads (via the Kubernetes API), picked one by one
function MembersField({
  f,
  val,
  setVal
}) {
  const v = val || {
    namespace: "",
    members: []
  };
  const [disc, setDisc] = useState(null);
  const {
    data: k
  } = useResource("mk8s|" + f.k8sClusterId, () => api.k8sCluster(f.k8sClusterId));
  const nss = k && k.namespaces || [];
  const discover = ns => {
    const init = {};
    ["Deployment", "StatefulSet", "VirtualMachine"].forEach(x => {
      init[x] = {
        loading: true
      };
    });
    setDisc(init);
    api.nsResourcesEach(ns, (kind, items, error) => {
      if (init[kind]) setDisc(d => Object.assign({}, d, {
        [kind]: {
          items: items || [],
          error
        }
      }));
    });
  };
  const has = (kind, name) => v.members.some(m => m.kind === kind && m.name === name);
  const toggle = (kind, name) => setVal(Object.assign({}, v, {
    members: has(kind, name) ? v.members.filter(m => !(m.kind === kind && m.name === name)) : v.members.concat({
      kind,
      name
    })
  }));
  return /*#__PURE__*/React.createElement("div", {
    className: "field"
  }, /*#__PURE__*/React.createElement("span", {
    className: "flabel"
  }, "Namespace"), /*#__PURE__*/React.createElement("div", {
    style: {
      display: "flex",
      gap: 6
    }
  }, /*#__PURE__*/React.createElement("select", {
    className: "finput",
    value: v.namespace,
    onChange: e => {
      setVal({
        namespace: e.target.value,
        members: []
      });
      setDisc(null);
    }
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "choose\u2026"), nss.map(n => /*#__PURE__*/React.createElement("option", {
    key: n
  }, n))), /*#__PURE__*/React.createElement("button", {
    type: "button",
    className: "chip",
    disabled: !v.namespace,
    onClick: () => discover(v.namespace)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 11
  }), disc ? "Re-discover" : "Discover workloads")), disc && /*#__PURE__*/React.createElement("div", {
    className: "rcpdisc",
    style: {
      marginTop: 8
    }
  }, ["VirtualMachine", "StatefulSet", "Deployment"].map(kind => {
    const d = disc[kind] || {};
    return /*#__PURE__*/React.createElement("div", {
      key: kind,
      className: "rcpkind"
    }, /*#__PURE__*/React.createElement("b", null, kind), d.loading ? /*#__PURE__*/React.createElement("span", {
      className: "dots"
    }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)) : d.error ? /*#__PURE__*/React.createElement("span", {
      className: "mdesc",
      style: {
        margin: 0,
        color: "var(--warn)"
      }
    }, "not available") : !d.items.length ? /*#__PURE__*/React.createElement("span", {
      className: "mdesc",
      style: {
        margin: 0
      }
    }, "none") : /*#__PURE__*/React.createElement("span", {
      style: {
        flex: 1,
        minWidth: 0,
        display: "flex",
        flexWrap: "wrap"
      }
    }, d.items.map(i => /*#__PURE__*/React.createElement("label", {
      key: i.metadata.name
    }, /*#__PURE__*/React.createElement("span", {
      className: "selbox" + (has(kind, i.metadata.name) ? " on" : ""),
      onClick: () => toggle(kind, i.metadata.name)
    }, has(kind, i.metadata.name) && /*#__PURE__*/React.createElement(Icon, {
      n: "check",
      s: 10
    })), /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11
      }
    }, i.metadata.name)))));
  }), /*#__PURE__*/React.createElement("span", {
    className: "fhint"
  }, v.members.length, " workload(s) selected \xB7 VMs are live-migrated, containers are restarted on the target site. Their PVCs are resolved when the group is queued.")), !disc && /*#__PURE__*/React.createElement("span", {
    className: "fhint"
  }, "Pick the namespace, then discover its VMs, StatefulSets and Deployments."));
}

// ---- tiles -------------------------------------------------------------------
function MPathTile({
  m: p,
  nav
}) {
  const {
    data: gs
  } = useResource("mpg|" + p.id, () => api.mpathGroups(p.id), 3000);
  const G = gs || [];
  const cur = G.find(g => !["Queued", "Completed", "Failed"].includes(g.phase));
  const done = G.filter(g => g.phase === "Completed").length;
  const backlog = G.reduce((n, g) => n + g.backlog, 0);
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[p.status].c
    },
    onDoubleClick: () => nav.detail(p)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: p,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    }), /*#__PURE__*/React.createElement(Name, null, p.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, done, "/", G.length, " groups")
  }), /*#__PURE__*/React.createElement(Uuid, {
    value: p.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openCluster(p.sourceClusterId);
    }
  }, /*#__PURE__*/React.createElement("i", null, "from"), regName(p.sourceClusterId)), /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      border: "none",
      background: "none",
      padding: 0
    }
  }, "\u2192"), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openCluster(p.targetClusterId);
    }
  }, /*#__PURE__*/React.createElement("i", null, "to"), regName(p.targetClusterId)), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.k8sDetail(p.k8sClusterId);
    }
  }, /*#__PURE__*/React.createElement("i", null, "k8s"), regName(p.k8sClusterId, "Kubernetes cluster"))), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Queued"), /*#__PURE__*/React.createElement("b", null, G.filter(g => g.phase === "Queued").length)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Completed"), /*#__PURE__*/React.createElement("b", null, done)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Backlog"), /*#__PURE__*/React.createElement("b", {
    style: backlog ? null : {
      color: "var(--ok)"
    }
  }, fmtBytes(backlog))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Volumes"), /*#__PURE__*/React.createElement("b", null, G.reduce((n, g) => n + g.counts.volumes, 0)))), cur ? /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, /*#__PURE__*/React.createElement("b", null, cur.name), " \xB7 ", PH_LABEL[cur.phase] || cur.phase, " \u2014 ", cur.message)) : p.status === "paused" ? /*#__PURE__*/React.createElement("div", {
    className: "prepbox"
  }, "Paused \u2014 nothing starts until the path is resumed.") : !G.length ? /*#__PURE__*/React.createElement("div", {
    className: "prepbox"
  }, "No application groups yet. Add one to start moving workloads.") : null, /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Groups",
      count: G.length,
      icon: "cluster",
      onClick: () => nav.layer(p, "appgroups")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(p)
    }]
  }));
}
function AppGroupTile({
  g,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[g.phase].c
    },
    onDoubleClick: () => nav.detail(g)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: g,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: g.phase
    }), /*#__PURE__*/React.createElement(Name, null, g.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "#", g.order + 1)
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, "namespace ", g.namespace), /*#__PURE__*/React.createElement(Uuid, {
    value: g.id
  }), /*#__PURE__*/React.createElement(PhaseStrip, {
    phase: g.phase,
    compact: true
  }), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "VMs"), /*#__PURE__*/React.createElement("b", null, g.counts.vms)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Containers"), /*#__PURE__*/React.createElement("b", null, g.counts.members - g.counts.vms)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Volumes"), /*#__PURE__*/React.createElement("b", null, g.counts.volumes)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Backlog"), /*#__PURE__*/React.createElement("b", {
    style: g.backlog ? null : {
      color: "var(--ok)"
    }
  }, fmtBytes(g.backlog)))), g.message && g.phase !== "Completed" && /*#__PURE__*/React.createElement("div", {
    className: "prepbox" + (["Queued", "Paused", "Failed"].includes(g.phase) ? "" : " running")
  }, g.message, g.phase === "Converged" && g.approval === "manual" ? " — approve the move from Actions." : ""), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "Volumes",
      count: g.counts.volumes,
      icon: "volume",
      onClick: () => nav.layer(g, "volumes")
    }, g.phase === "Converged" && g.approval === "manual" ? {
      label: "Move now",
      icon: "move",
      onClick: () => window.__ui.dialog(ACTIONS.appgroup(g).find(a => /Move/.test(a.label)).dialog, g)
    } : null, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(g)
    }]
  }));
}

// ---- details -----------------------------------------------------------------
function MPathDetail({
  o: p,
  nav
}) {
  const {
    data: gs,
    reload
  } = useResource("mpgd|" + p.id, () => api.mpathGroups(p.id), 2500);
  const [q, setQ] = useState("");
  const [gf, setGf] = useState("");
  const G = gs || [];
  const cur = G.find(g => !["Queued", "Completed", "Failed"].includes(g.phase));
  const done = G.filter(g => g.phase === "Completed").length;
  const backlog = G.reduce((n, g) => n + g.backlog, 0);
  const move = async (i, d) => {
    const j = i + d;
    if (j < 0 || j >= G.length) return;
    const ids = G.map(g => g.id);
    [ids[i], ids[j]] = [ids[j], ids[i]];
    try {
      await api.mpathReorder(p.id, ids);
      reload();
    } catch (e) {
      window.__toast && window.__toast(e.message);
    }
  };
  const lines = p.log.filter(l => (!gf || l.groupId === gf) && (!q || l.msg.toLowerCase().includes(q.toLowerCase()))).slice().reverse();
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: p,
    title: p.name,
    badge: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, done, "/", G.length, " groups completed"),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "labels"
    }, /*#__PURE__*/React.createElement("button", {
      className: "lab link",
      onClick: () => nav.openCluster(p.sourceClusterId)
    }, /*#__PURE__*/React.createElement("i", null, "site A"), regName(p.sourceClusterId), p.sourceZoneId ? ` · ${regName(p.sourceZoneId, "zone")}` : ""), /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)"
      }
    }, "\u2192"), /*#__PURE__*/React.createElement("button", {
      className: "lab link",
      onClick: () => nav.openCluster(p.targetClusterId)
    }, /*#__PURE__*/React.createElement("i", null, "site B"), regName(p.targetClusterId), p.targetZoneId ? ` · ${regName(p.targetZoneId, "zone")}` : ""), /*#__PURE__*/React.createElement("button", {
      className: "lab link",
      onClick: () => nav.k8sDetail(p.k8sClusterId)
    }, /*#__PURE__*/React.createElement("i", null, "kubernetes"), regName(p.k8sClusterId, "Kubernetes cluster")))
  }), p.status === "paused" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Paused."), " A group already moving finishes its current step; no new step and no new group starts until the path is resumed.")), cur && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)"
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, cur.name, " \u2014 ", PH_LABEL[cur.phase] || cur.phase, "."), " ", cur.message), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    style: {
      marginLeft: "auto"
    },
    onClick: () => nav.detail(cur)
  }, "Open group")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Status",
    v: /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    }),
    s: cur ? `${cur.name} in progress` : p.status === "completed" ? "all groups migrated" : "idle"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Application groups",
    v: G.length,
    s: `${done} completed · ${G.filter(g => g.phase === "Queued").length} queued`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Workloads",
    v: G.reduce((n, g) => n + g.counts.members, 0),
    s: `${G.reduce((n, g) => n + g.counts.vms, 0)} VMs`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Volumes",
    v: G.reduce((n, g) => n + g.counts.volumes, 0),
    s: `${G.reduce((n, g) => n + g.counts.migrated, 0)} at site B`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backlog",
    v: fmtBytes(backlog),
    c: backlog ? "var(--warn)" : "var(--ok)",
    s: "remaining to replicate"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Data",
    v: fmtBytes(G.reduce((n, g) => n + g.capacity.total, 0)),
    s: "across all groups"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Queue"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    onClick: () => window.__ui.dialog(newAppGroupDialog(p), p)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Add application group")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, !G.length && /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "8px 0"
    }
  }, "No groups. Add the first application group \u2014 its VMs and containers, their PVCs are resolved automatically."), G.map((g, i) => /*#__PURE__*/React.createElement("div", {
    key: g.id,
    className: "qrow" + (cur && cur.id === g.id ? " cur" : "")
  }, /*#__PURE__*/React.createElement("span", {
    className: "qn"
  }, g.phase === "Completed" ? /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 10
  }) : i + 1), /*#__PURE__*/React.createElement("span", {
    className: "qname"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    style: {
      border: "none",
      background: "none",
      padding: 0,
      fontSize: 12.5,
      color: "var(--text)",
      fontFamily: "inherit"
    },
    onClick: () => nav.detail(g)
  }, g.name), /*#__PURE__*/React.createElement("small", null, g.namespace, " \xB7 ", g.counts.vms, " VM \xB7 ", g.counts.members - g.counts.vms, " containers \xB7 ", g.counts.volumes, " volumes")), /*#__PURE__*/React.createElement("span", {
    className: "qmeta"
  }, /*#__PURE__*/React.createElement(TrafficLight, {
    status: g.phase
  })), /*#__PURE__*/React.createElement("span", {
    className: "qmeta"
  }, g.phase === "Replicating" ? `backlog ${fmtBytes(g.backlog)}` : g.phase === "MovingWorkloads" ? `${g.counts.moved}/${g.counts.members} moved` : g.phase === "MigratingVolumes" ? `${g.counts.migrated}/${g.counts.volumes} migrated` : g.phase === "Converged" ? g.approval === "manual" ? "awaiting approval" : "auto" : g.finishedAt ? fmtAgo(g.finishedAt) : g.approval), /*#__PURE__*/React.createElement("span", {
    style: {
      display: "flex",
      gap: 2,
      justifyContent: "flex-end"
    }
  }, g.phase === "Queued" && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("button", {
    className: "kebab",
    disabled: i === 0 || G[i - 1].phase !== "Queued",
    onClick: () => move(i, -1),
    title: "Earlier"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "chevu",
    s: 11
  })), /*#__PURE__*/React.createElement("button", {
    className: "kebab",
    disabled: i === G.length - 1,
    onClick: () => move(i, 1),
    title: "Later"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "chevd",
    s: 11
  }))), /*#__PURE__*/React.createElement("button", {
    className: "kebab",
    onClick: () => nav.detail(g),
    title: "Open"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "chev",
    s: 11
  }))))))), cur && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Current group \xB7 ", cur.name), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Workloads"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(Members, {
    g: cur
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Volumes and backlog"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement(VolumeBacklog, {
    g: cur,
    nav: nav
  }))))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Migration log", /*#__PURE__*/React.createElement("span", {
    className: "live",
    style: {
      marginLeft: 8
    }
  }, /*#__PURE__*/React.createElement("i", null), "live")), /*#__PURE__*/React.createElement("div", {
    className: "logtools"
  }, /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: gf,
    onChange: e => setGf(e.target.value)
  }, /*#__PURE__*/React.createElement("option", {
    value: ""
  }, "all groups"), G.map(g => /*#__PURE__*/React.createElement("option", {
    key: g.id,
    value: g.id
  }, g.name))), /*#__PURE__*/React.createElement("input", {
    className: "inp",
    style: {
      width: 200
    },
    placeholder: "search",
    value: q,
    onChange: e => setQ(e.target.value)
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: "auto",
      fontSize: 11,
      color: "var(--dim2)"
    }
  }, lines.length, " of ", p.log.length), /*#__PURE__*/React.createElement(CopyBtn, {
    get: () => lines.map(l => `${l.ts} ${l.level} ${l.msg}`).join("\n")
  })), /*#__PURE__*/React.createElement(LogStream, {
    lines: lines,
    loading: false,
    height: 280,
    empty: "Nothing logged yet."
  })));
}
const Members = ({
  g
}) => /*#__PURE__*/React.createElement("div", {
  className: "memlist"
}, g.members.map(m => /*#__PURE__*/React.createElement("div", {
  key: m.kind + m.name,
  className: "memrow"
}, /*#__PURE__*/React.createElement("span", {
  className: "mk"
}, m.kind), /*#__PURE__*/React.createElement("b", {
  style: {
    flex: 1,
    minWidth: 0,
    overflow: "hidden",
    textOverflow: "ellipsis"
  }
}, m.name), ["LiveMigrating", "Restarting"].includes(m.state) && /*#__PURE__*/React.createElement("div", {
  className: "sbar"
}, /*#__PURE__*/React.createElement("i", {
  style: {
    width: m.progress + "%"
  }
})), /*#__PURE__*/React.createElement("span", {
  style: {
    display: "flex",
    gap: 5,
    alignItems: "center",
    fontSize: 11,
    color: STATUS_META[m.state] ? STATUS_META[m.state].c : "var(--dim2)"
  }
}, /*#__PURE__*/React.createElement(Dot, {
  c: STATUS_META[m.state] ? STATUS_META[m.state].c : "var(--dim2)"
}), m.state === "Pending" ? phaseIdx(g.phase) < phaseIdx("MovingWorkloads") ? "at site A" : "pending" : (STATUS_META[m.state] || {}).label || m.state))));
function VolumeBacklog({
  g,
  nav
}) {
  return /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Volume"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Size"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Backlog"), /*#__PURE__*/React.createElement("th", null, "Last replication"), /*#__PURE__*/React.createElement("th", null, "Migration"))), /*#__PURE__*/React.createElement("tbody", null, g.volumes.map(v => /*#__PURE__*/React.createElement("tr", {
    key: v.id
  }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: () => {
      const lv = REG[v.id];
      lv ? nav.detail(lv) : api.volume(v.id).then(x => nav.detail(x)).catch(() => {});
    }
  }, v.name)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(v.size)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right",
      color: v.backlog ? "var(--warn)" : "var(--ok)"
    }
  }, v.backlog ? fmtBytes(v.backlog) : "0"), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, v.lastAt ? clockOfM(v.lastAt) : "—"), /*#__PURE__*/React.createElement("td", null, v.migrated ? /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ok)"
    }
  }, "at site B") : g.phase === "MigratingVolumes" ? /*#__PURE__*/React.createElement("div", {
    className: "sbar",
    style: {
      width: 90
    }
  }, /*#__PURE__*/React.createElement("i", {
    style: {
      width: v.progress + "%"
    }
  })) : /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, "at site A")))), !g.volumes.length && /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("td", {
    colSpan: 5,
    className: "lmsg"
  }, "No bound PVC resolved for these workloads."))));
}
function AppGroupDetail({
  o: g,
  nav
}) {
  const path = REG[g.pathId];
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: g,
    title: g.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "#", g.order + 1, " in queue"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, g.approval === "auto" ? "moves automatically" : "approval required")),
    sub: /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "namespace ", g.namespace, path ? /*#__PURE__*/React.createElement(React.Fragment, null, " \xB7 path ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openMPath(g.pathId),
      label: path.name
    })) : null)
  }), g.phase === "Converged" && g.approval === "manual" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "check",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Replication has converged."), " Every volume's backlog is zero. Approve the move to live-migrate the VMs and restart the containers at site B."), /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    style: {
      marginLeft: "auto",
      flex: "none"
    },
    onClick: () => window.__ui.dialog(ACTIONS.appgroup(g).find(a => /Move/.test(a.label)).dialog, g)
  }, "Move workloads now")), g.phase === "Paused" && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Paused."), " The replication policy stays in place, so the backlog keeps being tracked; resume to continue.")), /*#__PURE__*/React.createElement(PhaseStrip, {
    phase: g.phase
  }), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, PH_HINT[g.phase] || g.message), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Phase",
    v: PH_LABEL[g.phase] || g.phase,
    c: STATUS_META[g.phase].c,
    s: g.message || ""
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Workloads",
    v: g.counts.members,
    s: `${g.counts.vms} VMs · ${g.counts.moved} moved`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Volumes",
    v: g.counts.volumes,
    s: `${g.counts.migrated} at site B`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backlog",
    v: fmtBytes(g.backlog),
    c: g.backlog ? "var(--warn)" : "var(--ok)",
    s: g.backlog ? "still replicating" : "converged"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Data",
    v: fmtBytes(g.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Started",
    v: g.startedAt ? fmtAgo(g.startedAt) : "—",
    s: g.finishedAt ? `finished ${fmtAgo(g.finishedAt)}` : ""
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Volumes",
    sub: "with per-volume backlog",
    count: g.counts.volumes,
    onClick: () => nav.layer(g, "volumes")
  }), g.rpolicyId && /*#__PURE__*/React.createElement(NavCard, {
    icon: "shield",
    title: "Replication policy",
    sub: "created under the hood for this group",
    count: "\u2192",
    onClick: () => nav.openRPolicy(g.rpolicyId)
  }), path && /*#__PURE__*/React.createElement(NavCard, {
    icon: "move",
    title: "Migration path",
    sub: path.name,
    count: "\u2192",
    onClick: () => nav.openMPath(g.pathId)
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Workloads"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(Members, {
    g: g
  }), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginTop: 10,
      marginBottom: 0
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "VMs move by KubeVirt live migration without downtime; containers are restarted on a node at the target site. Both keep using their volumes over the network until the volumes follow."))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Volumes and backlog"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement(VolumeBacklog, {
    g: g,
    nav: nav
  })))));
}
Object.assign(window, {
  MPathTile,
  AppGroupTile,
  MPathDetail,
  AppGroupDetail,
  MembersField,
  newMPathDialog,
  newAppGroupDialog,
  PhaseStrip
});
})();
// ---- appdr.jsx ----
(function(){
// ---------------------------------------------------------------------------
// PROTECTED APPLICATIONS
//
// One application binds a workload to a plan. The plan declares several
// protection methods; each becomes a leg. Exactly one leg is orchestrated —
// Ramen drives one DRPC per application, because a DRPC selects PVCs by label
// and two over the same PVCs would both claim them. The other legs run with
// identical parameters in the data plane and report their lag up the side
// channel, which is the only reason this page can show them at all.
// ---------------------------------------------------------------------------
const PROG_HINT = {
  Completed: "steady state",
  PinningGeneration: "pinning the chosen generation out of band, before the placement is rebound",
  UpdatingPlacement: "rebinding the placement control to the vault policy",
  MaterialisingVolumes: "the driver is building volumes from the pinned generation's objects",
  RestoringKubeObjects: "recreating the application's Kubernetes objects from the metadata bucket",
  WaitingForResourceRestore: "waiting for the restored objects to become ready",
  UpdatedPlacement: "placement updated, waiting for the workload to settle",
  "Cleaning Up": "removing the stale workload at the old site",
  WaitOnUserToCleanUp: "the stale workload has to be deleted by hand before DR resumes",
  EnsuringVolumesAreSecondary: "demoting the old site's volumes so only one side is writable",
  PreparingFinalSync: "flushing the last delta so nothing is lost on a planned move",
  RunningFinalSync: "shipping the final delta",
  Deleting: "tearing the relationship down"
};

// ---- one leg -------------------------------------------------------------
// A leg is one declared method's replication relationship. Its RPO comes from
// DRPC.status only for the orchestrated one; every other leg's lag is a
// side-channel read (payload 5).
function LegRow({
  leg,
  plan,
  app,
  nav
}) {
  const m = mmeta(leg.type);
  const method = plan ? (plan.methods || []).find(x => x.name === leg.method) : null;
  const budget = method && method.interval ? ivMins(method.interval) * 60 : 0;
  const over = budget && leg.lagSeconds > budget;
  const dead = leg.state === "Unprotected";
  return /*#__PURE__*/React.createElement("div", {
    className: "legrow" + (leg.orchestrated ? " orch" : "") + (dead ? " dead" : "")
  }, /*#__PURE__*/React.createElement("span", {
    className: "ldot",
    style: {
      background: dead ? "var(--bad)" : m.c
    }
  }), /*#__PURE__*/React.createElement("div", {
    className: "lmain"
  }, /*#__PURE__*/React.createElement("div", {
    className: "lhead"
  }, /*#__PURE__*/React.createElement("b", null, leg.method), /*#__PURE__*/React.createElement(MethodBadge, {
    type: leg.type,
    sm: true
  }), leg.orchestrated ? /*#__PURE__*/React.createElement("span", {
    className: "badge",
    style: {
      color: "var(--accent)",
      borderColor: "var(--accent-line)"
    },
    title: "the method Ramen currently drives through the application's single DRPC"
  }, "orchestrated") : /*#__PURE__*/React.createElement("span", {
    className: "badge",
    title: "running in the data plane with identical parameters; its lag is a side-channel read"
  }, "data plane only"), /*#__PURE__*/React.createElement("span", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: () => nav.openSiteByName && nav.openSiteByName(leg.target)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 10
  }), leg.target)), dead ? /*#__PURE__*/React.createElement("div", {
    className: "lnote",
    style: {
      color: "var(--bad)"
    }
  }, leg.note) : /*#__PURE__*/React.createElement("div", {
    className: "lstats"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "RPO target"), /*#__PURE__*/React.createElement("b", null, leg.type === "sync" ? "0" : method ? method.interval : "—")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Actual lag"), /*#__PURE__*/React.createElement("b", {
    style: over ? {
      color: "var(--bad)"
    } : leg.type === "sync" ? {
      color: "var(--ok)"
    } : null
  }, fmtLag(leg.lagSeconds))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Last cycle"), /*#__PURE__*/React.createElement("b", null, leg.lastAt ? clockOf(leg.lastAt) : "—")), leg.type === "snapshot-s3" ? /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Generations"), /*#__PURE__*/React.createElement("b", null, leg.generations)) : /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Shipped"), /*#__PURE__*/React.createElement("b", null, leg.bytesLastCycle != null ? fmtBytes(leg.bytesLastCycle) : "inline"))), leg.type === "snapshot-s3" && !dead && !!leg.generations && /*#__PURE__*/React.createElement("div", {
    className: "labels",
    style: {
      marginTop: 7
    }
  }, /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "oldest"), fmtDate(leg.oldestAt)), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "newest"), fmtDate(leg.newestAt)), leg.lockedUntil && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "lock",
    s: 10
  }), "locked until ", fmtDate(leg.lockedUntil)), method && !method.failback && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    title: "the source is gone or untrusted after a restore, and the vault holds generations rather than a live peer"
  }, /*#__PURE__*/React.createElement("i", null, "failback"), "not available"))));
}
const ivMins = iv => {
  if (!iv) return 0;
  const n = parseInt(iv, 10),
    u = String(iv).replace(/[\d.]/g, "");
  return n * (u === "m" ? 1 : u === "h" ? 60 : u === "d" ? 1440 : u === "w" ? 10080 : 1);
};
function LegsCard({
  a,
  plan,
  nav
}) {
  const legs = a.legs || [];
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Protection legs"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, legs.filter(l => l.orchestrated).length, " orchestrated of ", legs.length)), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: "4px 0"
    }
  }, legs.map(l => /*#__PURE__*/React.createElement(LegRow, {
    key: l.id,
    leg: l,
    plan: plan,
    app: a,
    nav: nav
  })), !legs.length && /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "This application's plan declares no method, so nothing is protected."))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Ramen drives exactly one leg. Switching which one rebinds the application's placement control \u2014 a delete and a create, because every DRPolicy field is immutable. The remaining legs keep replicating with identical parameters; only their reporting path differs, which is why their lag is read from the control plane rather than from Kubernetes."));
}

// ---- protection lag, rolled up over the group -----------------------------
function GroupLagCard({
  a,
  nav
}) {
  const g = a.group || {
    volumes: []
  };
  const orch = (a.legs || []).find(l => l.orchestrated) || {};
  return /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Group protection lag"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Group point in time"), /*#__PURE__*/React.createElement("b", null, g.lastAt ? clockOf(g.lastAt) : "never")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Written since"), /*#__PURE__*/React.createElement("b", {
    style: g.writtenSince ? {
      color: "var(--warn)"
    } : null
  }, fmtBytes(g.writtenSince || 0))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Orchestrated leg"), /*#__PURE__*/React.createElement("b", null, orch.method || "—"))), !!(g.volumes || []).length && /*#__PURE__*/React.createElement("table", {
    className: "dt",
    style: {
      marginTop: 10
    }
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Volume"), /*#__PURE__*/React.createElement("th", null, "Status"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Last safe"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Written since"))), /*#__PURE__*/React.createElement("tbody", null, g.volumes.map(v => /*#__PURE__*/React.createElement("tr", {
    key: v.id
  }, /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("button", {
    className: "tlink",
    onClick: () => {
      const o = REG[v.id];
      o && nav.detail(o);
    }
  }, v.name)), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement(TrafficLight, {
    status: v.status,
    sm: true,
    label: true
  })), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, v.lastAt ? clockOf(v.lastAt) : "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(v.writtenSince)))))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "9px 0 0"
    }
  }, "The group is only as current as its most stale member. Crash consistency across the set holds only when every volume shares one storageID \u2014 Ramen derives the consistency-group boundary from it, so volumes on different storageIDs land in different groups and a restore spanning them is not consistent.")));
}

// ---- generation catalogue (payload 4) -------------------------------------
// No Kubernetes object represents a generation, so this whole table is a
// side-channel read. Selecting one pins it out of band immediately before the
// placement rebind, because PromoteVolume carries no point-in-time argument.
function GenerationsCard({
  a,
  nav
}) {
  const [tier, setTier] = useState("");
  const {
    data,
    loading,
    reload
  } = useResource("gens|" + a.id, () => api.appGenerations(a.id), 20000);
  const gens = (data || a.generations || []).filter(g => !tier || g.tier === tier);
  const tiers = [...new Set((a.generations || []).map(g => g.tier))];
  const restore = g => window.__ui.dialog(restoreGenerationDialog(a, g), a);
  const verify = async g => {
    try {
      await api.appVerifyGeneration(a.id, g.generation);
      window.__toast(`Generation ${g.generation} verified`);
      reload();
    } catch (e) {
      window.__toast(e.message);
    }
  };
  if (!a.vaultMethod) return null;
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Generation catalogue"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement(SourceTag, {
    what: "simplyblock control plane"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, (a.generations || []).length, " restorable")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "ptools"
  }, /*#__PURE__*/React.createElement("div", {
    className: "chips"
  }, /*#__PURE__*/React.createElement("button", {
    className: "chip" + (tier ? "" : " on"),
    onClick: () => setTier("")
  }, "all tiers"), tiers.map(t => /*#__PURE__*/React.createElement("button", {
    key: t,
    className: "chip" + (tier === t ? " on" : ""),
    onClick: () => setTier(t)
  }, t))), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, gens.length)), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, loading && !gens.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "loading\u2026") : !gens.length ? /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "No generation in this tier.") : /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Gen"), /*#__PURE__*/React.createElement("th", null, "Taken"), /*#__PURE__*/React.createElement("th", null, "Tier"), /*#__PURE__*/React.createElement("th", null, "Kind"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Size"), /*#__PURE__*/React.createElement("th", null, "Integrity"), /*#__PURE__*/React.createElement("th", null, "Object lock"), /*#__PURE__*/React.createElement("th", {
    style: {
      width: 130
    }
  }))), /*#__PURE__*/React.createElement("tbody", null, gens.map(g => {
    const ok = g.integrity === "Verified";
    const pinned = a.pinnedGeneration === g.generation;
    const restored = a.restoredFromGeneration === g.generation;
    return /*#__PURE__*/React.createElement("tr", {
      key: g.generation,
      className: pinned ? "on" : ""
    }, /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, g.generation, pinned && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        marginLeft: 5,
        color: "var(--info)"
      }
    }, "pinned"), restored && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        marginLeft: 5,
        color: "var(--ok)"
      }
    }, "restored")), /*#__PURE__*/React.createElement("td", {
      className: "mono"
    }, fmtDate(g.at)), /*#__PURE__*/React.createElement("td", null, g.tier), /*#__PURE__*/React.createElement("td", null, g.kind === "full" ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ro)"
      }
    }, "full") : /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)"
      }
    }, "delta")), /*#__PURE__*/React.createElement("td", {
      className: "mono",
      style: {
        textAlign: "right"
      }
    }, fmtBytes(g.size)), /*#__PURE__*/React.createElement("td", null, ok ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ok)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "check",
      s: 10
    }), "verified") : /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--warn)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "alert",
      s: 10
    }), g.integrity)), /*#__PURE__*/React.createElement("td", null, g.lockedUntil ? /*#__PURE__*/React.createElement("span", {
      className: "lab",
      style: {
        color: "var(--ro)",
        borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "lock",
      s: 10
    }), fmtDate(g.lockedUntil)) : /*#__PURE__*/React.createElement("span", {
      style: {
        color: "var(--dim2)"
      }
    }, "none")), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("div", {
      style: {
        display: "flex",
        gap: 5,
        justifyContent: "flex-end"
      }
    }, !ok && /*#__PURE__*/React.createElement("button", {
      className: "chip",
      onClick: () => verify(g)
    }, "Verify"), /*#__PURE__*/React.createElement("button", {
      className: "chip",
      disabled: !ok,
      title: ok ? null : "verify the generation first",
      onClick: () => restore(g)
    }, "Restore"))));
  }))), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      margin: "0 12px",
      padding: "9px 0 12px"
    }
  }, "Restoring pins the chosen generation out of band and then rebinds the application to the vault policy \u2014 the ordinary Ramen failover path, with one addition. The driver reads the pin when the promote arrives and materialises volumes from those objects. Locked generations cannot be deleted before their lock expires, which is the guarantee the vault exists for; the object store enforces it, not the resource lifecycle."))));
}

// ---- tile ------------------------------------------------------------------
function ProtectedAppTile({
  a,
  nav
}) {
  const legs = a.legs || [];
  const orch = legs.find(l => l.orchestrated) || {};
  const busy = ["FailingOver", "Relocating"].includes(a.phase);
  return /*#__PURE__*/React.createElement("div", {
    className: "tile",
    style: {
      "--sc": STATUS_META[a.status].c
    },
    onDoubleClick: () => nav.detail(a)
  }, /*#__PURE__*/React.createElement(TileHead, {
    obj: a,
    left: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(TrafficLight, {
      status: a.phase
    }), /*#__PURE__*/React.createElement(Name, null, a.name)),
    right: /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, a.appKind)
  }), /*#__PURE__*/React.createElement("div", {
    className: "tsub",
    style: {
      marginTop: 2
    }
  }, "namespace ", a.namespace), /*#__PURE__*/React.createElement(Uuid, {
    value: a.id
  }), /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openPlanByName && nav.openPlanByName(a.planName);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "shield",
    s: 10
  }), a.planName), /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    onClick: e => {
      e.stopPropagation();
      nav.openSiteByName && nav.openSiteByName(a.activeSite);
    }
  }, /*#__PURE__*/React.createElement("i", null, "active"), a.activeSite), a.activeSite !== a.preferredSite && /*#__PURE__*/React.createElement("span", {
    className: "lab",
    style: {
      color: "var(--warn)"
    }
  }, /*#__PURE__*/React.createElement("i", null, "preferred"), a.preferredSite), a.cgId && /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    },
    onClick: e => {
      e.stopPropagation();
      nav.openCgroup(a.cgId);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), a.cgName)), /*#__PURE__*/React.createElement("div", {
    className: "mlist"
  }, legs.map(l => /*#__PURE__*/React.createElement("div", {
    className: "mrow" + (l.state === "Unprotected" ? " bad" : ""),
    key: l.id
  }, /*#__PURE__*/React.createElement(MethodBadge, {
    type: l.type,
    sm: true
  }), /*#__PURE__*/React.createElement("b", null, l.method), l.orchestrated && /*#__PURE__*/React.createElement("span", {
    className: "ordot",
    title: "orchestrated by Ramen"
  }), /*#__PURE__*/React.createElement("span", {
    className: "spacer"
  }), l.state === "Unprotected" ? /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      color: "var(--bad)"
    }
  }, "unprotected") : /*#__PURE__*/React.createElement("span", {
    className: "mono rpo"
  }, fmtLag(l.lagSeconds)), l.type === "snapshot-s3" && !!l.generations && /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      color: "var(--dim2)"
    }
  }, l.generations, " gen"))), !legs.length && /*#__PURE__*/React.createElement("div", {
    className: "nolim",
    style: {
      color: "var(--bad)"
    }
  }, "No leg \u2014 this application is not protected")), /*#__PURE__*/React.createElement("div", {
    className: "kv"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "PVCs"), /*#__PURE__*/React.createElement("b", null, a.counts.pvcs)), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Group safe as of"), /*#__PURE__*/React.createElement("b", {
    style: a.rpoMet ? null : {
      color: "var(--bad)"
    }
  }, a.group && a.group.lastAt ? clockOf(a.group.lastAt) : "—")), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("span", null, "Written since"), /*#__PURE__*/React.createElement("b", {
    style: a.group && a.group.writtenSince ? {
      color: "var(--warn)"
    } : null
  }, fmtBytes((a.group || {}).writtenSince || 0)))), busy && /*#__PURE__*/React.createElement("div", {
    className: "prepbox running"
  }, /*#__PURE__*/React.createElement("span", {
    className: "dots"
  }, /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null), /*#__PURE__*/React.createElement("i", null)), a.progression, " \u2014 ", PROG_HINT[a.progression] || "in progress"), a.phase === "WaitForUser" && /*#__PURE__*/React.createElement("div", {
    className: "prepbox",
    style: {
      color: "var(--bad)",
      borderColor: "color-mix(in srgb,var(--bad) 40%,transparent)"
    }
  }, "Waiting for the operator to clean up the stale workload before DR resumes."), a.needsReprotect && /*#__PURE__*/React.createElement("div", {
    className: "prepbox",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    }
  }, "Restored from generation ", a.restoredFromGeneration, " \u2014 re-protect as a new plan with a full baseline."), /*#__PURE__*/React.createElement(Foot, {
    items: [{
      label: "PVCs",
      count: a.counts.pvcs,
      icon: "volume",
      onClick: () => nav.layer(a, "pvcs")
    }, {
      label: "Details",
      right: true,
      onClick: () => nav.detail(a)
    }]
  }));
}

// ---- detail ----------------------------------------------------------------
function ProtectedAppDetail({
  o: a,
  nav
}) {
  const busy = ["FailingOver", "Relocating"].includes(a.phase);
  const {
    data: plan
  } = useResource("app.plan|" + a.planId, () => a.planId ? api.plan(a.planId) : Promise.resolve(null), 0);
  const legs = a.legs || [];
  const orch = legs.find(l => l.orchestrated) || {};
  const method = plan ? (plan.methods || []).find(m => m.name === a.orchestratedMethod) : null;
  // a leg is behind when its lag has passed its own method's interval, which is
  // the only definition the plan gives
  const behind = legs.filter(l => {
    if (l.state === "Unprotected") return true;
    const m = plan ? (plan.methods || []).find(x => x.name === l.method) : null;
    return m && m.interval && l.lagSeconds > ivMins(m.interval) * 60;
  });
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: a,
    title: `${a.namespace}/${a.name}`,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, a.appKind), /*#__PURE__*/React.createElement(TrafficLight, {
      status: a.phase
    }), a.kubeObjectProtection && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ro)",
        borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"
      }
    }, "k8s objects protected")),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "plan ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPlanByName(a.planName),
      label: a.planName
    }), " \xB7 orchestrated method ", /*#__PURE__*/React.createElement("b", null, a.orchestratedMethod))
  }), a.phase === "WaitForUser" && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Waiting for the operator."), " The application is running at the target, but the stale workload at the old site must be deleted before DR can resume. Use ", /*#__PURE__*/React.createElement("b", null, "Confirm cleanup"), " once it is gone.")), busy && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "clock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, a.phase, " \u2014 ", a.progression, "."), " ", PROG_HINT[a.progression] || "In progress.", " This is asynchronous; the phase advances on its own.")), a.pinnedGeneration != null && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--ro)",
      background: "color-mix(in srgb,var(--ro) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "camera",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Generation ", a.pinnedGeneration, " is pinned."), " The pin is a side-channel value the driver reads when the promote arrives, because PromoteVolume has no point-in-time argument. It is cleared once the promote resolves \u2014 a stale pin would make the next ordinary failover resolve to an old generation.")), a.needsReprotect && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--ro)",
      background: "color-mix(in srgb,var(--ro) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Restored from generation ", a.restoredFromGeneration, " \u2014 failback is not available."), " What cannot be reconstructed is the original site's pre-compromise state: the source volume is gone or untrusted, and the vault holds generations rather than a live peer. Re-protect this application as a new plan; it will take a full baseline.")), behind.length > 0 && !busy && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, behind.length === 1 ? "A leg is" : `${behind.length} legs are`, " outside the scheduling interval."), " ", behind.map(l => l.method).join(", "), " ", behind.length === 1 ? "is" : "are", " behind; a failover onto ", behind.length === 1 ? "that leg" : "those legs", " now would lose more work than the plan allows.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Phase",
    v: STATUS_META[a.phase].label,
    c: STATUS_META[a.phase].c
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Progression",
    v: a.progression,
    s: PROG_HINT[a.progression] || ""
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Active site",
    v: a.activeSite,
    s: a.activeSite === a.preferredSite ? "the preferred site" : `preferred is ${a.preferredSite}`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Orchestrated",
    v: a.orchestratedMethod,
    s: method ? mmeta(method.type).label : ""
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Legs",
    v: legs.length,
    s: `${legs.filter(l => l.status === "healthy").length} healthy`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "PVCs",
    v: a.counts.pvcs,
    s: `VRG ${a.vrgState}`
  })), /*#__PURE__*/React.createElement(LegsCard, {
    a: a,
    plan: plan,
    nav: nav
  }), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Protected PVCs",
    sub: a.cgId ? `from ${a.cgName}` : "matched by label selector",
    count: a.counts.pvcs,
    onClick: () => nav.layer(a, "pvcs")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "shield",
    title: "Protection plan",
    sub: `${a.planName} · ${legs.length} methods`,
    count: "\u2192",
    onClick: () => nav.openPlanByName(a.planName)
  }), a.cgId && /*#__PURE__*/React.createElement(NavCard, {
    icon: "link",
    title: "Consistency group",
    sub: "the volume set failed over together",
    count: "\u2192",
    onClick: () => nav.openCgroup(a.cgId)
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "k8s",
    title: "Active site",
    sub: a.activeSite,
    count: "\u2192",
    onClick: () => nav.openSiteByName(a.activeSite)
  })), /*#__PURE__*/React.createElement(GenerationsCard, {
    a: a,
    nav: nav
  }), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Placement control"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Application", a.name], ["Namespace", a.namespace], ["Kind", a.appKind], ["Plan", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPlanByName(a.planName),
      label: a.planName
    })], ["Storage profile", a.storageProfile], ["Orchestrated method", /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, a.orchestratedMethod), method ? ` · ${mmeta(method.type).label}` : "")], ["Bound DRPolicy", /*#__PURE__*/React.createElement("span", {
      className: "mono"
    }, plan && orch.target ? `sb-${a.activeSite}-${orch.target}${method && method.interval ? "-" + method.interval : ""}` : "—")], ["Phase", /*#__PURE__*/React.createElement(TrafficLight, {
      status: a.phase
    })], ["Progression", a.progression], ["Pending action", a.action || "none"], ["Preferred site", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openSiteByName(a.preferredSite),
      label: a.preferredSite
    })], ["Active site", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openSiteByName(a.activeSite),
      label: a.activeSite
    })], ["Failover targets", a.failoverTargets.length ? a.failoverTargets.join(", ") : "none — failback unavailable after a vault restore"], ["Restore targets", a.restoreTargets.length ? a.restoreTargets.join(", ") : "none"], ["Volume basis", a.cgId ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCgroup(a.cgId),
      label: `consistency group ${a.cgName}`
    }) : "PVC label selector"], ["VRG state", a.vrgState], ["Kubernetes object protection", a.kubeObjectProtection ? "enabled" : "disabled"], ["Recipe", a.recipe ? `${a.recipe.namespace}/${a.recipe.name} · ${((a.recipe.recoverWorkflow || {}).sequence || []).length} recover steps` : "none — unordered restore"], a.restoredFromGeneration != null ? ["Restored from", `generation ${a.restoredFromGeneration}`] : null, ["Protected since", fmtDate(a.createdAt)]].filter(Boolean)
  }))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, a.cgId ? "Volume basis" : "PVC selector"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, a.cgId ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, /*#__PURE__*/React.createElement("button", {
    className: "lab link",
    style: {
      color: "var(--ro)",
      borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"
    },
    onClick: () => nav.openCgroup(a.cgId)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 10
  }), a.cgName), /*#__PURE__*/React.createElement("span", {
    className: "lab"
  }, /*#__PURE__*/React.createElement("i", null, "pvcs"), a.counts.pvcs)), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginTop: 10,
      marginBottom: 0
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "The group's members are exactly the volumes protected and failed over together, and its membership is fixed while its policies are attached, so the set cannot drift underneath the application. Crash consistency holds only across volumes sharing one storageID.")) : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    className: "labels"
  }, Object.entries((a.selector || {}).matchLabels || a.selector || {}).map(([k, v]) => /*#__PURE__*/React.createElement("span", {
    className: "lab",
    key: k
  }, /*#__PURE__*/React.createElement("i", null, k), v))), /*#__PURE__*/React.createElement("div", {
    className: "fnote",
    style: {
      marginTop: 10,
      marginBottom: 0
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  }), "The selector must never be empty \u2014 an empty selector claims every PVC in the namespace, and two placement controls would then contend over the same volumes. A PVC created later that matches is picked up automatically; VolSync-created PVCs are excluded.")))), /*#__PURE__*/React.createElement(GroupLagCard, {
    a: a,
    nav: nav
  }), /*#__PURE__*/React.createElement(RecipeCard, {
    a: a
  }), /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "What each move costs"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, /*#__PURE__*/React.createElement("b", null, "Fail over"), " \u2014 unplanned. The application comes up on a peer site from the last synced group; anything written after that sync is lost, and the old site should be fenced. Available on ", a.failoverTargets.length ? a.failoverTargets.join(", ") : "no site", "."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, /*#__PURE__*/React.createElement("b", null, "Fail back"), " \u2014 planned. Replication runs in reverse and the move only proceeds once the group is inside its interval, so nothing is lost."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, /*#__PURE__*/React.createElement("b", null, "Restore from a generation"), " \u2014 the ransomware path. A chosen generation is materialised on ", a.restoreTargets.length ? a.restoreTargets.join(", ") : "no site", " and the application is re-protected from scratch afterwards. Recovery takes minutes rather than seconds, because volumes are built from object storage."))))));
}
Object.assign(window, {
  GroupLagCard,
  LegsCard,
  LegRow,
  GenerationsCard,
  ProtectedAppTile,
  ProtectedAppDetail,
  PROG_HINT,
  ivMins
});
})();
// ---- details-data.jsx ----
(function(){
function PoolDetail({
  o: p,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: p,
    title: p.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "storage pool"), p.dhchap && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ro)",
        borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"
      }
    }, "dhchap bi-dir"), !p.enabled && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--warn)",
        borderColor: "color-mix(in srgb,var(--warn) 45%,transparent)"
      }
    }, "disabled"))
  }), p.dhchap && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--ro)",
      background: "color-mix(in srgb,var(--ro) 7%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "lock",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Bi-directional DH-CHAP."), " Host and subsystem authenticate each other on every connection. It was set when the pool was created and cannot be turned off \u2014 a pool without mutual authentication has to be created separately.")), !p.enabled && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Pool disabled."), " Existing volumes keep serving I/O normally \u2014 only provisioning of new volumes into this pool is blocked.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Volumes",
    v: p.counts.volumes,
    s: `${p.counts.volumesOnline} online`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Provisioned",
    v: fmtBytes(p.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Utilized",
    v: fmtBytes(p.capacity.used),
    s: pct(p.capacity.used, p.capacity.total).toFixed(0) + "% of provisioned"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Volume data",
    v: fmtBytes(p.lvolBytes)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Snapshot data",
    v: fmtBytes(p.snapshotBytes),
    s: `${p.counts.snapshots} snapshots`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backup chains",
    v: p.counts.backups
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "volume",
    title: "Logical volumes",
    sub: "in this pool",
    count: p.counts.volumes,
    onClick: () => nav.layer(p, "volumes")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "camera",
    title: "Snapshots",
    sub: "scoped to this pool",
    count: p.counts.snapshots,
    onClick: () => nav.layer(p, "snapshots")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "cloud",
    title: "Backups",
    sub: "scoped to this pool",
    count: p.counts.backups,
    onClick: () => nav.layer(p, "backups")
  }), p.counts.storageClasses > 0 && /*#__PURE__*/React.createElement(NavCard, {
    icon: "k8s",
    title: "StorageClasses",
    sub: "generated from this pool",
    count: p.counts.storageClasses,
    onClick: () => nav.layer(p, "storageclasses")
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Pool properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", p.name], ["State", /*#__PURE__*/React.createElement(TrafficLight, {
      status: p.status
    })], ["Provisioning", p.enabled ? "allowed" : "blocked — pool disabled"], ["Node affinity", "none — pools are cluster-wide and carry no primary, secondary or tertiary node"], ["StorageClasses", p.storageClasses.length ? /*#__PURE__*/React.createElement("span", {
      style: {
        display: "flex",
        flexDirection: "column",
        gap: 3,
        alignItems: "flex-end"
      }
    }, p.storageClasses.map(sc => /*#__PURE__*/React.createElement("span", {
      key: sc.uuid,
      style: {
        display: "flex",
        gap: 7,
        alignItems: "baseline"
      }
    }, /*#__PURE__*/React.createElement("em", {
      style: {
        fontStyle: "normal",
        color: "var(--dim2)",
        fontSize: 10.5
      }
    }, sc.k8s_cluster), /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openStorageClass(sc.uuid),
      label: sc.name
    })))) : null], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(p.clusterId),
      label: regName(p.clusterId)
    })], ["Volumes", p.counts.volumes], ["Provisioned", fmtBytes(p.capacity.total)], ["Volume data", fmtBytes(p.lvolBytes)], ["Snapshot data", fmtBytes(p.snapshotBytes)], ["Utilized", `${fmtBytes(p.capacity.used)} · ${pct(p.capacity.used, p.capacity.total).toFixed(0)}%`]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Quality of service"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(QosProps, {
    qos: p.qos,
    fallback: "No QoS limits configured on this pool."
  })))));
}
function VolumeDetail({
  o: v,
  nav
}) {
  const nref = (r, label) => r ? [label, /*#__PURE__*/React.createElement(Ref, {
    onClick: () => nav.openNode(v.clusterId, r.uuid),
    label: r.hostname
  })] : [label, null];
  const primaryIp = v.nodes.primary && REG[v.nodes.primary.uuid] ? REG[v.nodes.primary.uuid].ip : v.nodes.primary ? v.nodes.primary.hostname : "—";
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: v,
    title: v.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, v.crypto ? /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"
      }
    }, "encrypted") : /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "unencrypted"), v.dataReduction && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "comp-dedup"), v.baseSnapshot && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "clone")),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "pool ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPool(v.clusterId, v.poolId),
      label: v.poolName
    }))
  }), v.migration && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--info)",
      background: "color-mix(in srgb,var(--info) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "move",
    s: 15
  }), v.migration.instant ? /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Moved instantly."), " The primary went from ", v.migration.from, " to ", v.migration.target, " without copying data (", String(v.migration.reason).replace("_", " "), "). The move was recorded as an ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, "lvol_migration"), " task on the cluster.") : /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Migration queued."), " Primary will move to ", v.migration.target, " at the next opportunity.")), v.affinity && v.affinity.satisfied === false && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--warn)",
      background: "color-mix(in srgb,var(--warn) 8%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Affinity not satisfied."), " The workload ", v.affinity.workload, " runs on ", v.affinity.workload_node, ", but front storage is elsewhere. An instant migration would restore locality.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Provisioned",
    v: fmtBytes(v.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Utilized",
    v: fmtBytes(v.capacity.used),
    s: pct(v.capacity.used, v.capacity.total).toFixed(0) + "%"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "IOPS read",
    v: fmtNum(v.iops.r)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "IOPS write",
    v: fmtNum(v.iops.w)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Snapshots",
    v: v.counts.snapshots,
    s: `${v.counts.snapshotsBackedUp} backed up`
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backup versions",
    v: v.counts.backupVersions || 0,
    s: v.backupChainId ? "one chain" : "no backup chain"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Drill down"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  })), /*#__PURE__*/React.createElement("div", {
    className: "navcards"
  }, /*#__PURE__*/React.createElement(NavCard, {
    icon: "camera",
    title: "Snapshots",
    sub: "of this volume",
    count: v.counts.snapshots,
    onClick: () => nav.layer(v, "snapshots")
  }), /*#__PURE__*/React.createElement(NavCard, {
    icon: "cloud",
    title: "Backup chain",
    sub: v.backupChainId ? "versions in object storage" : "not backed up yet",
    count: v.counts.backupVersions || 0,
    onClick: () => nav.layer(v, "backups")
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Volume properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", v.name], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: v.status
    })], ["Pool", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPool(v.clusterId, v.poolId),
      label: v.poolName
    })], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(v.clusterId),
      label: regName(v.clusterId)
    })], nref(v.nodes.primary, "Primary node"), nref(v.nodes.secondary, "Secondary node"), nref(v.nodes.tertiary, "Tertiary node"), ["Base snapshot", v.baseSnapshot ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openSnapshot(v.clusterId, v.baseSnapshot.uuid),
      label: v.baseSnapshot.snapshot_name
    }) : null], ["Backup policy", v.backupPolicy ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.layerRef(v.clusterId, "policies"),
      label: v.backupPolicy.policy_name
    }) : null], ["Replication policy", v.replication ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openRPolicy(v.replication.policyId),
      label: v.replication.policyName
    }) : null], v.replication ? ["Replication mode", v.replication.mode] : null, v.replication ? ["Replication status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: v.replication.status
    })] : null, v.replication ? ["Last replication", `${fmtDate(v.replication.lastAt)} · ${fmtAgo(v.replication.lastAt)}`] : null, v.replication ? ["Backlog", v.replication.mode === "synchronous" ? "none (synchronous)" : fmtBytes(v.replication.backlog)] : null, (v.consistencyGroups || []).length ? ["Consistency groups", /*#__PURE__*/React.createElement("span", {
      className: "labels",
      style: {
        margin: 0
      }
    }, v.consistencyGroups.map(g => /*#__PURE__*/React.createElement("button", {
      className: "lab link",
      key: g.uuid,
      onClick: () => nav.openCgroup(g.uuid)
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "link",
      s: 10
    }), g.name)))] : null, ["Bucket", v.bucket ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openBucket(v.bucket.uuid),
      label: v.bucket.name
    }) : null], ["PVC", v.pvc ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPvc(v.pvc.uuid),
      label: v.pvc.namespace + "/" + v.pvc.name
    }) : "not provisioned via CSI"], v.pvc ? ["Kubernetes cluster", v.pvc.k8s_cluster] : null, v.pvc ? ["Storage class", v.pvc.storage_class] : null, v.pvc ? ["Workload", v.pvc.workload] : null, ["Affinity", v.affinity ? v.affinity.mode === "pod" ? `pod — follows ${v.affinity.workload}${v.affinity.satisfied === false ? " (not satisfied)" : ""}` : `node — pinned to ${v.affinity.pinned_node}` : "none"], v.migration ? ["Last move", v.migration.instant ? `${v.migration.from} → ${v.migration.target} · instant · ${String(v.migration.reason).replace("_", " ")}` : `queued → ${v.migration.target}`] : null, ["Encryption", v.crypto ? "enabled" : "disabled"], ["Compression-dedup", v.dataReduction ? "enabled" : "disabled"], v.dataReduction ? ["Logical vs on disk", `${fmtBytes(v.logicalUsed)} → ${fmtBytes(v.capacity.used)} · ${(v.logicalUsed / Math.max(1, v.capacity.used)).toFixed(2)}×`] : null, ["Created", fmtDate(v.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Quality of service"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement(QosProps, {
    qos: v.qos,
    fallback: `No volume-level QoS — limits inherited from pool ${v.poolName}.`
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Connection"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "code"
  }, `nvme connect --transport=tcp \\\n  --traddr=${primaryIp} --trsvcid=4420 \\\n  --nqn=${v.nqn}`)))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(VolumeReplicationCard, {
    v: v,
    nav: nav
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement(IOCards, {
    o: v
  })))));
}
function VolumeReplicationCard({
  v,
  nav
}) {
  const r = v.replication;
  if (!r) return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Replication"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "nolim"
  }, "Not replicated. Attach this volume to a replication policy from the policy\u2019s volume list, or from the volume actions.")));
  const sync = r.mode === "synchronous";
  return /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", {
    style: {
      display: "flex",
      alignItems: "center",
      gap: 9
    }
  }, "Replication", /*#__PURE__*/React.createElement(TrafficLight, {
    status: r.status
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }), /*#__PURE__*/React.createElement("span", {
    className: "badge"
  }, r.mode)), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Policy", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openRPolicy(r.policyId),
      label: r.policyName
    })], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: r.status
    })], ["Last replication", sync ? "continuous" : `${clockOf(r.lastAt)} · ${fmtAgo(r.lastAt)}`], ["Backlog", sync ? "none — writes are acknowledged at every zone" : fmtBytes(r.backlog)], ["Generations kept", sync ? null : r.generations || "latest only"], ["Consistency group", r.consistencyGroup || "none"]]
  })));
}
function SnapshotDetail({
  o: s,
  nav
}) {
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: s,
    title: s.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "snapshot"), s.seq && /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "generation ", s.seq), s.backupVersionId ? /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ok)",
        borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"
      }
    }, "backed up \xB7 ", s.backupVersionId) : /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "not backed up")),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, fmtDate(s.createdAt), " \xB7 ", fmtAgo(s.createdAt))
  }), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Delta size",
    v: fmtBytes(s.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Taken",
    v: fmtAgo(s.createdAt),
    s: fmtDate(s.createdAt)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Generation",
    v: s.seq || "—",
    s: s.parentId ? "chains to predecessor" : "chains to volume"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backup version",
    v: s.backupVersionId || "none",
    c: s.backupVersionId ? "var(--ok)" : undefined
  })), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Snapshot properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", s.name], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: s.status
    })], ["Created", fmtDate(s.createdAt)], ["Delta size", fmtBytes(s.capacity.total)], ["Generation", s.seq], ["Chains to", s.parentId ? "predecessor snapshot" : "the volume itself"], ["Backup version", s.backupVersionId || null], ["Base volume", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openVolume(s.clusterId, s.poolId, s.volumeId),
      label: s.volumeName
    })], ["Pool", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPool(s.clusterId, s.poolId),
      label: s.poolName
    })], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(s.clusterId),
      label: regName(s.clusterId)
    })]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Snapshots and backups"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Copy-on-write snapshot, chained as a delta against its predecessor. It shares blocks with the base volume and grows only as the volume diverges."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, s.backupVersionId ? /*#__PURE__*/React.createElement(React.Fragment, null, "A backup version was taken from this snapshot. The two are now ", /*#__PURE__*/React.createElement("b", null, "independent objects"), " \u2014 deleting this online snapshot leaves version ", /*#__PURE__*/React.createElement("span", {
    className: "mono"
  }, s.backupVersionId), " in the bucket untouched.") : /*#__PURE__*/React.createElement(React.Fragment, null, "No backup has been taken from this snapshot yet. Backups are always taken ", /*#__PURE__*/React.createElement("b", null, "from a snapshot"), ", never from the live volume."))))));
}
function BackupDetail({
  o: b,
  nav
}) {
  const vs = b.versions || [];
  const merge = v => window.__ui.dialog({
    title: `Merge ${v.id} into its predecessor?`,
    danger: v.seq === 2,
    desc: v.seq === 2 ? "This is the earliest delta. Merging it grows the full version to include its changes and removes the delta — this is how older retention is aged out. Restoring to a point in time before this version is no longer possible afterwards." : `The changes in ${v.id} are folded into version ${vs[v.seq - 2] ? vs[v.seq - 2].id : "the predecessor"}, which then covers both. ${v.id} disappears from the chain.`,
    confirm: "Merge",
    run: () => api.backupMergeVersion(b.id, v.id)
  }, b);
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: b,
    title: b.chainId,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "backup chain"), /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, b.counts.versions, " version", b.counts.versions === 1 ? "" : "s"), b.policyName && /*#__PURE__*/React.createElement("span", {
      className: "badge k8s"
    }, b.policyName)),
    sub: /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 11.5,
        color: "var(--dim)"
      }
    }, "of ", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openVolume(b.clusterId, b.poolId, b.volumeId),
      label: b.volumeName
    }), " \xB7 latest ", fmtDate(b.latestAt))
  }), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Versions",
    v: b.counts.versions,
    s: "1 full + deltas"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Full version",
    v: fmtBytes(b.fullBytes)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Deltas",
    v: fmtBytes(b.deltaBytes)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Chain size",
    v: fmtBytes(b.capacity.total)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Oldest point",
    v: fmtAgo(b.createdAt),
    s: fmtDate(b.createdAt)
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Merged so far",
    v: b.counts.merged,
    s: b.lastMergeAt ? `last ${fmtAgo(b.lastMergeAt)}` : "never merged"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Version chain"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, "oldest first \xB7 restore picks a point in time")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Version"), /*#__PURE__*/React.createElement("th", null, "Type"), /*#__PURE__*/React.createElement("th", null, "Taken from snapshot"), /*#__PURE__*/React.createElement("th", null, "Tier"), /*#__PURE__*/React.createElement("th", null, "Created"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Size"), /*#__PURE__*/React.createElement("th", null, "Merged"), /*#__PURE__*/React.createElement("th", null))), /*#__PURE__*/React.createElement("tbody", null, vs.map(v => /*#__PURE__*/React.createElement("tr", {
    key: v.id
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600
    }
  }, v.id), /*#__PURE__*/React.createElement("td", null, /*#__PURE__*/React.createElement("span", {
    className: "badge " + (v.type === "full" ? "k8s" : "")
  }, v.type)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, v.snapshotName || /*#__PURE__*/React.createElement("span", {
    style: {
      color: "var(--dim2)"
    }
  }, "snapshot deleted")), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: "var(--dim)"
    }
  }, v.tier), /*#__PURE__*/React.createElement("td", {
    className: "mono"
  }, fmtDate(v.createdAt)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, fmtBytes(v.size)), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      color: v.merged ? "var(--warn)" : "var(--dim2)"
    }
  }, v.merged ? `${v.merged} folded in` : "—"), /*#__PURE__*/React.createElement("td", {
    style: {
      textAlign: "right"
    }
  }, /*#__PURE__*/React.createElement("button", {
    className: "chip",
    disabled: v.seq === 1,
    title: v.seq === 1 ? "the full version has no predecessor" : "Merge into predecessor",
    onClick: () => merge(v)
  }, "Merge")))))))), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Chain properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Chain id", b.chainId], ["Status", /*#__PURE__*/React.createElement(TrafficLight, {
      status: b.status
    })], ["Volume", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openVolume(b.clusterId, b.poolId, b.volumeId),
      label: b.volumeName
    })], ["Pool", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openPool(b.clusterId, b.poolId),
      label: b.poolName
    })], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(b.clusterId),
      label: regName(b.clusterId)
    })], ["Backup policy", b.policyName ? /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.layerRef(b.clusterId, "policies"),
      label: b.policyName
    }) : "manual backups only"], ["Oldest point in time", fmtDate(b.createdAt)], ["Newest version", fmtDate(b.latestAt)], ["Last merge", b.lastMergeAt ? `${fmtDate(b.lastMergeAt)} · ${fmtAgo(b.lastMergeAt)}` : null], b.exportedTo ? ["Exported to", `${b.exportedTo} (${b.exportedVersion || "latest"})`] : null]
  }))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "How this chain ages"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Every backup version was taken ", /*#__PURE__*/React.createElement("b", null, "from a snapshot"), ". Once taken, the two are independent: deleting the online snapshot leaves its backup version in the bucket unchanged."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Versions are chained exactly like snapshots \u2014 one full version followed by deltas. ", /*#__PURE__*/React.createElement("b", null, "Merging"), " a version folds it into its predecessor. Merging the earliest delta grows the full version and drops that delta, which is how retention ages out without ever losing the full baseline."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Deleting this backup deletes the ", /*#__PURE__*/React.createElement("b", null, "entire chain"), "; individual versions can only leave it by being merged."))), /*#__PURE__*/React.createElement("div", {
    className: "card",
    style: {
      marginTop: 12
    }
  }, /*#__PURE__*/React.createElement("h3", null, "Bucket location"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("div", {
    className: "code"
  }, b.bucket, b.chainId, "/"))))));
}
function PolicyDetail({
  o: p,
  nav
}) {
  const span = r => {
    const n = parseInt(r.interval, 10),
      unit = r.interval.replace(/[\d]/g, "");
    const mins = n * (unit === "m" ? 1 : unit === "h" ? 60 : unit === "d" ? 1440 : 10080);
    const total = mins * r.versions;
    return total >= 1440 ? Math.round(total / 1440) + " d" : total >= 60 ? Math.round(total / 60) + " h" : total + " min";
  };
  return /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement(DetailHead, {
    obj: p,
    title: p.name,
    badge: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
      className: "badge"
    }, "backup policy"), p.consistencyGroup && /*#__PURE__*/React.createElement("span", {
      className: "badge",
      style: {
        color: "var(--ro)"
      }
    }, "group-consistent"))
  }), p.consistencyGroup && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--ro)",
      background: "color-mix(in srgb,var(--ro) 7%,var(--panel))",
      borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "link",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, "Group-consistent."), " All linked volumes form one consistency group; every cycle snapshots and backs them up atomically, and every retained version is a recovery point an application can be failed over to \u2014 the basis for ransomware recovery.")), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, /*#__PURE__*/React.createElement(Stat, {
    k: "Finest interval",
    v: p.finest || "—",
    s: "snapshot + backup cadence"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Versions retained",
    v: p.counts.versions,
    s: "across all tiers"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Snapshots online",
    v: p.counts.online,
    s: "the rest live only as backups"
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Volumes linked",
    v: p.counts.volumes
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Backup chains",
    v: p.counts.chains
  }), /*#__PURE__*/React.createElement(Stat, {
    k: "Coverage",
    v: p.schedule.length ? span(p.schedule[p.schedule.length - 1]) : "—",
    s: "oldest recoverable point"
  })), /*#__PURE__*/React.createElement("div", {
    className: "sech"
  }, /*#__PURE__*/React.createElement("h2", null, "Schedule"), /*#__PURE__*/React.createElement("span", {
    className: "ln"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, "each row is a tier: cadence, retained versions, snapshots kept online")), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      padding: 0
    }
  }, /*#__PURE__*/React.createElement("table", {
    className: "dt"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, /*#__PURE__*/React.createElement("th", null, "Every"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Versions kept"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Snapshots online"), /*#__PURE__*/React.createElement("th", {
    style: {
      textAlign: "right"
    }
  }, "Covers"), /*#__PURE__*/React.createElement("th", null, "Merge cadence"))), /*#__PURE__*/React.createElement("tbody", null, p.schedule.map((r, i) => /*#__PURE__*/React.createElement("tr", {
    key: i
  }, /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      fontWeight: 600
    }
  }, r.interval), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right"
    }
  }, r.versions, "\xD7"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right",
      color: r.online ? "var(--accent)" : "var(--dim2)"
    }
  }, r.online ? r.online + "×" : "—"), /*#__PURE__*/React.createElement("td", {
    className: "mono",
    style: {
      textAlign: "right",
      color: "var(--dim)"
    }
  }, span(r)), /*#__PURE__*/React.createElement("td", {
    style: {
      color: "var(--dim)",
      fontSize: 11
    }
  }, "every ", r.interval, " once ", r.versions, " versions exist"))))))), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "Policy properties"), /*#__PURE__*/React.createElement("div", {
    className: "bd",
    style: {
      paddingTop: 2,
      paddingBottom: 2
    }
  }, /*#__PURE__*/React.createElement(Props, {
    rows: [["Name", p.name], ["Schedule rows", p.schedule.length], ["Finest interval", p.finest], ["Versions retained", p.counts.versions], ["Snapshots kept online", p.counts.online], ["Volumes linked", p.counts.volumes], ["Backup chains", p.counts.chains], ["Cluster", /*#__PURE__*/React.createElement(Ref, {
      onClick: () => nav.openCluster(p.clusterId),
      label: regName(p.clusterId)
    })], ["Created", fmtDate(p.createdAt)]]
  }))), /*#__PURE__*/React.createElement("div", {
    className: "card"
  }, /*#__PURE__*/React.createElement("h3", null, "What the schedule means"), /*#__PURE__*/React.createElement("div", {
    className: "bd"
  }, /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Each row is one tier and carries three things at once: how often a snapshot is taken and backed up, how many backup ", /*#__PURE__*/React.createElement("b", null, "versions"), " of that tier are retained, and how many of those snapshots stay ", /*#__PURE__*/React.createElement("b", null, "online"), " on the cluster."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc"
  }, "Once a tier holds more versions than it retains, the oldest is ", /*#__PURE__*/React.createElement("b", null, "merged"), " into its predecessor \u2014 so the interval is also the merge cadence. Snapshots beyond the online count are deleted from the cluster; their backup versions stay in the bucket."), /*#__PURE__*/React.createElement("p", {
    className: "mdesc",
    style: {
      marginBottom: 0
    }
  }, "Written the short way, this policy reads:", /*#__PURE__*/React.createElement("span", {
    className: "code",
    style: {
      marginTop: 7,
      display: "block"
    }
  }, p.schedule.map(r => `${r.interval}\t${r.versions}x${r.online ? `\t${r.online}x online` : ""}`).join("\n")))))));
}
const DETAILS = {
  cluster: ClusterDetail,
  host: HostDetail,
  node: NodeDetail,
  device: DeviceDetail,
  pool: PoolDetail,
  volume: VolumeDetail,
  snapshot: SnapshotDetail,
  backup: BackupDetail,
  policy: PolicyDetail
};
// pair / rpolicy / zone live in dr.jsx — resolved at render time so load order cannot break the shell
const DETAIL_KIND = {
  pair: "PairDetail",
  rpolicy: "RPolicyDetail",
  zone: "ZoneDetail",
  plan: "PlanDetail",
  site: "SiteDetail",
  slot: "SlotDetail",
  replops: "ReplOpsDetail",
  cgroup: "CgroupDetail",
  cgsnapshot: "CgSnapshotDetail",
  migration: "MigrationDetail",
  k8sc: "K8sDetail",
  storageclass: "StorageClassDetail",
  pvc: "PvcDetail",
  bucket: "BucketDetail",
  protectedapp: "ProtectedAppDetail",
  deployconfig: "DeployConfigDetail",
  mpath: "MPathDetail",
  appgroup: "AppGroupDetail"
};
const Detail = ({
  obj,
  nav
}) => {
  const C = DETAILS[obj.kind] || (DETAIL_KIND[obj.kind] ? window[DETAIL_KIND[obj.kind]] : null);
  return C ? /*#__PURE__*/React.createElement(C, {
    o: obj,
    nav: nav
  }) : null;
};
Object.assign(window, {
  PoolDetail,
  VolumeDetail,
  VolumeReplicationCard,
  SnapshotDetail,
  BackupDetail,
  PolicyDetail,
  Detail
});
})();
// ---- app.jsx ----
(function(){
const LAYER_META = {
  clusters: {
    label: "Clusters",
    icon: "cluster"
  },
  cluster: {
    icon: "cluster"
  },
  cp: {
    label: "Control plane",
    icon: "host"
  },
  hosts: {
    label: "Hosts",
    icon: "host"
  },
  host: {
    icon: "host"
  },
  nodes: {
    label: "Storage nodes",
    icon: "node"
  },
  node: {
    icon: "node"
  },
  devices: {
    label: "Devices",
    icon: "device"
  },
  device: {
    icon: "device"
  },
  pools: {
    label: "Storage pools",
    icon: "pool"
  },
  pool: {
    icon: "pool"
  },
  volumes: {
    label: "Logical volumes",
    icon: "volume"
  },
  volume: {
    icon: "volume"
  },
  snapshots: {
    label: "Snapshots",
    icon: "camera"
  },
  snapshot: {
    icon: "camera"
  },
  backups: {
    label: "Backups",
    icon: "cloud"
  },
  backup: {
    icon: "cloud"
  },
  policies: {
    label: "Backup policies",
    icon: "clock"
  },
  policy: {
    icon: "clock"
  },
  dr: {
    label: "Disaster recovery",
    icon: "shield"
  },
  plans: {
    label: "Protection plans",
    icon: "shield"
  },
  plan: {
    icon: "shield"
  },
  sites: {
    label: "Sites",
    icon: "k8s"
  },
  site: {
    icon: "k8s"
  },
  slots: {
    label: "Replication slots",
    icon: "volume"
  },
  slot: {
    icon: "volume"
  },
  replops: {
    label: "Operations",
    icon: "clock"
  },
  replop: {
    icon: "clock"
  },
  pairs: {
    label: "Replication pairs",
    icon: "swap"
  },
  pair: {
    icon: "swap"
  },
  rpolicies: {
    label: "Replication policies",
    icon: "shield"
  },
  rpolicy: {
    icon: "shield"
  },
  zones: {
    label: "Zones",
    icon: "zone"
  },
  zone: {
    icon: "zone"
  },
  cgroups: {
    label: "Consistency groups",
    icon: "link"
  },
  cgroup: {
    icon: "link"
  },
  cgsnapshots: {
    label: "Group snapshots",
    icon: "camera"
  },
  cgsnapshot: {
    icon: "camera"
  },
  migrations: {
    label: "Migrations",
    icon: "move"
  },
  migration: {
    icon: "move"
  },
  k8s: {
    label: "Kubernetes",
    icon: "k8s"
  },
  k8sc: {
    icon: "k8s"
  },
  discovery: {
    label: "Discovery & deployment",
    icon: "search"
  },
  deploywizard: {
    label: "Deploy a cluster",
    icon: "plus"
  },
  deployconfigs: {
    label: "Deployment documents",
    icon: "cluster"
  },
  deployconfig: {
    icon: "cluster"
  },
  storageclasses: {
    label: "Storage classes",
    icon: "pool"
  },
  storageclass: {
    icon: "pool"
  },
  pvcs: {
    label: "PVCs",
    icon: "volume"
  },
  pvc: {
    icon: "volume"
  },
  buckets: {
    label: "Buckets",
    icon: "cloud"
  },
  bucket: {
    icon: "cloud"
  },
  protectedapps: {
    label: "Protected applications",
    icon: "cluster"
  },
  protectedapp: {
    icon: "cluster"
  },
  mpaths: {
    label: "Migration paths",
    icon: "move"
  },
  mpath: {
    icon: "move"
  },
  appgroups: {
    label: "Application groups",
    icon: "cluster"
  },
  appgroup: {
    icon: "cluster"
  }
};
const pC = cid => [{
  t: "clusters"
}, {
  t: "cluster",
  id: cid
}];
const pH = (cid, hid) => [...pC(cid), {
  t: "hosts"
}, {
  t: "host",
  id: hid
}];
const pN = (cid, nid) => [...pC(cid), {
  t: "nodes"
}, {
  t: "node",
  id: nid
}];
const pD = (cid, nid, did) => [...pN(cid, nid), {
  t: "devices"
}, {
  t: "device",
  id: did
}];
const pP = (cid, pid) => [...pC(cid), {
  t: "pools"
}, {
  t: "pool",
  id: pid
}];
const pV = (cid, pid, vid) => [...pP(cid, pid), {
  t: "volumes"
}, {
  t: "volume",
  id: vid
}];
const pPlan = id => [{
  t: "dr"
}, {
  t: "plans"
}, {
  t: "plan",
  id
}];
const pSite = id => [{
  t: "dr"
}, {
  t: "sites"
}, {
  t: "site",
  id
}];
const pPair = id => [{
  t: "dr"
}, {
  t: "pairs"
}, {
  t: "pair",
  id
}];
const pSlot = id => [{
  t: "dr"
}, {
  t: "slots"
}, {
  t: "slot",
  id
}];
const pReplOp = id => [{
  t: "dr"
}, {
  t: "replops"
}, {
  t: "replop",
  id
}];
const pRPol = id => [{
  t: "dr"
}, {
  t: "rpolicies"
}, {
  t: "rpolicy",
  id
}];
const pZone = id => [{
  t: "dr"
}, {
  t: "zones"
}, {
  t: "zone",
  id
}];
const pCg = (cid, gid) => [...pC(cid), {
  t: "cgroups"
}, {
  t: "cgroup",
  id: gid
}];
const pMig = (cid, id) => [...pC(cid), {
  t: "migrations"
}, {
  t: "migration",
  id
}];
const pBucket = (cid, id) => [...pC(cid), {
  t: "buckets"
}, {
  t: "bucket",
  id
}];
const pK = id => [{
  t: "k8s"
}, {
  t: "k8sc",
  id
}];
const pSc = (kid, id) => [...pK(kid), {
  t: "storageclasses"
}, {
  t: "storageclass",
  id
}];
const pDep = (kid, id) => [...pK(kid), {
  t: "deployconfigs"
}, {
  t: "deployconfig",
  id
}];
const pPvc = (kid, id) => [...pK(kid), {
  t: "pvcs"
}, {
  t: "pvc",
  id
}];
const pApp = id => [{
  t: "dr"
}, {
  t: "protectedapps"
}, {
  t: "protectedapp",
  id
}];
const pMp = id => [{
  t: "dr"
}, {
  t: "mpaths"
}, {
  t: "mpath",
  id
}];
const pAg = (pid, id) => [...pMp(pid), {
  t: "appgroups"
}, {
  t: "appgroup",
  id
}];
const detailPath = o => o.kind === "cluster" ? pC(o.id) : o.kind === "deployconfig" ? pDep(o.k8sClusterId, o.id) : o.kind === "host" ? pH(o.clusterId, o.id) : o.kind === "node" ? pN(o.clusterId, o.id) : o.kind === "device" ? pD(o.clusterId, o.nodeId, o.id) : o.kind === "pool" ? pP(o.clusterId, o.id) : o.kind === "volume" ? pV(o.clusterId, o.poolId, o.id) : o.kind === "plan" ? pPlan(o.id) : o.kind === "site" ? pSite(o.id) : o.kind === "pair" ? pPair(o.id) : o.kind === "slot" ? pSlot(o.id) : o.kind === "replops" ? pReplOp(o.id) : o.kind === "rpolicy" ? pRPol(o.id) : o.kind === "zone" ? pZone(o.id) : o.kind === "protectedapp" ? pApp(o.id) : o.kind === "mpath" ? pMp(o.id) : o.kind === "appgroup" ? pAg(o.pathId, o.id) : o.kind === "bucket" ? pBucket(o.clusterId, o.id) : o.kind === "k8sc" ? pK(o.id) : o.kind === "storageclass" ? pSc(o.k8sClusterId, o.id) : o.kind === "pvc" ? pPvc(o.k8sClusterId, o.id) : o.kind === "migration" ? pMig(o.sourceClusterId || o.clusterId, o.id) : o.kind === "cgroup" ? pCg(o.clusterId, o.id) : o.kind === "cgsnapshot" ? [...pCg(o.clusterId, o.cgId), {
  t: "cgsnapshots"
}, {
  t: "cgsnapshot",
  id: o.id
}] : o.kind === "snapshot" ? [...pV(o.clusterId, o.poolId, o.volumeId), {
  t: "snapshots"
}, {
  t: "snapshot",
  id: o.id
}] : o.kind === "backup" ? [...pV(o.clusterId, o.poolId, o.volumeId), {
  t: "backups"
}, {
  t: "backup",
  id: o.id
}] : [...pC(o.clusterId), {
  t: "policies"
}, {
  t: "policy",
  id: o.id
}];
const nameOf = o => o.hostname || o.name || (o.mode === "nvme" ? o.serial : o.blockdev) || o.serial || o.chainId || "";
const segLabel = s => s.id ? REG[s.id] ? nameOf(REG[s.id]) : shortId(s.id) : LAYER_META[s.t].label;
const searchable = o => [nameOf(o), o.id, o.ip, o.mgmtIp, o.serial, o.pcie, o.blockdev, o.failureDomain, o.owner, o.storageClass, ...Object.entries(o.tags || {}).map(([k, v]) => `${k}=${v}`), o.physicalLabel, o.poolName, o.volumeName, o.chainId, o.bucket, o.type, o.finest, o.rack, o.cabinet, o.hostClass, o.location, o.region, o.mode, o.replication && o.replication.policyName, o.sourceClusterId && regName(o.sourceClusterId), o.targetClusterId && regName(o.targetClusterId)].filter(Boolean).join(" ").toLowerCase();
const healthOf = o => (STATUS_META[o.status] || {
  rank: 9
}).rank;
const ioSum = o => ((o.iops || {}).r || 0) + ((o.iops || {}).w || 0);
const bwSum = o => ((o.bw || {}).r || 0) + ((o.bw || {}).w || 0);
const ts = o => Date.parse(o.createdAt || 0) || 0;
const SORTS = {
  health: {
    label: "Unhealthy first",
    cmp: (a, b) => healthOf(b) - healthOf(a) || nameOf(a).localeCompare(nameOf(b))
  },
  name: {
    label: "Name (A–Z)",
    cmp: (a, b) => nameOf(a).localeCompare(nameOf(b))
  },
  util: {
    label: "Utilization ↓",
    cmp: (a, b) => pct(b.capacity.used, b.capacity.total) - pct(a.capacity.used, a.capacity.total)
  },
  cap: {
    label: "Capacity ↓",
    cmp: (a, b) => b.capacity.total - a.capacity.total
  },
  iops: {
    label: "IOPS ↓",
    cmp: (a, b) => ioSum(b) - ioSum(a)
  },
  bw: {
    label: "Throughput ↓",
    cmp: (a, b) => bwSum(b) - bwSum(a)
  },
  newest: {
    label: "Newest first",
    cmp: (a, b) => ts(b) - ts(a)
  },
  oldest: {
    label: "Oldest first",
    cmp: (a, b) => ts(a) - ts(b)
  },
  size: {
    label: "Size ↓",
    cmp: (a, b) => b.capacity.total - a.capacity.total
  },
  free: {
    label: "Free devices ↓",
    cmp: (a, b) => (b.counts.free || 0) - (a.counts.free || 0)
  },
  backlog: {
    label: "Backlog ↓",
    cmp: (a, b) => (b.backlog || 0) - (a.backlog || 0)
  },
  members: {
    label: "Volumes ↓",
    cmp: (a, b) => (b.counts.volumes || 0) - (a.counts.volumes || 0)
  },
  slots: {
    label: "Slots ↓",
    cmp: (a, b) => (b.counts.slots || 0) - (a.counts.slots || 0)
  },
  hosts: {
    label: "Hosts ↓",
    cmp: (a, b) => (b.counts.hosts || 0) - (a.counts.hosts || 0)
  },
  rtt: {
    label: "Latency ↑",
    cmp: (a, b) => (a.link ? a.link.rtt_ms : 0) - (b.link ? b.link.rtt_ms : 0)
  }
};
const SORT_KEYS = {
  cluster: ["health", "name", "util", "cap", "iops", "bw"],
  host: ["health", "free", "name", "cap"],
  node: ["health", "name", "util", "cap", "iops", "bw"],
  device: ["health", "name", "util", "cap", "iops", "bw"],
  pool: ["health", "name", "util", "cap"],
  volume: ["health", "name", "util", "cap", "iops", "bw", "newest"],
  snapshot: ["newest", "oldest", "size", "name"],
  backup: ["newest", "oldest", "size", "name", "health"],
  policy: ["name", "members"],
  plan: ["health", "name", "members", "newest"],
  site: ["health", "name", "members"],
  pair: ["health", "name", "slots", "newest"],
  slot: ["health", "name", "newest"],
  replops: ["newest", "health", "name"],
  rpolicy: ["health", "slots", "name", "newest"],
  zone: ["name", "hosts", "cap"],
  cgroup: ["health", "name", "members", "cap"],
  cgsnapshot: ["newest", "oldest", "size", "name"],
  migration: ["health", "name", "newest", "members"],
  k8sc: ["health", "name", "members"],
  storageclass: ["name", "members", "cap"],
  pvc: ["health", "name", "cap", "newest"],
  bucket: ["health", "name", "util", "cap", "newest"],
  protectedapp: ["health", "name", "newest"]
};
const clusterOf = seg => seg.t === "cluster" ? seg.id : (REG[seg.id] || {}).clusterId;
// Each view advertises the surface that actually backs it: a CRD, a core object,
// or an operator /proposed collection for the kinds v1alpha1 does not model yet.
const G_ = "/apis/storage.simplyblock.io/v1alpha1/namespaces/{ns}";
const crd = (p, sel) => `GET ${G_}/${p}${sel ? "?labelSelector=" + sel.replace("storage.simplyblock.io/", "…/") : ""}`;
const crd1 = p => `GET ${G_}/${p}/{name}`;
const prop = p => `GET /operator/v1/proposed/${p}`;
const OWNER = "storage.simplyblock.io/owner-name";
const CLUS = "storage.simplyblock.io/cluster";
const PV = "/api/v1/persistentvolumes";
const VIEWS = {
  hosts: {
    kind: "host",
    load: p => p.t === "zone" ? api.zoneHosts(p.id) : p.t === "k8sc" ? api.k8sHosts(p.id) : api.hosts(p.id),
    api: p => prop(`hosts?scope=${p.t === "zone" ? "zones" : p.t === "k8sc" ? "k8s-clusters" : "clusters"}`)
  },
  nodes: {
    kind: "node",
    load: p => p.t === "host" ? api.nodes(clusterOf(p)).then(ns => ns.filter(n => n.hostId === p.id)) : api.nodes(p.id),
    api: p => crd("storagenodes", p.t === "host" ? OWNER : CLUS)
  },
  devices: {
    kind: "device",
    load: p => api.devices(p.id),
    api: () => crd("storagedevices", OWNER)
  },
  pools: {
    kind: "pool",
    load: p => api.pools(p.id),
    api: () => crd("storagepools", CLUS)
  },
  volumes: {
    kind: "volume",
    load: p => p.t === "rpolicy" ? api.rpolicyVolumes(p.id) : p.t === "cgroup" ? api.cgroupVolumes(p.id) : p.t === "migration" ? api.migrationVolumes(p.id) : p.t === "appgroup" ? api.appGroupVolumes(p.id) : p.t === "pool" ? api.poolVolumes(p.id) : api.clusterVolumes(p.id),
    api: p => p.t === "appgroup" ? prop("app-groups/{uuid}/lvols") : ["rpolicy", "cgroup", "migration"].includes(p.t) ? prop(`${p.t}s/{uuid}/lvols`) : `GET ${PV}?labelSelector=storage.simplyblock.io/${p.t === "pool" ? "pool" : "cluster"}`
  },
  cgroups: {
    kind: "cgroup",
    load: p => api.cgroups(p.id),
    api: () => prop("consistency-groups?cluster={uuid}")
  },
  k8s: {
    kind: "k8sc",
    load: () => api.k8sClusters(),
    api: () => prop("kubernetes-clusters")
  },
  deployconfigs: {
    kind: "deployconfig",
    load: p => p.t === "k8sc" ? api.k8sDeployConfigs(p.id) : api.deployConfigs(),
    api: () => crd("clusterdeploymentconfigs")
  },
  mpaths: {
    kind: "mpath",
    load: () => api.mpaths(),
    api: () => prop("migration-paths")
  },
  appgroups: {
    kind: "appgroup",
    load: p => api.mpathGroups(p.id),
    api: () => prop("migration-paths/{uuid}/app-groups")
  },
  protectedapps: {
    kind: "protectedapp",
    load: p => p.t === "plan" ? api.planApps(p.id) : p.t === "site" ? api.siteApps(p.id) : p.t === "rpolicy" ? api.rpolicyApps(p.id) : api.protectedApps(),
    api: p => prop(p.t === "plan" ? "protection-plans/{uuid}/protected-apps" : p.t === "site" ? "dr-sites/{uuid}/protected-apps" : p.t === "rpolicy" ? "replication-policies/{uuid}/protected-apps" : "protected-apps")
  },
  storageclasses: {
    kind: "storageclass",
    load: p => p.t === "pool" ? api.poolStorageClasses(p.id) : api.k8sStorageClasses(p.id),
    api: () => "GET /apis/storage.k8s.io/v1/storageclasses"
  },
  pvcs: {
    kind: "pvc",
    load: p => p.t === "storageclass" ? api.storageClassPvcs(p.id) : p.t === "k8sc" ? api.k8sPvcs(p.id) : p.t === "protectedapp" ? api.protectedAppPvcs(p.id) : api.pvcs(),
    api: () => "GET /api/v1/namespaces/{ns}/persistentvolumeclaims"
  },
  buckets: {
    kind: "bucket",
    load: p => api.buckets(p.id),
    api: () => prop("buckets?cluster={uuid}")
  },
  migrations: {
    kind: "migration",
    load: p => p.t === "cluster" ? api.migrations(p.id) : api.allMigrations(),
    api: p => prop(p.t === "cluster" ? "migrations?cluster={uuid}" : "migrations")
  },
  cgsnapshots: {
    kind: "cgsnapshot",
    load: p => api.cgSnapshots(p.id),
    api: () => prop("cg-snapshots?group={uuid}")
  },
  plans: {
    kind: "plan",
    load: p => p.t === "site" ? api.sitePlans(p.id) : api.plans(),
    api: p => prop(p.t === "site" ? "dr-sites/{uuid}/protection-plans" : "protection-plans")
  },
  sites: {
    kind: "site",
    load: p => p.t === "plan" ? api.planSites(p.id) : api.sites(),
    api: p => prop(p.t === "plan" ? "protection-plans/{uuid}/sites" : "dr-sites")
  },
  // replication reads three CRD lists and joins them: the resources reference
  // each other by name and carry no rollups
  pairs: {
    kind: "pair",
    load: () => api.pairs(),
    api: () => crd("replicationpairs")
  },
  slots: {
    kind: "slot",
    load: p => p.t === "rpolicy" ? api.policySlots(p.id) : p.t === "pair" ? api.pairSlots(p.id) : api.slots(),
    api: () => crd("replicationslots")
  },
  replops: {
    kind: "replops",
    load: p => p.t === "rpolicy" || p.t === "pair" ? api.refReplOps((REG[p.id] || {}).name) : api.replOps(),
    api: () => crd("replicationops")
  },
  rpolicies: {
    kind: "rpolicy",
    load: p => p.t === "pair" ? api.pairRPolicies(p.id) : p.t === "cluster" ? api.rpolicies(p.id) : api.allRPolicies(),
    api: () => crd("replicationpolicies")
  },
  zones: {
    kind: "zone",
    load: p => p.t === "cluster" ? api.clusterZones(p.id) : p.t === "k8sc" ? api.k8sZones(p.id) : api.zones(),
    api: () => prop("zones")
  },
  snapshots: {
    kind: "snapshot",
    load: p => api.snapshots(p.t === "volume" ? "lvols" : p.t === "pool" ? "pools" : "clusters", p.id),
    api: () => prop("snapshots?parent={uuid}")
  },
  backups: {
    kind: "backup",
    load: p => api.backups(p.t === "volume" ? "lvols" : p.t === "pool" ? "pools" : "clusters", p.id),
    api: () => crd("storagebackups", CLUS)
  },
  policies: {
    kind: "policy",
    load: p => api.policies(p.id),
    api: () => prop("backup-policies?cluster={uuid}")
  },
  clusters: {
    kind: "cluster",
    load: p => p.t === "zone" ? api.zoneClusters(p.id) : p.t === "k8sc" ? api.k8sStorageClusters(p.id) : api.clusters(),
    api: () => crd("storageclusters")
  }
};
const DETAIL_API = {
  cluster: crd1("storageclusters"),
  node: crd1("storagenodes"),
  device: crd1("storagedevices"),
  pool: crd1("storagepools"),
  backup: crd1("storagebackups"),
  host: "GET /api/v1/nodes/{name}",
  volume: `GET ${PV}/{name}`,
  storageclass: "GET /apis/storage.k8s.io/v1/storageclasses/{name}",
  pvc: "GET /api/v1/namespaces/{ns}/persistentvolumeclaims/{name}",
  snapshot: prop("snapshots/{uuid}"),
  policy: prop("backup-policies/{uuid}"),
  plan: prop("protection-plans/{uuid}"),
  site: prop("dr-sites/{uuid}"),
  pair: crd1("replicationpairs"),
  rpolicy: crd1("replicationpolicies"),
  slot: crd1("replicationslots"),
  replops: crd1("replicationops"),
  zone: prop("zones/{uuid}"),
  cgroup: prop("consistency-groups/{uuid}"),
  cgsnapshot: prop("cg-snapshots/{uuid}"),
  migration: prop("migrations/{uuid}"),
  k8sc: prop("kubernetes-clusters/{uuid}"),
  bucket: prop("buckets/{uuid}"),
  protectedapp: prop("protected-apps/{uuid}"),
  mpath: prop("migration-paths/{uuid}"),
  appgroup: prop("app-groups/{uuid}"),
  deployconfig: crd1("clusterdeploymentconfigs")
};
const KIND_LABEL = {
  cluster: "cluster",
  host: "host",
  node: "storage node",
  device: "device",
  pool: "storage pool",
  volume: "logical volume",
  snapshot: "snapshot",
  backup: "backup",
  policy: "backup policy",
  plan: "protection plan",
  site: "site",
  pair: "replication pair",
  rpolicy: "replication policy",
  slot: "replication slot",
  replops: "replication operation",
  zone: "zone",
  cgroup: "consistency group",
  cgsnapshot: "group snapshot",
  migration: "migration",
  k8sc: "Kubernetes cluster",
  storageclass: "storage class",
  pvc: "persistent volume claim",
  bucket: "bucket",
  protectedapp: "protected application",
  deployconfig: "deployment document",
  mpath: "migration path",
  appgroup: "application group"
};
function ErrorState({
  error,
  onRetry,
  kind,
  onUp,
  upLabel
}) {
  if (error.status === 501) {
    return /*#__PURE__*/React.createElement("div", {
      className: "empty"
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "link",
      s: 24,
      c: "var(--warn)"
    }), /*#__PURE__*/React.createElement("b", {
      style: {
        color: "var(--text)"
      }
    }, "Not available on this cluster"), /*#__PURE__*/React.createElement("span", {
      style: {
        maxWidth: 480
      }
    }, error.message), /*#__PURE__*/React.createElement("span", {
      style: {
        maxWidth: 480,
        color: "var(--dim2)"
      }
    }, "Snapshot replication itself still works here \u2014 it is the orchestration of asynchronous replication that is unavailable on this cluster."), onUp && /*#__PURE__*/React.createElement("button", {
      className: "chip",
      style: {
        marginTop: 8
      },
      onClick: onUp
    }, "Back to ", upLabel));
  }
  if (error.status === 403) {
    return /*#__PURE__*/React.createElement("div", {
      className: "empty"
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "shield",
      s: 24,
      c: "var(--warn)"
    }), /*#__PURE__*/React.createElement("b", {
      style: {
        color: "var(--text)"
      }
    }, "Forbidden"), /*#__PURE__*/React.createElement("span", {
      style: {
        maxWidth: 480
      }
    }, error.message), /*#__PURE__*/React.createElement("span", {
      style: {
        maxWidth: 480,
        color: "var(--dim2)"
      }
    }, "This is a 403 from the API server, not an empty collection \u2014 the objects may well exist. Ask the scope's admin for a grant."), onUp && /*#__PURE__*/React.createElement("button", {
      className: "chip",
      style: {
        marginTop: 8
      },
      onClick: onUp
    }, "Back to ", upLabel));
  }
  if (error.status === 404) {
    return /*#__PURE__*/React.createElement("div", {
      className: "empty"
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "alert",
      s: 24,
      c: "var(--dim2)"
    }), /*#__PURE__*/React.createElement("b", {
      style: {
        color: "var(--text)"
      }
    }, "This ", KIND_LABEL[kind] || "object", " no longer exists"), /*#__PURE__*/React.createElement("span", {
      style: {
        maxWidth: 440
      }
    }, "It was removed from the control plane, or this link points at an object from an earlier deployment."), /*#__PURE__*/React.createElement("span", {
      className: "mono",
      style: {
        fontSize: 10.5
      }
    }, "404 \xB7 ", API, error.path), onUp && /*#__PURE__*/React.createElement("button", {
      className: "chip",
      style: {
        marginTop: 8
      },
      onClick: onUp
    }, "Back to ", upLabel));
  }
  return /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 24,
    c: "var(--bad)"
  }), /*#__PURE__*/React.createElement("b", {
    style: {
      color: "var(--text)"
    }
  }, "Could not reach the Kubernetes API"), /*#__PURE__*/React.createElement("span", {
    style: {
      maxWidth: 420
    }
  }, error.status ? `HTTP ${error.status} — ` : "", error.message), /*#__PURE__*/React.createElement("span", {
    className: "mono",
    style: {
      fontSize: 10.5
    }
  }, (error.path || "").startsWith("/apis") || (error.path || "").startsWith("/api/") ? API : window.SB_CONFIG.operatorBase, error.path), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    style: {
      marginTop: 8
    },
    onClick: onRetry
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "refresh",
    s: 12
  }), "Retry"));
}
function Toolbar({
  items,
  kind,
  scope,
  q,
  setQ,
  sort,
  setSort,
  filters,
  setFilters,
  density,
  setDensity,
  count,
  onRefresh,
  extra
}) {
  const counts = {};
  items.forEach(o => counts[o.status] = (counts[o.status] || 0) + 1);
  const order = Object.keys(counts).sort((a, b) => (STATUS_META[b] || {
    rank: 0
  }).rank - (STATUS_META[a] || {
    rank: 0
  }).rank);
  const keys = SORT_KEYS[kind] || ["name"];
  return /*#__PURE__*/React.createElement("div", {
    className: "toolbar"
  }, /*#__PURE__*/React.createElement("div", {
    className: "search"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 13,
    c: "var(--dim2)"
  }), /*#__PURE__*/React.createElement("input", {
    value: q,
    placeholder: "Search name, UUID, IP, serial\u2026  ( / )",
    onChange: e => setQ(e.target.value)
  }), q && /*#__PURE__*/React.createElement("button", {
    onClick: () => setQ(""),
    style: {
      display: "flex",
      color: "var(--dim2)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "x",
    s: 11
  }))), scope && /*#__PURE__*/React.createElement("span", {
    className: "scopepill",
    title: "Results are pre-filtered to this scope"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: LAYER_META[scope.t].icon,
    s: 11
  }), scope.label), /*#__PURE__*/React.createElement("div", {
    className: "chips"
  }, order.length > 1 && order.map(s => /*#__PURE__*/React.createElement("button", {
    key: s,
    className: "chip" + (filters.includes(s) ? " on" : ""),
    onClick: () => setFilters(filters.includes(s) ? filters.filter(x => x !== s) : [...filters, s])
  }, /*#__PURE__*/React.createElement(Dot, {
    c: (STATUS_META[s] || {}).c
  }), (STATUS_META[s] || {}).label || s, /*#__PURE__*/React.createElement("span", {
    className: "n"
  }, counts[s]))), filters.length > 0 && /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: () => setFilters([])
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "x",
    s: 10
  }), "clear")), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), /*#__PURE__*/React.createElement("span", {
    className: "count"
  }, count === items.length ? items.length : `${count} / ${items.length}`), /*#__PURE__*/React.createElement("select", {
    className: "sel",
    value: sort,
    onChange: e => setSort(e.target.value),
    title: "Sort tiles"
  }, keys.map(k => /*#__PURE__*/React.createElement("option", {
    key: k,
    value: k
  }, SORTS[k].label))), /*#__PURE__*/React.createElement("div", {
    className: "seg"
  }, [["380px", "S"], ["340px", "M"], ["290px", "L"]].map(([v, l], i) => /*#__PURE__*/React.createElement("button", {
    key: v,
    className: density === v ? "on" : "",
    title: ["Spacious", "Balanced", "Compact"][i],
    onClick: () => setDensity(v)
  }, l))), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    onClick: onRefresh,
    title: "Refresh from the Kubernetes API"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "refresh",
    s: 12
  })), extra);
}
function DiscoveryView({
  kid,
  nav
}) {
  const {
    data: k,
    loading,
    error,
    reload
  } = useResource("dv|" + kid, () => api.k8sCluster(kid));
  if (error) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload,
    kind: "k8sc"
  }));
  if (loading || !k) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, Array.from({
    length: 4
  }).map((_, i) => /*#__PURE__*/React.createElement("div", {
    className: "skel",
    key: i,
    style: {
      height: 62
    }
  }))));
  return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "dhead"
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      minWidth: 0
    }
  }, /*#__PURE__*/React.createElement("h1", null, "Discovery & deployment"), /*#__PURE__*/React.createElement("div", {
    className: "mdesc",
    style: {
      marginTop: 4
    }
  }, "The control plane and the operator are installed with Helm, outside this console. From here on the operator does the work: it discovers what hardware ", /*#__PURE__*/React.createElement("b", null, k.name), " has, and a deployment document turns that into a storage cluster."))), /*#__PURE__*/React.createElement(DiscoveryPanel, {
    k: k,
    nav: nav
  }));
}
const TILE = {
  cluster: ClusterTile,
  host: HostTile,
  node: NodeTile,
  device: DeviceTile,
  pool: PoolTile,
  volume: VolumeTile,
  snapshot: SnapshotTile,
  backup: BackupTile,
  policy: PolicyTile,
  plan: PlanTile,
  site: SiteTile,
  pair: PairTile,
  rpolicy: RPolicyTile,
  slot: SlotTile,
  replops: ReplOpsTile,
  zone: ZoneTile,
  cgroup: CgroupTile,
  cgsnapshot: CgSnapshotTile,
  migration: MigrationTile,
  k8sc: K8sTile,
  storageclass: StorageClassTile,
  pvc: PvcTile,
  bucket: BucketTile,
  protectedapp: ProtectedAppTile,
  deployconfig: DeployConfigTile,
  mpath: MPathTile,
  appgroup: AppGroupTile
};
const TKEY = {
  cluster: "c",
  host: "h",
  node: "n",
  device: "d",
  pool: "p",
  volume: "v",
  snapshot: "s",
  backup: "b",
  plan: "p",
  site: "s",
  policy: "p",
  pair: "p",
  rpolicy: "p",
  slot: "s",
  replops: "o",
  zone: "s",
  cgroup: "g",
  cgsnapshot: "s",
  migration: "m",
  k8sc: "k",
  storageclass: "s",
  pvc: "p",
  bucket: "b",
  protectedapp: "a",
  deployconfig: "d",
  mpath: "m",
  appgroup: "g"
};
const kindPlural = kind => {
  const l = KIND_LABEL[kind] || kind;
  return /(s|ss|y)$/.test(l) && !/[aeiou]y$/.test(l) ? l.replace(/y$/, "ies") : l + "s";
};
function OverviewView({
  seg,
  parent,
  nav,
  prefs,
  rev,
  up,
  upLabel
}) {
  const {
    q,
    setQ,
    sort,
    setSort,
    filters,
    setFilters,
    density,
    setDensity
  } = prefs;
  const [sel, setSel] = useState([]);
  const XF0 = {
    sc: "",
    ann: "",
    tag: "",
    region: "",
    feat: ""
  };
  const [xf, setXf] = useState(XF0);
  useEffect(() => {
    setXf(XF0);
  }, [seg.t, parent && parent.id]);
  // S3 tag filter: "key" matches any bucket carrying the key, "key=value" exact
  const tagMatch = (o, t) => {
    if (!t) return true;
    const [k, v] = t.split("=").map(s => s.trim());
    const tags = o.tags || {};
    return v === undefined ? Object.keys(tags).some(x => x.toLowerCase().includes(k.toLowerCase())) : Object.entries(tags).some(([x, y]) => x.toLowerCase() === k.toLowerCase() && String(y).toLowerCase() === v.toLowerCase());
  };
  const featMatch = (o, f) => !f || (f === "versioned" ? o.versioning : f === "locked" ? o.objectLock : f === "public" ? o.access && o.access.public : f === "replicated" ? !!o.replication : f === "unreplicated" ? !o.replication : f === "encrypted" ? o.encrypted : f === "lifecycle" ? (o.lifecycle || []).length > 0 : true);
  const cfg = VIEWS[seg.t];
  const key = seg.t + "|" + (parent ? parent.id : "") + "|" + rev;
  const {
    data,
    loading,
    error,
    reload
  } = useResource(key, () => cfg.load(parent || {}), 6000);
  const acc = useAccess();
  // §5.1/§5.2: whole scopes the caller cannot see are hidden (scope discovery);
  // inside a visible scope the API server's list is shown as returned.
  const items = useMemo(() => (data || []).filter(o => acc.canRead(o)), [data, acc.state.user, acc.state.rules]);
  const keys = SORT_KEYS[cfg.kind] || ["name"];
  const activeSort = keys.includes(sort) ? sort : keys[0];
  const filtered = useMemo(() => {
    const s = q.trim().toLowerCase();
    return items.filter(o => (!filters.length || filters.includes(o.status)) && (!s || searchable(o).includes(s)) && (!xf.sc || o.storageClass === xf.sc) && (!xf.ann || Object.keys(o.annotations || {}).includes(xf.ann)) && tagMatch(o, xf.tag) && (!xf.region || o.region === xf.region) && featMatch(o, xf.feat)).sort(SORTS[activeSort].cmp);
  }, [data, q, activeSort, filters, xf.sc, xf.ann, xf.tag, xf.region, xf.feat]);
  const T = TILE[cfg.kind],
    tk = TKEY[cfg.kind];
  // "New …" is a create on the entity that owns this layer, evaluated against
  // the parent object (a pool is created in a cluster, a cluster on a k8s cluster)
  const parentObj = parent && parent.id ? REG[parent.id] || {
    kind: parent.t,
    id: parent.id
  } : null;
  const createKind = seg.t === "clusters" ? "k8sc" : seg.t === "deployconfigs" ? "deployconfig" : cfg.kind;
  const mayCreate = seg.t === "clusters" ? acc.canAnywhere("create", "k8scluster") : acc.canCreateIn(createKind, parentObj);
  const createWhy = mayCreate ? "" : seg.t === "clusters" ? "Needs create on nodepoolallocations at cluster scope" : acc.whyCreateIn(createKind, parentObj);
  // §5.5: a denied create is disabled with the reason, not hidden
  const gateCreate = el => !el ? null : mayCreate ? el : React.cloneElement(el, {
    disabled: true,
    title: createWhy,
    onClick: undefined
  });
  // §5.1: a scope the caller may not read is a 403, not an empty list
  const layerProbe = parentObj && parentObj.kind ? Object.assign({
    kind: cfg.kind
  }, parentObj.kind === "cluster" ? {
    clusterId: parentObj.id
  } : parentObj.kind === "pool" ? {
    poolId: parentObj.id,
    clusterId: parentObj.clusterId
  } : parentObj.kind === "k8sc" ? {
    k8sClusterId: parentObj.id
  } : parentObj.kind === "protectedapp" ? {
    appId: parentObj.id,
    clusterId: parentObj.sourceClusterId
  } : {
    clusterId: parentObj.clusterId
  }) : null;
  const layerEntity = KIND_ENTITY[cfg.kind] || "storagecluster";
  // …unless scope discovery shows a child of this layer the caller may see (a pool in its own sb-sp-* namespace)
  const childVisible = parentObj && acc.state.scopes && (cfg.kind === "pool" && acc.state.scopes.pools.some(p => p.clusterId === parentObj.id && p.visible) || cfg.kind === "protectedapp" && acc.state.scopes.apps.some(a => a.clusterId === parentObj.id && a.visible) || cfg.kind === "cluster" && acc.state.scopes.clusters.some(x => x.k8sIds.includes(parentObj.id) && x.visible));
  const forbidden = layerProbe && acc.state.ready && !acc.state.incomplete && !childVisible && acc.nsOf(layerEntity, layerProbe).length > 0 && !acc.can("read", layerEntity, layerProbe);
  const bad = items.filter(o => healthOf(o) >= 3).length;
  const scope = parent && parent.id && parent.t !== "cluster" ? {
    t: parent.t,
    label: segLabel(parent)
  } : null;
  const prepScope = parent && parent.t === "cluster" ? parent.id : null;
  const candidates = cfg.kind === "host" ? items.filter(o => o.status === "discovered") : [];
  const isPvc = cfg.kind === "pvc";
  const isBucket = cfg.kind === "bucket";
  const regions = isBucket ? [...new Set(items.map(o => o.region).filter(Boolean))].sort() : [];
  const tagKeys = isBucket ? [...new Set(items.flatMap(o => Object.keys(o.tags || {})))].sort() : [];
  const scNames = isPvc ? [...new Set(items.map(o => o.storageClass))].sort() : [];
  const annKeys = isPvc ? [...new Set(items.flatMap(o => Object.keys(o.annotations || {})))].sort() : [];
  const select = cfg.kind === "host" ? {
    has: id => sel.includes(id),
    toggle: id => setSel(s => s.includes(id) ? s.filter(x => x !== id) : s.concat(id))
  } : null;
  const prepare = async () => {
    try {
      await api.hostsPrepare(prepScope || clusterOf(parent), sel);
      window.__toast(`Inspection pod scheduled on ${sel.length} node(s)`);
      setSel([]);
      reload();
    } catch (e) {
      window.__toast(e.message);
    }
  };
  if (error) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload,
    kind: parent ? parent.t : cfg.kind,
    onUp: up,
    upLabel: upLabel
  }));
  if (forbidden) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement(ErrorState, {
    error: {
      status: 403,
      message: `You may not list ${kindPlural(cfg.kind)} in this ${KIND_LABEL[parentObj.kind] || "scope"}. ${acc.why("read", layerEntity, layerProbe)}.`
    },
    onRetry: () => acc.load(),
    kind: parent.t,
    onUp: up,
    upLabel: upLabel
  }));
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Toolbar, {
    items,
    q,
    setQ,
    filters,
    setFilters,
    density,
    setDensity,
    kind: cfg.kind,
    scope: scope,
    sort: activeSort,
    setSort: setSort,
    count: filtered.length,
    onRefresh: reload,
    extra: gateCreate(seg.t === "clusters" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(deployFromDialog(nav), {
        kind: "cluster",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "Deploy cluster") : seg.t === "deployconfigs" && parent && parent.t === "k8sc" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => nav.deployWizard(parent.id)
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "Deploy a cluster") : seg.t === "pools" && parent && parent.t === "cluster" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newPoolDialog(REG[parent.id] || {
        id: parent.id,
        name: "this cluster"
      }), {
        kind: "pool",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New pool") : seg.t === "plans" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => api.sites().then(ss => window.__ui.dialog(newPlanDialog(ss), {
        kind: "plan",
        id: "new"
      }))
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New plan") : seg.t === "pairs" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newPairDialog(), {
        kind: "pair",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New pair") : seg.t === "rpolicies" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newReplPolicyDialog(parent && parent.t === "pair" ? REG[parent.id] : null), {
        kind: "rpolicy",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New policy") : seg.t === "__pairs_old" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newPairDialog(), {
        kind: "cluster pair",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "Pair clusters") : seg.t === "mpaths" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newMPathDialog(), {
        kind: "migration path",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New migration path") : seg.t === "appgroups" && parent ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newAppGroupDialog(REG[parent.id] || {
        id: parent.id
      }), {
        kind: "application group",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "Add application group") : seg.t === "rpolicies" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newRPolicyDialog(), {
        kind: "replication policy",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New policy") : seg.t === "policies" && parent ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newBackupPolicyDialog({
        id: parent.id
      }), {
        kind: "backup policy",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New policy") : seg.t === "protectedapps" && (!parent || !parent.id) ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(protectAppDialog(), {
        kind: "protected application",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "shield",
      s: 12
    }), "Protect application") : isPvc ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: xf.sc,
      onChange: e => setXf(x => Object.assign({}, x, {
        sc: e.target.value
      })),
      title: "Filter by storage class"
    }, /*#__PURE__*/React.createElement("option", {
      value: ""
    }, "All storage classes"), scNames.map(n => /*#__PURE__*/React.createElement("option", {
      key: n,
      value: n
    }, n))), /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: xf.ann,
      onChange: e => setXf(x => Object.assign({}, x, {
        ann: e.target.value
      })),
      title: "Filter by annotation"
    }, /*#__PURE__*/React.createElement("option", {
      value: ""
    }, "Any annotation"), annKeys.map(n => /*#__PURE__*/React.createElement("option", {
      key: n,
      value: n
    }, n))), (xf.sc || xf.ann) && /*#__PURE__*/React.createElement("button", {
      className: "chip",
      onClick: () => setXf(XF0)
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "x",
      s: 10
    }), "clear")) : isBucket ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("input", {
      className: "sel",
      list: "bucket-tag-keys",
      style: {
        width: 170
      },
      placeholder: "tag key or key=value",
      value: xf.tag,
      onChange: e => setXf(x => Object.assign({}, x, {
        tag: e.target.value
      })),
      title: "Filter by S3 bucket tag"
    }), /*#__PURE__*/React.createElement("datalist", {
      id: "bucket-tag-keys"
    }, tagKeys.map(k => /*#__PURE__*/React.createElement("option", {
      key: k,
      value: k
    }))), regions.length > 1 && /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: xf.region,
      onChange: e => setXf(x => Object.assign({}, x, {
        region: e.target.value
      })),
      title: "Filter by region"
    }, /*#__PURE__*/React.createElement("option", {
      value: ""
    }, "All regions"), regions.map(r => /*#__PURE__*/React.createElement("option", {
      key: r,
      value: r
    }, r))), /*#__PURE__*/React.createElement("select", {
      className: "sel",
      value: xf.feat,
      onChange: e => setXf(x => Object.assign({}, x, {
        feat: e.target.value
      })),
      title: "Filter by bucket configuration"
    }, /*#__PURE__*/React.createElement("option", {
      value: ""
    }, "Any configuration"), /*#__PURE__*/React.createElement("option", {
      value: "versioned"
    }, "Versioned"), /*#__PURE__*/React.createElement("option", {
      value: "locked"
    }, "Object lock"), /*#__PURE__*/React.createElement("option", {
      value: "encrypted"
    }, "Encrypted"), /*#__PURE__*/React.createElement("option", {
      value: "public"
    }, "Public"), /*#__PURE__*/React.createElement("option", {
      value: "replicated"
    }, "Replicated"), /*#__PURE__*/React.createElement("option", {
      value: "unreplicated"
    }, "Not replicated"), /*#__PURE__*/React.createElement("option", {
      value: "lifecycle"
    }, "Has lifecycle rules")), (xf.tag || xf.region || xf.feat) && /*#__PURE__*/React.createElement("button", {
      className: "chip",
      onClick: () => setXf(XF0)
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "x",
      s: 10
    }), "clear"), parent && /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newBucketDialog(REG[parent.id] || {
        id: parent.id,
        name: "cluster",
        objectStorage: {}
      }), {
        kind: "bucket",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New bucket")) : seg.t === "migrations" && parent && parent.t === "cluster" ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newMigrationDialog(REG[parent.id] || {
        id: parent.id
      }), {
        kind: "migration",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "move",
      s: 12
    }), "New migration") : seg.t === "cgroups" && parent ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(newCgroupDialog({
        id: parent.id
      }), {
        kind: "consistency group",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "New group") : seg.t === "cgsnapshots" && parent ? /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      onClick: () => window.__ui.dialog(ACTIONS.cgroup(REG[parent.id] || {
        id: parent.id,
        name: "group",
        counts: {}
      })[0].dialog, {
        kind: "group snapshot",
        id: "new"
      })
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "camera",
      s: 12
    }), "Take snapshot") : seg.t === "hosts" && candidates.length ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("button", {
      className: "chip",
      onClick: () => setSel(sel.length === candidates.length ? [] : candidates.map(c => c.id))
    }, sel.length === candidates.length ? "Clear" : `Select all ${candidates.length}`), /*#__PURE__*/React.createElement("button", {
      className: "btn primary",
      disabled: !sel.length,
      onClick: prepare
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "plus",
      s: 12
    }), "Prepare ", sel.length || "", " node", sel.length === 1 ? "" : "s")) : null)
  }), /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, !loading && candidates.length > 0 && /*#__PURE__*/React.createElement("div", {
    className: "banner",
    style: {
      color: "var(--accent)",
      background: "var(--accent-soft)",
      borderColor: "var(--accent-line)"
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "host",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, candidates.length, " Kubernetes worker node(s) are not prepared."), " Select the ones that should become storage hosts \u2014 an inspection pod collects their NUMA topology, devices and NICs before you configure them.")), !loading && bad > 0 && activeSort === "health" && !q && !filters.length && /*#__PURE__*/React.createElement("div", {
    className: "banner"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 15
  }), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement("b", null, bad, " of ", items.length, " need attention."), " Sorted so non-healthy objects come first.")), loading ? /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, Array.from({
    length: 8
  }).map((_, i) => /*#__PURE__*/React.createElement("div", {
    className: "skel",
    key: i
  }))) : items.length === 0 ? /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: LAYER_META[seg.t].icon,
    s: 24
  }), /*#__PURE__*/React.createElement("b", null, "No ", kindPlural(cfg.kind), " yet"), /*#__PURE__*/React.createElement("span", null, "The API returned an empty collection for this scope."), seg.t === "deployconfigs" && parent && parent.t === "k8sc" && /*#__PURE__*/React.createElement("button", {
    className: "btn primary",
    style: {
      marginTop: 8
    },
    onClick: () => nav.deployWizard(parent.id)
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "plus",
    s: 12
  }), "Deploy a cluster")) : filtered.length === 0 ? /*#__PURE__*/React.createElement("div", {
    className: "empty"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "search",
    s: 22
  }), /*#__PURE__*/React.createElement("b", null, "No ", kindPlural(cfg.kind), " match"), /*#__PURE__*/React.createElement("span", null, "Adjust the search or clear the status filters."), /*#__PURE__*/React.createElement("button", {
    className: "chip",
    style: {
      marginTop: 6
    },
    onClick: () => {
      setQ("");
      setFilters([]);
    }
  }, "Reset filters")) : /*#__PURE__*/React.createElement("div", {
    className: "grid"
  }, filtered.map(o => /*#__PURE__*/React.createElement(T, {
    key: o.id,
    [tk]: o,
    nav,
    select
  })))));
}
function DetailView({
  seg,
  nav,
  rev,
  up,
  upLabel
}) {
  const {
    data,
    loading,
    error,
    reload
  } = useResource(seg.t + "|" + seg.id + "|" + rev, () => GETTER[seg.t](seg.id), 6000);
  if (error) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement(ErrorState, {
    error: error,
    onRetry: reload,
    kind: seg.t,
    onUp: up,
    upLabel: upLabel
  }));
  if (loading || !data) return /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "dhead"
  }, /*#__PURE__*/React.createElement("div", {
    className: "skel",
    style: {
      height: 26,
      width: 220
    }
  })), /*#__PURE__*/React.createElement("div", {
    className: "stats"
  }, Array.from({
    length: 6
  }).map((_, i) => /*#__PURE__*/React.createElement("div", {
    className: "skel",
    key: i,
    style: {
      height: 62
    }
  }))), /*#__PURE__*/React.createElement("div", {
    className: "dcols"
  }, /*#__PURE__*/React.createElement("div", {
    className: "skel",
    style: {
      height: 300
    }
  }), /*#__PURE__*/React.createElement("div", {
    className: "skel",
    style: {
      height: 300
    }
  })));
  return /*#__PURE__*/React.createElement("div", {
    className: "scroll",
    style: {
      paddingTop: 2
    }
  }, /*#__PURE__*/React.createElement(Detail, {
    obj: data,
    nav: nav
  }));
}
function MockPanel() {
  const [open, setOpen] = useState(false);
  const [, f] = useState(0);
  const M = window.SB_MOCK;
  const set = (k, v) => {
    M[k] = v;
    f(n => n + 1);
  };
  const Row = ({
    label,
    on,
    onClick
  }) => /*#__PURE__*/React.createElement("button", {
    className: "menu-item",
    onClick: onClick
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, label), /*#__PURE__*/React.createElement("span", {
    className: "badge",
    style: on ? {
      color: "var(--accent)",
      borderColor: "var(--accent-line)"
    } : null
  }, on ? "on" : "off"));
  return /*#__PURE__*/React.createElement("div", {
    className: "switcher"
  }, /*#__PURE__*/React.createElement("button", {
    className: "swbtn",
    onClick: () => setOpen(!open),
    title: "Mock API controls"
  }, /*#__PURE__*/React.createElement(Dot, {
    c: "var(--warn)"
  }), "mock api", /*#__PURE__*/React.createElement(Icon, {
    n: "chevd",
    s: 9
  })), open && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    style: {
      position: "fixed",
      inset: 0,
      zIndex: 50
    },
    onClick: () => setOpen(false)
  }), /*#__PURE__*/React.createElement("div", {
    className: "menu",
    style: {
      right: 0,
      left: "auto"
    }
  }, /*#__PURE__*/React.createElement("div", {
    className: "menu-lbl"
  }, "Fixture backend \xB7 ", M.requests, " requests served"), /*#__PURE__*/React.createElement(Row, {
    label: "Slow network (1.5\u20133 s)",
    on: M.latency[1] > 1000,
    onClick: () => set("latency", M.latency[1] > 1000 ? [140, 380] : [1500, 3000])
  }), /*#__PURE__*/React.createElement(Row, {
    label: "Kubernetes API offline",
    on: M.offline,
    onClick: () => set("offline", !M.offline)
  }), /*#__PURE__*/React.createElement(Row, {
    label: "Empty collections",
    on: M.forceEmpty,
    onClick: () => set("forceEmpty", !M.forceEmpty)
  }), /*#__PURE__*/React.createElement(Row, {
    label: "Random 503s (20%)",
    on: M.failRate > 0,
    onClick: () => set("failRate", M.failRate ? 0 : .2)
  }), /*#__PURE__*/React.createElement("button", {
    className: "menu-item",
    onClick: () => {
      set("failNext", true);
      setOpen(false);
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, "Fail the next request"), /*#__PURE__*/React.createElement(Icon, {
    n: "alert",
    s: 12
  })), /*#__PURE__*/React.createElement("div", {
    className: "menu-lbl",
    style: {
      borderTop: "1px solid var(--line)",
      marginTop: 4,
      paddingTop: 8
    }
  }, "Base URL"), /*#__PURE__*/React.createElement("div", {
    className: "menu-item mono",
    style: {
      color: "var(--dim)",
      fontSize: 11
    }
  }, API))));
}
class ViewBoundary extends React.Component {
  constructor(p) {
    super(p);
    this.state = {
      err: null
    };
  }
  static getDerivedStateFromError(err) {
    return {
      err
    };
  }
  componentDidUpdate(prev) {
    if (prev.routeKey !== this.props.routeKey && this.state.err) this.setState({
      err: null
    });
  }
  render() {
    if (!this.state.err) return this.props.children;
    return /*#__PURE__*/React.createElement("div", {
      className: "scroll"
    }, /*#__PURE__*/React.createElement("div", {
      className: "empty"
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "alert",
      s: 24,
      c: "var(--bad)"
    }), /*#__PURE__*/React.createElement("b", {
      style: {
        color: "var(--text)"
      }
    }, "This view failed to render"), /*#__PURE__*/React.createElement("span", {
      style: {
        maxWidth: 460
      }
    }, String(this.state.err.message || this.state.err)), /*#__PURE__*/React.createElement("button", {
      className: "chip",
      style: {
        marginTop: 8
      },
      onClick: this.props.onReset
    }, "Back to clusters")));
  }
}
function App() {
  const [rawPath, setPath] = useLocal("sb.path", [{
    t: "clusters"
  }]);
  // a stored path may name a layer that no longer exists (renamed kinds) — fall back to the root
  const path = useMemo(() => Array.isArray(rawPath) && rawPath.length && rawPath.every(s => LAYER_META[s.t]) ? rawPath : [{
    t: "clusters"
  }], [rawPath]);
  const [theme, setTheme] = useLocal("sb.theme", "light");
  const [density, setDensity] = useLocal("sb.density", "340px");
  const [sort, setSort] = useLocal("sb.sort", "health");
  const [q, setQ] = useState("");
  const [filters, setFilters] = useState([]);
  const [menu, setMenu] = useState(false);
  const [toast, setToast] = useState(null);
  const [clusters, setClusters] = useState([]);
  const [alertCount, setAlertCount] = useState(0);
  const [rev, setRev] = useState(0);
  const [, force] = useState(0);
  useEffect(() => {
    document.documentElement.setAttribute("data-theme", theme);
  }, [theme]);
  useEffect(() => {
    document.documentElement.style.setProperty("--grid", density);
  }, [density]);
  useEffect(() => {
    window.__toast = m => {
      setToast(m);
      setTimeout(() => setToast(null), 2200);
    };
    window.__refresh = () => setRev(r => r + 1);
    window.__removed = obj => {
      const p = pathRef.current,
        last = p[p.length - 1];
      delete REG[obj.id];
      if (last && last.id === obj.id && p.length > 1) setPath(p.slice(0, -1));
      setRev(r => r + 1);
    };
  }, []);
  useEffect(() => {
    api.clusters().then(setClusters).catch(() => {});
  }, [rev]);
  // zones are a small, cluster-independent collection; warm the registry once so
  // every regName(zoneId) call zone can label them without its own fetch
  useEffect(() => {
    api.zones().then(() => force(n => n + 1)).catch(() => {});
  }, [rev]);
  // DR clusters are named all over the application-DR views but never appear in a
  // breadcrumb path, so warm them too
  useEffect(() => {
    api.drClusters().then(() => force(n => n + 1)).catch(() => {});
  }, [rev]);
  useEffect(() => {
    const load = () => api.allAlerts().then(as => setAlertCount(as.filter(a => a.severity === "critical" && !a.silenced).length)).catch(() => {});
    load();
    const i = setInterval(load, 15000);
    return () => clearInterval(i);
  }, [rev]);
  const acc = useAccess();
  useEffect(() => {
    acc.load();
    // the fixture switcher changes identity in place; everything re-reads
    const h = () => {
      force(x => x + 1);
      setRev(r => r + 1);
    };
    window.addEventListener("sb:access", h);
    return () => window.removeEventListener("sb:access", h);
  }, []);
  const cur = path[path.length - 1];
  const viewKey = path.map(s => s.t + (s.id || "")).join("/");
  const pathRef = useRef(path);
  pathRef.current = path;
  useEffect(() => {
    setQ("");
    setFilters([]);
  }, [viewKey]);
  useEffect(() => {
    const missing = path.filter(s => s.id && !REG[s.id]);
    if (missing.length) Promise.all(missing.map(s => resolve(s.t, s.id))).then(() => force(n => n + 1));
  }, [viewKey]);
  const go = useCallback(p => {
    setPath(p);
    setMenu(false);
  }, [setPath]);
  const nav = useMemo(() => ({
    detail: o => go(detailPath(o)),
    layer: (o, l) => go([...detailPath(o), {
      t: l
    }]),
    layerRef: (cid, l) => go([...pC(cid), {
      t: l
    }]),
    openCluster: cid => go(pC(cid)),
    openHost: (cid, hid) => go(pH(cid, hid)),
    openNode: (cid, nid) => go(pN(cid, nid)),
    openDevice: (cid, nid, did) => go(pD(cid, nid, did)),
    openPool: (cid, pid) => go(pP(cid, pid)),
    openVolume: (cid, pid, vid) => go(pV(cid, pid, vid)),
    openPair: id => go(pPair(id)),
    openRPolicy: id => go(pRPol(id)),
    openPolicy: (cid, id) => go([...pC(cid), {
      t: "policies"
    }, {
      t: "policy",
      id
    }]),
    openZone: id => go(pZone(id)),
    openK8s: id => go(pK(id)),
    openPlan: id => go(pPlan(id)),
    openSite: id => go(pSite(id)),
    // the plan and the application both name sites and methods as strings, the
    // way the CRs do, so the console resolves the name to its object
    openPairByName: name => api.pairs().then(ps => {
      const p = ps.find(x => x.name === name);
      if (p) go(pPair(p.id));
    }).catch(() => {}),
    openRPolicyByName: name => api.allRPolicies().then(ps => {
      const p = ps.find(x => x.name === name);
      if (p) go(pRPol(p.id));
    }).catch(() => {}),
    openSlotByName: name => api.slots().then(ss => {
      const s = ss.find(x => x.name === name);
      if (s) go(pSlot(s.id));
    }).catch(() => {}),
    openClusterByName: name => api.clusters().then(cs => {
      const c = cs.find(x => x.name === name);
      if (c) go(pC(c.id));
    }).catch(() => {}),
    openPvcByName: name => api.pvcs().then(ps => {
      const p = ps.find(x => x.name === name);
      if (p) go(detailPath(p));
    }).catch(() => {}),
    openPlanByName: name => api.plans().then(ps => {
      const p = ps.find(x => x.name === name);
      if (p) go(pPlan(p.id));
    }).catch(() => {}),
    openSiteByName: name => api.sites().then(ss => {
      const s = ss.find(x => x.name === name);
      if (s) go(pSite(s.id));
    }).catch(() => {}),
    openProtectedApp: id => go(pApp(id)),
    openBucket: id => (REG[id] ? Promise.resolve(REG[id]) : api.bucket(id)).then(x => go(pBucket(x.clusterId, x.id))).catch(() => {}),
    openPvc: id => (REG[id] ? Promise.resolve(REG[id]) : api.pvc(id)).then(x => go(pPvc(x.k8sClusterId, x.id))).catch(() => {}),
    openStorageClass: id => (REG[id] ? Promise.resolve(REG[id]) : api.storageClass(id)).then(x => go(pSc(x.k8sClusterId, x.id))).catch(() => {}),
    openVolumeById: id => (REG[id] ? Promise.resolve(REG[id]) : api.volume(id)).then(v => go(pV(v.clusterId, v.poolId, v.id))).catch(() => {}),
    k8s: () => go([{
      t: "k8s"
    }]),
    openCgroup: gid => (REG[gid] ? Promise.resolve(REG[gid]) : api.cgroup(gid)).then(g => go(pCg(g.clusterId, g.id))).catch(() => {}),
    drLayer: l => go([{
      t: "dr"
    }, {
      t: l
    }]),
    openMPath: id => go(pMp(id)),
    openAppGroup: (pid, id) => go(pAg(pid, id)),
    zones: () => go([{
      t: "dr"
    }, {
      t: "zones"
    }]),
    dr: () => go([{
      t: "dr"
    }]),
    cp: () => go([{
      t: "cp"
    }]),
    openSnapshot: (cid, sid) => (REG[sid] ? Promise.resolve(REG[sid]) : api.snapshot(sid)).then(s => go(detailPath(s))).catch(() => {}),
    hostNodes: h => go([...pH(h.clusterId, h.id), {
      t: "nodes"
    }]),
    discovery: kid => go([...pK(kid), {
      t: "discovery"
    }]),
    deployWizard: kid => go([...pK(kid), {
      t: "deploywizard"
    }]),
    deployConfig: (kid, id) => go(pDep(kid, id)),
    k8sDetail: kid => go(pK(kid)),
    root: () => go([{
      t: "clusters"
    }])
  }), [go]);
  useEffect(() => {
    const h = e => {
      if (/^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName)) return;
      if (e.key === "Escape") {
        if (window.__ui && window.__ui.isOpen()) {
          window.__ui.close();
          return;
        }
        if (path.length > 1) go(path.slice(0, -1));
        return;
      }
      if (e.key === "/") {
        e.preventDefault();
        const el = document.querySelector(".search input");
        el && el.focus();
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [path, go]);
  const ctxCluster = useMemo(() => {
    const s = path.find(x => x.t === "cluster");
    return s ? REG[s.id] : null;
  }, [viewKey, clusters.length]);
  const parentSeg = path[path.length - 2];
  const apiHint = cur.id ? DETAIL_API[cur.t] : VIEWS[cur.t] ? VIEWS[cur.t].api(parentSeg || {}) : "—";
  const s0 = path[0] && path[0].t;
  const section = s0 === "dr" ? "dr" : s0 === "k8s" ? "k8s" : s0 === "cp" ? "cp" : "clusters";
  const upOne = path.length > 1 ? () => go(path.slice(0, -1)) : null;
  const upLabel = path.length > 1 ? segLabel(path[path.length - 2]) : "";
  return /*#__PURE__*/React.createElement("div", {
    className: "app"
  }, /*#__PURE__*/React.createElement("div", {
    className: "topbar"
  }, /*#__PURE__*/React.createElement("a", {
    href: "https://simplyblock.io",
    target: "_blank",
    rel: "noopener",
    style: {
      display: "flex",
      alignItems: "center"
    },
    title: "simplyblock"
  }, /*#__PURE__*/React.createElement("img", {
    className: "logo",
    src: window.SB_CONFIG && window.SB_CONFIG.logoUrl || "https://simplyblock.io/assets/images/Logo-white.svg",
    alt: "simplyblock",
    onError: e => {
      e.target.style.display = "none";
      e.target.nextSibling.style.display = "block";
    }
  }), /*#__PURE__*/React.createElement("span", {
    className: "logofb",
    style: {
      display: "none"
    }
  }, "simplyblock")), /*#__PURE__*/React.createElement("span", {
    className: "prod"
  }, "Control Center"), /*#__PURE__*/React.createElement("div", {
    className: "sectionsw"
  }, /*#__PURE__*/React.createElement("button", {
    className: section === "clusters" ? "on" : "",
    onClick: () => nav.root(),
    title: "Clusters"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 13
  }), /*#__PURE__*/React.createElement("span", {
    className: "swlabel"
  }, "Clusters")), acc.canAnywhere("read", "k8scluster") && /*#__PURE__*/React.createElement("button", {
    className: section === "k8s" ? "on" : "",
    onClick: () => nav.k8s(),
    title: "Kubernetes"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "k8s",
    s: 13
  }), /*#__PURE__*/React.createElement("span", {
    className: "swlabel"
  }, "Kubernetes")), (acc.canAnywhere("read", "drpolicy") || acc.canAnywhere("read", "replicationpolicy") || acc.canAnywhere("read", "application")) && /*#__PURE__*/React.createElement("button", {
    className: section === "dr" ? "on" : "",
    onClick: () => nav.dr(),
    title: "Disaster recovery"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "shield",
    s: 13
  }), /*#__PURE__*/React.createElement("span", {
    className: "swlabel"
  }, "Disaster recovery")), /*#__PURE__*/React.createElement("button", {
    className: section === "cp" ? "on" : "",
    onClick: () => nav.cp(),
    title: "Control plane"
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "host",
    s: 13
  }), /*#__PURE__*/React.createElement("span", {
    className: "swlabel"
  }, "Control plane"))), section === "clusters" && /*#__PURE__*/React.createElement("div", {
    className: "switcher"
  }, /*#__PURE__*/React.createElement("button", {
    className: "swbtn",
    onClick: () => setMenu(!menu)
  }, ctxCluster ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Dot, {
    c: STATUS_META[ctxCluster.status].c
  }), ctxCluster.name) : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 13
  }), "All clusters"), /*#__PURE__*/React.createElement(Icon, {
    n: "chevd",
    s: 9
  })), menu && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("div", {
    style: {
      position: "fixed",
      inset: 0,
      zIndex: 50
    },
    onClick: () => setMenu(false)
  }), /*#__PURE__*/React.createElement("div", {
    className: "menu"
  }, /*#__PURE__*/React.createElement("div", {
    className: "menu-lbl"
  }, "Managed clusters \xB7 ", clusters.length), /*#__PURE__*/React.createElement("button", {
    className: "menu-item",
    onClick: () => nav.root()
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "cluster",
    s: 13,
    c: "var(--dim)"
  }), "All clusters"), clusters.map(c => /*#__PURE__*/React.createElement("button", {
    key: c.id,
    className: "menu-item" + (ctxCluster && ctxCluster.id === c.id ? " on" : ""),
    onClick: () => nav.openCluster(c.id)
  }, /*#__PURE__*/React.createElement(Dot, {
    c: STATUS_META[c.status].c
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, c.name), /*#__PURE__*/React.createElement("span", {
    className: "badge"
  }, c.siting === "edge" ? "edge" : "dc")))))), /*#__PURE__*/React.createElement("div", {
    className: "spacer"
  }), window.SB_CONFIG.mock && /*#__PURE__*/React.createElement(MockPanel, null), /*#__PURE__*/React.createElement("span", {
    className: "env"
  }, /*#__PURE__*/React.createElement("span", {
    className: "pulse"
  }), "control plane healthy"), /*#__PURE__*/React.createElement("button", {
    className: "tbtn bellwrap",
    title: alertCount ? `${alertCount} critical alert(s)` : "No critical alerts",
    onClick: () => {
      const s = path.find(x => x.t === "cluster");
      if (s) nav.openCluster(s.id);else if (clusters.length) nav.openCluster(clusters.find(c => c.status === "degraded") ? clusters.find(c => c.status === "degraded").id : clusters[0].id);
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    n: "bell",
    s: 14
  }), alertCount > 0 && /*#__PURE__*/React.createElement("span", {
    className: "bellbadge"
  }, alertCount > 99 ? "99+" : alertCount)), /*#__PURE__*/React.createElement("button", {
    className: "tbtn",
    title: "Toggle theme",
    onClick: () => setTheme(theme === "light" ? "dark" : "light")
  }, /*#__PURE__*/React.createElement(Icon, {
    n: theme === "light" ? "moon" : "sun",
    s: 14
  })), /*#__PURE__*/React.createElement(IdentityMenu, {
    here: cur.id ? REG[cur.id] : null
  })), /*#__PURE__*/React.createElement("div", {
    className: "crumbbar"
  }, /*#__PURE__*/React.createElement("div", {
    className: "crumbs"
  }, path.map((s, i) => {
    const last = i === path.length - 1;
    return /*#__PURE__*/React.createElement(React.Fragment, {
      key: i
    }, i > 0 && /*#__PURE__*/React.createElement("span", {
      className: "csep"
    }, /*#__PURE__*/React.createElement(Icon, {
      n: "chev",
      s: 12,
      sw: 1.6
    })), /*#__PURE__*/React.createElement("button", {
      className: "crumb" + (last ? " cur" : ""),
      onClick: () => !last && go(path.slice(0, i + 1))
    }, /*#__PURE__*/React.createElement("span", {
      className: "ic"
    }, /*#__PURE__*/React.createElement(Icon, {
      n: LAYER_META[s.t].icon,
      s: 13
    })), segLabel(s)));
  })), /*#__PURE__*/React.createElement("span", {
    className: "apihint",
    title: apiHint
  }, apiHint)), !acc.state.ready ? /*#__PURE__*/React.createElement("div", {
    className: "scroll"
  }, /*#__PURE__*/React.createElement("div", {
    className: "lmsg"
  }, "Resolving your access\u2026")) : /*#__PURE__*/React.createElement(ViewBoundary, {
    routeKey: viewKey,
    onReset: () => nav.root()
  }, cur.id ? /*#__PURE__*/React.createElement(DetailView, {
    key: viewKey,
    seg: cur,
    nav: nav,
    rev: rev,
    up: upOne,
    upLabel: upLabel
  }) : cur.t === "dr" ? /*#__PURE__*/React.createElement(DrHome, {
    key: viewKey,
    nav: nav
  }) : cur.t === "cp" ? /*#__PURE__*/React.createElement(ControlPlaneView, {
    key: viewKey,
    nav: nav
  }) : cur.t === "discovery" ? /*#__PURE__*/React.createElement(DiscoveryView, {
    key: viewKey,
    kid: (path.find(x => x.t === "k8sc") || {}).id,
    nav: nav
  }) : cur.t === "deploywizard" ? /*#__PURE__*/React.createElement(DeployWizard, {
    key: viewKey,
    kid: (path.find(x => x.t === "k8sc") || {}).id,
    nav: nav
  }) : /*#__PURE__*/React.createElement(OverviewView, {
    key: viewKey,
    seg: cur,
    parent: parentSeg,
    nav: nav,
    rev: rev,
    up: upOne,
    upLabel: upLabel,
    prefs: {
      q,
      setQ,
      sort,
      setSort,
      filters,
      setFilters,
      density,
      setDensity
    }
  })), /*#__PURE__*/React.createElement(UiLayer, {
    routeKey: viewKey
  }), toast && /*#__PURE__*/React.createElement("div", {
    className: "toast"
  }, toast));
}
ReactDOM.createRoot(document.getElementById("root")).render(/*#__PURE__*/React.createElement(App, null));
})();
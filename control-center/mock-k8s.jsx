// ---------------------------------------------------------------------------
// MOCK KUBERNETES API SERVER
// Serves the fixture store as Kubernetes objects on the real paths, so the
// console can be developed against the same shapes the API server returns.
// Delete this script and point SB_CONFIG at a real cluster; nothing else moves.
//
// Kinds WITHOUT a CRD in storage.simplyblock.io/v1alpha1 — replication, DR,
// consistency groups, migrations, buckets, zones — are served from the operator
// API under /proposed/ and are flagged in the UI. That boundary is deliberate:
// it is the list of CRDs the model still owes the console.
// ---------------------------------------------------------------------------
const KDB = () => window.SB_DB;
const KU = () => window.SB_UTIL;
const KNS = () => window.SB_CONFIG.namespace || "simplyblock";
const KGROUP = "storage.simplyblock.io";
const KAPI = KGROUP + "/v1alpha1";

const kmeta = (name, o, extra) => Object.assign({
  name, namespace: KNS(), uid: o.uuid,
  creationTimestamp: o.created_at || o.prepared_at || null,
  generation: 1,
  resourceVersion: String(1000 + (o.__rv || 0)),
  labels: Object.assign({"app.kubernetes.io/managed-by": "simplyblock-operator"}, (extra && extra.labels) || {}),
  annotations: (extra && extra.annotations) || {}
}, (extra && extra.rest) || {});

const dns = s => String(s || "").toLowerCase().replace(/[^a-z0-9.-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63);
const clusterName = id => dns((KDB().clusters.find(c => c.uuid === id) || {}).name);
const activeOps = (kind, targetName) => {
  const o = (KDB().ops || []).find(x => x.target_kind === kind && x.target_name === targetName
    && !["Succeeded", "Failed", "Aborted"].includes(x.phase));
  return o ? {kind: o.kind, name: o.name, action: o.action} : null;
};
const cond = (type, ok, reason, message) => ({
  type, status: ok ? "True" : "False", reason, message,
  lastTransitionTime: KU().ago(0), observedGeneration: 1
});

// ---- entity mappers: fixture record -> Kubernetes object -------------------
const TO_K8S = {
  StorageCluster: c => ({
    apiVersion: KAPI, kind: "StorageCluster",
    metadata: kmeta(dns(c.name), c, {labels: {[KGROUP + "/environment"]: c.cluster_type}}),
    spec: {
      stripe: {dataChunks: c.distr_ndcs, parityChunks: c.distr_npcs},
      enableFailureDomains: !!c.failure_domain_enabled,
      edgeCluster: c.location_type === "edge",
      version: c.cluster_version,  // status-side: the operator release
      enableFileStorage: !!(c.file_storage || {}).enabled,
      enableObjectStorage: !!(c.object_storage || {}).enabled,
      encryption: c.kms ? {provider: c.kms.provider, keyName: c.kms.key_name} : null
    },
    status: {
      phase: pascal(c.status),
      observedGeneration: 1,
      activeOpsRef: activeOps("StorageCluster", dns(c.name)),
      clusterId: c.uuid,
      nodes: {total: c.storage_nodes_count, ready: c.storage_nodes_online},
      devices: {total: c.devices_count, ready: c.devices_online},
      capacity: {total: String(c.size_total), used: String(c.size_util)},
      rebalancing: !!c.rebalancing,
      pools: c.pools_count, volumes: c.lvols_count,
      io: {readIops: c.io_stats.read_io_ps, writeIops: c.io_stats.write_io_ps,
        readBytes: c.io_stats.read_bytes_ps, writeBytes: c.io_stats.write_bytes_ps},
      ioHistory: c.io_history,
      failureDomains: (c.failure_domains || []).map(f => ({name: f.name, nodes: f.nodes})),
      extras: c,
      conditions: [
        cond("Ready", c.status === "online", pascal(c.status), "Cluster reported by the control plane"),
        cond("Degraded", c.status === "degraded", c.status === "degraded" ? "NodesUnavailable" : "AllNodesReady", "")
      ]
    }
  }),
  StorageNode: n => ({
    apiVersion: KAPI, kind: "StorageNode",
    metadata: kmeta(dns(n.hostname), n, {labels: {
      [KGROUP + "/cluster"]: clusterName(n.cluster_id),
      [KGROUP + "/owner-kind"]: "StorageCluster",
      [KGROUP + "/owner-name"]: clusterName(n.cluster_id)
    }, rest: {ownerReferences: [{apiVersion: KAPI, kind: "StorageCluster",
      name: clusterName(n.cluster_id), uid: n.cluster_id, controller: true}]}}),
    spec: {
      nodeName: (KDB().hosts.find(h => h.uuid === n.host_id) || {}).hostname || null,
      sizing: {maxSubsystemCount: n.max_subsystem_count, vcpuCount: n.vcpu_reserved,
        minHugePagesSize: Math.round((n.hugepages_total || 0) / 1e9) + "G"},
      mgmtInterface: (KDB().hosts.find(h => h.uuid === n.host_id) || {}).mgmt_nic || null,
      dataInterfaces: (n.data_nics || []).map(x => x.name),
      failureDomain: n.failure_domain || null
    },
    status: {
      phase: pascal(n.status), observedGeneration: 1,
      activeOpsRef: activeOps("StorageNode", dns(n.hostname)),
      nodeId: n.uuid,
      dataIPs: (n.data_nics || []).map(x => `${x.ip}:${x.port}`),
      devices: {total: n.devices_count, ready: n.devices_online},
      capacity: {total: String(n.size_total), used: String(n.size_util)},
      memory: {total: String(n.memory_total), used: String(n.memory_used),
        reserved: n.memory_reserved == null ? null : String(n.memory_reserved)},
      hugePages: {total: String(n.hugepages_total), used: String(n.hugepages_used)},
      spdkVersion: n.spdk_version,
      io: {readIops: n.io_stats.read_io_ps, writeIops: n.io_stats.write_io_ps,
        readBytes: n.io_stats.read_bytes_ps, writeBytes: n.io_stats.write_bytes_ps},
      ioHistory: n.io_history, extras: n,
      conditions: [cond("Ready", n.status === "online", pascal(n.status), "")]
    }
  }),
  StorageDevice: d => ({
    apiVersion: KAPI, kind: "StorageDevice",
    metadata: kmeta(dns(d.serial_number), d, {labels: {
      [KGROUP + "/cluster"]: clusterName(d.cluster_id),
      [KGROUP + "/owner-kind"]: "StorageNode",
      [KGROUP + "/owner-name"]: dns((KDB().storage_nodes.find(n => n.uuid === d.node_id) || {}).hostname)
    }}),
    spec: {
      pcieAddress: d.pcie_address, devicePath: d.device_name,
      deviceClass: d.pcie_address ? "NVMe" : "Block",
      numaSocket: d.numa_socket
    },
    status: {
      phase: pascal(d.status), observedGeneration: 1,
      activeOpsRef: activeOps("StorageDevice", dns(d.serial_number)),
      deviceId: d.uuid, serialNumber: d.serial_number,
      model: d.model_number, firmware: d.firmware_revision,
      health: d.health_check ? pascal(d.health_check) : null,
      capacity: {total: String(d.size_total), used: String(d.size_util)},
      temperatureCelsius: d.temperature_c, percentageUsed: d.percentage_used,
      powerOnHours: d.power_on_hours,
      io: {readIops: d.io_stats.read_io_ps, writeIops: d.io_stats.write_io_ps,
        readBytes: d.io_stats.read_bytes_ps, writeBytes: d.io_stats.write_bytes_ps},
      ioHistory: d.io_history, extras: d,
      conditions: [cond("Ready", d.status === "online", pascal(d.status), "")]
    }
  }),
  StoragePool: p => ({
    apiVersion: KAPI, kind: "StoragePool",
    metadata: kmeta(dns(p.pool_name), p, {labels: {
      [KGROUP + "/cluster"]: clusterName(p.cluster_id),
      [KGROUP + "/owner-kind"]: "StorageCluster",
      [KGROUP + "/owner-name"]: clusterName(p.cluster_id)
    }}),
    spec: {
      clusterRef: {name: clusterName(p.cluster_id)},
      enabled: p.enabled !== false,
      storageClassParameters: p.qos ? {
        qosRwIops: p.qos.rw_ios_per_sec, qosRwMbytes: p.qos.rw_mbytes_per_sec,
        qosRMbytes: p.qos.r_mbytes_per_sec, qosWMbytes: p.qos.w_mbytes_per_sec
      } : {}
    },
    status: {
      phase: p.enabled === false ? "Disabled" : "Enabled", observedGeneration: 1,
      activeOpsRef: activeOps("StoragePool", dns(p.pool_name)),
      poolId: p.uuid,
      storageClassNames: (p.storage_classes || []).map(x => x.name),
      volumes: {total: p.lvols_count, ready: p.lvols_online},
      capacity: {provisioned: String(p.size_prov), used: String(p.size_util)},
      extras: p,
      conditions: [cond("Ready", p.enabled !== false, p.enabled === false ? "Disabled" : "Enabled", "")]
    }
  }),
  StorageBackup: b => ({
    apiVersion: KAPI, kind: "StorageBackup",
    metadata: kmeta(dns(b.chain_id), b, {labels: {
      [KGROUP + "/cluster"]: clusterName(b.cluster_id),
      [KGROUP + "/volume"]: dns(b.lvol_name)
    }}),
    spec: {
      sourceRef: {kind: "PersistentVolume", name: dns(b.lvol_name)},
      bucket: b.bucket, policyName: b.policy_name || null
    },
    status: {
      phase: pascal(b.status || "online"), observedGeneration: 1,
      activeOpsRef: activeOps("StorageBackup", dns(b.chain_id)),
      backupId: b.uuid, chainId: b.chain_id,
      versions: (b.versions || []).map(v => ({id: v.id, sequence: v.seq,
        type: pascal(v.type), createdAt: v.created_at, size: String(v.size),
        sourceSnapshot: v.source_snapshot_name, mergedCount: v.merged_count || 0})),
      lastMergeAt: b.last_merge_at || null, extras: b,
      conditions: [cond("Ready", (b.status || "online") === "online", "Available", "")]
    }
  })
};

const pascal = s => String(s || "").split(/[_\s-]+/)
  .map(w => w.charAt(0).toUpperCase() + w.slice(1)).join("");

// which fixture collection backs each kind, and how a name is derived
const COLL = {
  StorageCluster: ["clusters", c => dns(c.name)],
  StorageNode: ["storage_nodes", n => dns(n.hostname)],
  StorageDevice: ["devices", d => dns(d.serial_number)],
  StoragePool: ["pools", p => dns(p.pool_name)],
  StorageBackup: ["backups", b => dns(b.chain_id)]
};

// ---- Ops execution ---------------------------------------------------------
// Creating an Ops object runs the action. The object then carries the phase and
// the step, and the entity carries activeOpsRef until it reaches a terminal phase.
KDB().ops = KDB().ops || [];

const OPS_ACTIONS = {
  StorageClusterOps: ["Suspend", "Activate", "Expand", "Rebalance", "CancelTask"],
  StorageNodeOps: ["Restart", "Shutdown", "Migrate", "Remove", "AddDevice"],
  StorageDeviceOps: ["Restart", "Fail", "Remove", "HealthCheck"],
  StoragePoolOps: ["Enable", "Disable", "UpdateQos"],
  StorageBackupOps: ["Merge", "Restore", "Export", "Delete"],
  PersistentVolumeOps: ["Resize", "Snapshot", "Clone", "Migrate", "Backup"],
  ControlPlaneOps: ["Restart", "RestoreStateDatabase"],
  OperatorOps: ["Discover"]
};
// Steps are the state machine each action walks; the last one is terminal.
const OPS_STEPS = {
  Restart: ["Restarting", "Online"],
  Shutdown: ["ShuttingDown", "Offline"],
  Remove: ["DataMigration", "VolumeMigration", "Removed"],
  // a device is taken out of service, not destroyed: it can be added back
  RemoveDevice: ["Detaching", "Removed"],
  // failing one is permanent, and costs a full rebuild of its chunks
  Fail: ["Excluding", "Rebuilding", "Failed"],
  Migrate: ["RestartingNode", "Rebalancing", "RemovingNode", "Migrated"],
  Expand: ["AddingNode", "RebalancingData", "Complete"],
  Discover: ["Inspecting", "Collecting", "Writing"],
  Resize: ["Validating", "Expanding"],
  Default: ["Preparing", "Applying"]
};
// Which steps an abort can unwind. A step absent here refuses DELETE (§3.1).
const OPS_ABORTABLE = {
  Restart: [], Shutdown: ["ShuttingDown"], Migrate: ["RestartingNode"],
  Remove: ["DataMigration"], RemoveDevice: ["Detaching"], Fail: ["Excluding"], Expand: ["AddingNode"], Discover: ["Inspecting", "Collecting"]
};

function runOps(kind, obj) {
  const spec = obj.spec || {};
  const action = spec.action;
  if (!action) return {err: "spec.action is required", reason: "Invalid"};
  if (!(OPS_ACTIONS[kind] || []).includes(action))
    return {err: `${action} is not a valid action for ${kind}. Valid: ${(OPS_ACTIONS[kind] || []).join(", ")}`, reason: "Invalid"};
  const targetKind = window.RESOURCES[kind].ops;
  const targetName = spec.targetRef ? spec.targetRef.name : null;
  if (targetKind && !targetName) return {err: "spec.targetRef.name is required", reason: "Invalid"};
  if (targetKind && activeOps(targetKind, targetName))
    return {err: `${targetKind}/${targetName} already has an operation in flight. Wait for it or abort it first.`, reason: "Conflict"};

  const pre = preflight(kind, action, targetName, spec[action.charAt(0).toLowerCase() + action.slice(1)] || null);
  if (pre) return {err: pre, reason: "Invalid"};

  const steps = OPS_STEPS[kind === "StorageDeviceOps" && action === "Remove" ? "RemoveDevice" : action] || OPS_STEPS.Default;
  const rec = {
    uuid: KU().uuid(), kind, name: (obj.metadata || {}).name || dns(action + "-" + Date.now().toString(36)),
    action, target_kind: targetKind, target_name: targetName, cluster_id: clusterIdOf(targetKind, targetName),
    payload: spec[action.charAt(0).toLowerCase() + action.slice(1)] || null,
    abort: false, phase: "Running", step: steps[0], steps,
    started_ms: Date.now(), created_at: KU().ago(0), message: "",
    events: [{reason: "OperationStarted", message: `${action} started`, at: KU().ago(0)}]
  };
  KDB().ops.push(rec);
  applyEffect(rec);
  return {rec};
}

// Which cluster an operation belongs to, resolved from its target.
function clusterIdOf(targetKind, targetName) {
  const D = KDB();
  if (!targetName) return null;
  if (targetKind === "StorageCluster") { const c = D.clusters.find(x => dns(x.name) === targetName); return c ? c.uuid : null; }
  if (targetKind === "StorageNode") { const n = D.storage_nodes.find(x => dns(x.hostname) === targetName); return n ? n.cluster_id : null; }
  if (targetKind === "StorageDevice") { const d = D.devices.find(x => dns(x.serial_number) === targetName); return d ? d.cluster_id : null; }
  if (targetKind === "StoragePool") { const p = D.pools.find(x => dns(x.pool_name) === targetName); return p ? p.cluster_id : null; }
  return null;
}

// What an action refuses before it starts. Same rules the REST mock enforced.
function preflight(kind, action, targetName, payload) {
  const D = KDB();
  const hostOf = p => p && p.host_id ? D.hosts.find(h => h.uuid === p.host_id) : null;
  if (kind === "StorageClusterOps" && action === "Expand") {
    const c = D.clusters.find(x => dns(x.name) === targetName);
    const h = hostOf(payload);
    if (!c) return "cluster not found";
    if (!h) return "spec.expand.host_id must name a host";
    if (h.storage_node_ids.length >= 2) return "That host already runs two storage nodes.";
    if ((c.zone_ids || []).length && !(c.zone_ids || []).includes(h.zone_id))
      return "That host is not in one of the cluster's zones. Storage nodes can only be added from the zones assigned at cluster creation.";
    if (c.failure_domain_enabled || c.failure_domains_enabled) {
      const fd = payload.failure_domain || h.rack_id || h.zone;
      if (!fd) return "This cluster uses failure domains, so a new node needs a failure domain label. It is fixed for the node's lifetime.";
      // domains hold at least two nodes and stay within one node of each other
      const counts = {};
      D.storage_nodes.filter(n => n.cluster_id === c.uuid).forEach(n => {
        if (n.failure_domain) counts[n.failure_domain] = (counts[n.failure_domain] || 0) + 1; });
      counts[fd] = (counts[fd] || 0) + 1;
      const vals = Object.values(counts);
      const thin = Object.keys(counts).filter(x => counts[x] < 2);
      if (thin.length) return `${thin.join(", ")} would carry a single node. Each failure domain must carry at least two storage nodes, so add them in pairs.`;
      if (Math.max(...vals) - Math.min(...vals) > 1)
        return `That would unbalance the failure domains (${Object.keys(counts).map(x => x + ": " + counts[x]).join(", ")}). Node counts may differ by at most one.`;
    }
  }
  if (kind === "StorageNodeOps" && action === "Migrate") {
    const n = D.storage_nodes.find(x => dns(x.hostname) === targetName);
    const h = hostOf(payload);
    if (!n) return "node not found";
    if (!h) return "spec.migrate.host_id must name a host";
    if (h.uuid === n.host_id) return "The node already runs on that host.";
    if (h.storage_node_ids.length >= 2) return "The target host already runs two storage nodes.";
    if (!h.prepared_at) return "The target host is not prepared. Hugepages and core isolation have to be in place before a node can restart on it.";
  }
  if (kind === "StorageNodeOps" && action === "Remove") {
    const n = D.storage_nodes.find(x => dns(x.hostname) === targetName);
    if (!n) return "node not found";
    const others = D.storage_nodes.filter(x => x.cluster_id === n.cluster_id && x.uuid !== n.uuid && x.status === "online");
    const hosted = D.lvols.filter(v => v.nodes && v.nodes.primary && v.nodes.primary.uuid === n.uuid);
    const pinned = hosted.filter(v => v.affinity && v.affinity.mode === "node" && v.affinity.pinned_node_id === n.uuid);
    if (pinned.length) return `${pinned.length} volume(s) are pinned to this node by affinity. Repin or clear their affinity first.`;
    if (hosted.length && !others.length) return `${hosted.length} volume(s) are primary on this node and there is no other online node to move them to.`;
  }
  return null;
}

// The phase names above, as the UI shows them.
const OPS_STEP_LABEL = {
  DataMigration: "data migration and rebalancing", VolumeMigration: "volume migration", Removed: "removed",
  Detaching: "detaching device", Excluding: "excluding device", Rebuilding: "rebuilding chunks", Failed: "failed",
  Restarting: "in restart", Online: "online", ShuttingDown: "in shutdown", Offline: "offline",
  RestartingNode: "restarting node", Rebalancing: "rebalancing", RemovingNode: "removing node", Migrated: "migrated",
  AddingNode: "adding node", RebalancingData: "rebalancing data", Complete: "complete"
};
// Mirror a running operation onto its node as a phase tracker the tiles read.
function syncNodeOp(o) {
  const D = KDB();
  const n = D.storage_nodes.find(x => dns(x.hostname) === o.target_name);
  if (!n) return;
  const kind = {Remove: "removal", Migrate: "migration", Expand: "expansion"}[o.action];
  if (!kind) return;
  if (["Succeeded", "Failed", "Aborted"].includes(o.phase)) { n.op = null; return; }
  n.op = {kind, phase: OPS_STEP_LABEL[o.step] || o.step, phase_index: Math.max(0, o.steps.indexOf(o.step)),
    phases: o.steps.map(s => OPS_STEP_LABEL[s] || s), started_at: o.created_at,
    volumes_moved: (n.op && n.op.volumes_moved) || 0,
    target_hostname: (n.op && n.op.target_hostname) || (o.payload && o.payload.targetHost) || null,
    source_hostname: (n.op && n.op.source_hostname) || null, task_id: o.uuid};
}

// The fixture-store side effect of an action, applied when the operation starts.
function applyEffect(rec) {
  const D = KDB(), U = KU();
  const node = () => D.storage_nodes.find(n => dns(n.hostname) === rec.target_name);
  const dev = () => D.devices.find(d => dns(d.serial_number) === rec.target_name);
  const clus = () => D.clusters.find(c => dns(c.name) === rec.target_name);
  const pool = () => D.pools.find(p => dns(p.pool_name) === rec.target_name);
  const a = rec.action;
  if (rec.kind === "StorageClusterOps") {
    const c = clus(); if (!c) return;
    if (a === "Suspend") c.status = "suspended";
    if (a === "Activate") c.status = "in_activation";
    if (a === "Expand") {
      // the new node joins in_creation and its devices come up as new; the
      // rebalance phase then moves existing data onto it
      const h = (rec.payload && rec.payload.host_id) ? D.hosts.find(x => x.uuid === rec.payload.host_id)
        : D.hosts.find(x => x.cluster_id === c.uuid && x.storage_node_ids.length < 2);
      if (h && window.SB_ADD_NODE) {
        const n = window.SB_ADD_NODE(c, h);
        if (n) {
          n.status = "in_creation";
          if (rec.payload && rec.payload.failure_domain) n.failure_domain = rec.payload.failure_domain;
          // the tracker hangs off the node, so the op has to point at it
          rec.target_kind = "StorageNode"; rec.target_name = dns(n.hostname);
          syncNodeOp(rec);
          D.devices.filter(d => d.node_id === n.uuid).forEach(d => { d.status = "new"; });
        }
      }
    }
  }
  if (rec.kind === "StorageNodeOps") {
    const n = node(); if (!n) return;
    if (a === "Shutdown") { n.status = "in_shutdown"; n.maintenance = true;
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => { if (d.status !== "removed") d.status = "unavailable"; }); }
    if (a === "Restart") { n.status = "in_restart"; n.maintenance = false;
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => { if (d.status === "unavailable") d.status = "online"; }); }
    if (a === "Remove") {
      n.status = "in_removal";
      // every volume whose primary sits here is moved off first
      const hosted = D.lvols.filter(v => v.nodes && v.nodes.primary && v.nodes.primary.uuid === n.uuid);
      const others = D.storage_nodes.filter(x => x.cluster_id === n.cluster_id && x.uuid !== n.uuid && x.status === "online");
      hosted.forEach((v, i) => { const t = others[i % Math.max(1, others.length)]; if (!t) return;
        v.nodes = Object.assign({}, v.nodes, {primary: {uuid: t.uuid, hostname: t.hostname}}); });
      syncNodeOp(rec); if (n.op) n.op.volumes_moved = hosted.length;
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => { d.status = "unavailable"; });
    }
    if (a === "Migrate") {
      const tgt = (rec.payload && rec.payload.host_id) ? D.hosts.find(x => x.uuid === rec.payload.host_id) : null;
      n.status = "in_migration"; syncNodeOp(rec);
      if (n.op) { n.op.source_hostname = n.hostname; n.op.target_hostname = tgt ? tgt.hostname : null; n.op.target_host_id = tgt ? tgt.uuid : null; }
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => { d.status = "unavailable"; });
    }
  }
  if (rec.kind === "StorageDeviceOps") {
    const d = dev(); if (!d) return;
    if (a === "Restart") { d.status = "in_restart"; }
    if (a === "Remove") { d.status = "in_removal"; }
    if (a === "Fail") { d.status = "in_failure"; }
    if (a === "HealthCheck" && D.runHealthCheck) D.runHealthCheck(d.uuid);
  }
  if (rec.kind === "StoragePoolOps") {
    const p = pool(); if (!p) return;
    if (a === "Enable") p.enabled = true;
    if (a === "Disable") p.enabled = false;
  }
  U.rollup();
}

// How long each step of an operation takes. Long enough that an operator can
// watch the state machine walk through its phases.
const STEP_MS = 5000;

// Operations advance on their own and land in a terminal phase.
function tickOps() {
  (KDB().ops || []).forEach(o => {
    if (["Succeeded", "Failed", "Aborted"].includes(o.phase)) return;
    const age = Date.now() - o.started_ms;
    const idx = Math.min(o.steps.length - 1, Math.floor(age / STEP_MS));
    const next = o.steps[idx];
    if (next !== o.step) {
      o.step = next;
      o.events.push({reason: "StepChanged", message: `Entered ${next}`, at: KU().ago(0)});
      syncNodeOp(o);
    }
    if (o.abort) {
      const canAbort = (OPS_ABORTABLE[o.action] || []).includes(o.step);
      o.phase = "Aborted";
      o.message = canAbort ? `Unwound from ${o.step}` : `Aborted at ${o.step}`;
      o.events.push({reason: "OperationAborted", message: o.message, at: KU().ago(0)});
      finishEffect(o);
      return;
    }
    if (age > o.steps.length * STEP_MS) {
      o.phase = "Succeeded";
      o.message = `${o.action} completed`;
      o.completed_at = KU().ago(0);
      o.events.push({reason: "OperationSucceeded", message: o.message, at: KU().ago(0)});
      finishEffect(o);
    }
  });
}

function finishEffect(o) {
  const D = KDB(), U = KU();
  if (o.phase === "Succeeded" && o.target_kind === "StorageDevice") {
    const d = D.devices.find(x => dns(x.serial_number) === o.target_name);
    if (d) {
      if (o.action === "Restart") { d.status = "online"; d.health_check = d.health_check || "good"; }
      // Removal is reversible: the device is out of service but still known to
      // the cluster and to its node, and can be added back with a restart.
      if (o.action === "Remove") { d.status = "removed"; d.removed_at = U.ago(0); }
      // Failing is not: the device is permanently excluded and its chunks have
      // been rebuilt onto the remaining devices, so fault tolerance is restored
      // without it. It never comes back, and its host device is released.
      if (o.action === "Fail") {
        d.status = "failed"; d.failed_at = U.ago(0); d.health_check = null; d.size_util = 0;
        const h = D.hosts.find(x => x.uuid === d.host_id);
        if (h) h.devices.forEach(hd => { if (hd.serial_number === d.serial_number) hd.assigned_node_id = null; });
      }
    }
  }
  if (o.phase === "Succeeded" && o.target_kind === "StorageNode") {
    const n = D.storage_nodes.find(x => dns(x.hostname) === o.target_name);
    if (n && o.action === "Restart") n.status = "online";
    if (n && o.action === "Shutdown") n.status = "offline";
    if (n && o.action === "Expand") {
      n.status = "online";
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => { if (d.status === "new") d.status = "online"; });
    }
    if (n && o.action === "Migrate") {
      const tgtId = n.op && n.op.target_host_id;
      const old = D.hosts.find(x => x.uuid === n.host_id);
      const tgt = tgtId ? D.hosts.find(x => x.uuid === tgtId) : null;
      if (old) { old.devices.forEach(d => { if (d.assigned_node_id === n.uuid) d.assigned_node_id = null; });
        old.storage_node_ids = old.storage_node_ids.filter(x => x !== n.uuid); }
      if (tgt) {
        n.host_id = tgt.uuid; n.mgmt_ip = tgt.mgmt_ip;
        if (!tgt.storage_node_ids.includes(n.uuid)) tgt.storage_node_ids.push(n.uuid);
        tgt.devices.filter(d => !d.assigned_node_id).forEach(d => d.assigned_node_id = n.uuid);
        D.devices.filter(d => d.node_id === n.uuid).forEach(d => { d.host_id = tgt.uuid; });
      }
      n.status = "online";
      D.devices.filter(d => d.node_id === n.uuid).forEach(d => { if (d.status === "unavailable") d.status = "online"; });
    }
    if (n) n.op = null;
    if (n && o.action === "Remove") {
      const h = D.hosts.find(x => x.uuid === n.host_id);
      if (h) h.devices.forEach(d => { if (d.assigned_node_id === n.uuid) d.assigned_node_id = null; });
      D.devices = D.devices.filter(d => d.node_id !== n.uuid);
      D.storage_nodes = D.storage_nodes.filter(x => x.uuid !== n.uuid);
    }
  }
  if (o.phase === "Succeeded" && o.kind === "StorageClusterOps" && o.action === "Activate") {
    const c = D.clusters.find(x => dns(x.name) === o.target_name);
    if (c) c.status = "online";
  }
  U.rollup();
}

const opsToK8s = o => ({
  apiVersion: KAPI, kind: o.kind,
  metadata: {name: o.name, namespace: KNS(), uid: o.uuid, generation: 1,
    creationTimestamp: o.created_at,
    labels: {[KGROUP + "/target"]: o.target_name || "", [KGROUP + "/action"]: o.action}},
  spec: Object.assign({action: o.action, abort: !!o.abort},
    o.target_name ? {targetRef: {name: o.target_name}} : {},
    o.payload ? {[o.action.charAt(0).toLowerCase() + o.action.slice(1)]: o.payload} : {}),
  status: {
    phase: o.phase, observedGeneration: 1,
    clusterId: o.cluster_id || null,
    // the kind the operation actually landed on: an Expand is filed against the
    // cluster but ends up owning the node it created
    targetKind: o.target_kind || null,
    step: {state: o.step, steps: o.steps, index: Math.max(0, o.steps.indexOf(o.step)),
      labels: o.steps.map(s => OPS_STEP_LABEL[s] || s),
      label: OPS_STEP_LABEL[o.step] || o.step,
      abortable: (OPS_ABORTABLE[o.action] || []).includes(o.step)},
    message: o.message, startedAt: o.created_at, completedAt: o.completed_at || null,
    events: o.events
  }
});

// A little operation history at boot, so the Operations panel is not empty
// before the operator has done anything: two finished machines and one still
// walking its phases.
(function seedOps() {
  const D = KDB(), U = KU();
  const done = (kind, action, targetKind, targetName, clusterId, hoursAgo, phase) => {
    const steps = OPS_STEPS[kind === "StorageDeviceOps" && action === "Remove" ? "RemoveDevice" : action] || OPS_STEPS.Default;
    D.ops.push({uuid: U.uuid(), kind, name: dns(`${targetName}-${action.toLowerCase()}-${U.hex(4)}`),
      action, target_kind: targetKind, target_name: targetName, cluster_id: clusterId, payload: null,
      abort: phase === "Aborted", phase, step: phase === "Succeeded" ? steps[steps.length - 1] : steps[0], steps,
      started_ms: Date.now() - hoursAgo * 3600e3 - steps.length * STEP_MS,
      created_at: U.ago(hoursAgo), completed_at: U.ago(hoursAgo - .05),
      message: phase === "Succeeded" ? `${action} completed` : `Unwound from ${steps[0]}`,
      events: [{reason: "OperationStarted", message: `${action} started`, at: U.ago(hoursAgo)},
        {reason: phase === "Succeeded" ? "OperationSucceeded" : "OperationAborted",
          message: phase === "Succeeded" ? `${action} completed` : `Unwound from ${steps[0]}`, at: U.ago(hoursAgo - .05)}]});
  };
  D.clusters.filter(c => c.status === "online" || c.status === "degraded").slice(0, 3).forEach((c, i) => {
    const ns = D.storage_nodes.filter(n => n.cluster_id === c.uuid);
    if (!ns.length) return;
    done("StorageNodeOps", "Restart", "StorageNode", dns(ns[0].hostname), c.uuid, 6 + i * 3, "Succeeded");
    if (ns[1]) done("StorageNodeOps", "Shutdown", "StorageNode", dns(ns[1].hostname), c.uuid, 30 + i * 5, "Succeeded");
    if (i === 0) done("StorageClusterOps", "Expand", "StorageCluster", dns(c.name), c.uuid, 52, "Succeeded");
    if (i === 1 && ns[2]) done("StorageNodeOps", "Migrate", "StorageNode", dns(ns[2].hostname), c.uuid, 12, "Aborted");
  });
  // and one machine still in flight: an expansion of the first eligible cluster
  const c0 = D.clusters.find(c => (c.status === "online" || c.status === "degraded")
    && D.hosts.some(h => h.cluster_id === c.uuid && h.storage_node_ids.length < 2 && h.prepared_at));
  if (c0) {
    const h = D.hosts.find(h2 => h2.cluster_id === c0.uuid && h2.storage_node_ids.length < 2 && h2.prepared_at);
    const r = runOps("StorageClusterOps", {apiVersion: KAPI, kind: "StorageClusterOps",
      metadata: {name: dns(`${c0.name}-expand-boot`), namespace: KNS()},
      spec: {action: "Expand", targetRef: {name: dns(c0.name)},
        expand: {host_id: h.uuid, failure_domain: h.rack_id || h.zone || null}}});
    // pretend it started a moment ago, so it is mid-machine rather than brand new
    if (r && r.rec) r.rec.started_ms = Date.now() - STEP_MS * 0.6;
  }
})();

Object.assign(window, {TO_K8S, COLL, opsToK8s, runOps, tickOps, dnsName: dns, syncNodeOp, OPS_STEP_LABEL, STEP_MS,
  OPS_ACTIONS, OPS_STEPS, OPS_ABORTABLE, pascalCase: pascal});

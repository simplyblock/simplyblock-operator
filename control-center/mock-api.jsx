// ---------------------------------------------------------------------------
// MOCK API — intercepts fetch() for the control plane API v2 surface and
// answers from mock-backend.jsx. Remove this <script> tag (or set
// SB_CONFIG.mock = false) and the UI talks to the real control plane.
// ---------------------------------------------------------------------------
const SB_CONFIG = window.SB_CONFIG = Object.assign({apiBase: "/api/v2", token: "mock-token", mock: true}, window.SB_CONFIG);

const SB_MOCK = window.SB_MOCK = {
  latency: [140, 380],   // simulated round-trip range, ms
  failRate: 0,           // 0..1 — random 503s
  forceEmpty: false,     // return empty collections (empty-state design check)
  offline: false,        // every request fails (error-state design check)
  failNext: false,
  requests: 0
};

const wait = ms => new Promise(r => setTimeout(r, ms));
const ok = body => new Response(JSON.stringify(Object.assign({status: true}, body)), {status: 200, headers: {"Content-Type": "application/json"}});
const fail = (code, msg) => new Response(JSON.stringify({status: false, error: msg}), {status: code, headers: {"Content-Type": "application/json"}});

// Why a protected group refuses membership changes.
const cgLocked = g => `${g.name} carries ${[g.backup_policy && "a backup policy", g.replication_config && "a replication cadence"].filter(Boolean).join(" and ")}, so its membership is fixed — the retained versions and the replica stream are all defined against exactly this set of volumes. Detach ${g.backup_policy && g.replication_config ? "them" : "it"} to change the members, or create a second consistency group with the volumes you want: a volume can belong to more than one group.`;

const D = () => window.SB_DB;
const U = () => window.SB_UTIL;
const byId = (coll, id) => D()[coll].find(x => x.uuid === id);
const where = (coll, k, v) => D()[coll].filter(x => x[k] === v);
const one = (coll, id) => { const r = byId(coll, id); return r ? {results: [r]} : {__404: true}; };
const list = (pColl, pId, coll, fk) => byId(pColl, pId) ? {results: SB_MOCK.forceEmpty ? [] : where(coll, fk, pId)} : {__404: true};
const drop = (coll, id) => { const i = D()[coll].findIndex(x => x.uuid === id); if (i < 0) return {__404: true}; D()[coll].splice(i, 1); return {results: []}; };
const mut = (coll, id, f) => { const r = byId(coll, id); if (!r) return {__404: true}; f(r); U().rollup(); return {results: [r]}; };

const GET_ROUTES = [
  [/^\/clusters$/, () => ({results: D().clusters})],
  [/^\/clusters\/([\w-]+)$/, m => one("clusters", m[1])],
  [/^\/clusters\/([\w-]+)\/hosts$/, m => list("clusters", m[1], "hosts", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/storage-nodes$/, m => list("clusters", m[1], "storage_nodes", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/pools$/, m => list("clusters", m[1], "pools", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/lvols$/, m => list("clusters", m[1], "lvols", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/snapshots$/, m => list("clusters", m[1], "snapshots", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/backups$/, m => list("clusters", m[1], "backups", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/backup-policies$/, m => list("clusters", m[1], "backup_policies", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/consistency-groups$/, m => list("clusters", m[1], "consistency_groups", "cluster_id")],
  [/^\/clusters\/([\w-]+)\/migrations$/, m => byId("clusters", m[1])
    ? {results: SB_MOCK.forceEmpty ? [] : D().migrations.filter(x => x.source_cluster_id === m[1])} : {__404: true}],
  [/^\/migrations$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().migrations})],
  [/^\/migrations\/([\w-]+)$/, m => one("migrations", m[1])],
  [/^\/migrations\/([\w-]+)\/lvols$/, m => {
    const g = byId("migrations", m[1]);
    if (!g) return {__404: true};
    return {results: SB_MOCK.forceEmpty ? [] : D().lvols.filter(v => g.lvol_ids.includes(v.uuid))};
  }],
  [/^\/consistency-groups\/([\w-]+)$/, m => one("consistency_groups", m[1])],
  [/^\/consistency-groups\/([\w-]+)\/lvols$/, m => {
    const g = byId("consistency_groups", m[1]);
    if (!g) return {__404: true};
    return {results: SB_MOCK.forceEmpty ? [] : D().lvols.filter(v => g.lvol_ids.includes(v.uuid))};
  }],
  [/^\/consistency-groups\/([\w-]+)\/snapshots$/, m => list("consistency_groups", m[1], "cg_snapshots", "cg_id")],
  [/^\/cg-snapshots\/([\w-]+)$/, m => one("cg_snapshots", m[1])],
  [/^\/clusters\/([\w-]+)\/replication-policies$/, m => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    return {results: SB_MOCK.forceEmpty ? [] : D().dr_policies.filter(p => p.source_cluster_id === m[1])};
  }],
  [/^\/clusters\/([\w-]+)\/zones$/, m => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    return {results: D().zones.filter(s => (c.zone_ids || []).includes(s.uuid))};
  }],
  [/^\/dr-clusters$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().dr_clusters})],
  [/^\/dr-clusters\/([\w-]+)$/, m => one("dr_clusters", m[1])],
  [/^\/dr-clusters\/([\w-]+)\/protected-apps$/, m => {
    const dc = byId("dr_clusters", m[1]);
    if (!dc) return {__404: true};
    return {results: D().protected_apps.filter(a => a.preferred_cluster_id === dc.uuid || a.failover_cluster_id === dc.uuid)};
  }],
  [/^\/replication-policies\/([\w-]+)\/protected-apps$/, m => list("dr_policies", m[1], "protected_apps", "policy_id")],
  [/^\/protected-apps$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().protected_apps})],
  [/^\/protected-apps\/([\w-]+)$/, m => one("protected_apps", m[1])],
  [/^\/protected-apps\/([\w-]+)\/pvcs$/, m => {
    const a = byId("protected_apps", m[1]);
    if (!a) return {__404: true};
    return {results: SB_MOCK.forceEmpty ? [] : D().pvcs.filter(p => a.pvc_ids.includes(p.uuid))};
  }],
  [/^\/kubernetes-clusters$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().k8s_clusters})],
  [/^\/kubernetes-clusters\/([\w-]+)$/, m => one("k8s_clusters", m[1])],
  [/^\/kubernetes-clusters\/([\w-]+)\/storage-classes$/, m => list("k8s_clusters", m[1], "storage_classes", "k8s_cluster_id")],
  [/^\/kubernetes-clusters\/([\w-]+)\/pvcs$/, m => list("k8s_clusters", m[1], "pvcs", "k8s_cluster_id")],
  [/^\/kubernetes-clusters\/([\w-]+)\/hosts$/, m => list("k8s_clusters", m[1], "hosts", "k8s_cluster_id")],
  [/^\/kubernetes-clusters\/([\w-]+)\/storage-clusters$/, m => {
    const kc = byId("k8s_clusters", m[1]);
    if (!kc) return {__404: true};
    return {results: D().clusters.filter(c => (kc.storage_cluster_ids || []).includes(c.uuid))};
  }],
  [/^\/kubernetes-clusters\/([\w-]+)\/zones$/, m => {
    const kc = byId("k8s_clusters", m[1]);
    if (!kc) return {__404: true};
    return {results: D().zones.filter(s => (kc.zone_ids || []).includes(s.uuid))};
  }],
  [/^\/clusters\/([\w-]+)\/kubernetes-clusters$/, m => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    return {results: D().k8s_clusters.filter(k => (c.k8s_cluster_ids || []).includes(k.uuid))};
  }],
  [/^\/storage-classes\/([\w-]+)$/, m => one("storage_classes", m[1])],
  [/^\/storage-classes\/([\w-]+)\/pvcs$/, m => list("storage_classes", m[1], "pvcs", "storage_class_id")],
  [/^\/pvcs$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().pvcs})],
  [/^\/clusters\/([\w-]+)\/buckets$/, m => list("clusters", m[1], "buckets", "cluster_id")],
  [/^\/buckets\/([\w-]+)$/, m => one("buckets", m[1])],
  [/^\/pvcs\/([\w-]+)$/, m => one("pvcs", m[1])],
  [/^\/zones\/([\w-]+)\/kubernetes-clusters$/, m => {
    const s = byId("zones", m[1]);
    if (!s) return {__404: true};
    return {results: D().k8s_clusters.filter(k => (s.k8s_cluster_ids || []).includes(k.uuid))};
  }],
  [/^\/zones$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().zones})],
  [/^\/zones\/([\w-]+)$/, m => one("zones", m[1])],
  [/^\/zones\/([\w-]+)\/hosts$/, m => list("zones", m[1], "hosts", "zone_id")],
  [/^\/zones\/([\w-]+)\/clusters$/, m => {
    const s = byId("zones", m[1]);
    if (!s) return {__404: true};
    return {results: D().clusters.filter(c => (s.cluster_ids || []).includes(c.uuid))};
  }],
  [/^\/cluster-pairs$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().cluster_pairs})],
  [/^\/cluster-pairs\/([\w-]+)$/, m => one("cluster_pairs", m[1])],
  [/^\/cluster-pairs\/([\w-]+)\/replication-policies$/, m => {
    const p = byId("cluster_pairs", m[1]);
    if (!p) return {__404: true};
    return {results: SB_MOCK.forceEmpty ? [] : D().dr_policies.filter(x => x.pair_id === m[1])};
  }],
  [/^\/replication-policies$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().dr_policies})],
  [/^\/replication-policies\/([\w-]+)$/, m => one("dr_policies", m[1])],
  [/^\/replication-policies\/([\w-]+)\/lvols$/, m => {
    const p = byId("dr_policies", m[1]);
    if (!p) return {__404: true};
    return {results: SB_MOCK.forceEmpty ? [] : D().lvols.filter(v => p.lvol_ids.includes(v.uuid))};
  }],
  [/^\/hosts\/unassigned$/, () => ({results: SB_MOCK.forceEmpty ? [] : D().hosts.filter(h => !h.cluster_id)})],
  [/^\/hosts\/([\w-]+)$/, m => one("hosts", m[1])],
  [/^\/storage-nodes\/([\w-]+)$/, m => one("storage_nodes", m[1])],
  [/^\/storage-nodes\/([\w-]+)\/devices$/, m => list("storage_nodes", m[1], "devices", "node_id")],
  [/^\/devices\/([\w-]+)$/, m => one("devices", m[1])],
  [/^\/pools\/([\w-]+)$/, m => one("pools", m[1])],
  [/^\/pools\/([\w-]+)\/lvols$/, m => list("pools", m[1], "lvols", "pool_id")],
  [/^\/pools\/([\w-]+)\/storage-classes$/, m => list("pools", m[1], "storage_classes", "pool_id")],
  [/^\/pools\/([\w-]+)\/snapshots$/, m => list("pools", m[1], "snapshots", "pool_id")],
  [/^\/pools\/([\w-]+)\/backups$/, m => list("pools", m[1], "backups", "pool_id")],
  [/^\/lvols\/([\w-]+)$/, m => one("lvols", m[1])],
  [/^\/lvols\/([\w-]+)\/snapshots$/, m => list("lvols", m[1], "snapshots", "lvol_id")],
  [/^\/lvols\/([\w-]+)\/backups$/, m => list("lvols", m[1], "backups", "lvol_id")],
  [/^\/snapshots\/([\w-]+)$/, m => one("snapshots", m[1])],
  [/^\/backups\/([\w-]+)$/, m => one("backups", m[1])],
  [/^\/backup-policies\/([\w-]+)$/, m => one("backup_policies", m[1])]
];

// Start a phased node operation. The phase list and the subtask names come from
// SB_UTIL.NODE_OPS so the tracker, the tasks and the tick cannot drift apart.
function startNodeOp(n, kind, extra) {
  const spec = U().NODE_OPS[kind];
  n.status = spec.status;
  n.op = Object.assign({kind, phase: spec.phases[0], phase_index: 0, phases: spec.phases,
    started_at: U().ago(0), started_ms: Date.now(), volumes_moved: 0, task_id: null}, extra || {});
  if (D().tasks) {
    const master = {uuid: U().uuid(), cluster_id: n.cluster_id, parent_id: null,
      function_name: "node_" + kind, target_id: `NodeID:${n.uuid}`, node_id: n.uuid, distrib: null,
      retry: 0, max_retry: 3, status: "running", result: "running",
      created_at: U().ago(0), updated_at: U().ago(0), canceled: false, subtask_total: spec.subtasks.length};
    D().tasks.unshift(master);
    spec.subtasks.forEach((s, i) => D().tasks.push({uuid: U().uuid(), cluster_id: n.cluster_id, parent_id: master.uuid,
      function_name: s, target_id: null, node_id: n.uuid, distrib: null, retry: 0, max_retry: 0,
      status: i === 0 ? "running" : "new", result: "", created_at: U().ago(0), updated_at: U().ago(0),
      canceled: false, subtask_total: 0}));
    n.op.task_id = master.uuid;
  }
  return n;
}

// Every logical-volume move — manual, rebalance or affinity-driven — files an
// lvol_migration task. That task is how the move is followed, and the moved
// counters on the cluster are derived from these records.
function lvolMigrationTask(v, target, reason) {
  const t = {uuid: U().uuid(), cluster_id: v.cluster_id, parent_id: null,
    function_name: "lvol_migration", target_id: `LvolID:${v.uuid}`,
    node_id: target ? target.uuid : null, distrib: null, retry: 0, max_retry: 3,
    status: "done", result: `Volume ${v.lvol_name} moved to ${target ? target.hostname : "another node"}${reason ? " (" + reason + ")" : ""}`,
    created_at: U().ago(0), updated_at: U().ago(0), canceled: false, subtask_total: 0};
  if (D().tasks) D().tasks.unshift(t);
  return t;
}

// ---- mutations -------------------------------------------------------------
function addStorageNode(cluster, host) {
  const n = {
    uuid: U().uuid(), cluster_id: cluster.uuid, host_id: host.uuid,
    hostname: `${cluster.name.split("-").slice(0, 2).join("-")}-stor-${String(D().storage_nodes.filter(x => x.cluster_id === cluster.uuid).length + 1).padStart(2, "0")}`,
    data_nics: Array.from({length: cluster.multipathing_enabled ? 2 : 1}, (_, k) => ({
      name: (host.data_nics && host.data_nics[k]) || (k === 0 ? "ens1f0" : "ens1f1"),
      ip: `10.${U().int(10, 60)}.${U().int(0, 40)}.${U().int(2, 250)}`, port: 4420 + k,
      numa_socket: k, state: "up"
    })),
    mgmt_ip: host.mgmt_ip, failure_domain: host.labels["topology.kubernetes.io/zone"], physical_label: host.hostname,
    status: "in_restart", cpu_count: Math.round(host.vcpu_count / 2), vcpu_reserved: 15,
    max_subsystem_count: 100,
    memory_total: host.memory_total / 2,
    memory_reserved: Math.round(host.memory_total / 2 * .6),
    memory_used: 0,
    hugepages_total: host.hugepages_reserved, hugepages_used: 0, spdk_version: "v24.09"
  };
  D().storage_nodes.push(n);
  host.devices.filter(d => !d.assigned_node_id && (cluster.device_class === "nvme" ? d.kind === "nvme" : true))
    .forEach(hd => {
      hd.assigned_node_id = n.uuid;
      D().devices.push({uuid: U().uuid(), node_id: n.uuid, cluster_id: cluster.uuid, host_id: host.uuid,
        cluster_device_class: cluster.device_class, numa_socket: hd.numa_socket,
        serial_number: hd.serial_number || "PENDING", pcie_address: hd.pcie_address, device_name: hd.device_name,
        model_number: hd.model_number, firmware_revision: "GXA7711", status: "new", health_check: null,
        size_total: hd.size, size_util: 0, temperature_c: 34, percentage_used: 0, power_on_hours: 0,
        io_stats: U().ioStats(0, 0), io_history: {iops: U().series(0, 0), bytes: U().series(0, 0)}});
    });
  return n;
}

function normQos(b) {
  const n = k => (b[k] === "" || b[k] === undefined || b[k] === null) ? 0 : Number(b[k]);
  const q = {rw_ios_per_sec: n("rw_ios_per_sec"), rw_mbytes_per_sec: n("rw_mbytes_per_sec"),
    r_mbytes_per_sec: n("r_mbytes_per_sec"), w_mbytes_per_sec: n("w_mbytes_per_sec")};
  return Object.values(q).some(v => v > 0) ? q : null;
}

// Two rules govern failure domains, and both have to hold after the operation:
//   * every domain in use carries at least two nodes — a domain with one node
//     cannot lose it without losing the whole domain's share of the data
//   * node counts across domains differ by at most one
function fdBalanceCheck(c, host, op, node) {
  if (!c || !c.failure_domain_enabled) return null;
  const scope = c.failure_domain_scope;
  const fdOf = h => {
    if (!h) return null;
    if (scope === "zone") { const st = D().zones.find(x => x.uuid === h.zone_id); return st ? st.name : null; }
    return scope === "cabinet" ? h.cabinet_id : h.rack_id;
  };
  const target = op === "remove" ? (node ? node.failure_domain : null) : fdOf(host);
  if (!target) return `The host carries no ${scope} taint, so the node cannot be placed in a failure domain.`;
  const ns = D().storage_nodes.filter(n => n.cluster_id === c.uuid);
  const counts = {};
  (c.failure_domains || []).forEach(f => { counts[f.name] = 0; });
  ns.forEach(n => { const k = n.failure_domain || "unassigned"; counts[k] = (counts[k] || 0) + 1; });
  if (counts[target] === undefined) counts[target] = 0;
  counts[target] += op === "remove" ? -1 : 1;
  const used = Object.entries(counts).filter(([k, v]) => k !== "unassigned" && v > 0);
  const list = () => Object.entries(counts).filter(([k]) => k !== "unassigned")
    .map(([k, v]) => `${k}: ${v}`).join(", ");
  // a domain must hold at least two nodes to be a failure domain at all
  const thin = used.filter(([, v]) => v < 2);
  if (thin.length) {
    return op === "remove"
      ? `That would leave ${thin.map(([k, v]) => `${k}` + (v ? " with one node" : "")).join(", ")} (${list()}). Each ${scope} must carry at least two storage nodes — remove the domain's other node too, or add a node there first.`
      : `${thin.map(([k]) => k).join(", ")} would carry a single node (${list()}). Each ${scope} must carry at least two storage nodes, so add them in pairs.`;
  }
  const vals = used.map(([, v]) => v);
  if (vals.length < 2) return `A cluster with failure domains needs at least two ${scope}s, each with at least two storage nodes.`;
  const spread = Math.max(...vals) - Math.min(...vals);
  if (spread > 1) return `That would unbalance the failure domains (${list()}). Node counts per ${scope} may differ by at most one.`;
  return null;
}

function appendVersion(snap, bucket) {
  const D2 = D(), U2 = U();
  const v = D2.lvols.find(x => x.uuid === snap.lvol_id);
  if (!v) return {__err: "Source volume no longer exists"};
  let bk = D2.backups.find(x => x.lvol_id === snap.lvol_id);
  if (!bk) {
    bk = {uuid: U2.uuid(), cluster_id: v.cluster_id, pool_id: v.pool_id, pool_name: v.pool_name,
      lvol_id: v.uuid, lvol_name: v.lvol_name, chain_id: `bk-${U2.hex(6)}`,
      policy_id: v.backup_policy ? v.backup_policy.uuid : null,
      policy_name: v.backup_policy ? v.backup_policy.policy_name : null,
      bucket: bucket || `s3://sb-backup-eu/${v.lvol_name}/`,
      status: "online", created_at: U2.ago(0), last_merge_at: null, versions: []};
    D2.backups.push(bk);
  }
  const seq = bk.versions.length + 1;
  const ver = {id: `v${String(seq).padStart(4, "0")}`, seq,
    tier: v.backup_policy ? "policy" : "manual",
    type: seq === 1 ? "full" : "delta",
    created_at: U2.ago(0),
    size: Math.round(seq === 1 ? v.size_util * .9 : snap.size),
    source_snapshot_id: snap.uuid, source_snapshot_name: snap.snapshot_name, merged_count: 0};
  bk.versions.push(ver);
  snap.backup_version_id = ver.id;
  return bk;
}

const MUT_ROUTES = [
  ["POST", /^\/clusters$/, (m, b) => {
    if (!b.name) return {__err: "A cluster label is required"};
    const hosts = (b.host_ids || []).map(id => byId("hosts", id)).filter(Boolean);
    if (!hosts.length) return {__err: "Select at least one prepared host"};
    const edge = (b.location_type || "datacenter") === "edge";
    const deviceClass = edge ? "block" : (b.device_class || "nvme");
    if (edge && b.device_class === "nvme") return {__err: "The NVMe device class is not available for edge clusters"};
    const c = {
      uuid: U().uuid(), name: b.name,
      device_class: deviceClass,
      status: "in_activation", location_type: edge ? "edge" : "datacenter",
      rebalancing: false,
      capabilities: {snapshot_replication: true, async_replication: true,
        rebalancing: !edge, tasks: !edge},
      multipathing_enabled: !!b.multipathing_enabled,
      node_affinity: b.node_affinity || "soft",
      pod_affinity_enabled: !!b.pod_affinity_enabled,
      auto_rebalance: {enabled: !edge},
      failure_domain_enabled: !!b.failure_domain_enabled,
      failure_domain_scope: b.failure_domain_enabled ? (b.failure_domain_scope || "rack") : null,
      sync_replication_enabled: !!b.sync_replication_enabled,
      backup_enabled: !!b.backup_enabled,
      s3: b.backup_enabled ? {endpoint: b.s3_endpoint, region: b.s3_region, bucket: b.s3_bucket,
        path_prefix: b.s3_path_prefix || "", access_key_id: b.s3_access_key_id,
        secret_access_key: "••••••••••••••••••••", addressing: b.s3_addressing || "virtual-hosted",
        verify_tls: b.s3_verify_tls !== false} : null,
      kms: null,
      ha_type: "ha", distr_npcs: Number(b.distr_npcs || 1), distr_ndcs: Number(b.distr_ndcs || 2),
      blk_size: 4096, page_size_in_blocks: 2097152,
      cluster_version: "26.2.1",  // the operator release, not a per-cluster choice
      mgmt_endpoint: edge ? `https://k8s-api.${b.name}.local:6443` : (b.mgmt_endpoint || `https://${b.name}.simplyblock.internal:5000`),
      mgmt_endpoint_kind: "Kubernetes API",
      created_at: U().ago(0)
    };
    const zoneIds = [...new Set((b.zone_ids && b.zone_ids.length ? b.zone_ids : hosts.map(h => h.zone_id)).filter(Boolean))];
    c.zone_ids = zoneIds;
    c.stretched = zoneIds.length > 1;
    zoneIds.forEach(id => { const st = byId("zones", id); if (st) st.cluster_ids.push(c.uuid); });
    D().clusters.push(c);
    hosts.forEach(h => { h.cluster_id = c.uuid; addStorageNode(c, h); });
    D().pools.push({uuid: U().uuid(), cluster_id: c.uuid, pool_name: "default", enabled: true, qos: null});
    U().rollup();
    return {results: [c]};
  }],
  ["PUT", /^\/lvols\/([\w-]+)\/qos$/, (m, b) => mut("lvols", m[1], v => { v.qos = normQos(b); })],
  // ---- file storage (pNFS) ------------------------------------------------
  ["PUT", /^\/clusters\/([\w-]+)\/file-storage$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    if (b.enabled) {
      const cand = D().hosts.filter(h => h.cluster_id === c.uuid && h.control_plane && h.status === "available");
      if (!cand.length) return {__err: "No control-plane worker available to run the NFS metadata server."};
      const mds = byId("hosts", b.mds_host_id) || cand[0];
      c.file_storage = Object.assign({}, c.file_storage, {
        enabled: true, nfs_version: "4.2", layout_type: b.layout_type || "flexfile",
        export_root: b.export_root || "/export/simplyblock",
        mds_host: {uuid: mds.uuid, hostname: mds.hostname},
        mds_candidates: cand.filter(h => h.uuid !== mds.uuid).slice(0, 3).map(h => ({uuid: h.uuid, hostname: h.hostname})),
        mds_state: "active",
        lease_seconds: Number(b.lease_seconds || 20),
        grace_seconds: Number(b.grace_seconds || 45),
        failover_budget_seconds: Number(b.failover_budget_seconds || 8),
        filesystem: "xfs", max_exports: Number(b.max_exports || 128)});
    } else {
      const inUse = D().pvcs.filter(p => p.access_mode === "ReadWriteMany"
        && D().lvols.some(v => v.uuid === p.lvol_id && v.cluster_id === c.uuid)).length;
      if (inUse) return {__err: inUse + " RWX claim(s) still use pNFS. Delete them before disabling file storage."};
      c.file_storage = {enabled: false};
    }
    U().rollup(); return {results: [c]};
  }],
  // The metadata server holds no local state, so it restarts on another
  // control-plane worker within the failover budget; data paths keep serving.
  ["POST", /^\/clusters\/([\w-]+)\/file-storage\/failover$/, m => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    if (!c.file_storage.enabled) return {__err: "File storage is not enabled on this cluster."};
    const cand = c.file_storage.mds_candidates || [];
    if (!cand.length) return {__err: "No standby control-plane worker to move the metadata server to."};
    const prev = c.file_storage.mds_host;
    c.file_storage.mds_host = cand[0];
    c.file_storage.mds_candidates = cand.slice(1).concat(prev ? [prev] : []);
    c.file_storage.mds_state = "restarting";
    c.file_storage.mds_restarted_at = U().ago(0);
    c.file_storage.mds_restart_ms = Date.now();
    return {results: [c]};
  }],
  ["PUT", /^\/clusters\/([\w-]+)\/object-storage$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    if (!b.enabled) {
      const n = (D().buckets || []).filter(x => x.cluster_id === c.uuid).length;
      if (n) return {__err: n + " bucket(s) still exist. Delete them before disabling object storage."};
      c.object_storage = {enabled: false};
    } else {
      c.object_storage = Object.assign({}, c.object_storage, {
        enabled: true, endpoint: b.endpoint, region: b.region,
        addressing: b.addressing || "virtual-hosted", metadata_backend: "foundationdb",
        versioning_default: !!b.versioning_default, max_buckets: Number(b.max_buckets || 500)});
    }
    U().rollup(); return {results: [c]};
  }],
  // Compression-dedup is one per-volume switch and only takes effect for data
  // written after the change; existing blocks keep their current form.
  ["PUT", /^\/lvols\/([\w-]+)\/data-reduction$/, (m, b) => mut("lvols", m[1], v => {
    v.compression_dedup_enabled = !!b.compression_dedup_enabled;
    v.logical_used = v.compression_dedup_enabled ? Math.round(v.size_util * 1.9) : v.size_util;
  })],
  ["POST", /^\/cluster-pairs$/, (m, b) => {
    const src = byId("clusters", b.source_cluster_id), tgt = byId("clusters", b.target_cluster_id);
    if (!src || !tgt) return {__404: true};
    if (src.uuid === tgt.uuid) return {__err: "A cluster cannot be paired with itself"};
    if (!tgt.dr_target_eligible) return {__err: `${tgt.name} is not qualified as a DR target`};
    if (D().cluster_pairs.some(p => p.source_cluster_id === src.uuid && p.target_cluster_id === tgt.uuid))
      return {__err: "This directional pair already exists"};
    const p = {uuid: U().uuid(), source_cluster_id: src.uuid, target_cluster_id: tgt.uuid, state: "pairing",
      link: {endpoint: b.endpoint || `nvmf://${tgt.name}.simplyblock.remote:4420`,
        nqn: `nqn.2023-02.io.simplyblock:repl:${U().hex(8)}`,
        rtt_ms: +(2 + Math.random() * 30).toFixed(1),
        bandwidth_mbit: Number(b.bandwidth_mbit || 10000), throughput_bytes_ps: 0},
      last_handshake_at: U().ago(0), created_at: U().ago(0), policies_count: 0};
    D().cluster_pairs.push(p); U().rollup(); return {results: [p]};
  }],
  ["POST", /^\/cluster-pairs\/([\w-]+)\/test$/, m => mut("cluster_pairs", m[1], p => {
    p.last_handshake_at = U().ago(0);
    if (p.state === "pairing") p.state = "paired";
  })],
  ["DELETE", /^\/cluster-pairs\/([\w-]+)$/, m => {
    const p = byId("cluster_pairs", m[1]);
    if (!p) return {__404: true};
    if (D().dr_policies.some(x => x.pair_id === p.uuid))
      return {__err: "This pair still carries replication policies. Delete them first."};
    const r = drop("cluster_pairs", m[1]); U().rollup(); return r;
  }],
  ["POST", /^\/replication-policies$/, (m, b) => {
    const sync = b.mode === "synchronous";
    const retention = (b.retention || []).filter(r => r.interval && Number(r.keep) > 0)
      .map(r => ({interval: r.interval, keep: Number(r.keep)}));
    let src, pair = null, zoneIds = null, cg = null;
    if (sync) {
      src = byId("clusters", b.source_cluster_id);
      if (!src) return {__404: true};
      if (!src.sync_replication_enabled)
        return {__err: "Synchronous replication is not enabled on this cluster. The flag is set at creation time and cannot be changed."};
      zoneIds = (b.zone_ids || []).slice();
      if (zoneIds.length < 2) return {__err: "Synchronous replication needs at least two zones"};
      if (zoneIds.some(id => !(src.zone_ids || []).includes(id)))
        return {__err: "Every zone must be assigned to the cluster first"};
    } else {
      pair = byId("cluster_pairs", b.pair_id);
      if (!pair) return {__404: true};
      if (pair.state === "unreachable") return {__err: "The cluster pair link is down"};
      src = byId("clusters", pair.source_cluster_id);
      // The cadence is not a property of the policy: an asynchronous policy names
      // a consistency group, and the group owns the frequency and the retention.
      if (!b.cg_id) return {__err: "An asynchronous DR policy has to name a consistency group — the group owns the replication frequency and retention."};
      cg = byId("consistency_groups", b.cg_id);
      if (!cg) return {__err: "No such consistency group"};
      if (cg.cluster_id !== src.uuid) return {__err: "That group belongs to a different cluster than the pair's source."};
      if (!cg.replication_config) return {__err: `${cg.name} has no replication cadence. Attach replication to the group first.`};
    }
    const pol = {uuid: U().uuid(), name: b.name, mode: sync ? "synchronous" : "asynchronous",
      pair_id: pair ? pair.uuid : null,
      source_cluster_id: src.uuid, target_cluster_id: pair ? pair.target_cluster_id : null,
      zone_ids: zoneIds,
      cg_id: cg ? cg.uuid : null, cg_name: cg ? cg.name : null,
      frequency_minutes: cg ? cg.replication_config.frequency_minutes : 0,
      retention: cg ? cg.replication_config.retention : [],
      failback: {mode: b.failback_mode || "manual",
        frequency_minutes: Number(b.failback_frequency_minutes || b.frequency_minutes || 0),
        reverse_on_failover: b.reverse_on_failover !== false, resync_full: !!b.resync_full},
      state: "healthy", last_replication_at: U().ago(0), backlog_bytes: 0,
      generations_kept: (cg ? cg.replication_config.retention : []).reduce((a, r) => a + r.keep, 0),
      created_at: U().ago(0), last_failover_at: null, last_test_at: null, lvol_ids: []};
    D().dr_policies.push(pol); U().rollup(); return {results: [pol]};
  }],
  ["PUT", /^\/replication-policies\/([\w-]+)$/, (m, b) => mut("dr_policies", m[1], p => {
    // Frequency and retention are NOT settings of the policy: they belong to the
    // consistency group it names. Only the group reference is editable here.
    if (b.cg_id !== undefined && p.mode !== "synchronous") {
      if (!b.cg_id) return {__err: "An asynchronous DR policy has to name a consistency group."};
      const g = byId("consistency_groups", b.cg_id);
      if (!g) return {__err: "No such consistency group"};
      if (g.cluster_id !== p.source_cluster_id) return {__err: "That group belongs to a different cluster than the policy's source."};
      if (!g.replication_config) return {__err: `${g.name} has no replication cadence. Attach replication to the group first — the group owns the frequency and retention.`};
      p.cg_id = g.uuid; p.cg_name = g.name;
    }
    if (b.failback_mode) p.failback.mode = b.failback_mode;
    if (b.failback_frequency_minutes !== undefined) p.failback.frequency_minutes = Number(b.failback_frequency_minutes);
    if (b.resync_full !== undefined) p.failback.resync_full = !!b.resync_full;
    p.generations_kept = p.retention.reduce((a, r) => a + r.keep, 0);
    p.lvol_ids.forEach(id => { const v = byId("lvols", id); if (v && v.replication) v.replication.generations = p.generations_kept; });
  })],
  ["POST", /^\/replication-policies\/([\w-]+)\/lvols$/, (m, b) => mut("dr_policies", m[1], p => {
    (b.lvol_ids || []).forEach(id => {
      const v = byId("lvols", id);
      if (!v || p.lvol_ids.includes(id) || v.replication) return;
      p.lvol_ids.push(id);
      v.replication = {policy_id: p.uuid, policy_name: p.name, mode: p.mode, status: "healthy",
        last_replication_at: U().ago(0), backlog_bytes: 0,
        consistency_group: p.consistency_group ? `cg-${p.name}` : null, generations: p.generations_kept};
    });
  })],
  ["DELETE", /^\/replication-policies\/([\w-]+)\/lvols\/([\w-]+)$/, m => mut("dr_policies", m[1], p => {
    p.lvol_ids = p.lvol_ids.filter(x => x !== m[2]);
    const v = byId("lvols", m[2]); if (v) v.replication = null;
  })],
  ["POST", /^\/replication-policies\/([\w-]+)\/failover$/, (m, b) => mut("dr_policies", m[1], p => {
    p.last_failover_at = U().ago(0);
    p.failover_mode = b.planned ? "planned cutover" : "unplanned failover";
    p.state = "degraded";
    if (p.failback.reverse_on_failover && p.pair_id) {
      const t = p.target_cluster_id; p.target_cluster_id = p.source_cluster_id; p.source_cluster_id = t;
      const back = D().cluster_pairs.find(x => x.source_cluster_id === p.source_cluster_id && x.target_cluster_id === p.target_cluster_id);
      p.pair_id = back ? back.uuid : p.pair_id;
    }
  })],
  ["POST", /^\/replication-policies\/([\w-]+)\/failback$/, m => mut("dr_policies", m[1], p => {
    if (!p.last_failover_at) return;
    const t = p.target_cluster_id; p.target_cluster_id = p.source_cluster_id; p.source_cluster_id = t;
    p.failover_mode = null; p.last_failover_at = null; p.state = "healthy"; p.backlog_bytes = 0;
  })],
  ["POST", /^\/replication-policies\/([\w-]+)\/test-failover$/, m => mut("dr_policies", m[1], p => { p.last_test_at = U().ago(0); })],
  ["POST", /^\/replication-policies\/([\w-]+)\/resync$/, m => mut("dr_policies", m[1], p => {
    p.backlog_bytes = 0; p.state = "healthy"; p.last_replication_at = U().ago(0);
    p.lvol_ids.forEach(id => { const v = byId("lvols", id); if (v && v.replication) { v.replication.backlog_bytes = 0; v.replication.status = "healthy"; } });
  })],
  ["DELETE", /^\/replication-policies\/([\w-]+)$/, m => {
    const p = byId("dr_policies", m[1]);
    if (!p) return {__404: true};
    if (p.lvol_ids.length)
      return {__err: `${p.lvol_ids.length} volume(s) are still attached. A policy can only be deleted once it is empty.`};
    const r = drop("dr_policies", m[1]); U().rollup(); return r;
  }],
  ["POST", /^\/clusters\/([\w-]+)\/consistency-groups$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    if (!b.name) return {__err: "A consistency group name is required"};
    const ids = (b.lvol_ids || []).filter(id => { const v = byId("lvols", id); return v && v.cluster_id === c.uuid; });
    if (ids.length < 2) return {__err: "A consistency group needs at least two volumes"};
    if (D().consistency_groups.some(x => x.cluster_id === c.uuid && x.name === b.name))
      return {__err: `A consistency group named ${b.name} exists on this cluster`};
    // Groups may overlap: the same volume can be a member of several groups, so
    // a different crash-consistent set can be protected without disturbing the
    // existing ones.
    const g = {uuid: U().uuid(), cluster_id: c.uuid, name: b.name, lvol_ids: ids, created_at: U().ago(0),
      backup_policy: null, replication_policy: null};
    D().consistency_groups.push(g);
    U().rollup(); return {results: [g]};
  }],
  // A group can own its protection. Attaching a policy to the group attaches it
  // to every member, and the group keeps it for members added later.
  ["PUT", /^\/consistency-groups\/([\w-]+)\/backup-policy$/, (m, b) => mut("consistency_groups", m[1], g => {
    if (!b.policy_id) { g.backup_policy = null; 
      D().lvols.filter(v => g.lvol_ids.includes(v.uuid)).forEach(v => { if (v.backup_policy && v.backup_policy.via_cg === g.uuid) v.backup_policy = null; });
      return; }
    const p = byId("backup_policies", b.policy_id);
    if (!p) return {__err: "No such backup policy"};
    if (p.cluster_id !== g.cluster_id) return {__err: "The policy belongs to a different cluster"};
    // a policy driving a group has to be group-consistent, or the members would
    // be snapshotted at different instants and the group would mean nothing
    p.consistency_group = true;
    g.backup_policy = {uuid: p.uuid, policy_name: p.policy_name};
  })],
  // The group owns the replication cadence: how often the group snapshot is
  // taken and shipped, and how many older generations the target keeps. A DR
  // policy then just names the group.
  ["POST", /^\/consistency-groups\/([\w-]+)\/replicate$/, (m, b) => mut("consistency_groups", m[1], g => {
    const freq = Number(b.frequency_minutes);
    if (!freq || freq < 1) return {__err: "A replication frequency in minutes is required"};
    const retention = (b.retention || []).filter(r => r.interval && Number(r.keep) > 0)
      .map(r => ({interval: r.interval, keep: Number(r.keep)}));
    g.replication_config = {frequency_minutes: freq, retention};
  })],
  ["POST", /^\/consistency-groups\/([\w-]+)\/unreplicate$/, m => mut("consistency_groups", m[1], g => {
    const used = (D().dr_policies || []).filter(p => p.cg_id === g.uuid);
    if (used.length) return {__err: `${used.map(p => p.name).join(", ")} replicate${used.length > 1 ? "" : "s"} this group. Delete the DR polic${used.length > 1 ? "ies" : "y"} first.`};
    D().lvols.filter(v => g.lvol_ids.includes(v.uuid)).forEach(v => {
      if (v.replication && v.replication.via_cg === g.uuid) v.replication = null;
    });
    g.replication_config = null;
  })],
  ["POST", /^\/consistency-groups\/([\w-]+)\/lvols$/, (m, b) => mut("consistency_groups", m[1], g => {
    if (g.backup_policy || g.replication_config) return {__err: cgLocked(g)};
    (b.lvol_ids || []).forEach(id => {
      const v = byId("lvols", id);
      if (!v || v.cluster_id !== g.cluster_id || g.lvol_ids.includes(id)) return;
      g.lvol_ids.push(id);
    });
  })],
  ["DELETE", /^\/consistency-groups\/([\w-]+)\/lvols\/([\w-]+)$/, m => {
    const g = byId("consistency_groups", m[1]);
    if (!g) return {__404: true};
    if (g.backup_policy || g.replication_config) return {__err: cgLocked(g)};
    const app = (D().protected_apps || []).find(a => a.cg_id === g.uuid);
    if (app) return {__err: `${app.namespace}/${app.app_name} is protected through this group. Repoint the application first.`};
    if (g.lvol_ids.length <= 2) return {__err: "A consistency group must keep at least two volumes. Delete the group instead."};
    g.lvol_ids = g.lvol_ids.filter(x => x !== m[2]);
    U().rollup(); return {results: [g]};
  }],
  ["POST", /^\/consistency-groups\/([\w-]+)\/snapshot$/, (m, b) => {
    const g = byId("consistency_groups", m[1]);
    if (!g) return {__404: true};
    const vs = D().lvols.filter(v => g.lvol_ids.includes(v.uuid));
    const s = {uuid: U().uuid(), cluster_id: g.cluster_id, cg_id: g.uuid, cg_name: g.name,
      snapshot_name: b.name || `${g.name}-cgsnap`, created_at: U().ago(0), status: "online",
      members: vs.map(v => ({lvol_id: v.uuid, lvol_name: v.lvol_name, snapshot_id: U().uuid(),
        size: Math.round(v.size_util * .05)})),
      backup_version_id: null, backup_bucket: null};
    // a group snapshot also lands as a per-volume snapshot on each member
    vs.forEach((v, i) => {
      const chain = D().snapshots.filter(x => x.lvol_id === v.uuid);
      D().snapshots.push({uuid: s.members[i].snapshot_id, cluster_id: v.cluster_id, pool_id: v.pool_id,
        pool_name: v.pool_name, lvol_id: v.uuid, lvol_name: v.lvol_name,
        snapshot_name: `${s.snapshot_name}/${v.lvol_name}`,
        seq: chain.length + 1, parent_id: chain.length ? chain[chain.length - 1].uuid : null,
        created_at: s.created_at, size: s.members[i].size, status: "online",
        backup_version_id: null, cg_snapshot_id: s.uuid});
    });
    D().cg_snapshots.push(s); U().rollup(); return {results: [s]};
  }],
  ["POST", /^\/cg-snapshots\/([\w-]+)\/backup$/, (m, b) => mut("cg_snapshots", m[1], s => {
    s.backup_version_id = `v${String(D().cg_snapshots.filter(x => x.cg_id === s.cg_id && x.backup_version_id).length + 1).padStart(4, "0")}`;
    s.backup_bucket = b.bucket || `s3://sb-backup-eu/cg/${s.cg_name}/`;
  })],
  // Restore a group snapshot into new volumes — in this or any other cluster.
  ["POST", /^\/cg-snapshots\/([\w-]+)\/restore$/, (m, b) => {
    const s = byId("cg_snapshots", m[1]);
    if (!s) return {__404: true};
    const target = byId("clusters", b.cluster_id || s.cluster_id);
    if (!target) return {__404: true};
    const pool = D().pools.find(p => p.cluster_id === target.uuid && p.enabled !== false);
    if (!pool) return {__err: "The target cluster has no enabled pool to provision into"};
    const made = s.members.map((mem, i) => {
      const src = byId("lvols", mem.lvol_id) || {};
      const v = Object.assign({}, src, {uuid: U().uuid(),
        lvol_name: `${b.prefix || "restored"}-${mem.lvol_name}`,
        cluster_id: target.uuid, pool_id: pool.uuid, pool_name: pool.pool_name,
        status: "online", created_at: U().ago(0), size_util: mem.size,
        base_snapshot: {uuid: mem.snapshot_id, snapshot_name: s.snapshot_name, lvol_name: mem.lvol_name},
        nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`, snapshots_count: 0, backups_count: 0,
        consistency_groups: [], backup_policy: null, replication: null, migration: null});
      D().lvols.push(v); return v;
    });
    U().rollup(); return {results: made};
  }],
  ["DELETE", /^\/cg-snapshots\/([\w-]+)$/, m => {
    const s = byId("cg_snapshots", m[1]);
    if (s) D().snapshots = D().snapshots.filter(x => x.cg_snapshot_id !== s.uuid);
    const r = drop("cg_snapshots", m[1]); U().rollup(); return r;
  }],
  ["DELETE", /^\/consistency-groups\/([\w-]+)$/, m => {
    const g = byId("consistency_groups", m[1]);
    if (!g) return {__404: true};
    const app = (D().protected_apps || []).find(a => a.cg_id === g.uuid);
    if (app) return {__err: `${app.namespace}/${app.app_name} is protected through this group. Repoint the application first.`};
    if (g.backup_policy || g.replication_config)
      return {__err: "Detach the group's backup policy and replication cadence first."};
    if (D().cg_snapshots.some(x => x.cg_id === g.uuid))
      return {__err: "Delete the group's snapshots first."};
    const r = drop("consistency_groups", m[1]); U().rollup(); return r;
  }],
  // Store only the fields the chosen provider actually has, so switching from
  // Vault to AWS KMS does not leave a stale transit mount behind.
  ["PUT", /^\/clusters\/([\w-]+)\/kms$/, (m, b) => mut("clusters", m[1], c => {
    if (!b.address) return;
    const p = b.provider || "hashicorp_vault";
    const kms = {provider: p, address: b.address,
      auth_method: b.auth_method || (p === "hashicorp_vault" ? "kubernetes" : "workload_identity"),
      key_name: b.key_name, key_type: b.key_type || "aes256-gcm96",
      verify_tls: b.verify_tls !== false, rotation_days: Number(b.rotation_days || 0),
      keys_in_use: c.encrypted_lvols_count || 0,
      status: "connected", last_check_at: U().ago(0)};
    if (b.auth_role) kms.auth_role = b.auth_role;
    if (p === "hashicorp_vault") {
      kms.namespace = b.namespace || null;
      kms.mount_path = b.mount_path || "transit";
    }
    if (p === "aws_kms") kms.region = b.region || null;
    c.kms = kms;
  })],
  ["POST", /^\/clusters\/([\w-]+)\/kms\/test$/, m => mut("clusters", m[1], c => {
    if (!c.kms) return;
    c.kms.last_check_at = U().ago(0);
    c.kms.status = Math.random() > .15 ? "connected" : U().pick(["unreachable", "sealed"]);
  })],
  ["PUT", /^\/hosts\/([\w-]+)\/migration-taint$/, (m, b) => mut("hosts", m[1], h => {
    h.migration_taint = b.taint || null;
  })],
  ["POST", /^\/clusters\/([\w-]+)\/migrations$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    if (!b.name) return {__err: "A migration name is required"};
    const cross = b.mode === "cross_cluster";
    const tgtCluster = cross ? byId("clusters", b.target_cluster_id) : c;
    if (cross && !tgtCluster) return {__err: "Pick a target cluster"};
    if (cross && !tgtCluster.dr_target_eligible) return {__err: `${tgtCluster.name} is not qualified as a migration target`};
    if (cross && !D().cluster_pairs.some(p => p.source_cluster_id === c.uuid && p.target_cluster_id === tgtCluster.uuid))
      return {__err: "Cross-cluster migration ships data over a cluster pair. Pair the two clusters first."};
    const vols = b.scope === "cluster"
      ? D().lvols.filter(v => v.cluster_id === c.uuid && v.status === "online")
      : (b.lvol_ids || []).map(id => byId("lvols", id)).filter(v => v && v.cluster_id === c.uuid);
    if (!vols.length) return {__err: "No volume selected to migrate"};
    if (!cross) {
      const tainted = D().hosts.filter(h => h.cluster_id === c.uuid
        && (h.migration_taint || (b.target_zone_id && h.zone_id === b.target_zone_id))
        && (h.storage_node_ids || []).length);
      if (!tainted.length) return {__err: "No tainted target host with a storage node. Taint the destination hosts first."};
    }
    const first = vols.reduce((a, v) => a + v.size_util, 0);
    const g = {uuid: U().uuid(), cluster_id: c.uuid, name: b.name,
      mode: cross ? "cross_cluster" : "intra_cluster", scope: b.scope || "volumes",
      source_cluster_id: c.uuid, target_cluster_id: tgtCluster.uuid, target_zone_id: b.target_zone_id || null,
      target_taint: b.target_taint || "simplyblock.io/migration-target=true",
      follow_workload: !!b.follow_workload,
      state: cross ? "replicating" : "running",
      lvol_ids: vols.map(v => v.uuid), moved_count: 0,
      iterations: 0, iteration_limit: Number(b.iteration_limit || 12),
      first_snapshot_bytes: cross ? first : 0, last_snapshot_bytes: cross ? first : 0,
      freeze_threshold_bytes: cross ? Number(b.freeze_threshold_mb || 256) * 1e6 : 0,
      estimated_freeze_ms: 0, throughput_bytes_ps: 0,
      started_at: U().ago(0), completed_at: null, frozen_at: null, error: null};
    D().migrations.push(g); U().rollup(); return {results: [g]};
  }],
  // Final phase: freeze IO briefly, apply the last small snapshot, roll the
  // NVMe paths over to the target.
  ["POST", /^\/migrations\/([\w-]+)\/cutover$/, m => {
    const g = byId("migrations", m[1]);
    if (!g) return {__404: true};
    if (g.mode !== "cross_cluster") return {__err: "Intra-cluster migrations cut over per volume as they move."};
    if (g.last_snapshot_bytes > g.freeze_threshold_bytes)
      return {__err: `The outstanding snapshot is still ${Math.round(g.last_snapshot_bytes / 1e6)} MB — above the ${Math.round(g.freeze_threshold_bytes / 1e6)} MB freeze threshold. Let it converge further.`};
    const tgt = byId("clusters", g.target_cluster_id);
    const pool = D().pools.find(p => p.cluster_id === g.target_cluster_id && p.enabled !== false);
    if (!pool) return {__err: "The target cluster has no enabled pool"};
    const tgtNodes = D().storage_nodes.filter(n => n.cluster_id === g.target_cluster_id && n.status === "online");
    g.frozen_at = U().ago(0);
    g.lvol_ids.forEach((id, i) => {
      const v = byId("lvols", id);
      if (!v) return;
      v.cluster_id = g.target_cluster_id; v.pool_id = pool.uuid; v.pool_name = pool.pool_name;
      const t = tgtNodes[i % (tgtNodes.length || 1)];
      if (t) v.nodes = {primary: {uuid: t.uuid, hostname: t.hostname},
        secondary: tgtNodes[(i + 1) % (tgtNodes.length || 1)] ? {uuid: tgtNodes[(i + 1) % tgtNodes.length].uuid, hostname: tgtNodes[(i + 1) % tgtNodes.length].hostname} : null,
        tertiary: null};
      v.nqn = `nqn.2023-02.io.simplyblock:${U().hex(8)}`;
      v.migration = {state: "completed", instant: false, from: `${byId("clusters", g.source_cluster_id).name}`,
        target: tgt ? tgt.name : "target", reason: "cluster_migration",
        queued_at: g.started_at, completed_at: U().ago(0)};
      v.consistency_groups = []; v.replication = null;
    });
    g.moved_count = g.lvol_ids.length;
    g.state = "completed"; g.completed_at = U().ago(0);
    U().rollup(); return {results: [g]};
  }],
  ["POST", /^\/migrations\/([\w-]+)\/pause$/, m => mut("migrations", m[1], g => {
    if (g.state !== "completed") { g.state = "paused"; g.throughput_bytes_ps = 0; }
  })],
  ["POST", /^\/migrations\/([\w-]+)\/resume$/, m => mut("migrations", m[1], g => {
    if (g.state === "paused") g.state = g.mode === "cross_cluster" ? "converging" : "running";
  })],
  ["DELETE", /^\/migrations\/([\w-]+)$/, m => {
    const g = byId("migrations", m[1]);
    if (!g) return {__404: true};
    if (g.state === "frozen") return {__err: "The migration is mid-cutover and cannot be cancelled now."};
    const r = drop("migrations", m[1]); U().rollup(); return r;
  }],
  // Ramen actions: failover moves the app to its failover cluster, relocate
  // moves it back to the preferred one after a clean sync.
  ["POST", /^\/protected-apps\/([\w-]+)\/failover$/, (m, b = {}) => {
    const app = byId("protected_apps", m[1]);
    if (!app) return {__404: true};
    if (app.phase === "FailedOver") return {__err: "This application is already failed over. Use relocate to move it back."};
    if (app.phase === "WaitForUser")
      return {__err: "The previous failover is still waiting for the stale workload to be cleaned up. Confirm cleanup first."};
    if (["FailingOver", "Relocating"].includes(app.phase))
      return {__err: "An action is already in flight on this application."};
    if (app.protection_mode === "backup") {
      // point-in-time: the last backup at or before the chosen generation / time
      const rp = b.recovery_point || {};
      const pts = (app.recovery_points || []).slice().sort((x, y) => y.generation - x.generation);
      const hit = rp.generation ? pts.find(p => p.generation <= Number(rp.generation))
        : rp.before ? pts.find(p => new Date(p.at) <= new Date(rp.before)) : pts[0];
      if (!hit) return {__err: rp.before ? "No backup exists at or before that time." : "No backup at or below that generation."};
      app.restore_point = hit;
    }
    return mut("protected_apps", m[1], a => {
      a.phase = "FailingOver"; a.action = "Failover";
      a.progression = a.protection_mode === "backup" ? "RestoringBackups" : "EnsuringVolumesAreSecondary";
      a.action_started_ms = Date.now();
    });
  }],
  ["PUT", /^\/protected-apps\/([\w-]+)\/protection$/, (m, b) => {
    const a = byId("protected_apps", m[1]);
    if (!a) return {__404: true};
    if (["FailingOver", "Relocating"].includes(a.phase)) return {__err: "Protection cannot change while an action is running."};
    if (b.mode === "backup") {
      const pol0 = byId("dr_policies", a.policy_id);
      const g = pol0 && pol0.cg_id ? byId("consistency_groups", pol0.cg_id) : null;
      if (!g) return {__err: "This application's DR policy names no consistency group, so there is no group-consistent chain to recover from."};
      if (!g.backup_policy) return {__err: `${g.name} has no backup policy. Attach one to the group — the group's chain is what a backup-based recovery restores from.`};
      const pol = byId("backup_policies", g.backup_policy.uuid);
      if (!pol) return {__err: "The group's backup policy no longer exists"};
      a.protection_mode = "backup"; a.backup_policy_id = pol.uuid; a.backup_policy_name = pol.policy_name;
      a.recovery_points = a.recovery_points && a.recovery_points.length ? a.recovery_points : [];
      a.pvc_ids.forEach(id => { const p = D().pvcs.find(x => x.uuid === id); const lv = p && D().lvols.find(x => x.uuid === p.lvol_id); if (lv) lv.backup_policy = {uuid: pol.uuid, policy_name: pol.policy_name}; });
    } else { a.protection_mode = "replication"; a.backup_policy_id = null; a.backup_policy_name = null; a.restore_point = null; }
    U().rollup(); return {results: [a]};
  }],
  ["POST", /^\/protected-apps\/([\w-]+)\/relocate$/, m => {
    const a = byId("protected_apps", m[1]);
    if (!a) return {__404: true};
    if (a.phase !== "FailedOver") return {__err: "Relocate returns a failed-over application to its preferred cluster. This one is not failed over."};
    if (!a.rpo_met) return {__err: "The last group sync is outside the scheduling interval. Let it catch up before relocating."};
    a.phase = "Relocating"; a.action = "Relocate";
    a.progression = "WaitingForResourceRestore";     // first step of Relocating
    a.action_started_ms = Date.now();
    U().rollup(); return {results: [a]};
  }],
  ["POST", /^\/protected-apps\/([\w-]+)\/cleanup$/, m => mut("protected_apps", m[1], a => {
    if (a.phase !== "WaitForUser") return;
    a.phase = "FailedOver"; a.progression = "Completed"; a.action = null;
  })],
  ["PUT", /^\/protected-apps\/([\w-]+)$/, (m, b) => mut("protected_apps", m[1], a => {
    if (b.policy_id !== undefined) {
      const pol = byId("dr_policies", b.policy_id);
      if (!pol) return {__err: "No such DR policy"};
      // The policy brings its consistency group with it: that group's members
      // are the volumes this application protects and fails over.
      const g = pol.cg_id ? byId("consistency_groups", pol.cg_id) : null;
      if (pol.mode === "asynchronous" && !g) return {__err: `${pol.name} names no consistency group.`};
      a.cg_id = g ? g.uuid : null; a.cg_name = g ? g.name : null;
      if (g) {
        const pvcs = g.lvol_ids.map(vid => D().pvcs.find(p => p.lvol_id === vid)).filter(Boolean);
        if (!pvcs.length) return {__err: `No PVC is backed by a volume of ${g.name}.`};
        a.pvc_ids = pvcs.map(p => p.uuid);
      }
      if (a.protection_mode === "backup") {
        if (!g || !g.backup_policy) { a.protection_mode = "replication"; a.backup_policy_id = null; a.backup_policy_name = null; }
        else { a.backup_policy_id = g.backup_policy.uuid; a.backup_policy_name = g.backup_policy.policy_name; }
      }
    }
    if (b.policy_id && byId("dr_policies", b.policy_id)) {
      a.policy_id = b.policy_id;
      a.policy_name = byId("dr_policies", b.policy_id).name;
    }
    if (b.pvc_selector) a.pvc_selector = b.pvc_selector;
    if (b.kube_object_protection !== undefined) a.kube_object_protection = !!b.kube_object_protection;
  })],
  // An application's PVC set can come from a consistency group instead of a
  // label selector: the group is then the crash-consistent boundary.

  ["PUT", /^\/protected-apps\/([\w-]+)\/recipe$/, (m, b) => {
    const a = byId("protected_apps", m[1]);
    if (!a) return {__404: true};
    if (["FailingOver", "Relocating"].includes(a.phase)) return {__err: "The recipe cannot change while a failover or relocate is running."};
    if (b === null || b.remove) { a.recipe = null; return {results: [a]}; }
    if (!b.name || !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(b.name)) return {__err: "Recipe name must be a DNS-1123 label."};
    const groups = b.groups || [], hooks = b.hooks || [];
    if (groups.some(g => !g.name)) return {__err: "Every group needs a name."};
    if (new Set(groups.map(g => g.name)).size !== groups.length) return {__err: "Group names must be unique."};
    if (hooks.some(h => !h.name)) return {__err: "Every hook needs a name."};
    if (hooks.some(h => h.type === "exec" ? !((h.ops || [])[0] || {}).command : !((h.chks || [])[0] || {}).condition)) return {__err: "Every exec hook needs a command, every check hook a condition."};
    const refs = new Set([...groups.map(g => "group:" + g.name), ...hooks.flatMap(h => [].concat(h.ops || [], h.chks || []).map(o => "hook:" + h.name + "/" + o.name))]);
    for (const wf of ["captureWorkflow", "recoverWorkflow"]) {
      const bad = ((b[wf] || {}).sequence || []).find(s => !refs.has(s.group ? "group:" + s.group : "hook:" + s.hook));
      if (bad) return {__err: `${wf} references ${bad.group ? "group " + bad.group : "hook " + bad.hook}, which is not defined.`};
    }
    a.recipe = {name: b.name, namespace: a.namespace, appType: b.appType || null, groups, hooks,
      captureWorkflow: Object.assign({failOn: "any-error", sequence: []}, b.captureWorkflow || {}),
      recoverWorkflow: Object.assign({failOn: "any-error", sequence: []}, b.recoverWorkflow || {})};
    return {results: [a]};
  }],
  ["DELETE", /^\/protected-apps\/([\w-]+)$/, m => {
    const a = byId("protected_apps", m[1]);
    if (!a) return {__404: true};
    if (["FailingOver", "Relocating"].includes(a.phase))
      return {__err: "The application is mid-action. Wait for it to settle before unprotecting."};
    const r = drop("protected_apps", m[1]); U().rollup(); return r;
  }],
  ["POST", /^\/protected-apps$/, (m, b) => {
    const pol = byId("dr_policies", b.policy_id);
    if (!pol) return {__err: "Pick a DR policy"};
    if ((pol.dr_cluster_ids || []).length < 2) return {__err: `${pol.name} does not reach two Kubernetes clusters — no application can fail over on it.`};
    if (!b.app_name || !b.namespace) return {__err: "Application name and namespace are required"};
    const pref = byId("dr_clusters", b.preferred_cluster_id) || byId("dr_clusters", pol.dr_cluster_ids[0]);
    const other = pol.dr_cluster_ids.find(x => x !== pref.uuid);
    const claims = D().pvcs.filter(p => p.k8s_cluster_id === pref.k8s_cluster_id && p.namespace === b.namespace);
    if (!claims.length) return {__err: `No PVC found in namespace ${b.namespace} on ${pref.name}`};
    const a = {uuid: U().uuid(), app_name: b.app_name, namespace: b.namespace,
      app_kind: b.app_kind || "ApplicationSet",
      policy_id: pol.uuid, policy_name: pol.policy_name,
      preferred_cluster_id: pref.uuid, failover_cluster_id: other,
      pvc_selector: b.pvc_selector_key ? {[b.pvc_selector_key]: b.pvc_selector_value || ""} : {},
      phase: "Deployed", progression: "Completed", action: null,
      pvc_ids: claims.map(p => p.uuid), vrg_state: "primary",
      last_group_sync_at: U().ago(0), last_group_sync_duration_s: 0, last_group_sync_bytes: 0,
      kube_object_protection: !!b.kube_object_protection, created_at: U().ago(0)};
    D().protected_apps.push(a); U().rollup(); return {results: [a]};
  }],
  ["POST", /^\/dr-clusters\/([\w-]+)\/fence$/, m => mut("dr_clusters", m[1], dc => {
    dc.fencing_state = "ManuallyFenced";
  })],
  ["POST", /^\/dr-clusters\/([\w-]+)\/unfence$/, m => mut("dr_clusters", m[1], dc => {
    dc.fencing_state = "Unfenced";
  })],
  // Zones are fixed when the cluster is created and cannot be changed afterwards.
  ["PUT", /^\/clusters\/([\w-]+)\/zones$/, m => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    return {__err: "Cluster zones are fixed at creation time and cannot be added or changed afterwards."};
  }],
  // zone and region are read from the node labels; only the rack / cabinet
  // taints can be edited here
  ["PUT", /^\/hosts\/([\w-]+)\/placement$/, (m, b) => mut("hosts", m[1], h => {
    h.rack_id = b.rack_id || null;
    h.cabinet_id = b.cabinet_id || null;
  })],
  ["POST", /^\/clusters\/([\w-]+)\/pools$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    const name = (b.name || "").trim();
    if (!name) return {__err: "Name the pool"};
    if (D().pools.some(p => p.cluster_id === c.uuid && p.pool_name === name)) return {__err: `A pool named ${name} exists on this cluster`};
    // bi-directional DH-CHAP is decided here and cannot be changed later: the
    // pool's namespaces are created with mutual authentication or without it
    const p = {uuid: U().uuid(), cluster_id: c.uuid, pool_name: name, enabled: true,
      dhchap_bidirectional: !!b.dhchap_bidirectional, qos: normQos(b), created_at: U().ago(0)};
    D().pools.push(p); U().rollup(); return {results: [p]};
  }],
  ["PUT", /^\/pools\/([\w-]+)\/qos$/, (m, b) => mut("pools", m[1], p => { p.qos = normQos(b); })],
  ["POST", /^\/pools\/([\w-]+)\/enable$/, m => mut("pools", m[1], p => { p.enabled = true; })],
  ["POST", /^\/pools\/([\w-]+)\/disable$/, m => mut("pools", m[1], p => { p.enabled = false; })],
  ["POST", /^\/clusters\/([\w-]+)\/suspend$/, m => mut("clusters", m[1], c => { c.status = "suspended"; })],
  ["POST", /^\/clusters\/([\w-]+)\/activate$/, m => mut("clusters", m[1], c => { c.status = "in_activation"; })],
  ["POST", /^\/clusters\/([\w-]+)\/storage-nodes$/, (m, b) => {
    const c = byId("clusters", m[1]), h = byId("hosts", b.host_id);
    if (!c || !h) return {__404: true};
    if (h.storage_node_ids.length >= 2) return {__err: "Host already runs two storage nodes"};
    if ((c.zone_ids || []).length && !(c.zone_ids || []).includes(h.zone_id))
      return {__err: "That host is not in one of the cluster's zones. Storage nodes can only be added from the zones assigned at cluster creation."};
    const fdErr = fdBalanceCheck(c, h, "add");
    if (fdErr) return {__err: fdErr};
    const n = addStorageNode(c, h);
    if (c.failure_domains_enabled && !b.failure_domain) return {__err: "This cluster uses failure domains. A new node must be given a failure domain label — it is fixed for the node's lifetime."};
    if (b.failure_domain) n.failure_domain = b.failure_domain;
    startNodeOp(n, "expansion");
    D().devices.filter(d => d.node_id === n.uuid).forEach(d => { d.status = "new"; });
    U().rollup(); return {results: [n]};
  }],
  ["POST", /^\/storage-nodes\/([\w-]+)\/shutdown$/, (m, b) => mut("storage_nodes", m[1], n => {
    n.status = "offline";
    D().devices.filter(d => d.node_id === n.uuid).forEach(d => { if (d.status !== "removed") d.status = "unavailable"; });
    n.last_action = b.force ? "force shutdown" : "shutdown"; n.maintenance = true;
  })],
  ["POST", /^\/storage-nodes\/([\w-]+)\/restart$/, m => mut("storage_nodes", m[1], n => {
    n.status = "in_restart";
    D().devices.filter(d => d.node_id === n.uuid).forEach(d => { if (d.status === "unavailable") d.status = "online"; });
  })],
  ["POST", /^\/storage-nodes\/([\w-]+)\/migrate$/, (m, b) => {
    const h = byId("hosts", b.host_id);
    if (!h) return {__404: true};
    const n0 = byId("storage_nodes", m[1]);
    const c0 = n0 ? byId("clusters", n0.cluster_id) : null;
    if (c0 && (c0.zone_ids || []).length && !(c0.zone_ids || []).includes(h.zone_id))
      return {__err: "The target host is not in one of the cluster's zones."};
    if (n0 && n0.op) return {__err: `A ${n0.op.kind} is already running on this node.`};
    if (h.storage_node_ids.length >= 2) return {__err: "The target host already runs two storage nodes."};
    if (!h.prepared) return {__err: "The target host is not prepared. Prepare it first — hugepages and core isolation have to be in place before a node can restart on it."};
    return mut("storage_nodes", m[1], n => {
      startNodeOp(n, "migration", {target_host_id: h.uuid, target_hostname: h.hostname, source_hostname: n.hostname});
      D().devices.filter(d => d.node_id === n.uuid).forEach(d => { d.status = "unavailable"; });
    });
  }],
  ["POST", /^\/storage-nodes\/([\w-]+)\/devices$/, (m, b) => {
    const n = byId("storage_nodes", m[1]);
    if (!n) return {__404: true};
    const c = byId("clusters", n.cluster_id);
    const [model, size] = U().pick(U().MODELS);
    D().devices.push({uuid: U().uuid(), node_id: n.uuid, cluster_id: n.cluster_id, host_id: n.host_id,
      cluster_device_class: c.device_class, numa_socket: 0,
      serial_number: `S${U().hex(3).toUpperCase()}NY0${U().int(100000, 999999)}`,
      pcie_address: b.pcie_address || null, device_name: b.device_name || null,
      model_number: `${model} ${(size / 1e12).toFixed(2)}TB`, firmware_revision: "GXA7711",
      status: "new", health_check: null, size_total: size, size_util: 0,
      temperature_c: 33, percentage_used: 0, power_on_hours: 0,
      io_stats: U().ioStats(0, 0), io_history: {iops: U().series(0, 0), bytes: U().series(0, 0)}});
    U().rollup(); return {results: []};
  }],
  ["POST", /^\/devices\/([\w-]+)\/restart$/, m => mut("devices", m[1], d => { d.status = "online"; d.health_check = d.health_check || "good"; })],
  ["POST", /^\/devices\/([\w-]+)\/fail$/, m => mut("devices", m[1], d => { d.status = "removed"; d.health_check = null; d.size_util = 0; })],
  ["POST", /^\/devices\/([\w-]+)\/health-check$/, m => mut("devices", m[1], d => {
    d.health_check = d.status === "online" || d.status === "read_only" ? U().pick(["good", "good", "good", "warn"]) : null;
    d.last_health_check = U().ago(0);
  })],
  ["DELETE", /^\/devices\/([\w-]+)$/, m => {
    const d = byId("devices", m[1]);
    if (d) { const h = byId("hosts", d.host_id); if (h) h.devices.forEach(x => { if (x.pcie_address === d.pcie_address || x.device_name === d.device_name) x.assigned_node_id = null; }); }
    const r = drop("devices", m[1]); U().rollup(); return r;
  }],
  ["DELETE", /^\/storage-nodes\/([\w-]+)$/, m => {
    const n = byId("storage_nodes", m[1]);
    if (!n) return {__404: true};
    const cRem = byId("clusters", n.cluster_id);
    if (cRem) { const e = fdBalanceCheck(cRem, byId("hosts", n.host_id), "remove", n); if (e) return {__err: e}; }
    // Drain first: every volume whose primary sits here is moved off by instant
    // migration. Volumes pinned to this node block the removal.
    const others = D().storage_nodes.filter(x => x.cluster_id === n.cluster_id && x.uuid !== n.uuid && x.status === "online");
    const hosted = D().lvols.filter(v => v.nodes && v.nodes.primary && v.nodes.primary.uuid === n.uuid);
    const pinned = hosted.filter(v => v.affinity && v.affinity.mode === "node" && v.affinity.pinned_node_id === n.uuid);
    if (pinned.length)
      return {__err: `${pinned.length} volume(s) are pinned to this node by affinity. Repin or clear their affinity first.`};
    if (hosted.length && !others.length)
      return {__err: `${hosted.length} volume(s) are primary on this node and there is no other online node to move them to.`};
    const counts = {};
    others.forEach(x => counts[x.uuid] = 0);
    D().lvols.filter(v => v.cluster_id === n.cluster_id && v.nodes && v.nodes.primary && v.nodes.primary.uuid !== n.uuid)
      .forEach(v => { counts[v.nodes.primary.uuid] = (counts[v.nodes.primary.uuid] || 0) + 1; });
    hosted.forEach(v => {
      const t = others.slice().sort((p, q) => (counts[p.uuid] || 0) - (counts[q.uuid] || 0))[0];
      v.nodes = Object.assign({}, v.nodes, {primary: {uuid: t.uuid, hostname: t.hostname}});
      v.migration = {state: "completed", instant: true, from: n.hostname, target: t.hostname,
        reason: "node_removal", queued_at: U().ago(0), completed_at: U().ago(0)};
      counts[t.uuid] = (counts[t.uuid] || 0) + 1;
    });
    // Removal is asynchronous: the node enters in_removal and the control plane
    // works through it. finalizeRemovals() completes it a while later.
    startNodeOp(n, "removal", {volumes_moved: hosted.length});
    D().devices.filter(d => d.node_id === n.uuid).forEach(d => { d.status = "unavailable"; });
    U().rollup();
    return {results: [n]};
  }],
  ["POST", /^\/clusters\/([\w-]+)\/hosts\/prepare$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    const hosts = (b.host_ids || []).map(id => byId("hosts", id)).filter(h => h && h.cluster_id === c.uuid);
    if (!hosts.length) return {__err: "Select at least one worker node"};
    hosts.forEach(h => {
      h.status = "inspecting";
      h.inspection = {state: "running", started_at: U().ago(0), started_ms: Date.now(),
        pod: `sb-inspect-${h.hostname}`, step: "deploying inspection pod"};
    });
    U().rollup();
    return {results: hosts};
  }],
  ["POST", /^\/hosts\/([\w-]+)\/configure$/, (m, b) => {
    const h = byId("hosts", m[1]);
    if (!h) return {__404: true};
    if (h.status !== "inspected") return {__err: "Host inventory has not been collected yet"};
    const sockets = (b.numa_sockets || []).map(Number);
    if (!sockets.length) return {__err: "Select at least one NUMA socket"};
    if (!(b.device_ids || []).length) return {__err: "Select at least one device to assign"};
    if (!b.mgmt_nic) return {__err: "Select a management NIC"};
    if (!(b.data_nics || []).length) return {__err: "Select at least one data NIC"};
    const hc = byId("clusters", h.cluster_id);
    if (hc && hc.multipathing_enabled && (b.data_nics || []).length !== 2)
      return {__err: "This cluster uses multipathing: exactly two data NICs must be specified per storage node."};
    if (hc && !hc.multipathing_enabled && (b.data_nics || []).length > 1)
      return {__err: "This cluster does not use multipathing: specify a single data NIC."};
    h.status = "available";
    h.numa_sockets_used = sockets;
    h.memory_per_pod = Number(b.memory_per_pod) * 1e9;
    h.hugepages_reserved = Number(b.hugepages_per_pod || Math.round(b.memory_per_pod / 2)) * 1e9;
    h.hugepages_allocated = 0;
    h.mgmt_nic = b.mgmt_nic;
    h.data_nics = b.data_nics;
    h.selected_device_ids = b.device_ids;
    h.devices = h.devices.filter(d => b.device_ids.includes(d.id));
    h.prepared_at = U().ago(0);
    h.labels = Object.assign({}, h.k8s_labels, {"simplyblock.io/storage-node": "true"});
    h.inspection = null;
    U().rollup();
    return {results: [h]};
  }],
  ["POST", /^\/hosts\/([\w-]+)\/devices\/([\w-]+)\/reserve$/, (m, b) => {
    const h = byId("hosts", m[1]);
    if (!h) return {__404: true};
    const d = h.devices.find(x => x.id === m[2]);
    if (!d) return {__404: true};
    d.reserved_for_node_id = b.node_id; d.reserved = true;
    U().rollup(); return {results: [h]};
  }],
  ["DELETE", /^\/lvols\/([\w-]+)$/, m => {
    D().snapshots = D().snapshots.filter(s => s.lvol_id !== m[1]);
    const r = drop("lvols", m[1]); U().rollup(); return r;
  }],
  // Volumes can only grow — shrinking is refused, as it is in Kubernetes.
  ["POST", /^\/lvols\/([\w-]+)\/resize$/, (m, b) => {
    const v = byId("lvols", m[1]);
    if (!v) return {__404: true};
    const size = Number(b.size);
    if (!(size > 0)) return {__err: "A positive size is required"};
    if (size < v.size_prov)
      return {__err: `Volumes can only be expanded. Current size is ${Math.round(v.size_prov / 1e9)} GB.`};
    if (size === v.size_prov) return {__err: "That is the current size"};
    v.size_prov = size;
    const pvc = (D().pvcs || []).find(p => p.lvol_id === v.uuid);
    if (pvc) { pvc.requested_bytes = size; pvc.actual_bytes = size; }
    U().rollup(); return {results: [v]};
  }],
  // A PVC expansion propagates straight through to its logical volume.
  ["POST", /^\/pvcs\/([\w-]+)\/resize$/, (m, b) => {
    const p = byId("pvcs", m[1]);
    if (!p) return {__404: true};
    const sc = byId("storage_classes", p.storage_class_id);
    if (sc && sc.allow_volume_expansion === false)
      return {__err: `StorageClass ${sc.name} does not allow volume expansion.`};
    const size = Number(b.size);
    if (size < p.requested_bytes)
      return {__err: `A PVC can only be expanded. It currently requests ${Math.round(p.requested_bytes / 1e9)} GB.`};
    if (size === p.requested_bytes) return {__err: "That is the current request"};
    p.requested_bytes = size; p.actual_bytes = size;
    const v = p.lvol_id ? byId("lvols", p.lvol_id) : null;
    if (v) v.size_prov = size;
    U().rollup(); return {results: [p]};
  }],
  // ---- buckets: one bucket is one filesystem is one logical volume --------
  ["POST", /^\/clusters\/([\w-]+)\/buckets$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    if (b.lvol_id) {
      const ex = byId("lvols", b.lvol_id);
      if (ex && ex.pvc) return {__err: "That volume already backs a PVC. A volume is either a bucket filesystem or a claim, never both."};
    }
    if (!c.object_storage.enabled) return {__err: "Object storage is not enabled on this cluster."};
    if (!b.name) return {__err: "A bucket name is required"};
    if (!/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(b.name))
      return {__err: "Bucket names must be 3–63 characters, lowercase letters, digits, dots or hyphens."};
    if (D().buckets.some(x => x.cluster_id === c.uuid && x.name === b.name))
      return {__err: "A bucket with that name already exists in this cluster."};
    const pool = byId("pools", b.pool_id) || D().pools.find(p => p.cluster_id === c.uuid && p.enabled !== false);
    if (!pool) return {__err: "No enabled pool to provision the bucket's volume into."};
    const nodes = D().storage_nodes.filter(n => n.cluster_id === c.uuid && n.status === "online");
    const size = Number(b.size) || 1e12;
    const v = {uuid: U().uuid(), pool_id: pool.uuid, pool_name: pool.pool_name, cluster_id: c.uuid,
      lvol_name: `bucket-${b.name}`, status: "online",
      nodes: {primary: nodes[0] ? {uuid: nodes[0].uuid, hostname: nodes[0].hostname} : null,
        secondary: nodes[1] ? {uuid: nodes[1].uuid, hostname: nodes[1].hostname} : null, tertiary: null},
      size_prov: size, size_util: 0, crypto_enabled: !!b.encryption, qos: null,
      nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`,
      base_snapshot: null, consistency_groups: [], affinity: null, pvc: null,
      backup_policy: null, replication: null, migration: null,
      compression_dedup_enabled: true, logical_used: 0,
      created_at: U().ago(0),
      io_stats: U().ioStats(0, 0), io_history: {iops: U().series(0, 0), bytes: U().series(0, 0)}};
    D().lvols.push(v);
    const bucket = {uuid: U().uuid(), cluster_id: c.uuid, name: b.name,
      lvol_id: v.uuid, lvol_name: v.lvol_name, pool_id: pool.uuid, pool_name: pool.pool_name,
      status: "online", versioning: !!b.versioning, object_lock: !!b.object_lock,
      quota_bytes: Number(b.quota) || 0, objects: 0, size_bytes: 0,
      access: {service_account: b.service_account || `sb-s3-${b.name}`,
        namespace: b.namespace || "default",
        secret_name: `${b.name}-s3-credentials`,
        access_key_id: `SB${U().hex(9).toUpperCase()}`,
        policy: b.policy || "read-write", public: false},
      created_at: U().ago(0)};
    D().buckets.push(bucket);
    v.bucket = {uuid: bucket.uuid, name: bucket.name};
    U().rollup(); return {results: [bucket]};
  }],
  ["PUT", /^\/buckets\/([\w-]+)$/, (m, b) => mut("buckets", m[1], x => {
    if (b.versioning !== undefined) x.versioning = !!b.versioning;
    if (b.object_lock !== undefined) x.object_lock = !!b.object_lock;
    if (b.quota !== undefined) x.quota_bytes = Number(b.quota) || 0;
  })],
  ["PUT", /^\/buckets\/([\w-]+)\/access$/, (m, b) => mut("buckets", m[1], x => {
    x.access = Object.assign({}, x.access, {
      service_account: b.service_account || x.access.service_account,
      namespace: b.namespace || x.access.namespace,
      policy: b.policy || x.access.policy,
      public: !!b.public});
    if (b.rotate_key) x.access.access_key_id = "SB" + U().hex(9).toUpperCase();
  })],
  ["POST", /^\/buckets\/([\w-]+)\/resize$/, (m, b) => {
    const x = byId("buckets", m[1]);
    if (!x) return {__404: true};
    const v = byId("lvols", x.lvol_id);
    if (!v) return {__err: "The bucket's volume no longer exists."};
    const size = Number(b.size);
    if (size < v.size_prov)
      return {__err: `A bucket's filesystem can only grow. It is currently ${Math.round(v.size_prov / 1e9)} GB.`};
    v.size_prov = size;
    U().rollup(); return {results: [x]};
  }],
  ["DELETE", /^\/buckets\/([\w-]+)$/, m => {
    const x = byId("buckets", m[1]);
    if (!x) return {__404: true};
    if (x.objects > 0) return {__err: `The bucket still holds ${x.objects.toLocaleString()} object(s). Empty it first.`};
    D().lvols = D().lvols.filter(v => v.uuid !== x.lvol_id);
    D().snapshots = D().snapshots.filter(s2 => s2.lvol_id !== x.lvol_id);
    const r = drop("buckets", m[1]); U().rollup(); return r;
  }],
  ["POST", /^\/lvols\/([\w-]+)\/snapshot$/, (m, b) => {
    const v = byId("lvols", m[1]);
    if (!v) return {__404: true};
    const chain = D().snapshots.filter(x => x.lvol_id === v.uuid).sort((a, c) => a.seq - c.seq);
    const prev = chain[chain.length - 1] || null;
    const s = {uuid: U().uuid(), cluster_id: v.cluster_id, pool_id: v.pool_id, pool_name: v.pool_name,
      lvol_id: v.uuid, lvol_name: v.lvol_name,
      snapshot_name: b.name || `${v.lvol_name}-snap-${String(chain.length + 1).padStart(3, "0")}`,
      seq: chain.length + 1, parent_id: prev ? prev.uuid : null,
      created_at: U().ago(0), size: Math.round(v.size_util * 0.06), status: "online", backup_version_id: null};
    D().snapshots.push(s); U().rollup(); return {results: [s]};
  }],
  ["POST", /^\/lvols\/([\w-]+)\/clone$/, (m, b) => {
    const v = byId("lvols", m[1]);
    if (!v) return {__404: true};
    const c = Object.assign({}, v, {uuid: U().uuid(), lvol_name: b.name || v.lvol_name + "-clone",
      size_util: Math.round(v.size_util * .02), created_at: U().ago(0), status: "online",
      nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`, snapshots_count: 0, backups_count: 0,
      // a clone is a new volume: it is not protected until it is explicitly joined
      replication: null, base_snapshot: null, migration: null});
    D().lvols.push(c); U().rollup(); return {results: [c]};
  }],
  // Instant volume migration: the primary role moves to another node with no
  // data copy, so the move completes immediately rather than being queued.
  ["POST", /^\/lvols\/([\w-]+)\/migrate$/, (m, b) => mut("lvols", m[1], v => {
    const n = byId("storage_nodes", b.node_id);
    if (!n) return;
    const from = v.nodes && v.nodes.primary ? v.nodes.primary.hostname : "?";
    v.nodes = Object.assign({}, v.nodes, {primary: {uuid: n.uuid, hostname: n.hostname}});
    v.migration = {state: "completed", instant: true, from, target: n.hostname,
      reason: b.reason || "manual", queued_at: U().ago(0), completed_at: U().ago(0),
      task_id: lvolMigrationTask(v, n, b.reason || "manual").uuid};
    if (v.affinity && v.affinity.mode === "node") {
      v.affinity.pinned_node_id = n.uuid; v.affinity.pinned_node = n.hostname; v.affinity.satisfied = true;
    }
    if (v.affinity && v.affinity.mode === "pod") v.affinity.satisfied = v.affinity.workload_node === n.hostname;
  })],
  ["POST", /^\/lvols\/([\w-]+)\/rebalance$/, m => {
    const v = byId("lvols", m[1]);
    if (!v) return {__404: true};
    const c = byId("clusters", v.cluster_id);
    const ns = D().storage_nodes.filter(n => n.cluster_id === v.cluster_id && n.status === "online");
    if (!ns.length) return {__err: "No online node to move the volume to"};
    const counts = {};
    ns.forEach(n => counts[n.uuid] = 0);
    D().lvols.filter(x => x.cluster_id === v.cluster_id && x.nodes && x.nodes.primary)
      .forEach(x => { counts[x.nodes.primary.uuid] = (counts[x.nodes.primary.uuid] || 0) + 1; });
    const target = ns.slice().sort((p, q) => (counts[p.uuid] || 0) - (counts[q.uuid] || 0))[0];
    if (v.nodes && v.nodes.primary && v.nodes.primary.uuid === target.uuid)
      return {__err: "This volume already sits on the least loaded node"};
    const from = v.nodes && v.nodes.primary ? v.nodes.primary.hostname : "?";
    v.nodes = Object.assign({}, v.nodes, {primary: {uuid: target.uuid, hostname: target.hostname}});
    v.migration = {state: "completed", instant: true, from, target: target.hostname,
      reason: "rebalance", queued_at: U().ago(0), completed_at: U().ago(0),
      task_id: lvolMigrationTask(v, target, "rebalance").uuid};
    U().rollup(); return {results: [v]};
  }],
  ["PUT", /^\/lvols\/([\w-]+)\/affinity$/, (m, b) => mut("lvols", m[1], v => {
    if (b.mode === "none") { v.affinity = null; return; }
    if (b.mode === "node") {
      const n = byId("storage_nodes", b.node_id) || (v.nodes && v.nodes.primary ? byId("storage_nodes", v.nodes.primary.uuid) : null);
      v.affinity = {mode: "node", pinned_node_id: n ? n.uuid : null,
        pinned_node: n ? n.hostname : null, satisfied: true};
    } else {
      v.affinity = {mode: "pod", workload: b.workload || `${v.lvol_name}-0`,
        workload_node: v.nodes && v.nodes.primary ? v.nodes.primary.hostname : null, satisfied: true};
    }
  })],
  ["PUT", /^\/clusters\/([\w-]+)\/auto-rebalance$/, (m, b) => mut("clusters", m[1], c => {
    c.auto_rebalance = Object.assign({}, c.auto_rebalance, {enabled: !!b.enabled});
  })],
  ["POST", /^\/clusters\/([\w-]+)\/rebalance$/, m => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    if (!c.capabilities.rebalancing) return {__err: "Edge clusters do not rebalance."};
    const ns = D().storage_nodes.filter(n => n.cluster_id === c.uuid && n.status === "online");
    if (ns.length < 2) return {__err: "Rebalancing needs at least two online nodes"};
    const counts = {};
    ns.forEach(n => counts[n.uuid] = 0);
    const vols = D().lvols.filter(v => v.cluster_id === c.uuid && v.nodes && v.nodes.primary);
    vols.forEach(v => { counts[v.nodes.primary.uuid] = (counts[v.nodes.primary.uuid] || 0) + 1; });
    let moves = 0;
    for (let pass = 0; pass < 40; pass++) {
      const sorted = ns.slice().sort((p, q) => (counts[q.uuid] || 0) - (counts[p.uuid] || 0));
      const hot = sorted[0], cold = sorted[sorted.length - 1];
      const goal = 1;
      if ((counts[hot.uuid] || 0) - (counts[cold.uuid] || 0) <= goal) break;
      const v = vols.find(x => x.nodes.primary.uuid === hot.uuid
        && !(x.affinity && x.affinity.mode === "node"));
      if (!v) break;
      const from = v.nodes.primary.hostname;
      v.nodes = Object.assign({}, v.nodes, {primary: {uuid: cold.uuid, hostname: cold.hostname}});
      v.migration = {state: "completed", instant: true, from, target: cold.hostname,
        reason: "rebalance", queued_at: U().ago(0), completed_at: U().ago(0),
        task_id: lvolMigrationTask(v, cold, "rebalance").uuid};
      counts[hot.uuid]--; counts[cold.uuid]++; moves++;
    }
    U().rollup();
    return {results: [c], message: `${moves} volume(s) moved`};
  }],
  // A backup is always taken from a snapshot. Backing "the volume" up means:
  // take a snapshot now, then back that snapshot up — two independent objects.
  ["POST", /^\/lvols\/([\w-]+)\/backup$/, (m, b) => {
    const v = byId("lvols", m[1]);
    if (!v) return {__404: true};
    const chain = D().snapshots.filter(x => x.lvol_id === v.uuid).sort((a, c) => a.seq - c.seq);
    const prev = chain[chain.length - 1] || null;
    const snap = {uuid: U().uuid(), cluster_id: v.cluster_id, pool_id: v.pool_id, pool_name: v.pool_name,
      lvol_id: v.uuid, lvol_name: v.lvol_name,
      snapshot_name: `${v.lvol_name}-snap-${String(chain.length + 1).padStart(3, "0")}`,
      seq: chain.length + 1, parent_id: prev ? prev.uuid : null,
      created_at: U().ago(0), size: Math.round(v.size_util * .06), status: "online", backup_version_id: null};
    D().snapshots.push(snap);
    const r = appendVersion(snap, b.bucket);
    U().rollup();
    return r.__err ? r : {results: [r]};
  }],
  ["POST", /^\/snapshots\/([\w-]+)\/backup$/, (m, b) => {
    const snap = byId("snapshots", m[1]);
    if (!snap) return {__404: true};
    if (snap.backup_version_id) return {__err: "A backup version has already been taken from this snapshot"};
    const r = appendVersion(snap, b.bucket);
    U().rollup();
    return r.__err ? r : {results: [r]};
  }],
  // Merge a version into its predecessor. Merging the earliest delta grows the
  // full version and removes that delta — this is how retention ages out.
  ["POST", /^\/backups\/([\w-]+)\/versions\/([\w-]+)\/merge$/, m => {
    const bk = byId("backups", m[1]);
    if (!bk) return {__404: true};
    const i = bk.versions.findIndex(x => x.id === m[2]);
    if (i < 0) return {__404: true};
    if (i === 0) return {__err: "The full version has no predecessor to merge into"};
    const prev = bk.versions[i - 1], cur = bk.versions[i];
    prev.size += cur.size;
    prev.merged_count = (prev.merged_count || 0) + 1 + (cur.merged_count || 0);
    prev.created_at = cur.created_at;
    bk.versions.splice(i, 1);
    bk.versions.forEach((x, k) => { x.seq = k + 1; x.type = k === 0 ? "full" : "delta"; });
    bk.last_merge_at = U().ago(0);
    U().rollup();
    return {results: [bk]};
  }],
  ["POST", /^\/backups\/([\w-]+)\/merge$/, m => {
    const bk = byId("backups", m[1]);
    if (!bk) return {__404: true};
    if (bk.versions.length < 2) return {__err: "Nothing to merge — the chain holds a single full version"};
    const full = bk.versions[0], second = bk.versions[1];
    full.size += second.size;
    full.merged_count = (full.merged_count || 0) + 1 + (second.merged_count || 0);
    full.created_at = second.created_at;
    bk.versions.splice(1, 1);
    bk.versions.forEach((x, k) => { x.seq = k + 1; x.type = k === 0 ? "full" : "delta"; });
    bk.last_merge_at = U().ago(0);
    U().rollup();
    return {results: [bk]};
  }],
  ["PUT", /^\/lvols\/([\w-]+)\/backup-policy$/, (m, b) => mut("lvols", m[1], v => {
    const p = byId("backup_policies", b.policy_id);
    v.backup_policy = p ? {uuid: p.uuid, policy_name: p.policy_name} : null;
  })],
  ["POST", /^\/clusters\/([\w-]+)\/backup-policies$/, (m, b) => {
    const c = byId("clusters", m[1]);
    if (!c) return {__404: true};
    const schedule = (b.schedule || []).filter(r => r.interval && Number(r.versions) > 0)
      .map(r => ({interval: r.interval, versions: Number(r.versions), online: Number(r.online || 0)}));
    if (!schedule.length) return {__err: "A policy needs at least one schedule row"};
    const p = {uuid: U().uuid(), cluster_id: c.uuid, policy_name: b.name, schedule, consistency_group: !!b.consistency_group, created_at: U().ago(0)};
    D().backup_policies.push(p); U().rollup(); return {results: [p]};
  }],
  ["PUT", /^\/backup-policies\/([\w-]+)$/, (m, b) => mut("backup_policies", m[1], p => {
    if (b.consistency_group !== undefined) p.consistency_group = !!b.consistency_group;
    if (b.schedule) p.schedule = b.schedule.filter(r => r.interval && Number(r.versions) > 0)
      .map(r => ({interval: r.interval, versions: Number(r.versions), online: Number(r.online || 0)}));
  })],
  ["DELETE", /^\/backup-policies\/([\w-]+)$/, m => {
    const p = byId("backup_policies", m[1]);
    if (!p) return {__404: true};
    const used = D().lvols.filter(v => v.backup_policy && v.backup_policy.uuid === p.uuid).length;
    if (used) return {__err: `${used} volume(s) still use this policy. Detach them first.`};
    const r = drop("backup_policies", m[1]); U().rollup(); return r;
  }],
  ["DELETE", /^\/snapshots\/([\w-]+)$/, m => {
    // deleting the online snapshot leaves any backup version taken from it intact
    const s = byId("snapshots", m[1]);
    if (s) D().snapshots.filter(x => x.parent_id === s.uuid).forEach(x => { x.parent_id = s.parent_id; });
    const r = drop("snapshots", m[1]); U().rollup(); return r;
  }],
  ["POST", /^\/snapshots\/([\w-]+)\/restore$/, (m, b) => {
    const s = byId("snapshots", m[1]);
    if (!s) return {__404: true};
    const target = byId("clusters", b.cluster_id || s.cluster_id);
    if (!target) return {__404: true};
    const pool = D().pools.find(p => p.cluster_id === target.uuid && p.enabled !== false);
    if (!pool) return {__err: "The target cluster has no enabled pool to provision into"};
    const src = byId("lvols", s.lvol_id) || {};
    const v = Object.assign({}, src, {uuid: U().uuid(), lvol_name: b.name || `${s.lvol_name}-restored`,
      cluster_id: target.uuid, pool_id: pool.uuid, pool_name: pool.pool_name,
      status: "online", created_at: U().ago(0), size_util: s.size,
      base_snapshot: {uuid: s.uuid, snapshot_name: s.snapshot_name, lvol_name: s.lvol_name},
      nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`, snapshots_count: 0, backups_count: 0,
      consistency_groups: [], backup_policy: null, replication: null, migration: null});
    D().lvols.push(v); U().rollup(); return {results: [v]};
  }],
  ["POST", /^\/snapshots\/([\w-]+)\/clone$/, (m, b) => {
    const s = byId("snapshots", m[1]);
    if (!s) return {__404: true};
    const src = byId("lvols", s.lvol_id) || {};
    const c = Object.assign({}, src, {uuid: U().uuid(), lvol_name: b.name || s.snapshot_name + "-clone",
      size_util: Math.round(s.size * .05), created_at: U().ago(0), status: "online",
      base_snapshot: {uuid: s.uuid, snapshot_name: s.snapshot_name, lvol_name: s.lvol_name},
      nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`, snapshots_count: 0, backups_count: 0,
      replication: null, migration: null});
    D().lvols.push(c); U().rollup(); return {results: [c]};
  }],
  // Restore rebuilds a volume from the chain up to the chosen version.
  ["POST", /^\/backups\/([\w-]+)\/restore$/, (m, b) => {
    const bk = byId("backups", m[1]);
    if (!bk) return {__404: true};
    const ver = b.version_id ? bk.versions.find(x => x.id === b.version_id) : bk.versions[bk.versions.length - 1];
    if (!ver) return {__err: "Unknown backup version"};
    const src = byId("lvols", bk.lvol_id) || {};
    const upTo = bk.versions.filter(x => x.seq <= ver.seq).reduce((a, x) => a + x.size, 0);
    const v = Object.assign({}, src, {uuid: U().uuid(), lvol_name: b.name || `${bk.lvol_name}-restored`,
      status: "online", created_at: U().ago(0), size_util: upTo, base_snapshot: null,
      nqn: `nqn.2023-02.io.simplyblock:${U().hex(8)}`, snapshots_count: 0, backups_count: 0,
      backup_policy: null, replication: null, migration: null});
    D().lvols.push(v); U().rollup(); return {results: [v]};
  }],
  ["POST", /^\/backups\/([\w-]+)\/export$/, (m, b) => mut("backups", m[1], bk => {
    bk.exported_to = b.destination;
    bk.exported_version = b.version_id || (bk.versions[bk.versions.length - 1] || {}).id;
  })],
  // Deleting a volume backup deletes the whole chain.
  ["DELETE", /^\/backups\/([\w-]+)$/, m => {
    const bk = byId("backups", m[1]);
    if (bk) D().snapshots.filter(s => s.lvol_id === bk.lvol_id).forEach(s => { s.backup_version_id = null; });
    const r = drop("backups", m[1]); U().rollup(); return r;
  }]
];

// The operator mock delegates /proposed mutations here, so the control-plane
// behaviours (validation, state changes) stay in one place.
window.SB_CP_ROUTES = {GET_ROUTES, MUT_ROUTES};

const realFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const url = typeof input === "string" ? input : input.url;
  const base = SB_CONFIG.apiBase;
  if (!SB_CONFIG.mock || !url.startsWith(base)) return realFetch(input, init);

  const method = ((init && init.method) || "GET").toUpperCase();
  const route = url.slice(base.length).split("?")[0].replace(/\/$/, "");
  let body = {};
  try { if (init && init.body) body = JSON.parse(init.body); } catch (e) {}
  SB_MOCK.requests++;
  await wait(SB_MOCK.latency[0] + Math.random() * (SB_MOCK.latency[1] - SB_MOCK.latency[0]));

  if (SB_MOCK.offline) return fail(0, "Control plane unreachable (mock offline)");
  if (SB_MOCK.failNext) { SB_MOCK.failNext = false; return fail(503, "Upstream control plane returned 503 (injected)"); }
  if (SB_MOCK.failRate && Math.random() < SB_MOCK.failRate) return fail(503, "Upstream control plane returned 503");

  if (method === "GET") {
    window.SB_JITTER();
    for (const [re, h] of GET_ROUTES) {
      const m = route.match(re);
      if (m) { const r = h(m); return r.__404 ? fail(404, "Resource not found") : r.__cap ? fail(501, r.__cap) : ok(r); }
    }
  } else {
    for (const [mm, re, h] of MUT_ROUTES) {
      if (mm !== method) continue;
      const m = route.match(re);
      if (m) {
        const r = h(m, body);
        if (r.__404) return fail(404, "Resource not found");
        if (r.__err) return fail(409, r.__err);
        return ok(r);
      }
    }
  }
  return fail(404, `No route for ${method} ${route}`);
};

window.SB_ADD_NODE = addStorageNode;

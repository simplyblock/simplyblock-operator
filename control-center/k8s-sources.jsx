// ---------------------------------------------------------------------------
// SOURCES — the storage view models' inputs, built from real objects
//
// The normalizers in api.jsx consume one record shape per kind (a snake_case
// record, historically the control plane's own). Those records used to come
// from a status field only the fixture backend wrote. Here they are assembled
// from what exists: the CRDs in storage.simplyblock.io, the core objects the
// CSI driver and the operator leave behind (PV, PVC, StorageClass,
// VolumeSnapshot, Node, Pod), and, when the context's control plane is
// reachable, the v2 DTOs that carry live state. Each record also carries
// `ctx`, `ns`, and `__k8s`, the object's Kubernetes identity.
//
// Reads go through the store, so a view that touches six kinds for every
// cluster of every context costs one list per kind per context, not one per
// tile. v2 enrichment is cached a few seconds per call.
// ---------------------------------------------------------------------------
const CTX_ERRORS = {};
const SOURCE_TTL = 4000;

const v2cache = new Map();
function v2cached(key, fn, ttl) {
  const hit = v2cache.get(key);
  if (hit && Date.now() - hit.at < (ttl || SOURCE_TTL)) return hit.p;
  const p = fn().catch(e => { v2cache.delete(key); throw e; });
  v2cache.set(key, {at: Date.now(), p});
  return p;
}
const quiet = (ctx, kind, p) => p.then(items => { if (CTX_ERRORS[ctx]) delete CTX_ERRORS[ctx][kind]; return items; })
  .catch(e => { CTX_ERRORS[ctx] = CTX_ERRORS[ctx] || {}; CTX_ERRORS[ctx][kind] = e; return []; });
const readK = (ctx, kind, opts) => quiet(ctx, kind, store.read(ctx, kind, opts));
const k8sIdentity = o => {
  const m = o.metadata || {}, st = o.status || {};
  return {name: m.name, namespace: m.namespace, kind: o.kind, uid: m.uid, labels: m.labels || {}, annotations: m.annotations || {},
    generation: m.generation, observedGeneration: st.observedGeneration, resourceVersion: m.resourceVersion,
    creationTimestamp: m.creationTimestamp, ownerReferences: m.ownerReferences || [],
    phase: st.phase, conditions: st.conditions || [], activeOpsRef: st.activeOpsRef || null};
};
const lower = s => (s === null || s === undefined ? s : String(s).toLowerCase());
const asIso = t => (t ? (typeof t === "number" ? new Date(t).toISOString() : t) : null);

// ---- the world of one context ------------------------------------------------
// Every storage kind plus the core objects they join to, read together.
async function world(ctx) {
  const c = ctx || LOCAL_CTX;
  const [clusters, sets, nodes, devices, pools, backups, policies, pvs, pvcs, scs, vss, vscs, k8sNodes, namespaces,
    clusterOps, nodeOps, migrations, cps] = await Promise.all([
    readK(c, "StorageCluster"), readK(c, "StorageNodeSet"), readK(c, "StorageNode"), readK(c, "StorageDevice"),
    readK(c, "StoragePool"), readK(c, "StorageBackup"), readK(c, "BackupPolicy"),
    readK(c, "PersistentVolume"), readK(c, "PersistentVolumeClaim", {allNamespaces: true}), readK(c, "StorageClass"),
    readK(c, "VolumeSnapshot", {allNamespaces: true}), readK(c, "VolumeSnapshotContent"),
    readK(c, "Node"), readK(c, "Namespace"),
    readK(c, "StorageClusterOps"), readK(c, "StorageNodeOps"), readK(c, "VolumeMigration"), readK(c, "ControlPlane")
  ]);
  // a hub's projection of a cluster into a transport namespace is the same
  // cluster seen twice; the tenant's own object is the one the console shows
  const own = clusters.filter(x => !((x.metadata || {}).annotations || {})["simplyblock.io/projection-of"]);
  const w = {ctx: c, clusters: own, sets, nodes, devices, pools, backups, policies,
    pvs: pvs.filter(isSimplyblockPV), pvcs, scs: scs.filter(isSimplyblockSC), vss, vscs, k8sNodes, namespaces,
    clusterOps, nodeOps, migrations, controlPlanes: cps};
  // lookups
  w.clusterByName = new Map(clusters.map(x => [nameOf(x), x]));
  w.clusterByUUID = new Map(clusters.filter(x => statusOf(x).uuid).map(x => [statusOf(x).uuid, x]));
  w.setByName = new Map(sets.map(x => [nameOf(x), x]));
  w.nodeByName = new Map(nodes.map(x => [nameOf(x), x]));
  w.nodeByUUID = new Map(nodes.filter(x => statusOf(x).uuid).map(x => [statusOf(x).uuid, x]));
  w.poolByUUID = new Map(pools.filter(x => statusOf(x).uuid).map(x => [statusOf(x).uuid, x]));
  w.k8sNodeByName = new Map(k8sNodes.map(x => [nameOf(x), x]));
  w.nsByName = new Map(namespaces.map(x => [nameOf(x), x]));
  w.scByName = new Map(w.scs.map(x => [nameOf(x), x]));
  w.pvByName = new Map(w.pvs.map(x => [nameOf(x), x]));
  w.pvcByKey = new Map(pvcs.map(x => [`${x.metadata.namespace}/${x.metadata.name}`, x]));
  w.vscByName = new Map(vscs.map(x => [nameOf(x), x]));
  // cluster of a node, through its set
  w.clusterOfNode = n => {
    const set = w.setByName.get(specOf(n).storageNodeSetRef) || w.setByName.get(labelsOf(n)[LABELS.nodeSet]);
    return set ? w.clusterByName.get(specOf(set).clusterName) : (statusOf(n).clusterID ? w.clusterByUUID.get(statusOf(n).clusterID) : null);
  };
  w.poolOf = (sc, cluster) => {
    if (!sc) return null;
    const pname = (sc.parameters || {}).pool_name || labelsOf(sc)[LABELS.scPool];
    const cname = labelsOf(sc)[LABELS.scCluster] || (cluster ? nameOf(cluster) : null);
    return pools.find(p => nameOf(p) === pname && (!cname || specOf(p).clusterName === cname)) || null;
  };
  return w;
}
// the volume handle of a PV, completed from its StorageClass when the handle
// is the legacy short form
function resolvePvHandle(w, pv) {
  const h = pvHandle(pv);
  const sc = w.scByName.get(specOf(pv).storageClassName);
  if (!h.clusterUUID) h.clusterUUID = (sc && (sc.parameters || {}).cluster_id) || (w.clusters.length === 1 ? statusOf(w.clusters[0]).uuid : null);
  if (!h.poolUUID) {
    const pool = w.poolOf(sc, w.clusterByUUID.get(h.clusterUUID)) || (h.poolName ? w.pools.find(p => nameOf(p) === h.poolName) : null);
    if (pool) { h.poolUUID = statusOf(pool).uuid || null; h.poolName = nameOf(pool); }
  } else if (!h.poolName) {
    const pool = w.poolByUUID.get(h.poolUUID);
    if (pool) h.poolName = nameOf(pool);
  }
  return h;
}

// ---- v2 enrichment ---------------------------------------------------------
const v2 = {
  clusters: ctx => hasControlPlane(ctx) ? v2cached(`${ctx}|clusters`, () => cp.clusters(ctx)).catch(() => []) : Promise.resolve([]),
  nodes: (ctx, cid) => hasControlPlane(ctx) && cid ? v2cached(`${ctx}|nodes|${cid}`, () => cp.nodes(ctx, cid)).catch(() => []) : Promise.resolve([]),
  devices: (ctx, cid, nid) => hasControlPlane(ctx) && cid && nid ? v2cached(`${ctx}|devices|${cid}|${nid}`, () => cp.devices(ctx, cid, nid)).catch(() => []) : Promise.resolve([]),
  pools: (ctx, cid) => hasControlPlane(ctx) && cid ? v2cached(`${ctx}|pools|${cid}`, () => cp.pools(ctx, cid)).catch(() => []) : Promise.resolve([]),
  volumes: (ctx, cid) => hasControlPlane(ctx) && cid ? v2cached(`${ctx}|volumes|${cid}`, () => cp.clusterVolumes(ctx, cid), 8000).catch(() => []) : Promise.resolve([]),
  snapshots: (ctx, cid) => hasControlPlane(ctx) && cid ? v2cached(`${ctx}|snapshots|${cid}`, () => cp.clusterSnapshots(ctx, cid), 8000).catch(() => []) : Promise.resolve([])
};

// ---- records -----------------------------------------------------------------
function clusterRecord(w, c, live) {
  const st = statusOf(c), sp = specOf(c);
  const sets = nodeSetsOf(c, w.sets), nodes = nodesOfSets(sets, w.nodes), devices = devicesOfCluster(c, w.devices), pools = poolsOf(c, w.pools);
  const uuid = st.uuid || null;
  const pvs = uuid ? w.pvs.filter(pv => resolvePvHandle(w, pv).clusterUUID === uuid) : [];
  const vscs = uuid ? w.vscs.filter(x => snapHandle(x).clusterUUID === uuid) : [];
  const backups = w.backups.filter(b => specOf(b).clusterName === nameOf(c));
  const policies = w.policies.filter(p => specOf(p).clusterName === nameOf(c));
  const workers = workersOf(sets, nodes, w.k8sNodes);
  const online = s => s === "online";
  const nodeStatus = n => lower(statusOf(n).status);
  const devStatus = d => lower(statusOf(d).deviceStatus || statusOf(d).phase);
  const capFromNodes = nodes.reduce((a, n) => { const cap = (statusOf(n).resources || {}).capacity || {}; return {t: a.t + (Number(cap.totalBytes) || 0), u: a.u + (Number(cap.usedBytes) || 0)}; }, {t: 0, u: 0});
  const liveCap = live && live.capacity ? live.capacity : null;
  const sizeTotal = liveCap && liveCap.total !== null ? liveCap.total : capFromNodes.t;
  const sizeUsed = liveCap && liveCap.used !== null ? liveCap.used : capFromNodes.u;
  const env = {ns: w.nsByName.get(c.metadata.namespace), ctx: w.ctx, workers};
  const sites = siteOf(c, env), stage = stageOf(c, env), tenant = tenantOf(c, env);
  const ops = w.clusterOps.filter(o => specOf(o).clusterRef === nameOf(c));
  return {
    uuid, name: nameOf(c), ctx: w.ctx, ns: c.metadata.namespace, __k8s: k8sIdentity(c),
    labels: labelsOf(c), sites, stage, tenant, stretched: sites.length > 1,
    status: lower(live ? live.status : null) || lower(st.status) || (st.phase ? lower(st.phase) : "unready"),
    phase: st.phase || null, sub_phase: st.subPhase || null, configured: !!st.configured,
    rebalancing: !!(live ? live.rebalancing : st.rebalancing),
    device_class: "nvme", location_type: labelsOf(c)["storage.simplyblock.io/edge"] === "true" ? "edge" : "datacenter",
    size_total: sizeTotal, size_util: sizeUsed, size_prov: liveCap ? liveCap.provisioned : null,
    capacity_approx: !liveCap,
    hosts_count: workers.length, hosts_available: workers.filter(isPreparedWorker).length,
    storage_nodes_count: nodes.length, storage_nodes_online: nodes.filter(n => online(nodeStatus(n))).length,
    nodes_planned_offline: nodes.filter(n => ["in_shutdown", "in_removal", "pending_removal", "removed"].includes(nodeStatus(n))).length,
    devices_count: devices.length, devices_online: devices.filter(d => online(devStatus(d))).length,
    pools_count: pools.length, lvols_count: pvs.length, snapshots_count: vscs.length,
    backups_count: backups.length, backup_policies_count: policies.length,
    k8s_cluster_ids: [w.ctx], pvcs_count: pvs.filter(pv => specOf(pv).claimRef).length,
    capabilities: {tasks: hasControlPlane(w.ctx), rebalancing: true, async_replication: true, event_log: hasControlPlane(w.ctx)},
    distr_ndcs: (sp.stripe || {}).dataChunks, distr_npcs: (sp.stripe || {}).parityChunks,
    ha_type: live ? (live.ha ? "ha" : "single") : null,
    max_fault_tolerance: live ? live.maxFaultTolerance : st.maxFaultTolerance,
    fault_budget: {kind: "nodes", tolerated: (live ? live.maxFaultTolerance : st.maxFaultTolerance) || (sp.stripe || {}).parityChunks || 1,
      lost: nodes.length - nodes.filter(n => online(nodeStatus(n))).length},
    failure_domain_enabled: !!sp.enableFailureDomains, node_affinity: sp.enableNodeAffinity ? "node" : "none",
    thresholds: {warn: sp.warningThreshold || {}, crit: sp.criticalThreshold || {}},
    backup_enabled: !!(sp.backup && sp.backup.localEndpoint) || !!(live && live.backupEnabled),
    kms: sp.hashicorpVaultSettings && sp.hashicorpVaultSettings.baseURL ? {provider: "vault", endpoint: sp.hashicorpVaultSettings.baseURL} : null,
    auto_rebalance: {enabled: !!((sp.volumeAutoPlacement || {}).enabled)},
    mgmt_endpoint: hasControlPlane(w.ctx) ? ctxByName(w.ctx).cpBase : null, mgmt_endpoint_kind: "control plane API",
    nqn: st.nqn || (live ? live.nqn : null), erasure_coding: st.erasureCodingScheme || null,
    active_ops_ref: st.activeOpsRef || null, active_ops: ops.filter(o => !["Succeeded", "Failed", "Aborted"].includes(statusOf(o).phase)).map(nameOf),
    worker_names: workers.map(nameOf), node_uuids: nodes.map(n => statusOf(n).uuid).filter(Boolean),
    created_at: c.metadata.creationTimestamp, live: live || null
  };
}

function nodeRecord(w, n, cluster, live) {
  const st = statusOf(n), sp = specOf(n), res = st.resources || {}, ports = st.ports || {};
  const cap = res.capacity || {};
  const devices = devicesOfNode(n, w.devices);
  const worker = w.k8sNodeByName.get(sp.workerNode) || null;
  const op = w.nodeOps.find(o => specOf(o).storageNodeRef === nameOf(n) && !["Succeeded", "Failed", "Aborted"].includes(statusOf(o).phase)) || null;
  const mgmt = String(ports.management || "");
  return {
    uuid: st.uuid || null, name: nameOf(n), ctx: w.ctx, ns: n.metadata.namespace, __k8s: k8sIdentity(n),
    cluster_id: cluster ? statusOf(cluster).uuid : (st.clusterID || null), cluster_name: cluster ? nameOf(cluster) : null,
    host_id: worker ? worker.metadata.uid : null, worker_node: sp.workerNode || null, hostname: st.hostname || sp.workerNode,
    set_name: sp.storageNodeSetRef || null, socket: sp.socketId !== undefined ? sp.socketId : null, node_index: sp.nodeIndex,
    zone: worker ? (labelsOf(worker)[LABELS.zone] || null) : null,
    status: lower(live ? live.status : null) || lower(st.status) || "unknown", health_check: live ? live.health : (st.health === undefined ? null : !!st.health),
    mgmt_ip: mgmt.split(":")[0] || (live ? live.mgmtIp : null), rpc_port: ports.rpc || (live ? live.ports.rpc : null), nvmf_port: ports.nvmeof || null, lvol_port: ports.lvol || null,
    data_nics: [], failure_domain: st.failureDomain, uptime: st.uptime || (live ? live.uptime : null),
    size_total: Number(cap.totalBytes) || (live && live.capacity.total) || 0, size_util: Number(cap.usedBytes) || (live && live.capacity.used) || 0,
    devices_count: devices.length || (live ? live.devices.count : 0), devices_online: devices.filter(d => lower(statusOf(d).deviceStatus || statusOf(d).phase) === "online").length,
    cpu_count: res.cpu || (live ? live.cpu.total : null), memory_total: parseQuantity(res.memory) || (live ? live.memory.total : null),
    hugepages_total: live ? live.memory.hugepages : null, max_subsystem_count: live ? live.volumes.max : null,
    lvols_count: res.volumes !== undefined ? res.volumes : (live ? live.volumes.count : null),
    latency: st.latencyMetrics || null, posted_at: st.postedAt || null,
    active_ops_ref: st.activeOpsRef || null,
    op: op ? {kind: specOf(op).action, phase: statusOf(op).subPhase || statusOf(op).phase, phases: [], phase_index: 0,
      volumes_moved: statusOf(op).volumesMigrated || 0, volumes_pending: statusOf(op).volumesPending || 0,
      target_hostname: specOf(op).targetWorkerNode || null, started_at: statusOf(op).startedAt, ops_name: nameOf(op)} : null,
    created_at: n.metadata.creationTimestamp, live: live || null
  };
}

function deviceRecord(w, d, node, live) {
  const st = statusOf(d), sp = specOf(d), hw = st.hardware || {}, cap = st.capacity || {};
  const worker = node ? w.k8sNodeByName.get(specOf(node).workerNode) : null;
  return {
    uuid: sp.deviceID || st.deviceID || d.metadata.uid, name: nameOf(d), ctx: w.ctx, ns: d.metadata.namespace, __k8s: k8sIdentity(d),
    node_id: st.nodeID || (node ? statusOf(node).uuid : null), node_name: sp.nodeRef || null,
    cluster_id: st.clusterID || (node ? (w.clusterOfNode(node) ? statusOf(w.clusterOfNode(node)).uuid : null) : null),
    host_id: worker ? worker.metadata.uid : null, cluster_device_class: "nvme", numa_socket: node ? specOf(node).socketId : null,
    serial_number: hw.serialNumber || (live ? live.serial : null), pcie_address: hw.pciAddress || (live ? live.pcie : null),
    model_number: hw.model || (live ? live.model : null), nvme_controller: hw.nvmeController || null, firmware_revision: null,
    role: st.role || null, phase: st.phase || null,
    status: lower(live ? live.status : null) || lower(st.deviceStatus) || lower(st.phase) || "unknown",
    health_check: live ? live.health : null, io_error: live ? live.ioError : null, retries_exhausted: live ? live.retriesExhausted : null,
    size_total: Number(cap.totalBytes) || (live ? live.size : 0) || 0, size_util: Number(cap.usedBytes) || (live && live.capacity.used) || 0,
    message: st.message || null, observed_generation: st.observedGeneration,
    created_at: d.metadata.creationTimestamp, live: live || null
  };
}

function poolRecord(w, p, cluster, live) {
  const st = statusOf(p), sp = specOf(p), q = sp.qos || {}, th = q.throughput || {};
  const uuid = st.uuid || null;
  const scs = w.scs.filter(sc => w.poolOf(sc, cluster) === p);
  const pvs = w.pvs.filter(pv => { const h = resolvePvHandle(w, pv); return (uuid && h.poolUUID === uuid) || h.poolName === nameOf(p); });
  const vscs = uuid ? w.vscs.filter(x => snapHandle(x).poolUUID === uuid) : [];
  const backups = w.backups.filter(b => statusOf(b).poolUUID === uuid || statusOf(b).poolName === nameOf(p));
  const used = pvs.reduce((n, pv) => n + (parseQuantity((specOf(pv).capacity || {}).storage) || 0), 0);
  return {
    uuid, pool_name: nameOf(p), name: nameOf(p), ctx: w.ctx, ns: p.metadata.namespace, __k8s: k8sIdentity(p),
    cluster_id: cluster ? statusOf(cluster).uuid : null, cluster_name: sp.clusterName,
    dhchap_bidirectional: !!sp.dhchap, enabled: lower(sp.status) !== "disabled" && lower(st.status) !== "disabled",
    qos: {rw_iops: q.iops || null, rw_mbytes: th.readWrite || null, r_mbytes: th.read || null, w_mbytes: th.write || null},
    size_prov: parseQuantity(sp.capacityLimit) || (live && live.capacity.total) || 0, size_util: (live && live.capacity.used) || used,
    lvols_bytes: used, lvol_max_size: parseQuantity(sp.logicalVolumeMaxSize) || null,
    storage_classes: scs.map(sc => ({uuid: sc.metadata.uid, name: nameOf(sc), k8s_cluster: w.ctx, variant: "default"})),
    k8s_cluster_id: w.ctx, allowed_nodes: sp.allowedNodes || [], storage_class_parameters: sp.storageClassParameters || {},
    lvols_count: pvs.length, lvols_online: pvs.filter(pv => statusOf(pv).phase === "Bound").length,
    snapshots_count: vscs.length, backups_count: backups.length, storage_classes_count: scs.length,
    created_at: p.metadata.creationTimestamp, live: live || null
  };
}

function volumeRecord(w, pv, liveByHandle) {
  const sp = specOf(pv), st = statusOf(pv), csi = sp.csi || {}, attrs = csi.volumeAttributes || {};
  const h = resolvePvHandle(w, pv);
  const cluster = h.clusterUUID ? w.clusterByUUID.get(h.clusterUUID) : null;
  const pool = h.poolUUID ? w.poolByUUID.get(h.poolUUID) : null;
  const claim = sp.claimRef ? w.pvcByKey.get(`${sp.claimRef.namespace}/${sp.claimRef.name}`) : null;
  const sc = w.scByName.get(sp.storageClassName) || null;
  const live = (liveByHandle && liveByHandle.get(`${h.clusterUUID}:${h.poolUUID}:${h.id}`)) || null;
  const nodeUUID = (live && live.nodeId) || (claim && (claim.metadata.annotations || {})[LABELS.selectedNode]) || attrs.storage_node_id || null;
  const node = nodeUUID ? w.nodeByUUID.get(nodeUUID) : null;
  const ann = (claim && claim.metadata.annotations) || {};
  const vss = claim ? w.vss.filter(s => s.metadata.namespace === claim.metadata.namespace && (specOf(s).source || {}).persistentVolumeClaimName === claim.metadata.name) : [];
  const backups = claim ? w.backups.filter(b => (specOf(b).pvcRef || {}).name === claim.metadata.name && ((specOf(b).pvcRef || {}).namespace || b.metadata.namespace) === claim.metadata.namespace) : [];
  const size = parseQuantity((sp.capacity || {}).storage) || (live ? live.size : 0) || 0;
  return {
    uuid: h.id, ctx: w.ctx, ns: claim ? claim.metadata.namespace : null, __k8s: k8sIdentity(pv), pv_name: nameOf(pv), handle: csi.volumeHandle,
    pool_id: h.poolUUID, pool_name: h.poolName || (pool ? nameOf(pool) : (sc ? (sc.parameters || {}).pool_name : null)),
    cluster_id: h.clusterUUID, cluster_name: cluster ? nameOf(cluster) : null,
    lvol_name: (live && live.name) || nameOf(pv), status: lower(live ? live.status : null) || (st.phase === "Bound" ? "online" : lower(st.phase) || "unknown"),
    nodes: nodeUUID ? {primary: {uuid: nodeUUID, hostname: node ? (statusOf(node).hostname || specOf(node).workerNode) : null}} : {},
    size_prov: size, size_util: live && live.capacity.used !== null ? live.capacity.used : null,
    crypto_enabled: String(attrs.encryption || (sc && (sc.parameters || {}).encryption) || "").toLowerCase() === "true" || !!(live && live.crypto),
    qos: {rw_iops: Number(attrs.qos_rw_iops) || (live && live.qos.rwIops) || 0, rw_mbytes: Number(attrs.qos_rw_mbytes) || (live && live.qos.rwMbps) || 0,
      r_mbytes: Number(attrs.qos_r_mbytes) || (live && live.qos.rMbps) || 0, w_mbytes: Number(attrs.qos_w_mbytes) || (live && live.qos.wMbps) || 0},
    nqn: (live && live.nqn) || attrs.nqn || null, fs_type: csi.fsType || null,
    pvc: claim ? {uuid: claim.metadata.uid, namespace: claim.metadata.namespace, name: claim.metadata.name, k8s_cluster: w.ctx,
      storage_class: sp.storageClassName, workload: null} : null,
    storage_class: sp.storageClassName || null, storage_class_id: sc ? sc.metadata.uid : null,
    backup_policy: ann[LABELS.backupPolicy] || ann["simplybk/backup-policy"] || null,
    replication_policy: ann[LABELS.replicationPolicy] || null,
    migrating: !!(live && live.migrating),
    snapshots_count: vss.length, backups_count: backups.length,
    created_at: pv.metadata.creationTimestamp, live: live || null
  };
}

function snapshotRecord(w, vs) {
  const sp = specOf(vs), st = statusOf(vs);
  const vsc = st.boundVolumeSnapshotContentName ? w.vscByName.get(st.boundVolumeSnapshotContentName) : null;
  const h = vsc ? snapHandle(vsc) : parseHandle(null);
  const pvcName = (sp.source || {}).persistentVolumeClaimName || null;
  const claim = pvcName ? w.pvcByKey.get(`${vs.metadata.namespace}/${pvcName}`) : null;
  const pv = claim && specOf(claim).volumeName ? w.pvByName.get(specOf(claim).volumeName) : null;
  const vh = pv ? resolvePvHandle(w, pv) : null;
  const clusterUUID = h.clusterUUID || (vh ? vh.clusterUUID : null);
  const poolUUID = h.poolUUID || (vh ? vh.poolUUID : null);
  const pool = poolUUID ? w.poolByUUID.get(poolUUID) : null;
  return {
    uuid: h.id || vs.metadata.uid, ctx: w.ctx, ns: vs.metadata.namespace, __k8s: k8sIdentity(vs),
    cluster_id: clusterUUID, pool_id: poolUUID, pool_name: pool ? nameOf(pool) : (vh ? vh.poolName : null),
    lvol_id: vh ? vh.id : null, lvol_name: pv ? nameOf(pv) : pvcName, pvc_name: pvcName,
    snapshot_name: nameOf(vs), content_name: vsc ? nameOf(vsc) : null, handle: vsc ? statusOf(vsc).snapshotHandle : null,
    status: st.readyToUse ? "online" : (st.error ? "error" : "in_creation"), error: st.error || null,
    size: parseQuantity(st.restoreSize) || 0, created_at: st.creationTime || vs.metadata.creationTimestamp,
    snapshot_class: sp.volumeSnapshotClassName || null
  };
}

function backupRecord(w, b) {
  const sp = specOf(b), st = statusOf(b);
  const pvcRef = sp.pvcRef || {};
  return {
    uuid: st.backupID || b.metadata.uid, ctx: w.ctx, ns: b.metadata.namespace, __k8s: k8sIdentity(b), name: nameOf(b),
    cluster_id: st.clusterUUID || null, cluster_name: sp.clusterName, pool_id: st.poolUUID || null, pool_name: st.poolName || null,
    lvol_id: st.lvolID || null, lvol_name: st.lvolName || pvcRef.name || nameOf(b), chain_id: st.prevBackupID ? null : (st.backupID || null),
    prev_backup_id: st.prevBackupID || null, policy_name: (b.metadata.labels || {})["storage.simplyblock.io/backup-policy"] || null,
    status: lower(st.phase) === "done" ? "online" : lower(st.phase) || "pending", phase: st.phase || null, message: st.message || null,
    bucket: st.s3ID || null, snapshot_id: st.snapshotID || null, snapshot_name: st.snapshotName || null, node_id: st.nodeID || null,
    pvc: {namespace: st.pvcNamespace || pvcRef.namespace || b.metadata.namespace, name: pvcRef.name}, pv_name: st.pvName || null, fs_type: st.fsType || null,
    source_cluster_id: st.sourceClusterUUID || null, size: Number(st.size) || 0,
    versions: [{id: st.backupID || b.metadata.uid, seq: 1, tier: "s3", type: st.prevBackupID ? "delta" : "full", created_at: st.createdAt || b.metadata.creationTimestamp,
      size: Number(st.size) || 0, source_snapshot_id: st.snapshotID || null, source_snapshot_name: st.snapshotName || null}],
    versions_count: 1, earliest_at: st.createdAt || b.metadata.creationTimestamp, latest_at: st.completedAt || st.createdAt || null,
    created_at: b.metadata.creationTimestamp
  };
}

function policyRecord(w, p, cluster) {
  const sp = specOf(p), st = statusOf(p);
  const schedule = String(sp.schedule || "").trim().split(/\s+/).filter(Boolean).map(s => { const [interval, versions] = s.split(","); return {interval, versions: Number(versions) || 0}; });
  const attached = st.attachedLvols || [];
  return {
    uuid: st.policyID || p.metadata.uid, ctx: w.ctx, ns: p.metadata.namespace, __k8s: k8sIdentity(p),
    policy_name: nameOf(p), cluster_id: cluster ? statusOf(cluster).uuid : (st.clusterUUID || null), cluster_name: sp.clusterName,
    status: lower(st.phase) || "pending", phase: st.phase || null, message: st.message || null,
    schedule, max_versions: sp.maxVersions, max_age: sp.maxAge || null,
    lvols_count: attached.length, attached: attached, chains_count: attached.length,
    finest_interval: schedule.length ? schedule[0].interval : null, created_at: p.metadata.creationTimestamp
  };
}

function hostRecord(w, node) {
  const st = statusOf(node), labels = labelsOf(node), cap = st.capacity || {};
  const nodeRefs = workerLabelNodeUUIDs(node);
  const sns = w.nodes.filter(n => specOf(n).workerNode === nameOf(node));
  const clusterIds = Array.from(new Set(nodeRefs.map(r => r.clusterUUID).concat(sns.map(n => { const c = w.clusterOfNode(n); return c ? statusOf(c).uuid : null; })).filter(Boolean)));
  const prepared = isPreparedWorker(node) || sns.length > 0;
  return {
    uuid: node.metadata.uid, ctx: w.ctx, __k8s: k8sIdentity(node), hostname: nameOf(node),
    cluster_id: clusterIds[0] || null, cluster_ids: clusterIds, k8s_cluster: w.ctx,
    mgmt_ip: nodeAddress(node, "InternalIP"), zone: labels[LABELS.zone] || null, region: labels[LABELS.region] || null,
    status: !nodeReady(node) ? "unavailable" : prepared ? "available" : "discovered", source: "kubernetes",
    numa_sockets: nodeRefs.length ? new Set(nodeRefs.map(r => r.socket)).size : (sns.length ? new Set(sns.map(n => specOf(n).socketId)).size : null),
    kubelet_version: (st.nodeInfo || {}).kubeletVersion || null, os_image: (st.nodeInfo || {}).osImage || null,
    roles: Object.keys(labels).filter(k => k.startsWith("node-role.kubernetes.io/")).map(k => k.split("/")[1]),
    k8s_labels: labels, vcpu_count: parseQuantity(cap.cpu), memory_total: parseQuantity(cap.memory),
    storage_node_ids: nodeRefs.map(r => r.nodeUUID).concat(sns.map(n => statusOf(n).uuid)).filter((v, i, a) => v && a.indexOf(v) === i),
    inspection: null, devices: [], control_plane: !!labels["node-role.kubernetes.io/control-plane"] && !prepared,
    size_total: sns.reduce((n, s) => n + (Number(((statusOf(s).resources || {}).capacity || {}).totalBytes) || 0), 0),
    size_assigned: sns.reduce((n, s) => n + (Number(((statusOf(s).resources || {}).capacity || {}).usedBytes) || 0), 0),
    created_at: node.metadata.creationTimestamp
  };
}

function storageClassRecord(w, sc) {
  const params = sc.parameters || {}, labels = labelsOf(sc);
  const cluster = labels[LABELS.scCluster] ? w.clusterByName.get(labels[LABELS.scCluster]) : (params.cluster_id ? w.clusterByUUID.get(params.cluster_id) : null);
  const pool = w.poolOf(sc, cluster);
  const pvcs = w.pvcs.filter(p => specOf(p).storageClassName === nameOf(sc));
  const provisioned = pvcs.reduce((n, p) => n + (parseQuantity(((statusOf(p).capacity || {}).storage) || ((specOf(p).resources || {}).requests || {}).storage) || 0), 0);
  return {
    uuid: sc.metadata.uid, ctx: w.ctx, __k8s: k8sIdentity(sc), k8s_cluster_id: w.ctx, name: nameOf(sc), provisioner: sc.provisioner,
    cluster_id: params.cluster_id || (cluster ? statusOf(cluster).uuid : null), cluster_name: cluster ? nameOf(cluster) : (labels[LABELS.scCluster] || null),
    pool_id: pool ? statusOf(pool).uuid : null, pool_name: params.pool_name || labels[LABELS.scPool] || null,
    operator_namespace: labels[LABELS.scNamespace] || null, parameters: params,
    dhchap: !!params.dhchap_node_selector, zone_cluster_map: safeJson(params.zone_cluster_map), region_cluster_map: safeJson(params.region_cluster_map),
    reclaim_policy: sc.reclaimPolicy, volume_binding_mode: sc.volumeBindingMode, allow_volume_expansion: !!sc.allowVolumeExpansion,
    is_default: ((sc.metadata.annotations || {})["storageclass.kubernetes.io/is-default-class"] === "true"),
    pvcs_count: pvcs.length, bound_count: pvcs.filter(p => statusOf(p).phase === "Bound").length, provisioned_bytes: provisioned,
    created_at: sc.metadata.creationTimestamp
  };
}
const safeJson = s => { if (!s) return {}; try { return JSON.parse(s); } catch (e) { return {}; } };

function pvcRecord(w, pvc) {
  const sp = specOf(pvc), st = statusOf(pvc);
  const sc = w.scByName.get(sp.storageClassName) || null;
  const pv = sp.volumeName ? w.pvByName.get(sp.volumeName) : null;
  const h = pv ? resolvePvHandle(w, pv) : null;
  const ann = pvc.metadata.annotations || {};
  return {
    uuid: pvc.metadata.uid, ctx: w.ctx, ns: pvc.metadata.namespace, __k8s: k8sIdentity(pvc), k8s_cluster_id: w.ctx,
    namespace: pvc.metadata.namespace, pvc_name: nameOf(pvc), storage_class: sp.storageClassName || null, storage_class_id: sc ? sc.metadata.uid : null,
    lvol_id: h ? h.id : null, cluster_id: h ? h.clusterUUID : (sc ? (sc.parameters || {}).cluster_id : null), pool_id: h ? h.poolUUID : null,
    pv_name: sp.volumeName || null, status: st.phase || "Pending", access_mode: (sp.accessModes || [])[0] || null, volume_mode: sp.volumeMode || "Filesystem",
    filesystem: ann[LABELS.onDiskFs] || (pv ? (specOf(pv).csi || {}).fsType : null) || null,
    workload: null, workload_kind: null, annotations: ann, labels: pvc.metadata.labels || {},
    data_source: sp.dataSource || sp.dataSourceRef || null,
    backup_policy: ann[LABELS.backupPolicy] || ann["simplybk/backup-policy"] || null, replication_policy: ann[LABELS.replicationPolicy] || null,
    actual_bytes: parseQuantity((st.capacity || {}).storage) || 0, requested_bytes: parseQuantity(((sp.resources || {}).requests || {}).storage) || 0,
    created_at: pvc.metadata.creationTimestamp
  };
}

function k8sClusterRecord(ctx, w, version) {
  const c = ctxByName(ctx) || {};
  const workers = w.k8sNodes.filter(n => !labelsOf(n)["node-role.kubernetes.io/control-plane"] && !labelsOf(n)["node-role.kubernetes.io/master"]);
  const cp = w.controlPlanes[0] || null;
  return {
    uuid: ctx, ctx, name: ctx, version: version ? version.gitVersion : null, status: version ? "online" : (CTX_ERRORS[ctx] && Object.keys(CTX_ERRORS[ctx]).length ? "unreachable" : "online"),
    environment: (c.labels || {}).stage || null, labels: c.labels || {}, local: !!c.local,
    api_endpoint: null, operator_namespace: cp ? cp.metadata.namespace : (Array.isArray(c.namespaces) ? c.namespaces[0] : null),
    csi_version: null, csi_status: w.scs.length ? "installed" : "unknown", discovered: true,
    control_plane: cp ? {phase: statusOf(cp).phase, message: statusOf(cp).message, reachable: hasControlPlane(ctx)} : null,
    storage_cluster_ids: w.clusters.map(x => statusOf(x).uuid).filter(Boolean), namespaces: Array.from(new Set(w.clusters.map(x => x.metadata.namespace))),
    storage_classes_count: w.scs.length, pvcs_count: w.pvcs.filter(p => w.scByName.has(specOf(p).storageClassName)).length,
    pvcs_bound: w.pvcs.filter(p => w.scByName.has(specOf(p).storageClassName) && statusOf(p).phase === "Bound").length,
    worker_nodes_count: workers.length, prepared_hosts_count: workers.filter(isPreparedWorker).length,
    provisioned_bytes: w.pvs.reduce((n, pv) => n + (parseQuantity((specOf(pv).capacity || {}).storage) || 0), 0),
    created_at: null
  };
}

// ---- the sources --------------------------------------------------------------
const allCtx = () => CONTEXTS.map(c => c.name);
const inCtx = async (ctxs, fn) => (await Promise.all(ctxs.map(ctx => fn(ctx).catch(e => { CTX_ERRORS[ctx] = CTX_ERRORS[ctx] || {}; CTX_ERRORS[ctx].world = e; return []; })))).flat();
const liveById = list => new Map((list || []).map(x => [x.id, x]));

const sources = {
  world,
  errors: () => CTX_ERRORS,
  clusters: async ctx => {
    const w = await world(ctx), live = liveById(await v2.clusters(ctx));
    return w.clusters.map(c => clusterRecord(w, c, live.get(statusOf(c).uuid) || null));
  },
  allClusters: () => inCtx(allCtx(), sources.clusters),
  cluster: async (ctx, id) => (await sources.clusters(ctx)).find(c => c.uuid === id || c.name === id) || null,
  nodes: async (ctx, clusterId) => {
    const w = await world(ctx);
    const cluster = clusterId ? w.clusterByUUID.get(clusterId) || w.clusterByName.get(clusterId) : null;
    const nodes = cluster ? nodesOf(cluster, w.sets, w.nodes) : w.nodes;
    const live = liveById(cluster ? await v2.nodes(ctx, statusOf(cluster).uuid) : []);
    return nodes.map(n => nodeRecord(w, n, cluster || w.clusterOfNode(n), live.get(statusOf(n).uuid) || null));
  },
  node: async (ctx, id) => (await sources.nodes(ctx)).find(n => n.uuid === id || n.name === id) || null,
  devices: async (ctx, nodeId) => {
    const w = await world(ctx);
    const node = nodeId ? w.nodeByUUID.get(nodeId) || w.nodeByName.get(nodeId) : null;
    const devices = node ? devicesOfNode(node, w.devices) : w.devices;
    const cluster = node ? w.clusterOfNode(node) : null;
    const live = liveById(node && cluster ? await v2.devices(ctx, statusOf(cluster).uuid, statusOf(node).uuid) : []);
    return devices.map(d => deviceRecord(w, d, node || w.nodeByName.get(specOf(d).nodeRef) || null, live.get(specOf(d).deviceID) || null));
  },
  device: async (ctx, id) => (await sources.devices(ctx)).find(d => d.uuid === id || d.name === id) || null,
  pools: async (ctx, clusterId) => {
    const w = await world(ctx);
    const cluster = clusterId ? w.clusterByUUID.get(clusterId) || w.clusterByName.get(clusterId) : null;
    const pools = cluster ? poolsOf(cluster, w.pools) : w.pools;
    const live = liveById(cluster ? await v2.pools(ctx, statusOf(cluster).uuid) : []);
    return pools.map(p => poolRecord(w, p, cluster || w.clusterByName.get(specOf(p).clusterName) || null, live.get(statusOf(p).uuid) || null));
  },
  pool: async (ctx, id) => (await sources.pools(ctx)).find(p => p.uuid === id || p.name === id) || null,
  volumes: async (ctx, scope) => {
    const s = scope || {};
    const w = await world(ctx);
    let pvs = w.pvs.map(pv => ({pv, h: resolvePvHandle(w, pv)}));
    if (s.clusterId) pvs = pvs.filter(x => x.h.clusterUUID === s.clusterId);
    if (s.poolId) pvs = pvs.filter(x => x.h.poolUUID === s.poolId || x.h.poolName === s.poolId);
    if (s.nodeId) { /* resolved after enrichment */ }
    const clusterIds = Array.from(new Set(pvs.map(x => x.h.clusterUUID).filter(Boolean)));
    const liveLists = await Promise.all(clusterIds.map(cid => v2.volumes(ctx, cid)));
    const liveByHandle = new Map(liveLists.flat().map(v => [v.handle, v]));
    let out = pvs.map(x => volumeRecord(w, x.pv, liveByHandle));
    if (s.nodeId) out = out.filter(v => v.nodes.primary && v.nodes.primary.uuid === s.nodeId);
    return out;
  },
  volume: async (ctx, id) => (await sources.volumes(ctx)).find(v => v.uuid === id || v.pv_name === id) || null,
  snapshots: async (ctx, scope) => {
    const s = scope || {};
    const w = await world(ctx);
    let out = w.vss.map(vs => snapshotRecord(w, vs));
    if (s.clusterId) out = out.filter(x => x.cluster_id === s.clusterId);
    if (s.poolId) out = out.filter(x => x.pool_id === s.poolId);
    if (s.volumeId) out = out.filter(x => x.lvol_id === s.volumeId);
    return out;
  },
  snapshot: async (ctx, id) => (await sources.snapshots(ctx)).find(x => x.uuid === id || x.snapshot_name === id) || null,
  backups: async (ctx, scope) => {
    const s = scope || {};
    const w = await world(ctx);
    let out = w.backups.map(b => backupRecord(w, b));
    if (s.clusterId) out = out.filter(x => x.cluster_id === s.clusterId || (w.clusterByUUID.get(s.clusterId) && x.cluster_name === nameOf(w.clusterByUUID.get(s.clusterId))));
    if (s.poolId) out = out.filter(x => x.pool_id === s.poolId);
    if (s.volumeId) out = out.filter(x => x.lvol_id === s.volumeId);
    return out;
  },
  backup: async (ctx, id) => (await sources.backups(ctx)).find(x => x.uuid === id || x.name === id) || null,
  policies: async (ctx, clusterId) => {
    const w = await world(ctx);
    const cluster = clusterId ? w.clusterByUUID.get(clusterId) || w.clusterByName.get(clusterId) : null;
    return w.policies.filter(p => !cluster || specOf(p).clusterName === nameOf(cluster)).map(p => policyRecord(w, p, cluster || w.clusterByName.get(specOf(p).clusterName)));
  },
  policy: async (ctx, id) => (await sources.policies(ctx)).find(x => x.uuid === id || x.policy_name === id) || null,
  hosts: async (ctx, clusterId) => {
    const w = await world(ctx);
    const all = w.k8sNodes.map(n => hostRecord(w, n));
    if (!clusterId) return all;
    return all.filter(h => h.cluster_ids.includes(clusterId));
  },
  unassignedHosts: async ctx => (await sources.hosts(ctx)).filter(h => h.status !== "unavailable" && !h.storage_node_ids.length),
  host: async (ctx, id) => (await sources.hosts(ctx)).find(h => h.uuid === id || h.hostname === id) || null,
  storageClasses: async (ctx, poolId) => {
    const w = await world(ctx);
    const out = w.scs.map(sc => storageClassRecord(w, sc));
    return poolId ? out.filter(x => x.pool_id === poolId || x.pool_name === poolId) : out;
  },
  storageClass: async (ctx, id) => (await sources.storageClasses(ctx)).find(x => x.uuid === id || x.name === id) || null,
  pvcs: async (ctx, scope) => {
    const s = scope || {};
    const w = await world(ctx);
    let out = w.pvcs.filter(p => w.scByName.has(specOf(p).storageClassName) || (specOf(p).volumeName && w.pvByName.has(specOf(p).volumeName))).map(p => pvcRecord(w, p));
    if (s.storageClassId) out = out.filter(x => x.storage_class_id === s.storageClassId || x.storage_class === s.storageClassId);
    if (s.namespace) out = out.filter(x => x.namespace === s.namespace);
    return out;
  },
  pvc: async (ctx, id) => (await sources.pvcs(ctx)).find(x => x.uuid === id) || null,
  k8sClusters: () => Promise.all(allCtx().map(async ctx => {
    const [w, version] = await Promise.all([world(ctx), k8s.version(ctx).catch(() => null)]);
    return k8sClusterRecord(ctx, w, version);
  })),
  k8sCluster: async ctx => {
    const [w, version] = await Promise.all([world(ctx), k8s.version(ctx).catch(() => null)]);
    return k8sClusterRecord(ctx, w, version);
  },
  // ---- pods and logs --------------------------------------------------------
  // the storage plane pods of a node: the node API pod on its worker and the
  // SPDK pod named after its rpc port and cluster
  nodePods: async (ctx, nodeVm) => {
    const w = await world(ctx);
    const node = w.nodeByUUID.get(nodeVm.id) || w.nodeByName.get(nodeVm.name);
    if (!node) return [];
    const cluster = w.clusterOfNode(node);
    const nss = Array.from(new Set([node.metadata.namespace, nsOfCtx(ctx)]));
    const pods = (await Promise.all(nss.map(n => readK(ctx, "Pod", {namespace: n})))).flat();
    const worker = specOf(node).workerNode, rpc = (statusOf(node).ports || {}).rpc, cuuid = cluster ? statusOf(cluster).uuid : null;
    return pods.filter(p => {
      const l = labelsOf(p), onWorker = (specOf(p).nodeName || l["kubernetes.io/hostname"]) === worker;
      const isApi = l.app === "storage-node" || /storage-node/.test(nameOf(p));
      const isSpdk = l.role === "simplyblock-storage-node" || (cuuid && nameOf(p) === `snode-spdk-pod-${rpc}-${cuuid}`) || /^snode-spdk-pod-/.test(nameOf(p));
      return onWorker && (isApi || isSpdk);
    }).map(p => ({name: nameOf(p), namespace: p.metadata.namespace, ctx, kind: labelsOf(p).role === "simplyblock-storage-node" || /^snode-spdk-pod-/.test(nameOf(p)) ? "spdk" : "node-api",
      containers: (specOf(p).containers || []).map(c => c.name), phase: statusOf(p).phase, raw: p}));
  },
  // every pod of the storage plane and the control plane of a cluster
  clusterPods: async (ctx, clusterVm) => {
    const w = await world(ctx);
    const cluster = w.clusterByUUID.get(clusterVm.id) || w.clusterByName.get(clusterVm.name);
    if (!cluster) return [];
    // the cluster's own namespace, plus the console's when the control plane
    // lives there rather than beside the cluster
    const ns = cluster.metadata.namespace;
    const nss = Array.from(new Set([ns, nsOfCtx(ctx)]));
    const pods = (await Promise.all(nss.map(n => readK(ctx, "Pod", {namespace: n})))).flat();
    const cuuid = statusOf(cluster).uuid;
    const nodes = nodesOf(cluster, w.sets, w.nodes);
    const workers = new Set(nodes.map(n => specOf(n).workerNode));
    return pods.map(p => {
      const l = labelsOf(p), ann = p.metadata.annotations || {};
      const onWorker = workers.has(specOf(p).nodeName);
      const spdk = l.role === "simplyblock-storage-node" || /^snode-spdk-pod-/.test(nameOf(p));
      const nodeApi = l.app === "storage-node" || l["simplyblock-cluster"] === nameOf(cluster);
      const cpPod = /^simplyblock-(admin-control|webappapi|monitoring|tasks|fdb)/.test(nameOf(p)) || !!l["foundationdb.org/fdb-cluster-name"]
        || /^simplyblock-operator/.test(nameOf(p)) || ann[LABELS.logCollector] === "true";
      const mine = spdk ? (nameOf(p).endsWith(cuuid) || onWorker) : nodeApi ? (l["simplyblock-cluster"] ? l["simplyblock-cluster"] === nameOf(cluster) : onWorker) : cpPod;
      if (!mine) return null;
      const group = spdk ? "spdk" : nodeApi ? "node-api" : l["foundationdb.org/fdb-cluster-name"] ? "fdb" : "control-plane";
      const node = nodes.find(n => specOf(n).workerNode === specOf(p).nodeName) || null;
      return {name: nameOf(p), namespace: p.metadata.namespace, ctx, group, worker: specOf(p).nodeName || null, nodeId: node ? statusOf(node).uuid : null,
        containers: (specOf(p).containers || []).map(c => c.name), phase: statusOf(p).phase,
        ready: (statusOf(p).containerStatuses || []).every(c => c.ready), restarts: (statusOf(p).containerStatuses || []).reduce((n, c) => n + (c.restartCount || 0), 0), raw: p};
    }).filter(Boolean);
  },
  podLog: (ctx, ns, pod, opts) => k8s.logs(pod, Object.assign({ctx, namespace: ns, timestamps: true}, opts || {}))
};

Object.assign(window, {sources, CTX_ERRORS, resolvePvHandle, k8sIdentity});

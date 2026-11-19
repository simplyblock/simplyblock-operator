// ---------------------------------------------------------------------------
// CONTROL PLANE v2 CLIENT (per context)
//
// What no CRD carries comes from the simplyblock control plane's REST API
// (shared/openapi.json): tasks, the cluster event log, capacity and I/O
// statistics, device health, and a live change stream (?watch=true, SSE).
// Each context reaches its own control plane through the console pod's proxy
// at `cpBase` (/cp/<name>); the bearer is injected there, never held here.
//
// A context with no `cpBase` throws an ApiError with reason NoControlPlane.
// Every panel treats that as "degrade", not "fail": the CRD views still work.
//
// Everything returned is normalized once, with the wire DTO kept under `raw`.
// The console is read-only against the control plane, except for canceling a
// migration, which is the one v2 route that stops a running operation.
// ---------------------------------------------------------------------------
const NO_CP = ctx => new ApiError(503, `control plane of context "${ctx || LOCAL_CTX}" is not reachable from the console`,
  "", REASON_NO_CONTROL_PLANE);

function cpBaseFor(ctx) {
  const c = ctxByName(ctx);
  if (!c || !c.cpBase) throw NO_CP(ctx);
  return c.cpBase + "/api/v2";
}

async function cpCall(ctx, path, init) {
  const base = cpBaseFor(ctx);
  let res;
  try {
    res = await fetch(base + path, Object.assign({headers: {Accept: "application/json"}}, init || {}));
  } catch (e) {
    throw new ApiError(0, "Cannot reach the control plane", path, "Unreachable");
  }
  const text = await res.text();
  let body = null;
  try { body = JSON.parse(text); } catch (e) {}
  if (!res.ok) {
    // the proxy answers a Kubernetes Status when an upstream is not configured
    const reason = body && body.kind === "Status" && res.status === 503 ? REASON_NO_CONTROL_PLANE
      : res.status === 401 || res.status === 403 ? "Forbidden" : (body && body.reason) || null;
    const msg = (body && (body.message || body.detail || body.error)) || res.statusText || "Request failed";
    throw new ApiError(res.status, typeof msg === "string" ? msg : JSON.stringify(msg), path, reason);
  }
  if (body === null && text.trim()) throw new ApiError(502, "control plane returned a non-JSON response", path, "BadGateway");
  return body;
}

const isUuid = s => /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(String(s || ""));
const num = v => (v === null || v === undefined || v === "" ? null : Number(v));
const handleOf = (c, p, v) => `${c}:${p}:${v}`;

// ---- normalizers -----------------------------------------------------------
const normCpCluster = (ctx, c) => ({
  id: c.id, ctx, name: c.name || null, nqn: c.nqn, status: c.status,
  rebalancing: !!c.is_re_balancing, ha: !!c.ha, distrNdcs: c.distr_ndcs, distrNpcs: c.distr_npcs,
  maxFaultTolerance: c.max_fault_tolerance, backupEnabled: !!c.backup_enabled, tls: !!c.tls_enabled,
  nodeAffinity: !!c.node_affinity, failureDomains: !!c.enable_failure_domain, blockSize: c.block_size,
  thresholds: {utilWarn: num(c.utilization_warning), utilCrit: num(c.utilization_critical),
    provWarn: num(c.provisioned_capacity_warning), provCrit: num(c.provisioned_capacity_critical)},
  capacity: normCapacity(c.capacity), raw: c
});
// CapacityStatDTO fields vary by endpoint; keep what is there under stable names
const normCapacity = s => {
  const c = s || {};
  return {
    total: num(c.size_total !== undefined ? c.size_total : c.total), used: num(c.size_used !== undefined ? c.size_used : c.used),
    free: num(c.size_free !== undefined ? c.size_free : c.free), provisioned: num(c.size_prov !== undefined ? c.size_prov : c.provisioned),
    utilPct: num(c.size_util !== undefined ? c.size_util : c.utilization), provPct: num(c.size_prov_util !== undefined ? c.size_prov_util : c.provisioned_utilization),
    raw: c
  };
};
const normCpNode = (ctx, n) => ({
  id: n.id, ctx, clusterId: n.cluster_id, hostname: n.hostname, status: n.status,
  health: n.health_check === null || n.health_check === undefined ? null : !!n.health_check,
  mgmtIp: n.mgmt_ip, hostNqn: n.host_nqn, secondaryNodeId: n.secondary_node_id || null, uptime: n.uptime || null,
  cpu: {total: n.cpu_total_count, spdk: n.cpu_spdk_count, poller: n.cpu_poller_count},
  memory: {total: num(n.memory), hugepages: num(n.hugepage_memory), spdk: num(n.spdk_mem)},
  volumes: {count: n.lvols, max: n.lvols_max}, snapshotsMax: n.snapshots_max,
  ports: {rpc: n.rpc_port, lvol: n.lvol_subsys_port, hublvol: n.hublvol_port, nvmf: n.nvmf_port},
  devices: {count: n.device_count, online: n.online_device_count}, failureDomain: n.failure_domain,
  capacity: normCapacity(n.capacity), raw: n
});
const normCpDevice = (ctx, d) => ({
  id: d.id, ctx, clusterId: d.cluster_id, nodeId: d.storage_node_id, status: d.status,
  health: d.health_check === null || d.health_check === undefined ? null : !!d.health_check,
  retriesExhausted: !!d.retries_exhausted, ioError: !!d.io_error, isPartition: !!d.is_partition,
  model: d.model, serial: d.serial_number, controller: d.nvme_controller, pcie: d.pcie_address,
  size: num(d.size), order: d.cluster_device_order,
  nvmf: {ips: d.nvmf_ips || [], nqn: d.nvmf_nqn, port: d.nvmf_port},
  capacity: normCapacity(d.capacity), raw: d
});
const normCpPool = (ctx, p) => ({
  id: p.id, ctx, clusterId: p.cluster_id, name: p.name || p.pool_name, status: p.status,
  secret: !!p.secret, maxSize: num(p.pool_max_size), lvolMaxSize: num(p.lvol_max_size),
  qos: {rwIops: num(p.max_rw_ios_per_sec), rwMbps: num(p.max_rw_mbytes_per_sec), rMbps: num(p.max_r_mbytes_per_sec), wMbps: num(p.max_w_mbytes_per_sec)},
  capacity: normCapacity(p.capacity), raw: p
});
const normCpVolume = (ctx, cid, pid, v) => ({
  id: v.id, ctx, clusterId: cid || v.cluster_id, poolId: pid || v.pool_id || v.pool_uuid, name: v.name || v.lvol_name,
  status: v.status, size: num(v.size), nqn: v.nqn, pvcName: v.pvc_name || null,
  nodeId: v.storage_node_id || v.node_id || null, nodes: {primary: v.storage_node_id || v.node_id || null, secondary: v.secondary_node_id || null},
  crypto: !!v.crypto, migrating: !!v.migrating,
  qos: {rwIops: num(v.max_rw_ios_per_sec), rwMbps: num(v.max_rw_mbytes_per_sec), rMbps: num(v.max_r_mbytes_per_sec), wMbps: num(v.max_w_mbytes_per_sec)},
  handle: handleOf(cid || v.cluster_id, pid || v.pool_id || v.pool_uuid, v.id),
  capacity: normCapacity(v.capacity), raw: v
});
const normCpSnapshot = (ctx, cid, pid, s) => ({
  id: s.id, ctx, clusterId: cid || s.cluster_id, poolId: pid || s.pool_id || s.pool_uuid, volumeId: s.lvol_id || s.volume_id || null,
  name: s.name || s.snapshot_name, status: s.status, size: num(s.size), createdAt: s.created_at || null,
  handle: handleOf(cid || s.cluster_id, pid || s.pool_id || s.pool_uuid, s.id), raw: s
});

// Tasks: v2 carries no parent field. A sub-task says so in function_params
// when the control plane records it; otherwise a family of migration tasks
// on the same node or device, started after a running master, is attributed
// to that master and flagged `derived` so the monitor can say it guessed.
const MASTER_FNS = ["node_restart", "device_restart", "node_add"];
const SUB_FNS = ["device_migration", "new_device_migration", "failed_device_migration",
  "balancing_on_restart", "balancing_on_dev_rem", "balancing_on_dev_add"];
function taskParentOf(t, all) {
  const p = t.function_params || {};
  const explicit = p.parent_task_id || p.parent_id || p.master_task_id || p.master_id || null;
  if (explicit) return {parentId: explicit, derived: false};
  if (!SUB_FNS.includes(t.function_name)) return {parentId: null, derived: false};
  const master = (all || []).find(m => MASTER_FNS.includes(m.function_name) && m.status !== "done" && !m.canceled
    && ((t.storage_node_id && m.storage_node_id === t.storage_node_id) || (t.device_id && m.device_id === t.device_id)));
  return master ? {parentId: master.id, derived: true} : {parentId: null, derived: false};
}
const normCpTask = (ctx, t, all) => {
  const {parentId, derived} = taskParentOf(t, all);
  return {
    id: t.id, ctx, clusterId: t.cluster_id, nodeId: t.storage_node_id || null, deviceId: t.device_id || null,
    status: t.status, canceled: !!t.canceled, fn: t.function_name, params: t.function_params || {},
    result: t.function_result || "", retry: num(t.retry) || 0, maxRetry: num(t.max_retry),
    parentId, derived, kind: parentId ? "sub" : "master",
    createdAt: t.date ? new Date(Number(t.date) * (Number(t.date) > 1e11 ? 1 : 1000)).toISOString() : (t.created_at || null),
    updatedAt: t.updated_at || null, raw: t
  };
};

// The event log: {Date, NodeId, Event, Level, Message, Storage_ID, VUID, Status}.
// Dates are "YYYY-MM-DD HH:MM:SS[.mmm]" with no zone and are read as UTC, as
// the chart's Grafana rules do.
const parseLogDate = d => {
  if (!d) return null;
  const m = String(d).match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,6}))?/);
  if (!m) { const t = Date.parse(d); return isNaN(t) ? null : t; }
  const ms = m[7] ? Math.round(Number("0." + m[7]) * 1000) : 0;
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6], ms);
};
let logSeq = 0;
const normCpLog = (ctx, cid, r) => {
  const ts = parseLogDate(r.Date);
  const key = `${r.Date}|${r.Event}|${r.NodeId}|${r.Message}`;
  return {
    // `ts` is the ISO string the panels format; `tsMs` is for sorting and windows
    id: hashKey(key), ctx, clusterId: cid, ts: ts === null ? "" : new Date(ts).toISOString(), tsMs: ts, tsRaw: r.Date,
    level: String(r.Level || "info").toLowerCase(), event: r.Event, message: r.Message,
    nodeId: r.NodeId || null, storageId: r.Storage_ID && r.Storage_ID !== "None" ? r.Storage_ID : null,
    vuid: r.VUID && r.VUID !== "None" ? r.VUID : null, recordStatus: r.Status || null,
    objectKind: null, objectId: null, objectName: null, seq: logSeq++, raw: r
  };
};
function hashKey(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); }
  return (h >>> 0).toString(36);
}

// DeviceHealthInfoDTO is NVMe SMART. The derived status is what a tile needs.
const normHealthInfo = (ctx, h) => {
  const spare = num(h.available_spare_percentage), spareMin = num(h.available_spare_threshold_percentage);
  const used = num(h.percentage_used), temp = num(h.temperature_celsius);
  const critical = (num(h.critical_warning) || 0) !== 0 || (num(h.media_errors) || 0) > 0 || (spare !== null && spareMin !== null && spare < spareMin);
  const warn = !critical && ((used !== null && used > 80) || (temp !== null && temp > 70));
  return {
    id: h.id, ctx, model: h.model_number, serial: h.serial_number, firmware: h.firmware_revision, traddr: h.traddr,
    criticalWarning: num(h.critical_warning), tempC: temp, spare, spareThreshold: spareMin, pctUsed: used,
    unitsRead: num(h.data_units_read), unitsWritten: num(h.data_units_written),
    hostReads: num(h.host_read_commands), hostWrites: num(h.host_write_commands),
    busyTime: num(h.controller_busy_time), powerCycles: num(h.power_cycles), powerOnHours: num(h.power_on_hours),
    unsafeShutdowns: num(h.unsafe_shutdowns), mediaErrors: num(h.media_errors), errLogEntries: num(h.num_err_log_entries),
    warnTempMin: num(h.warning_temperature_time_minutes), critTempMin: num(h.critical_composite_temperature_time_minutes),
    status: critical ? "critical" : warn ? "warn" : "good", raw: h
  };
};
// statistics endpoints are untyped in the spec: keep the keys, coerce the time
const normSeries = r => {
  const arr = Array.isArray(r) ? r : (r && Array.isArray(r.results)) ? r.results : r ? [r] : [];
  const points = arr.map(p => Object.assign({}, p, {ts: p.date ? parseLogDate(p.date) || Number(p.date) * 1000 : (p.ts || null)}));
  return {points, latest: points.length ? points[points.length - 1] : null};
};
const normMigration = (ctx, cid, nqn, m) => ({
  id: m.id, ctx, clusterId: cid, nqn, volumeId: m.lvol_id || null, sourceNodeId: m.source_node_id, targetNodeId: m.target_node_id,
  targetNqn: m.target_nqn || null, phase: m.phase, status: m.status, memberCount: num(m.member_count),
  snaps: {total: num(m.snaps_total), migrated: num(m.snaps_migrated)}, retries: {n: num(m.retry_count) || 0, max: num(m.max_retries)},
  error: m.error_message || "", startedAt: m.started_at ? new Date(Number(m.started_at) * 1000).toISOString() : null,
  completedAt: m.completed_at ? new Date(Number(m.completed_at) * 1000).toISOString() : null, raw: m
});

// ---- reads -----------------------------------------------------------------
const C = cid => `/clusters/${encodeURIComponent(cid)}`;
const cp = {
  reachable: ctx => hasControlPlane(ctx),
  health: async ctx => {
    const t0 = performance.now();
    try { await cpCall(ctx, "/_meta/health"); return {ok: true, ms: Math.round(performance.now() - t0), at: Date.now()}; }
    catch (e) { return {ok: false, ms: Math.round(performance.now() - t0), at: Date.now(), error: e}; }
  },
  clusters: ctx => cpCall(ctx, "/clusters/").then(r => (r || []).map(c => normCpCluster(ctx, c))),
  cluster: (ctx, cid) => cpCall(ctx, `${C(cid)}/`).then(c => normCpCluster(ctx, c)),
  nodes: (ctx, cid) => cpCall(ctx, `${C(cid)}/storage-nodes/`).then(r => (r || []).map(n => normCpNode(ctx, n))),
  node: (ctx, cid, nid) => cpCall(ctx, `${C(cid)}/storage-nodes/${nid}/`).then(n => normCpNode(ctx, n)),
  devices: (ctx, cid, nid) => cpCall(ctx, `${C(cid)}/storage-nodes/${nid}/devices/`).then(r => (r || []).map(d => normCpDevice(ctx, d))),
  device: (ctx, cid, nid, did) => cpCall(ctx, `${C(cid)}/storage-nodes/${nid}/devices/${did}/`).then(d => normCpDevice(ctx, d)),
  healthInfo: (ctx, cid, nid, did) => cpCall(ctx, `${C(cid)}/storage-nodes/${nid}/devices/${did}/health-info`).then(h => normHealthInfo(ctx, h)),
  pools: (ctx, cid) => cpCall(ctx, `${C(cid)}/storage-pools/`).then(r => (r || []).map(p => normCpPool(ctx, p))),
  volumes: (ctx, cid, pid) => cpCall(ctx, `${C(cid)}/storage-pools/${pid}/volumes/`).then(r => (r || []).map(v => normCpVolume(ctx, cid, pid, v))),
  volume: (ctx, cid, pid, vid) => cpCall(ctx, `${C(cid)}/storage-pools/${pid}/volumes/${vid}/`).then(v => normCpVolume(ctx, cid, pid, v)),
  snapshots: (ctx, cid, pid) => cpCall(ctx, `${C(cid)}/storage-pools/${pid}/snapshots/`).then(r => (r || []).map(s => normCpSnapshot(ctx, cid, pid, s))),
  // every volume and snapshot of a cluster, pool by pool
  clusterVolumes: async (ctx, cid) => {
    const pools = await cp.pools(ctx, cid);
    const rs = await Promise.all(pools.map(p => cp.volumes(ctx, cid, p.id).catch(() => [])));
    return rs.flat();
  },
  clusterSnapshots: async (ctx, cid) => {
    const pools = await cp.pools(ctx, cid);
    const rs = await Promise.all(pools.map(p => cp.snapshots(ctx, cid, p.id).catch(() => [])));
    return rs.flat();
  },
  tasks: (ctx, cid) => cpCall(ctx, `${C(cid)}/tasks/`).then(r => { const all = r || []; return all.map(t => normCpTask(ctx, t, all)); }),
  task: (ctx, cid, tid) => cpCall(ctx, `${C(cid)}/tasks/${tid}/`).then(t => normCpTask(ctx, t, [])),
  logs: (ctx, cid, limit) => cpCall(ctx, `${C(cid)}/logs?limit=${limit || 200}`)
    .then(r => (Array.isArray(r) ? r : (r && r.results) || []).map(x => normCpLog(ctx, cid, x))),
  capacity: (ctx, cid, history) => cpCall(ctx, `${C(cid)}/capacity${history ? `?history=${encodeURIComponent(history)}` : ""}`).then(normSeries),
  iostats: (ctx, cid, history) => cpCall(ctx, `${C(cid)}/iostats${history ? `?history=${encodeURIComponent(history)}` : ""}`).then(normSeries),
  nodeIostats: (ctx, cid, nid, history) => cpCall(ctx, `${C(cid)}/storage-nodes/${nid}/iostats${history ? `?history=${encodeURIComponent(history)}` : ""}`).then(normSeries),
  nodeCapacity: (ctx, cid, nid, history) => cpCall(ctx, `${C(cid)}/storage-nodes/${nid}/capacity${history ? `?history=${encodeURIComponent(history)}` : ""}`).then(normSeries),
  deviceIostats: (ctx, cid, nid, did, history) => cpCall(ctx, `${C(cid)}/storage-nodes/${nid}/devices/${did}/iostats${history ? `?history=${encodeURIComponent(history)}` : ""}`).then(normSeries),
  poolIostats: (ctx, cid, pid, limit) => cpCall(ctx, `${C(cid)}/storage-pools/${pid}/iostats${limit ? `?limit=${limit}` : ""}`).then(normSeries),
  volumeIostats: (ctx, cid, pid, vid, history) => cpCall(ctx, `${C(cid)}/storage-pools/${pid}/volumes/${vid}/iostats${history ? `?history=${encodeURIComponent(history)}` : ""}`).then(normSeries),
  migrations: (ctx, cid, nqn) => cpCall(ctx, `${C(cid)}/subsystems/${encodeURIComponent(nqn)}/migrations/`).then(r => (r || []).map(m => normMigration(ctx, cid, nqn, m))),
  migration: (ctx, cid, nqn, id) => cpCall(ctx, `${C(cid)}/subsystems/${encodeURIComponent(nqn)}/migrations/${id}/`).then(m => normMigration(ctx, cid, nqn, m)),
  // the one write: DELETE cancels a running migration
  cancelMigration: (ctx, cid, nqn, id) => cpCall(ctx, `${C(cid)}/subsystems/${encodeURIComponent(nqn)}/migrations/${id}/`, {method: "DELETE"}),

  // ---- live stream ---------------------------------------------------------
  // cp.watch(ctx, "/clusters/<c>/tasks/", {snapshot, created, updated, deleted, state}) -> cancel()
  // The server sends a full `snapshot` on every (re)connect, then one event per
  // change, a ping every 15 s, and closes after an hour. There is no resume, so
  // a reconnect is just a fresh snapshot. After three failed opens in two
  // minutes the caller is told to poll (`state("polling")`); SSE is retried
  // every five minutes after that.
  watch: (ctx, path, handlers) => {
    const h = handlers || {};
    let base;
    try { base = cpBaseFor(ctx); } catch (e) { if (h.error) h.error(e); if (h.state) h.state("unavailable"); return () => {}; }
    const url = base + path + (path.includes("?") ? "&" : "?") + "watch=true";
    let es = null, closed = false, lastPing = Date.now(), fails = [], timer = null, retryTimer = null;
    const parse = e => { try { return JSON.parse(e.data); } catch (x) { return null; } };
    const open = () => {
      if (closed) return;
      try { es = new EventSource(url); } catch (e) { onFail(e); return; }
      if (h.state) h.state("connecting");
      es.onopen = () => { lastPing = Date.now(); fails = []; if (h.state) h.state("live"); };
      es.addEventListener("snapshot", e => { lastPing = Date.now(); const d = parse(e); if (d !== null && h.snapshot) h.snapshot(d); });
      es.addEventListener("created", e => { lastPing = Date.now(); const d = parse(e); if (d && h.created) h.created(d); });
      es.addEventListener("updated", e => { lastPing = Date.now(); const d = parse(e); if (d && h.updated) h.updated(d); });
      es.addEventListener("deleted", e => { lastPing = Date.now(); const d = parse(e); if (h.deleted) h.deleted(d || {}); });
      es.addEventListener("ping", () => { lastPing = Date.now(); if (h.ping) h.ping(); });
      es.addEventListener("error", e => { if (e && e.data) { const d = parse(e); if (h.error) h.error(new ApiError(502, (d && d.detail) || "stream error", path, "StreamError")); } });
      es.onmessage = e => { const d = parse(e); if (d && d.type && h[d.type]) h[d.type](d.data !== undefined ? d.data : d); lastPing = Date.now(); };
      es.onerror = () => { onFail(); };
    };
    const onFail = () => {
      if (closed) return;
      if (es) { es.close(); es = null; }
      const now = Date.now();
      fails = fails.filter(t => now - t < 120000).concat([now]);
      if (fails.length >= 3) {
        if (h.state) h.state("polling");
        retryTimer = setTimeout(() => { fails = []; open(); }, 300000);
      } else {
        if (h.state) h.state("reconnecting");
        retryTimer = setTimeout(open, 3000);
      }
    };
    timer = setInterval(() => { if (es && Date.now() - lastPing > 45000) onFail(); }, 15000);
    open();
    return () => { closed = true; clearInterval(timer); clearTimeout(retryTimer); if (es) es.close(); };
  }
};

Object.assign(window, {cp, cpBaseFor, isUuid, parseLogDate, taskParentOf, normCpTask, normCpLog, hashKey});

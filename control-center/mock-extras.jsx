// ---------------------------------------------------------------------------
// MOCK EXTRAS — fixtures + routes for the data that is NOT part of control
// plane API v2 read models: tasks & cluster event log (v2), and the
// agent/Prometheus sources (container allocation, container logs, FoundationDB
// backups, SPDK logs & thread utilization, SMART output).
// Chains onto the fetch installed by mock-api.jsx.
// ---------------------------------------------------------------------------
const X = window.SB_DB, XU = window.SB_UTIL;
const xpick = XU.pick, xint = XU.int, xuuid = XU.uuid, xhex = XU.hex, xago = XU.ago;
const XGB = 1e9;

X.tasks = []; X.logs = []; X.alerts = []; X.containers = []; X.container_logs = {};
X.fdb_backups = []; X.spdk_threads = {}; X.spdk_logs = {}; X.smart = {};

const TASK_FUNCS = ["node_restart", "port_allow", "balancing_on_restart", "fdb_backup", "device_migration",
  "new_device_discovery", "device_restart", "snapshot_delete", "lvol_migration", "cluster_upgrade"];
const TASK_STATUS = ["done", "done", "done", "done", "running", "new", "suspended"];
const RESULTS = {
  node_restart: ["Node is restarting", "canceled: node back online", "", "Node restarted"],
  port_allow: ["Port 4440 allowed on node", "Port 4432 allowed on node", ""],
  balancing_on_restart: ["", "running", "Done"],
  fdb_backup: ["Done", "running", ""],
  device_migration: ["Done", "canceled", "running", ""],
  new_device_discovery: ["Done", ""],
  device_restart: ["Done", "canceled: device unavailable", ""],
  snapshot_delete: ["Done", ""],
  lvol_migration: ["Done", "running", ""],
  cluster_upgrade: ["running", ""]
};

X.clusters.forEach(c => {
  const nodes = X.storage_nodes.filter(n => n.cluster_id === c.uuid);
  const devs = X.devices.filter(d => d.cluster_id === c.uuid);

  // ---- tasks + subtasks (edge clusters run no task engine) ----
  // Mirrors `cluster list-tasks`: Task ID · Target ID · Function · Retry · Status · Result · Updated At.
  // Subtasks add Node ID and Distrib, as in `cluster list-subtasks`.
  for (let i = 0; c.capabilities.tasks && i < xint(9, 18); i++) {
    const fn = xpick(TASK_FUNCS);
    const status = xpick(TASK_STATUS);
    const node = nodes.length ? xpick(nodes) : null;
    const master = fn === "balancing_on_restart" || (fn === "device_migration" && Math.random() > .5);
    const subCount = master ? xint(4, 36) : 0;
    const maxRetry = fn === "node_restart" ? 11 : fn === "port_allow" ? 8 : 0;
    const t = {uuid: xuuid(), cluster_id: c.uuid, parent_id: null,
      function_name: fn,
      target_id: master ? `Master task for ${subCount} subtasks` : (node ? `NodeID:${node.uuid}` : `ClusterID:${c.uuid}`),
      node_id: node ? node.uuid : null, distrib: null,
      retry: 0, max_retry: maxRetry,
      status, result: status === "running" ? xpick(["running", ""]) : xpick(RESULTS[fn] || [""]),
      created_at: xago(xint(1, 400)), updated_at: xago(Math.random() * 40), canceled: false,
      subtask_total: subCount};
    if (t.status === "done" && /^canceled/.test(t.result)) t.canceled = true;
    X.tasks.push(t);
    for (let s = 0; s < subCount; s++) {
      const sstatus = status === "done" ? "done" : xpick(["done", "done", "running", "new"]);
      const sn = nodes.length ? xpick(nodes) : null;
      const sretry = Math.random() > .8 ? xint(1, 3) : 0;
      X.tasks.push({uuid: xuuid(), cluster_id: c.uuid, parent_id: t.uuid,
        function_name: fn === "balancing_on_restart" ? "device_migration" : fn,
        target_id: null, node_id: sn ? sn.uuid : null,
        distrib: `distrib_${s + 1}`,
        retry: sretry, max_retry: 0,
        status: sstatus,
        result: sstatus === "done" ? (sretry ? "canceled" : "Done") : sstatus === "running" ? "running" : "",
        created_at: t.created_at, updated_at: xago(Math.random() * 30), canceled: sretry > 0,
        subtask_total: 0});
    }
  }

  // ---- cluster event log (mirrors `cluster get-logs` output) ----
  // Columns: Date · NodeId · Event · Level · Message · Storage_ID · VUID · Status
  const nodeIds = nodes.map(n => n.uuid);
  const devIds = devs.map(d => d.uuid);
  const lvols = X.lvols.filter(v => v.cluster_id === c.uuid);
  const nodeName = id => (nodes.find(n => n.uuid === id) || {}).hostname || null;
  const devOf = id => devs.find(d => d.uuid === id) || {};
  const SKIP = ["skipped:dev_unavailable", "skipped:device_node_offline", "skipped:device_node_unreachable",
    "skipped:node_wide_io_quorum", "skipped:node_offline", "late_by_13s_skipping", "late_by_14s_skipping", "late_by_15s_skipping"];
  const DEV_ERR = ["SPDK_BDEV_EVENT_REMOVE (3)", "error_write (3)", "error_read (3)",
    "error_write_cannot_allocate (4)", "error_unmap (3)", "error_read (4)"];

  const push = (ts, o) => X.logs.push(Object.assign({
    uuid: xuuid(), cluster_id: c.uuid, ts,
    node_id: null, event: "STATUS_CHANGE", level: "Info", message: "",
    storage_id: null, vuid: null, record_status: "None",
    object_kind: null, object_id: null, object_name: null
  }, o));

  // bootstrap trail
  push(xago(310), {event: "OBJ_CREATED", message: `Cluster created ${c.uuid}`, object_kind: "cluster", object_id: c.uuid, object_name: c.name});
  push(xago(309.9), {event: "OBJ_CREATED", node_id: xuuid(), message: `Management node added ip-172-31-46-${xint(10, 90)}`, object_kind: "node"});
  devs.slice(0, 12).forEach((d, i) => push(xago(309 - i * .01), {event: "OBJ_CREATED", node_id: d.uuid,
    message: `Device created: ${d.uuid}`, storage_id: i, object_kind: "device", object_id: d.uuid, object_name: d.serial_number}));
  nodes.forEach((n, i) => push(xago(308 - i * .02), {message: "Storage node status changed from: in_creation to: online",
    node_id: n.uuid, object_kind: "node", object_id: n.uuid, object_name: n.hostname}));
  push(xago(307.5), {message: "Cluster status changed from unready to in_activation", object_kind: "cluster", object_id: c.uuid, object_name: c.name});
  nodes.forEach((n, i) => push(xago(307.2 - i * .02), {level: "Warning", node_id: n.uuid,
    message: `Storage node ports set, LVol:${4432 + i * 2} RPC:${4420 + i} Internal:${4426 + i}`,
    object_kind: "node", object_id: n.uuid, object_name: n.hostname}));
  push(xago(307), {message: `Cluster status changed from in_activation to ${c.status === "online" ? "active" : c.status}`, object_kind: "cluster", object_id: c.uuid, object_name: c.name});
  X.pools.filter(p => p.cluster_id === c.uuid).forEach((p, i) => push(xago(306.8 - i * .05), {event: "OBJ_CREATED",
    node_id: c.uuid, message: `Pool created ${p.pool_name}`, object_kind: "pool", object_id: p.uuid, object_name: p.pool_name}));
  lvols.slice(0, 8).forEach((v, i) => push(xago(300 - i * .02), {event: "OBJ_CREATED", node_id: v.uuid,
    message: `LVol created, ${v.lvol_name}`, object_kind: "lvol", object_id: v.uuid, object_name: v.lvol_name}));

  // running operational stream
  for (let i = 0; i < 260; i++) {
    const ts = xago(i * 0.42 + Math.random() * 0.3);
    const roll = Math.random();
    const nid = nodeIds.length ? xpick(nodeIds) : null;
    const d = devIds.length ? devOf(xpick(devIds)) : {};
    const sid = xint(0, 11);
    if (roll < .26) {
      push(ts, {event: "device_status", level: "Error", node_id: nid, message: xpick(DEV_ERR),
        storage_id: sid, record_status: Math.random() > .3 ? "processed" : xpick(SKIP),
        object_kind: "device", object_id: d.uuid, object_name: d.serial_number});
    } else if (roll < .40) {
      const allowed = Math.random() > .5;
      push(ts, {level: "Warning", node_id: nid, message: `Port ${allowed ? "allowed" : "blocked"}: ${4420 + xint(0, 22)}`,
        record_status: "None", object_kind: "node", object_id: nid, object_name: nodeName(nid)});
    } else if (roll < .54) {
      const to = Math.random() > .5;
      push(ts, {node_id: nid, message: `Storage node health check changed from: ${!to} to: ${to}`,
        object_kind: "node", object_id: nid, object_name: nodeName(nid)});
    } else if (roll < .68) {
      const down = Math.random() > .45;
      push(ts, {node_id: d.uuid, storage_id: sid,
        message: down ? "Device status changed from: online to: unavailable" : "Device restarted, status: online",
        level: down ? "Warning" : "Info",
        record_status: down && Math.random() > .6 ? "forced_unavailable:remote_io_quorum" : "None",
        object_kind: "device", object_id: d.uuid, object_name: d.serial_number});
    } else if (roll < .76) {
      push(ts, {node_id: d.uuid, storage_id: sid, message: "Device health changed from: None to: True",
        object_kind: "device", object_id: d.uuid, object_name: d.serial_number});
    } else if (roll < .84) {
      const deg = Math.random() > .5;
      push(ts, {message: `Cluster status changed from ${deg ? "active to degraded" : "degraded to active"}`,
        level: deg ? "Warning" : "Info", object_kind: "cluster", object_id: c.uuid, object_name: c.name});
    } else if (roll < .90) {
      push(ts, {event: "OBJ_CREATED", node_id: nid, message: xpick(["Task created", "Re-balancing task updated"]),
        record_status: xpick(["new", "running", "suspended"]), object_kind: "task", object_id: nid});
    } else if (roll < .96) {
      const from = xpick(["offline to: in_restart", "in_restart to: online", "online to: in_shutdown",
        "in_shutdown to: offline", "online to: unreachable", "unreachable to: offline", "in_restart to: offline"]);
      push(ts, {node_id: nid, message: `Storage node status changed from: ${from}`,
        level: /offline|unreachable/.test(from.split("to: ")[1]) ? "Warning" : "Info",
        object_kind: "node", object_id: nid, object_name: nodeName(nid)});
    } else if (roll < .985) {
      push(ts, {event: "jm_compression", node_id: nid, vuid: String(xint(1, 24)),
        message: xpick(["compression_started", "compression_finished"]),
        record_status: xpick(["running", "processed"]), object_kind: "node", object_id: nid, object_name: nodeName(nid)});
    } else {
      push(ts, {level: "Error", node_id: nid, message: "Storage node LVStore recovery failed",
        object_kind: "node", object_id: nid, object_name: nodeName(nid)});
    }
  }
  // one long, wrapped diagnostic like the real output
  if (devs.length) {
    const d = devs[0];
    push(xago(4.2), {level: "Warning", node_id: d.uuid, storage_id: 2,
      message: `Device ${d.uuid} (storage_id 2) block-size-normalized IO latency 2.7x the cluster average over 10m (19.29 vs 7.25 ticks/byte) - possible degraded device`,
      object_kind: "device", object_id: d.uuid, object_name: d.serial_number});
  }
  X.logs.sort((a, b) => Date.parse(b.ts) - Date.parse(a.ts));

  // ---- SPDK threads + logs per node (Prometheus / node agent) ----
  nodes.forEach(n => {
    const cores = xint(4, 10);
    const threads = [];
    for (let core = 0; core < cores; core++) {
      const names = core === 0 ? ["app_thread"] : [`nvmf_tgt_poll_group_${core - 1}`];
      if (core > 0 && Math.random() > .6) names.push(`distr_qos_${core}`);
      names.forEach(nm => threads.push({name: nm, core,
        busy_pct: n.status === "online" ? +(Math.random() * 82 + 4).toFixed(1) : 0,
        poll_count: n.status === "online" ? xint(200000, 9000000) : 0,
        idle_tsc: xint(1e9, 9e9)}));
    }
    X.spdk_threads[n.uuid] = {cores, threads};
    X.spdk_logs[n.uuid + "/spdk"] = Array.from({length: 80}, (_, i) => spdkLine(i, false));
    X.spdk_logs[n.uuid + "/spdk-proxy"] = Array.from({length: 80}, (_, i) => spdkLine(i, true));
  });
});

// Some volumes have been moved recently, so the rebalancing counters on a
// cluster are not all zero. Every move is an lvol_migration task, and the
// counters are derived from these records by the rollup.
X.clusters.forEach(c => {
  if (!c.auto_rebalance || !c.auto_rebalance.enabled) return;
  const vols = X.lvols.filter(v => v.cluster_id === c.uuid && v.nodes && v.nodes.primary);
  const n = Math.min(vols.length, xint(0, 9));
  for (let i = 0; i < n; i++) {
    const v = vols[i];
    const hrs = i < 2 ? Math.random() * 0.9 : 1 + Math.random() * 22;
    X.tasks.push({uuid: xuuid(), cluster_id: c.uuid, parent_id: null,
      function_name: "lvol_migration", target_id: `LvolID:${v.uuid}`, node_id: v.nodes.primary.uuid,
      distrib: null, retry: 0, max_retry: 3, status: "done",
      result: `Volume ${v.lvol_name} moved to ${v.nodes.primary.hostname}`,
      created_at: xago(hrs), updated_at: xago(hrs - .01), canceled: false, subtask_total: 0});
  }
});
XU.rollup();

// ---- SMART per device -----------------------------------------------------
// A health check reads the drive's SMART counters and the verdict follows from
// them: spare below the 10% threshold, rising media errors or a set critical
// warning bit mean critical; a handful of corrected errors or a hot drive mean
// warn. The device's health traffic light is that verdict — there is no second,
// independent notion of health anywhere.
function smartRead(d, bias) {
  // bias lets the fixture keep a seeded verdict stable across a re-read, and
  // lets a genuinely dying drive stay dying instead of randomly recovering
  const b = bias || d.health_check || "good";
  const bad = b === "critical", warn = b === "warn";
  const spare = bad ? xint(3, 9) : warn ? xint(28, 62) : xint(92, 100);
  const media = bad ? xint(180, 4000) : warn ? xint(1, 12) : 0;
  const errLog = bad ? xint(40, 900) : warn ? xint(1, 6) : 0;
  const temp = d.temperature_c;
  const critWarn = bad ? 0x04 : 0x00;
  // the verdict, derived from the counters above
  const verdict = (critWarn || spare < 10 || media > 100) ? "critical"
    : (media > 0 || errLog > 0 || spare < 70 || temp > 60) ? "warn" : "good";
  return {
    verdict,
    report: {
      checked_at: xago(0),
      overall: verdict === "critical" ? "FAILED" : verdict === "warn" ? "PASSED (warnings)" : "PASSED",
      verdict,
      model: d.model_number, serial: d.serial_number, firmware: d.firmware_revision,
      attributes: [
        {name: "Critical warning", value: "0x" + critWarn.toString(16).padStart(2, "0"),
          note: critWarn ? "reliability degraded" : null},
        {name: "Temperature", value: temp + " °C", note: temp > 60 ? "above advisory threshold" : null},
        {name: "Available spare", value: spare + " %",
          note: spare < 10 ? "below threshold (10 %)" : spare < 70 ? "declining" : null},
        {name: "Percentage used", value: d.percentage_used + " %"},
        {name: "Data units read", value: xint(2, 900) + " M"},
        {name: "Data units written", value: xint(2, 700) + " M"},
        {name: "Power on hours", value: d.power_on_hours + " h"},
        {name: "Unsafe shutdowns", value: String(xint(0, 12))},
        {name: "Media & data integrity errors", value: String(media),
          note: media > 100 ? "rising" : media ? "corrected" : null},
        {name: "Error information log entries", value: String(errLog)}
      ]
    }
  };
}

// Run a health check: re-read SMART and update the device's health from it.
// Both the HealthCheck operation and an on-demand SMART re-read come here.
X.runHealthCheck = function (uuid, bias) {
  const d = X.devices.find(y => y.uuid === uuid);
  if (!d) return null;
  const live = d.status === "online" || d.status === "read_only";
  if (!live) {
    // nvme-cli cannot talk to a drive that is not attached
    X.smart[uuid] = Object.assign({}, X.smart[uuid] || {}, {unavailable: true, checked_at: xago(0)});
    d.health_check = null;
    return X.smart[uuid];
  }
  // a drive usually reports what it reported last time; sometimes it degrades
  const drift = bias || (Math.random() > .88
    ? (d.health_check === "good" ? "warn" : d.health_check === "warn" ? "critical" : d.health_check)
    : d.health_check);
  const r = smartRead(d, drift);
  X.smart[uuid] = r.report;
  d.health_check = r.verdict;
  d.last_health_check = xago(0);
  return r.report;
};

X.devices.forEach(d => {
  const live = d.status === "online" || d.status === "read_only";
  if (!live) { X.smart[d.uuid] = {unavailable: true, checked_at: xago(xint(1, 60))}; return; }
  const r = smartRead(d);
  d.health_check = r.verdict;
  d.last_health_check = xago(xint(0, 48));
  X.smart[d.uuid] = Object.assign(r.report, {checked_at: d.last_health_check});
});

function logLine(name, i) {
  const lvl = xpick(["INFO", "INFO", "INFO", "INFO", "WARN", "ERROR", "DEBUG"]);
  const msgs = {
    INFO: [`${name} healthy, heartbeat ok`, "reconciled desired state", "GET /api/v2/clusters 200 12ms",
      "flushed metrics to prometheus", "lease renewed"],
    WARN: ["retrying transaction after conflict", "slow query 812ms", "connection pool at 85%"],
    ERROR: ["transaction_too_old, retrying", "failed to reach node, backing off", "read version timeout"],
    DEBUG: ["cache hit ratio 0.94", "gc pass complete"]
  };
  return {ts: xago(i * 0.12), level: lvl, msg: xpick(msgs[lvl])};
}
function spdkLine(i, proxy) {
  const lvl = xpick(["INFO", "INFO", "INFO", "NOTICE", "WARNING", "ERROR"]);
  const msgs = proxy
    ? ["rpc: bdev_get_bdevs took 3ms", "proxy accepted connection from 10.20.0.4", "forwarding rpc distr_status",
       "client disconnected", "rpc timeout, retry 1/3"]
    : ["nvmf_tgt: new qpair on poll group 2", "bdev_distr: chunk rebuild 42%", "accel_fw: task queue depth 12",
       "nvme_pcie: admin cmd completed", "bdev_nvme: reset controller 0000:5e:00.0", "distr: page migration queued"];
  return {ts: xago(i * 0.05), level: lvl, msg: xpick(msgs)};
}

// ---- the control plane (agent, not API) -----------------------------------
// One deployment manages every cluster, so its containers and its state
// database are seeded once — not per cluster.
(function seedControlPlane() {
  const cs = window.SB_DB.clusters;
  const version = (cs[0] || {}).cluster_version || "26.2.1";
  const anyLive = cs.some(c => c.status !== "unready");
  const mk = (name, group, cores, memLimit, diskLimit) => {
    X.containers.push({name, group, image: `simplyblock/${name}:${version}`,
      state: "running", cpu_cores_alloc: cores,
      cpu_pct: +(Math.random() * cores * 60).toFixed(1),
      mem_limit: memLimit, mem_used: Math.round(memLimit * (.25 + Math.random() * .5)),
      disk_limit: diskLimit, disk_used: Math.round(diskLimit * (.15 + Math.random() * .6)),
      restarts: xint(0, 4), uptime_h: xint(2, 900)});
    X.container_logs[name] = Array.from({length: 60}, (_, i) => logLine(name, i));
  };
  mk("fdb-coordinator", "state db", 2, 4 * XGB, 40 * XGB);
  mk("fdb-storage-1", "state db", 4, 8 * XGB, 200 * XGB);
  mk("fdb-storage-2", "state db", 4, 8 * XGB, 200 * XGB);
  mk("simplyblock-core", "services", 4, 8 * XGB, 20 * XGB);
  mk("simplyblock-tasks", "services", 2, 4 * XGB, 10 * XGB);
  mk("simplyblock-webapp", "services", 1, 2 * XGB, 5 * XGB);
  mk("prometheus", "monitoring", 2, 8 * XGB, 500 * XGB);
  if (anyLive) { mk("graylog", "observability", 2, 8 * XGB, 200 * XGB);
    mk("opensearch", "observability", 4, 16 * XGB, 1000 * XGB);
    mk("mongodb", "observability", 2, 4 * XGB, 100 * XGB); }
  for (let i = 0; i < xint(8, 16); i++) {
    X.fdb_backups.push({id: `fdb-bk-${xhex(6)}`,
      version: `${version}-${String(200 - i * 3).padStart(4, "0")}`,
      created_at: xago(i * 6 + xint(0, 3)), size: xint(180, 2400) * 1e6,
      type: i % 4 === 0 ? "full" : "incremental",
      status: i === 0 ? xpick(["complete", "complete", "in_progress"]) : "complete"});
  }
  // A few conditions are lit on purpose, so the indicator board has something
  // to show. Everything else the rules derive from ordinary fixture state.
  cs.forEach((c, ci) => {
    const ns = window.SB_DB.storage_nodes.filter(n => n.cluster_id === c.uuid && n.status === "online");
    if (!ns.length) return;
    if (ci % 3 === 0 && ns[0]) { ns[0].meta_compaction_failed = true; ns[0].meta_size_util = Math.round(ns[0].meta_size_total * .93); }
    if (ci % 3 === 1 && ns[0]) ns[0].objects_used = ns[0].objects_max - xint(1, 400);
    if (ns[1]) ns[1].latency_us = xint(1400, 2600);
  });
  const fdbFail = X.fdb_backups[1]; if (fdbFail) fdbFail.status = "failed";
  const core = X.containers.find(c => c.name === "simplyblock-webapp"); if (core) core.restarts = 6;
})();

// ---- alerts ---------------------------------------------------------------
// An alert is NOT an event. It is an indicator: a named condition evaluated
// against live state on every read. While the condition holds the alert is
// there; the moment it clears the alert is gone. Nothing to dismiss, no
// reconciliation event to file — the lamp simply goes out.
//
// Silences are the one piece of stored state: an operator can turn a lamp's
// noise off, keyed by rule + object, and the silence dies with the condition.
X.silences = {};
const silKey = (rule, id, scopeId) => rule + "|" + (scopeId || "cp") + "|" + (id || "-");
// first_seen: the console wants to know how long a lamp has been lit, and the
// condition itself carries no timestamp, so the first sighting is remembered.
X.alert_seen = {};
// Conditions that are already true when the mock boots have been true for a
// while — dating them all "now" would make every lamp look brand new.
const ALERT_BOOT_MS = Date.now();
const seenAt = key => {
  if (!X.alert_seen[key]) X.alert_seen[key] = Date.now() - ALERT_BOOT_MS < 20000
    ? xago(xint(1, 190) + Math.random()) : new Date().toISOString();
  return X.alert_seen[key];
};

const pctOf = (u, t) => t ? u / t : 0;

// Every rule: id, severity, scope, and a function returning zero or more
// conditions. Node and device rules always name the node and the device(s).
const ALERT_RULES = [
  {rule: "storage_node_offline", severity: "critical", scope: "cluster",
    title: "Storage node offline",
    remedy: "Restart the node. If it was stopped on purpose, this alert clears once it is back online.",
    eval: (c, ctx) => ctx.nodes.filter(n => n.status === "offline" && !n.maintenance).map(n => ({
      node: n, devices: ctx.devices.filter(d => d.node_id === n.uuid),
      detail: `${n.hostname} stopped without a shutdown request. Volumes primary on this node have failed over to their secondaries.`}))},
  {rule: "storage_node_unreachable", severity: "critical", scope: "cluster",
    title: "Storage node unreachable",
    remedy: "Check the host and the management network, then restart the node.",
    eval: (c, ctx) => ctx.nodes.filter(n => n.status === "unreachable" || n.status === "down").map(n => ({
      node: n, devices: ctx.devices.filter(d => d.node_id === n.uuid),
      detail: `No heartbeat from ${n.hostname} on ${n.mgmt_ip}. Its devices cannot be reached, so their chunks are served with reduced redundancy.`}))},
  {rule: "device_unavailable", severity: "critical", scope: "cluster",
    title: "Device unavailable",
    remedy: "Restart the device. If it does not come back, fail it so the cluster rebuilds its chunks elsewhere.",
    eval: (c, ctx) => {
      // grouped per node: one lamp per node, naming every affected device
      const byNode = {};
      ctx.devices.filter(d => d.status === "unavailable").forEach(d => (byNode[d.node_id] = byNode[d.node_id] || []).push(d));
      return Object.keys(byNode).map(nid => {
        const n = ctx.nodes.find(y => y.uuid === nid), ds = byNode[nid];
        // a device that is unavailable only because its node is down is not a
        // separate fault; the node alert already says it
        if (n && n.status !== "online" && n.status !== "read_only") return null;
        return {node: n, devices: ds,
          detail: `${ds.length} device(s) on ${n ? n.hostname : "an unknown node"} report gone to SPDK: ${ds.map(d => d.serial_number).join(", ")}.`};
      }).filter(Boolean);
    }},
  {rule: "metadata_compaction_failed", severity: "critical", scope: "cluster",
    title: "Metadata compaction failed",
    remedy: "Inspect the SPDK log on the node, free metadata capacity, then let compaction retry.",
    eval: (c, ctx) => ctx.nodes.filter(n => n.meta_compaction_failed).map(n => ({
      node: n, devices: ctx.devices.filter(d => d.node_id === n.uuid && d.status === "online").slice(0, 2),
      detail: `The last compaction pass on ${n.hostname} did not finish. Metadata keeps growing until it succeeds — at ${Math.round(pctOf(n.meta_size_util, n.meta_size_total) * 100)}% of the metadata arena.`}))},
  {rule: "metadata_capacity_critical", severity: "critical", scope: "cluster",
    title: "Metadata capacity critical",
    remedy: "Free logical volumes or snapshots on this node, or add capacity. Writes stop when the arena is full.",
    eval: (c, ctx) => ctx.nodes.filter(n => pctOf(n.meta_size_util, n.meta_size_total) > .9).map(n => ({
      node: n, devices: ctx.devices.filter(d => d.node_id === n.uuid && d.status === "online").slice(0, 2),
      detail: `The metadata arena on ${n.hostname} is ${Math.round(pctOf(n.meta_size_util, n.meta_size_total) * 100)}% full.`}))},
  {rule: "cluster_degraded", severity: "critical", scope: "cluster",
    title: "Cluster degraded",
    remedy: "Bring the affected nodes back online and let rebalancing finish.",
    eval: (c, ctx) => {
      if (c.status !== "degraded") return [];
      const down = ctx.nodes.filter(n => n.status !== "online" && n.status !== "read_only");
      // a degradation that is nothing but a maintenance shutdown is expected
      if (down.length && down.every(n => n.maintenance)) return [];
      return [{node: down[0] || null, devices: down.length === 1 ? ctx.devices.filter(d => d.node_id === down[0].uuid).slice(0, 6) : [],
        nodes: down,
        detail: `${down.length} of ${ctx.nodes.length} storage nodes are not online${down.length ? ": " + down.map(n => n.hostname).join(", ") : ""}. Capacity is served with reduced redundancy.`}];
    }},
  {rule: "slow_node", severity: "warning", scope: "cluster",
    title: "Slow node",
    remedy: "Check the node's SPDK thread utilization and its devices' latency; consider failing a slow device.",
    eval: (c, ctx) => {
      const live = ctx.nodes.filter(n => n.status === "online");
      if (live.length < 2) return [];
      const avg = live.reduce((s, n) => s + (n.latency_us || 0), 0) / live.length;
      return live.filter(n => n.latency_us > avg * 1.8).map(n => ({
        node: n, devices: ctx.devices.filter(d => d.node_id === n.uuid && d.status === "online").sort((a, b) => b.temperature_c - a.temperature_c).slice(0, 3),
        detail: `${n.hostname} answers at ${n.latency_us} µs against a cluster average of ${Math.round(avg)} µs. Volumes primary on this node see the difference.`}));
    }},
  {rule: "object_limit_reached", severity: "critical", scope: "cluster",
    title: "Object limit reached",
    remedy: "Move volumes to another node, or raise max-subsystems and restart the node.",
    eval: (c, ctx) => ctx.nodes.filter(n => pctOf(n.objects_used, n.objects_max) > .95).map(n => ({
      node: n, devices: [],
      detail: `${n.hostname} holds ${n.objects_used} of ${n.objects_max} objects. No new volume, snapshot or clone can be created on this node.`}))},

  {rule: "capacity_warning", severity: "warning", scope: "cluster",
    title: "Cluster capacity warning",
    remedy: "Expand the cluster with another node, or free provisioned capacity.",
    eval: c => {
      const p = pctOf(c.size_util, c.size_total);
      return p > .75 && p <= .9 ? [{detail: `${Math.round(p * 100)}% of ${(c.size_total / 1e12).toFixed(1)} TB is utilized.`}] : [];
    }},
  {rule: "capacity_critical", severity: "critical", scope: "cluster",
    title: "Cluster capacity critical",
    remedy: "Expand the cluster now. The cluster switches to read-only when it fills.",
    eval: c => {
      const p = pctOf(c.size_util, c.size_total);
      return p > .9 ? [{detail: `${Math.round(p * 100)}% of ${(c.size_total / 1e12).toFixed(1)} TB is utilized.`}] : [];
    }},
  {rule: "cluster_read_only", severity: "critical", scope: "cluster",
    title: "Cluster switched to read-only",
    remedy: "Free or add capacity. Writes resume once the cluster is below its threshold.",
    eval: c => c.status === "read_only" ? [{detail: "Every volume in this cluster refuses writes. Reads are unaffected."}] : []},
  {rule: "cluster_suspended", severity: "warning", scope: "cluster",
    title: "Cluster suspended",
    remedy: "Restart the cluster to resume serving volumes.",
    eval: c => c.status === "suspended" ? [{detail: "All storage nodes are stopped and no volume in this cluster is being served."}] : []},

  {rule: "fdb_backup_failed", severity: "critical", scope: "control-plane",
    title: "State DB backup failed",
    remedy: "Check the backup target and the fdb containers, then take a backup manually.",
    eval: () => X.fdb_backups.some(b => b.status === "failed")
      ? [{detail: `The most recent FoundationDB backup did not complete. Last good version: ${(X.fdb_backups.find(b => b.status === "complete") || {}).version || "none"}.`}] : []},
  {rule: "fdb_degraded", severity: "critical", scope: "control-plane",
    title: "State DB degraded",
    remedy: "Restart the failed fdb container. The control plane cannot accept writes while the state DB is degraded.",
    eval: () => X.containers.some(c => c.group === "state db" && c.state !== "running")
      ? [{detail: `${X.containers.filter(c => c.group === "state db" && c.state !== "running").map(c => c.name).join(", ")} not running.`}] : []},
  {rule: "fdb_capacity_critical", severity: "critical", scope: "control-plane",
    title: "State DB capacity critical",
    remedy: "Grow the fdb volumes or trim old task and log records.",
    eval: () => X.containers.filter(c => c.group === "state db" && c.disk_limit && c.disk_used / c.disk_limit > .9)
      .map(c => ({detail: `${c.name} is at ${Math.round(c.disk_used / c.disk_limit * 100)}% of its ${Math.round(c.disk_limit / 1e9)} GB volume.`, container: c.name}))},
  {rule: "webapi_degraded", severity: "warning", scope: "control-plane",
    title: "Web API degraded",
    remedy: "Check the simplyblock-core and webapp containers; the console and the CSI driver both talk through this API.",
    eval: () => X.containers.filter(c => (c.name === "simplyblock-core" || c.name === "simplyblock-webapp") && (c.state !== "running" || c.restarts > 3))
      .map(c => ({detail: c.state !== "running" ? `${c.name} is ${c.state}.` : `${c.name} has restarted ${c.restarts} times.`, container: c.name}))}
];

// Evaluate every rule in a scope and return the lamps that are lit.
function evalAlerts(scope, cluster) {
  const out = [];
  ALERT_RULES.filter(r => r.scope === scope).forEach(r => {
    const ctx = cluster ? {
      nodes: window.SB_DB.storage_nodes.filter(n => n.cluster_id === cluster.uuid),
      devices: window.SB_DB.devices.filter(d => d.cluster_id === cluster.uuid)
    } : {nodes: [], devices: []};
    let hits = [];
    try { hits = r.eval(cluster, ctx) || []; } catch (e) { hits = []; }
    hits.forEach(h => {
      const node = h.node || null;
      const key = silKey(r.rule, node ? node.uuid : (h.container || null), cluster ? cluster.uuid : null);
      out.push({
        uuid: key, rule: r.rule, severity: r.severity, scope: r.scope,
        cluster_id: cluster ? cluster.uuid : null,
        cluster_name: cluster ? cluster.name : null,
        title: r.title, detail: h.detail, remedy: r.remedy,
        node_id: node ? node.uuid : null, node_name: node ? node.hostname : null,
        node_ids: (h.nodes || (node ? [node] : [])).map(n => n.uuid),
        node_names: (h.nodes || (node ? [node] : [])).map(n => n.hostname),
        device_ids: (h.devices || []).map(d => d.uuid),
        device_names: (h.devices || []).map(d => d.serial_number || d.device_name),
        container: h.container || null,
        since: seenAt(key),
        silenced: !!X.silences[key], silenced_by: X.silences[key] || null
      });
    });
  });
  // Forget the first-sighting timestamp and the silence of a condition that has
  // cleared, so the lamp starts fresh when it lights again. The sweep may only
  // touch keys belonging to the scope just evaluated — this function is called
  // once per cluster, and a cluster must not garbage-collect its neighbours.
  const live = new Set(out.map(a => a.uuid));
  const mine = k2 => k2.split("|")[1] === (cluster ? cluster.uuid : "cp");
  Object.keys(X.alert_seen).forEach(k2 => { if (mine(k2) && !live.has(k2)) delete X.alert_seen[k2]; });
  Object.keys(X.silences).forEach(k2 => { if (mine(k2) && !live.has(k2)) delete X.silences[k2]; });
  return out;
}
X.evalAlerts = evalAlerts;
X.alertRuleCounts = {cluster: ALERT_RULES.filter(r => r.scope === "cluster").length,
  "control-plane": ALERT_RULES.filter(r => r.scope === "control-plane").length};

// ---- live drift -----------------------------------------------------------
function extrasTick() {
  X.containers.forEach(c => {
    if (c.state !== "running") return;
    c.cpu_pct = Math.max(0, +(c.cpu_pct * (1 + (Math.random() - .5) * .3)).toFixed(1));
    c.mem_used = Math.min(c.mem_limit, Math.round(c.mem_used * (1 + (Math.random() - .5) * .04)));
  });
  Object.values(X.spdk_threads).forEach(s => s.threads.forEach(t => {
    if (!t.busy_pct) return;
    t.busy_pct = Math.min(99.9, Math.max(0.5, +(t.busy_pct + (Math.random() - .5) * 9).toFixed(1)));
    t.poll_count += xint(1000, 90000);
  }));
  X.tasks.forEach(t => {
    if (t.status === "running" && !t.canceled) t.updated_at = xago(Math.random() * .3);
  });
  X.storage_nodes.forEach(n => {
    if (n.status !== "online") return;
    n.memory_used = Math.min(n.memory_total, Math.round(n.memory_used * (1 + (Math.random() - .5) * .03)));
  });
}

// ---- routes ---------------------------------------------------------------
const xok = b => new Response(JSON.stringify(Object.assign({status: true}, b)), {status: 200, headers: {"Content-Type": "application/json"}});
const xfail = (c, m) => new Response(JSON.stringify({status: false, error: m}), {status: c, headers: {"Content-Type": "application/json"}});

const API_EXTRA = [
  ["GET", /^\/clusters\/([\w-]+)\/tasks$/, m => {
    const c = X.clusters.find(x => x.uuid === m[1]);
    if (!c) return {__404: true};
    if (!c.capabilities.tasks) return {__cap: "This is an edge cluster. Edge deployments run no task engine — long-running operations are executed directly by the Kubernetes operator."};
    return {results: X.tasks.filter(t => t.cluster_id === m[1] && !t.parent_id)};
  }],
  ["GET", /^\/tasks\/([\w-]+)\/subtasks$/, m => ({results: X.tasks.filter(t => t.parent_id === m[1])})],
  ["POST", /^\/tasks\/([\w-]+)\/cancel$/, m => {
    const t = X.tasks.find(x => x.uuid === m[1]);
    if (!t) return {__404: true};
    if (t.status === "done") return {__err: "Completed tasks cannot be cancelled"};
    t.status = "suspended"; t.canceled = true; t.updated_at = xago(0);
    X.tasks.filter(s => s.parent_id === t.uuid && s.status !== "done").forEach(s => { s.status = "suspended"; s.canceled = true; });
    return {results: [t]};
  }],
  ["GET", /^\/clusters\/([\w-]+)\/logs$/, m => ({results: X.logs.filter(l => l.cluster_id === m[1])})],
  ["GET", /^\/clusters\/([\w-]+)\/alerts$/, m => {
    const c = window.SB_DB.clusters.find(x2 => x2.uuid === m[1]);
    return {results: c ? X.evalAlerts("cluster", c) : []};
  }],
  ["GET", /^\/alerts$/, () => ({results: window.SB_DB.clusters.flatMap(c => X.evalAlerts("cluster", c)).concat(X.evalAlerts("control-plane", null))})],
  ["GET", /^\/control-plane\/alerts$/, () => ({results: X.evalAlerts("control-plane", null)})],
  ["POST", /^\/alerts\/(.+)\/silence$/, m => { X.silences[decodeURIComponent(m[1])] = "ops@simplyblock.io"; return {results: []}; }],
  ["POST", /^\/alerts\/(.+)\/unsilence$/, m => { delete X.silences[decodeURIComponent(m[1])]; return {results: []}; }],
  // ---- S3 buckets: one bucket is one logical volume ----
  ["POST", /^\/clusters\/([\w-]+)\/buckets$/, (m, b) => {
    const c = X.clusters.find(x => x.uuid === m[1]);
    if (!c) return {__404: true};
    if (!c.object_storage || !c.object_storage.enabled) return {__cap: "Object storage is not enabled on this cluster."};
    if (!b.name || !/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(b.name))
      return {__err: "Bucket names are 3–63 characters of lowercase letters, digits, dots and hyphens (S3 naming rules)."};
    if (X.buckets.some(x => x.name === b.name)) return {__err: `A bucket named ${b.name} already exists — bucket names are unique across the endpoint.`};
    const pool = X.pools.find(p => p.uuid === b.pool_id) || X.pools.find(p => p.cluster_id === c.uuid);
    if (!pool) return {__err: "No pool to provision the bucket's volume in."};
    const tmpl = X.lvols.find(v => v.cluster_id === c.uuid && v.status === "online") || X.lvols[0];
    const v = JSON.parse(JSON.stringify(tmpl));
    v.uuid = xuuid(); v.lvol_name = `s3-${b.name}`; v.pool_id = pool.uuid; v.pool_name = pool.pool_name;
    v.size_prov = Number(b.size) || 1e12; v.size_util = 0; v.status = "online";
    v.pvc = null; v.bucket = null; v.replication = null; v.consistency_group = null; v.crypto_enabled = !!b.encryption;
    v.created_at = xago(0);
    X.lvols.push(v);
    const bucket = {uuid: xuuid(), cluster_id: c.uuid, name: b.name,
      lvol_id: v.uuid, lvol_name: v.lvol_name, pool_id: pool.uuid, pool_name: pool.pool_name,
      status: "online", versioning: !!b.versioning, object_lock: !!b.object_lock,
      quota_bytes: Number(b.quota) || 0, objects: 0, size_bytes: 0,
      region: c.object_storage.region, storage_class: b.storage_class || "standard",
      owner: b.owner || b.namespace || "default",
      tags: b.tags || {}, lifecycle_rules: [], cors_enabled: false,
      access: {service_account: b.service_account || `sb-s3-${b.name}`, namespace: b.namespace || "default",
        secret_name: `${b.name}-s3-credentials`, access_key_id: `SB${xhex(9).toUpperCase()}`,
        policy: b.policy || "read-write", public: false},
      created_at: xago(0)};
    X.buckets.push(bucket);
    v.bucket = {uuid: bucket.uuid, name: bucket.name};
    XU.rollup();
    return {results: [bucket]};
  }],
  ["DELETE", /^\/buckets\/([\w-]+)$/, m => {
    const i = X.buckets.findIndex(x => x.uuid === m[1]);
    if (i < 0) return {__404: true};
    const b = X.buckets[i];
    if (b.objects > 0) return {__err: `${b.name} still holds ${b.objects} object(s). S3 refuses to delete a non-empty bucket — empty it first.`};
    if (b.object_lock) return {__err: "Object lock is enabled — the bucket cannot be deleted while a retention configuration exists."};
    X.buckets.splice(i, 1);
    X.lvols = X.lvols.filter(v => v.uuid !== b.lvol_id);
    (X.dr_policies || []).forEach(p => { p.lvol_ids = p.lvol_ids.filter(id => id !== b.lvol_id); });
    XU.rollup();
    return {results: []};
  }],
  ["PUT", /^\/buckets\/([\w-]+)$/, (m, b) => {
    const x = X.buckets.find(y => y.uuid === m[1]);
    if (!x) return {__404: true};
    if (x.object_lock && b.object_lock === false) return {__err: "Object lock cannot be disabled once enabled (S3 semantics)."};
    if (b.versioning !== undefined) x.versioning = !!b.versioning;
    if (b.object_lock !== undefined) x.object_lock = !!b.object_lock;
    if (b.quota !== undefined) x.quota_bytes = Number(b.quota) || 0;
    if (b.storage_class) x.storage_class = b.storage_class;
    return {results: [x]};
  }],
  ["PUT", /^\/buckets\/([\w-]+)\/tags$/, (m, b) => {
    const x = X.buckets.find(y => y.uuid === m[1]);
    if (!x) return {__404: true};
    const tags = b.tags || {};
    if (Object.keys(tags).length > 50) return {__err: "S3 allows at most 50 tags per bucket."};
    x.tags = tags;
    return {results: [x]};
  }],
  ["PUT", /^\/buckets\/([\w-]+)\/access$/, (m, b) => {
    const x = X.buckets.find(y => y.uuid === m[1]);
    if (!x) return {__404: true};
    Object.assign(x.access, {namespace: b.namespace || x.access.namespace, service_account: b.service_account || x.access.service_account,
      policy: b.policy || x.access.policy, public: !!b.public});
    if (b.rotate_key) x.access.access_key_id = `SB${xhex(9).toUpperCase()}`;
    return {results: [x]};
  }],
  ["POST", /^\/buckets\/([\w-]+)\/resize$/, (m, b) => {
    const x = X.buckets.find(y => y.uuid === m[1]);
    if (!x) return {__404: true};
    const v = X.lvols.find(y => y.uuid === x.lvol_id);
    if (v && Number(b.size) < v.size_prov) return {__err: "A bucket's filesystem can only grow."};
    if (v) v.size_prov = Number(b.size);
    XU.rollup();
    return {results: [x]};
  }],
  // Replication: the bucket is its volume, so attaching it to a policy is
  // attaching the volume. Only policies whose source is this cluster qualify.
  ["POST", /^\/buckets\/([\w-]+)\/replicate$/, (m, b) => {
    const x = X.buckets.find(y => y.uuid === m[1]);
    if (!x) return {__404: true};
    const pol = (X.dr_policies || []).find(p => p.uuid === b.policy_id);
    if (!pol) return {__404: true};
    if (pol.source_cluster_id !== x.cluster_id) return {__err: `${pol.name} replicates from another cluster — a bucket can only join a policy whose source is its own cluster.`};
    const v = X.lvols.find(y => y.uuid === x.lvol_id);
    if (!v) return {__404: true};
    if (v.replication) return {__err: `${x.name} is already replicated by ${v.replication.policy_name}. Detach it first.`};
    pol.lvol_ids.push(v.uuid);
    v.replication = {policy_id: pol.uuid, policy_name: pol.name, mode: pol.mode, status: "healthy",
      last_replication_at: xago(0), backlog_bytes: 0, target_cluster_id: pol.target_cluster_id};
    XU.rollup();
    return {results: [x]};
  }],
  ["POST", /^\/buckets\/([\w-]+)\/unreplicate$/, m => {
    const x = X.buckets.find(y => y.uuid === m[1]);
    if (!x) return {__404: true};
    const v = X.lvols.find(y => y.uuid === x.lvol_id);
    if (!v || !v.replication) return {__err: "The bucket is not replicated."};
    (X.dr_policies || []).forEach(p => { p.lvol_ids = p.lvol_ids.filter(id => id !== v.uuid); });
    v.replication = null;
    XU.rollup();
    return {results: [x]};
  }]
];

const AGENT = [
  ["GET", /^\/control-plane\/containers$/, () => ({results: X.containers})],
  ["GET", /^\/control-plane\/containers\/([\w.-]+)\/logs$/, m => ({results: X.container_logs[m[1]] || []})],
  ["GET", /^\/control-plane\/fdb\/backups$/, () => ({results: X.fdb_backups})],
  ["POST", /^\/control-plane\/fdb\/backups\/([\w-]+)\/restore$/, m => {
    const b = X.fdb_backups.find(x => x.id === m[1]);
    if (!b) return {__404: true};
    if (b.status !== "complete") return {__err: "Backup is still being written"};
    b.restore_requested_at = xago(0);
    return {results: [b]};
  }],
  ["GET", /^\/nodes\/([\w-]+)\/logs\/([\w-]+)$/, m => {
    const key = m[1] + "/" + m[2];
    const buf = X.spdk_logs[key];
    if (!buf) return {__404: true};
    for (let i = 0; i < xint(1, 4); i++) { buf.unshift(spdkLine(0, m[2] === "spdk-proxy")); buf.pop(); }
    return {results: buf};
  }],
  ["GET", /^\/devices\/([\w-]+)\/smart$/, m => X.smart[m[1]] ? {results: [X.smart[m[1]]]} : {__404: true}],
  ["POST", /^\/devices\/([\w-]+)\/smart\/refresh$/, m => {
    const r = X.runHealthCheck(m[1]);
    return r ? {results: [r]} : {__404: true};
  }]
];

const prevFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const cfg = window.SB_CONFIG;
  if (!cfg.mock) return prevFetch(input, init);
  const url = typeof input === "string" ? input : input.url;
  const method = ((init && init.method) || "GET").toUpperCase();
  const M = window.SB_MOCK;

  const table = url.startsWith(cfg.agentBase) ? {base: cfg.agentBase, routes: AGENT}
    : url.startsWith(cfg.operatorBase + "/proposed") ? {base: cfg.operatorBase + "/proposed", routes: API_EXTRA} : null;
  const isProm = url.startsWith(cfg.promBase);
  if (!table && !isProm) return prevFetch(input, init);

  if (isProm) {
    await new Promise(r => setTimeout(r, M.latency[0] + Math.random() * (M.latency[1] - M.latency[0])));
    if (M.offline) return xfail(0, "Prometheus unreachable (mock offline)");
    extrasTick();
    const q = decodeURIComponent((url.split("query=")[1] || "").split("&")[0]);
    const nodeId = (q.match(/node="([\w-]+)"/) || [])[1];
    const s = X.spdk_threads[nodeId];
    if (!s) return xok({data: {resultType: "vector", result: []}});
    return xok({data: {resultType: "vector", result: s.threads.map(t => ({
      metric: {__name__: "spdk_thread_busy_percent", node: nodeId, thread: t.name, core: String(t.core)},
      value: [Date.now() / 1000, String(t.busy_pct)]
    }))}});
  }

  // The client now scopes child collections with ?scope=<parent>&scopeId=<id>.
  // These routes were written against the nested form, so fold the query back
  // into it and keep their logic — including the edge-cluster capability gates.
  const [rawRoute, rawQ] = url.slice(table.base.length).split("?");
  const q = new URLSearchParams(rawQ || "");
  const scope = q.get("scope"), scopeId = q.get("scopeId");
  const route = (scope && scopeId
    ? `/${scope}/${scopeId}${rawRoute}`
    : rawRoute).replace(/\/$/, "");
  let body = {};
  try { if (init && init.body) body = JSON.parse(init.body); } catch (e) {}
  const hit = table.routes.find(([mm, re]) => mm === method && re.test(route));
  if (!hit) return prevFetch(input, init);

  M.requests++;
  await new Promise(r => setTimeout(r, M.latency[0] + Math.random() * (M.latency[1] - M.latency[0])));
  if (M.offline) return xfail(0, "Control plane unreachable (mock offline)");
  if (M.failNext) { M.failNext = false; return xfail(503, "Upstream returned 503 (injected)"); }
  if (M.failRate && Math.random() < M.failRate) return xfail(503, "Upstream returned 503");
  extrasTick();
  const r = hit[2](route.match(hit[1]), body);
  if (r.__404) return xfail(404, "Resource not found");
  if (r.__cap) return xfail(501, r.__cap);
  if (r.__err) return xfail(409, r.__err);
  return xok(M.forceEmpty && r.results ? {results: []} : r);
};

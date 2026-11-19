// ---------------------------------------------------------------------------
// CONTAINERS, POD LOGS, DEVICE HEALTH, PROMETHEUS
//
// What used to be read from an operator-side "agent" is read from where it
// lives: the control plane's pods and the storage plane's pods are Kubernetes
// Pods, their logs are the pod-log subresource, a device's SMART report is the
// control plane's health-info, and SPDK thread utilization is a Prometheus
// query. The `agent` name stays so the panels that call it do not move.
//
// Data read from a container's stdout or from Prometheus carries a SourceTag:
// it has a different availability guarantee than an object the API server
// stores.
// ---------------------------------------------------------------------------
const PROM_BASE = window.SB_CONFIG.promBase;

async function promreq(path) {
  let res;
  try { res = await fetch(PROM_BASE + path, {headers: {Accept: "application/json"}}); }
  catch (e) { throw new ApiError(0, "Prometheus unreachable", path, "Unreachable"); }
  let body = null;
  try { body = await res.json(); } catch (e) {}
  if (!res.ok || (body && body.status === "error")) throw new ApiError(res.status, (body && body.error) || "Request failed", path);
  return body;
}

// ---- pod log text -> lines --------------------------------------------------
// Lines arrive with the kubelet's RFC 3339 timestamp first. The level is read
// from the line itself: python logging (INFO, WARNING, ...), SPDK
// ("*ERROR*:", "[2026-...] module.c:12:fn: *WARNING*:"), klog ("I1117 ..."),
// and JSON lines; anything else has no level and renders muted.
const LEVEL_MAP = {DEBUG: "DEBUG", INFO: "INFO", NOTICE: "NOTICE", WARNING: "WARN", WARN: "WARN", ERROR: "ERROR", CRITICAL: "ERROR", FATAL: "ERROR"};
function levelOf(msg) {
  const head = msg.slice(0, 200);
  let m = /\*(ERROR|WARNING|NOTICE|INFO|DEBUG)\*/.exec(head) || /\b(DEBUG|INFO|NOTICE|WARNING|WARN|ERROR|CRITICAL|FATAL)\b/.exec(head);
  if (m) return LEVEL_MAP[m[1]] || m[1];
  m = /^([IWEF])\d{4} \d{2}:\d{2}:\d{2}/.exec(head);
  if (m) return {I: "INFO", W: "WARN", E: "ERROR", F: "ERROR"}[m[1]];
  if (head.startsWith("{")) { try { const j = JSON.parse(msg); const l = String(j.level || j.severity || j.lvl || "").toUpperCase(); if (l) return LEVEL_MAP[l] || l; } catch (e) {} }
  return "";
}
function parsePodLog(text, meta) {
  return String(text || "").split("\n").filter(Boolean).map(line => {
    const sp = line.indexOf(" ");
    const tsRaw = sp > 0 ? line.slice(0, sp) : "";
    const ts = Date.parse(tsRaw);
    const msg = isNaN(ts) ? line : line.slice(sp + 1);
    // `ts` is the ISO string the panels format; `tsMs` is for sorting and windows
    return Object.assign({ts: isNaN(ts) ? "" : new Date(ts).toISOString(), tsMs: isNaN(ts) ? null : ts, level: levelOf(msg), msg, raw: line}, meta || {});
  });
}

// ---- control plane pods ------------------------------------------------------
// One entry per container, named pod/container, so the log picker can address
// a single container of a multi-container pod (simplyblock-monitoring runs ten).
const POD_INDEX = {};
const CP_APPS = /^simplyblock-(admin-control|webappapi|monitoring|tasks|fdb|operator|graylog|thanos|grafana|minio|mongo)/;
const OBS_APPS = /^simplyblock-(graylog|thanos|grafana|minio|mongo)/;
const normContainer = (ctx, pod, c) => {
  const st = (pod.status || {}).containerStatuses || [];
  const cs = st.find(x => x.name === c.name) || {};
  const state = cs.state ? Object.keys(cs.state)[0] : ((pod.status || {}).phase || "unknown").toLowerCase();
  const started = cs.state && cs.state.running ? Date.parse(cs.state.running.startedAt) : null;
  const req = (c.resources || {}).requests || {}, lim = (c.resources || {}).limits || {};
  const app = (pod.metadata.labels || {}).app || pod.metadata.name.replace(/-[a-z0-9]+-[a-z0-9]+$/, "");
  const name = `${pod.metadata.name}/${c.name}`;
  POD_INDEX[name] = {ctx, namespace: pod.metadata.namespace, pod: pod.metadata.name, container: c.name};
  return {
    name, pod: pod.metadata.name, container: c.name, ctx, namespace: pod.metadata.namespace,
    group: OBS_APPS.test(app) ? "observability" : (pod.metadata.labels || {})["foundationdb.org/fdb-cluster-name"] ? "state database" : "control plane",
    app, image: c.image, state, ready: !!cs.ready, restarts: cs.restartCount || 0,
    uptimeH: started ? Math.max(0, Math.round((Date.now() - started) / 36e5)) : null,
    node: (pod.spec || {}).nodeName || null,
    cpu: {request: parseQuantity(req.cpu), limit: parseQuantity(lim.cpu)},
    mem: {request: parseQuantity(req.memory), limit: parseQuantity(lim.memory)},
    reason: cs.state && (cs.state.waiting || cs.state.terminated) ? (cs.state.waiting || cs.state.terminated).reason : null
  };
};

const agent = {
  // every container of the control plane's pods in the console's namespace
  containers: async ctx => {
    const c = ctx || LOCAL_CTX;
    const pods = await k8s.list("Pod", {ctx: c, namespace: nsOfCtx(c)});
    return pods.filter(p => CP_APPS.test(p.metadata.name) || (p.metadata.annotations || {})[LABELS.logCollector] === "true"
      || (p.metadata.labels || {})["foundationdb.org/fdb-cluster-name"])
      .flatMap(p => ((p.spec || {}).containers || []).map(ct => normContainer(c, p, ct)))
      .sort((a, b) => a.group.localeCompare(b.group) || a.name.localeCompare(b.name));
  },
  containerLogs: (name, opts) => {
    const ix = POD_INDEX[name];
    if (!ix) return Promise.reject(new ApiError(404, `container ${name} is not known`, "/" + name, "NotFound"));
    return k8s.logs(ix.pod, Object.assign({ctx: ix.ctx, namespace: ix.namespace, container: ix.container, tailLines: 400, timestamps: true}, opts || {}))
      .then(t => parsePodLog(t, {pod: ix.pod, container: ix.container}));
  },
  // FoundationDB backups have no API yet
  fdbBackups: () => Promise.reject(new ApiError(503, "FoundationDB backups are not part of this deployment: no API serves them", "/fdb", REASON_FEATURE_DISABLED)),
  fdbRestore: () => Promise.reject(new ApiError(503, "FoundationDB restore is not part of this deployment: no API serves it", "/fdb", REASON_FEATURE_DISABLED)),
  // the storage plane pods of one node, and one container's log
  nodePods: nid => { const n = REG[nid]; return n ? sources.nodePods(n.ctx, n) : Promise.resolve([]); },
  nodeLogs: async (nid, stream, opts) => {
    const n = REG[nid];
    if (!n) throw new ApiError(404, "storage node is not known", "/" + nid, "NotFound");
    const pods = await sources.nodePods(n.ctx, n);
    const want = stream === "spdk-proxy" ? ["spdk-proxy-container", "spdk-proxy"] : stream === "snode-api" ? ["s-node-api-container", "snode-api"] : ["spdk-container", "spdk"];
    const kind = stream === "snode-api" ? "node-api" : "spdk";
    const pod = pods.find(p => p.kind === kind && p.containers.some(c => want.includes(c))) || pods.find(p => p.kind === kind);
    if (!pod) throw new ApiError(503, `no ${kind === "spdk" ? "SPDK" : "node API"} pod is running on ${n.hostname}; there is nothing to read until it starts`, "/" + nid, "NoPod");
    const container = pod.containers.find(c => want.includes(c)) || pod.containers[0];
    const text = await sources.podLog(n.ctx, pod.namespace, pod.name, Object.assign({container, tailLines: 400}, opts || {}));
    return parsePodLog(text, {pod: pod.name, container});
  },
  // the device's NVMe SMART report, from the control plane, in the shape the SmartCard renders
  smart: async did => {
    const d = REG[did];
    if (!d) throw new ApiError(404, "device is not known", "/" + did, "NotFound");
    if (!d.nodeId || !d.clusterId) return {unavailable: true, checked_at: null};
    const h = await cp.healthInfo(d.ctx, d.clusterId, d.nodeId, d.id);
    const n = v => (v === null || v === undefined ? "—" : String(v));
    const spareNote = h.spare !== null && h.spareThreshold !== null && h.spare < h.spareThreshold ? `below the ${h.spareThreshold}% threshold` : "";
    return {
      overall: h.status === "critical" ? "FAILED" : h.status === "warn" ? "PASSED (warnings)" : "PASSED",
      model: h.model, firmware: h.firmware, checked_at: new Date().toISOString(),
      verdict: h.status === "critical" ? "unhealthy" : h.status === "warn" ? "degraded" : "good",
      attributes: [
        {name: "Critical warning", value: n(h.criticalWarning), note: h.criticalWarning ? "a critical warning bit is set" : ""},
        {name: "Temperature", value: h.tempC !== null ? `${h.tempC} °C` : "—", note: h.tempC > 70 ? "above 70 °C" : ""},
        {name: "Available spare", value: h.spare !== null ? `${h.spare}%` : "—", note: spareNote},
        {name: "Spare threshold", value: h.spareThreshold !== null ? `${h.spareThreshold}%` : "—"},
        {name: "Percentage used", value: h.pctUsed !== null ? `${h.pctUsed}%` : "—", note: h.pctUsed > 80 ? "past 80% of rated endurance" : ""},
        {name: "Data units read", value: n(h.unitsRead)}, {name: "Data units written", value: n(h.unitsWritten)},
        {name: "Host read commands", value: n(h.hostReads)}, {name: "Host write commands", value: n(h.hostWrites)},
        {name: "Controller busy time", value: n(h.busyTime)}, {name: "Power cycles", value: n(h.powerCycles)},
        {name: "Power-on hours", value: n(h.powerOnHours)}, {name: "Unsafe shutdowns", value: n(h.unsafeShutdowns)},
        {name: "Media errors", value: n(h.mediaErrors), note: h.mediaErrors > 0 ? "media errors recorded" : ""},
        {name: "Error log entries", value: n(h.errLogEntries)},
        {name: "Warning temperature time", value: h.warnTempMin !== null ? `${h.warnTempMin} min` : "—"},
        {name: "Critical temperature time", value: h.critTempMin !== null ? `${h.critTempMin} min` : "—"}
      ], raw: h
    };
  },
  // there is no route to trigger a fresh read; a re-read is a refetch
  smartRefresh: did => agent.smart(did),
  // Prometheus: spdk_thread_busy_percent{node="<uuid>"}
  spdkThreads: nid => promreq(`/query?query=${encodeURIComponent(`spdk_thread_busy_percent{node="${nid}"}`)}`)
    .then(d => (d.data.result || []).map(r => ({
      name: r.metric.thread, core: Number(r.metric.core), busy: Number(r.value[1])
    })).sort((a, b) => a.core - b.core || a.name.localeCompare(b.name)))
};

const SourceTag = ({what}) => (
  <span className="srctag" title={`Read from ${what}, not from an object the API server stores`}><Icon n="alert" s={10} />{what}</span>
);

Object.assign(window, {agent, PROM_BASE, SourceTag, normContainer, parsePodLog, levelOf, POD_INDEX});

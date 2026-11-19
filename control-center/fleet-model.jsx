// ---------------------------------------------------------------------------
// FLEET MODEL — identity, grouping, and the joins between kinds
//
// The console shows many storage clusters across many Kubernetes contexts,
// so every module needs the same answers to the same questions: how is an
// object identified across contexts, which site, stage, and tenant does a
// cluster belong to, and how does one kind point at another. Those answers
// live here, as pure functions over raw objects, so the sources, the
// dashboard, the alert engine, and the CR views agree.
//
// Identity: storage entities keep the control plane UUID as their id (it is
// unique across the fleet); objects without one are addressed by
// objKey(ctx, ns, kind, name).
//
// Site, stage, tenant precedence (documented in README.md, "Fleet labels"):
//   1. StorageCluster labels storage.simplyblock.io/site|stage|tenant
//      (site may be comma-separated for a stretched cluster)
//   2. the same labels on the cluster's Namespace (tenant defaults to the
//      namespace name)
//   3. the context's configured labels
//   4. site only: the topology.kubernetes.io/zone (then region) labels of the
//      cluster's worker Nodes; several zones mean a stretched cluster
// Every derived value carries where it came from.
// ---------------------------------------------------------------------------
const objKey = (ctx, ns, kind, name) => `${ctx || LOCAL_CTX}/${ns || ""}/${kind}/${name}`;
const keyOf = (ctx, o) => objKey(ctx, (o.metadata || {}).namespace, o.kind, (o.metadata || {}).name);
const LABELS = {
  site: "storage.simplyblock.io/site", stage: "storage.simplyblock.io/stage", tenant: "storage.simplyblock.io/tenant",
  zone: "topology.kubernetes.io/zone", region: "topology.kubernetes.io/region",
  nodeType: "io.simplyblock.node-type", nodeSet: "io.simplyblock.storagenodeset",
  scNamespace: "storage.simplyblock.io/namespace", scCluster: "storage.simplyblock.io/cluster", scPool: "storage.simplyblock.io/pool",
  drainNode: "storage.simplyblock.io/drain-node", worker: "storage.simplyblock.io/worker",
  backupPolicy: "simplyblock.io/backup-policy", replicationPolicy: "storage.simplyblock.io/replication-policy",
  selectedNode: "simplyblock.io/selected-storage-node", onDiskFs: "storage.simplyblock.io/on-disk-filesystem",
  logCollector: "log-collector/enabled"
};
const STAGE_ORDER = ["DEV", "TEST", "QA", "UAT", "STAGING", "PREPROD", "PRD", "PROD"];
const stageRank = s => { const i = STAGE_ORDER.indexOf(String(s || "").toUpperCase()); return i < 0 ? STAGE_ORDER.length : i; };
const labelsOf = o => ((o && o.metadata && o.metadata.labels) || {});
const splitSites = v => String(v || "").split(",").map(s => s.trim()).filter(Boolean);

// siteOf(cluster, {ns, ctx, workers}) -> [{value, from}]
function siteOf(c, env) {
  const e = env || {};
  const own = splitSites(labelsOf(c)[LABELS.site]);
  if (own.length) return own.map(value => ({value, from: "cluster-label"}));
  const nsv = splitSites(labelsOf(e.ns)[LABELS.site]);
  if (nsv.length) return nsv.map(value => ({value, from: "namespace"}));
  const cl = (ctxLabels(e.ctx) || {}).site;
  if (cl) return splitSites(cl).map(value => ({value, from: "context"}));
  const zones = Array.from(new Set((e.workers || []).map(n => labelsOf(n)[LABELS.zone] || labelsOf(n)[LABELS.region]).filter(Boolean)));
  return zones.map(value => ({value, from: "node-zone"}));
}
function stageOf(c, env) {
  const e = env || {};
  const own = labelsOf(c)[LABELS.stage]; if (own) return {value: own, from: "cluster-label"};
  const nsv = labelsOf(e.ns)[LABELS.stage]; if (nsv) return {value: nsv, from: "namespace"};
  const cl = (ctxLabels(e.ctx) || {}).stage; if (cl) return {value: cl, from: "context"};
  return null;
}
function tenantOf(c, env) {
  const e = env || {};
  const own = labelsOf(c)[LABELS.tenant]; if (own) return {value: own, from: "cluster-label"};
  const nsv = labelsOf(e.ns)[LABELS.tenant]; if (nsv) return {value: nsv, from: "namespace"};
  const cl = (ctxLabels(e.ctx) || {}).tenant; if (cl) return {value: cl, from: "context"};
  const nsName = (c.metadata || {}).namespace; if (nsName) return {value: nsName, from: "namespace-name"};
  return null;
}

// ---- joins over raw objects ------------------------------------------------
const specOf = o => (o && o.spec) || {};
const statusOf = o => (o && o.status) || {};
const nameOf = o => ((o && o.metadata) || {}).name;

const nodeSetsOf = (cluster, sets) => (sets || []).filter(s => specOf(s).clusterName === nameOf(cluster)
  && (s.metadata || {}).namespace === (cluster.metadata || {}).namespace);
const nodesOfSets = (sets, nodes) => {
  const names = new Set((sets || []).map(nameOf));
  return (nodes || []).filter(n => names.has(specOf(n).storageNodeSetRef) || names.has(labelsOf(n)[LABELS.nodeSet]));
};
const nodesOf = (cluster, sets, nodes) => nodesOfSets(nodeSetsOf(cluster, sets), nodes);
const devicesOfNode = (node, devices) => (devices || []).filter(d => specOf(d).nodeRef === nameOf(node)
  || (statusOf(node).uuid && statusOf(d).nodeID === statusOf(node).uuid));
const devicesOfCluster = (cluster, devices) => {
  const uuid = statusOf(cluster).uuid;
  return uuid ? (devices || []).filter(d => statusOf(d).clusterID === uuid) : [];
};
const poolsOf = (cluster, pools) => (pools || []).filter(p => specOf(p).clusterName === nameOf(cluster)
  && (p.metadata || {}).namespace === (cluster.metadata || {}).namespace);
const workersOf = (sets, nodes, k8sNodes) => {
  const names = new Set();
  (sets || []).forEach(s => (specOf(s).workerNodes || []).forEach(w => names.add(w)));
  (nodes || []).forEach(n => { if (specOf(n).workerNode) names.add(specOf(n).workerNode); });
  return (k8sNodes || []).filter(n => names.has(nameOf(n)));
};

// Volume handle <clusterUUID>:<poolUUID>:<lvolUUID>. Volumes from before the
// v2 API carry a pool name in the middle, and a bare uuid is a lone lvol id.
const parseHandle = h => {
  const p = String(h || "").split(":");
  if (p.length >= 3) return {clusterUUID: p[0], pool: p[1], poolUUID: isUuid(p[1]) ? p[1] : null, poolName: isUuid(p[1]) ? null : p[1], id: p[2], complete: true};
  if (p.length === 2) return {clusterUUID: p[0], pool: null, poolUUID: null, poolName: null, id: p[1], complete: false};
  return {clusterUUID: null, pool: null, poolUUID: null, poolName: null, id: p[0] || null, complete: false};
};
const pvHandle = pv => parseHandle(((specOf(pv).csi) || {}).volumeHandle);
const snapHandle = vsc => parseHandle(statusOf(vsc).snapshotHandle);
const isSimplyblockPV = pv => ((specOf(pv).csi || {}).driver) === "csi.simplyblock.io";
const isSimplyblockSC = sc => (sc && sc.provisioner) === "csi.simplyblock.io";

// Kubernetes Node label simplyblock.io/storage-node-uuid.<clusterUUID>.<socket>=<nodeUUID>
const NODE_UUID_LABEL = /^simplyblock\.io\/storage-node-uuid\.([0-9a-f-]{36})\.(\d+)$/i;
const workerLabelNodeUUIDs = node => Object.entries(labelsOf(node)).map(([k, v]) => {
  const m = NODE_UUID_LABEL.exec(k);
  return m ? {clusterUUID: m[1], socket: Number(m[2]), nodeUUID: v} : null;
}).filter(Boolean);
const isPreparedWorker = node => !!labelsOf(node)[LABELS.nodeType];
const nodeReady = node => (statusOf(node).conditions || []).some(c => c.type === "Ready" && c.status === "True");
const nodeAddress = (node, type) => ((statusOf(node).addresses || []).find(a => a.type === type) || {}).address || null;

// Kubernetes quantities ("4Gi", "512Mi", "1500m", "2T") to bytes or cores
function parseQuantity(q) {
  if (q === null || q === undefined || q === "") return null;
  if (typeof q === "number") return q;
  const m = /^([0-9.]+)\s*([EPTGMKk]i?|[mEPTGMK]?)$/.exec(String(q).trim());
  if (!m) { const n = Number(q); return isNaN(n) ? null : n; }
  const n = parseFloat(m[1]), u = m[2];
  const bin = {Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4, Pi: 1024 ** 5, Ei: 1024 ** 6};
  const dec = {k: 1e3, K: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, E: 1e18, m: 1e-3};
  if (bin[u]) return Math.round(n * bin[u]);
  if (dec[u]) return n * dec[u];
  return n;
}

// Placeholders the dashboard reads before alerts.jsx and tasks.jsx load.
if (!window.alerts) window.alerts = {countsFor: () => ({critical: 0, warning: 0, silenced: 0}), forObject: () => [], ready: false};
if (!window.work) window.work = {countsFor: () => ({running: 0, failed: 0}), forObject: () => [], ready: false};

Object.assign(window, {objKey, keyOf, LABELS, STAGE_ORDER, stageRank, labelsOf, siteOf, stageOf, tenantOf,
  specOf, statusOf, nameOf, nodeSetsOf, nodesOfSets, nodesOf, devicesOfNode, devicesOfCluster, poolsOf, workersOf,
  parseHandle, pvHandle, snapHandle, isSimplyblockPV, isSimplyblockSC, workerLabelNodeUUIDs, isPreparedWorker,
  nodeReady, nodeAddress, parseQuantity});

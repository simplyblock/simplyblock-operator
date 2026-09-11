// ---------------------------------------------------------------------------
// MOCK: DISCOVERY + CLUSTER DEPLOYMENT
//   helm install control plane          (outside this console)
//   install operator, connect to cp     (outside this console)
//   OperatorOps{Discover}               -> the Kubernetes cluster becomes "discovered":
//                                          nodes, free devices, NUMA, vCPU, RAM, NICs
//   ClusterDeploymentConfig (draft)     -> review -> approve
//   three asynchronous steps, per node where it makes sense, each with a log
// ---------------------------------------------------------------------------
const P = window.SB_DB, PU = window.SB_UTIL;
const puuid = PU.uuid, pint = PU.int, ppick = PU.pick, pago = PU.ago;
const PGB = 1e9, PTB = 1e12;

P.discoveries = [];
P.deployment_configs = [];
P.deployment_logs = {};
(P.k8s_clusters || []).forEach(k => { k.discovered = false; k.discovered_at = null; });

// SPDK pre-allocates per-subsystem structures out of hugepage memory:
// 2 GB base + 256 MB per subsystem, rounded up to a 2 GB group.
const hugepagesFor = maxSubsystems => Math.ceil((2 + maxSubsystems * .25) / 2) * 2 * PGB;
window.hugepagesFor = hugepagesFor;

// ---- device filter ---------------------------------------------------------
// The same predicate runs at discovery (what is reported at all), on the
// cluster (the default selection) and per node (a narrower override).
const globRe = p => new RegExp("^" + p.trim().replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*").replace(/\?/g, ".") + "$");
function deviceMatches(d, f) {
  if (!f) return true;
  if (d.kind === "block" && f.enableLogicalBlockDevices === false) return false;
  if (f.deviceClass && d.kind !== f.deviceClass) return false;
  const pcie = d.pcie_address || "";
  const allow = (f.pcieAllowList || []).filter(Boolean);
  const deny = (f.pcieDenyList || []).filter(Boolean);
  if (allow.length && !allow.some(p => pcie.startsWith(p))) return false;
  if (deny.some(p => pcie && pcie.startsWith(p))) return false;
  const names = (f.blockDeviceNames || []).filter(Boolean);
  if (names.length && !names.some(n => (n.includes("*") || n.includes("?")) ? globRe(n).test(d.device_name || "") : (d.device_name || "").includes(n))) return false;
  const models = (f.models || []).filter(Boolean);
  if (models.length && !models.some(m => (d.model_number || "").toLowerCase().includes(m.toLowerCase()))) return false;
  const r = f.driveSizeRange || {};
  if (r.min && d.size < r.min) return false;
  if (r.max && d.size > r.max) return false;
  return true;
}
window.deviceMatches = deviceMatches;

const nodeMatches = (h, sel) => {
  const ml = (sel || {}).matchLabels || {};
  return Object.entries(ml).every(([k, v]) => ((h.k8s_labels || {})[k] || (h.labels || {})[k] || "") === v);
};

const DEFAULT_FILTER = () => ({
  pcieAllowList: [], pcieDenyList: [], blockDeviceNames: [], models: [],
  driveSizeRange: {min: 400 * PGB, max: 0}, enableLogicalBlockDevices: true
});

// ---- discovery -------------------------------------------------------------
const DISCOVERY_STEPS = ["Scheduling inspection pods", "Collecting inventory", "Writing inventory"];

function startDiscovery(kid, spec, opName) {
  const rec = {
    uuid: puuid(), k8s_cluster_id: kid, op_name: opName || null,
    started_at: pago(0), finished_at: null,
    status: "running", step: DISCOVERY_STEPS[0], tick: 0,
    node_selector: (spec && spec.nodeSelector) || {matchLabels: {}},
    device_filter: (spec && spec.deviceFilter) || DEFAULT_FILTER(),
    host_ids: [], node_count: 0, device_count: 0, filtered_count: 0
  };
  P.discoveries = P.discoveries.filter(d => d.k8s_cluster_id !== kid || d.status === "complete");
  P.discoveries.unshift(rec);
  return rec;
}

function finishDiscovery(rec) {
  const hosts = P.hosts.filter(h => h.k8s_cluster_id === rec.k8s_cluster_id
    && !(h.storage_node_ids || []).length && nodeMatches(h, rec.node_selector));
  let devices = 0, filtered = 0;
  hosts.forEach(h => {
    if (h.status === "discovered" || !h.devices.length) PU.inspectHost(h);
    h.devices.forEach(d => {
      d.usable = deviceMatches(d, rec.device_filter) && !d.assigned_node_id;
      if (d.usable) devices++; else filtered++;
    });
    h.discovered_at = pago(0);
  });
  rec.host_ids = hosts.map(h => h.uuid);
  rec.node_count = hosts.length; rec.device_count = devices; rec.filtered_count = filtered;
  rec.status = "complete"; rec.step = null; rec.started_ms = null; rec.finished_at = pago(0);
  const kc = P.k8s_clusters.find(k => k.uuid === rec.k8s_cluster_id);
  if (kc) { kc.discovered = true; kc.discovered_at = rec.finished_at; }
  PU.rollup();
}

// ---- the deployment document ----------------------------------------------
const DEPLOY_STEPS = [
  {k: "ConfigureNodes", label: "Configure worker nodes — hugepages, core isolation, reboot", perNode: true},
  {k: "AddStorageNodes", label: "Add storage nodes — deploy pods, configure SPDK", perNode: true},
  {k: "ActivateCluster", label: "Activate the cluster", perNode: false}
];
window.DEPLOY_STEPS = DEPLOY_STEPS;
const freshSteps = () => DEPLOY_STEPS.map(s => ({name: s.k, label: s.label, phase: "Pending", message: null, startedAt: null, finishedAt: null, progress: null}));

// one storage node per selected NUMA socket
function groupsFor(host, hsel, cl) {
  const sockets = (hsel.sockets && hsel.sockets.length ? hsel.sockets : cl.numaSockets || [0])
    .filter(s => s < (host.numa_sockets || 1));
  const hp = cl.hugepagesOverride || hugepagesFor(cl.maxSubsystems || 128);
  return sockets.map(s => {
    const devs = host.devices.filter(d => d.numa_socket === s && (hsel.deviceIds || []).includes(d.id));
    return {
      numaSocket: s,
      devices: {nvme: devs.filter(d => d.kind === "nvme").map(d => d.pcie_address), block: devs.filter(d => d.kind === "block").map(d => d.device_name)},
      mgmtInterface: cl.mgmtNic || (host.nics[0] || {}).name || "eth0",
      dataInterfaces: (cl.dataNics || []).filter(Boolean),
      sizing: {maxSubsystemCount: cl.maxSubsystems || 128, vcpuCount: cl.vcpu || 8, minHugePagesSize: hp,
        systemMemory: (cl.systemMemoryGb || 32) * PGB},
      coreIsolation: !!cl.coreIsolation,
      failureDomain: cl.failureDomains ? (host.rack_id || host.zone || "fd-1") : null
    };
  });
}

function mkConfig(o) {
  const kc = P.k8s_clusters.find(k => k.uuid === o.k8s_cluster_id) || {};
  const disc = P.discoveries.find(d => d.k8s_cluster_id === o.k8s_cluster_id && d.status === "complete");
  const cl = o.cluster || {};
  const hsels = (o.hosts || []).map(hs => Object.assign({}, hs, {host: P.hosts.find(h => h.uuid === hs.id)})).filter(x => x.host);
  const cfg = {
    uuid: puuid(), name: o.name, created_at: pago(o.ageHours || 0),
    k8s_cluster_id: o.k8s_cluster_id, cluster_id: o.cluster_id || null,
    spec: {
      approved: !!o.approved, environment: kc.environment || "Vanilla", kubernetesClusterRef: kc.name || null,
      nodeSelector: o.nodeSelector || {matchLabels: {}},
      cluster: cl,
      nodeSets: [{name: "default", nodes: hsels.map(x => x.host.hostname),
        groups: hsels.flatMap(x => groupsFor(x.host, x, cl).map(g => Object.assign({node: x.host.hostname}, g)))}]
    },
    status: {
      phase: o.phase || "Draft", message: o.message || null,
      kubernetesClusterId: o.k8s_cluster_id, clusterId: o.cluster_id || null,
      discoveryRef: disc ? disc.uuid : null, hostIds: hsels.map(x => x.host.uuid),
      steps: o.steps || freshSteps(),
      nodes: hsels.map(x => ({host: x.host.hostname, hostId: x.host.uuid, phase: "Pending", message: null, progress: 0, storageNodeIds: []})),
      log: [], observedGeneration: 1
    }
  };
  P.deployment_logs[cfg.uuid] = {};
  P.deployment_configs.push(cfg);
  return cfg;
}

// ---- logging ---------------------------------------------------------------
const nowIso = () => new Date().toISOString();
function glog(cfg, level, step, node, msg) {
  cfg.status.log.push({ts: nowIso(), level, step, node: node || null, msg});
  if (cfg.status.log.length > 400) cfg.status.log.shift();
}
function dlog(cfg, name, level, msg) {
  const L = P.deployment_logs[cfg.uuid] = P.deployment_logs[cfg.uuid] || {};
  (L[name] = L[name] || []).push({ts: nowIso(), level, msg});
}
const logName = (step, host) => host ? `${step}/${host}` : step;

// ---- seeds -----------------------------------------------------------------
(P.k8s_clusters || []).slice(0, 2).forEach(kc => {
  const rec = startDiscovery(kc.uuid, {deviceFilter: DEFAULT_FILTER()}, null);
  rec.started_at = pago(pint(20, 200));
  finishDiscovery(rec);
  rec.finished_at = rec.started_at; kc.discovered_at = rec.started_at;
});

const seedCluster = (name, dc) => ({
  name, deviceClass: dc, vcpu: 12, maxSubsystems: 128, hugepagesOverride: null, systemMemoryGb: 32,
  ec: "2+1", backups: true, objectStorage: false, failureDomains: true, coreIsolation: true,
  mgmtNic: "eno1", dataNics: ["ens1f0", "ens1f1"], numaSockets: [0, 1],
  deviceFilter: {deviceClass: dc, pcieAllowList: [], pcieDenyList: [], models: [], blockDeviceNames: [], driveSizeRange: {min: 400 * PGB, max: 0}}
});

// a deployed document — the record of how a live cluster was built
(function seedDeployed() {
  const kc = (P.k8s_clusters || [])[0];
  const cluster = P.clusters.find(c => c.status === "online");
  if (!kc || !cluster) return;
  const hosts = P.hosts.filter(h => h.cluster_id === cluster.uuid && h.storage_node_ids.length).slice(0, 3);
  const cfg = mkConfig({
    name: cluster.name + "-deployment", k8s_cluster_id: kc.uuid, cluster_id: cluster.uuid,
    approved: true, phase: "Deployed", ageHours: pint(900, 2000),
    hosts: hosts.map(h => ({id: h.uuid, sockets: [0, 1], deviceIds: h.devices.map(d => d.id)})),
    cluster: Object.assign(seedCluster(cluster.name, cluster.device_class), {ec: `${cluster.distr_ndcs}+${cluster.distr_npcs}`})
  });
  const t = cfg.created_at;
  cfg.status.steps.forEach(s => Object.assign(s, {phase: "Succeeded", startedAt: t, finishedAt: t, message: "completed", progress: 100}));
  cfg.status.nodes.forEach(n => Object.assign(n, {phase: "Added", message: "storage node online", progress: 100}));
  cfg.status.message = "Cluster deployed and activated";
  cfg.status.log = [
    {ts: t, level: "INFO", step: "ConfigureNodes", node: null, msg: `${hosts.length} worker node(s) configured`},
    {ts: t, level: "INFO", step: "AddStorageNodes", node: null, msg: `${cfg.spec.nodeSets[0].groups.length} storage node(s) added`},
    {ts: t, level: "INFO", step: "ActivateCluster", node: null, msg: "cluster active"}];
})();

// a draft awaiting approval
(function seedDraft() {
  const disc = P.discoveries.find(d => d.status === "complete" && d.host_ids.length >= 2);
  if (!disc) return;
  const hosts = disc.host_ids.slice(0, Math.min(4, disc.host_ids.length)).map(id => P.hosts.find(h => h.uuid === id));
  mkConfig({
    name: "prod-eu-west-3-deployment", k8s_cluster_id: disc.k8s_cluster_id,
    approved: false, phase: "Draft", ageHours: pint(1, 30),
    hosts: hosts.map(h => ({id: h.uuid, sockets: [0, 1], deviceIds: h.devices.filter(d => d.usable && d.kind === "nvme").map(d => d.id)})),
    cluster: seedCluster("prod-eu-west-3", "nvme"),
    message: "Awaiting approval. Nothing has been applied to any node yet."
  });
})();

// ---- deployment state machine ----------------------------------------------
// Approval creates the cluster record in the control plane (unready) and
// starts step 1. Steps 1 and 2 run per node, in parallel and staggered; each
// node and each step writes its own log.
function createClusterRecord(cfg) {
  const spec = cfg.spec.cluster || {};
  const tmpl = P.clusters.find(c => c.status === "online") || P.clusters[0];
  const c = JSON.parse(JSON.stringify(tmpl));
  const [nd, np] = String(spec.ec || "2+1").split("+").map(Number);
  Object.assign(c, {
    uuid: puuid(), name: spec.name || cfg.name, status: "unready", device_class: spec.deviceClass || "nvme",
    location_type: "datacenter", distr_ndcs: nd || 2, distr_npcs: np || 1, ha_type: "ha", cluster_version: "26.2.1",
    zone_ids: [], rebalancing: false, failure_domain_enabled: !!spec.failureDomains,
    failure_domain_scope: spec.failureDomains ? "rack" : null,
    file_storage: {enabled: false}, object_storage: {enabled: !!spec.objectStorage},
    backup: Object.assign({}, c.backup || {}, {enabled: !!spec.backups}),
    auto_rebalance: Object.assign({}, c.auto_rebalance, {enabled: false, last_run_at: null, moves_last_run: 0}),
    created_at: nowIso(), mgmt_endpoint: `https://cp-${P.clusters.length + 1}.simplyblock.internal:5000`,
    size_total: 0, size_util: 0, storage_nodes_count: 0, storage_nodes_online: 0, devices_count: 0
  });
  P.clusters.push(c);
  cfg.cluster_id = c.uuid; cfg.status.clusterId = c.uuid;
  const kc = P.k8s_clusters.find(k => k.uuid === cfg.k8s_cluster_id);
  if (kc) kc.storage_cluster_ids = [...new Set((kc.storage_cluster_ids || []).concat(c.uuid))];
  glog(cfg, "INFO", "Approve", null, `Cluster ${c.name} created in the control plane (${c.uuid.slice(0, 8)}), status unready`);
  return c;
}

const CONFIGURE_PHASES = [
  {at: 0, phase: "Applying", msg: "writing sysctl and kernel parameters", lines: h => [
    `applying hugepage reservation: vm.nr_hugepages persisted via /etc/sysctl.d/90-simplyblock.conf`,
    `grub: adding hugepagesz=2M default_hugepagesz=2M to GRUB_CMDLINE_LINUX`,
    `tuned profile simplyblock-storage activated`]},
  {at: 3, phase: "Isolating", msg: "core isolation and CPU topology", lines: h => [
    `cpu topology: ${h.numa_sockets} NUMA node(s), ${h.vcpu_count} vCPU — enforced`,
    `isolcpus set for the storage node cores, kubelet reservedSystemCPUs updated`]},
  {at: 5, phase: "Rebooting", msg: "node rebooting to apply kernel parameters", lines: h => [
    `cordoning node ${h.hostname}`, `draining node ${h.hostname} (ignore-daemonsets, delete-emptydir-data)`,
    `reboot requested via privileged pod`]},
  {at: 10, phase: "Verifying", msg: "node back, verifying hugepages", lines: h => [
    `node ${h.hostname} Ready again after reboot`,
    `verified: HugePages_Total matches reservation`, `uncordoning node ${h.hostname}`]},
  {at: 12, phase: "Configured", msg: "configured", lines: () => [`node configuration complete`]}
];
const ADD_PHASES = [
  {at: 0, phase: "Scheduling", msg: "scheduling storage node pod", lines: (h, g) => [
    `creating pod simplyblock-storage-node-${h.hostname}-s${g.numaSocket}`,
    `pod scheduled to ${h.hostname}, NUMA socket ${g.numaSocket}`]},
  {at: 3, phase: "Starting SPDK", msg: "starting SPDK, binding devices", lines: (h, g) => [
    `spdk_tgt started with ${g.sizing.minHugePagesSize / PGB} GB hugepages, ${g.sizing.vcpuCount} cores`,
    ...(g.devices.nvme || []).map(a => `nvme attach ${a} → bdev ok`),
    ...(g.devices.block || []).map(a => `aio bdev ${a} → ok`)]},
  {at: 7, phase: "Joining", msg: "joining the cluster", lines: (h, g) => [
    `NVMe/TCP listener on ${g.dataInterfaces.join(", ") || "data nic"}:4420`,
    `registering storage node with the control plane`, `distrib layout: node accepted, devices reported`]},
  {at: 10, phase: "Added", msg: "storage node added", lines: () => [`storage node online — waiting for cluster activation`]}
];
const ACTIVATE_LINES = [
  `validating node set: all storage nodes online`, `building distribution map (erasure coding)`,
  `creating default storage pool`, `enabling NVMe/TCP subsystems`, `starting health monitor and task engine`,
  `cluster status → online`];

function runPerNode(cfg, step, phases, secsPer, stagger, onNodeDone) {
  const groups = (cfg.spec.nodeSets[0] || {}).groups || [];
  let allDone = true, sum = 0;
  cfg.status.nodes.forEach((n, i) => {
    const host = P.hosts.find(h => h.uuid === n.hostId);
    if (!n.__ms) n.__ms = Date.now() + i * stagger * 1000;
    const el = (Date.now() - n.__ms) / 1000;
    if (el < 0) { n.phase = "Pending"; n.message = "queued"; n.progress = 0; allDone = false; return; }
    const ph = phases.filter(p => el >= p.at).pop() || phases[0];
    const mine = groups.filter(g => g.node === n.host);
    if (n.__phase !== ph.phase) {
      n.__phase = ph.phase; n.phase = ph.phase; n.message = ph.msg;
      glog(cfg, "INFO", step, n.host, `${n.host}: ${ph.msg}`);
      const lines = step === "AddStorageNodes" ? mine.flatMap(g => ph.lines(host, g)) : ph.lines(host);
      lines.forEach(l => dlog(cfg, logName(step, n.host), "INFO", l));
      if (ph === phases[phases.length - 1] && onNodeDone) onNodeDone(n, host, mine);
    }
    n.progress = Math.min(100, Math.round(el / secsPer * 100));
    if (el < secsPer) allDone = false;
    sum += n.progress;
  });
  return {allDone, progress: cfg.status.nodes.length ? Math.round(sum / cfg.status.nodes.length) : 100};
}

function configureDone(cfg, n, h, mine) {
  const hp = mine.reduce((a, g) => a + g.sizing.minHugePagesSize, 0);
  Object.assign(h, {hugepages_reserved: hp, hugepages_allocated: 0, core_isolation: mine.some(g => g.coreIsolation),
    cpu_topology_enforced: true, status: "available", prepared_at: pago(0), cluster_id: cfg.cluster_id, inspection: null,
    labels: Object.assign({}, h.k8s_labels, {"simplyblock.io/storage-node": "true"})});
}

function addDone(cfg, n, h, mine) {
  const c = P.clusters.find(x => x.uuid === cfg.cluster_id);
  if (!c) return;
  const spec = cfg.spec.cluster || {};
  mine.forEach(g => {
    const idx = P.storage_nodes.filter(x => x.cluster_id === c.uuid).length + 1;
    const sn = {
      uuid: puuid(), cluster_id: c.uuid, host_id: h.uuid,
      hostname: `${(spec.name || cfg.name).split("-").slice(0, 2).join("-")}-stor-${String(idx).padStart(2, "0")}`,
      mgmt_ip: h.mgmt_ip, failure_domain: g.failureDomain, physical_label: h.cabinet_id || null,
      status: "in_restart", numa_socket: g.numaSocket, cpu_count: g.sizing.vcpuCount, vcpu_reserved: g.sizing.vcpuCount,
      max_subsystem_count: g.sizing.maxSubsystemCount, memory_total: g.sizing.systemMemory, memory_reserved: g.sizing.systemMemory,
      memory_used: Math.round(g.sizing.systemMemory * .3), hugepages_total: g.sizing.minHugePagesSize,
      hugepages_used: Math.round(g.sizing.minHugePagesSize * .45), spdk_version: "v24.09", core_isolation: g.coreIsolation,
      data_nics: g.dataInterfaces.map((nm, k) => ({name: nm, ip: `10.${pint(10, 60)}.${pint(0, 40)}.${pint(2, 250)}`, port: 4420 + k, numa_socket: g.numaSocket, state: "up"}))
    };
    P.storage_nodes.push(sn);
    h.storage_node_ids.push(sn.uuid); h.hugepages_allocated += g.sizing.minHugePagesSize;
    n.storageNodeIds.push(sn.uuid);
    const want = [].concat(g.devices.nvme || [], g.devices.block || []);
    h.devices.filter(d => want.includes(d.pcie_address) || want.includes(d.device_name)).forEach(hd => {
      hd.assigned_node_id = sn.uuid;
      P.devices.push({uuid: puuid(), node_id: sn.uuid, cluster_id: c.uuid, host_id: h.uuid, cluster_device_class: c.device_class,
        numa_socket: hd.numa_socket, serial_number: hd.serial_number, pcie_address: hd.pcie_address, device_name: hd.device_name,
        model_number: hd.model_number, firmware_revision: `GXA7${pint(10, 99)}1`, status: "new", health_check: null,
        size_total: hd.size, size_util: 0, temperature_c: pint(31, 48), percentage_used: 0, power_on_hours: pint(20, 400),
        io_stats: PU.ioStats(0, 0), io_history: {iops: PU.series(0, 0), bytes: PU.series(0, 0)}});
    });
  });
  PU.rollup();
}

function activate(cfg) {
  const c = P.clusters.find(x => x.uuid === cfg.cluster_id);
  if (!c) return;
  c.status = "online";
  P.storage_nodes.filter(n => n.cluster_id === c.uuid).forEach(n => { n.status = "online"; });
  P.devices.filter(d => d.cluster_id === c.uuid).forEach(d => {
    d.status = "online"; d.health_check = "good";
    const io = pint(9000, 60000); d.io_stats = PU.ioStats(io, io * pint(3800, 9200));
  });
  if (!P.pools.some(p => p.cluster_id === c.uuid))
    P.pools.push({uuid: puuid(), cluster_id: c.uuid, pool_name: "default", status: "active", enabled: true, qos: null,
      size_total: 0, size_used: 0, lvol_count: 0, created_at: nowIso()});
  PU.rollup();
}

function deployTick() {
  const now = Date.now();
  P.discoveries.forEach(d => {
    if (d.status !== "running") return;
    if (!d.started_ms) d.started_ms = now;
    const secs = (now - d.started_ms) / 1000, per = 3;
    d.step = DISCOVERY_STEPS[Math.min(DISCOVERY_STEPS.length - 1, Math.floor(secs / per))];
    if (secs >= DISCOVERY_STEPS.length * per) finishDiscovery(d);
  });

  P.deployment_configs.forEach(cfg => {
    if (cfg.status.phase !== "Deploying") return;
    const steps = cfg.status.steps;
    let cur = steps.find(s => s.phase === "Running");
    if (!cur) {
      cur = steps.find(s => s.phase === "Pending");
      if (!cur) { cfg.status.phase = "Deployed"; return; }
      cur.phase = "Running"; cur.startedAt = nowIso(); cur.__ms = now;
      if (cur.name !== "ActivateCluster") cfg.status.nodes.forEach(n => { n.__ms = null; n.__phase = null; n.phase = "Pending"; n.progress = 0; });
      glog(cfg, "INFO", cur.name, null, `step started: ${cur.label}`);
      if (cur.name === "ActivateCluster") { cur.__lines = 0; }
    }
    if (cur.name === "ConfigureNodes") {
      const r = runPerNode(cfg, cur.name, CONFIGURE_PHASES, 12, .8, (n, h, mine) => configureDone(cfg, n, h, mine));
      cur.progress = r.progress; cur.message = `${cfg.status.nodes.filter(n => n.phase === "Configured").length}/${cfg.status.nodes.length} node(s) configured`;
      if (r.allDone) finishStep(cfg, cur, `${cfg.status.nodes.length} worker node(s) configured — hugepages persisted, core isolation applied`);
    } else if (cur.name === "AddStorageNodes") {
      const r = runPerNode(cfg, cur.name, ADD_PHASES, 10, .6, (n, h, mine) => addDone(cfg, n, h, mine));
      cur.progress = r.progress; cur.message = `${cfg.status.nodes.filter(n => n.phase === "Added").length}/${cfg.status.nodes.length} node(s) added`;
      const c = P.clusters.find(x => x.uuid === cfg.cluster_id); if (c && c.status === "unready") c.status = "in_activation";
      if (r.allDone) finishStep(cfg, cur, `${(cfg.spec.nodeSets[0] || {}).groups.length} storage node(s) added`);
    } else {
      const el = (now - cur.__ms) / 1000, secs = 8;
      const want = Math.min(ACTIVATE_LINES.length, Math.floor(el / secs * ACTIVATE_LINES.length) + 1);
      while ((cur.__lines || 0) < want) { dlog(cfg, "ActivateCluster", "INFO", ACTIVATE_LINES[cur.__lines]); cur.__lines = (cur.__lines || 0) + 1; }
      cur.progress = Math.min(99, Math.round(el / secs * 100)); cur.message = ACTIVATE_LINES[Math.max(0, (cur.__lines || 1) - 1)];
      if (el >= secs) { activate(cfg); finishStep(cfg, cur, "Cluster active"); cfg.status.phase = "Deployed"; cfg.status.message = "Cluster deployed and activated"; }
    }
  });
}
function finishStep(cfg, step, msg) {
  step.phase = "Succeeded"; step.progress = 100; step.finishedAt = nowIso(); step.message = msg;
  glog(cfg, "INFO", step.name, null, `step complete: ${msg}`);
}
setInterval(() => { try { deployTick(); } catch (e) { window.__tickErr = e.message; } }, 1200);

// ---- routes ----------------------------------------------------------------
const dres = (b, s) => new Response(JSON.stringify(b), {status: s || 200, headers: {"Content-Type": "application/json"}});
const dfail = (code, reason, message) => dres({kind: "Status", apiVersion: "v1", status: "Failure", code, reason, message}, code);
const strip = o => JSON.parse(JSON.stringify(o, (k, v) => k.startsWith("__") ? undefined : v));
const cdcOut = c => ({apiVersion: window.API_GROUP, kind: "ClusterDeploymentConfig",
  metadata: {name: c.name, namespace: window.SB_CONFIG.namespace, uid: c.uuid, creationTimestamp: c.created_at, generation: 1},
  spec: c.spec, status: strip(c.status)});

const deployBase = window.SB_CONFIG.k8sBase;
const priorFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const cfgc = window.SB_CONFIG;
  if (!cfgc.mock) return priorFetch(input, init);
  const url = typeof input === "string" ? input : input.url;
  const method = ((init && init.method) || "GET").toUpperCase();
  try { deployTick(); } catch (e) { window.__tickErr = e.message; }
  let body = {};
  try { if (init && init.body) body = JSON.parse(init.body); } catch (e) {}

  if (url.startsWith(cfgc.operatorBase + "/proposed/discoveries")) {
    const q = new URLSearchParams(url.split("?")[1] || "");
    const kid = q.get("scopeId");
    return dres({results: kid ? P.discoveries.filter(d => d.k8s_cluster_id === kid) : P.discoveries, proposed: true});
  }
  // deployment step / node logs — pod logs of the operator's job pods
  const lm = url.match(/\/proposed\/deployments\/([\w-]+)\/logs\?name=(.+)$/);
  if (url.startsWith(cfgc.operatorBase) && lm) {
    const L = P.deployment_logs[lm[1]] || {};
    return dres({results: L[decodeURIComponent(lm[2])] || [], proposed: true});
  }

  const cdc = url.startsWith(deployBase) && url.includes("/clusterdeploymentconfigs");
  if (cdc && method === "GET") {
    const name = (url.split("/clusterdeploymentconfigs/")[1] || "").split("?")[0];
    if (name) { const rec = P.deployment_configs.find(c => c.name === name); return rec ? dres(cdcOut(rec)) : dfail(404, "NotFound", `clusterdeploymentconfigs "${name}" not found`); }
    return dres({apiVersion: window.API_GROUP, kind: "ClusterDeploymentConfigList", items: P.deployment_configs.map(cdcOut)});
  }
  if (cdc && method !== "GET") {
    const name = (url.split("/clusterdeploymentconfigs/")[1] || "").split("?")[0];
    if (method === "POST") {
      const b = body.spec || {};
      if (!b.cluster || !b.cluster.name) return dfail(422, "Invalid", "spec.cluster.name is required");
      const kc = P.k8s_clusters.find(k => k.uuid === b.__kubernetesClusterId);
      if (!kc || !kc.discovered) return dfail(422, "Invalid", "the Kubernetes cluster has not been discovered — run discovery first");
      if (P.deployment_configs.some(c => c.name === body.metadata.name)) return dfail(409, "AlreadyExists", `clusterdeploymentconfigs "${body.metadata.name}" already exists`);
      if (P.clusters.some(c => c.name === b.cluster.name)) return dfail(409, "AlreadyExists", `a storage cluster named ${b.cluster.name} already exists`);
      if (!(b.__hosts || []).length) return dfail(422, "Invalid", "no nodes selected");
      const rec = mkConfig({name: body.metadata.name, k8s_cluster_id: b.__kubernetesClusterId, hosts: b.__hosts,
        nodeSelector: b.nodeSelector, cluster: b.cluster, approved: false, phase: "Draft",
        message: "Awaiting approval. Nothing has been applied to any node yet."});
      if (!rec.spec.nodeSets[0].groups.some(g => g.devices.nvme.length + g.devices.block.length)) {
        P.deployment_configs = P.deployment_configs.filter(c => c !== rec);
        return dfail(422, "Invalid", "no devices selected on any node");
      }
      return dres(cdcOut(rec), 201);
    }
    const rec = P.deployment_configs.find(c => c.name === name);
    if (!rec) return dfail(404, "NotFound", `clusterdeploymentconfigs "${name}" not found`);
    if (method === "DELETE") {
      if (rec.status.phase === "Deploying") return dfail(403, "Forbidden", "admission webhook denied the request: a deployment in flight cannot be deleted — it would strand half-configured nodes");
      P.deployment_configs = P.deployment_configs.filter(c => c !== rec);
      return dres({kind: "Status", status: "Success"});
    }
    if (method === "PATCH") {
      const keys = Object.keys(body.spec || {});
      if (keys.some(k => k !== "approved")) return dfail(422, "Invalid", `spec is immutable except for spec.approved; refused: ${keys.filter(k => k !== "approved").join(", ")}`);
      if (!body.spec.approved) return dfail(422, "Invalid", "approval cannot be withdrawn");
      if (rec.spec.approved) return dfail(409, "Conflict", "this document is already approved");
      rec.spec.approved = true;
      rec.status.phase = "Deploying"; rec.status.message = "Approved — deployment running";
      rec.status.steps = freshSteps(); rec.status.log = [];
      P.deployment_logs[rec.uuid] = {};
      glog(rec, "INFO", "Approve", null, `document approved by operator`);
      createClusterRecord(rec);
      return dres(cdcOut(rec));
    }
  }

  if (url.startsWith(deployBase) && url.includes("/operatorops") && method === "POST" && (body.spec || {}).action === "Discover") {
    const res = await priorFetch(input, init);
    if (!res.ok) return res;
    const out = await res.clone().json().catch(() => ({}));
    const kid = (body.spec.target || {}).kubernetesClusterId
      || (body.spec.kubernetesClusterRef ? (P.k8s_clusters.find(k => k.name === body.spec.kubernetesClusterRef) || {}).uuid : null);
    if (kid) startDiscovery(kid, body.spec.discover || body.spec, (out.metadata || {}).name);
    return dres(out, 201);
  }
  return priorFetch(input, init);
};

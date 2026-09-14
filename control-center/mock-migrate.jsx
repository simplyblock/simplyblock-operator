// ---------------------------------------------------------------------------
// MOCK: MIGRATION PATHS — online migration of workloads and their volumes from
// site A to site B inside one (stretched) Kubernetes cluster.
//   path      A → B: source storage cluster, target storage cluster, the k8s cluster
//   app group ordered unit of work: VMs + containers → their PVCs → their volumes
//   phases    Queued → Replicating → Converged → MovingWorkloads → MigratingVolumes → Cleanup → Completed
// Under the hood a group owns an asynchronous replication policy (created at
// start, deleted at cleanup); its per-volume backlog is what "converged" means.
// ---------------------------------------------------------------------------
const MG = window.SB_DB, MGU = window.SB_UTIL;
const mguuid = MGU.uuid, mgint = MGU.int, mgpick = MGU.pick, mgago = MGU.ago;
MG.migration_paths = [];
MG.app_groups = [];

const PHASES = ["Queued", "Replicating", "Converged", "MovingWorkloads", "MigratingVolumes", "Cleanup", "Completed"];
window.MIG_PHASES = PHASES;
const mnow = () => new Date().toISOString();
// backlog, in the same units the UI shows elsewhere
const fb = b => b >= 1e12 ? (b / 1e12).toFixed(2) + " TB" : b >= 1e9 ? (b / 1e9).toFixed(1) + " GB" : Math.round(b / 1e6) + " MB";
const plog = (p, level, msg, gid) => { p.log.push({ts: mnow(), level, group_id: gid || null, msg}); if (p.log.length > 300) p.log.shift(); };

// the PVCs a member owns: in the mock, the namespace's claims whose name shares the member's stem
function claimsFor(kc, ns, members) {
  const pvcs = MG.pvcs.filter(p => p.k8s_cluster_id === kc.uuid && p.namespace === ns && p.lvol_id);
  const stems = members.map(m => m.name.split("-")[0].toLowerCase());
  const mine = pvcs.filter(p => stems.some(s => p.pvc_name.toLowerCase().includes(s)));
  return (mine.length ? mine : pvcs.slice(0, Math.max(1, members.length)));
}

function mkGroup(path, o) {
  const kc = MG.k8s_clusters.find(k => k.uuid === path.k8s_cluster_id);
  const taken = new Set(MG.app_groups.filter(g => g.path_id === path.uuid).flatMap(g => g.pvc_ids));
  const pvcs = claimsFor(kc, o.namespace, o.members).filter(p => !taken.has(p.uuid));
  const vols = pvcs.map(p => MG.lvols.find(v => v.uuid === p.lvol_id)).filter(Boolean);
  const g = {
    uuid: mguuid(), path_id: path.uuid, name: o.name, namespace: o.namespace,
    members: o.members.map(m => ({kind: m.kind, name: m.name, state: "Pending", progress: 0})),
    approval: o.approval || "manual", order: path.queue.length,
    pvc_ids: pvcs.map(p => p.uuid), lvol_ids: vols.map(v => v.uuid),
    volumes: vols.map(v => ({lvol_id: v.uuid, lvol_name: v.lvol_name, size: v.size_util || v.size_prov, backlog_bytes: v.size_util || 0, last_replication_at: null, progress: 0, migrated: false})),
    phase: o.phase || "Queued", message: "queued", rpolicy_id: null, cg_id: null, created_at: mgago(o.ageHours || 0), started_at: null, finished_at: null
  };
  MG.app_groups.push(g);
  path.queue.push(g.uuid);
  return g;
}

function mkPath(o) {
  const p = {uuid: mguuid(), name: o.name, k8s_cluster_id: o.k8s_cluster_id, source_cluster_id: o.source_cluster_id, target_cluster_id: o.target_cluster_id,
    source_zone_id: o.source_zone_id || null, target_zone_id: o.target_zone_id || null,
    status: o.status || "active", created_at: mgago(o.ageHours || 0), queue: [], log: []};
  MG.migration_paths.push(p);
  plog(p, "INFO", `migration path ${p.name} created`);
  return p;
}

// ---- seeds -----------------------------------------------------------------
(function seed() {
  const kc = MG.k8s_clusters.find(k => (k.storage_cluster_ids || []).length >= 2) || MG.k8s_clusters[0];
  if (!kc) return;
  const src = MG.clusters.find(c => (kc.storage_cluster_ids || []).includes(c.uuid) && c.status === "online") || MG.clusters[0];
  const tgt = MG.clusters.find(c => c.uuid !== src.uuid && c.status === "online" && c.dr_target_eligible) || MG.clusters.find(c => c.uuid !== src.uuid);
  if (!src || !tgt) return;
  kc.storage_cluster_ids = [...new Set((kc.storage_cluster_ids || []).concat([src.uuid, tgt.uuid]))];
  const p = mkPath({name: `${src.name.split("-").slice(0, 2).join("-")}-to-${tgt.name.split("-").slice(0, 2).join("-")}`, k8s_cluster_id: kc.uuid,
    source_cluster_id: src.uuid, target_cluster_id: tgt.uuid, source_zone_id: (src.zone_ids || [])[0] || null, target_zone_id: (tgt.zone_ids || [])[0] || null, ageHours: mgint(30, 300)});
  const nss = [...new Set(MG.pvcs.filter(x => x.k8s_cluster_id === kc.uuid && x.lvol_id).map(x => x.namespace))];
  const defs = [
    {name: "erp-vms", members: [{kind: "VirtualMachine", name: "erp-db-vm"}, {kind: "VirtualMachine", name: "erp-app-vm"}], phase: "Completed"},
    {name: "payments", members: [{kind: "StatefulSet", name: "postgres"}, {kind: "Deployment", name: "payments-api"}], phase: "Replicating"},
    {name: "analytics", members: [{kind: "StatefulSet", name: "kafka"}, {kind: "Deployment", name: "flink-worker"}, {kind: "VirtualMachine", name: "legacy-etl-vm"}], phase: "Queued"},
    {name: "shared-services", members: [{kind: "Deployment", name: "grafana"}, {kind: "StatefulSet", name: "registry"}], phase: "Queued"}
  ];
  defs.forEach((d, i) => {
    const g = mkGroup(p, Object.assign({namespace: nss[i % Math.max(1, nss.length)] || "default", approval: i === 1 ? "manual" : "auto", ageHours: mgint(2, 28)}, d));
    if (d.phase === "Completed") {
      g.members.forEach(m => { m.state = "Moved"; m.progress = 100; });
      g.volumes.forEach(v => { v.backlog_bytes = 0; v.progress = 100; v.migrated = true; v.last_replication_at = g.created_at; });
      g.message = "completed — source volumes deleted"; g.started_at = g.created_at; g.finished_at = g.created_at;
      g.lvol_ids.forEach(id => { const v = MG.lvols.find(x => x.uuid === id); if (v) v.cluster_id = tgt.uuid; });
      plog(p, "INFO", `${g.name}: completed`, g.uuid);
    }
    if (d.phase === "Replicating") startGroup(p, g);
  });
  // a second, paused path on another cluster pair, still empty
  const other = MG.clusters.find(c => ![src.uuid, tgt.uuid].includes(c.uuid) && c.status === "online");
  if (other) mkPath({name: `${other.name.split("-").slice(0, 2).join("-")}-to-${tgt.name.split("-").slice(0, 2).join("-")}`, k8s_cluster_id: kc.uuid,
    source_cluster_id: other.uuid, target_cluster_id: tgt.uuid, status: "paused", ageHours: mgint(5, 40)});
})();

// ---- state machine ---------------------------------------------------------
function startGroup(p, g) {
  // the group's volumes become a consistency group, which owns the cadence
  const cg = {uuid: mguuid(), cluster_id: p.source_cluster_id, name: `mig-${g.name}`,
    lvol_ids: g.lvol_ids.slice(), created_at: mnow(),
    backup_policy: null, replication_config: {frequency_minutes: 5, retention: []}};
  MG.consistency_groups.push(cg);
  g.cg_id = cg.uuid;
  const pol = {uuid: mguuid(), name: `mig-${g.name}`, mode: "asynchronous", pair_id: null, migration_group_id: g.uuid,
    source_cluster_id: p.source_cluster_id, target_cluster_id: p.target_cluster_id, zone_ids: null,
    cg_id: cg.uuid, cg_name: cg.name,
    frequency_minutes: 5, retention: [], failback: {mode: "manual", frequency_minutes: 0, reverse_on_failover: false, resync_full: false},
    state: "healthy", last_replication_at: mnow(), backlog_bytes: 0, generations_kept: 0, created_at: mnow(), last_failover_at: null, last_test_at: null,
    lvol_ids: g.lvol_ids.slice(), dr_cluster_ids: [], apps_count: 0};
  MG.dr_policies.push(pol);
  g.lvol_ids.forEach(id => { const v = MG.lvols.find(x => x.uuid === id); if (v) v.replication = {policy_id: pol.uuid, policy_name: pol.name, mode: "asynchronous", status: "healthy", last_replication_at: mnow(), backlog_bytes: v.size_util || 0, target_cluster_id: p.target_cluster_id, consistency_group: `cg-${pol.name}`}; });
  g.rpolicy_id = pol.uuid; g.phase = "Replicating"; g.message = "initial replication of the group's volumes"; g.started_at = mnow(); g.__ms = Date.now();
  plog(p, "INFO", `${g.name}: consistency group ${cg.name} and replication policy ${pol.name} created for ${g.lvol_ids.length} volume(s)`, g.uuid);
}

function setPhase(p, g, phase, msg) { g.phase = phase; g.message = msg; g.__ms = Date.now(); plog(p, "INFO", `${g.name}: ${phase} — ${msg}`, g.uuid); }

function tickGroup(p, g) {
  const now = Date.now();
  const el = (now - (g.__ms || now)) / 1000;
  if (g.phase === "Replicating") {
    let left = 0;
    g.volumes.forEach(v => {
      const rate = Math.max(2e8, v.size * .06);              // bytes per second, ~16 s for a full copy
      v.backlog_bytes = Math.max(0, v.backlog_bytes - rate * 1.2 + (Math.random() < .3 ? v.size * .002 : 0));
      if (v.backlog_bytes < v.size * .005) v.backlog_bytes = 0;
      v.last_replication_at = mnow(); left += v.backlog_bytes;
      const lv = MG.lvols.find(x => x.uuid === v.lvol_id); if (lv && lv.replication) { lv.replication.backlog_bytes = v.backlog_bytes; lv.replication.last_replication_at = v.last_replication_at; }
    });
    const pol = MG.dr_policies.find(x => x.uuid === g.rpolicy_id); if (pol) pol.backlog_bytes = left;
    g.message = left ? `backlog ${fb(left)} across ${g.volumes.filter(v => v.backlog_bytes).length} volume(s)` : "converged";
    if (!left) setPhase(p, g, "Converged", g.approval === "auto" ? "backlog zero — moving workloads" : "backlog zero — waiting for approval to move");
  } else if (g.phase === "Converged") {
    if (g.approval === "auto" && el > 2) setPhase(p, g, "MovingWorkloads", "live-migrating VMs, restarting containers on the target site");
  } else if (g.phase === "MovingWorkloads") {
    let done = true;
    g.members.forEach((m, i) => {
      const dur = m.kind === "VirtualMachine" ? 8 : 3, start = i * .8;
      const t = el - start;
      if (t < 0) { m.state = "Pending"; done = false; return; }
      m.progress = Math.min(100, Math.round(t / dur * 100));
      if (m.progress < 100) { done = false; if (m.state === "Pending") { m.state = m.kind === "VirtualMachine" ? "LiveMigrating" : "Restarting"; plog(p, "INFO", `${g.name}: ${m.kind}/${m.name} ${m.kind === "VirtualMachine" ? "live migration started (kubevirt)" : "rescheduled to the target site"}`, g.uuid); } }
      else if (m.state !== "Moved") { m.state = "Moved"; plog(p, "INFO", `${g.name}: ${m.kind}/${m.name} running on the target site`, g.uuid); }
    });
    g.message = `${g.members.filter(m => m.state === "Moved").length}/${g.members.length} workload(s) moved`;
    if (done) setPhase(p, g, "MigratingVolumes", "online migration of the volumes to the target nodes");
  } else if (g.phase === "MigratingVolumes") {
    let done = true;
    g.volumes.forEach((v, i) => {
      const t = el - i * .5, dur = 7;
      v.progress = Math.max(0, Math.min(100, Math.round(t / dur * 100)));
      if (v.progress < 100) done = false;
      else if (!v.migrated) { v.migrated = true; const lv = MG.lvols.find(x => x.uuid === v.lvol_id); if (lv) lv.cluster_id = p.target_cluster_id; plog(p, "INFO", `${g.name}: ${v.lvol_name} now served from the target site`, g.uuid); }
    });
    g.message = `${g.volumes.filter(v => v.migrated).length}/${g.volumes.length} volume(s) migrated`;
    if (done) setPhase(p, g, "Cleanup", "deleting the source copies and the replication policy");
  } else if (g.phase === "Cleanup") {
    if (el > 3) {
      MG.dr_policies = MG.dr_policies.filter(x => x.uuid !== g.rpolicy_id);
      // the group's consistency group goes with it
      MG.consistency_groups = MG.consistency_groups.filter(x => x.uuid !== g.cg_id);
      g.lvol_ids.forEach(id => { const v = MG.lvols.find(x => x.uuid === id); if (v) v.replication = null; });
      g.finished_at = mnow(); setPhase(p, g, "Completed", "completed — source volumes deleted");
      MGU.rollup();
    }
  }
}

function migTick() {
  MG.migration_paths.forEach(p => {
    if (p.status !== "active") return;
    const groups = p.queue.map(id => MG.app_groups.find(g => g.uuid === id)).filter(Boolean);
    const active = groups.find(g => !["Queued", "Completed", "Paused", "Failed"].includes(g.phase));
    if (active) { tickGroup(p, active); return; }
    const next = groups.find(g => g.phase === "Queued");
    if (next) { if (!next.lvol_ids.length) { next.phase = "Failed"; next.message = "no bound PVC found for the members"; plog(p, "ERROR", `${next.name}: no volumes to migrate`, next.uuid); return; } startGroup(p, next); }
    else if (groups.length && groups.every(g => g.phase === "Completed") && p.status !== "completed") { p.status = "completed"; plog(p, "INFO", "all application groups migrated"); }
  });
}
setInterval(() => { try { migTick(); } catch (e) { window.__tickErr = e.message; } }, 1300);

// ---- routes ----------------------------------------------------------------
const gid = (m, i) => MG.app_groups.find(g => g.uuid === m[i]);
const pid = (m, i) => MG.migration_paths.find(p => p.uuid === m[i]);
const ROUTES = [
  ["POST", /^\/migration-paths$/, (m, b) => {
    if (!b.name) return {__err: "A name is required"};
    const kc = MG.k8s_clusters.find(k => k.uuid === b.k8s_cluster_id);
    if (!kc) return {__err: "Pick the stretched Kubernetes cluster"};
    if (!b.source_cluster_id || !b.target_cluster_id || b.source_cluster_id === b.target_cluster_id) return {__err: "Source and target storage cluster must differ"};
    if (!(kc.storage_cluster_ids || []).includes(b.target_cluster_id)) return {__err: `${kc.name} has no storage class on the target cluster — add nodes at the target site and a storage class first`};
    if (MG.migration_paths.some(p => p.source_cluster_id === b.source_cluster_id && p.target_cluster_id === b.target_cluster_id && p.status !== "completed")) return {__err: "A path between these clusters already exists"};
    const s = MG.clusters.find(c => c.uuid === b.source_cluster_id), t = MG.clusters.find(c => c.uuid === b.target_cluster_id);
    return {results: [mkPath({name: b.name, k8s_cluster_id: kc.uuid, source_cluster_id: s.uuid, target_cluster_id: t.uuid, source_zone_id: (s.zone_ids || [])[0], target_zone_id: (t.zone_ids || [])[0]})]};
  }],
  ["POST", /^\/migration-paths\/([\w-]+)\/pause$/, m => { const p = pid(m, 1); if (!p) return {__404: true}; p.status = "paused"; plog(p, "WARN", "path paused — the running group finishes its current step, nothing new starts"); return {results: [p]}; }],
  ["POST", /^\/migration-paths\/([\w-]+)\/resume$/, m => { const p = pid(m, 1); if (!p) return {__404: true}; p.status = "active"; plog(p, "INFO", "path resumed"); return {results: [p]}; }],
  ["DELETE", /^\/migration-paths\/([\w-]+)$/, m => {
    const p = pid(m, 1); if (!p) return {__404: true};
    if (MG.app_groups.some(g => g.path_id === p.uuid && g.phase !== "Completed")) return {__err: "The path still has application groups that are not completed. Remove them first."};
    MG.app_groups = MG.app_groups.filter(g => g.path_id !== p.uuid);
    MG.migration_paths = MG.migration_paths.filter(x => x !== p); return {results: []};
  }],
  ["PUT", /^\/migration-paths\/([\w-]+)\/queue$/, (m, b) => {
    const p = pid(m, 1); if (!p) return {__404: true};
    const order = (b.order || []).filter(id => p.queue.includes(id));
    if (order.length !== p.queue.length) return {__err: "The order must list every group of the path exactly once"};
    p.queue = order; p.queue.forEach((id, i) => { const g = gid([id], 0); if (g) g.order = i; });
    plog(p, "INFO", "queue reordered"); return {results: [p]};
  }],
  ["POST", /^\/migration-paths\/([\w-]+)\/app-groups$/, (m, b) => {
    const p = pid(m, 1); if (!p) return {__404: true};
    if (!b.name) return {__err: "A group name is required"};
    if (!b.namespace) return {__err: "A namespace is required"};
    if (!(b.members || []).length) return {__err: "Add at least one VM or container workload"};
    if (MG.app_groups.some(g => g.path_id === p.uuid && g.name === b.name)) return {__err: `A group named ${b.name} exists on this path`};
    const g = mkGroup(p, {name: b.name, namespace: b.namespace, members: b.members, approval: b.approval});
    plog(p, "INFO", `${g.name}: queued with ${g.members.length} workload(s), ${g.lvol_ids.length} volume(s)`, g.uuid);
    return {results: [g]};
  }],
  ["POST", /^\/app-groups\/([\w-]+)\/move$/, m => {
    const g = gid(m, 1); if (!g) return {__404: true};
    if (g.phase !== "Converged") return {__err: "The group can only be moved once replication has converged"};
    const p = pid([g.path_id], 0); setPhase(p, g, "MovingWorkloads", "approved — live-migrating VMs, restarting containers"); return {results: [g]};
  }],
  ["POST", /^\/app-groups\/([\w-]+)\/pause$/, m => {
    const g = gid(m, 1); if (!g) return {__404: true};
    if (!["Replicating", "Converged"].includes(g.phase)) return {__err: "Only a group that is replicating or converged can be paused — workload and volume moves run to completion"};
    g.__resume = g.phase; const p = pid([g.path_id], 0); setPhase(p, g, "Paused", "paused by operator — replication policy stays in place"); return {results: [g]};
  }],
  ["POST", /^\/app-groups\/([\w-]+)\/resume$/, m => {
    const g = gid(m, 1); if (!g) return {__404: true};
    if (g.phase !== "Paused") return {__err: "The group is not paused"};
    const p = pid([g.path_id], 0); setPhase(p, g, g.__resume || "Replicating", "resumed"); return {results: [g]};
  }],
  ["PUT", /^\/app-groups\/([\w-]+)\/approval$/, (m, b) => { const g = gid(m, 1); if (!g) return {__404: true}; g.approval = b.approval === "auto" ? "auto" : "manual"; return {results: [g]}; }],
  ["DELETE", /^\/app-groups\/([\w-]+)$/, m => {
    const g = gid(m, 1); if (!g) return {__404: true};
    if (!["Queued", "Completed", "Failed"].includes(g.phase)) return {__err: "A group in progress cannot be removed — pause it, or let it finish"};
    const p = pid([g.path_id], 0); if (p) { p.queue = p.queue.filter(id => id !== g.uuid); plog(p, "INFO", `${g.name}: removed`, g.uuid); }
    MG.app_groups = MG.app_groups.filter(x => x !== g); return {results: []};
  }]
];
if (window.SB_CP_ROUTES) window.SB_CP_ROUTES.MUT_ROUTES.push(...ROUTES);

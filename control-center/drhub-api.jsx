// ---------------------------------------------------------------------------
// DR HUB DATA LAYER — dr-simplyblock (dr.simplyblock.io/v1alpha1)
//
// The DR hub has no REST API. Every screen is backed by a CR the hub's
// controllers write status on, and every write is a CR write with the caller's
// own RBAC. This file turns the raw objects into the view models the DR tiles
// and details render, registers them in REG so breadcrumbs resolve, and holds
// the handful of mutations the console performs: create a RecoveryAction,
// TestBubble, RestoreAction, TestSchedule, ProtectionPlan, DRPath,
// ProtectedApplication or RecoveryPlan; abort a test; suspend a schedule;
// delete with the hub's confirm-delete annotation.
//
// Reads go through the Kubernetes API proxy (/k8s). Namespaced kinds are
// listed across all namespaces: applications live in Ramen's ops namespace
// or their own, runs live next to their application.
// ---------------------------------------------------------------------------
const DR_KINDS = {pplan: "ProtectionPlan", drpath: "DRPath", papp: "ProtectedApplication", rplan: "RecoveryPlan",
  raction: "RecoveryAction", tbubble: "TestBubble", tsched: "TestSchedule", restore: "RestoreAction",
  siteprofile: "SiteProfile", drconfig: "DRConfig", dhcpserver: "DHCPServer"};
const DR_ANN = {
  createdBy: "dr.simplyblock.io/created-by",
  confirmDelete: "dr.simplyblock.io/confirm-delete",
  awaitingRestore: "dr.simplyblock.io/awaiting-restore",
  autoRestart: "dr.simplyblock.io/auto-restart",
  storageCluster: "dr.simplyblock.io/storage-cluster",
  storageRecovery: "dr.simplyblock.io/storage-recovery",
  schedule: "dr.simplyblock.io/schedule"
};
const VERDICT_RANK = {NotReady: 3, Degraded: 2, Unknown: 1, Ready: 0};
const ACTION_TERMINAL = ["Completed", "Failed", "RolledBack"];
const TEST_TERMINAL = ["Completed", "Failed"];
const RESTORE_TERMINAL = ["Completed", "Failed"];

// Vocabulary the hub writes, coloured for TrafficLight. Merged into the shared
// table so every existing component (tiles, sorting by health) understands it.
Object.assign(STATUS_META, {
  Ready: {c: "var(--ok)", rank: 0, label: "ready"},
  Degraded: {c: "var(--warn)", rank: 3, label: "degraded"},
  NotReady: {c: "var(--bad)", rank: 4, label: "not ready"},
  Unknown: {c: "var(--idle)", rank: 2, label: "unknown"},
  Valid: {c: "var(--ok)", rank: 0, label: "valid"},
  Invalid: {c: "var(--bad)", rank: 4, label: "invalid"},
  Consistent: {c: "var(--ok)", rank: 0, label: "consistent"},
  Inconsistent: {c: "var(--warn)", rank: 3, label: "inconsistent"},
  Protected: {c: "var(--ok)", rank: 0, label: "protected"},
  Unprotected: {c: "var(--bad)", rank: 4, label: "not protected"},
  PreFlight: {c: "var(--info)", rank: 1, label: "pre-flight", blink: true},
  PreSource: {c: "var(--info)", rank: 1, label: "pre-source hooks", blink: true},
  RamenHandoff: {c: "var(--info)", rank: 1, label: "Ramen hand-off", blink: true},
  TargetStarting: {c: "var(--info)", rank: 1, label: "target starting", blink: true},
  Workflow: {c: "var(--info)", rank: 1, label: "workflow", blink: true},
  PostTargetReady: {c: "var(--info)", rank: 1, label: "post-target hooks", blink: true},
  Confirming: {c: "var(--info)", rank: 1, label: "confirming", blink: true},
  RolledBack: {c: "var(--warn)", rank: 3, label: "rolled back"},
  Cloning: {c: "var(--info)", rank: 1, label: "cloning", blink: true},
  Provisioning: {c: "var(--info)", rank: 1, label: "provisioning", blink: true},
  Restoring: {c: "var(--info)", rank: 1, label: "restoring", blink: true},
  Restored: {c: "var(--ok)", rank: 0, label: "restored"},
  Reprotecting: {c: "var(--info)", rank: 1, label: "re-protecting", blink: true},
  Holding: {c: "var(--accent)", rank: 1, label: "holding"},
  TearingDown: {c: "var(--info)", rank: 1, label: "tearing down", blink: true},
  Passed: {c: "var(--ok)", rank: 0, label: "passed"},
  FailedInvariant: {c: "var(--bad)", rank: 5, label: "invariant violated"},
  Unsupported: {c: "var(--dim2)", rank: 3, label: "unsupported"},
  Skipped: {c: "var(--dim2)", rank: 2, label: "skipped"},
  Scheduled: {c: "var(--ok)", rank: 0, label: "scheduled"},
  Suspended: {c: "var(--warn)", rank: 3, label: "suspended"},
  Reported: {c: "var(--ok)", rank: 0, label: "inventory reported"},
  NotReported: {c: "var(--warn)", rank: 3, label: "no inventory yet"},
  Pass: {c: "var(--ok)", rank: 0, label: "pass"},
  Fail: {c: "var(--bad)", rank: 4, label: "fail"},
  Warn: {c: "var(--warn)", rank: 3, label: "warn"},
  Info: {c: "var(--info)", rank: 1, label: "info"},
  NotApplicable: {c: "var(--dim2)", rank: 2, label: "n/a"},
  Resolved: {c: "var(--ok)", rank: 0, label: "resolved"},
  Open: {c: "var(--warn)", rank: 3, label: "open"},
  Rendered: {c: "var(--ok)", rank: 0, label: "rendered"},
  Aborted: {c: "var(--warn)", rank: 3, label: "aborted"}
});

// ---- helpers -------------------------------------------------------------------
const drMeta = o => o.metadata || {};
const drCond = (o, type) => (((o.status || {}).conditions) || []).find(c => c.type === type) || null;
const drCondOK = (o, type) => { const c = drCond(o, type); return c ? (c.status === "True" ? true : c.status === "False" ? false : null) : null; };
const drConds = o => (((o.status || {}).conditions) || []).map(c => ({type: c.type, status: c.status, reason: c.reason, message: c.message, since: c.lastTransitionTime}));
const nsName = (ns, name) => ns ? `${ns}/${name}` : name;
const splitRef = s => { const i = String(s || "").indexOf("/"); return i < 0 ? {namespace: "", name: s || ""} : {namespace: s.slice(0, i), name: s.slice(i + 1)}; };
const refName = r => (r && r.name) || "";
const worstVerdict = vs => vs.reduce((w, v) => (VERDICT_RANK[v] || 0) > (VERDICT_RANK[w] || 0) ? v : w, vs[0] || "Unknown");
const isSyncType = t => /^sync/.test(t || "");
const durMs = (a, b) => a && b ? Math.max(0, Date.parse(b) - Date.parse(a)) : a ? Math.max(0, Date.now() - Date.parse(a)) : null;
const fmtSecs = s => s == null ? "—" : s < 60 ? `${Math.round(s)}s` : s < 3600 ? `${Math.floor(s / 60)}m ${Math.round(s % 60)}s` : `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
const base = (o, kind) => ({
  kind, id: drMeta(o).uid || `${kind}:${nsName(drMeta(o).namespace, drMeta(o).name)}`, name: drMeta(o).name, namespace: drMeta(o).namespace || "",
  createdAt: drMeta(o).creationTimestamp, generation: drMeta(o).generation, observedGeneration: (o.status || {}).observedGeneration,
  labels: drMeta(o).labels || {}, annotations: drMeta(o).annotations || {}, conditions: drConds(o), raw: o
});
const reg = vm => { REG[vm.id] = vm; return vm; };

// ---- normalizers --------------------------------------------------------------------
function normPPlan(o) {
  const sp = o.spec || {}, st = o.status || {};
  const methods = (sp.methods || []).map(m => ({name: m.name, type: m.type, interval: m.schedulingInterval || "",
    s3Backup: m.s3Backup || null, parameters: m.replicationParameters || {}}));
  const ready = drCondOK(o, "Ready");
  const siteStatus = Object.fromEntries((st.sites || []).map(s => [s.name, s]));
  const sites = (sp.sites || []).map(s => Object.assign({name: s.name, cluster: s.cluster, zone: s.zone || "", region: s.region || "", veleroNamespace: s.veleroNamespace || ""},
    {drCluster: (siteStatus[s.name] || {}).drCluster || "", classesApplied: !!(siteStatus[s.name] || {}).classesApplied, agentAvailable: (siteStatus[s.name] || {}).agentAvailable}));
  const pairs = (st.pairs || []).map(p => ({sites: p.sites || [], paths: p.paths || [], drPolicies: p.drPolicies || [], peerClassesResolved: !!p.peerClassesResolved}));
  return reg(Object.assign(base(o, "pplan"), {
    status: ready === true ? "Ready" : ready === false ? "NotReady" : "Unknown",
    sites, siteNames: sites.map(s => s.name), methods, sync: methods.length > 0 && methods.every(m => isSyncType(m.type)),
    storageProfile: sp.storageProfile || {}, s3Profile: refName(sp.s3Profile), s3Profiles: sp.s3Profiles || [],
    autoRestart: sp.autoRestart || null, veleroNamespace: sp.veleroNamespace || "", pairs,
    pathNames: pairs.flatMap(p => p.paths), drPolicies: pairs.flatMap(p => p.drPolicies),
    counts: {sites: sites.length, methods: methods.length, paths: pairs.reduce((n, p) => n + p.paths.length, 0),
      agentsAvailable: sites.filter(s => s.agentAvailable).length}
  }));
}

function normDRPath(o) {
  const sp = o.spec || {}, st = o.status || {};
  const valid = drCondOK(o, "Valid");
  const lastActions = Object.fromEntries((st.lastActions || []).map(a => [a.kind, a]));
  return reg(Object.assign(base(o, "drpath"), {
    status: valid === false ? "Invalid" : st.profileConsistency === "Inconsistent" ? "Inconsistent" : valid === true ? "Valid" : "Unknown",
    from: sp.from, to: sp.to, planName: refName(sp.planRef), actions: sp.actions || [], announcementHandover: !!sp.announcementHandover,
    test: sp.test || null, drPolicies: st.drPolicies || [], applications: (st.applications || []).map(splitRef),
    lastActions, profileConsistency: st.profileConsistency || "Unknown", profileComparison: st.profileComparison || [],
    counts: {apps: (st.applications || []).length, policies: (st.drPolicies || []).length}
  }));
}

function normPApp(o) {
  const sp = o.spec || {}, st = o.status || {};
  const paths = (st.paths || []).map(p => ({name: p.name, from: p.from, to: p.to, actions: p.actions || [],
    verdict: ((p.readiness || {}).verdict) || "Unknown", checks: ((p.readiness || {}).checks) || [], since: (p.readiness || {}).lastTransitionTime}));
  const protectedOK = drCondOK(o, "Protected");
  const verdict = paths.length ? worstVerdict(paths.map(p => p.verdict)) : (protectedOK === false ? "NotReady" : "Unknown");
  const anns = drMeta(o).annotations || {};
  return reg(Object.assign(base(o, "papp"), {
    status: verdict, verdict, protected: protectedOK, bound: drCondOK(o, "Bound"),
    planName: refName(sp.planRef), source: sp.source || "", target: sp.target || "", method: sp.method || "", appKind: sp.kind || "discovered",
    managed: sp.managed || null, discovered: sp.discovered || null, drpcRef: refName(sp.drpcRef),
    probes: (sp.health || {}).probes || [], tiers: sp.tiers || [], externalHooks: sp.externalHooks || {}, dependsOn: sp.dependsOn || [],
    drpc: st.drpc || "", placement: st.placement || "", drPolicy: st.drPolicy || "", zoneBinding: st.zoneBinding || "", currentCluster: st.currentCluster || "",
    paths, siteMapping: st.siteMapping || "Unknown", recipe: st.recipe || null, suggestedTiers: st.suggestedTiers || [], lastAction: st.lastAction ? splitRef(st.lastAction) : null,
    // site mapper (ADR 0020): VM network findings and guest addresses, resolved per declared path
    mapping: st.mapping ? {site: st.mapping.site || "", findings: st.mapping.findings || [], guests: st.mapping.guests || [], counts: st.mapping.counts || {open: 0, resolved: 0}} : null,
    renderings: st.renderings || {},
    awaitingRestore: anns[DR_ANN.awaitingRestore] !== undefined, autoRestartOptOut: anns[DR_ANN.autoRestart] === "false",
    counts: {paths: paths.length, tiers: (sp.tiers || []).length, probes: ((sp.health || {}).probes || []).length,
      openFindings: st.mapping ? (st.mapping.counts || {}).open || 0 : 0, findings: st.mapping ? (st.mapping.findings || []).length : 0, guests: st.mapping ? (st.mapping.guests || []).length : 0}
  }));
}

function normRPlan(o) {
  const sp = o.spec || {}, st = o.status || {};
  const verdict = ((st.readiness || {}).verdict) || "Unknown";
  return reg(Object.assign(base(o, "rplan"), {
    status: drCondOK(o, "Valid") === false ? "Invalid" : verdict, verdict, checks: ((st.readiness || {}).checks) || [],
    pathName: refName(sp.pathRef), applications: (sp.applications || []).map(a => ({name: a.name, priority: a.priority || 1, dependsOn: a.dependsOn || []})),
    gates: sp.gates || {}, continueOnFailure: !!sp.continueOnFailure, hooks: sp.hooks || {},
    counts: {apps: (sp.applications || []).length, priorities: new Set((sp.applications || []).map(a => a.priority || 1)).size}
  }));
}

function normRAction(o) {
  const sp = o.spec || {}, st = o.status || {};
  const phase = st.phase || "Pending";
  const report = st.report || null;
  return reg(Object.assign(base(o, "raction"), {
    status: phase, phase, terminal: ACTION_TERMINAL.includes(phase), action: sp.kind, pathName: refName(sp.pathRef),
    appName: refName(sp.applicationRef), planName: refName(sp.planRef), targetName: refName(sp.applicationRef) || refName(sp.planRef),
    override: sp.override || null, timeout: sp.timeout || "", startTime: st.startTime, completionTime: st.completionTime,
    durationMs: durMs(st.startTime, st.completionTime), sourceCluster: st.sourceCluster || "", targetCluster: st.targetCluster || "",
    steps: st.steps || [], children: st.children || [], report, reportKey: st.reportKey || "",
    rtoSeconds: report ? report.rtoSeconds : null, rpoSeconds: report ? report.achievedRPOSeconds : null,
    guests: report ? report.guests || [] : [],
    createdBy: (drMeta(o).annotations || {})[DR_ANN.createdBy] || (report && report.operator) || "",
    counts: {steps: (st.steps || []).length, children: (st.children || []).length, warnings: report ? (report.warnings || []).length : 0}
  }));
}

function normTBubble(o) {
  const sp = o.spec || {}, st = o.status || {};
  const phase = st.phase || "Pending";
  const report = st.report || null;
  const outcome = report ? report.outcome : "";
  return reg(Object.assign(base(o, "tbubble"), {
    status: phase === "Completed" && outcome ? outcome : phase, phase, outcome, terminal: TEST_TERMINAL.includes(phase),
    pathName: refName(sp.pathRef), appName: refName(sp.applicationRef), planName: refName(sp.planRef), targetName: refName(sp.applicationRef) || refName(sp.planRef),
    cloneSource: sp.cloneSource || "latest-replicated-snapshot", holdFor: sp.holdFor || "", maxLifetime: sp.maxLifetime || "", abort: !!sp.abort,
    testID: st.testID || "", sourceCluster: st.sourceCluster || "", targetCluster: st.targetCluster || "", clonesReadyTime: st.clonesReadyTime,
    applications: st.applications || [], startTime: st.startTime, completionTime: st.completionTime, durationMs: durMs(st.startTime, st.completionTime),
    bubbleNamespaces: st.bubbleNamespaces || [], steps: st.steps || [], invariants: st.invariants || [], checks: st.checks || [], report, reportKey: st.reportKey || "",
    scheduleName: (drMeta(o).labels || {})[DR_ANN.schedule] || "", createdBy: (drMeta(o).annotations || {})[DR_ANN.createdBy] || (report && report.operator) || "",
    counts: {apps: (st.applications || []).length, steps: (st.steps || []).length, invariants: (st.invariants || []).length}
  }));
}

function normTSched(o) {
  const sp = o.spec || {}, st = o.status || {};
  const t = sp.template || {};
  return reg(Object.assign(base(o, "tsched"), {
    status: sp.suspend ? "Suspended" : drCondOK(o, "Valid") === false ? "Invalid" : "Scheduled",
    schedule: sp.schedule || "", suspend: !!sp.suspend, template: t, pathName: refName(t.pathRef), appName: refName(t.applicationRef), planName: refName(t.planRef),
    targetName: refName(t.applicationRef) || refName(t.planRef), retention: sp.retention || {},
    lastScheduleTime: st.lastScheduleTime, lastSuccessfulTime: st.lastSuccessfulTime, active: refName(st.active),
    counts: {}
  }));
}

function normRestore(o) {
  const sp = o.spec || {}, st = o.status || {};
  const phase = st.phase || "Pending";
  return reg(Object.assign(base(o, "restore"), {
    status: phase, phase, terminal: RESTORE_TERMINAL.includes(phase), appName: refName(sp.applicationRef), timeout: sp.timeout || "",
    cluster: st.cluster || "", capture: st.capture || null, volumes: st.volumes || [], restorePoint: st.restorePoint, steps: st.steps || [], checks: st.checks || [],
    dataLoss: st.dataLoss || "", startTime: st.startTime, completionTime: st.completionTime, durationMs: durMs(st.startTime, st.completionTime), message: st.message || "",
    counts: {volumes: (st.volumes || []).length, steps: (st.steps || []).length}
  }));
}

function normSiteProfile(o) {
  const sp = o.spec || {}, st = o.status || {};
  const inv = st.inventory || {};
  const reported = drCondOK(o, "InventoryReported");
  return reg(Object.assign(base(o, "siteprofile"), {
    status: reported === true || st.reportedAt ? "Reported" : "NotReported",
    inventory: inv, reportedAt: st.reportedAt, spec: sp, renderings: st.renderings || {},
    zones: inv.zones || [], nodes: inv.nodes || [], nads: inv.nads || [], storageClasses: inv.storageClasses || [], snapshotClasses: inv.volumeSnapshotClasses || [],
    ingressClasses: inv.ingressClasses || [], gatewayClasses: inv.gatewayClasses || [], ipAddressPools: inv.ipAddressPools || [], registryMirrors: inv.registryMirrors || [],
    counts: {zones: (inv.zones || []).length, nodes: (inv.nodes || []).length, nodesReady: (inv.nodes || []).filter(n => n.ready).length, nads: (inv.nads || []).length, storageClasses: (inv.storageClasses || []).length}
  }));
}

function normDRConfig(o) {
  const sp = o.spec || {}, st = o.status || {};
  const ramen = drCondOK(o, "RamenConfigured");
  const agents = st.agents || [];
  return reg(Object.assign(base(o, "drconfig"), {
    status: sp.recoveryMode ? "Suspended" : ramen === false ? "NotReady" : ramen === true ? "Ready" : "Unknown",
    executor: sp.executor || {}, agent: sp.agent || {}, ramen: sp.ramen || {}, archive: sp.archive || null, bootstrap: sp.bootstrap || {}, retention: sp.retention || {},
    recoveryMode: !!sp.recoveryMode, featureGates: sp.featureGates || {}, agents, stack: st.stack || [], lastBundle: st.lastBundle || null, ramenConfigured: ramen,
    counts: {agents: agents.length, agentsAvailable: agents.filter(a => a.available).length}
  }));
}

function normDHCPServer(o) {
  const sp = o.spec || {}, st = o.status || {};
  const failed = (((st.conditions) || []).some(c => c.status === "False"));
  return reg(Object.assign(base(o, "dhcpserver"), {
    status: failed ? "NotReady" : st.generation ? "Rendered" : "Pending",
    site: sp.site || "", type: sp.type || "", dnsmasq: sp.dnsmasq || null,
    target: sp.dnsmasq ? `${sp.dnsmasq.namespace}/${sp.dnsmasq.configMap}` : "",
    reservations: st.reservations || 0, generation: st.generation || "",
    counts: {reservations: st.reservations || 0}
  }));
}

const NORM = {pplan: normPPlan, drpath: normDRPath, papp: normPApp, rplan: normRPlan, raction: normRAction, tbubble: normTBubble,
  tsched: normTSched, restore: normRestore, siteprofile: normSiteProfile, drconfig: normDRConfig, dhcpserver: normDHCPServer};

// The resolution inbox (design 13.1): open findings of every application,
// grouped by (path, category, source value) so one decision clears every
// occurrence. The decision itself — binding the role on the target
// SiteProfile, pinning a MAC, adding a DHCPServer — is taken on those objects.
function openFindings(apps) {
  const groups = {};
  apps.forEach(a => {
    if (!a.mapping) return;
    a.mapping.findings.forEach(f => (f.paths || []).forEach(p => {
      if (p.result !== "Open") return;
      const key = `${p.path}|${f.category}|${f.value}`;
      const g = groups[key] = groups[key] || {key, path: p.path, site: p.site, cluster: p.cluster, category: f.category, value: f.value, role: p.role || "", reason: p.reason || "", candidates: new Set(), apps: new Set(), vms: new Set(), kind: "finding"};
      g.apps.add(`${a.namespace}/${a.name}`); g.vms.add(f.vm); (p.candidates || []).forEach(c => g.candidates.add(c));
      if (!g.reason && p.reason) g.reason = p.reason;
    }));
    a.mapping.guests.forEach(g0 => (g0.reservations || []).forEach(r => {
      if (r.result !== "Open") return;
      const key = `${r.path || "current"}|guest|${g0.role}|${r.reason}`;
      const g = groups[key] = groups[key] || {key, path: r.path || "", site: r.site, cluster: r.cluster, category: "guest-address", value: `role ${g0.role}`, role: g0.role, reason: r.reason || "", candidates: new Set(), apps: new Set(), vms: new Set(), kind: "guest"};
      g.apps.add(`${a.namespace}/${a.name}`); g.vms.add(g0.vm);
    }));
  });
  return Object.values(groups).map(g => Object.assign(g, {candidates: [...g.candidates], apps: [...g.apps], vms: [...g.vms]}))
    .sort((x, y) => y.apps.length - x.apps.length || x.path.localeCompare(y.path));
}

// ---- reads ---------------------------------------------------------------------------
const drList = kind => k8s.list(DR_KINDS[kind], RESOURCES[DR_KINDS[kind]].namespaced ? {allNamespaces: true} : {}).then(items => items.map(NORM[kind]));
// Kubernetes has no get-by-uid, and the console's paths carry uids: list and pick.
const drById = kind => id => drList(kind).then(items => { const hit = items.find(x => x.id === id); if (!hit) throw new ApiError(404, `${DR_KINDS[kind]} not found`, kind, "NotFound"); return hit; });
const drByName = (kind, name, namespace) => drList(kind).then(items => items.find(x => x.name === name && (!namespace || x.namespace === namespace)) || null);
const stamp = () => Date.now().toString(36);
const dns63 = s => String(s).toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63).replace(/-+$/, "");
const kvToObj = rows => Object.fromEntries((rows || []).filter(r => r.k).map(r => [r.k.trim(), (r.v || "").trim()]));
const csv = s => String(s || "").split(/[,\s]+/).map(x => x.trim()).filter(Boolean);

const drhub = {
  plans: () => drList("pplan"), plan: drById("pplan"),
  paths: () => drList("drpath"), path: drById("drpath"),
  apps: () => drList("papp"), app: drById("papp"),
  rplans: () => drList("rplan"), rplan: drById("rplan"),
  actions: () => drList("raction"), action: drById("raction"),
  tests: () => drList("tbubble"), test: drById("tbubble"),
  schedules: () => drList("tsched"), schedule: drById("tsched"),
  restores: () => drList("restore"), restoreAction: drById("restore"),
  siteProfiles: () => drList("siteprofile"), siteProfile: drById("siteprofile"),
  dhcpServers: () => drList("dhcpserver"), dhcpServer: drById("dhcpserver"),
  siteDHCPServers: id => Promise.all([drhub.siteProfile(id), drhub.dhcpServers()]).then(([sp, ds]) => ds.filter(d => d.site === sp.name)),
  configs: () => drList("drconfig"), config: () => drList("drconfig").then(cs => cs.find(c => c.name === "default") || cs[0] || null),
  // derived Ramen objects, read-only
  drpcs: () => k8s.list("DRPlacementControl", {allNamespaces: true}).catch(() => []),
  drPolicies: () => k8s.list("DRPolicy").catch(() => []),
  drClusters: () => k8s.list("DRCluster").catch(() => []),
  managedClusters: () => k8s.list("ManagedCluster").catch(() => []),
  events: (ns, name) => k8s.list("Event", {namespace: ns, fieldSelector: `involvedObject.name=${name}`}).catch(() => []),

  // scoped reads used by the layers
  planPaths: planId => Promise.all([drhub.plan(planId), drhub.paths()]).then(([p, ps]) => ps.filter(x => x.planName === p.name)),
  planApps: planId => Promise.all([drhub.plan(planId), drhub.apps()]).then(([p, as]) => as.filter(x => x.planName === p.name)),
  pathApps: pathId => Promise.all([drhub.path(pathId), drhub.apps()]).then(([p, as]) => as.filter(a => a.paths.some(x => x.name === p.name) || p.applications.some(r => r.name === a.name && (!r.namespace || r.namespace === a.namespace)))),
  pathActions: pathId => Promise.all([drhub.path(pathId), drhub.actions()]).then(([p, xs]) => xs.filter(x => x.pathName === p.name)),
  pathTests: pathId => Promise.all([drhub.path(pathId), drhub.tests()]).then(([p, xs]) => xs.filter(x => x.pathName === p.name)),
  pathRPlans: pathId => Promise.all([drhub.path(pathId), drhub.rplans()]).then(([p, xs]) => xs.filter(x => x.pathName === p.name)),
  appActions: appId => Promise.all([drhub.app(appId), drhub.actions()]).then(([a, xs]) => xs.filter(x => x.namespace === a.namespace && x.appName === a.name)),
  appTests: appId => Promise.all([drhub.app(appId), drhub.tests()]).then(([a, xs]) => xs.filter(x => x.namespace === a.namespace && x.appName === a.name)),
  appSchedules: appId => Promise.all([drhub.app(appId), drhub.schedules()]).then(([a, xs]) => xs.filter(x => x.namespace === a.namespace && x.appName === a.name)),
  appRestores: appId => Promise.all([drhub.app(appId), drhub.restores()]).then(([a, xs]) => xs.filter(x => x.namespace === a.namespace && x.appName === a.name)),
  rplanActions: id => Promise.all([drhub.rplan(id), drhub.actions()]).then(([p, xs]) => xs.filter(x => x.namespace === p.namespace && x.planName === p.name)),
  rplanTests: id => Promise.all([drhub.rplan(id), drhub.tests()]).then(([p, xs]) => xs.filter(x => x.namespace === p.namespace && x.planName === p.name)),
  scheduleTests: id => Promise.all([drhub.schedule(id), drhub.tests()]).then(([s, xs]) => xs.filter(x => x.namespace === s.namespace && x.scheduleName === s.name)),
  siteProfilePaths: id => Promise.all([drhub.siteProfile(id), drhub.plans(), drhub.paths()]).then(([sp, plans, paths]) => {
    const siteNames = new Set(plans.flatMap(p => p.sites.filter(s => s.cluster === sp.name).map(s => s.name)));
    return paths.filter(p => siteNames.has(p.from) || siteNames.has(p.to));
  }),

  // ---- writes: every one is a CR the caller must be allowed to create ------
  // A Failover / Relocate along a declared path, or a Restart in place. Plan
  // actions fan out one child RecoveryAction per application on the hub.
  runAction: ({kind, target, path, override, timeout}) => k8s.create("RecoveryAction", {
    apiVersion: DR_API_GROUP, kind: "RecoveryAction",
    metadata: {name: dns63(`${kind}-${target.name}-${stamp()}`), namespace: target.namespace},
    spec: Object.assign({kind}, kind !== "Restart" && path ? {pathRef: {name: path}} : {},
      target.kind === "rplan" ? {planRef: {name: target.name}} : {applicationRef: {name: target.name}},
      override ? {override: {reason: override}} : {}, timeout ? {timeout} : {})
  }, {namespace: target.namespace}),
  runTest: ({target, path, cloneSource, holdFor, maxLifetime}) => k8s.create("TestBubble", {
    apiVersion: DR_API_GROUP, kind: "TestBubble",
    metadata: {name: dns63(`test-${target.name}-${stamp()}`), namespace: target.namespace},
    spec: Object.assign({pathRef: {name: path}}, target.kind === "rplan" ? {planRef: {name: target.name}} : {applicationRef: {name: target.name}},
      cloneSource ? {cloneSource} : {}, holdFor ? {holdFor} : {}, maxLifetime ? {maxLifetime} : {})
  }, {namespace: target.namespace}),
  abortTest: t => k8s.patch("TestBubble", t.name, {spec: {abort: true}}, {namespace: t.namespace}),
  holdTest: (t, holdFor) => k8s.patch("TestBubble", t.name, {spec: {holdFor}}, {namespace: t.namespace}),
  runRestore: ({app, timeout}) => k8s.create("RestoreAction", {
    apiVersion: DR_API_GROUP, kind: "RestoreAction",
    metadata: {name: dns63(`restore-${app.name}-${stamp()}`), namespace: app.namespace},
    spec: Object.assign({applicationRef: {name: app.name}}, timeout ? {timeout} : {})
  }, {namespace: app.namespace}),
  createSchedule: ({name, namespace, schedule, target, path, cloneSource, keepLast, keepFor, suspend}) => k8s.create("TestSchedule", {
    apiVersion: DR_API_GROUP, kind: "TestSchedule", metadata: {name: dns63(name), namespace},
    spec: Object.assign({schedule, template: Object.assign({pathRef: {name: path}}, target.kind === "rplan" ? {planRef: {name: target.name}} : {applicationRef: {name: target.name}},
      cloneSource ? {cloneSource} : {})}, {retention: Object.assign({}, keepLast ? {keepLast: Number(keepLast)} : {}, keepFor ? {keepFor} : {})}, suspend ? {suspend: true} : {})
  }, {namespace}),
  suspendSchedule: (s, suspend) => k8s.patch("TestSchedule", s.name, {spec: {suspend: !!suspend}}, {namespace: s.namespace}),
  createPlan: spec => k8s.create("ProtectionPlan", {apiVersion: DR_API_GROUP, kind: "ProtectionPlan", metadata: {name: dns63(spec.name)}, spec: spec.spec}),
  createPath: spec => k8s.create("DRPath", {apiVersion: DR_API_GROUP, kind: "DRPath", metadata: {name: dns63(spec.name)}, spec: spec.spec}),
  createApp: ({name, namespace, spec}) => k8s.create("ProtectedApplication", {apiVersion: DR_API_GROUP, kind: "ProtectedApplication", metadata: {name: dns63(name), namespace}, spec}, {namespace}),
  createRPlan: ({name, namespace, spec}) => k8s.create("RecoveryPlan", {apiVersion: DR_API_GROUP, kind: "RecoveryPlan", metadata: {name: dns63(name), namespace}, spec}, {namespace}),
  // A DHCP server of a site (ADR 0020): dr-hub renders the guest reservations
  // into its ConfigMap on the site; the hub never talks to the server.
  createDHCPServer: ({name, site, namespace, configMap}) => k8s.create("DHCPServer", {apiVersion: "sitemap.simplyblock.io/v1alpha1", kind: "DHCPServer", metadata: {name: dns63(name)},
    spec: {site, type: "dnsmasq", dnsmasq: {namespace, configMap}}}),
  // Optional per-application knob: opt out of the automatic restart after a
  // storage recovery (ADR 0017).
  setAutoRestart: (a, on) => k8s.patch("ProtectedApplication", a.name, {metadata: {annotations: {[DR_ANN.autoRestart]: on ? null : "false"}}}, {namespace: a.namespace}),
  // Deleting a plan or path still in use is refused by the hub unless the
  // caller confirms with an annotation; the console sets it after the confirm
  // dialog, then deletes.
  remove: (o, confirm) => {
    const kind = DR_KINDS[o.kind], opts = o.namespace ? {namespace: o.namespace} : {};
    const pre = confirm ? k8s.patch(kind, o.name, {metadata: {annotations: {[DR_ANN.confirmDelete]: "true"}}}, opts) : Promise.resolve();
    return pre.then(() => k8s.remove(kind, o.name, opts));
  }
};

// The generic detail loader keys on the breadcrumb's layer name.
Object.assign(GETTER, {pplan: drhub.plan, drpath: drhub.path, papp: drhub.app, rplan: drhub.rplan, raction: drhub.action,
  tbubble: drhub.test, tsched: drhub.schedule, restore: drhub.restoreAction, siteprofile: drhub.siteProfile, drconfig: drhub.config, dhcpserver: drhub.dhcpServer});

Object.assign(window, {drhub, DR_KINDS, DR_ANN, VERDICT_RANK, ACTION_TERMINAL, TEST_TERMINAL, worstVerdict, fmtSecs, drCond, drCondOK, splitRef, kvToObj, csv, dns63, openFindings,
  normPPlan, normDRPath, normPApp, normRPlan, normRAction, normTBubble, normTSched, normRestore, normSiteProfile, normDRConfig, normDHCPServer});

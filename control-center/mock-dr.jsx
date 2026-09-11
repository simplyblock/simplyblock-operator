// ---------------------------------------------------------------------------
// DR — TARGET ARCHITECTURE FIXTURES
//
// Two authored objects, everything below them derived:
//
//   ProtectionPlan        sites + storageProfile + methods[]        cluster-scoped
//   ProtectedApplication  planRef + pvcSelector + consistencyGroup + preferredSite
//                         + orchestratedMethod                     namespaced
//
// A method is one declared protection relationship — sync, async or
// snapshot-s3. All three are configured identically and travel the same control
// path; only the class parameters the driver receives differ. From the plan the
// orchestrator derives DRCluster (one per site), DRPolicy (one per pair ×
// interval, all created up front because every field is immutable),
// DRPlacementControl (one per application) and the VRClass / VGRClass matrix.
//
// Six payloads have no field anywhere in the Ramen API and travel a side
// channel: peering, generation selection, retention/lock enforcement, the
// generation catalogue, per-leg lag, and the arbitration token. Two of them are
// reads the console cannot do without — without payloads 4 and 5 a plan with
// three declared methods looks like a healthy single-leg posture with no
// generations.
// ---------------------------------------------------------------------------
const R = window.SB_DB, RU = window.SB_UTIL;
const ruuid = RU.uuid, rint = RU.int, rpick = RU.pick, rago = RU.ago;
const rnd2 = Math.random;

// ---- sites: one per managed cluster ---------------------------------------
// A site name must equal the OCM ManagedCluster name — it is the identity Ramen
// keys DRCluster on. The region is how sync and async are declared: equal
// region means the pair can mirror synchronously, distinct region cannot.
R.dr_sites = [];
R.k8s_clusters.forEach(kc => {
  const zone = R.zones.find(z => z.uuid === (kc.zone_ids || [])[0]) || {};
  R.dr_sites.push({
    uuid: ruuid(), k8s_cluster_id: kc.uuid, name: kc.name,
    region: zone.region || "unknown",
    s3_profile_name: `s3-${kc.name.replace(/^k8s-/, "")}`,
    s3_endpoint: `https://s3.${zone.region || "eu-central-1"}.amazonaws.com`,
    s3_bucket: `ramen-metadata-${kc.name.replace(/^k8s-/, "")}`,
    cidrs: [`10.${rint(10, 90)}.0.0/16`],
    // clusterFence is pair-scoped in Ramen; three sites need a quorum decision
    // it does not model, which is why the arbitration token is payload 6
    fencing_state: "Unfenced",
    status: kc.status === "online" ? "online" : kc.status,
    // DRClusterConfig discovery, reported up by the managed cluster
    discovered_classes: [],
    ramen_version: "v4.19.0-rc2",
    onboarded_at: rago(rint(600, 4000))
  });
});
const siteBy = n => R.dr_sites.find(s => s.name === n);
const siteById = id => R.dr_sites.find(s => s.uuid === id);

// ---- the class matrix, laid out at install time ---------------------------
// A single shared replicationID across the async family pre-validates every
// cluster pair at once, because Ramen matches replicationID pairwise. The
// synchronous class carries a second, pair-scoped identifier instead.
const RETENTION_DEFS = [
  {hourly: 24, daily: 14, weekly: 8},
  {hourly: 12, daily: 7, weekly: 4},
  {hourly: 48, daily: 30, weekly: 12}
];
function classFor(plan, m) {
  const base = {
    storage_class: plan.storage_profile,
    storage_id: `sb-${plan.name}`,
    provisioner: "csi.simplyblock.io"
  };
  if (m.type === "sync") return Object.assign(base, {
    name: `sb-sync-${m.name}`,
    kind: "VolumeGroupReplicationClass",
    replication_id: `sb-sync-${plan.name}-${m.name}`,
    labels: {"sb.io/method": "sync"},
    parameters: {mode: "sync"}
  });
  if (m.type === "async") return Object.assign(base, {
    name: `sb-async-${m.interval}`,
    kind: "VolumeGroupReplicationClass",
    replication_id: `sb-async-mesh`,
    labels: {"sb.io/method": "async"},
    parameters: {mode: "async", schedulingInterval: m.class_interval || m.interval}
  });
  return Object.assign(base, {
    name: `sb-vault-${m.interval}`,
    kind: "VolumeGroupReplicationClass",
    replication_id: `sb-async-mesh`,
    labels: {"sb.io/method": "snapshot-s3"},
    parameters: {
      mode: "snapshot-s3",
      schedulingInterval: m.class_interval || m.interval,
      bucket: m.bucket,
      objectLock: m.immutable ? "compliance" : "none",
      retentionHourly: m.retention.hourly, retentionDaily: m.retention.daily, retentionWeekly: m.retention.weekly
    }
  });
}

// ---- derived Ramen objects ------------------------------------------------
// DRPolicy fields are immutable, so every pair a method could ever use is
// created at onboarding. Nothing may depend on editing one later.
function derivedPolicies(plan) {
  const out = [];
  const names = plan.site_names;
  names.forEach(a => names.forEach(b => {
    if (a === b) return;
    const sa = siteBy(a), sb = siteBy(b);
    if (!sa || !sb) return;
    const sync = sa.region === sb.region;
    const intervals = sync ? [null] : [...new Set(plan.methods.filter(m => m.type !== "sync").map(m => m.interval))];
    intervals.forEach(iv => {
      const method = plan.methods.find(m => m.target === b && (sync ? m.type === "sync" : m.interval === iv));
      // peerClass is what proves protection exists. It appears only when the
      // interval on the policy and the interval in the class parameters match
      // exactly, and the replicationID is present on both clusters.
      const cls = method ? classFor(plan, method) : null;
      const ivOk = !method || method.type === "sync"
        || (method.class_interval || method.interval) === method.interval;
      out.push({
        name: `sb-${a}-${b}${iv ? "-" + iv : ""}`,
        kind: "DRPolicy",
        dr_clusters: [a, b],
        scheduling_interval: iv,
        replication_class_selector: method ? {"sb.io/method": method.type} : {"sb.io/method": sync ? "sync" : "async"},
        method_name: method ? method.name : null,
        peer_class: method && ivOk ? {
          storageClassName: plan.storage_profile,
          replicationId: cls.replication_id,
          storageId: cls.storage_id,
          grouping: "VolumeGroupReplication"
        } : null,
        // documented failure mode: the policy validates cleanly, no class
        // resolves, and the application is protected by nothing
        validated: !!(method && ivOk) || !method,
        created_at: plan.created_at
      });
    });
  }));
  return out;
}

// ---- plans ----------------------------------------------------------------
R.protection_plans = [];
// Two sites in one region can mirror synchronously; a third in another region
// cannot, and takes the asynchronous and vault legs instead. Sites are named
// rather than sliced, because a synchronous method across regions is invalid.
const byRegion = {};
R.dr_sites.forEach(s => (byRegion[s.region] = byRegion[s.region] || []).push(s.name));
const metroPair = Object.values(byRegion).find(v => v.length >= 2) || [];
const remotes = R.dr_sites.filter(s => !metroPair.includes(s.name)).map(s => s.name);
const PLAN_DEFS = [
  {
    // the document's own example: a metro pair, a remote async leg, and a vault
    name: "gold", profile: "sb-nvme-gold",
    sites: [metroPair[0], metroPair[1], remotes[0]].filter(Boolean),
    methods: [
      {name: "metro", type: "sync", target: metroPair[1]},
      {name: "regional", type: "async", target: remotes[0], interval: "5m"},
      {name: "vault", type: "snapshot-s3", target: remotes[0], interval: "1h", retention: 0, immutable: true}
    ]
  },
  {
    name: "silver", profile: "sb-nvme-standard",
    sites: [remotes[0], remotes[1]].filter(Boolean),
    methods: [
      {name: "regional", type: "async", target: remotes[1], interval: "15m"},
      {name: "vault", type: "snapshot-s3", target: remotes[1], interval: "6h", retention: 1, immutable: true}
    ]
  },
  {
    // vault only: generations in an object store and no live peer at all, so
    // the vault method is the orchestrated one in steady state
    name: "archive", profile: "sb-nvme-standard",
    sites: [remotes[1], remotes[2] || metroPair[0]].filter(Boolean),
    methods: [
      {name: "vault", type: "snapshot-s3", target: remotes[2] || metroPair[0], interval: "1d", retention: 2, immutable: false}
    ]
  },
  {
    // deliberately broken: the interval on the policy and the interval in the
    // class parameters disagree, so no class resolves and nothing is protected
    name: "bronze", profile: "sb-nvme-standard",
    sites: [metroPair[0], remotes[1]].filter(Boolean),
    methods: [
      {name: "regional", type: "async", target: remotes[1], interval: "10m", classInterval: "10min"}
    ]
  }
];
PLAN_DEFS.forEach(def => {
  const siteNames = (def.sites || []).filter(Boolean);
  const sites = siteNames.map(siteBy).filter(Boolean);
  if (sites.length < 2) return;
  const plan = {
    uuid: ruuid(), name: def.name, kind: "ProtectionPlan",
    storage_profile: def.profile,
    site_names: siteNames,
    site_ids: sites.map(s => s.uuid),
    methods: def.methods.filter(m => siteNames.includes(m.target)).map(m => ({
      name: m.name, type: m.type,
      target: m.target,
      interval: m.interval || null,
      // when these two disagree the derivation silently produces nothing
      class_interval: m.classInterval || m.interval || null,
      retention: m.type === "snapshot-s3" ? RETENTION_DEFS[m.retention || 0] : null,
      immutable: m.type === "snapshot-s3" ? !!m.immutable : false,
      bucket: m.type === "snapshot-s3" ? `sb-vault-${def.name}` : null
    })),
    created_at: rago(rint(400, 3000))
  };
  plan.classes = plan.methods.map(m => classFor(plan, m));
  plan.policies = derivedPolicies(plan);
  R.protection_plans.push(plan);
});
const planBy = n => R.protection_plans.find(p => p.name === n);

// ---- per-leg status: bypass payload 5 -------------------------------------
// Only the orchestrated method has a VRG, so only its RPO appears in
// DRPC.status. Every other declared method reports its lag up the side channel.
const ivMinutes = iv => {
  if (!iv) return 0;
  const n = parseInt(iv, 10), u = iv.replace(/[\d.]/g, "");
  return n * (u === "m" ? 1 : u === "h" ? 60 : u === "d" ? 1440 : u === "w" ? 10080 : 1);
};
function legFor(plan, m, orchestrated, broken, stale) {
  const mins = ivMinutes(m.interval);
  const sync = m.type === "sync";
  if (broken) return {
    leg_id: `${plan.name}/${m.name}`, method: m.name, type: m.type, target: m.target,
    state: "Unprotected", last_sync_at: null, lag_seconds: null, epoch: 0,
    health: "unhealthy", orchestrated,
    note: "No replication class resolved for this method, so nothing is being replicated."
  };
  const lagMin = sync ? 0 : stale ? mins * (2.6 + rnd2() * 3) : mins * (.15 + rnd2() * .7);
  return {
    leg_id: `${plan.name}/${m.name}`, method: m.name, type: m.type, target: m.target,
    state: "Replicating",
    last_sync_at: rago(lagMin / 60),
    lag_seconds: Math.round(lagMin * 60),
    epoch: sync ? null : rint(400, 9000),
    bytes_last_cycle: sync ? null : rint(40, 3800) * 1e6,
    health: stale ? "degraded" : "healthy",
    orchestrated, note: null
  };
}

// ---- generation catalogue: bypass payload 4 -------------------------------
// No Kubernetes object represents a generation. VRG status describes the
// current relationship only, so the catalogue is a side-channel read.
function generationsFor(m, cgName) {
  const out = [];
  const tiers = [
    {label: "hourly", every: 60, n: m.retention.hourly},
    {label: "daily", every: 1440, n: m.retention.daily},
    {label: "weekly", every: 10080, n: m.retention.weekly}
  ];
  let gen = 1;
  tiers.slice().reverse().forEach(t => {
    for (let i = t.n - 1; i >= 0; i--) {
      out.push({
        generation: gen++, tier: t.label,
        at: rago((t.every * (i + 1)) / 60),
        size_bytes: rint(300, 9000) * 1e6,
        integrity_state: "Verified",
        consistency_group: cgName || null,
        // Object Lock state is object-store state with no CR anywhere:
        // enforcement is payload 3
        locked_until: m.immutable ? rago(-(14 * 24) + (t.every * (i + 1)) / 60) : null
      });
    }
  });
  const kept = out.reverse().map((g, i) => Object.assign(g, {generation: out.length - i}));
  if (kept.length) {
    const base = kept[kept.length - 1];
    base.kind = "full"; base.size_bytes = rint(9000, 44000) * 1e6;
  }
  kept.forEach(g => { if (!g.kind) g.kind = "delta"; });
  // one generation in the middle failed verification, so the console has
  // something honest to show about integrity
  if (kept.length > 6) kept[3].integrity_state = "Unverified";
  return kept;
}

// ---- bind the existing protected applications to plans --------------------
// One DRPC per application is the hard limit: a DRPC selects PVCs by label, so
// two over the same PVCs would both claim them and the documented outcome is
// data corruption. orchestratedMethod names which method Ramen drives, and the
// rest run with identical parameters in the data plane.
let planCursor = 0;
(R.protected_apps || []).forEach((a, ai) => {
  const plan = R.protection_plans[planCursor % R.protection_plans.length];
  planCursor++;
  if (!plan) return;
  const broken = plan.name === "bronze";
  const stale = ai === 3;
  a.kind = "ProtectedApplication";
  a.plan_id = plan.uuid;
  a.plan_name = plan.name;
  a.storage_profile = plan.storage_profile;
  // the site the workload normally runs on, and where it is right now
  a.preferred_site = plan.site_names[0];
  a.active_site = a.phase === "FailedOver" ? plan.site_names[1] : plan.site_names[0];
  a.preferred_site_id = (siteBy(a.preferred_site) || {}).uuid || null;
  a.active_site_id = (siteBy(a.active_site) || {}).uuid || null;
  // Ramen drives exactly one method. The vault method is never the orchestrated
  // one in steady state — it becomes so only for the duration of a restore.
  const orch = plan.methods.find(m => m.type === "sync") || plan.methods.find(m => m.type === "async") || plan.methods[0];
  a.orchestrated_method = orch.name;
  a.legs = plan.methods.map(m => legFor(plan, m, m.name === orch.name, broken, stale && m.type !== "sync"));
  // the vault leg's catalogue, and the restore targets it makes possible
  const vault = plan.methods.find(m => m.type === "snapshot-s3");
  a.generations = vault ? generationsFor(vault, a.cg_name) : [];
  a.vault_method = vault ? vault.name : null;
  if (vault && a.generations.length) {
    const leg = a.legs.find(l => l.method === vault.name);
    if (leg) {
      leg.generations = a.generations.length;
      leg.oldest_generation_at = a.generations[a.generations.length - 1].at;
      leg.newest_generation_at = a.generations[0].at;
      leg.locked_until = a.generations[0].locked_until;
    }
  }
  // failover targets are every site with a live peer leg; restore targets are
  // the sites a vault generation can be materialised on
  a.failover_targets = plan.methods.filter(m => m.type !== "snapshot-s3").map(m => m.target);
  a.restore_targets = vault ? [vault.target] : [];
  // a vault restore has run: the source is gone or untrusted, so the original
  // site is no longer a failover target and the app is re-protected as new
  a.restored_from_generation = null;
  a.pvc_selector = a.pvc_selector || {matchLabels: {app: (a.app_name || "").split("-")[0]}};
  // payload 2: pinned immediately before the DRPC rebind, cleared after the
  // promote resolves — a stale pin would make an ordinary failover resolve to
  // an old generation
  a.pinned_generation = null;
  delete a.protection_mode;
  delete a.recovery_points;
  delete a.restore_point;
});

// ---- arbitration token: bypass payload 6 ----------------------------------
// DRCluster.clusterFence and NetworkFence are pair-scoped. Three sites need a
// quorum decision Ramen does not model at all.
R.dr_arbitration = {
  token_holder: (R.dr_sites[0] || {}).name || null,
  generation: rint(40, 900),
  quorum_ack: R.dr_sites.slice(0, 3).map(s => ({site: s.name, acked_at: rago(rnd2() * .4)})),
  fenced_sites: [],
  quorum_size: Math.min(3, R.dr_sites.length),
  updated_at: rago(rnd2() * .2)
};

// ---- rollup ---------------------------------------------------------------
function drRollup() {
  R.protection_plans.forEach(p => {
    const apps = (R.protected_apps || []).filter(a => a.plan_id === p.uuid);
    p.apps_count = apps.length;
    p.pvcs_count = apps.reduce((n, a) => n + (a.pvcs_count || 0), 0);
    p.methods_count = p.methods.length;
    p.sites_count = p.site_names.length;
    p.policies_count = p.policies.length;
    p.unvalidated_count = p.policies.filter(x => !x.validated).length;
    // a plan whose methods do not all resolve a class is protecting nothing on
    // those legs, however healthy the rest of it looks
    p.protection_gap = p.methods.some(m => (m.class_interval || m.interval) !== m.interval);
    const legs = apps.flatMap(a => a.legs || []);
    p.health = p.protection_gap || legs.some(l => l.health === "unhealthy") ? "unhealthy"
      : legs.some(l => l.health === "degraded") ? "degraded" : "healthy";
    p.worst_lag_seconds = legs.reduce((n, l) => Math.max(n, l.lag_seconds || 0), 0);
    p.generations_total = apps.reduce((n, a) => n + (a.generations || []).length, 0);
  });
  R.dr_sites.forEach(s => {
    const apps = (R.protected_apps || []);
    s.active_apps_count = apps.filter(a => a.active_site === s.name).length;
    s.standby_apps_count = apps.filter(a => a.active_site !== s.name
      && (a.failover_targets || []).concat(a.restore_targets || []).includes(s.name)).length;
    s.plans_count = R.protection_plans.filter(p => p.site_names.includes(s.name)).length;
    s.discovered_classes = [...new Set(R.protection_plans
      .filter(p => p.site_names.includes(s.name))
      .flatMap(p => p.classes.map(c => c.name)))];
  });
  (R.protected_apps || []).forEach(a => {
    const plan = R.protection_plans.find(p => p.uuid === a.plan_id);
    if (!plan) return;
    a.legs = (a.legs || []).map(l => Object.assign(l, {orchestrated: l.method === a.orchestrated_method}));
    const orch = a.legs.find(l => l.orchestrated);
    // the orchestrated leg's RPO is the only one Ramen reports; it is also what
    // DRPC.status.lastGroupSyncTime carries
    a.last_group_sync_at = orch && orch.last_sync_at ? orch.last_sync_at : a.last_group_sync_at;
    a.rpo_met = !a.legs.some(l => {
      const m = plan.methods.find(x => x.name === l.method);
      return m && m.interval && l.lag_seconds > ivMinutes(m.interval) * 60;
    });
    a.health = a.phase === "WaitForUser" ? "unhealthy"
      : a.legs.some(l => l.health === "unhealthy") ? "unhealthy"
      : a.legs.some(l => l.health === "degraded") || !a.rpo_met ? "degraded"
      : a.progression === "Completed" ? "healthy" : "degraded";
    a.legs_count = a.legs.length;
    a.generations_count = (a.generations || []).length;
  });
}
drRollup();
R.dr_rollup = drRollup;

// ---- routes ---------------------------------------------------------------
const rfail = (c, m) => new Response(JSON.stringify({status: false, error: m}), {status: c, headers: {"Content-Type": "application/json"}});
const appById = id => (R.protected_apps || []).find(a => a.uuid === id);
const planById = id => R.protection_plans.find(p => p.uuid === id);

const DR_ROUTES = [
  ["GET", /^\/protection-plans$/, () => ({results: R.protection_plans})],
  ["GET", /^\/protection-plans\/([\w-]+)$/, m => {
    const p = planById(m[1]); return p ? {results: [p]} : {__404: true};
  }],
  ["GET", /^\/protection-plans\/([\w-]+)\/protected-apps$/, m =>
    ({results: (R.protected_apps || []).filter(a => a.plan_id === m[1])})],
  ["GET", /^\/protection-plans\/([\w-]+)\/sites$/, m => {
    const p = planById(m[1]);
    return {results: p ? p.site_names.map(siteBy).filter(Boolean) : []};
  }],
  ["GET", /^\/dr-sites$/, () => ({results: R.dr_sites})],
  ["GET", /^\/dr-sites\/([\w-]+)$/, m => {
    const s = siteById(m[1]); return s ? {results: [s]} : {__404: true};
  }],
  ["GET", /^\/dr-sites\/([\w-]+)\/protected-apps$/, m => {
    const s = siteById(m[1]);
    if (!s) return {__404: true};
    return {results: (R.protected_apps || []).filter(a => a.active_site === s.name
      || (a.failover_targets || []).includes(s.name) || (a.restore_targets || []).includes(s.name))};
  }],
  ["GET", /^\/dr-sites\/([\w-]+)\/protection-plans$/, m => {
    const s = siteById(m[1]);
    return {results: s ? R.protection_plans.filter(p => p.site_names.includes(s.name)) : []};
  }],
  // payload 4: the generation catalogue, per application
  ["GET", /^\/protected-apps\/([\w-]+)\/generations$/, m => {
    const a = appById(m[1]);
    return a ? {results: a.generations || []} : {__404: true};
  }],
  // payload 6: the arbitration token
  ["GET", /^\/dr-arbitration$/, () => ({results: [R.dr_arbitration]})],

  // ---- plan mutations ----
  ["POST", /^\/protection-plans$/, b => {
    const name = (b.name || "").trim();
    if (!name) return {__err: "Name the plan"};
    if (planBy(name)) return {__err: `A plan named ${name} already exists`};
    const siteNames = (b.site_names || []).filter(Boolean);
    if (siteNames.length < 2) return {__err: "A plan needs at least two sites"};
    const plan = {
      uuid: ruuid(), name, kind: "ProtectionPlan",
      storage_profile: b.storage_profile || "sb-nvme-standard",
      site_names: siteNames, site_ids: siteNames.map(n => (siteBy(n) || {}).uuid).filter(Boolean),
      methods: [], created_at: rago(0)
    };
    plan.classes = []; plan.policies = derivedPolicies(plan);
    R.protection_plans.push(plan);
    drRollup();
    return {results: [plan]};
  }],
  ["POST", /^\/protection-plans\/([\w-]+)\/methods$/, (m, b) => {
    const p = planById(m[1]);
    if (!p) return {__404: true};
    const name = (b.name || "").trim();
    if (!name) return {__err: "Name the method"};
    if (p.methods.some(x => x.name === name)) return {__err: `${p.name} already has a method named ${name}`};
    const target = b.target;
    if (!p.site_names.includes(target)) return {__err: `${target} is not a site in this plan`};
    const src = siteBy(p.site_names[0]), tgt = siteBy(target);
    if (b.type === "sync" && src && tgt && src.region !== tgt.region)
      return {__err: `Synchronous replication needs both sites in one region — ${src.name} is in ${src.region} and ${tgt.name} in ${tgt.region}.`};
    if (b.type !== "sync" && !b.interval) return {__err: "An interval is required for asynchronous and vault methods"};
    const method = {
      name, type: b.type, target,
      interval: b.type === "sync" ? null : b.interval,
      // one field, emitted to both places, never configured independently
      class_interval: b.type === "sync" ? null : b.interval,
      retention: b.type === "snapshot-s3"
        ? {hourly: Number(b.retention_hourly) || 24, daily: Number(b.retention_daily) || 14, weekly: Number(b.retention_weekly) || 8}
        : null,
      immutable: b.type === "snapshot-s3" ? b.immutable !== false : false,
      bucket: b.type === "snapshot-s3" ? (b.bucket || `sb-vault-${p.name}`) : null
    };
    p.methods.push(method);
    p.classes = p.methods.map(x => classFor(p, x));
    p.policies = derivedPolicies(p);
    // every application on the plan gains the leg
    (R.protected_apps || []).filter(a => a.plan_id === p.uuid).forEach(a => {
      a.legs.push(legFor(p, method, false, false, false));
      if (method.type === "snapshot-s3") {
        a.generations = generationsFor(method, a.cg_name);
        a.vault_method = method.name;
        a.restore_targets = [method.target];
      } else if (!a.failover_targets.includes(method.target)) a.failover_targets.push(method.target);
    });
    drRollup();
    return {results: [p]};
  }],
  ["DELETE", /^\/protection-plans\/([\w-]+)\/methods\/([\w-]+)$/, m => {
    const p = planById(m[1]);
    if (!p) return {__404: true};
    const name = m[2];
    const apps = (R.protected_apps || []).filter(a => a.plan_id === p.uuid);
    if (apps.some(a => a.orchestrated_method === name))
      return {__err: `${name} is the orchestrated method of ${apps.filter(a => a.orchestrated_method === name).length} application(s). Point them at another method first.`};
    p.methods = p.methods.filter(x => x.name !== name);
    p.classes = p.methods.map(x => classFor(p, x));
    p.policies = derivedPolicies(p);
    apps.forEach(a => { a.legs = a.legs.filter(l => l.method !== name); });
    drRollup();
    return {results: [p]};
  }],
  // fixing the interval writes it to both places at once, which is the whole
  // point of the invariant
  ["PUT", /^\/protection-plans\/([\w-]+)\/methods\/([\w-]+)\/interval$/, (m, b) => {
    const p = planById(m[1]);
    if (!p) return {__404: true};
    const method = p.methods.find(x => x.name === m[2]);
    if (!method) return {__404: true};
    if (!b.interval) return {__err: "An interval is required"};
    method.interval = b.interval;
    method.class_interval = b.interval;
    p.classes = p.methods.map(x => classFor(p, x));
    p.policies = derivedPolicies(p);
    (R.protected_apps || []).filter(a => a.plan_id === p.uuid).forEach(a => {
      a.legs = a.legs.map(l => l.method === method.name
        ? legFor(p, method, l.orchestrated, false, false) : l);
    });
    drRollup();
    return {results: [p]};
  }],
  ["DELETE", /^\/protection-plans\/([\w-]+)$/, m => {
    const p = planById(m[1]);
    if (!p) return {__404: true};
    const apps = (R.protected_apps || []).filter(a => a.plan_id === p.uuid);
    if (apps.length) return {__err: `${apps.length} application(s) are still bound to ${p.name}. Unbind them first.`};
    R.protection_plans = R.protection_plans.filter(x => x.uuid !== p.uuid);
    drRollup();
    return {results: []};
  }],

  // ---- application mutations ----
  ["POST", /^\/protected-apps$/, (m, b) => {
    const p = planById(b.plan_id);
    if (!p) return {__err: "Choose a protection plan"};
    if (!p.methods.length) return {__err: `${p.name} declares no method, so it protects nothing`};
    const name = (b.app_name || "").trim(), ns = (b.namespace || "").trim();
    if (!name || !ns) return {__err: "Name the application and its namespace"};
    if ((R.protected_apps || []).some(a => a.app_name === name && a.namespace === ns))
      return {__err: `${ns}/${name} is already protected`};
    const key = (b.pvc_selector_key || "").trim(), val = (b.pvc_selector_value || "").trim();
    // an empty selector claims every PVC in the namespace, and two placement
    // controls would then contend over the same volumes
    if (!key || !val) return {__err: "A PVC selector is required — an empty selector claims every PVC in the namespace"};
    const site = b.preferred_site && p.site_names.includes(b.preferred_site) ? b.preferred_site : p.site_names[0];
    const orch = p.methods.find(x => x.name === b.orchestrated_method)
      || p.methods.find(x => x.type !== "snapshot-s3") || p.methods[0];
    const vault = p.methods.find(x => x.type === "snapshot-s3");
    const a = {
      uuid: ruuid(), kind: "ProtectedApplication",
      app_name: name, namespace: ns, app_kind: b.app_kind || "ApplicationSet",
      plan_id: p.uuid, plan_name: p.name, storage_profile: p.storage_profile,
      preferred_site: site, active_site: site,
      preferred_site_id: (siteBy(site) || {}).uuid || null,
      active_site_id: (siteBy(site) || {}).uuid || null,
      orchestrated_method: orch.name,
      pvc_selector: {matchLabels: {[key]: val}},
      phase: "Deployed", progression: "Completed", action: null, vrg_state: "primary",
      kube_object_protection: b.kube_object_protection !== false,
      recipe: null, pvc_ids: [], pvcs_count: 0, volumes: [],
      legs: p.methods.map(x => legFor(p, x, x.name === orch.name, false, false)),
      generations: vault ? generationsFor(vault, null) : [],
      vault_method: vault ? vault.name : null,
      pinned_generation: null, restored_from_generation: null,
      failover_targets: p.methods.filter(x => x.type !== "snapshot-s3").map(x => x.target),
      restore_targets: vault ? [vault.target] : [],
      last_group_sync_at: rago(0), group_last_at: rago(0), group_written_since: 0,
      rpo_met: true, health: "healthy", created_at: rago(0)
    };
    R.protected_apps.push(a);
    drRollup();
    return {results: [a]};
  }],
  // rebinding the DRPC: delete + create, because DRPolicy fields are immutable
  ["PUT", /^\/protected-apps\/([\w-]+)\/orchestrated-method$/, (m, b) => {
    const a = appById(m[1]);
    if (!a) return {__404: true};
    const p = planById(a.plan_id);
    const method = p && p.methods.find(x => x.name === b.method);
    if (!method) return {__err: `${b.method} is not a method on ${p ? p.name : "this plan"}`};
    if (method.type === "snapshot-s3" && !b.allow_vault)
      return {__err: "The vault method becomes the orchestrated one only for the duration of a restore. Use Restore from a generation instead."};
    a.orchestrated_method = method.name;
    a.progression = "UpdatingPlacement";
    a.action_started_ms = Date.now();
    drRollup();
    return {results: [a]};
  }],
  // payload 2: pin the generation, then rebind. PromoteVolume has no
  // point-in-time argument, so the driver reads the pin when the promote lands.
  ["POST", /^\/protected-apps\/([\w-]+)\/restore$/, (m, b) => {
    const a = appById(m[1]);
    if (!a) return {__404: true};
    const p = planById(a.plan_id);
    const vault = p && p.methods.find(x => x.type === "snapshot-s3");
    if (!vault) return {__err: `${p ? p.name : "This plan"} declares no vault method, so there are no generations to restore from.`};
    const gen = (a.generations || []).find(g => String(g.generation) === String(b.generation));
    if (!gen) return {__err: "That generation is not in the catalogue"};
    if (gen.integrity_state !== "Verified")
      return {__err: `Generation ${gen.generation} has not passed verification. Restoring from it may produce an unusable volume set.`};
    a.pinned_generation = gen.generation;
    a.orchestrated_method = vault.name;
    a.phase = "FailingOver";
    a.progression = "RestoringGeneration";
    a.action = "Failover";
    a.action_started_ms = Date.now();
    a.restore_target = vault.target;
    drRollup();
    return {results: [a]};
  }],
  // Fencing is pair-scoped in Ramen, so with three or more sites the decision
  // belongs to the arbitration token rather than to the DRCluster alone.
  ["POST", /^\/dr-sites\/([\w-]+)\/fence$/, m => {
    const s = siteById(m[1]);
    if (!s) return {__404: true};
    s.fencing_state = "Fenced";
    if (!R.dr_arbitration.fenced_sites.includes(s.name)) R.dr_arbitration.fenced_sites.push(s.name);
    R.dr_arbitration.generation++;
    R.dr_arbitration.updated_at = rago(0);
    drRollup();
    return {results: [s]};
  }],
  ["POST", /^\/dr-sites\/([\w-]+)\/unfence$/, m => {
    const s = siteById(m[1]);
    if (!s) return {__404: true};
    s.fencing_state = "Unfenced";
    R.dr_arbitration.fenced_sites = R.dr_arbitration.fenced_sites.filter(n => n !== s.name);
    R.dr_arbitration.generation++;
    R.dr_arbitration.updated_at = rago(0);
    drRollup();
    return {results: [s]};
  }],
  ["POST", /^\/protected-apps\/([\w-]+)\/verify-generation$/, (m, b) => {
    const a = appById(m[1]);
    if (!a) return {__404: true};
    const gen = (a.generations || []).find(g => String(g.generation) === String(b.generation));
    if (!gen) return {__404: true};
    gen.integrity_state = "Verified";
    gen.verified_at = rago(0);
    return {results: [gen]};
  }]
];

// ---- fetch chain ----------------------------------------------------------
const drPrevFetch = window.fetch.bind(window);
window.fetch = async function (input, init) {
  const cfg = window.SB_CONFIG;
  if (!cfg.mock) return drPrevFetch(input, init);
  const url = typeof input === "string" ? input : input.url;
  const base = cfg.operatorBase + "/proposed";
  if (!url.startsWith(base)) return drPrevFetch(input, init);
  // The client flattens a GET drill-down into /child?scope=<parent collection>
  // &scopeId=<uuid>, so fold it back into the nested form these routes are
  // written against. Anything not ours falls through to the next handler.
  const qi = url.indexOf("?");
  const params = new URLSearchParams(qi >= 0 ? url.slice(qi) : "");
  let route = url.slice(base.length).split("?")[0];
  const scope = params.get("scope"), scopeId = params.get("scopeId");
  if (scope && scopeId) {
    const child = route.replace(/^\//, "");
    const nest = {
      "protection-plans|protected-apps": "/protection-plans/" + scopeId + "/protected-apps",
      "protection-plans|sites": "/protection-plans/" + scopeId + "/sites",
      "dr-sites|protected-apps": "/dr-sites/" + scopeId + "/protected-apps",
      "dr-sites|protection-plans": "/dr-sites/" + scopeId + "/protection-plans",
      "protected-apps|generations": "/protected-apps/" + scopeId + "/generations"
    }[scope + "|" + child];
    if (nest) route = nest;
  }
  const method = ((init || {}).method || "GET").toUpperCase();
  let body = {};
  try { body = init && init.body ? JSON.parse(init.body) : {}; } catch (e) { body = {}; }
  for (const [verb, re, h] of DR_ROUTES) {
    if (verb !== method) continue;
    const mm = route.match(re);
    if (!mm) continue;
    window.SB_JITTER();
    const out = verb === "POST" && re.source === "^\\/protection-plans$" ? h(body) : h(mm, body);
    if (out.__404) return rfail(404, "not found");
    if (out.__err) return rfail(409, out.__err);
    return new Response(JSON.stringify({status: true, results: out.results}),
      {status: 200, headers: {"Content-Type": "application/json"}});
  }
  return drPrevFetch(input, init);
};

window.SB_DR = {drRollup, siteBy, siteById, planBy, ivMinutes, generationsFor, derivedPolicies, classFor};

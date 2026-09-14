// ---------------------------------------------------------------------------
// MOCK ACCESS CONTROL — the hub API server's view of RBAC, per
// uploads/simplyblock-multicluster-rbac-design.md §2.3, §4, §5.
//
// Kubernetes RBAC IS the store. A grant is (subject, role, scope) and becomes a
// RoleBinding in the scope's namespace — or an AccessGrant when it needs expiry,
// a reason, or DR fan-out. Roles are the eight aggregated sb:* ClusterRoles from
// the chart; nothing here authors roles. Served from /proposed/access*.
// The "View as" switcher (localStorage sb.viewas) stands in for impersonation.
// ---------------------------------------------------------------------------
const RB_DB = window.SB_DB, RB_U = window.SB_UTIL;
const rbSlug = s => String(s || "").toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 40);
const RB_RW = ["get", "list", "watch", "create", "update", "patch", "delete"], RB_RO = ["get", "list", "watch"];
const RB_RBAC = "rbac.authorization.k8s.io";
const RB_NS_DR = "sb-dr-system";
const rbRule = (resources, verbs, extra) => Object.assign({apiGroups: ["simplyblock.io"], resources, verbs}, extra || {});

const RB_STORAGE = ["storageclusters", "storagenodes", "devices", "storageclusterops", "storagenodeops", "deviceops", "backuppolicies"];
const RB_POOL = ["storagepools", "volumes", "snapshots", "backups", "buckets", "consistencygroups", "backuppolicies"];
const RB_DR = ["drpolicies", "drclusters", "replicationpolicies", "clusterpairs", "protectionplans"];
const RB_ROLE_NAMES = ["sb:infra-admin", "sb:cluster-admin", "sb:cluster-reader", "sb:pool-admin", "sb:pool-reader", "sb:dr-admin", "sb:dr-reader", "sb:app-admin"];

// ---- the eight aggregated ClusterRoles (§2.3) --------------------------------
const RB_ROLES = [
  {name: "sb:infra-admin", boundAt: "cluster scope", description: "Owns the envelope: node pool allocations, managed clusters, cluster classes. May bind every sb:* role anywhere.",
    parts: [
      {name: "sb:infra-admin-allocations", rules: [rbRule(["nodepoolallocations", "managedclusters", "storageclusterclasses"], RB_RW)]},
      {name: "sb:infra-admin-grants", rules: [rbRule(["accessgrants"], RB_RW), rbRule(["clusterroles"], ["bind"], {apiGroups: [RB_RBAC], resourceNames: RB_ROLE_NAMES})]}]},
  {name: "sb:cluster-admin", boundAt: "sb-sc-<storage cluster>", description: "Runs one storage cluster: nodes, devices, operations. Binds cluster and pool roles inside its own namespace only.",
    parts: [
      {name: "sb:cluster-admin-storage", rules: [rbRule(RB_STORAGE, RB_RW)]},
      {name: "sb:cluster-admin-grants", rules: [rbRule(["accessgrants"], RB_RW), rbRule(["clusterroles"], ["bind"], {apiGroups: [RB_RBAC], resourceNames: ["sb:cluster-admin", "sb:cluster-reader", "sb:pool-admin", "sb:pool-reader"]})]}]},
  {name: "sb:cluster-reader", boundAt: "sb-sc-<storage cluster>", description: "Reads the storage cluster, its nodes, devices and grants. Changes nothing.",
    parts: [{name: "sb:cluster-reader-storage", rules: [rbRule(RB_STORAGE, RB_RO), rbRule(["accessgrants"], RB_RO)]}]},
  {name: "sb:pool-admin", boundAt: "sb-sc-<storage cluster> or sb-sp-<pool>", description: "Owns the data plane below the pool: volumes, snapshots, backups, buckets, backup policies, KEKs. No verbs on nodes or devices, no RBAC on StorageClass.",
    parts: [{name: "sb:pool-admin-volumes", rules: [rbRule(RB_POOL, RB_RW)]}]},
  {name: "sb:pool-reader", boundAt: "sb-sc-<storage cluster> or sb-sp-<pool>", description: "Reads pools and everything beneath, including backups — which is what a restore into another pool needs on the source side.",
    parts: [{name: "sb:pool-reader-volumes", rules: [rbRule(RB_POOL, RB_RO)]}]},
  {name: "sb:dr-admin", boundAt: RB_NS_DR, description: "Defines DR between cluster pairs: DR policies, DR clusters, replication policies and protection plans.",
    parts: [{name: "sb:dr-admin-policies", rules: [rbRule(RB_DR, RB_RW)]}]},
  {name: "sb:dr-reader", boundAt: RB_NS_DR, description: "Reads DR configuration and replication backlog.",
    parts: [{name: "sb:dr-reader-policies", rules: [rbRule(RB_DR, RB_RO)]}]},
  {name: "sb:app-admin", boundAt: "application namespace", description: "Protects an application and may fail it over — failover is create on applicationfailovers, so it is grantable without the right to rewrite the policy.",
    parts: [{name: "sb:app-admin-apps", rules: [rbRule(["protectedapplications", "recipes"], RB_RW), rbRule(["applicationfailovers"], ["create", "get", "list"])]}]}
];
const rbRoleRules = name => { const r = RB_ROLES.find(x => x.name === name); return r ? r.parts.flatMap(p => p.rules) : []; };
RB_ROLES.forEach(r => { r.rules = r.parts.flatMap(p => p.rules); r.uuid = RB_U.uuid(); });

// ---- namespaces: hierarchy in labels, not names (§2.2) ----------------------
const RB_NS = {};
const rbClusterOf = id => RB_DB.clusters.find(c => c.uuid === id);
const rbK8sOf = id => (RB_DB.k8s_clusters || []).find(k => k.uuid === id);
const nsMc = k => "sb-mc-" + rbSlug(k.name);
const nsSc = c => "sb-sc-" + rbSlug(c.name);
const nsApp = (ns, c) => `${ns}@${rbSlug(c.name)}`;
(RB_DB.k8s_clusters || []).forEach(k => { RB_NS[nsMc(k)] = {kind: "managed-cluster", id: k.uuid, label: k.name, labels: {"simplyblock.io/managed-cluster": rbSlug(k.name), "simplyblock.io/scope-kind": "managed-cluster"}}; });
RB_DB.clusters.forEach(c => { RB_NS[nsSc(c)] = {kind: "storage-cluster", id: c.uuid, label: c.name, labels: {"simplyblock.io/storage-cluster": rbSlug(c.name), "simplyblock.io/scope-kind": "storage-cluster"}}; });
// pools default to the storage cluster's namespace (§2.1); every fourth is a
// tenant boundary with its own sb-sp-* namespace, so the difference is visible
RB_DB.pools.forEach((p, i) => {
  const c = rbClusterOf(p.cluster_id);
  p.isolated = i % 4 === 1;
  p.namespace = p.isolated ? `sb-sp-${rbSlug(p.pool_name)}-${p.uuid.slice(0, 6)}` : nsSc(c);
  if (p.isolated) RB_NS[p.namespace] = {kind: "storage-pool", id: p.uuid, label: p.pool_name, labels: {"simplyblock.io/storage-cluster": rbSlug(c.name), "simplyblock.io/storage-pool": rbSlug(p.pool_name), "simplyblock.io/scope-kind": "storage-pool"}};
});
RB_NS[RB_NS_DR] = {kind: "dr", label: "DR", labels: {"simplyblock.io/scope-kind": "dr"}};
(RB_DB.protected_apps || []).forEach(a => {
  const c = rbClusterOf(a.source_cluster_id); if (!c) return;
  const key = nsApp(a.namespace, c);
  RB_NS[key] = RB_NS[key] || {kind: "application", label: `${a.namespace} on ${c.name}`, namespace: a.namespace, clusterId: c.uuid, labels: {"simplyblock.io/scope-kind": "application"}};
});
const rbPoolNs = p => p.namespace;
// namespaces a scope resolves to; application + drPolicy fans out to both members (§4.4)
function rbScopeNamespaces(sc) {
  if (sc.kind === "cluster-scope") return ["*"];
  if (sc.kind === "managed-cluster") { const k = rbK8sOf(sc.id); return k ? [nsMc(k)] : []; }
  if (sc.kind === "storage-cluster") { const c = rbClusterOf(sc.id); return c ? [nsSc(c)] : []; }
  if (sc.kind === "storage-pool") { const p = RB_DB.pools.find(x => x.uuid === sc.id); return p ? [rbPoolNs(p)] : []; }
  if (sc.kind === "dr-pair") return [RB_NS_DR];
  if (sc.kind === "application") {
    const a = (RB_DB.protected_apps || []).find(x => x.uuid === sc.id); if (!a) return [];
    const src = rbClusterOf(a.source_cluster_id); const out = src ? [nsApp(a.namespace, src)] : [];
    if (sc.drPolicy) {
      const pol = (RB_DB.dr_policies || []).find(p => p.uuid === sc.drPolicy || p.name === sc.drPolicy);
      if (pol) [pol.source_cluster_id, pol.target_cluster_id].map(rbClusterOf).filter(Boolean).forEach(c => { const k = nsApp(a.namespace, c); if (!out.includes(k)) out.push(k); });
    }
    return out;
  }
  return [];
}
const rbScopeLabel = sc => sc.kind === "cluster-scope" ? "cluster scope" : sc.name || sc.id;

// ---- identities: IdP groups first, users flagged (§4.1) ----------------------
const rb_c0 = RB_DB.clusters[0], rb_c1 = RB_DB.clusters[1] || rb_c0, rb_c2 = RB_DB.clusters[2] || rb_c1;
const rb_k0 = (RB_DB.k8s_clusters || [])[0];
const rb_pool0 = RB_DB.pools.find(p => p.isolated) || RB_DB.pools[0];
const rb_app0 = (RB_DB.protected_apps || []).find(a => a.source_cluster_id === rb_c0.uuid && a.policy_id) || (RB_DB.protected_apps || [])[0];
const rb_pol0 = rb_app0 ? (RB_DB.dr_policies || []).find(p => p.uuid === rb_app0.policy_id) : null;

const RB_USERS = [
  {name: "oidc:root@simplyblock.io", groups: ["oidc:platform-admins", "system:authenticated"], label: "Root (infra admin)", initials: "RT"},
  {name: "oidc:maria@simplyblock.io", groups: ["oidc:eu-storage", "system:authenticated"], label: `Maria (cluster admin · ${rb_c0.name})`, initials: "MA"},
  {name: "oidc:jonas@simplyblock.io", groups: ["oidc:team-a", "system:authenticated"], label: `Jonas (pool admin · ${rb_pool0.pool_name})`, initials: "JO"},
  {name: "oidc:dr-oncall@simplyblock.io", groups: ["oidc:dr-oncall", "system:authenticated"], label: "DR on-call (app admin, failover)", initials: "DR"},
  {name: "oidc:audit@simplyblock.io", groups: ["oidc:auditors", "system:authenticated"], label: "Auditor (readers everywhere)", initials: "AU"},
  {name: "oidc:backup@simplyblock.io", groups: ["system:authenticated"], label: `Backup operator · ${rb_c1.name} (user-bound, external)`, initials: "BK"}
];

const scSc = c => ({kind: "storage-cluster", id: c.uuid, name: c.name});
const scPool = p => ({kind: "storage-pool", id: p.uuid, name: p.pool_name});
let rbSeq = 0;
function RB_G(subject, role, scope, o) {
  o = o || {};
  const namespaces = rbScopeNamespaces(scope);
  return {uuid: RB_U.uuid(), name: o.name || `${role.replace("sb:", "sb-")}-${rbSlug(subject.name.replace(/^oidc:/, ""))}-${++rbSeq}`,
    subject, role, scope, namespaces, source: o.source || "control-center",
    expires_at: o.expiresAt || null, reason: o.reason || "", created_by: o.by || "oidc:root@simplyblock.io",
    created_at: RB_U.ago(o.age || RB_U.int(50, 2000))};
}
const RB_GRANTS = [
  RB_G({kind: "Group", name: "oidc:platform-admins"}, "sb:infra-admin", {kind: "cluster-scope"}, {name: "sb-infra-admin-platform", reason: "installed by simplyblock-crds chart", age: 3000}),
  // the install also hands the platform group the storage roles on every cluster
  ...RB_DB.clusters.flatMap(c => [
    RB_G({kind: "Group", name: "oidc:platform-admins"}, "sb:cluster-admin", scSc(c), {reason: "bootstrap", age: 2900}),
    RB_G({kind: "Group", name: "oidc:platform-admins"}, "sb:pool-admin", scSc(c), {reason: "bootstrap", age: 2900})]),
  RB_G({kind: "Group", name: "oidc:platform-admins"}, "sb:dr-admin", {kind: "dr-pair", name: "all pairs"}, {reason: "bootstrap", age: 2900}),
  RB_G({kind: "Group", name: "oidc:eu-storage"}, "sb:cluster-admin", scSc(rb_c0), {reason: "OPS-3310 EU storage team"}),
  RB_G({kind: "Group", name: "oidc:eu-storage"}, "sb:pool-admin", scSc(rb_c0), {reason: "OPS-3310 EU storage team"}),
  RB_G({kind: "Group", name: "oidc:team-a"}, "sb:pool-admin", scPool(rb_pool0), {reason: "tenant pool, isolated namespace"}),
  RB_G({kind: "Group", name: "oidc:team-a"}, "sb:cluster-reader", scSc(rb_c1), {by: "oidc:maria@simplyblock.io"}),
  RB_G({kind: "Group", name: "oidc:dr-oncall"}, "sb:dr-reader", {kind: "dr-pair", name: "all pairs"}),
  rb_app0 ? RB_G({kind: "Group", name: "oidc:dr-oncall"}, "sb:app-admin", {kind: "application", id: rb_app0.uuid, name: `${rb_app0.namespace}/${rb_app0.app_name}`, drPolicy: rb_pol0 ? rb_pol0.uuid : null, drPolicyName: rb_pol0 ? rb_pol0.name : null},
    {name: "team-dr-oncall-failover", expiresAt: new Date(Date.now() + 86400e3 * 45).toISOString(), reason: "OPS-4821 on-call rotation"}) : null,
  ...RB_DB.clusters.flatMap(c => [RB_G({kind: "Group", name: "oidc:auditors"}, "sb:cluster-reader", scSc(c), {reason: "SOC2 audit"}), RB_G({kind: "Group", name: "oidc:auditors"}, "sb:pool-reader", scSc(c), {reason: "SOC2 audit"})]),
  RB_G({kind: "Group", name: "oidc:auditors"}, "sb:dr-reader", {kind: "dr-pair", name: "all pairs"}, {reason: "SOC2 audit"}),
  // a RoleBinding someone made with kubectl — shown, not hidden (§4.5)
  RB_G({kind: "User", name: "oidc:backup@simplyblock.io"}, "sb:pool-admin", scSc(rb_c1), {name: "backup-ops-manual", source: "external", by: "kubectl"}),
  // expired: bindings removed by the controller, grant kept for the record
  RB_G({kind: "Group", name: "oidc:team-a"}, "sb:cluster-admin", scSc(rb_c2), {expiresAt: new Date(Date.now() - 86400e3 * 3).toISOString(), reason: "INC-2207 incident response"})
].filter(Boolean);
RB_DB.access_grants = RB_GRANTS; RB_DB.access_users = RB_USERS; RB_DB.access_roles = RB_ROLES;
const rbActive = g => !g.expires_at || Date.parse(g.expires_at) > Date.now();
const rbStatus = g => !rbActive(g) ? "Expired" : g.namespaces.length ? "Active" : "Unresolved";

// ---- who is asking ----------------------------------------------------------
const rbViewAs = () => { const n = localStorage.getItem("sb.viewas"); return RB_USERS.find(u => u.name === n) || RB_USERS[0]; };
const rbSubjectMatches = (g, u) => (g.subject.kind === "User" && g.subject.name === u.name) || (g.subject.kind === "Group" && (u.groups || []).includes(g.subject.name));
const rbGrantsFor = u => RB_GRANTS.filter(g => rbActive(g) && rbSubjectMatches(g, u));

// SelfSubjectRulesReview, one per namespace, aggregated (§5.3)
function rbRulesFor(u) {
  const out = {cluster: [], ns: {}};
  rbGrantsFor(u).forEach(g => {
    const rules = rbRoleRules(g.role).map(r => Object.assign({}, r, {via: g.role}));
    g.namespaces.forEach(n => { if (n === "*") out.cluster.push(...rules); else (out.ns[n] = out.ns[n] || []).push(...rules); });
  });
  return out;
}
const rbRuleAllows = (r, verb, resource, group, name) => (r.apiGroups.includes(group || "simplyblock.io") || r.apiGroups.includes("*"))
  && (r.resources.includes(resource) || r.resources.includes("*")) && (r.verbs.includes(verb) || r.verbs.includes("*"))
  && (!r.resourceNames || !name || r.resourceNames.includes(name));
function rbAllowed(u, verb, resource, ns, group, name) {
  const rules = rbRulesFor(u);
  if (rules.cluster.some(r => rbRuleAllows(r, verb, resource, group, name))) return true;
  return !!ns && ns !== "*" && (rules.ns[ns] || []).some(r => rbRuleAllows(r, verb, resource, group, name));
}
// §4.3: may `u` grant `role` in `ns`? bind on the ClusterRole by name, or every rule of it
const rbMayBind = (u, role, ns) => rbAllowed(u, "create", "accessgrants", ns) && (rbAllowed(u, "bind", "clusterroles", ns, RB_RBAC, role)
  || (rbRoleRules(role).length > 0 && rbRoleRules(role).every(r => r.resources.every(res => r.verbs.every(v => rbAllowed(u, v, res, ns, r.apiGroups[0]))))));

// ---- scope discovery (§5.2): SAR per candidate, only the permitted subtree ---
function rbScopes(u) {
  const canGet = (res, ns) => rbAllowed(u, "get", res, ns);
  const pools = RB_DB.pools.map(p => ({id: p.uuid, name: p.pool_name, clusterId: p.cluster_id, namespace: p.namespace, isolated: !!p.isolated,
    visible: canGet("storagepools", p.namespace)}));
  const clusters = RB_DB.clusters.map(c => {
    const ps = pools.filter(p => p.clusterId === c.uuid);
    const own = canGet("storageclusters", nsSc(c));
    return {id: c.uuid, name: c.name, namespace: nsSc(c), k8sIds: c.k8s_cluster_ids || [], pools: ps, visible: own || ps.some(p => p.visible), full: own,
      sharedPools: ps.filter(p => !p.isolated).length};
  });
  const managed = (RB_DB.k8s_clusters || []).map(k => {
    const cs = clusters.filter(c => c.k8sIds.includes(k.uuid));
    return {id: k.uuid, name: k.name, namespace: nsMc(k), visible: canGet("managedclusters", nsMc(k)) || cs.some(c => c.visible), clusters: cs.map(c => c.id)};
  });
  const dr = {namespace: RB_NS_DR, visible: canGet("drpolicies", RB_NS_DR),
    pairs: (RB_DB.dr_policies || []).map(p => ({id: p.uuid, name: p.name, sourceClusterId: p.source_cluster_id, targetClusterId: p.target_cluster_id}))};
  const apps = (RB_DB.protected_apps || []).map(a => { const c = rbClusterOf(a.source_cluster_id); const key = c ? nsApp(a.namespace, c) : null;
    return {id: a.uuid, name: `${a.namespace}/${a.app_name}`, clusterId: a.source_cluster_id, namespace: key, drPolicy: a.policy_id || null, visible: !!key && canGet("protectedapplications", key)}; });
  return {managed, clusters, pools, dr, apps,
    // id -> namespace, so the client never slugs names itself
    ns: {clusters: Object.fromEntries(clusters.map(c => [c.id, c.namespace])), pools: Object.fromEntries(pools.map(p => [p.id, p.namespace])),
      managed: Object.fromEntries(managed.map(m => [m.id, m.namespace])), apps: Object.fromEntries(apps.map(a => [a.id, a.namespace])), dr: RB_NS_DR}};
}

const rbSelf = () => {
  const u = rbViewAs();
  return {user: u.name, groups: u.groups, initials: u.initials, label: u.label, rules: rbRulesFor(u), incomplete: false,
    grants: rbGrantsFor(u).map(g => ({uuid: g.uuid, role: g.role, scope: g.scope, namespaces: g.namespaces, subject: g.subject})),
    scopes: rbScopes(u),
    demo_users: RB_USERS.map(x => ({name: x.name, label: x.label, initials: x.initials}))};
};

const rbJ = (o, code) => new Response(JSON.stringify(o), {status: code || 200, headers: {"Content-Type": "application/json"}});
const rbDeny = msg => rbJ({status: false, error: msg, reason: "Forbidden"}, 403);
const rbBad = msg => rbJ({status: false, error: msg, reason: "Invalid"}, 409);

// the AccessGrant webhook (§4.3, §4.4). Returns {error} or {grant}. `requester`
// comes from AdmissionRequest.userInfo — never from the body (invariant 4).
function rbAdmitGrant(u, body) {
  const sc = body.scope || {};
  if (!body.subject || !body.subject.name || !body.role) return {error: "subject and role are required"};
  if (!RB_ROLES.some(r => r.name === body.role)) return {error: `${body.role} is not one of the sb:* ClusterRoles. Roles are aggregated by the chart, not created here.`};
  if (body.expires_at && !(Date.parse(body.expires_at) > Date.now())) return {error: "expiresAt must be in the future"};
  const namespaces = rbScopeNamespaces(sc);
  if (!namespaces.length) return {error: "the scope does not resolve to a namespace"};
  if (sc.kind === "cluster-scope" && body.role !== "sb:infra-admin") return {error: "only sb:infra-admin is bound at cluster scope; every other role needs a namespace"};
  if (sc.kind !== "cluster-scope" && body.role === "sb:infra-admin") return {error: "sb:infra-admin is cluster-scoped"};
  // §4.4 DR symmetry: both members registered and reachable, or nothing
  if (sc.kind === "application" && sc.drPolicy) {
    const pol = (RB_DB.dr_policies || []).find(p => p.uuid === sc.drPolicy || p.name === sc.drPolicy);
    if (!pol) return {error: "the DR policy named in the scope does not exist"};
    const pair = (RB_DB.cluster_pairs || []).find(p => p.uuid === pol.pair_id);
    const members = [pol.source_cluster_id, pol.target_cluster_id].map(rbClusterOf);
    if (members.some(c => !c)) return {error: "a member cluster of the DR pair is not registered — refusing to bind one side only"};
    if (pair && pair.status === "unreachable") return {error: `the pair link to ${members[1].name} is down — bindings would land on ${members[0].name} only, so the grant is refused (both or neither)`};
    if (namespaces.length < 2) return {error: "DR fan-out resolved to a single namespace"};
  }
  // §4.3: the requester must be able to bind this role in EVERY target namespace
  const denied = namespaces.filter(n => !rbMayBind(u, body.role, n));
  if (denied.length) return {error: `${u.name} cannot bind ${body.role} in ${denied.join(", ")} — needs bind on that ClusterRole, or every right it grants, there`, forbidden: true};
  const g = RB_G(body.subject, body.role, sc, {name: body.name, expiresAt: body.expires_at || null, reason: body.reason || "", by: u.name, age: 0});
  g.created_at = RB_U.ago(0);
  return {grant: g, warning: body.subject.kind === "User" ? "Bound to a user, not a group. Offboarding this person now needs a Kubernetes action." : null};
}

// Called by the mock API server for /proposed/access*. Returns a Response or null.
window.SB_ACCESS_ROUTE = function (method, path, body) {
  const u = rbViewAs();
  const [p, qs] = path.split("?"); const q = new URLSearchParams(qs || "");
  if (p === "/proposed/access/self") return rbJ({results: [rbSelf()], proposed: true});
  if (p === "/proposed/access/scopes") return rbJ({results: [rbScopes(u)], proposed: true});
  if (p === "/proposed/access-roles") return method === "GET" ? rbJ({results: RB_ROLES, proposed: true})
    : rbJ({status: false, error: "sb:* roles are aggregated ClusterRoles owned by the simplyblock-crds chart. To extend one, label a ClusterRole simplyblock.io/aggregate-to-<role>: \"true\"."}, 405);

  if (p === "/proposed/access/effective") {
    const [kind, ...rest] = (q.get("subject") || "").split(":"); const name = rest.join(":");
    const user = kind === "User" ? (RB_USERS.find(x => x.name === name) || {name, groups: []}) : {name: "", groups: [name]};
    const gs = RB_GRANTS.filter(g => rbSubjectMatches(g, user));
    return rbJ({results: [{subject: {kind, name}, groups: user.groups, grants: gs, rules: rbRulesFor(user),
      note: "Assembled from grants issued through simplyblock. ClusterRoles the platform team created independently are not visible here."}], proposed: true});
  }
  if (p === "/proposed/access/review" && method === "POST") {
    // SubjectAccessReview for a named subject (§4.5 spot check)
    const s = body.subject || {}; const user = s.kind === "User" ? (RB_USERS.find(x => x.name === s.name) || {name: s.name, groups: []}) : {name: "", groups: [s.name]};
    const allowed = rbAllowed(user, body.verb, body.resource, body.namespace, body.group);
    const via = allowed ? rbGrantsFor(user).filter(g => (g.namespaces.includes("*") || g.namespaces.includes(body.namespace)) && rbRoleRules(g.role).some(r => rbRuleAllows(r, body.verb, body.resource, body.group))).map(g => g.role) : [];
    return rbJ({results: [{allowed, via: [...new Set(via)], reason: allowed ? `granted via ${[...new Set(via)].join(", ")}` : `no rule grants ${body.verb} on ${body.resource} in ${body.namespace || "cluster scope"}`}]});
  }
  if (p === "/proposed/access/subjects") {
    const subs = {}; RB_GRANTS.forEach(g => { subs[g.subject.kind + ":" + g.subject.name] = g.subject; });
    RB_USERS.forEach(x => { subs["User:" + x.name] = {kind: "User", name: x.name}; x.groups.forEach(gr => { subs["Group:" + gr] = {kind: "Group", name: gr}; }); });
    return rbJ({results: Object.values(subs)});
  }
  if (p === "/proposed/access/invariants") return rbJ({results: rbInvariants()});
  if (p === "/proposed/access/namespaces") return rbJ({results: Object.entries(RB_NS).map(([name, v]) => Object.assign({name}, v))});

  let m = p.match(/^\/proposed\/access-grants(?:\/([\w-]+))?$/);
  if (m) {
    if (method === "GET") {
      // a grant is visible where the caller may get accessgrants (§5.1: 403, not empty, when nothing at all is visible)
      const vis = RB_GRANTS.filter(g => g.namespaces.some(n => rbAllowed(u, "get", "accessgrants", n)));
      if (!vis.length && !rbAllowed(u, "get", "accessgrants", "*")) return rbDeny(`${u.name} may not list accessgrants in any namespace`);
      const withStatus = vis.map(g => Object.assign({}, g, {status: rbStatus(g)}));
      return rbJ({results: m[1] ? withStatus.filter(g => g.uuid === m[1]) : withStatus, proposed: true});
    }
    if (method === "POST") {
      const r = rbAdmitGrant(u, body);
      if (r.error) return r.forbidden ? rbDeny(r.error) : rbBad(r.error);
      RB_GRANTS.push(r.grant); return rbJ({results: [Object.assign({warning: r.warning, status: "Active"}, r.grant)]});
    }
    const g = RB_GRANTS.find(x => x.uuid === m[1]);
    if (!g) return rbJ({status: false, error: "not found"}, 404);
    if (method === "DELETE") {
      if (g.source === "external") return rbBad(`${g.name} is a RoleBinding not owned by an AccessGrant — remove it with kubectl in ${g.namespaces.join(", ")}`);
      const denied = g.namespaces.filter(n => !rbAllowed(u, "delete", "accessgrants", n));
      if (denied.length) return rbDeny(`${u.name} may not delete accessgrants in ${denied.join(", ")}`);
      RB_GRANTS.splice(RB_GRANTS.indexOf(g), 1); return rbJ({results: [g]});
    }
    return rbJ({status: "ok"});
  }
  return null;
};

// ---- §6 invariants, run against this authorizer ------------------------------
function rbInvariants() {
  const maria = RB_USERS[1], jonas = RB_USERS[2], root = RB_USERS[0];
  const out = [];
  const T = (n, title, run) => { try { const r = run(); out.push(Object.assign({n, title}, r)); } catch (e) { out.push({n, title, ok: false, detail: "threw: " + e.message}); } };
  T(1, "cluster-admin on one storage cluster reaches nothing in another", () => {
    const other = nsSc(rb_c1), own = nsSc(rb_c0);
    const leaks = ["get", "update", "delete"].filter(v => rbAllowed(maria, v, "storageclusters", other)).concat(rbAllowed(maria, "create", "accessgrants", other) ? ["grant"] : []);
    return {ok: !leaks.length && rbAllowed(maria, "update", "storageclusters", own), detail: leaks.length ? `leaks: ${leaks.join(", ")} in ${other}` : `no verb on ${other}; full on ${own}`};
  });
  T(2, "pool-admin cannot read nodes or devices, nor create a StorageClass", () => {
    const g = RB_G({kind: "Group", name: "probe-pool"}, "sb:pool-admin", scSc(rb_c0)); RB_GRANTS.push(g);
    const only = {name: "probe", groups: ["probe-pool"]};
    const bad = Object.keys(RB_NS).filter(n => rbAllowed(only, "get", "storagenodes", n) || rbAllowed(only, "get", "devices", n) || rbAllowed(only, "create", "storageclasses", n, "storage.k8s.io"));
    RB_GRANTS.pop();
    return {ok: !bad.length, detail: bad.length ? `reachable in ${bad.join(", ")}` : "no storagenodes, devices or storage.k8s.io verbs in any namespace"};
  });
  T(3, "StorageCluster outside its NodePoolAllocation is rejected at admission", () => {
    const env = window.SB_ENVELOPE; if (!env) return {ok: null, detail: "admission half: no envelope validator loaded; node-agent half is backend-only"};
    const r = env({managedCluster: "x", nodeSelector: {"simplyblock.io/pool": "other"}, devices: [{path: "/dev/disk/by-id/nvme-INTEL_1"}], isolatedCoresPerNode: 64}, {managedCluster: "x", allowedNodeSelector: {"simplyblock.io/pool": "storage-a"}, allowedDevicePatterns: ["/dev/disk/by-id/nvme-SAMSUNG_MZ"], maxIsolatedCoresPerNode: 8});
    return {ok: r.length >= 3, detail: `${r.length} violation(s): ${r.join("; ")} — node agent's second gate is backend-only`};
  });
  T(4, "client-supplied actor annotations are overwritten", () => {
    const r = rbAdmitGrant(root, {subject: {kind: "Group", name: "oidc:probe"}, role: "sb:cluster-reader", scope: scSc(rb_c0), created_by: "oidc:forged@evil", createdBy: "oidc:forged@evil"});
    return {ok: !!r.grant && r.grant.created_by === root.name, detail: r.grant ? `stamped ${r.grant.created_by}, body value ignored` : r.error};
  });
  out.push({n: 5, title: "delegated token scoped to pool1 is rejected for pool2 by the simplyblock API", ok: null, detail: "backend-only: token exchange and scope intersection live in the REST API (§3)"});
  T(6, "every console action maps to a (resource, verb)", () => {
    const A = window.ACTIONS || {}, KE = window.KIND_ENTITY || {};
    const missing = Object.keys(A).filter(k => !KE[k]);
    return {ok: !missing.length, detail: missing.length ? `unmapped kinds: ${missing.join(", ")}` : `${Object.keys(A).length} action kinds mapped — the REST router half (§3.5) is backend-only`};
  });
  T(7, "no role can grant a role it may not bind", () => {
    const own = nsSc(rb_c0); let checked = 0, bad = [];
    RB_ROLE_NAMES.forEach(a => RB_ROLE_NAMES.forEach(b => {
      const holder = {name: "probe", groups: ["probe-" + a]};
      const g = RB_G({kind: "Group", name: "probe-" + a}, a, a === "sb:infra-admin" ? {kind: "cluster-scope"} : a.startsWith("sb:dr") ? {kind: "dr-pair", name: "x"} : a === "sb:app-admin" && rb_app0 ? {kind: "application", id: rb_app0.uuid} : scSc(rb_c0));
      RB_GRANTS.push(g);
      const allowed = rbMayBind(holder, b, own);
      RB_GRANTS.pop(); checked++;
      const expected = a === "sb:infra-admin" || (a === "sb:cluster-admin" && ["sb:cluster-admin", "sb:cluster-reader", "sb:pool-admin", "sb:pool-reader"].includes(b));
      if (allowed !== expected) bad.push(`${a}→${b}`);
    }));
    return {ok: !bad.length, detail: bad.length ? `unexpected: ${bad.join(", ")}` : `${checked} role pairs, only infra-admin and cluster-admin (within its namespace) may bind`};
  });
  out.push({n: 8, title: "a UI backend bug cannot escalate", ok: null, detail: "every mutation here goes through the API server under the impersonated identity; the fixture re-authorizes accessgrants writes (403 on denied) — other kinds are backend-only"});
  T(9, "revoking IdP group membership removes access with no Kubernetes action", () => {
    const users = RB_GRANTS.filter(g => g.source === "control-center" && g.subject.kind === "User");
    const before = rbAllowed(maria, "update", "storageclusters", nsSc(rb_c0));
    const after = rbAllowed({name: maria.name, groups: ["system:authenticated"]}, "update", "storageclusters", nsSc(rb_c0));
    return {ok: before && !after && !users.length, detail: `group removal: ${before} → ${after}; ${users.length} control-center grant(s) bound to a user${users.length ? " — those need a Kubernetes action" : ""}`};
  });
  T(10, "a DR grant lands on both members or neither", () => {
    if (!rb_app0 || !rb_pol0) return {ok: null, detail: "no application with a DR policy in the fixture"};
    const pair = (RB_DB.cluster_pairs || []).find(p => p.uuid === rb_pol0.pair_id); const was = pair && pair.status;
    if (pair) pair.status = "unreachable";
    const r1 = rbAdmitGrant(root, {subject: {kind: "Group", name: "probe"}, role: "sb:app-admin", scope: {kind: "application", id: rb_app0.uuid, drPolicy: rb_pol0.uuid}});
    if (pair) pair.status = was;
    const r2 = rbAdmitGrant(root, {subject: {kind: "Group", name: "probe"}, role: "sb:app-admin", scope: {kind: "application", id: rb_app0.uuid, drPolicy: rb_pol0.uuid}});
    const ok = !!r1.error && !!r2.grant && r2.grant.namespaces.length === 2;
    return {ok, detail: `link down → ${r1.error ? "refused" : "ACCEPTED"}; link up → ${r2.grant ? r2.grant.namespaces.length + " namespaces" : r2.error}`};
  });
  return out;
}

// §2.4 envelope, the admission half — used by the deploy wizard's dry run
window.SB_ENVELOPE = function (spec, alloc) {
  const v = [];
  if (spec.managedCluster !== alloc.managedCluster) v.push("targets a managed cluster outside the bound allocation");
  if ((spec.isolatedCoresPerNode || 0) > alloc.maxIsolatedCoresPerNode) v.push("requested core isolation exceeds the allocation budget");
  const sel = spec.nodeSelector || {};
  if (!Object.keys(sel).every(k => k in alloc.allowedNodeSelector && sel[k] === alloc.allowedNodeSelector[k])) v.push("node selector outside the allocation");
  if (!(spec.devices || []).every(d => alloc.allowedDevicePatterns.some(p => (d.path || "").startsWith(p)))) v.push("device path outside the allocation");
  return v;
};
Object.assign(window, {ACCESS_ROLE_NAMES: RB_ROLE_NAMES});

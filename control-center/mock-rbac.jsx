// ---------------------------------------------------------------------------
// MOCK ACCESS CONTROL — AccessRole / AccessBinding, served from
// /proposed/access-roles, /proposed/access-bindings and /proposed/access/self.
//
// The fixture backend has a "View as" switcher (localStorage sb.viewas) so
// every pre-defined role can be walked in the preview. A real deployment gets
// the identity from the API server via SB_AUTH_MODE=passthrough — there is no
// user database here and there must not be one.
// ---------------------------------------------------------------------------
const RB_DB = window.SB_DB, RB_U = window.SB_UTIL;

const RB_CRUD = ["create", "read", "update", "delete"];
const RB_ENTITY_OPS = {
  k8scluster: RB_CRUD, storagecluster: RB_CRUD, storagepool: [...RB_CRUD, "restoresource"],
  backupop: ["backup", "restore"], replicationpolicy: RB_CRUD, backuppolicy: RB_CRUD, drpolicy: RB_CRUD,
  application: [...RB_CRUD, "failover", "failback", "fence"], role: RB_CRUD, binding: RB_CRUD
};
const rbAll = e => RB_ENTITY_OPS[e].slice();
const RB_R = (e, ops) => ({entity: e, ops});
const RB_ALL_ENTITIES = Object.keys(RB_ENTITY_OPS);

const RB_ROLES = [
  {name: "global-admin", builtin: true, description: "Everything, everywhere — including authoring roles and binding anyone at any scope. Only meaningful at global scope.",
    rights: RB_ALL_ENTITIES.map(e => RB_R(e, rbAll(e)))},
  {name: "cluster-admin", builtin: true, description: "Runs one or more clusters end to end and binds users within them. Cannot author roles.",
    rights: [RB_R("storagecluster", rbAll("storagecluster")), RB_R("storagepool", rbAll("storagepool")), RB_R("backupop", rbAll("backupop")),
      RB_R("replicationpolicy", RB_CRUD), RB_R("backuppolicy", RB_CRUD), RB_R("drpolicy", RB_CRUD), RB_R("application", rbAll("application")),
      RB_R("binding", RB_CRUD), RB_R("k8scluster", ["read"])]},
  {name: "storage-operator", builtin: true, description: "Day-2 operations on clusters and pools. Policies are visible, not editable.",
    rights: [RB_R("storagecluster", RB_CRUD), RB_R("storagepool", rbAll("storagepool")), RB_R("backupop", rbAll("backupop")),
      RB_R("replicationpolicy", ["read"]), RB_R("backuppolicy", ["read"]), RB_R("drpolicy", ["read"]), RB_R("application", ["read"]), RB_R("k8scluster", ["read"])]},
  {name: "volume-admin", builtin: true, description: "Owns the data plane below the pool: volumes, snapshots, clones, PVCs, backups, KEKs.",
    rights: [RB_R("storagepool", rbAll("storagepool")), RB_R("backupop", rbAll("backupop")), RB_R("storagecluster", ["read"]), RB_R("backuppolicy", ["read"])]},
  {name: "backup-operator", builtin: true, description: "Takes and restores backups and owns backup policies. Sees pools, changes nothing else in them.",
    rights: [RB_R("backupop", rbAll("backupop")), RB_R("storagepool", ["read", "restoresource"]), RB_R("backuppolicy", RB_CRUD), RB_R("storagecluster", ["read"])]},
  {name: "dr-operator", builtin: true, description: "Reads everything; owns replication, DR policies and protected applications, and may fail over, fail back and fence.",
    rights: [RB_R("storagecluster", ["read"]), RB_R("storagepool", ["read"]), RB_R("k8scluster", ["read"]), RB_R("backuppolicy", ["read"]),
      RB_R("replicationpolicy", RB_CRUD), RB_R("drpolicy", RB_CRUD), RB_R("application", rbAll("application"))]},
  {name: "viewer", builtin: true, description: "Read-only on every entity, including policy backlogs.",
    rights: RB_ALL_ENTITIES.filter(e => e !== "backupop").map(e => RB_R(e, ["read"]))},
  // one custom role, so the editor has something non-builtin to show
  {name: "eu-failover-only", builtin: false, description: "Can fail over and fence protected applications, nothing else. Used by the on-call rota.",
    rights: [RB_R("application", ["read", "failover", "fence"]), RB_R("storagecluster", ["read"]), RB_R("replicationpolicy", ["read"])],
    createdBy: "oidc:root@simplyblock.io"}
].map(r => Object.assign({uuid: RB_U.uuid(), created_at: RB_U.ago(RB_U.int(400, 3000))}, r));

// ---- identities: Kubernetes users and groups, as the API server names them --
const rb_c0 = RB_DB.clusters[0], rb_c1 = RB_DB.clusters[1] || RB_DB.clusters[0], rb_c2 = RB_DB.clusters[2] || rb_c1;
const rb_k0 = (RB_DB.k8s_clusters || [])[0];
const rb_pool0 = RB_DB.pools.find(p => p.cluster_id === rb_c1.uuid) || RB_DB.pools[0];
const rb_app0 = (RB_DB.protected_apps || []).find(a => a.source_cluster_id === rb_c0.uuid) || (RB_DB.protected_apps || [])[0];

const RB_USERS = [
  {name: "oidc:root@simplyblock.io", groups: ["oidc:platform-admins", "system:authenticated"], label: "Root (global admin)", initials: "RT"},
  {name: "oidc:maria@simplyblock.io", groups: ["oidc:eu-storage", "system:authenticated"], label: `Maria (cluster admin · ${rb_c0.name})`, initials: "MA"},
  {name: "oidc:jonas@simplyblock.io", groups: ["oidc:sre", "system:authenticated"], label: `Jonas (volume admin · pool ${rb_pool0.pool_name})`, initials: "JO"},
  {name: "oidc:dr-oncall@simplyblock.io", groups: ["oidc:dr-oncall", "system:authenticated"], label: "DR on-call (failover only)", initials: "DR"},
  {name: "oidc:audit@simplyblock.io", groups: ["oidc:auditors", "system:authenticated"], label: "Auditor (viewer)", initials: "AU"},
  {name: "oidc:backup@simplyblock.io", groups: ["oidc:sre", "system:authenticated"], label: `Backup operator · ${rb_c1.name}`, initials: "BK"}
];

const rbScope = (kind, obj) => kind === "global" ? {kind: "global"} : {kind, id: obj.uuid, name: obj.name || obj.pool_name};
const RB_B = (subject, role, sc, by) => ({uuid: RB_U.uuid(), subject, role, scope: sc, bound_by: by || "oidc:root@simplyblock.io",
  created_at: RB_U.ago(RB_U.int(50, 2000)), effective: true});
const RB_BINDINGS = [
  RB_B({kind: "Group", name: "oidc:platform-admins"}, "global-admin", rbScope("global")),
  RB_B({kind: "User", name: "oidc:maria@simplyblock.io"}, "cluster-admin", rbScope("storagecluster", rb_c0)),
  RB_B({kind: "User", name: "oidc:jonas@simplyblock.io"}, "volume-admin", rbScope("storagepool", rb_pool0)),
  RB_B({kind: "User", name: "oidc:jonas@simplyblock.io"}, "viewer", rbScope("storagecluster", rb_c1), "oidc:maria@simplyblock.io"),
  RB_B({kind: "Group", name: "oidc:dr-oncall"}, "eu-failover-only", rbScope("storagecluster", rb_c0)),
  rb_app0 ? RB_B({kind: "Group", name: "oidc:dr-oncall"}, "dr-operator", rbScope("application", rb_app0)) : null,
  RB_B({kind: "Group", name: "oidc:auditors"}, "viewer", rbScope("global")),
  RB_B({kind: "User", name: "oidc:backup@simplyblock.io"}, "backup-operator", rbScope("storagecluster", rb_c1)),
  rb_k0 ? RB_B({kind: "Group", name: "oidc:eu-storage"}, "storage-operator", rbScope("k8scluster", rb_k0)) : null,
  RB_B({kind: "Group", name: "oidc:sre"}, "viewer", rbScope("storagecluster", rb_c2))
].filter(Boolean);

RB_DB.access_roles = RB_ROLES; RB_DB.access_bindings = RB_BINDINGS; RB_DB.access_users = RB_USERS;

// ---- who is asking ----------------------------------------------------------
const rbViewAs = () => {
  const n = localStorage.getItem("sb.viewas");
  return RB_USERS.find(u => u.name === n) || RB_USERS[0];
};
const rbBindingsFor = u => RB_BINDINGS.filter(b =>
  (b.subject.kind === "User" && b.subject.name === u.name) ||
  (b.subject.kind === "Group" && u.groups.includes(b.subject.name)));

const rbSelf = () => {
  const u = rbViewAs();
  const bs = rbBindingsFor(u).map(b => Object.assign({}, b, {role_rights: (RB_ROLES.find(r => r.name === b.role) || {rights: []}).rights}));
  return {user: u.name, groups: u.groups, initials: u.initials, label: u.label, bindings: bs,
    // for the demo switcher only — a real /access/self never lists other users
    demo_users: RB_USERS.map(x => ({name: x.name, label: x.label, initials: x.initials}))};
};

// ---- authority checks the webhook would make --------------------------------
const rbRoleRights = name => (RB_ROLES.find(r => r.name === name) || {rights: []}).rights;
const rbHas = (u, entity, op, sc) => rbBindingsFor(u).some(b =>
  rbRoleRights(b.role).some(r => r.entity === entity && r.ops.includes(op)) && rbContains(b.scope, sc));
// scope containment, mirroring RBAC-DESIGN.md
function rbContains(outer, inner) {
  if (outer.kind === "global") return true;
  if (outer.kind === inner.kind) return outer.id === inner.id;
  if (outer.kind === "k8scluster") {
    const clusters = RB_DB.clusters.filter(c => (c.k8s_cluster_ids || []).includes(outer.id)).map(c => c.uuid);
    return clusters.some(cid => rbContains({kind: "storagecluster", id: cid}, inner));
  }
  if (outer.kind === "storagecluster") {
    if (inner.kind === "storagepool") return RB_DB.pools.some(p => p.uuid === inner.id && p.cluster_id === outer.id);
    if (inner.kind === "application") return (RB_DB.protected_apps || []).some(a => a.uuid === inner.id && a.source_cluster_id === outer.id);
  }
  return false;
}

const rbJ = (o, code) => new Response(JSON.stringify(o), {status: code || 200, headers: {"Content-Type": "application/json"}});
const rbDeny = (msg) => rbJ({status: false, error: msg, reason: "Forbidden"}, 403);

// Called by the mock API server for /proposed/access*. Returns a Response or null.
window.SB_ACCESS_ROUTE = function (method, path, body) {
  const u = rbViewAs();
  if (path === "/proposed/access/self") return rbJ({results: [rbSelf()], proposed: true});

  let m = path.match(/^\/proposed\/access-roles(?:\/([\w-]+))?$/);
  if (m) {
    if (method === "GET") return rbJ({results: m[1] ? RB_ROLES.filter(r => r.uuid === m[1] || r.name === m[1]) : RB_ROLES, proposed: true});
    const op = method === "POST" ? "create" : method === "DELETE" ? "delete" : "update";
    if (!rbHas(u, "role", op, {kind: "global"})) return rbDeny(`${u.name} may not ${op} roles — that needs role.${op} at global scope`);
    if (method === "POST") {
      if (!body.name || !/^[a-z0-9-]{3,40}$/.test(body.name)) return rbJ({status: false, error: "Role names are 3–40 lowercase letters, digits and dashes"}, 409);
      if (RB_ROLES.some(r => r.name === body.name)) return rbJ({status: false, error: `A role named ${body.name} already exists`}, 409);
      const r = {uuid: RB_U.uuid(), name: body.name, builtin: false, description: body.description || "", rights: rbNormRights(body.rights), createdBy: u.name, created_at: RB_U.ago(0)};
      RB_ROLES.push(r); return rbJ({results: [r]});
    }
    const r = RB_ROLES.find(x => x.uuid === m[1] || x.name === m[1]);
    if (!r) return rbJ({status: false, error: "not found"}, 404);
    if (r.builtin) return rbJ({status: false, error: `${r.name} is a pre-defined role and is immutable`}, 409);
    if (method === "DELETE") {
      const inUse = RB_BINDINGS.filter(b => b.role === r.name).length;
      if (inUse) return rbJ({status: false, error: `${r.name} is bound ${inUse} time(s). Remove the bindings first.`}, 409);
      RB_ROLES.splice(RB_ROLES.indexOf(r), 1); return rbJ({results: [r]});
    }
    if (body.description !== undefined) r.description = body.description;
    if (body.rights) r.rights = rbNormRights(body.rights);
    return rbJ({results: [r]});
  }

  m = path.match(/^\/proposed\/access-bindings(?:\/([\w-]+))?$/);
  if (m) {
    if (method === "GET") {
      // a subject sees the bindings in scopes they hold binding.read on
      const vis = RB_BINDINGS.filter(b => rbHas(u, "binding", "read", b.scope));
      return rbJ({results: m[1] ? vis.filter(b => b.uuid === m[1]) : vis, proposed: true});
    }
    if (method === "POST") {
      const sc = body.scope || {};
      if (!body.subject || !body.subject.name || !body.role) return rbJ({status: false, error: "subject and role are required"}, 409);
      if (!RB_ROLES.some(r => r.name === body.role)) return rbJ({status: false, error: `no role named ${body.role}`}, 409);
      if (!rbHas(u, "binding", "create", sc)) return rbDeny(`${u.name} may not bind at ${sc.kind}${sc.name ? " " + sc.name : ""} — a cluster admin binds only inside their own clusters`);
      if (body.role === "global-admin" && sc.kind !== "global") return rbJ({status: false, error: "global-admin is only meaningful at global scope"}, 409);
      const b = RB_B(body.subject, body.role, sc, u.name); b.created_at = RB_U.ago(0);
      RB_BINDINGS.push(b); return rbJ({results: [b]});
    }
    const b = RB_BINDINGS.find(x => x.uuid === m[1]);
    if (!b) return rbJ({status: false, error: "not found"}, 404);
    if (method === "DELETE") {
      if (!rbHas(u, "binding", "delete", b.scope)) return rbDeny(`${u.name} may not remove bindings at ${b.scope.kind}${b.scope.name ? " " + b.scope.name : ""}`);
      RB_BINDINGS.splice(RB_BINDINGS.indexOf(b), 1); return rbJ({results: [b]});
    }
    return rbJ({status: "ok"});
  }
  return null;
};

function rbNormRights(rs) {
  return (rs || []).map(r => ({entity: r.entity, ops: (r.ops || []).filter(o => (RB_ENTITY_OPS[r.entity] || []).includes(o))}))
    .filter(r => RB_ENTITY_OPS[r.entity] && r.ops.length);
}

Object.assign(window, {ACCESS_ENTITY_OPS: RB_ENTITY_OPS});

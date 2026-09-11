// ---------------------------------------------------------------------------
// ACCESS CONTROL — the client side of §5 of the RBAC design.
//
// The API server is the only policy decision point. This file never enforces:
// it asks one SelfSubjectRulesReview per namespace (aggregated in /access/self),
// evaluates locally, and renders. Denied controls are DISABLED with the missing
// permission in the tooltip; only whole scopes the caller cannot see are hidden.
// incomplete:true renders optimistically — the real call fails with a clear 403.
// ---------------------------------------------------------------------------
const OPS_LABEL = {create: "C", read: "R", update: "U", delete: "D", restoresource: "src", backup: "bak", restore: "rst", failover: "f/o", failback: "f/b", fence: "fence"};
const ENTITY_LABEL = {
  k8scluster: "Managed cluster", storagecluster: "Storage cluster", storagepool: "Storage pool", backupop: "Backup / restore",
  replicationpolicy: "Replication policy", backuppolicy: "Backup policy", drpolicy: "DR policy", application: "Application", role: "ClusterRole", binding: "AccessGrant"
};
const ENTITY_COVERS = {
  k8scluster: "worker nodes, discovery, allocations", storagecluster: "hosts, nodes, devices, tasks, logs, migrations, S3 target, KMS endpoint",
  storagepool: "volumes, snapshots, clones, PVCs, backups, buckets, KEKs", backupop: "per-volume backup and restore",
  replicationpolicy: "pairs, policies, slots", backuppolicy: "schedules and retention", drpolicy: "protection plans, methods, migration paths",
  application: "protected applications, recipes, failovers", role: "aggregated sb:* ClusterRole", binding: "RoleBinding or AccessGrant"
};
const ENTITY_OPS = {
  k8scluster: ["create", "read", "update", "delete"], storagecluster: ["create", "read", "update", "delete"],
  storagepool: ["create", "read", "update", "delete", "restoresource"], backupop: ["backup", "restore"],
  replicationpolicy: ["create", "read", "update", "delete"], backuppolicy: ["create", "read", "update", "delete"],
  drpolicy: ["create", "read", "update", "delete"], application: ["create", "read", "update", "delete", "failover", "failback", "fence"],
  role: ["read"], binding: ["create", "read", "delete"]
};
// UI kind -> the main entity whose namespace governs it
const KIND_ENTITY = {
  cluster: "storagecluster", host: "storagecluster", node: "storagecluster", device: "storagecluster",
  task: "storagecluster", migration: "storagecluster", deployconfig: "k8scluster", k8sc: "k8scluster", zone: "k8scluster",
  pool: "storagepool", volume: "storagepool", snapshot: "storagepool", backup: "storagepool", pvc: "storagepool",
  storageclass: "storagepool", bucket: "storagepool", cgroup: "storagepool", cgsnapshot: "storagepool",
  policy: "backuppolicy", pair: "replicationpolicy", rpolicy: "replicationpolicy", slot: "replicationpolicy", replop: "replicationpolicy",
  plan: "drpolicy", method: "drpolicy", site: "drpolicy", mpath: "drpolicy", appgroup: "drpolicy",
  protectedapp: "application", role: "role", binding: "binding", grant: "binding", replops: "replicationpolicy"
};
// UI kind -> the CRD resource the API server checks (§3.5, the console's column)
const KIND_RESOURCE = {
  cluster: "storageclusters", host: "storagenodes", node: "storagenodes", device: "devices", task: "storageclusters", migration: "storageclusters",
  k8sc: "managedclusters", zone: "managedclusters", deployconfig: "nodepoolallocations",
  pool: "storagepools", volume: "volumes", pvc: "volumes", storageclass: "storagepools", bucket: "buckets", cgroup: "consistencygroups",
  cgsnapshot: "snapshots", snapshot: "snapshots", backup: "backups", policy: "backuppolicies",
  pair: "clusterpairs", rpolicy: "replicationpolicies", slot: "replicationpolicies", replop: "replicationpolicies", replops: "replicationpolicies",
  plan: "protectionplans", method: "protectionplans", site: "drclusters", mpath: "protectionplans", appgroup: "protectionplans",
  protectedapp: "protectedapplications", role: "clusterroles", binding: "accessgrants", grant: "accessgrants"
};
const ENTITY_RESOURCE = {k8scluster: "managedclusters", storagecluster: "storageclusters", storagepool: "storagepools", backupop: "backups",
  replicationpolicy: "replicationpolicies", backuppolicy: "backuppolicies", drpolicy: "drpolicies", application: "protectedapplications", role: "clusterroles", binding: "accessgrants"};
const VERB_OF = {read: "get", create: "create", update: "update", delete: "delete", backup: "create", restoresource: "get"};

// ---- the store ---------------------------------------------------------------
const AC_STATE = {ready: false, user: null, groups: [], initials: "??", label: "", rules: {cluster: [], ns: {}}, incomplete: false,
  grants: [], scopes: null, ns: {clusters: {}, pools: {}, managed: {}, apps: {}, dr: "sb-dr-system"}, demoUsers: [], error: null};
const acListeners = new Set();
const acNotify = () => acListeners.forEach(f => f());

async function loadAccess() {
  try {
    const r = await api.accessSelf();
    Object.assign(AC_STATE, {ready: true, error: null, user: r.user, groups: r.groups || [], initials: r.initials || "??", label: r.label || r.user,
      rules: r.rules || {cluster: [], ns: {}}, incomplete: !!r.incomplete, grants: r.grants || [], scopes: r.scopes || null,
      ns: (r.scopes && r.scopes.ns) || AC_STATE.ns, demoUsers: r.demo_users || []});
  } catch (e) {
    Object.assign(AC_STATE, {ready: true, error: e, user: null, rules: {cluster: [], ns: {}}, grants: [], scopes: null});
  }
  acNotify();
}

// ---- object -> namespaces ------------------------------------------------------
const clusterIdsOf = o => [...new Set([o.kind === "cluster" ? o.id : null, o.clusterId, o.sourceClusterId, o.targetClusterId, ...(o.clusterIds || []), ...(o.involvedClusterIds || [])].filter(Boolean))];
function nsOf(entity, o, op) {
  const N = AC_STATE.ns;
  if (!o) return entity === "k8scluster" || entity === "binding" ? ["*"] : [];
  if (entity === "k8scluster") {
    if (op === "create") return ["*"];                                   // create nodepoolallocations is cluster-scoped
    const ids = o.kind === "k8sc" ? [o.id] : (o.k8sClusterIds || (o.k8sClusterId ? [o.k8sClusterId] : []));
    return ids.map(id => N.managed[id]).filter(Boolean);
  }
  if (entity === "storagepool" || entity === "backupop") {
    const pid = o.kind === "pool" ? o.id : o.poolId;
    if (pid && N.pools[pid]) return [N.pools[pid]];
    return clusterIdsOf(o).map(id => N.clusters[id]).filter(Boolean);   // pool create: the cluster namespace
  }
  if (entity === "storagecluster" || entity === "backuppolicy") return clusterIdsOf(o).map(id => N.clusters[id]).filter(Boolean);
  if (entity === "replicationpolicy" || entity === "drpolicy") return [N.dr];
  if (entity === "application") {
    const aid = o.kind === "protectedapp" ? o.id : o.appId;
    if (aid && N.apps[aid]) return [N.apps[aid]];
    return clusterIdsOf(o).map(id => N.clusters[id]).filter(Boolean);   // app create: source cluster
  }
  if (entity === "binding") return o.namespaces || (o.scope ? scopeNamespaces(o.scope) : ["*"]);
  return [];
}
function scopeNamespaces(sc) {
  const N = AC_STATE.ns;
  return sc.kind === "cluster-scope" ? ["*"] : sc.kind === "managed-cluster" ? [N.managed[sc.id]] : sc.kind === "storage-cluster" ? [N.clusters[sc.id]]
    : sc.kind === "storage-pool" ? [N.pools[sc.id]] : sc.kind === "dr-pair" ? [N.dr] : sc.kind === "application" ? [N.apps[sc.id]] : [];
}

// ---- rule evaluation (SSRR, local) --------------------------------------------
const ruleAllows = (r, verb, resource, group, name) => (r.apiGroups.includes(group || "simplyblock.io") || r.apiGroups.includes("*"))
  && (r.resources.includes(resource) || r.resources.includes("*")) && (r.verbs.includes(verb) || r.verbs.includes("*"))
  && (!r.resourceNames || !name || r.resourceNames.includes(name));
function allowedIn(ns, verb, resource, group, name) {
  if (AC_STATE.rules.cluster.some(r => ruleAllows(r, verb, resource, group, name))) return true;
  if (!ns || ns === "*") return false;
  return (AC_STATE.rules.ns[ns] || []).some(r => ruleAllows(r, verb, resource, group, name));
}
const anyNs = (verb, resource, group, name) => allowedIn("*", verb, resource, group, name) || Object.keys(AC_STATE.rules.ns).some(n => allowedIn(n, verb, resource, group, name));

// what (verb, resource) does (op, entity) become?
function target(op, entity, obj) {
  if (entity === "application" && (op === "failover" || op === "failback")) return {verb: "create", resource: "applicationfailovers"};
  if (entity === "application" && op === "fence") return {verb: "update", resource: "drclusters", nsOverride: [AC_STATE.ns.dr]};
  if (entity === "k8scluster" && op === "create") return {verb: "create", resource: "nodepoolallocations"};
  const resource = obj && KIND_RESOURCE[obj.kind] && KIND_ENTITY[obj.kind] === entity ? KIND_RESOURCE[obj.kind] : ENTITY_RESOURCE[entity] || entity;
  return {verb: VERB_OF[op] || op, resource};
}
// Is `op` on `entity` permitted for `obj`? Cross-cluster policies must pass on
// every namespace they touch. Returns true when the rules review is incomplete.
function acCan(op, entity, obj) {
  if (!AC_STATE.ready) return false;
  if (AC_STATE.incomplete) return true;
  const t = target(op, entity, obj);
  const nss = t.nsOverride || nsOf(entity, obj, op);
  if (!nss.length) return allowedIn("*", t.verb, t.resource);
  return nss.every(ns => allowedIn(ns, t.verb, t.resource));
}
// why not? — the tooltip text for a disabled control (§5.5)
function acWhy(op, entity, obj) {
  const t = target(op, entity, obj);
  const nss = t.nsOverride || nsOf(entity, obj, op);
  const missing = nss.filter(ns => !allowedIn(ns, t.verb, t.resource));
  return `Needs ${t.verb} on ${t.resource}${missing.length ? " in " + missing.map(n => n === "*" ? "cluster scope" : n).join(", ") : nss.length ? "" : " (cluster scope)"}`;
}
const canAnywhere = (op, entity) => AC_STATE.ready && (AC_STATE.incomplete || anyNs(target(op, entity, null).verb, target(op, entity, null).resource));
// scope discovery (§5.2): scopes the server says are visible; other kinds are the server's job
const SCOPE_KINDS = {cluster: "clusters", k8sc: "managed", pool: "pools", protectedapp: "apps"};
function canRead(o) {
  if (!AC_STATE.ready) return false;
  const coll = SCOPE_KINDS[o.kind];
  if (coll && AC_STATE.scopes) { const e = AC_STATE.scopes[coll].find(x => x.id === o.id); if (e) return !!e.visible; }
  return acCan("read", KIND_ENTITY[o.kind] || "storagecluster", o);
}
const canCreateIn = (kind, parent) => acCan("create", KIND_ENTITY[kind] || "storagecluster", parent);
const whyCreateIn = (kind, parent) => acWhy("create", KIND_ENTITY[kind] || "storagecluster", parent);
// restore = get backups in the source pool + create volumes in the target pool
const canRestore = (backup, targetPool) => acCan("read", "backupop", backup) && (!targetPool || allowedIn(AC_STATE.ns.pools[targetPool.id] || "", "create", "volumes"));
// §4.3 on the client: may the caller bind `role` in `ns`? (server decides; this only disables the option)
function mayBind(role, ns) {
  if (!allowedIn(ns, "create", "accessgrants")) return false;
  if (allowedIn(ns, "bind", "clusterroles", "rbac.authorization.k8s.io", role.name)) return true;
  return role.rules.length > 0 && role.rules.every(r => r.resources.every(res => r.verbs.every(v => allowedIn(ns, v, res, r.apiGroups[0]))));
}
const rulesIn = ns => [...AC_STATE.rules.cluster, ...(AC_STATE.rules.ns[ns] || [])];
const grantsFor = o => { const nss = new Set(["*", ...Object.keys(KIND_ENTITY).includes(o.kind) ? nsOf(KIND_ENTITY[o.kind], o) : []]); return AC_STATE.grants.filter(g => g.namespaces.some(n => nss.has(n))); };

const access = {can: acCan, why: acWhy, canAnywhere, canRead, canCreateIn, whyCreateIn, canRestore, mayBind, nsOf, scopeNamespaces, rulesIn, grantsFor, allowedIn, load: loadAccess, state: AC_STATE, KIND_ENTITY, KIND_RESOURCE};
window.access = access;

function useAccess() {
  const [, tick] = useState(0);
  useEffect(() => { const f = () => tick(t => t + 1); acListeners.add(f); return () => acListeners.delete(f); }, []);
  return access;
}
// <Can op="update" entity="storagepool" obj={pool}>…</Can> — hides; prefer disabled controls with access.why()
const Can = ({op, entity, obj, children, fallback}) => { useAccess(); return acCan(op, entity, obj) ? children : (fallback || null); };

// ---- identity in the top bar ------------------------------------------------
function IdentityMenu({here}) {
  const a = useAccess();
  const [open, setOpen] = useState(false);
  const ref = useRef(null);
  useEffect(() => {
    if (!open) return;
    const h = e => { if (ref.current && !ref.current.contains(e.target)) setOpen(false); };
    document.addEventListener("mousedown", h); return () => document.removeEventListener("mousedown", h);
  }, [open]);
  const s = a.state;
  const nss = here ? nsOf(KIND_ENTITY[here.kind] || "storagecluster", here) : [];
  const applying = here ? a.grantsFor(here) : s.grants.filter(g => g.namespaces.includes("*"));
  return (
    <div ref={ref} style={{position: "relative"}}>
      <button className="avatar" title={s.user || "not signed in"} onClick={() => setOpen(o => !o)} style={{border: 0, cursor: "pointer"}}>{s.initials}</button>
      {open && <div className="idmenu">
        <div className="idhead">
          <b>{s.label || s.user || "No identity"}</b>
          <span className="mono">{s.user}</span>
          {s.error && <span style={{color: "var(--bad)", fontSize: 11.5}}>{s.error.message} — nothing is permitted without an identity.</span>}
          {s.incomplete && <span style={{color: "var(--warn)", fontSize: 11.5}}>The rules review is incomplete — controls are shown optimistically and may fail with 403.</span>}
        </div>
        {!!s.groups.length && <div className="idsec"><h4>Groups</h4><div className="labels">{s.groups.map(g => <span key={g} className="lab mono">{g}</span>)}</div></div>}
        <div className="idsec"><h4>{here ? `Roles in ${nss.map(n => n === "*" ? "cluster scope" : n).join(", ") || "this scope"}` : "Roles at cluster scope"}</h4>
          {applying.length ? applying.map(g => <div key={g.uuid} className="idrole">
            <b>{g.role}</b><span className="mono">{g.namespaces.map(n => n === "*" ? "cluster scope" : n).join(", ")}</span>
            <span style={{color: "var(--dim2)"}}>via {g.subject.kind.toLowerCase()} {g.subject.name.replace(/^oidc:/, "")}</span>
          </div>) : <div style={{color: "var(--dim)", fontSize: 12}}>{here ? "No RoleBinding reaches this namespace — if you can see the object, you are reading it through a wider scope." : "No cluster-scoped binding."}</div>}
        </div>
        {!!s.demoUsers.length && <div className="idsec"><h4>View as <span style={{color: "var(--dim2)", fontWeight: 400}}>fixture backend — stands in for impersonation</span></h4>
          {s.demoUsers.map(u => <button key={u.name} className={"idpick" + (u.name === s.user ? " on" : "")} onClick={() => {
            localStorage.setItem("sb.viewas", u.name); setOpen(false); a.load().then(() => window.dispatchEvent(new Event("sb:access")));
          }}><span className="avatar sm">{u.initials}</span><span>{u.label}</span></button>)}
        </div>}
      </div>}
    </div>
  );
}

Object.assign(window, {access, useAccess, Can, IdentityMenu, KIND_ENTITY, KIND_RESOURCE, ENTITY_LABEL, ENTITY_COVERS, ENTITY_OPS, OPS_LABEL, ACCESS_OPS_LABEL: OPS_LABEL});

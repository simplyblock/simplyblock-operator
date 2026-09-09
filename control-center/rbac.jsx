// ---------------------------------------------------------------------------
// AC_STATE CONTROL — the client side of RBAC-DESIGN.md.
//
// One question, asked everywhere: access.can(op, entity, obj). It resolves the
// object's scope chain (global ⊇ k8scluster ⊇ storagecluster ⊇ pool | app) and
// answers from the caller's effective bindings. What the caller cannot read is
// not rendered; what they cannot do is not offered.
// ---------------------------------------------------------------------------
const OPS_LABEL = {create: "C", read: "R", update: "U", delete: "D", restoresource: "src", backup: "bak", restore: "rst", failover: "f/o", failback: "f/b", fence: "fence"};
const ENTITY_LABEL = {
  k8scluster: "Kubernetes cluster", storagecluster: "Storage cluster", storagepool: "Storage pool",
  backupop: "Backup / restore", replicationpolicy: "Replication policy", backuppolicy: "Backup policy",
  drpolicy: "DR policy", application: "Application", role: "Role", binding: "Binding"
};
const ENTITY_COVERS = {
  k8scluster: "worker nodes, discovery, deployment documents",
  storagecluster: "hosts, nodes, devices, tasks, logs, migrations, S3 target, KMS endpoint",
  storagepool: "volumes, snapshots, clones, storage classes, PVCs, backups, buckets, KEKs",
  backupop: "per-volume backup and restore operations",
  replicationpolicy: "pairs, policies, slots, replication ops",
  backuppolicy: "schedules and retention", drpolicy: "protection plans, methods, migration paths",
  application: "protected applications, recipes, DRPCs", role: "AccessRole", binding: "AccessBinding"
};
const ENTITY_OPS = window.ACCESS_ENTITY_OPS || {
  k8scluster: ["create", "read", "update", "delete"], storagecluster: ["create", "read", "update", "delete"],
  storagepool: ["create", "read", "update", "delete", "restoresource"], backupop: ["backup", "restore"],
  replicationpolicy: ["create", "read", "update", "delete"], backuppolicy: ["create", "read", "update", "delete"],
  drpolicy: ["create", "read", "update", "delete"], application: ["create", "read", "update", "delete", "failover", "failback", "fence"],
  role: ["create", "read", "update", "delete"], binding: ["create", "read", "update", "delete"]
};

// UI kind -> the main entity whose rights govern it
const KIND_ENTITY = {
  cluster: "storagecluster", host: "storagecluster", node: "storagecluster", device: "storagecluster",
  task: "storagecluster", migration: "storagecluster", deployconfig: "k8scluster", k8sc: "k8scluster", zone: "k8scluster",
  pool: "storagepool", volume: "storagepool", snapshot: "storagepool", backup: "storagepool", pvc: "storagepool",
  storageclass: "storagepool", bucket: "storagepool", cgroup: "storagepool", cgsnapshot: "storagepool",
  policy: "backuppolicy", pair: "replicationpolicy", rpolicy: "replicationpolicy", slot: "replicationpolicy", replop: "replicationpolicy",
  plan: "drpolicy", method: "drpolicy", site: "drpolicy", mpath: "drpolicy", appgroup: "drpolicy",
  protectedapp: "application", role: "role", binding: "binding"
};

// ---- the store ---------------------------------------------------------------
const AC_STATE = {ready: false, user: null, groups: [], initials: "??", label: "", bindings: [], demoUsers: [], error: null};
const acListeners = new Set();
const acNotify = () => acListeners.forEach(f => f());

async function loadAccess() {
  try {
    const r = await api.accessSelf();
    Object.assign(AC_STATE, {ready: true, error: null, user: r.user, groups: r.groups || [], initials: r.initials || "??",
      label: r.label || r.user, bindings: r.bindings || [], demoUsers: r.demo_users || []});
  } catch (e) {
    // no identity = nothing is permitted. Render the shell, offer nothing.
    Object.assign(AC_STATE, {ready: true, error: e, user: null, bindings: []});
  }
  acNotify();
}

// Scope chain for an object: every scope key that contains it.
function chainOf(o) {
  const keys = ["global"];
  if (!o) return keys;
  const k8s = o.kind === "k8sc" ? [o.id] : (o.k8sClusterIds || (o.k8sClusterId ? [o.k8sClusterId] : []));
  k8s.forEach(id => keys.push("k8scluster:" + id));
  const clusterIds = new Set([o.kind === "cluster" ? o.id : null, o.clusterId, o.sourceClusterId, o.targetClusterId,
    ...(o.clusterIds || []), ...(o.involvedClusterIds || [])].filter(Boolean));
  // a cluster's k8s parents are the k8s scopes above it
  clusterIds.forEach(cid => {
    keys.push("storagecluster:" + cid);
    const c = REG[cid];
    if (c && c.k8sClusterIds) c.k8sClusterIds.forEach(id => keys.push("k8scluster:" + id));
  });
  if (o.kind === "pool") keys.push("storagepool:" + o.id);
  if (o.poolId) keys.push("storagepool:" + o.poolId);
  if (o.kind === "protectedapp") keys.push("application:" + o.id);
  if (o.appId) keys.push("application:" + o.appId);
  return [...new Set(keys)];
}
const scopeKey = s => s.kind === "global" ? "global" : `${s.kind}:${s.id}`;
const acGrants = (b, entity, op) => (b.role_rights || []).some(r => r.entity === entity && r.ops.includes(op));

// Is `op` on `entity` permitted for `obj`? Policies are cross-cluster: every
// cluster they touch must pass, so they are checked cluster by cluster.
function acCan(op, entity, obj) {
  if (!AC_STATE.ready) return false;
  const bs = AC_STATE.bindings.filter(b => acGrants(b, entity, op));
  if (!bs.length) return false;
  if (isCrossCluster(obj)) {
    return acInvolved(obj).every(cid => bs.some(b => chainOf({kind: "cluster", id: cid}).includes(scopeKey(b.scope))));
  }
  const chain = chainOf(obj);
  return bs.some(b => chain.includes(scopeKey(b.scope)));
}
const POLICY_KINDS = new Set(["pair", "rpolicy", "slot", "replop", "policy", "plan", "method", "mpath", "appgroup", "site"]);
const isCrossCluster = o => !!o && POLICY_KINDS.has(o.kind) && acInvolved(o).length > 1;
const acInvolved = o => [...new Set([o.clusterId, o.sourceClusterId, o.targetClusterId, ...(o.clusterIds || []), ...(o.involvedClusterIds || [])].filter(Boolean))];

// Anywhere at all? Used to decide whether a layer or section is worth drawing.
const canAnywhere = (op, entity) => AC_STATE.ready && AC_STATE.bindings.some(b => acGrants(b, entity, op));
const canRead = o => acCan("read", KIND_ENTITY[o.kind] || "storagecluster", o);
const canCreateIn = (kind, parent) => acCan("create", KIND_ENTITY[kind] || "storagecluster", parent);
// restore: three checks, per RBAC-DESIGN.md §11
const canRestore = (backup, targetPool) => acCan("restore", "backupop", backup) && acCan("restoresource", "storagepool", backup)
  && (!targetPool || acCan("create", "storagepool", targetPool));

// the bindings that apply to an object — for the identity popover
const acBindingsFor = o => { const chain = chainOf(o); return AC_STATE.bindings.filter(b => chain.includes(scopeKey(b.scope))); };

const access = {can: acCan, canAnywhere, canRead, canCreateIn, canRestore, chainOf, bindingsFor: acBindingsFor, load: loadAccess, state: AC_STATE, KIND_ENTITY};
window.access = access;

function useAccess() {
  const [, tick] = useState(0);
  useEffect(() => { const f = () => tick(t => t + 1); acListeners.add(f); return () => acListeners.delete(f); }, []);
  return access;
}
// <Can op="update" entity="storagepool" obj={pool}>…</Can>
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
  const applying = here ? a.bindingsFor(here) : s.bindings.filter(b => b.scope.kind === "global");
  return (
    <div ref={ref} style={{position: "relative"}}>
      <button className="avatar" title={s.user || "not signed in"} onClick={() => setOpen(o => !o)} style={{border: 0, cursor: "pointer"}}>{s.initials}</button>
      {open && <div className="idmenu">
        <div className="idhead">
          <b>{s.label || s.user || "No identity"}</b>
          <span className="mono">{s.user}</span>
          {s.error && <span style={{color: "var(--bad)", fontSize: 11.5}}>{s.error.message} — nothing is permitted without an identity.</span>}
        </div>
        {!!s.groups.length && <div className="idsec"><h4>Groups</h4><div className="labels">{s.groups.map(g => <span key={g} className="lab mono">{g}</span>)}</div></div>}
        <div className="idsec"><h4>{here ? "Roles that apply here" : "Roles at global scope"}</h4>
          {applying.length ? applying.map(b => <div key={b.uuid} className="idrole">
            <b>{b.role}</b><span className="mono">{b.scope.kind === "global" ? "global" : `${b.scope.kind} · ${b.scope.name || b.scope.id}`}</span>
            <span style={{color: "var(--dim2)"}}>via {b.subject.kind.toLowerCase()} {b.subject.name.replace(/^oidc:/, "")}</span>
          </div>) : <div style={{color: "var(--dim)", fontSize: 12}}>{here ? "No binding reaches this object — it is read-only for you if you can see it at all." : "No global bindings."}</div>}
        </div>
        {!!s.demoUsers.length && <div className="idsec"><h4>View as <span style={{color: "var(--dim2)", fontWeight: 400}}>fixture backend only</span></h4>
          {s.demoUsers.map(u => <button key={u.name} className={"idpick" + (u.name === s.user ? " on" : "")} onClick={() => {
            localStorage.setItem("sb.viewas", u.name); setOpen(false); a.load().then(() => window.dispatchEvent(new Event("sb:access")));
          }}><span className="avatar sm">{u.initials}</span><span>{u.label}</span></button>)}
        </div>}
      </div>}
    </div>
  );
}

Object.assign(window, {access, useAccess, Can, IdentityMenu, KIND_ENTITY, ENTITY_LABEL, ENTITY_COVERS, ENTITY_OPS, OPS_LABEL, ACCESS_OPS_LABEL: OPS_LABEL});

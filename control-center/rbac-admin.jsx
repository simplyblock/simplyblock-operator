// ---------------------------------------------------------------------------
// ACCESS ADMINISTRATION — roles and bindings, under Control plane → Access.
// A global admin authors roles and binds anywhere; a cluster admin binds inside
// their own clusters and sees only those bindings. Everything a caller cannot
// do is absent, not disabled — the same rule as the rest of the console.
// ---------------------------------------------------------------------------
const RBA_ENTITIES = Object.keys(ENTITY_OPS);
const RBA_ALL_OPS = ["create", "read", "update", "delete", "restoresource", "backup", "restore", "failover", "failback", "fence"];
const scopeLabel = s => s.kind === "global" ? "global" : `${SCOPE_KIND_LABEL[s.kind] || s.kind} · ${s.name || s.id}`;
const SCOPE_KIND_LABEL = {k8scluster: "Kubernetes", storagecluster: "cluster", storagepool: "pool", application: "application"};

// ---- the rights matrix, read-only or editable ------------------------------
function RightsMatrix({rights, onChange}) {
  const has = (e, op) => (rights.find(r => r.entity === e) || {ops: []}).ops.includes(op);
  const toggle = (e, op) => {
    const cur = rights.find(r => r.entity === e);
    const ops = cur ? (cur.ops.includes(op) ? cur.ops.filter(o => o !== op) : [...cur.ops, op]) : [op];
    onChange([...rights.filter(r => r.entity !== e), ...(ops.length ? [{entity: e, ops}] : [])]);
  };
  return (
    <table className="dt rights">
      <thead><tr><th>Entity</th>{RBA_ALL_OPS.map(op => <th key={op} title={op}>{OPS_LABEL[op]}</th>)}</tr></thead>
      <tbody>{RBA_ENTITIES.map(e => (
        <tr key={e}>
          <td><b>{ENTITY_LABEL[e]}</b><div className="sub">{ENTITY_COVERS[e]}</div></td>
          {RBA_ALL_OPS.map(op => {
            const valid = ENTITY_OPS[e].includes(op);
            if (!valid) return <td key={op} className="na"></td>;
            const on = has(e, op);
            return <td key={op} className={on ? "on" : ""}>
              {onChange ? <button className={"rbit" + (on ? " on" : "")} onClick={() => toggle(e, op)} title={`${op} ${ENTITY_LABEL[e]}`}>{on ? <Icon n="check" s={11} /> : null}</button>
                : <span className={"rbit" + (on ? " on" : "")}>{on ? <Icon n="check" s={11} /> : null}</span>}
            </td>;
          })}
        </tr>
      ))}</tbody>
    </table>
  );
}

// ---- roles -------------------------------------------------------------------
function RolesPanel() {
  const a = useAccess();
  const [rev, setRev] = useState(0);
  const {data, loading, error, reload} = useResource("access.roles|" + rev, () => api.accessRoles(), 15000);
  const [editing, setEditing] = useState(null);   // null | {name, description, rights, uuid?}
  const [open, setOpen] = useState(null);
  const roles = data || [];
  const mayCreate = a.canAnywhere("create", "role");
  if (error) return <ErrorState error={error} onRetry={reload} kind="roles" />;
  return (
    <div>
      <div className="sech"><h2>Roles</h2><span className="ln"></span><span className="count">{roles.length}</span>
        {mayCreate && !editing && <button className="btn primary" onClick={() => setEditing({name: "", description: "", rights: []})}><Icon n="plus" s={12} />New role</button>}</div>
      <p className="mdesc">A role is a named set of rights. Pre-defined roles are immutable and generate the matching Kubernetes ClusterRoles; custom roles are built from the same matrix. A right on a main entity covers every object beneath it — <b>C R U D</b>, plus <b>src</b> (use a pool's backups as a restore source), <b>bak/rst</b> (per-volume backup and restore), and the DR verbs.</p>
      {editing && <RoleEditor role={editing} onDone={() => { setEditing(null); setRev(r => r + 1); }} onCancel={() => setEditing(null)} />}
      {loading && !data ? <div className="lmsg">Loading roles…</div> : (
        <div className="rolelist">{roles.map(r => {
          const isOpen = open === r.uuid;
          const mayEdit = !r.builtin && a.canAnywhere("update", "role");
          const mayDelete = !r.builtin && a.canAnywhere("delete", "role");
          return (
            <div key={r.uuid} className={"card role" + (isOpen ? " open" : "")}>
              <button className="rolehead" onClick={() => setOpen(isOpen ? null : r.uuid)}>
                <Icon n="shield" s={14} /><b className="mono">{r.name}</b>
                {r.builtin ? <span className="badge">pre-defined</span> : <span className="badge" style={{color: "var(--ro)"}}>custom</span>}
                <span className="sub" style={{flex: 1, textAlign: "left"}}>{r.description}</span>
                <span className="count">{r.rights.reduce((n, x) => n + x.ops.length, 0)} rights</span>
                <Icon n={isOpen ? "chevron-up" : "chevron-down"} s={12} />
              </button>
              {isOpen && <div className="bd" style={{padding: 0}}>
                <RightsMatrix rights={r.rights} />
                {(mayEdit || mayDelete) && <div className="roleacts">
                  {mayEdit && <button className="chip" onClick={() => setEditing({uuid: r.uuid, name: r.name, description: r.description, rights: r.rights})}>Edit rights</button>}
                  {mayDelete && <button className="chip" style={{color: "var(--bad)"}} onClick={() => window.__ui.dialog({
                    title: `Delete role ${r.name}?`, danger: true, confirm: "Delete role",
                    desc: "Refused while any binding references it. Pre-defined roles cannot be deleted at all.",
                    run: () => api.accessRoleDelete(r.uuid).then(() => setRev(x => x + 1))
                  }, {kind: "role", id: r.uuid})}>Delete</button>}
                </div>}
                {r.builtin && <div className="roleacts" style={{color: "var(--dim2)", fontSize: 11.5}}>Pre-defined roles are immutable. To vary one, create a custom role from this matrix.</div>}
              </div>}
            </div>
          );
        })}</div>
      )}
    </div>
  );
}

function RoleEditor({role, onDone, onCancel}) {
  const [r, setR] = useState(role);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const n = r.rights.reduce((x, y) => x + y.ops.length, 0);
  const save = async () => {
    setBusy(true); setErr(null);
    try {
      if (r.uuid) await api.accessRoleUpdate(r.uuid, {description: r.description, rights: r.rights});
      else await api.accessRoleCreate({name: r.name, description: r.description, rights: r.rights});
      window.__toast && window.__toast(r.uuid ? `Role ${r.name} updated` : `Role ${r.name} created`);
      onDone();
    } catch (e) { setErr(e.message); setBusy(false); }
  };
  return (
    <div className="card editor"><h3>{r.uuid ? `Edit ${r.name}` : "New role"}</h3><div className="bd">
      <div className="frow">
        <label className="fl">Name<input className="finput mono" disabled={!!r.uuid} value={r.name} placeholder="e.g. eu-oncall" onChange={e => setR({...r, name: e.target.value})} />
          {!r.uuid && <span className="sub">3–40 lowercase letters, digits, dashes. Immutable once created.</span>}</label>
        <label className="fl" style={{flex: 2}}>Description<input className="finput" value={r.description} placeholder="Who is this for, and what may they do?" onChange={e => setR({...r, description: e.target.value})} /></label>
      </div>
      <RightsMatrix rights={r.rights} onChange={rights => setR({...r, rights})} />
      {err && <div className="ferr"><Icon n="alert" s={12} />{err}</div>}
      <div className="facts">
        <span className="count">{n} right{n === 1 ? "" : "s"}</span>
        <span className="spacer"></span>
        <button className="btn" onClick={onCancel} disabled={busy}>Cancel</button>
        <button className="btn primary" onClick={save} disabled={busy || !n || (!r.uuid && !r.name)}>{busy ? "Saving…" : r.uuid ? "Save rights" : "Create role"}</button>
      </div>
    </div></div>
  );
}

// ---- bindings ----------------------------------------------------------------
function BindingsPanel({nav}) {
  const a = useAccess();
  const [rev, setRev] = useState(0);
  const {data, loading, error, reload} = useResource("access.bindings|" + rev, () => api.accessBindings(), 10000);
  const roles = useResource("access.roles.list", () => api.accessRoles(), 30000);
  const [adding, setAdding] = useState(false);
  const bs = data || [];
  const mayBind = a.canAnywhere("create", "binding");
  if (error) return <ErrorState error={error} onRetry={reload} kind="bindings" />;
  const bySubject = {};
  bs.forEach(b => { (bySubject[b.subject.kind + " " + b.subject.name] = bySubject[b.subject.kind + " " + b.subject.name] || []).push(b); });
  return (
    <div>
      <div className="sech"><h2>Bindings</h2><span className="ln"></span><span className="count">{bs.length}</span>
        {mayBind && !adding && <button className="btn primary" onClick={() => setAdding(true)}><Icon n="plus" s={12} />Bind a user or group</button>}</div>
      <p className="mdesc">A binding gives a Kubernetes <b>User</b> or <b>Group</b> — exactly as the API server names them — a role at a scope. The scope contains everything beneath it: a role at a storage cluster reaches its pools and its applications. You see the bindings in scopes you hold <span className="mono">binding.read</span> on, and may bind only where you hold <span className="mono">binding.create</span>.</p>
      {adding && <BindingForm roles={roles.data || []} onDone={() => { setAdding(false); setRev(r => r + 1); }} onCancel={() => setAdding(false)} />}
      {loading && !data ? <div className="lmsg">Loading bindings…</div> : !bs.length ? <div className="card"><div className="lmsg">No binding is visible in the scopes you hold.</div></div> : (
        <div className="card"><div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr><th>Subject</th><th>Role</th><th>Scope</th><th>Bound by</th><th>Since</th><th></th></tr></thead>
          <tbody>{Object.entries(bySubject).map(([k, list]) => list.map((b, i) => (
            <tr key={b.uuid}>
              <td>{i === 0 && <span className="lab"><Icon n={b.subject.kind === "Group" ? "users" : "user"} s={10} /><span className="mono">{b.subject.name}</span></span>}</td>
              <td><b className="mono">{b.role}</b></td>
              <td><ScopeRef scope={b.scope} nav={nav} /></td>
              <td className="mono" style={{color: "var(--dim)"}}>{(b.bound_by || "").replace(/^oidc:/, "")}</td>
              <td className="mono">{relAge(b.created_at)}</td>
              <td style={{textAlign: "right"}}>{a.can("delete", "binding", scopeObj(b.scope)) && <button className="chip" onClick={() => window.__ui.dialog({
                title: `Remove ${b.subject.name.replace(/^oidc:/, "")} from ${b.role}?`, danger: true, confirm: "Remove binding",
                desc: `Takes effect on their next request. Scope: ${scopeLabel(b.scope)}.`,
                run: () => api.accessBindingDelete(b.uuid).then(() => setRev(x => x + 1))
              }, {kind: "binding", id: b.uuid})}>Remove</button>}</td>
            </tr>
          )))}</tbody></table>
        </div></div>
      )}
    </div>
  );
}

// a scope as an object can() understands
const scopeObj = s => s.kind === "global" ? null
  : s.kind === "storagecluster" ? {kind: "cluster", id: s.id}
  : s.kind === "storagepool" ? {kind: "pool", id: s.id, clusterId: (REG[s.id] || {}).clusterId}
  : s.kind === "application" ? {kind: "protectedapp", id: s.id, sourceClusterId: (REG[s.id] || {}).sourceClusterId}
  : {kind: "k8sc", id: s.id};

const ScopeRef = ({scope, nav}) => scope.kind === "global" ? <span className="lab">global</span>
  : <button className="lab link" onClick={() => {
      if (scope.kind === "storagecluster") nav.openCluster(scope.id);
      else if (scope.kind === "k8scluster") nav.openK8s(scope.id);
      else if (scope.kind === "storagepool") nav.openPool && nav.openPool(scope.id);
      else if (scope.kind === "application") nav.openApp && nav.openApp(scope.id);
    }}><Icon n={scope.kind === "k8scluster" ? "k8s" : scope.kind === "storagepool" ? "pool" : scope.kind === "application" ? "shield" : "cluster"} s={10} />{scopeLabel(scope)}</button>;

function BindingForm({roles, onDone, onCancel}) {
  const a = useAccess();
  const [f, setF] = useState({kind: "User", name: "", role: "viewer", scopeKind: "storagecluster", scopeId: ""});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const clusters = useResource("access.f.clusters", () => api.clusters(), 60000);
  const k8s = useResource("access.f.k8s", () => api.k8sClusters(), 60000);
  const apps = useResource("access.f.apps", () => api.protectedApps(), 60000);
  const pools = useResource("access.f.pools|" + (f.scopeKind === "storagepool" ? f.clusterForPool : ""), () => f.clusterForPool ? api.pools(f.clusterForPool) : Promise.resolve([]), 60000);
  // only scopes the caller may bind in
  const targets = f.scopeKind === "global" ? []
    : f.scopeKind === "storagecluster" ? (clusters.data || []).filter(c => a.can("create", "binding", c))
    : f.scopeKind === "k8scluster" ? (k8s.data || []).filter(k => a.can("create", "binding", k))
    : f.scopeKind === "application" ? (apps.data || []).filter(x => a.can("create", "binding", x))
    : (pools.data || []).filter(p => a.can("create", "binding", p));
  const canGlobal = a.can("create", "binding", null);
  const chosen = targets.find(t => t.id === f.scopeId);
  const role = roles.find(r => r.name === f.role);
  const save = async () => {
    setBusy(true); setErr(null);
    try {
      const scope = f.scopeKind === "global" ? {kind: "global"} : {kind: f.scopeKind, id: chosen.id, name: chosen.name};
      await api.accessBindingCreate({subject: {kind: f.kind, name: f.name.trim()}, role: f.role, scope});
      window.__toast && window.__toast(`${f.name.trim()} bound to ${f.role}`);
      onDone();
    } catch (e) { setErr(e.message); setBusy(false); }
  };
  return (
    <div className="card editor"><h3>Bind a user or group</h3><div className="bd">
      <div className="frow">
        <label className="fl">Subject
          <div className="seg">{["User", "Group"].map(k => <button key={k} className={f.kind === k ? "on" : ""} onClick={() => setF({...f, kind: k})}>{k}</button>)}</div></label>
        <label className="fl" style={{flex: 2}}>{f.kind} name<input className="finput mono" value={f.name} placeholder={f.kind === "User" ? "oidc:alice@corp.example" : "oidc:storage-admins"} onChange={e => setF({...f, name: e.target.value})} />
          <span className="sub">As the API server authenticates it — the OIDC prefix included.</span></label>
      </div>
      <div className="frow">
        <label className="fl">Role<select className="sel" value={f.role} onChange={e => setF({...f, role: e.target.value})}>
          {roles.map(r => <option key={r.name} value={r.name}>{r.name}</option>)}</select>
          {role && <span className="sub">{role.description}</span>}</label>
        <label className="fl">Scope<select className="sel" value={f.scopeKind} onChange={e => setF({...f, scopeKind: e.target.value, scopeId: "", clusterForPool: ""})}>
          {canGlobal && <option value="global">global</option>}
          <option value="k8scluster">a Kubernetes cluster</option>
          <option value="storagecluster">a storage cluster</option>
          <option value="storagepool">a storage pool</option>
          <option value="application">an application</option></select></label>
        {f.scopeKind === "storagepool" && <label className="fl">In cluster<select className="sel" value={f.clusterForPool || ""} onChange={e => setF({...f, clusterForPool: e.target.value, scopeId: ""})}>
          <option value="">—</option>{(clusters.data || []).filter(c => a.can("create", "binding", c)).map(c => <option key={c.id} value={c.id}>{c.name}</option>)}</select></label>}
        {f.scopeKind !== "global" && <label className="fl" style={{flex: 2}}>{SCOPE_KIND_LABEL[f.scopeKind]}<select className="sel" value={f.scopeId} onChange={e => setF({...f, scopeId: e.target.value})}>
          <option value="">—</option>{targets.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}</select>
          {!targets.length && <span className="sub" style={{color: "var(--warn)"}}>You hold binding.create in none of these.</span>}</label>}
      </div>
      {f.role === "global-admin" && f.scopeKind !== "global" && <div className="ferr"><Icon n="alert" s={12} />global-admin is only meaningful at global scope.</div>}
      {err && <div className="ferr"><Icon n="alert" s={12} />{err}</div>}
      <div className="facts"><span className="spacer"></span>
        <button className="btn" onClick={onCancel} disabled={busy}>Cancel</button>
        <button className="btn primary" onClick={save} disabled={busy || !f.name.trim() || (f.scopeKind !== "global" && !chosen)}>{busy ? "Binding…" : "Bind"}</button>
      </div>
    </div></div>
  );
}

function AccessView({nav}) {
  const a = useAccess();
  const [tab, setTab] = useState(a.canAnywhere("read", "binding") ? "bindings" : "roles");
  const tabs = [a.canAnywhere("read", "binding") && ["bindings", "Bindings"], a.canAnywhere("read", "role") && ["roles", "Roles"]].filter(Boolean);
  if (!tabs.length) return null;
  return (
    <div>
      <div className="tabs" style={{marginTop: 0}}>{tabs.map(t => <button key={t[0]} className={"tab" + (tab === t[0] ? " on" : "")} onClick={() => setTab(t[0])}>{t[1]}</button>)}</div>
      {tab === "bindings" && <BindingsPanel nav={nav} />}
      {tab === "roles" && <RolesPanel />}
    </div>
  );
}

Object.assign(window, {RightsMatrix, RolesPanel, BindingsPanel, AccessView, scopeLabel});

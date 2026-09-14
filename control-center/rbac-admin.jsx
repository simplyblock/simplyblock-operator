// ---------------------------------------------------------------------------
// ACCESS MANAGEMENT — Control plane → Access. §4.5 of the RBAC design:
// scope tree, per-scope grant list with a source column, add grant (disabled
// for roles the caller cannot bind, with the reason), effective access for a
// subject with a SubjectAccessReview spot check, and the §6 invariants.
// ---------------------------------------------------------------------------
const RBA_RBAC = "rbac.authorization.k8s.io";
const SCOPE_KIND_LABEL = {"cluster-scope": "cluster scope", "managed-cluster": "managed cluster", "storage-cluster": "storage cluster", "storage-pool": "pool", "dr-pair": "DR", application: "application"};
const SCOPE_ICON = {"cluster-scope": "shield", "managed-cluster": "k8s", "storage-cluster": "cluster", "storage-pool": "pool", "dr-pair": "link", application: "shield"};
const scopeLabel = s => s.kind === "cluster-scope" ? "cluster scope" : `${SCOPE_KIND_LABEL[s.kind] || s.kind} · ${s.name || s.id}`;
const nsLabel = n => n === "*" ? "cluster scope" : n;
const fmtExpiry = e => !e ? "—" : Date.parse(e) < Date.now() ? "expired " + relAge(e) : "in " + Math.ceil((Date.parse(e) - Date.now()) / 86400e3) + " d";

// ---- rules of one aggregated role, read-only ----------------------------------
function RulesTable({rules}) {
  return (
    <table className="dt rules"><thead><tr><th>API group</th><th>Resources</th><th>Verbs</th><th>Names</th></tr></thead>
      <tbody>{rules.map((r, i) => <tr key={i}>
        <td className="mono">{r.apiGroups.join(", ")}</td>
        <td><div className="labels">{r.resources.map(x => <span key={x} className="lab mono">{x}</span>)}</div></td>
        <td className="mono" style={{color: r.verbs.length > 3 ? "var(--text)" : "var(--dim)"}}>{r.verbs.join(" ")}</td>
        <td className="mono" style={{fontSize: 10.5, color: "var(--dim)"}}>{(r.resourceNames || []).join(", ") || "—"}</td>
      </tr>)}</tbody></table>
  );
}

function RolesPanel() {
  const {data, loading, error, reload} = useResource("access.roles", () => api.accessRoles(), 60000);
  const [open, setOpen] = useState(null);
  if (error) return <ErrorState error={error} onRetry={reload} kind="roles" />;
  const roles = data || [];
  return (
    <div>
      <div className="sech"><h2>Roles</h2><span className="ln"></span><span className="count">{roles.length}</span></div>
      <p className="mdesc">The eight <span className="mono">sb:*</span> ClusterRoles are <b>aggregated</b>: the chart owns the name, the controller fills the rules from every ClusterRole labelled <span className="mono">simplyblock.io/aggregate-to-&lt;role&gt;: "true"</span>. Adding a CRD never edits a role, and there is no role editor here — a rule set that is not in the chart is added with kubectl and shows up on the next load. Actions are resources: failover is <span className="mono">create</span> on <span className="mono">applicationfailovers</span>, not a field on the policy.</p>
      {loading && !data ? <div className="lmsg">Loading roles…</div> : <div className="rolelist">{roles.map(r => {
        const isOpen = open === r.name;
        return <div key={r.name} className={"card role" + (isOpen ? " open" : "")}>
          <button className="rolehead" onClick={() => setOpen(isOpen ? null : r.name)}>
            <Icon n="shield" s={14} /><b className="mono">{r.name}</b>
            <span className="badge">bound at {r.boundAt}</span>
            <span className="sub" style={{flex: 1, textAlign: "left"}}>{r.description}</span>
            <span className="count">{r.rules.length} rule{r.rules.length === 1 ? "" : "s"}</span>
            <Icon n={isOpen ? "chevron-up" : "chevron-down"} s={12} />
          </button>
          {isOpen && <div className="bd" style={{padding: 0}}>
            <RulesTable rules={r.rules} />
            <div className="roleacts" style={{color: "var(--dim2)", fontSize: 11.5}}>Aggregated from {r.parts.map(p => <span key={p.name} className="lab mono" style={{marginLeft: 6}}>{p.name}</span>)}</div>
          </div>}
        </div>;
      })}</div>}
    </div>
  );
}

// ---- scope tree (§4.5) ---------------------------------------------------------
function ScopeTree({scopes, sel, onSel}) {
  const Node = ({k, label, icon, ns, depth, count, children, muted}) => {
    const on = sel === k;
    return <div>
      <button className={"stnode" + (on ? " on" : "") + (muted ? " muted" : "")} style={{paddingLeft: 8 + depth * 14}} onClick={() => onSel(on ? null : k)}>
        <Icon n={icon} s={12} /><span className="stl">{label}</span>{ns && <span className="mono stns">{ns}</span>}{count !== undefined && <span className="count">{count}</span>}
      </button>{children}</div>;
  };
  const counts = scopes.counts || {};
  return (
    <div className="scopetree">
      <Node k="*" label="Cluster scope" icon="shield" depth={0} count={counts["*"]} />
      {scopes.managed.filter(m => m.visible).map(m => <Node key={m.id} k={m.namespace} label={m.name} icon="k8s" ns={m.namespace} depth={0} count={counts[m.namespace]}>
        {scopes.clusters.filter(c => m.clusters.includes(c.id) && c.visible).map(c => <Node key={c.id} k={c.namespace} label={c.name} icon="cluster" ns={c.namespace} depth={1} count={counts[c.namespace]} muted={!c.full}>
          {c.pools.filter(p => p.visible && p.isolated).map(p => <Node key={p.id} k={p.namespace} label={p.name} icon="pool" ns={p.namespace} depth={2} count={counts[p.namespace]} />)}
          {c.sharedPools > 0 && <div className="stshared" style={{paddingLeft: 8 + 2 * 14}}>{c.sharedPools} pool{c.sharedPools === 1 ? "" : "s"} share {c.namespace}</div>}
        </Node>)}
      </Node>)}
      {scopes.dr.visible && <Node k={scopes.dr.namespace} label="Disaster recovery" icon="link" ns={scopes.dr.namespace} depth={0} count={counts[scopes.dr.namespace]} />}
      {scopes.apps.some(a => a.visible) && <div className="stgroup">Protected applications</div>}
      {scopes.apps.filter(a => a.visible).map(a => <Node key={a.id} k={a.namespace} label={a.name} icon="shield" ns={a.namespace} depth={0} count={counts[a.namespace]} />)}
    </div>
  );
}

// ---- grants ----------------------------------------------------------------------
function GrantsPanel({nav}) {
  const a = useAccess();
  const [rev, setRev] = useState(0);
  const {data, loading, error, reload} = useResource("access.grants|" + rev, () => api.accessGrants(), 10000);
  const roles = useResource("access.roles.list", () => api.accessRoles(), 60000);
  const [sel, setSel] = useState(null);
  const [adding, setAdding] = useState(false);
  const scopes = a.state.scopes;
  const gs = data || [];
  const counts = {}; gs.forEach(g => g.namespaces.forEach(n => { counts[n] = (counts[n] || 0) + 1; }));
  const shown = sel ? gs.filter(g => g.namespaces.includes(sel)) : gs;
  const mayAdd = a.canAnywhere("create", "binding");
  if (error) return <ErrorState error={error} onRetry={reload} kind="grants" />;
  return (
    <div className="grantlayout">
      {scopes && <div className="card" style={{alignSelf: "start"}}><h3>Scopes</h3><ScopeTree scopes={Object.assign({counts}, scopes)} sel={sel} onSel={setSel} /></div>}
      <div>
        <div className="sech"><h2>Grants{sel ? <span className="mono" style={{fontWeight: 400, color: "var(--dim)", marginLeft: 8}}>{nsLabel(sel)}</span> : null}</h2><span className="ln"></span><span className="count">{shown.length}</span>
          {!adding && <button className="btn primary" disabled={!mayAdd} title={mayAdd ? "" : a.why("create", "binding", null)} onClick={() => setAdding(true)}><Icon n="plus" s={12} />Add grant</button>}</div>
        <p className="mdesc">A grant is <b>(subject, role, scope)</b> and becomes a RoleBinding in the scope's namespace — Kubernetes RBAC is the store, so a grant made here and one made with <span className="mono">kubectl</span> are the same object. Grants with an expiry, a reason, or DR fan-out are <span className="mono">AccessGrant</span>s the controller reconciles. <b>Source</b> tells them apart: <span className="lab">control-center</span> is owned here, <span className="lab">external</span> is a RoleBinding someone made directly — shown, never hidden.</p>
        {adding && <GrantForm roles={roles.data || []} scopes={scopes} preNs={sel} onDone={() => { setAdding(false); setRev(r => r + 1); a.load(); }} onCancel={() => setAdding(false)} />}
        {loading && !data ? <div className="lmsg">Loading grants…</div> : !shown.length ? <div className="card"><div className="lmsg">{sel ? `No grant lands in ${nsLabel(sel)}.` : "No grants in the namespaces you can read."}</div></div> : (
          <div className="card"><div className="bd grantwrap" style={{padding: 0}}>
            <table className="dt grants"><thead><tr><th>Subject</th><th>Role</th><th>Scope</th><th>Source</th><th>Expires</th><th>Reason</th><th></th></tr></thead>
            <tbody>{shown.map(g => {
              const mayDel = g.source !== "external" && g.namespaces.every(n => a.allowedIn(n, "delete", "accessgrants"));
              return <tr key={g.uuid} className={g.status === "Expired" ? "expired" : ""}>
                <td><span className="lab" title={g.subject.kind === "User" ? "Bound to a user — offboarding needs a Kubernetes action. Prefer groups." : ""}><Icon n={g.subject.kind === "Group" ? "users" : "user"} s={10} /><span className="mono">{g.subject.name}</span>{g.subject.kind === "User" && <Icon n="alert" s={10} c="var(--warn)" />}</span></td>
                <td><b className="mono">{g.role}</b></td>
                <td><ScopeRef scope={g.scope} namespaces={g.namespaces} nav={nav} /></td>
                <td><span className={"lab" + (g.source === "external" ? " ext" : "")}>{g.source}</span></td>
                <td className="mono" style={{color: g.status === "Expired" ? "var(--bad)" : "var(--dim)"}}>{fmtExpiry(g.expires_at)}</td>
                <td style={{fontSize: 11.5, color: "var(--dim)"}}>{g.reason || "—"}<div className="sub mono">by {(g.created_by || "").replace(/^oidc:/, "")} · {relAge(g.created_at)}</div></td>
                <td style={{textAlign: "right"}}><button className="chip" disabled={!mayDel} title={g.source === "external" ? `Not owned by an AccessGrant — remove the RoleBinding with kubectl in ${g.namespaces.join(", ")}` : mayDel ? "" : `Needs delete on accessgrants in ${g.namespaces.map(nsLabel).join(", ")}`}
                  onClick={() => window.__ui.dialog({
                    title: `Revoke ${g.role} from ${g.subject.name.replace(/^oidc:/, "")}?`, danger: true, confirm: "Revoke grant",
                    desc: `Removes the RoleBinding${g.namespaces.length > 1 ? "s" : ""} in ${g.namespaces.map(nsLabel).join(", ")}. Takes effect on the subject's next request; the console may stay optimistic for up to a minute.`,
                    run: () => api.accessGrantDelete(g.uuid).then(() => { setRev(x => x + 1); a.load(); })
                  }, {kind: "grant", id: g.uuid})}>Revoke</button></td>
              </tr>;
            })}</tbody></table>
          </div></div>
        )}
      </div>
    </div>
  );
}

const ScopeRef = ({scope, namespaces, nav}) => (
  <div>
    {scope.kind === "cluster-scope" ? <span className="lab">cluster scope</span>
      : <button className="lab link" onClick={() => {
          if (scope.kind === "storage-cluster") nav.openCluster(scope.id);
          else if (scope.kind === "managed-cluster") nav.openK8s(scope.id);
          else if (scope.kind === "storage-pool" && nav.openPool) nav.openPool((REG[scope.id] || {}).clusterId, scope.id);
          else if (scope.kind === "application" && nav.openApp) nav.openApp(scope.id);
        }}><Icon n={SCOPE_ICON[scope.kind] || "cluster"} s={10} />{scope.name || scope.id}</button>}
    {scope.drPolicyName && <span className="lab" style={{marginLeft: 4}} title="Fans out to both members of the DR pair"><Icon n="link" s={10} />{scope.drPolicyName}</span>}
    <div className="sub mono" style={{fontSize: 10.5, color: "var(--dim2)", marginTop: 2}}>{(namespaces || []).map(nsLabel).join(" · ")}</div>
  </div>
);

function GrantForm({roles, scopes, preNs, onDone, onCancel}) {
  const a = useAccess();
  const pre = preNs && scopes ? (preNs === "*" ? {kind: "cluster-scope"} : scopes.clusters.find(c => c.namespace === preNs) ? {kind: "storage-cluster", id: scopes.clusters.find(c => c.namespace === preNs).id}
    : scopes.managed.find(m => m.namespace === preNs) ? {kind: "managed-cluster", id: scopes.managed.find(m => m.namespace === preNs).id}
    : scopes.pools.find(p => p.namespace === preNs && p.isolated) ? {kind: "storage-pool", id: scopes.pools.find(p => p.namespace === preNs).id}
    : preNs === scopes.dr.namespace ? {kind: "dr-pair"} : scopes.apps.find(x => x.namespace === preNs) ? {kind: "application", id: scopes.apps.find(x => x.namespace === preNs).id} : null) : null;
  const [f, setF] = useState({kind: "Group", name: "", role: "sb:cluster-reader", scopeKind: pre ? pre.kind : "storage-cluster", scopeId: pre ? pre.id || "" : "", fanout: false, expires: "", reason: ""});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  if (!scopes) return null;
  const targets = f.scopeKind === "storage-cluster" ? scopes.clusters.filter(c => c.visible) : f.scopeKind === "managed-cluster" ? scopes.managed.filter(m => m.visible)
    : f.scopeKind === "storage-pool" ? scopes.pools.filter(p => p.visible) : f.scopeKind === "application" ? scopes.apps.filter(x => x.visible) : [];
  const chosen = targets.find(t => t.id === f.scopeId);
  const app = f.scopeKind === "application" ? chosen : null;
  const drPol = app && app.drPolicy ? scopes.dr.pairs.find(p => p.id === app.drPolicy) : null;
  const scope = f.scopeKind === "cluster-scope" ? {kind: "cluster-scope"} : f.scopeKind === "dr-pair" ? {kind: "dr-pair", name: "all pairs"}
    : chosen ? Object.assign({kind: f.scopeKind, id: chosen.id, name: chosen.name}, f.fanout && drPol ? {drPolicy: drPol.id, drPolicyName: drPol.name} : {}) : null;
  // the namespaces this grant lands in, and which roles the caller may bind there
  let nss = scope ? a.scopeNamespaces(scope).filter(Boolean) : [];
  if (scope && scope.drPolicy) { const other = [drPol.sourceClusterId, drPol.targetClusterId].filter(id => id !== app.clusterId)[0]; const oc = scopes.clusters.find(c => c.id === other); if (oc) nss = [...nss, `${app.namespace.split("@")[0]}@${oc.namespace.replace(/^sb-sc-/, "")}`]; }
  const bindable = r => nss.length > 0 && nss.every(n => a.mayBind(r, n));
  const role = roles.find(r => r.name === f.role);
  const roleOk = role ? bindable(role) : false;
  const pool = f.scopeKind === "storage-pool" ? chosen : null;
  const sharedWith = pool && !pool.isolated ? scopes.pools.filter(p => p.namespace === pool.namespace).length - 1 : 0;
  const save = async () => {
    setBusy(true); setErr(null);
    try {
      const r = await api.accessGrantCreate({subject: {kind: f.kind, name: f.name.trim()}, role: f.role, scope, expires_at: f.expires ? new Date(f.expires).toISOString() : null, reason: f.reason.trim()});
      window.__toast && window.__toast(r && r[0] && r[0].warning ? r[0].warning : `${f.name.trim()} granted ${f.role}`);
      onDone();
    } catch (e) { setErr(e.message); setBusy(false); }
  };
  return (
    <div className="card editor"><h3>Add grant</h3><div className="bd">
      <div className="frow">
        <label className="fl">Subject<div className="seg">{["Group", "User"].map(k => <button key={k} className={f.kind === k ? "on" : ""} onClick={() => setF({...f, kind: k})}>{k}</button>)}</div></label>
        <label className="fl" style={{flex: 2}}>{`${f.kind} name`}<input className="finput mono" value={f.name} placeholder={f.kind === "User" ? "oidc:alice@corp.example" : "oidc:team-a-oncall"} onChange={e => setF({...f, name: e.target.value})} />
          <span className="sub">{f.kind === "User" ? <span style={{color: "var(--warn)"}}>Bind to IdP groups, not users — offboarding a user-bound grant needs a Kubernetes action.</span> : "An IdP group as the API server presents it, prefix included. Membership changes need no Kubernetes action."}</span></label>
      </div>
      <div className="frow">
        <label className="fl">Scope<select className="sel" value={f.scopeKind} onChange={e => setF({...f, scopeKind: e.target.value, scopeId: "", fanout: false, role: e.target.value === "cluster-scope" ? "sb:infra-admin" : e.target.value === "dr-pair" ? "sb:dr-reader" : e.target.value === "application" ? "sb:app-admin" : e.target.value === "storage-pool" ? "sb:pool-reader" : "sb:cluster-reader"})}>
          <option value="cluster-scope">cluster scope</option><option value="managed-cluster">a managed cluster</option><option value="storage-cluster">a storage cluster</option>
          <option value="storage-pool">a storage pool</option><option value="dr-pair">disaster recovery</option><option value="application">a protected application</option></select></label>
        {targets.length > 0 || (f.scopeKind !== "cluster-scope" && f.scopeKind !== "dr-pair") ? <label className="fl" style={{flex: 2}}>{SCOPE_KIND_LABEL[f.scopeKind]}<select className="sel" value={f.scopeId} onChange={e => setF({...f, scopeId: e.target.value, fanout: false})}>
          <option value="">—</option>{targets.map(t => <option key={t.id} value={t.id}>{t.name}{t.namespace ? ` · ${t.namespace}` : ""}</option>)}</select>
          {!targets.length && <span className="sub" style={{color: "var(--warn)"}}>No {SCOPE_KIND_LABEL[f.scopeKind]} is visible to you.</span>}</label> : null}
        <label className="fl" style={{flex: 2}}>Role<select className="sel" value={f.role} onChange={e => setF({...f, role: e.target.value})}>
          {roles.map(r => { const ok = bindable(r); return <option key={r.name} value={r.name} disabled={!ok}>{r.name}{ok ? "" : " — cannot bind here"}</option>; })}</select>
          {role && <span className="sub">{roleOk ? role.description : nss.length ? <span style={{color: "var(--warn)"}}>You hold neither <span className="mono">bind</span> on {role.name} nor every right it grants in {nss.map(nsLabel).join(", ")}.</span> : "Pick a scope first."}</span>}</label>
      </div>
      {app && drPol && <label className="fl chk"><input type="checkbox" checked={f.fanout} onChange={e => setF({...f, fanout: e.target.checked})} /> Fan out across DR pair <span className="mono">{drPol.name}</span> — bindings on both members or neither (§4.4)</label>}
      {app && !app.drPolicy && <div className="fnote"><Icon n="alert" s={12} />This application has no DR policy, so the grant lands on its source cluster only. During a failover, the standby side would be unreachable for this subject.</div>}
      {sharedWith > 0 && <div className="fnote"><Icon n="alert" s={12} />{pool.name} shares namespace <span className="mono">{pool.namespace}</span> with {sharedWith} other pool{sharedWith === 1 ? "" : "s"}. RoleBindings are namespace-wide, so this grant covers them all. Per-pool isolation needs the pool in its own <span className="mono">sb-sp-*</span> namespace.</div>}
      <div className="frow">
        <label className="fl">Expires<input className="finput" type="date" value={f.expires} onChange={e => setF({...f, expires: e.target.value})} /><span className="sub">Optional. The controller removes the bindings when it passes.</span></label>
        <label className="fl" style={{flex: 2}}>Reason<input className="finput" value={f.reason} placeholder="OPS-4821 on-call rotation" onChange={e => setF({...f, reason: e.target.value})} /></label>
      </div>
      {nss.length > 0 && <div className="sub mono" style={{color: "var(--dim2)"}}>Lands in: {nss.map(nsLabel).join(" · ")}</div>}
      {err && <div className="ferr"><Icon n="alert" s={12} />{err}</div>}
      <div className="facts"><span className="spacer"></span>
        <button className="btn" onClick={onCancel} disabled={busy}>Cancel</button>
        <button className="btn primary" onClick={save} disabled={busy || !f.name.trim() || !scope || !roleOk} title={!roleOk && role && nss.length ? `You cannot bind ${role.name} in ${nss.map(nsLabel).join(", ")}` : ""}>{busy ? "Granting…" : "Grant"}</button>
      </div>
    </div></div>
  );
}

// ---- effective access for a subject (§4.5) --------------------------------------
function EffectivePanel() {
  const subjects = useResource("access.subjects", () => api.accessSubjects(), 30000);
  const [subject, setSubject] = useState("");
  const eff = useResource("access.eff|" + subject, () => subject ? api.accessEffective(subject) : Promise.resolve(null), 30000);
  const [chk, setChk] = useState({verb: "get", resource: "storageclusters", namespace: ""});
  const [res, setRes] = useState(null);
  const e = eff.data;
  const nss = e ? [...new Set(e.grants.flatMap(g => g.namespaces))] : [];
  return (
    <div>
      <div className="sech"><h2>Effective access</h2><span className="ln"></span></div>
      <p className="mdesc">Kubernetes has no reverse lookup for "what may this subject do everywhere" — <span className="mono">SelfSubjectRulesReview</span> works only for the caller. What follows is assembled from <b>grants issued through simplyblock</b>. ClusterRoles the platform team created independently are not visible here; spot-check a specific right with a <span className="mono">SubjectAccessReview</span> below, which the API server answers completely.</p>
      <div className="frow" style={{maxWidth: 560}}>
        <label className="fl" style={{flex: 1}}>Subject<select className="sel mono" value={subject} onChange={ev => { setSubject(ev.target.value); setRes(null); }}>
          <option value="">—</option>{(subjects.data || []).map(s => <option key={s.kind + s.name} value={`${s.kind}:${s.name}`}>{s.kind} · {s.name}</option>)}</select></label>
      </div>
      {e && subject && <div className="card"><h3>Grants issued through simplyblock <span className="count">{e.grants.length}</span></h3><div className="bd" style={{padding: 0}}>
        {!!e.groups.length && <div style={{padding: "8px 14px", borderBottom: "1px solid var(--line)", fontSize: 11.5, color: "var(--dim)"}}>Member of <span className="labels" style={{display: "inline-flex", gap: 4, marginLeft: 4}}>{e.groups.map(g => <span key={g} className="lab mono">{g}</span>)}</span></div>}
        {e.grants.length ? <table className="dt"><thead><tr><th>Role</th><th>Namespaces</th><th>Via</th><th>Source</th><th>Expires</th></tr></thead>
          <tbody>{e.grants.map(g => <tr key={g.uuid} className={g.expires_at && Date.parse(g.expires_at) < Date.now() ? "expired" : ""}>
            <td><b className="mono">{g.role}</b></td><td className="mono" style={{fontSize: 11}}>{g.namespaces.map(nsLabel).join(", ")}</td>
            <td className="mono" style={{fontSize: 11, color: "var(--dim)"}}>{g.subject.kind.toLowerCase()} {g.subject.name}</td>
            <td><span className={"lab" + (g.source === "external" ? " ext" : "")}>{g.source}</span></td><td className="mono">{fmtExpiry(g.expires_at)}</td></tr>)}</tbody></table>
          : <div className="lmsg">No grant names this subject or any of its groups.</div>}
        <div style={{padding: "8px 14px", fontSize: 11, color: "var(--dim2)", borderTop: "1px solid var(--line)"}}>{e.note}</div>
      </div></div>}
      {subject && <div className="card editor" style={{marginTop: 10}}><h3>SubjectAccessReview</h3><div className="bd">
        <div className="frow">
          <label className="fl">Verb<select className="sel mono" value={chk.verb} onChange={ev => setChk({...chk, verb: ev.target.value})}>{["get", "list", "watch", "create", "update", "patch", "delete", "bind"].map(v => <option key={v}>{v}</option>)}</select></label>
          <label className="fl" style={{flex: 1}}>Resource<input className="finput mono" value={chk.resource} onChange={ev => setChk({...chk, resource: ev.target.value})} /></label>
          <label className="fl" style={{flex: 1}}>Namespace<input className="finput mono" list="ac-ns" value={chk.namespace} placeholder="empty = cluster scope" onChange={ev => setChk({...chk, namespace: ev.target.value})} />
            <datalist id="ac-ns">{nss.filter(n => n !== "*").map(n => <option key={n} value={n} />)}</datalist></label>
          <div className="fl" style={{justifyContent: "flex-end"}}><button className="btn" onClick={() => api.accessReview({subject: {kind: subject.split(":")[0], name: subject.split(":").slice(1).join(":")}, verb: chk.verb, resource: chk.resource, namespace: chk.namespace || undefined}).then(r => setRes(r[0]))}>Check</button></div>
        </div>
        {res && <div className={"ferr" + (res.allowed ? " ok" : "")} style={res.allowed ? {color: "var(--ok)"} : {}}><Icon n={res.allowed ? "check" : "x"} s={12} />{res.allowed ? "Allowed" : "Denied"} — {res.reason}</div>}
      </div></div>}
    </div>
  );
}

// ---- §6 invariants --------------------------------------------------------------
function InvariantsPanel() {
  const [rev, setRev] = useState(0);
  const {data, loading, error, reload} = useResource("access.inv|" + rev, () => api.accessInvariants());
  if (error) return <ErrorState error={error} onRetry={reload} kind="invariants" />;
  const rows = data || [];
  const fails = rows.filter(r => r.ok === false).length;
  return (
    <div>
      <div className="sech"><h2>Invariants</h2><span className="ln"></span>
        {rows.length > 0 && <span className="badge" style={{color: fails ? "var(--bad)" : "var(--ok)"}}>{fails ? `${fails} violated` : "all hold"}</span>}
        <button className="btn" onClick={() => setRev(r => r + 1)}><Icon n="refresh" s={12} />Re-run</button></div>
      <p className="mdesc">The properties whose violation is a security bug rather than a defect (§6). Those the console can exercise run against the authorizer behind this page on every load; the rest are named so nobody assumes they are covered here.</p>
      {loading && !data ? <div className="lmsg">Running…</div> : <div className="card"><div className="bd" style={{padding: 0}}>
        {rows.map(r => <div key={r.n} className="inv">
          <span className={"invdot " + (r.ok === true ? "ok" : r.ok === false ? "bad" : "na")}><Icon n={r.ok === true ? "check" : r.ok === false ? "x" : "dots"} s={11} /></span>
          <span className="mono" style={{color: "var(--dim2)"}}>{r.n}</span>
          <div><b>{r.title}</b><div className="sub">{r.detail}</div></div>
          <span className="badge">{r.ok === null ? "backend-only" : r.ok ? "holds" : "violated"}</span>
        </div>)}
      </div></div>}
    </div>
  );
}

function AccessView({nav}) {
  const a = useAccess();
  const [tab, setTab] = useState("grants");
  const canGrants = a.canAnywhere("read", "binding");
  const tabs = [canGrants && ["grants", "Grants"], ["roles", "Roles"], canGrants && ["effective", "Effective access"], canGrants && ["invariants", "Invariants"]].filter(Boolean);
  const cur = tabs.some(t => t[0] === tab) ? tab : tabs[0][0];
  return (
    <div>
      <div className="tabs" style={{marginTop: 0}}>{tabs.map(t => <button key={t[0]} className={"tab" + (cur === t[0] ? " on" : "")} onClick={() => setTab(t[0])}>{t[1]}</button>)}</div>
      {cur === "grants" && <GrantsPanel nav={nav} />}
      {cur === "roles" && <RolesPanel />}
      {cur === "effective" && <EffectivePanel />}
      {cur === "invariants" && <InvariantsPanel />}
    </div>
  );
}

Object.assign(window, {RulesTable, RolesPanel, GrantsPanel, EffectivePanel, InvariantsPanel, AccessView, scopeLabel});

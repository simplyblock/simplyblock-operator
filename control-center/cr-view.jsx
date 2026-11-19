// ---------------------------------------------------------------------------
// CR VIEW — two tiers over any custom resource, and its Kubernetes side
//
//   Essentials   the headline fields of the kind (cr-headlines.jsx), spec
//                editable in place where the CRD allows it, status read-only,
//                conditions, the active operation, generation drift
//   Everything   the full spec and status as trees, immutable fields marked,
//                and the object as YAML
//   Kubernetes   owners and dependents, labels and annotations, events, the
//                raw metadata
//
// Immutability is read from the CRD itself: a field whose validation rule
// says `self == oldSelf`, or whose parent says `self.x == oldSelf.x`, cannot
// change after creation. An edit is refused in the console for those; every
// other edit is sent as a dry run first, so a webhook or CEL message is shown
// verbatim before anything is written.
//
// CRPanel is embedded in the curated details; CRView is the standalone page
// for a kind without one (route segment `cr` with an objKey as its id);
// CRListView lists a kind under its parent.
// ---------------------------------------------------------------------------
const CRD_CACHE = new Map();
const getPath = (o, path) => String(path || "").split(".").reduce((v, k) => (v === null || v === undefined ? undefined : v[k]), o);
const setPath = (patch, path, value) => { const ks = path.split("."); let cur = patch; ks.slice(0, -1).forEach(k => { cur[k] = cur[k] || {}; cur = cur[k]; }); cur[ks[ks.length - 1]] = value; return patch; };
const kindOfRes = kind => (RESOURCES[kind] || {});
const crdNameOf = kind => { const r = kindOfRes(kind); if (!r.plural) return null; const group = r.core ? (r.core.includes("/") ? r.core.split("/")[0] : null) : GROUP; return group ? `${r.plural}.${group}` : null; };

// ---- the CRD schema: immutable paths, descriptions, enums -------------------
async function readCrdSchema(ctx, kind) {
  const key = `${ctx}|${kind}`;
  if (CRD_CACHE.has(key)) return CRD_CACHE.get(key);
  const name = crdNameOf(kind);
  const p = (async () => {
    const out = {immutable: new Set(STATIC_IMMUTABLE[kind] || []), descriptions: {}, enums: {}, types: {}, source: "static"};
    if (!name) return out;
    let crd;
    try { crd = await k8s.get("CustomResourceDefinition", name, {ctx}); } catch (e) { out.error = e; return out; }
    const r = kindOfRes(kind);
    const version = (crd.spec.versions || []).find(v => v.name === (r.version || (r.core ? r.core.split("/")[1] : VERSION))) || (crd.spec.versions || []).find(v => v.served) || null;
    const schema = version && version.schema && version.schema.openAPIV3Schema;
    if (!schema) return out;
    out.source = "crd";
    const walk = (node, path) => {
      if (!node || typeof node !== "object") return;
      if (path && node.description) out.descriptions[path] = node.description;
      if (path && node.enum) out.enums[path] = node.enum;
      if (path && node.type) out.types[path] = node.type;
      (node["x-kubernetes-validations"] || []).forEach(v => {
        const rule = String(v.rule || "");
        if (/^\s*self\s*==\s*oldSelf\s*$/.test(rule) && path) out.immutable.add(path);
        const re = /(?:!has\(oldSelf\.(\w+)\)\s*\|\|\s*)?self\.(\w+)\s*==\s*oldSelf\.(\w+)/g;
        let m;
        while ((m = re.exec(rule))) { if (m[2] === m[3]) out.immutable.add(path ? `${path}.${m[2]}` : m[2]); }
        const re2 = /!has\(self\.(\w+)\)\s*\|\|\s*!has\(oldSelf\.(\w+)\)\s*\|\|\s*self\.(\w+)\s*==\s*oldSelf\.(\w+)/g;
        while ((m = re2.exec(rule))) { if (m[3] === m[4]) out.immutable.add(path ? `${path}.${m[3]}` : m[3]); }
      });
      Object.entries(node.properties || {}).forEach(([k, child]) => walk(child, path ? `${path}.${k}` : k));
      if (node.items) walk(node.items, path);
    };
    walk(schema, "");
    return out;
  })();
  CRD_CACHE.set(key, p);
  return p;
}
function useCrdSchema(ctx, kind) {
  const [s, setS] = React.useState(null);
  React.useEffect(() => { let alive = true; readCrdSchema(ctx, kind).then(x => alive && setS(x)); return () => { alive = false; }; }, [ctx, kind]);
  return s || {immutable: new Set(STATIC_IMMUTABLE[kind] || []), descriptions: {}, enums: {}, types: {}, source: "static", loading: true};
}
const isImmutable = (schema, path) => { const parts = path.split("."); for (let i = 1; i <= parts.length; i++) if (schema.immutable.has(parts.slice(0, i).join("."))) return true; return false; };

// ---- the object -------------------------------------------------------------
function useCR(ref, rev) {
  const key = ref ? `${ref.ctx}|${ref.kind}|${ref.ns || ""}|${ref.name}|${rev || 0}` : "none";
  return useResource("cr|" + key, () => ref ? k8s.get(ref.kind, ref.name, {ctx: ref.ctx, namespace: ref.ns}) : Promise.resolve(null), 6000);
}
const refOfVm = vm => {
  if (!vm) return null;
  if (vm.k8s && vm.k8s.kind) return {ctx: vm.ctx || LOCAL_CTX, ns: vm.k8s.namespace || vm.ns || undefined, kind: vm.k8s.kind, name: vm.k8s.name};
  const kind = VM_CR_KIND[vm.kind];
  if (!kind) return null;
  return {ctx: vm.ctx || LOCAL_CTX, ns: vm.ns || undefined, kind, name: vm.name || vm.pvName};
};
const parseObjKey = key => { const [ctx, ns, kind, ...rest] = String(key).split("/"); return {ctx, ns: ns || undefined, kind, name: rest.join("/")}; };

// ---- editing ------------------------------------------------------------------
function useSpecPatch(ref, onDone) {
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState(null);
  const save = async (path, value, schema) => {
    setErr(null);
    if (schema && isImmutable(schema, path)) { setErr(`${path} is immutable once set; the API would refuse it`); return false; }
    const patch = setPath({}, path, value);
    setBusy(true);
    try {
      await k8s.patch(ref.kind, ref.name, patch, {ctx: ref.ctx, namespace: ref.ns, dryRun: "All"});
      await k8s.patch(ref.kind, ref.name, patch, {ctx: ref.ctx, namespace: ref.ns});
      store.invalidate(ref.ctx, ref.kind);
      window.__toast(`${path} updated`);
      if (onDone) onDone();
      return true;
    } catch (e) { setErr(e.message); return false; }
    finally { setBusy(false); }
  };
  return {save, busy, err, clear: () => setErr(null)};
}
const coerce = (type, raw) => {
  if (type === "number") { const n = Number(raw); return isNaN(n) ? raw : n; }
  if (type === "bool") return raw === true || raw === "true";
  if (type === "list") return String(raw).split(",").map(s => s.trim()).filter(Boolean);
  if (type === "kv") { const o = {}; String(raw).split(",").map(s => s.trim()).filter(Boolean).forEach(p => { const [k, v] = p.split("="); const n = Number(v); o[k.trim()] = isNaN(n) || v === "" ? (v || "").trim() : n; }); return o; }
  return raw;
};
const editable = (type, value) => type === "list" ? (Array.isArray(value) ? value.join(", ") : "") : type === "kv" ? Object.entries(value || {}).map(([k, v]) => `${k}=${v}`).join(", ") : value === undefined || value === null ? "" : String(value);

function InlineField({r, value, immutable, patch, schema}) {
  const [editing, setEditing] = React.useState(false);
  const [draft, setDraft] = React.useState("");
  const shown = r.fmt ? r.fmt(value) : value;
  const opts = r.options || (schema && schema.enums[r.path]) || null;
  const locked = immutable || r.mutable === false;
  const desc = (schema && schema.descriptions[r.path]) || r.hint || "";
  if (!editing) {
    return (
      <span className="ifield" title={desc}>
        <span>{shown === null || shown === undefined || shown === "" ? <span style={{color: "var(--dim2)"}}>—</span> : String(shown)}</span>
        {locked ? <span className="immut" title={r.mutable === "ops" ? "changes through an operation" : "immutable once set"}>{r.mutable === "ops" ? "ops" : "immutable"}</span>
          : <button className="pen" title="edit" onClick={() => { setDraft(editable(r.type, value)); setEditing(true); patch.clear(); }}><Icon n="edit" s={11} /></button>}
      </span>
    );
  }
  const commit = async () => { const ok = await patch.save(r.path, coerce(r.type === "select" ? "text" : r.type, r.type === "bool" ? draft === "true" : draft), schema); if (ok) setEditing(false); };
  return (
    <span className="ifield editing">
      {r.type === "bool" ? <select className="sel finput" value={String(draft)} onChange={e => setDraft(e.target.value)}><option value="true">yes</option><option value="false">no</option></select>
        : opts ? <select className="sel finput" value={draft} onChange={e => setDraft(e.target.value)}>{opts.map(o => <option key={o} value={o}>{o}</option>)}</select>
        : <input className="finput" value={draft} type={r.type === "number" ? "number" : "text"} onChange={e => setDraft(e.target.value)} onKeyDown={e => { if (e.key === "Enter") commit(); if (e.key === "Escape") setEditing(false); }} autoFocus />}
      <button className="chip" disabled={patch.busy} onClick={commit}>{patch.busy ? "…" : "Save"}</button>
      <button className="chip" onClick={() => setEditing(false)}>Cancel</button>
      {patch.err && <span className="ferr" style={{display: "block", width: "100%"}}>{patch.err}</span>}
    </span>
  );
}

// ---- tier 1 ----------------------------------------------------------------------
function DriftBanner({obj}) {
  const g = (obj.metadata || {}).generation, og = (obj.status || {}).observedGeneration;
  if (og === undefined || og === null) return <div className="mdesc" style={{marginBottom: 8}}>This kind reports no observedGeneration, so a stale status cannot be told from a current one.</div>;
  if (g === og) return null;
  return <div className="banner"><Icon n="clock" s={15} /><span><b>Status is behind the spec.</b> Generation {g} was written, the controller last reconciled generation {og}. What status says may predate the latest edit.</span></div>;
}
function LinkValue({r, value, obj, nav}) {
  if (!r.link || value === undefined || value === null || value === "") return null;
  const ctx = obj.__ctx || LOCAL_CTX;
  const byK8s = (kind, name) => Object.values(REG).find(v => v.ctx === ctx && v.k8s && v.k8s.kind === kind && v.k8s.name === name);
  const go = () => {
    if (r.link === "cluster") { const c = byK8s("StorageCluster", value); if (c) nav.openCluster(c.id); }
    else if (r.link === "node") { const n = byK8s("StorageNode", value); if (n) nav.openNode(n.clusterId, n.id); }
    else if (r.link === "nodeset") nav.openCR({ctx, ns: obj.metadata.namespace, kind: "StorageNodeSet", name: value});
    else if (r.link === "worker") nav.openCR({ctx, kind: "Node", name: value});
    else if (r.link === "storageclass") { const s = Object.values(REG).find(v => v.kind === "storageclass" && v.name === value && v.ctx === ctx); if (s) nav.openStorageClass(s.id); else nav.openCR({ctx, kind: "StorageClass", name: value}); }
    else if (r.link === "pvc") { const ns = getPath(obj, r.path.replace(/\.name$/, ".namespace")) || obj.metadata.namespace; const p = Object.values(REG).find(v => v.kind === "pvc" && v.name === value && v.namespace === ns && v.ctx === ctx); if (p) nav.openPvc(p.id); else nav.openCR({ctx, ns, kind: "PersistentVolumeClaim", name: value}); }
    else if (r.link === "pv") { const v = Object.values(REG).find(x => x.kind === "volume" && x.pvName === value && x.ctx === ctx); if (v) nav.openVolumeById(v.id); else nav.openCR({ctx, kind: "PersistentVolume", name: value}); }
    else if (r.link === "pool") { const p = Object.values(REG).find(v => v.kind === "pool" && v.name === value && v.ctx === ctx); if (p) nav.openPool(p.clusterId, p.id); }
    else if (r.link === "vsc") nav.openCR({ctx, kind: "VolumeSnapshotContent", name: value});
  };
  return <button className="lab link" style={{marginLeft: 6}} onClick={go}><Icon n="chev" s={9} />open</button>;
}
function EssentialsTab({obj, cref: ref, nav, vm, onChanged}) {
  const schema = useCrdSchema(ref.ctx, ref.kind);
  const patch = useSpecPatch(ref, onChanged);
  const h = HEADLINES[ref.kind] || {spec: [], status: []};
  const st = obj.status || {};
  const ops = window.work && window.work.ready ? window.work.forObject(vm || {}) : [];
  const running = ops.filter(o => o.phase === "Running" || o.phase === "Pending");
  const rows = list => list.filter(r => r.path.startsWith("spec") ? true : true).map(r => {
    const v = getPath(obj, r.path);
    return [<span title={schema.descriptions[r.path] || r.hint || r.path}>{r.label}</span>,
      <span style={{display: "inline-flex", alignItems: "center", gap: 4, justifyContent: "flex-end", flexWrap: "wrap"}}>
        {r.path.startsWith("spec") || !r.path.includes(".") || ref.kind === "StorageClass" ? <InlineField r={r} value={v} immutable={isImmutable(schema, r.path)} patch={patch} schema={schema} /> : <span>{(r.fmt ? r.fmt(v) : v) === null || (r.fmt ? r.fmt(v) : v) === undefined ? <span style={{color: "var(--dim2)"}}>—</span> : String(r.fmt ? r.fmt(v) : v)}</span>}
        <LinkValue r={r} value={v} obj={Object.assign({__ctx: ref.ctx}, obj)} nav={nav} />
      </span>];
  });
  const specRows = rows(h.spec), statusRows = rows(h.status.filter(r => !r.path.startsWith("spec")));
  const anns = (h.annotations || []).map(k => [k, (obj.metadata.annotations || {})[k]]).filter(([, v]) => v !== undefined);
  return (
    <>
      <DriftBanner obj={obj} />
      {st.activeOpsRef && <div className="banner" style={{color: "var(--info)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}><Icon n="clock" s={15} />
        <span><b>Operation in progress:</b> {st.activeOpsRef}. Other actions wait until it finishes. {nav.monitor && <button className="chip" onClick={() => nav.monitor({cluster: vm && (vm.kind === "cluster" ? vm.id : vm.clusterId), q: st.activeOpsRef})}>Follow it</button>}</span></div>}
      {!st.activeOpsRef && running.length > 0 && <div className="banner" style={{color: "var(--info)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}><Icon n="clock" s={15} /><span>{running.length} operation{running.length === 1 ? "" : "s"} running on this object.</span></div>}
      <div className="mdesc" style={{marginBottom: 8}}>Schema {schema.source === "crd" ? "read from the CRD" : schema.loading ? "loading" : "from the console's own table (the CRD could not be read)"}{schema.immutable.size ? ` · ${schema.immutable.size} immutable path${schema.immutable.size === 1 ? "" : "s"}` : ""}. Edits are dry-run against the API server first.</div>
      <div className="dcols">
        <div className="card"><h3>Spec · essentials</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>{specRows.length ? <Props rows={specRows} /> : <div className="nolim">No curated spec fields for {ref.kind}.</div>}</div></div>
        <div>
          <div className="card"><h3>Status · essentials</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>{statusRows.length ? <Props rows={statusRows} /> : <div className="nolim">No status reported.</div>}</div></div>
          {anns.length > 0 && <div className="card" style={{marginTop: 12}}><h3>Annotations that drive behavior</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={anns.map(([k, v]) => [<span className="mono" style={{fontSize: 11}}>{k}</span>, v])} /></div></div>}
          {window.ConditionsCard && (st.conditions || []).length > 0 && <div style={{marginTop: 12}}><ConditionsCard conditions={st.conditions} /></div>}
        </div>
      </div>
      {(h.tables || []).map(t => { const rowsT = getPath(obj, t.path); if (!Array.isArray(rowsT) || !rowsT.length) return null; return (
        <div key={t.path} className="card" style={{marginTop: 12}}><h3>{t.title}</h3><div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr>{t.columns.map(c => <th key={c}>{c}</th>)}</tr></thead><tbody>{rowsT.map((x, i) => <tr key={i}>{t.columns.map(c => <td key={c} className="mono" style={{fontSize: 11}}>{x[c] === undefined || x[c] === null ? "—" : typeof x[c] === "object" ? JSON.stringify(x[c]) : String(x[c])}</td>)}</tr>)}</tbody></table>
        </div></div>); })}
    </>
  );
}

// ---- tier 2 ------------------------------------------------------------------------
function JsonTree({data, path, schema, depth}) {
  const [open, setOpen] = React.useState((depth || 0) < 2);
  if (data === null || data === undefined) return <span className="v" style={{color: "var(--dim2)"}}>null</span>;
  if (typeof data !== "object") return <span className="v">{typeof data === "string" ? `"${data}"` : String(data)}</span>;
  const entries = Array.isArray(data) ? data.map((v, i) => [String(i), v]) : Object.entries(data);
  if (!entries.length) return <span className="v" style={{color: "var(--dim2)"}}>{Array.isArray(data) ? "[]" : "{}"}</span>;
  return (
    <div className="jt">
      <span className="tg" onClick={() => setOpen(!open)}>{open ? "▾" : "▸"} {Array.isArray(data) ? `[${entries.length}]` : `{${entries.length}}`}</span>
      {open && <div className="n">{entries.map(([k, v]) => {
        const p = path ? (Array.isArray(data) ? path : `${path}.${k}`) : k;
        const imm = schema && !Array.isArray(data) && isImmutable(schema, p);
        return <div key={k} title={schema && schema.descriptions[p] ? schema.descriptions[p] : p}><span className="k">{k}</span>{imm && <span className="immut">immutable</span>}: <JsonTree data={v} path={p} schema={schema} depth={(depth || 0) + 1} /></div>;
      })}</div>}
    </div>
  );
}
function toYaml(v, indent) {
  const pad = "  ".repeat(indent || 0);
  if (v === null || v === undefined) return "null";
  if (typeof v === "string") return /[\n:#{}\[\],&*?|<>=!%@`"']|^\s|\s$|^$|^(true|false|null|yes|no|~)$|^-?\d/.test(v) ? (v.includes("\n") ? "|\n" + v.split("\n").map(l => pad + "  " + l).join("\n") : JSON.stringify(v)) : v;
  if (typeof v !== "object") return String(v);
  if (Array.isArray(v)) return v.length ? "\n" + v.map(x => `${pad}- ${typeof x === "object" && x !== null ? toYaml(x, (indent || 0) + 1).replace(/^\n/, "").replace(new RegExp("^" + pad + "  "), "") : toYaml(x, (indent || 0) + 1)}`).join("\n") : "[]";
  const keys = Object.keys(v);
  if (!keys.length) return "{}";
  return "\n" + keys.map(k => { const x = v[k]; const s = toYaml(x, (indent || 0) + 1); return `${pad}${k}:${s.startsWith("\n") ? s : " " + s}`; }).join("\n");
}
function YamlBlock({obj}) {
  const clean = Object.assign({}, obj, {metadata: Object.assign({}, obj.metadata, {managedFields: undefined})});
  const text = toYaml(clean, 0).replace(/^\n/, "");
  return (
    <div className="card"><h3 style={{display: "flex", alignItems: "center", gap: 10}}>YAML<span style={{flex: 1}}></span><CopyBtn get={() => text} label="Copy" />
      <button className="chip" onClick={() => downloadText(`${obj.kind}-${obj.metadata.name}.yaml`, text)}><Icon n="cloud" s={11} />Download</button></h3>
      <div className="bd"><pre className="code yaml">{text}</pre></div></div>
  );
}
function EverythingTab({obj, cref: ref}) {
  const schema = useCrdSchema(ref.ctx, ref.kind);
  const spec = obj.spec !== undefined ? obj.spec : Object.fromEntries(Object.entries(obj).filter(([k]) => !["apiVersion", "kind", "metadata", "status"].includes(k)));
  return (
    <>
      <div className="dcols">
        <div className="card"><h3>spec</h3><div className="bd"><JsonTree data={spec} path="spec" schema={schema} depth={0} /></div></div>
        <div className="card"><h3>status</h3><div className="bd"><JsonTree data={obj.status || {}} path="status" schema={schema} depth={0} /></div></div>
      </div>
      <div style={{marginTop: 12}}><YamlBlock obj={obj} /></div>
    </>
  );
}

// ---- the Kubernetes side -----------------------------------------------------------------
function KubernetesTab({obj, cref: ref, nav}) {
  const m = obj.metadata || {};
  const {data: events} = useResource("crev|" + m.uid, () => k8s.list("Event", {ctx: ref.ctx, namespace: m.namespace || "default", fieldSelector: `involvedObject.uid=${m.uid}`}).catch(() => []), 10000);
  const {data: owners} = useResource("crown|" + m.uid, () => Promise.all((m.ownerReferences || []).map(o => k8s.get(o.kind, o.name, {ctx: ref.ctx, namespace: m.namespace}).catch(() => null))), 30000);
  const evs = (events || []).slice().sort((a, b) => Date.parse(b.lastTimestamp || b.eventTime || 0) - Date.parse(a.lastTimestamp || a.eventTime || 0));
  const openRef = r => nav.openCR({ctx: ref.ctx, ns: m.namespace, kind: r.kind, name: r.name});
  return (
    <>
      <div className="dcols">
        <div className="card"><h3>Owners</h3><div className="bd">
          {(m.ownerReferences || []).length ? (m.ownerReferences || []).map((o, i) => <div key={o.uid} className="labels" style={{marginBottom: 6}}>
            <button className="lab link" onClick={() => openRef(o)}><Icon n="chev" s={9} />{o.kind}/{o.name}</button>{o.controller && <span className="lab"><i>controller</i>yes</span>}
            {owners && owners[i] && <span className="lab"><i>phase</i>{(owners[i].status || {}).phase || (owners[i].status || {}).status || "—"}</span>}</div>) : <div className="nolim">No owner: this object stands on its own.</div>}
          <div className="mdesc" style={{marginTop: 8}}>Owned by: list the owner's kind to see its dependents; the API server deletes dependents with their controller owner.</div>
        </div></div>
        <div className="card"><h3>Metadata</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[
          ["Name", <span className="mono">{m.name}</span>], ["Namespace", m.namespace || "cluster-scoped"], ["UID", <span className="mono" style={{fontSize: 11}}>{m.uid}</span>],
          ["Resource version", m.resourceVersion], ["Generation", m.generation], ["Created", fmtDate(m.creationTimestamp)], ["Deleting since", m.deletionTimestamp ? fmtDate(m.deletionTimestamp) : null],
          ["Finalizers", (m.finalizers || []).join(", ") || null], ["Context", ref.ctx]]} /></div></div>
      </div>
      <div className="dcols" style={{marginTop: 12}}>
        <div className="card"><h3>Labels</h3><div className="bd"><div className="labels">{Object.entries(m.labels || {}).map(([k, v]) => <span key={k} className="lab mono" style={{fontSize: 10.5}}><i>{k}</i>{v}</span>)}{!Object.keys(m.labels || {}).length && <span className="nolim">none</span>}</div></div></div>
        <div className="card"><h3>Annotations</h3><div className="bd"><div className="labels">{Object.entries(m.annotations || {}).filter(([k]) => k !== "kubectl.kubernetes.io/last-applied-configuration").map(([k, v]) => <span key={k} className="lab mono" style={{fontSize: 10.5}} title={v}><i>{k}</i>{String(v).slice(0, 60)}</span>)}{!Object.keys(m.annotations || {}).length && <span className="nolim">none</span>}</div></div></div>
      </div>
      <div className="card" style={{marginTop: 12}}><h3>Events</h3><div className="bd" style={{padding: 0}}>
        {evs.length ? <table className="dt"><thead><tr><th>Type</th><th>Reason</th><th>Count</th><th>Last</th><th>Message</th></tr></thead><tbody>
          {evs.map(e => <tr key={e.metadata.uid}><td><span className="badge" style={{color: e.type === "Warning" ? "var(--warn)" : undefined}}>{e.type}</span></td><td className="mono">{e.reason}</td><td className="mono">{e.count || 1}</td><td className="mono">{fmtAgo(e.lastTimestamp || e.eventTime)}</td><td>{e.message}</td></tr>)}
        </tbody></table> : <div className="lmsg">No event recorded for this object.</div>}
      </div></div>
    </>
  );
}

// ---- panel, page, list ------------------------------------------------------------------------
function CRPanel({vm, nav, cref: refProp}) {
  const ref = refProp || refOfVm(vm);
  const [tab, setTab] = React.useState("essentials");
  const [rev, setRev] = React.useState(0);
  const {data: obj, loading, error, reload} = useCR(ref, rev);
  if (!ref) return <div className="lmsg">No custom resource backs this object.</div>;
  if (error) return <ErrorState error={error} onRetry={reload} />;
  if (loading || !obj) return <div className="lmsg">reading {ref.kind} {ref.name}…</div>;
  const items = [{k: "essentials", label: "Essentials", icon: "list"}, {k: "everything", label: "Everything", icon: "gauge"}, {k: "k8s", label: "Kubernetes", icon: "k8s"}];
  return (
    <div className="crpanel">
      <div className="tabs sub">{items.map(t => <button key={t.k} className={"tab" + (tab === t.k ? " on" : "")} onClick={() => setTab(t.k)}><Icon n={t.icon} s={12} />{t.label}</button>)}
        <span style={{flex: 1}}></span><span className="mono" style={{fontSize: 10.5, color: "var(--dim2)", alignSelf: "center"}}>{ref.kind} {ref.ns ? ref.ns + "/" : ""}{ref.name} · {ref.ctx}</span></div>
      {tab === "essentials" && <EssentialsTab obj={obj} cref={ref} nav={nav} vm={vm} onChanged={() => setRev(r => r + 1)} />}
      {tab === "everything" && <EverythingTab obj={obj} cref={ref} />}
      {tab === "k8s" && <KubernetesTab obj={obj} cref={ref} nav={nav} />}
    </div>
  );
}
const CR_PHASE = o => (o.status || {}).phase || (o.status || {}).status || (o.status || {}).state || ((o.status || {}).ready !== undefined ? ((o.status || {}).ready ? "ready" : "not ready") : null);
function CRView({seg, nav}) {
  const ref = parseObjKey(seg.id);
  const {data: obj, loading, error, reload} = useCR(ref, 0);
  if (error) return <div className="scroll"><ErrorState error={error} onRetry={reload} kind={ref.kind} /></div>;
  if (loading || !obj) return <div className="scroll"><div className="lmsg">reading {ref.kind} {ref.name}…</div></div>;
  const phase = CR_PHASE(obj);
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}>
        <div style={{display: "flex", alignItems: "center", gap: 11, flexWrap: "wrap"}}><h1>{obj.metadata.name}</h1><span className="badge">{obj.kind}</span>{phase && <span className="badge">{phase}</span>}</div>
        <div className="mdesc" style={{marginTop: 4}}>{obj.apiVersion} · {obj.metadata.namespace ? `namespace ${obj.metadata.namespace}` : "cluster-scoped"} · context {ref.ctx}</div></div>
        {ABORTABLE_KINDS.includes(obj.kind) && !["Succeeded", "Failed", "Aborted"].includes((obj.status || {}).phase) && <button className="btn" onClick={() => api.operationAbort(obj.kind, obj.metadata.name, {ctx: ref.ctx, namespace: ref.ns}).then(() => { window.__toast("Abort requested"); reload(); }).catch(e => window.__toast(e.message))}><Icon n="x" s={11} />Abort</button>}
      </div>
      <CRPanel cref={ref} nav={nav} vm={null} />
    </div>
  );
}
// a list of one kind under a parent, as a table that opens CRView
function CRListView({kind, items, nav, title, columns}) {
  const cols = columns || ["name", "namespace", "phase", "age"];
  return (
    <div className="card"><h3>{title || kind}</h3><div className="bd" style={{padding: 0}}>
      {items.length ? <table className="dt"><thead><tr>{cols.map(c => <th key={c}>{c}</th>)}<th></th></tr></thead><tbody>
        {items.map(o => <tr key={o.metadata.uid}>
          {cols.map(c => <td key={c} className={c === "name" ? "" : "mono"}>{c === "name" ? <button className="tlink" onClick={() => nav.openCR({ctx: o.__ctx || LOCAL_CTX, ns: o.metadata.namespace, kind: o.kind || kind, name: o.metadata.name})}>{o.metadata.name}</button>
            : c === "namespace" ? o.metadata.namespace || "—" : c === "phase" ? (CR_PHASE(o) || "—") : c === "age" ? fmtAgo(o.metadata.creationTimestamp) : (getPath(o, c) === undefined ? "—" : String(getPath(o, c)))}</td>)}
          <td style={{textAlign: "right"}}><button className="chip" onClick={() => nav.openCR({ctx: o.__ctx || LOCAL_CTX, ns: o.metadata.namespace, kind: o.kind || kind, name: o.metadata.name})}>Open</button></td>
        </tr>)}
      </tbody></table> : <div className="lmsg">No {kind} here.</div>}
    </div></div>
  );
}
// the layers added for drill-down: node sets of a cluster, consumers of a cluster
function NodeSetsView({cluster, nav}) {
  const {data, loading, error, reload} = useResource("nodesets|" + cluster.id, () => store.read(cluster.ctx, "StorageNodeSet").then(r => r.filter(s => (s.spec || {}).clusterName === cluster.k8s.name).map(s => Object.assign({__ctx: cluster.ctx, kind: "StorageNodeSet"}, s))), 8000);
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return loading && !data ? <div className="lmsg">loading…</div> : <CRListView kind="StorageNodeSet" items={data || []} nav={nav} title={`Node sets of ${cluster.name}`} columns={["name", "namespace", "status.onlineNodes", "status.totalNodes", "spec.workerNodes", "age"]} />;
}
function ServedView({cluster, nav}) {
  const {data, loading} = useResource("served|" + cluster.id, () => sources.pvcs(cluster.ctx).then(ps => ps.filter(p => p.cluster_id === cluster.id)), 15000);
  const byNs = {};
  (data || []).forEach(p => { const k = p.namespace; byNs[k] = byNs[k] || {ns: k, pvcs: 0, bound: 0, bytes: 0, classes: new Set()}; byNs[k].pvcs++; if (p.status === "Bound") byNs[k].bound++; byNs[k].bytes += p.actual_bytes || p.requested_bytes || 0; byNs[k].classes.add(p.storage_class); });
  const rows = Object.values(byNs).sort((a, b) => b.pvcs - a.pvcs);
  return (
    <div className="card"><h3>Kubernetes consumers of {cluster.name} · context {cluster.ctx}</h3><div className="bd" style={{padding: 0}}>
      {loading && !data ? <div className="lmsg">loading…</div> : rows.length ? <table className="dt"><thead><tr><th>Namespace</th><th>Claims</th><th>Bound</th><th>Provisioned</th><th>Storage classes</th><th></th></tr></thead><tbody>
        {rows.map(r => <tr key={r.ns}><td className="mono">{r.ns}</td><td className="mono">{r.pvcs}</td><td className="mono">{r.bound}</td><td className="mono">{fmtBytes(r.bytes)}</td><td>{Array.from(r.classes).join(", ")}</td>
          <td style={{textAlign: "right"}}><button className="chip" onClick={() => nav.layer(cluster, "volumes")}>Volumes</button></td></tr>)}
      </tbody></table> : <div className="lmsg">No claim is served by this cluster yet.</div>}
    </div></div>
  );
}
function NodePodsView({node, nav}) {
  const {data, loading} = useResource("npods|" + node.id, () => sources.nodePods(node.ctx, node), 8000);
  return (
    <div className="card"><h3>Pods on {node.hostname}</h3><div className="bd" style={{padding: 0}}>
      {loading && !data ? <div className="lmsg">loading…</div> : (data || []).length ? <table className="dt"><thead><tr><th>Pod</th><th>Kind</th><th>Phase</th><th>Containers</th><th></th></tr></thead><tbody>
        {data.map(p => <tr key={p.name}><td className="mono">{p.name}</td><td><span className="badge">{p.kind}</span></td><td><TrafficLight status={p.phase === "Running" ? "online" : "offline"} label={p.phase} /></td><td className="mono">{p.containers.join(", ")}</td>
          <td style={{textAlign: "right"}}><button className="chip" onClick={() => nav.openCR({ctx: p.ctx, ns: p.namespace, kind: "Pod", name: p.name})}>Open</button>{nav.nodeLogs && <button className="chip" onClick={() => nav.nodeLogs(node.clusterId, node.id, {pod: [p.name]})}>Logs</button>}</td></tr>)}
      </tbody></table> : <div className="lmsg">No storage plane pod runs on this worker.</div>}
    </div></div>
  );
}
// the relation cards a curated detail adds below its own
function RelationCards({vm, nav}) {
  const rels = RELATIONS[vm.kind] || [];
  if (!rels.length) return null;
  const go = r => {
    if (r.layer) return nav.layer(vm, r.layer);
    if (r.go === "served") return nav.layer(vm, "served");
    if (r.go === "pods") return nav.layer(vm, "pods");
    if (r.go === "monitor") return nav.monitor({cluster: vm.kind === "cluster" ? vm.id : vm.clusterId, q: vm.kind === "cluster" ? undefined : (vm.hostname || vm.name), state: "all"});
    if (r.go === "logs") return vm.kind === "node" ? nav.nodeLogs(vm.clusterId, vm.id) : nav.logs(vm.id);
    if (r.go === "alerts") return nav.alerts({cluster: vm.kind === "cluster" ? vm.id : vm.clusterId});
  };
  return <><div className="sech" style={{marginTop: 12}}><h2>More</h2><span className="ln"></span></div>
    <div className="navcards">{rels.map(r => <NavCard key={r.title} icon={r.icon} title={r.title} sub={r.sub} count="" onClick={() => go(r)} />)}</div></>;
}
// the configuration section appended to every curated detail
function CRSection({vm, nav}) {
  const [open, setOpen] = useLocal("sb.cr.open", true);
  const ref = refOfVm(vm);
  if (!ref) return null;
  return (
    <div style={{marginTop: 14}}>
      <div className="sech" style={{cursor: "pointer"}} onClick={() => setOpen(!open)}><h2><Icon n={open ? "chevd" : "chev"} s={11} /> Custom resource · {ref.kind}</h2><span className="ln"></span><span className="mono" style={{fontSize: 10.5, color: "var(--dim2)"}}>{ref.ns ? ref.ns + "/" : ""}{ref.name}</span></div>
      {open && <CRPanel vm={vm} nav={nav} />}
      <RelationCards vm={vm} nav={nav} />
    </div>
  );
}

Object.assign(window, {CRPanel, CRView, CRListView, CRSection, RelationCards, NodeSetsView, ServedView, NodePodsView, EssentialsTab, EverythingTab, KubernetesTab, JsonTree, YamlBlock, toYaml,
  useCrdSchema, readCrdSchema, isImmutable, useSpecPatch, refOfVm, parseObjKey, getPath, setPath});

// ---------------------------------------------------------------------------
// CR VIEW: two tiers over any custom resource, and its Kubernetes side
//
//   Essentials   the headline fields of the kind (cr-headlines.jsx), spec
//                editable in place where the CRD allows it, status read-only,
//                the active operation, generation drift
//   Everything   the full spec and status as trees, immutable fields marked,
//                descriptions from the CRD on hover, and the object as YAML
//   Kubernetes   owners, labels and annotations, events, the raw metadata
//
// Field names are the CRD's own; descriptions, enums, and immutability come
// from the CRD read through apiextensions (a field whose validation rule says
// `self == oldSelf`, or whose parent says `!has(oldSelf.x) || has(self.x)`,
// cannot change after creation). Two more properties come from the CRD's
// annotations, which the operator's +simplyblock: field markers write
// (operator/hack/fieldmarkers): an operator-managed field is the operator's to
// change, an advanced one is shown in Everything only, and an invisible one is
// not shown at all. Essentials lists the spec fields of the CRD, top-level ones
// first and then nested ones, each in natural order, so a field added to the
// API reaches the console without an edit here; the curated headlines of the
// kind (cr-headlines.jsx) only decorate the fields they name, and one the CRD
// no longer has is dropped. An edit is refused in the console for immutable
// and operator-managed fields; every other edit is sent as a dry
// run first, so a webhook or CEL message is shown verbatim before anything is
// written. The YAML block stays the whole object, invisible fields included,
// because it is what a copy or a download is made from.
//
// CRPanel is embedded in the curated details; CRView is the standalone page
// for a kind without one (route segment `cr` with an object key as its id).
// ---------------------------------------------------------------------------
const CRD_CACHE = new Map();
const getPath = (o, path) => String(path || "").split(".").reduce((v, k) => (v === null || v === undefined ? undefined : v[k]), o);
const setPath = (patch, path, value) => { const ks = path.split("."); let cur = patch; ks.slice(0, -1).forEach(k => { cur[k] = cur[k] || {}; cur = cur[k]; }); cur[ks[ks.length - 1]] = value; return patch; };
const kindOfRes = kind => (RESOURCES[kind] || {});
const crdNameOf = kind => { const r = kindOfRes(kind); if (!r.plural) return null; const group = r.core ? (r.core.includes("/") ? r.core.split("/")[0] : null) : GROUP; return group ? `${r.plural}.${group}` : null; };
const downloadText = (name, text) => { const a = document.createElement("a"); a.href = URL.createObjectURL(new Blob([text], {type: "text/plain"})); a.download = name; a.click(); URL.revokeObjectURL(a.href); };

// ---- the CRD schema: immutable paths, descriptions, enums -------------------
async function readCrdSchema(kind) {
  if (CRD_CACHE.has(kind)) return CRD_CACHE.get(kind);
  const name = crdNameOf(kind);
  const p = (async () => {
    const out = emptySchema(kind);
    if (!name) return out;
    let crd;
    try { crd = await k8s.get("CustomResourceDefinition", name); } catch (e) { out.error = e; return out; }
    const r = kindOfRes(kind);
    const want = r.version || (r.core ? r.core.split("/")[1] : VERSION);
    const version = (crd.spec.versions || []).find(v => v.name === want) || (crd.spec.versions || []).find(v => v.storage) || null;
    const schema = version && version.schema && version.schema.openAPIV3Schema;
    if (!schema) return out;
    out.source = "crd";
    out.version = version.name;
    markedPaths(crd, "simplyblock.io/operator-managed-fields", version.name).forEach(p => out.managed.add(p));
    markedPaths(crd, "simplyblock.io/invisible-fields", version.name).forEach(p => out.invisible.add(p));
    markedPaths(crd, "simplyblock.io/advanced-fields", version.name).forEach(p => out.advanced.add(p));
    // within is set below a list or a map: a field there names one field of
    // every element, which has no single value to show or to patch
    const walk = (node, path, within) => {
      if (!node || typeof node !== "object") return;
      if (path && node.description) out.descriptions[path] = node.description;
      if (path && node.enum) out.enums[path] = node.enum;
      if (path && node.type) out.types[path] = node.type;
      if (path) out.known.add(path);
      // a spec field Essentials can show and edit: a scalar, a list of
      // scalars, or a string map; an object is listed through its fields
      if (path.startsWith("spec.") && !within) out.order.push(path);
      if (path.startsWith("spec.") && !within) {
        const items = node.items || {};
        const scalar = ["string", "integer", "number", "boolean"].includes(node.type);
        const list = node.type === "array" && ["string", "integer", "number"].includes(items.type);
        const kv = node.type === "object" && !node.properties && node.additionalProperties && node.additionalProperties.type === "string";
        if (scalar || list || kv) out.fields.push({path, type: node.type, list, kv});
      }
      (node["x-kubernetes-validations"] || []).forEach(v => {
        const rule = String(v.rule || "");
        if (/^\s*self\s*==\s*oldSelf\s*$/.test(rule) && path) out.immutable.add(path);
        let m;
        // `!has(oldSelf.x) || has(self.x)`: write-once, so immutable once set
        const re0 = /!has\(oldSelf\.(\w+)\)\s*\|\|\s*has\(self\.(\w+)\)/g;
        while ((m = re0.exec(rule))) { if (m[1] === m[2]) out.immutable.add(path ? `${path}.${m[1]}` : m[1]); }
        const re = /(?:!has\(oldSelf\.(\w+)\)\s*\|\|\s*)?self\.(\w+)\s*==\s*oldSelf\.(\w+)/g;
        while ((m = re.exec(rule))) { if (m[2] === m[3]) out.immutable.add(path ? `${path}.${m[2]}` : m[2]); }
        const re2 = /!has\(self\.(\w+)\)\s*\|\|\s*!has\(oldSelf\.(\w+)\)\s*\|\|\s*self\.(\w+)\s*==\s*oldSelf\.(\w+)/g;
        while ((m = re2.exec(rule))) { if (m[3] === m[4]) out.immutable.add(path ? `${path}.${m[3]}` : m[3]); }
      });
      Object.entries(node.properties || {}).forEach(([k, child]) => walk(child, path ? `${path}.${k}` : k, within));
      if (node.items) walk(node.items, path, true);
      if (node.additionalProperties && typeof node.additionalProperties === "object") walk(node.additionalProperties, path, true);
    };
    walk(schema, "");
    return out;
  })();
  CRD_CACHE.set(kind, p);
  return p;
}
function useCrdSchema(kind) {
  const [s, setS] = React.useState(null);
  React.useEffect(() => { let alive = true; readCrdSchema(kind).then(x => alive && setS(x)); return () => { alive = false; }; }, [kind]);
  return s || Object.assign(emptySchema(kind), {loading: true});
}
const emptySchema = kind => ({immutable: new Set(STATIC_IMMUTABLE[kind] || []), managed: new Set(), invisible: new Set(), advanced: new Set(),
  known: new Set(), fields: [], order: [], descriptions: {}, enums: {}, types: {}, source: "static"});
// A marker annotation holds the marked paths per API version as JSON:
// {"v1alpha2":["spec.a","status.b"]}. A malformed one marks nothing.
const markedPaths = (crd, key, version) => {
  try { const v = JSON.parse(((crd.metadata || {}).annotations || {})[key] || "{}")[version]; return Array.isArray(v) ? v : []; } catch (e) { return []; }
};
// a path is marked when it, or an object above it, is
const marked = (set, path) => { const parts = path.split("."); for (let i = 1; i <= parts.length; i++) if (set.has(parts.slice(0, i).join("."))) return true; return false; };
const isImmutable = (schema, path) => marked(schema.immutable, path);
const isManaged = (schema, path) => !!schema.managed && marked(schema.managed, path);
// An operator-managed field is, to the console, an immutable one: neither is
// the user's to change. This is the one check every lock decision goes through.
const isLocked = (schema, path) => !!schema && (isImmutable(schema, path) || isManaged(schema, path));
const isInvisible = (schema, path) => !!schema.invisible && marked(schema.invisible, path);
const isAdvanced = (schema, path) => !!schema.advanced && marked(schema.advanced, path);
// what Essentials leaves out: an advanced field is in Everything only
const offEssentials = (schema, path) => isInvisible(schema, path) || isAdvanced(schema, path);
// The spec rows of Essentials. With the CRD read, they are its spec fields:
// the top-level ones first, then the nested ones, each in natural order
// (port2 before port10, case aside), which keeps every block together; a curated
// headline (cr-headlines.jsx) only decorates the field it names, with a label,
// a format, a link, or a read-only flag, and one naming an object stands in for
// the fields below it. A headline the CRD does not have is dropped. Without
// the CRD, the curated headlines are all there is to go by.
function essentialSpecRows(curated, schema) {
  if (schema.source !== "crd") return curated.filter(r => !offEssentials(schema, r.path)).sort(essentialOrder);
  const byPath = Object.fromEntries(curated.map(r => [r.path, r]));
  const fields = Object.fromEntries(schema.fields.map(f => [f.path, f]));
  const rows = [];
  let inside = null; // a curated object path whose fields it stands in for
  schema.order.forEach(path => {
    if (inside && path.startsWith(inside + ".")) return;
    inside = null;
    if (byPath[path]) {
      rows.push(byPath[path]);
      if (!fields[path]) inside = path;
      return;
    }
    const f = fields[path];
    if (!f) return;
    rows.push({
      path, label: path.replace(/^spec\./, ""), mutable: true, fmt: f.type === "boolean" ? F.bool : F.raw,
      type: f.type === "boolean" ? "bool" : f.type === "integer" || f.type === "number" ? "number" : f.list ? "list" : f.kv ? "kv" : "text",
    });
  });
  return rows.filter(r => !offEssentials(schema, r.path)).sort(essentialOrder);
}
const naturally = new Intl.Collator(undefined, {numeric: true, sensitivity: "base"});
const essentialOrder = (a, b) => {
  const nested = r => (r.path.match(/\./g) || []).length > 1; // beyond "spec."
  return (nested(a) - nested(b)) || naturally.compare(a.path, b.path);
};
// the badge a locked field carries, immutability first: it binds the operator too
const lockOf = (schema, path) => !isLocked(schema, path) ? null : isImmutable(schema, path) ? "immutable" : "operator-managed";
const LOCK_TITLE = {"immutable": "immutable once set", "operator-managed": "set by the operator, not by hand"};

// ---- the object -------------------------------------------------------------
function useCR(ref, rev) {
  const key = ref ? `${ref.ctx || ""}|${ref.kind}|${ref.ns || ""}|${ref.name}|${rev || 0}` : "none";
  return useResource("cr|" + key, () => ref ? k8s.get(ref.kind, ref.name, {namespace: ref.ns, ctx: ref.ctx}) : Promise.resolve(null), 6000);
}
const refOfVm = vm => {
  if (!vm) return null;
  if (vm.k8s && vm.k8s.kind) return {ctx: vm.k8s.ctx || vm.ctx || LOCAL_CTX, ns: vm.k8s.namespace || undefined, kind: vm.k8s.kind, name: vm.k8s.name};
  const kind = VM_CR_KIND[vm.kind];
  if (!kind) return null;
  return {ctx: vm.ctx || LOCAL_CTX, ns: vm.namespace || undefined, kind, name: vm.name || vm.pvName};
};
// an object key names the context too, so a deep link lands on the right cluster
const objKey = (ctx, ns, kind, name) => `${ctx || LOCAL_CTX}/${ns || ""}/${kind}/${name}`;
const parseObjKey = key => { const [ctx, ns, kind, ...rest] = String(key).split("/"); return {ctx: ctx || LOCAL_CTX, ns: ns || undefined, kind, name: rest.join("/")}; };

// ---- editing ------------------------------------------------------------------
function useSpecPatch(ref, onDone) {
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState(null);
  const save = async (path, value, schema) => {
    setErr(null);
    if (isLocked(schema, path)) {
      setErr(lockOf(schema, path) === "immutable" ? `${path} is immutable once set; the API server would refuse the change`
        : `${path} is set by the operator; a change made here would be overwritten`);
      return false;
    }
    const patch = setPath({}, path, value);
    setBusy(true);
    try {
      await k8s.patch(ref.kind, ref.name, patch, {namespace: ref.ns, ctx: ref.ctx, dryRun: "All"});
      await k8s.patch(ref.kind, ref.name, patch, {namespace: ref.ns, ctx: ref.ctx});
      if (window.invalidateWorld) invalidateWorld();
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
  if (type === "kv") { const o = {}; String(raw).split(",").map(s => s.trim()).filter(Boolean).forEach(p => { const [k, v] = p.split("="); o[k.trim()] = (v || "").trim(); }); return o; }
  return raw;
};
const editable = (type, value) => type === "list" ? (Array.isArray(value) ? value.join(", ") : "") : type === "kv" ? Object.entries(value || {}).map(([k, v]) => `${k}=${v}`).join(", ") : value === undefined || value === null ? "" : String(value);

function InlineField({r, value, lock, patch, schema}) {
  const [editing, setEditing] = React.useState(false);
  const [draft, setDraft] = React.useState("");
  const shown = r.fmt ? r.fmt(value) : value;
  const opts = r.options || (schema && schema.enums[r.path]) || null;
  const badge = lock || (r.mutable === false ? "immutable" : null);
  const desc = (schema && schema.descriptions[r.path]) || r.path;
  if (!editing) {
    return (
      <span className="ifield" title={desc}>
        <span>{shown === null || shown === undefined || shown === "" ? <span style={{color: "var(--dim2)"}}>unset</span> : String(shown)}</span>
        {badge ? <span className="immut" title={LOCK_TITLE[badge]}>{badge}</span>
          : <button className="pen" title="edit" onClick={() => { setDraft(editable(r.type, value)); setEditing(true); patch.clear(); }}><Icon n="gauge" s={11} /></button>}
      </span>
    );
  }
  const commit = async () => { const ok = await patch.save(r.path, coerce(r.type === "select" ? "text" : r.type, r.type === "bool" ? draft === "true" : draft), schema); if (ok) setEditing(false); };
  return (
    <span className="ifield editing">
      {r.type === "bool" ? <select className="sel finput" value={String(draft)} onChange={e => setDraft(e.target.value)}><option value="true">true</option><option value="false">false</option></select>
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
  if (og === undefined || og === null) return <div className="mdesc" style={{marginBottom: 8}}>This kind reports no status.observedGeneration, so a stale status cannot be told from a current one.</div>;
  if (g === og) return null;
  return <div className="banner"><Icon n="clock" s={15} /><span><b>Status is behind the spec.</b> metadata.generation is {g}; status.observedGeneration is {og}. What status says may predate the latest edit.</span></div>;
}
// the view model in the registry that a CR name points at
const vmByCR = (kind, name, ns, ctx) => Object.values(REG).find(v => v.k8s && v.k8s.kind === kind && v.k8s.name === name && (!ns || v.k8s.namespace === ns) && (!ctx || (v.ctx || LOCAL_CTX) === ctx));
function LinkValue({r, value, obj, nav}) {
  if (!r.link || value === undefined || value === null || value === "") return null;
  const ns = obj.metadata.namespace, ctx = obj.__ctx || LOCAL_CTX;
  const go = () => {
    if (r.link === "cluster") { const c = vmByCR("StorageCluster", value, ns, ctx); if (c) nav.openCluster(c.id); else nav.openCR({ctx, ns, kind: "StorageCluster", name: value}); }
    else if (r.link === "node") { const n = vmByCR("StorageNode", value, getPath(obj, r.path.replace(/\.name$/, ".namespace")) || ns, ctx); if (n) nav.openNode(n.clusterId, n.id); else nav.openCR({ctx, ns, kind: "StorageNode", name: value}); }
    else if (r.link === "pool") { const p = vmByCR("StoragePool", value, ns, ctx) || Object.values(REG).find(v => v.kind === "pool" && v.name === value && (v.ctx || LOCAL_CTX) === ctx); if (p) nav.openPool(p.clusterId, p.id); else nav.openCR({ctx, ns, kind: "StoragePool", name: value}); }
    else if (r.link === "worker") nav.openCR({ctx, kind: "Node", name: value});
    else if (r.link === "storageclass") nav.openCR({ctx, kind: "StorageClass", name: value});
    else if (r.link === "pvc") nav.openCR({ctx, ns: getPath(obj, r.path.replace(/\.name$/, ".namespace")) || getPath(obj, "status.source.claimNamespace") || ns, kind: "PersistentVolumeClaim", name: value});
    else if (r.link === "pv") nav.openCR({ctx, kind: "PersistentVolume", name: value});
    else if (r.link === "vsc") nav.openCR({ctx, kind: "VolumeSnapshotContent", name: value});
  };
  return <button className="lab link" style={{marginLeft: 6}} onClick={go}><Icon n="chev" s={9} />open</button>;
}
const shown = (r, v) => { const x = r.fmt ? r.fmt(v) : v; return x === null || x === undefined || x === "" ? null : String(x); };
// " · 2 immutable paths" for the note above Essentials, nothing for none
const countOf = (n, what) => n ? ` · ${n} ${what} path${n === 1 ? "" : "s"}` : "";
function EssentialsTab({obj, cref: ref, nav, vm, onChanged}) {
  const schema = useCrdSchema(ref.kind);
  const patch = useSpecPatch(ref, onChanged);
  const h = HEADLINES[ref.kind] || {spec: [], status: []};
  const st = obj.status || {};
  // a spec row is shown even when unset (it can be set); a status row only when the operator wrote it
  const rows = (list, isSpec) => list.filter(r => !offEssentials(schema, r.path) && (isSpec || getPath(obj, r.path) !== undefined)).map(r => {
    const v = getPath(obj, r.path);
    return [<span className="mono" style={{fontSize: 11}} title={schema.descriptions[r.path] || r.path}>{r.label}</span>,
      <span style={{display: "inline-flex", alignItems: "center", gap: 4, justifyContent: "flex-end", flexWrap: "wrap"}}>
        {isSpec ? <InlineField r={r} value={v} lock={lockOf(schema, r.path)} patch={patch} schema={schema} />
          : <span title={schema.descriptions[r.path] || r.path}>{shown(r, v) === null ? <span style={{color: "var(--dim2)"}}>unset</span> : shown(r, v)}</span>}
        <LinkValue r={r} value={v} obj={Object.assign({__ctx: ref.ctx}, obj)} nav={nav} />
      </span>];
  });
  const specRows = rows(essentialSpecRows(h.spec, schema), true), statusRows = rows(h.status, false);
  const anns = (h.annotations || []).map(k => [k, (obj.metadata.annotations || {})[k]]).filter(([, v]) => v !== undefined);
  return (
    <>
      <DriftBanner obj={obj} />
      {st.activeOpsRef && <div className="banner" style={{color: "var(--info)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}><Icon n="clock" s={15} />
        <span><b>status.activeOpsRef:</b> {st.activeOpsRef}. A second operation on this object stays Pending until it finishes. {nav.monitor && <button className="chip" onClick={() => nav.monitor({cluster: vm && (vm.kind === "cluster" ? vm.id : vm.clusterId), q: st.activeOpsRef})}>Follow it</button>}</span></div>}
      <div className="mdesc" style={{marginBottom: 8}}>Field names are the CRD's. Descriptions on hover {schema.source === "crd" ? `come from the ${ref.kind} CRD (${schema.version})` : schema.loading ? "are loading" : "are unavailable: the CRD could not be read, so only the console's static immutability table applies"}{countOf(schema.immutable.size, "immutable")}{countOf(schema.managed.size, "operator-managed")}. Edits are dry-run against the API server first.</div>
      <div className="dcols">
        <div className="card"><h3>spec</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>{specRows.length ? <Props rows={specRows} /> : <div className="nolim">No headline spec fields for {ref.kind}.</div>}</div></div>
        <div>
          <div className="card"><h3>status</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>{statusRows.length ? <Props rows={statusRows} /> : <div className="nolim">No status reported yet.</div>}</div></div>
          {anns.length > 0 && <div className="card" style={{marginTop: 12}}><h3>metadata.annotations the operator reads</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={anns.map(([k, v]) => [<span className="mono" style={{fontSize: 11}}>{k}</span>, v])} /></div></div>}
          {window.ConditionsCard && (st.conditions || []).length > 0 && <div style={{marginTop: 12}}><ConditionsCard conditions={st.conditions} /></div>}
        </div>
      </div>
      {(h.tables || []).filter(t => !offEssentials(schema, t.path)).map(t => { const rowsT = getPath(obj, t.path); if (!Array.isArray(rowsT) || !rowsT.length) return null; const columns = t.columns.filter(c => !isInvisible(schema, `${t.path}.${c}`)); return (
        <div key={t.path} className="card" style={{marginTop: 12}}><h3 title={schema.descriptions[t.path] || t.path}>{t.title}</h3><div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr>{columns.map(c => <th key={c} className="mono" title={schema.descriptions[`${t.path}.${c}`] || `${t.path}[].${c}`}>{c}</th>)}</tr></thead><tbody>{rowsT.map((x, i) => <tr key={i}>{columns.map(c => <td key={c} className="mono" style={{fontSize: 11}}>{x[c] === undefined || x[c] === null ? "unset" : typeof x[c] === "object" ? JSON.stringify(x[c]) : String(x[c])}</td>)}</tr>)}</tbody></table>
        </div></div>); })}
    </>
  );
}

// ---- tier 2 ------------------------------------------------------------------------
function JsonTree({data, path, schema, depth}) {
  const [open, setOpen] = React.useState((depth || 0) < 2);
  if (data === null || data === undefined) return <span className="v" style={{color: "var(--dim2)"}}>null</span>;
  if (typeof data !== "object") return <span className="v">{typeof data === "string" ? `"${data}"` : String(data)}</span>;
  // an invisible field is left out before anything is counted, so the {n} does not give it away
  const entries = Array.isArray(data) ? data.map((v, i) => [String(i), v])
    : Object.entries(data).filter(([k]) => !(schema && isInvisible(schema, path ? `${path}.${k}` : k)));
  if (!entries.length) return <span className="v" style={{color: "var(--dim2)"}}>{Array.isArray(data) ? "[]" : "{}"}</span>;
  return (
    <div className="jt">
      <span className="tg" onClick={() => setOpen(!open)}>{open ? "▾" : "▸"} {Array.isArray(data) ? `[${entries.length}]` : `{${entries.length}}`}</span>
      {open && <div className="n">{entries.map(([k, v]) => {
        const p = path ? (Array.isArray(data) ? path : `${path}.${k}`) : k;
        const lock = !Array.isArray(data) && lockOf(schema, p);
        return <div key={k} title={schema && schema.descriptions[p] ? schema.descriptions[p] : p}><span className="k">{k}</span>{lock && <span className="immut" title={LOCK_TITLE[lock]}>{lock}</span>}: <JsonTree data={v} path={p} schema={schema} depth={(depth || 0) + 1} /></div>;
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
  const schema = useCrdSchema(ref.kind);
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
  const {data: events} = useResource("crev|" + ref.ctx + "|" + m.uid, () => k8s.list("Event", {namespace: m.namespace || "default", fieldSelector: `involvedObject.uid=${m.uid}`, ctx: ref.ctx}).catch(() => []), 10000);
  const evs = (events || []).slice().sort((a, b) => Date.parse(b.lastTimestamp || b.eventTime || 0) - Date.parse(a.lastTimestamp || a.eventTime || 0));
  const openRef = r => nav.openCR({ctx: ref.ctx, ns: m.namespace, kind: r.kind, name: r.name});
  return (
    <>
      <div className="dcols">
        <div className="card"><h3>metadata.ownerReferences</h3><div className="bd">
          {(m.ownerReferences || []).length ? (m.ownerReferences || []).map(o => <div key={o.uid} className="labels" style={{marginBottom: 6}}>
            <button className="lab link" onClick={() => openRef(o)}><Icon n="chev" s={9} />{o.kind}/{o.name}</button>{o.controller && <span className="lab"><i>controller</i>true</span>}</div>) : <div className="nolim">No owner: this object stands on its own.</div>}
        </div></div>
        <div className="card"><h3>metadata</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[
          ["name", <span className="mono">{m.name}</span>], ["namespace", m.namespace || "cluster-scoped"], ["uid", <span className="mono" style={{fontSize: 11}}>{m.uid}</span>],
          ["resourceVersion", m.resourceVersion], ["generation", m.generation], ["creationTimestamp", fmtDate(m.creationTimestamp)], ["deletionTimestamp", m.deletionTimestamp ? fmtDate(m.deletionTimestamp) : null],
          ["finalizers", (m.finalizers || []).join(", ") || null], ["context", ref.ctx]]} /></div></div>
      </div>
      <div className="dcols" style={{marginTop: 12}}>
        <div className="card"><h3>metadata.labels</h3><div className="bd"><div className="labels">{Object.entries(m.labels || {}).map(([k, v]) => <span key={k} className="lab mono" style={{fontSize: 10.5}}><i>{k}</i>{v}</span>)}{!Object.keys(m.labels || {}).length && <span className="nolim">none</span>}</div></div></div>
        <div className="card"><h3>metadata.annotations</h3><div className="bd"><div className="labels">{Object.entries(m.annotations || {}).filter(([k]) => k !== "kubectl.kubernetes.io/last-applied-configuration").map(([k, v]) => <span key={k} className="lab mono" style={{fontSize: 10.5}} title={v}><i>{k}</i>{String(v).slice(0, 60)}</span>)}{!Object.keys(m.annotations || {}).length && <span className="nolim">none</span>}</div></div></div>
      </div>
      <div className="card" style={{marginTop: 12}}><h3>Events</h3><div className="bd" style={{padding: 0}}>
        {evs.length ? <table className="dt"><thead><tr><th>type</th><th>reason</th><th>count</th><th>lastTimestamp</th><th>message</th></tr></thead><tbody>
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
        <span style={{flex: 1}}></span><span className="mono" style={{fontSize: 10.5, color: "var(--dim2)", alignSelf: "center"}}>{obj.apiVersion} {ref.kind} {ref.ns ? ref.ns + "/" : ""}{ref.name}{window.MULTI_CTX ? ` · ${ref.ctx}` : ""}</span></div>
      {tab === "essentials" && <EssentialsTab obj={obj} cref={ref} nav={nav} vm={vm} onChanged={() => setRev(r => r + 1)} />}
      {tab === "everything" && <EverythingTab obj={obj} cref={ref} />}
      {tab === "k8s" && <KubernetesTab obj={obj} cref={ref} nav={nav} />}
    </div>
  );
}
const CR_PHASE = o => (o.status || {}).phase || (o.status || {}).status || (o.status || {}).state || null;
function CRView({seg, nav}) {
  const ref = parseObjKey(seg.id);
  const {data: obj, loading, error, reload} = useCR(ref, 0);
  if (error) return <div className="scroll"><ErrorState error={error} onRetry={reload} kind={ref.kind} /></div>;
  if (loading || !obj) return <div className="scroll"><div className="lmsg">reading {ref.kind} {ref.name}…</div></div>;
  const phase = CR_PHASE(obj);
  const abortable = (window.ABORTABLE_KINDS || []).includes(obj.kind) && !["Succeeded", "Failed", "Aborted"].includes((obj.status || {}).phase);
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}>
        <div style={{display: "flex", alignItems: "center", gap: 11, flexWrap: "wrap"}}><h1>{obj.metadata.name}</h1><span className="badge">{obj.kind}</span>{phase && <span className="badge">{phase}</span>}</div>
        <div className="mdesc" style={{marginTop: 4}}>{obj.apiVersion} · {obj.metadata.namespace ? `namespace ${obj.metadata.namespace}` : "cluster-scoped"}</div></div>
        {abortable && <button className="btn" onClick={() => abortOps(obj.kind, obj.metadata.name, {namespace: ref.ns, ctx: ref.ctx}).then(() => { window.__toast("spec.abort set"); reload(); }).catch(e => window.__toast(e.message))}><Icon n="x" s={11} />Abort</button>}
      </div>
      <CRPanel cref={ref} nav={nav} vm={null} />
    </div>
  );
}
// the relation cards a curated detail adds below its own
function RelationCards({vm, nav}) {
  const rels = RELATIONS[vm.kind] || [];
  if (!rels.length) return null;
  const go = r => {
    if (r.layer) return nav.layer(vm, r.layer);
    if (r.go === "monitor") return nav.monitor ? nav.monitor({cluster: vm.kind === "cluster" ? vm.id : vm.clusterId, q: vm.kind === "cluster" ? undefined : vm.name, state: "all"}) : nav.layer(vm, "ops");
    if (r.go === "alerts") return nav.alerts ? nav.alerts({cluster: vm.kind === "cluster" ? vm.id : vm.clusterId}) : null;
  };
  return <><div className="sech" style={{marginTop: 12}}><h2>More</h2><span className="ln"></span></div>
    <div className="navcards">{rels.map(r => <NavCard key={r.title} icon={r.icon} title={r.title} sub={r.sub} count="" onClick={() => go(r)} />)}</div></>;
}
// the custom resource section every curated detail ends with
function CRSection({vm, nav}) {
  const [open, setOpen] = useLocal("sb.cr.open", true);
  const ref = refOfVm(vm);
  if (!ref) return null;
  return (
    <div style={{marginTop: 14}}>
      <div className="sech" style={{cursor: "pointer"}} onClick={() => setOpen(!open)}><h2><Icon n={open ? "chevd" : "chev"} s={11} /> {ref.kind}: spec and status</h2><span className="ln"></span><span className="mono" style={{fontSize: 10.5, color: "var(--dim2)"}}>{ref.ns ? ref.ns + "/" : ""}{ref.name}</span></div>
      {open && <CRPanel vm={vm} nav={nav} />}
      <RelationCards vm={vm} nav={nav} />
    </div>
  );
}

Object.assign(window, {CRPanel, CRView, CRSection, RelationCards, EssentialsTab, EverythingTab, KubernetesTab, JsonTree, YamlBlock, toYaml, downloadText,
  useCrdSchema, readCrdSchema, isLocked, useSpecPatch, refOfVm, objKey, parseObjKey, getPath, setPath});

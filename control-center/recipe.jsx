// ---------------------------------------------------------------------------
// RAMEN RECIPE — the common subset of ramendr.openshift.io/v1alpha1 Recipe:
//   groups            resource groups (kinds + label selector, in the app namespace)
//   hooks             exec (command in a pod) or check (condition on a resource)
//   captureWorkflow   ordered groups/hooks run when Kubernetes objects are captured
//   recoverWorkflow   ordered groups/hooks run on the standby cluster — the boot sequence
//   failOn            any-error | essential-error | full-error
// The DRPlacementControl references it via kubeObjectProtection.recipeRef.
// Advanced fields (includeClusterResources, inverseOp, essential flags per
// step, recipeParameters) are intentionally left out.
// ---------------------------------------------------------------------------
const FAIL_ON = [
  {v: "any-error", l: "any-error — stop at the first failing step"},
  {v: "essential-error", l: "essential-error — stop only when an essential step fails"},
  {v: "full-error", l: "full-error — run everything, fail at the end"}];
const HOOK_RES = ["pod", "deployment", "statefulset"];
const NS_KINDS = ["Deployment", "StatefulSet", "Service", "ConfigMap", "Secret", "Ingress", "PersistentVolumeClaim", "VirtualMachine"];
const selText = ml => Object.entries(ml || {}).map(([k, v]) => `${k}=${v}`).join(", ");
const parseSel = s => String(s || "").split(",").map(x => x.trim()).filter(Boolean).reduce((o, kv) => { const [k, ...r] = kv.split("="); if (k) o[k.trim()] = r.join("=").trim(); return o; }, {});
const stepLabel = s => s.group ? `group · ${s.group}` : `hook · ${s.hook}`;

// ---- read-only card on the protected application ----------------------------
function RecipeCard({a}) {
  const r = a.recipe;
  const hookOf = ref => { const [hn, on] = String(ref).split("/"); const h = (r.hooks || []).find(x => x.name === hn); const op = h && [].concat(h.ops || [], h.chks || []).find(o => o.name === on); return {h, op}; };
  const Seq = ({wf, title}) => (
    <div style={{marginBottom: 10}}>
      <div className="mdesc" style={{marginBottom: 6}}><b>{title}</b> · fail on <span className="mono">{wf.failOn || "any-error"}</span></div>
      {!(wf.sequence || []).length ? <p className="mdesc" style={{margin: 0}}>empty — all objects in one pass</p> : <ol className="bootseq">
        {wf.sequence.map((s, i) => {
          const g = s.group && (r.groups || []).find(x => x.name === s.group);
          const {h, op} = s.hook ? hookOf(s.hook) : {};
          return <li key={i}><span className="bootn">{i + 1}</span><div className="bootb">
            {g ? <><div className="bootl"><b>{g.name}</b><span className="bootk">{(g.includedResourceTypes || []).join(" · ") || "all kinds"}</span></div>
              <div className="bootw">{g.labelSelector && Object.keys(g.labelSelector.matchLabels || {}).length ? selText(g.labelSelector.matchLabels) : "every object in the namespace"}</div></>
            : h ? <><div className="bootl"><b>{h.name}/{op ? op.name : "?"}</b><span className="bootk">{h.type} hook · {h.selectResource} {selText(h.labelSelector && h.labelSelector.matchLabels)}</span></div>
              <div className="boothook"><i>{h.type}</i>{op ? (h.type === "exec" ? op.command : op.condition) : ""}{op && op.timeout ? ` · ≤${op.timeout}s` : ""}{op && op.onError ? ` · on error: ${op.onError}` : ""}</div></>
            : <div className="bootl"><b style={{color: "var(--bad)"}}>{stepLabel(s)} — not defined</b></div>}
          </div></li>;
        })}
      </ol>}
    </div>
  );
  return (
    <div className="card" style={{marginTop: 12}}><h3>Recipe · recover and capture workflows</h3><div className="bd" style={{paddingTop: 6}}>
      {!a.kubeObjectProtection && <div className="fnote" style={{marginBottom: 8}}><Icon n="alert" s={12} />Kubernetes object protection is off — only the PVCs fail over; the recipe is stored but not executed.</div>}
      {!r ? <p className="mdesc" style={{marginBottom: 0}}>No Recipe referenced: Ramen captures and restores all namespace objects in one pass, without ordering or hooks. Edit the recipe from Actions.</p> : <>
        <div className="labels" style={{marginBottom: 10}}>
          <span className="lab"><i>recipeRef</i>{r.namespace}/{r.name}</span>
          {r.appType && <span className="lab"><i>appType</i>{r.appType}</span>}
          <span className="lab"><i>groups</i>{(r.groups || []).length}</span><span className="lab"><i>hooks</i>{(r.hooks || []).length}</span>
        </div>
        <Seq wf={r.recoverWorkflow || {}} title="Recover workflow — boot sequence on the standby cluster" />
        <Seq wf={r.captureWorkflow || {}} title="Capture workflow — before Kubernetes objects are backed up" />
        <div className="fnote" style={{marginBottom: 0}}><Icon n="alert" s={12} />Objects of kinds not covered by any group are restored after the last step. Ramen reads the Recipe from the application namespace on both managed clusters.</div>
      </>}
    </div></div>
  );
}

// ---- the editor (a form field type) ----------------------------------------
function RecipeField({f, val, setVal}) {
  const r = val || {};
  const set = patch => setVal(Object.assign({}, r, patch));
  const groups = r.groups || [], hooks = r.hooks || [];
  const [disc, setDisc] = useState(null);   // {kind: {items|error|loading}}
  const [tab, setTab] = useState("recover");

  const discover = () => {
    const init = {}; NS_KINDS.forEach(k => { init[k] = {loading: true}; });
    setDisc(init);
    api.nsResourcesEach(f.namespace, (kind, items, error) => setDisc(d => Object.assign({}, d, {[kind]: {items: items || [], error}})));
  };
  const commonLabels = items => {
    const keys = ["app.kubernetes.io/name", "app.kubernetes.io/instance", "app"];
    for (const k of keys) { const vals = [...new Set(items.map(i => (i.metadata.labels || {})[k]).filter(Boolean))]; if (vals.length === 1) return {[k]: vals[0]}; }
    return {};
  };
  const addGroupFromKind = (kind, items) => {
    const name = kind.toLowerCase() + "s";
    if (groups.some(g => g.name === name)) return;
    set({groups: groups.concat({name, type: "resource", includedResourceTypes: [kind], labelSelector: {matchLabels: commonLabels(items)}}),
      recoverWorkflow: Object.assign({failOn: "any-error"}, r.recoverWorkflow, {sequence: ((r.recoverWorkflow || {}).sequence || []).concat({group: name})})});
  };
  const setG = (i, patch) => set({groups: groups.map((g, j) => j === i ? Object.assign({}, g, patch) : g)});
  const setH = (i, patch) => set({hooks: hooks.map((h, j) => j === i ? Object.assign({}, h, patch) : h)});
  const opOf = h => (h.type === "check" ? (h.chks || [])[0] : (h.ops || [])[0]) || {};
  const setOp = (i, patch) => { const h = hooks[i]; const cur = opOf(h); const op = Object.assign({name: cur.name || "run", timeout: 300, onError: "fail"}, cur, patch);
    setH(i, h.type === "check" ? {chks: [op], ops: []} : {ops: [op], chks: []}); };
  const wf = r[tab + "Workflow"] || {failOn: "any-error", sequence: []};
  const setWf = patch => set({[tab + "Workflow"]: Object.assign({}, wf, patch)});
  const seq = wf.sequence || [];
  const stepOptions = [...groups.map(g => ({v: "group:" + g.name, l: `group · ${g.name}`})),
    ...hooks.flatMap(h => { const ops = [].concat(h.ops || [], h.chks || []); return (ops.length ? ops : [{name: "run"}]).map(o => ({v: "hook:" + h.name + "/" + (o.name || "run"), l: `hook · ${h.name}/${o.name || "run"}`})); })];
  const stepKey = s => s.group ? "group:" + s.group : "hook:" + s.hook;
  const fromKey = k => k.startsWith("group:") ? {group: k.slice(6)} : {hook: k.slice(5)};
  const move = (i, d) => { const j = i + d; if (j < 0 || j >= seq.length) return; const n = seq.slice(); [n[i], n[j]] = [n[j], n[i]]; setWf({sequence: n}); };
  const Row = ({children, className}) => <div className={"schedrow rcp" + (className ? " " + className : "")}>{children}</div>;
  const Del = ({onClick}) => <button type="button" className="kebab" title="Remove" onClick={onClick}><Icon n="x" s={11} /></button>;

  return (
    <div className="field">
      <div className="frow" style={{marginBottom: 6}}>
        <label className="fl sm">Recipe name<input className="finput sm" value={r.name || ""} onChange={e => set({name: e.target.value})} placeholder="postgres-recipe" /></label>
        <label className="fl sm">Namespace<input className="finput sm" value={f.namespace} disabled /></label>
        <label className="fl sm">appType<input className="finput sm" value={r.appType || ""} onChange={e => set({appType: e.target.value})} placeholder="postgres" /></label>
      </div>

      <div className="rcph"><span className="flabel">Resources in {f.namespace}</span>
        <button type="button" className="chip" onClick={discover}><Icon n="search" s={11} />{disc ? "Re-discover" : "Discover via Kubernetes API"}</button></div>
      {disc && <div className="rcpdisc">
        {NS_KINDS.map(k => { const d = disc[k] || {}; return <div key={k} className="rcpkind">
          <b>{k}</b>
          {d.loading ? <span className="dots"><i></i><i></i><i></i></span> : d.error ? <span className="mdesc" style={{margin: 0, color: "var(--warn)"}}>not available</span>
            : !d.items.length ? <span className="mdesc" style={{margin: 0}}>none</span>
            : <><span className="mono" style={{fontSize: 11, color: "var(--dim)"}}>{d.items.map(i => i.metadata.name).join(", ")}</span>
              {k !== "PersistentVolumeClaim" && <button type="button" className="chip" disabled={groups.some(g => g.name === k.toLowerCase() + "s")} onClick={() => addGroupFromKind(k, d.items)}><Icon n="plus" s={10} />group</button>}</>}
        </div>; })}
        <span className="fhint">PVCs are protected through the VRG's PVC selector, not a recipe group. Adding a group appends it to the recover workflow.</span>
      </div>}

      <div className="rcph"><span className="flabel">Groups <em>({groups.length})</em></span>
        <button type="button" className="chip" onClick={() => set({groups: groups.concat({name: "", type: "resource", includedResourceTypes: [], labelSelector: {matchLabels: {}}})})}><Icon n="plus" s={11} />Add group</button></div>
      <div className="schedbox">
        <Row><span className="sl">name</span><span className="sl">resource kinds</span><span className="sl">label selector</span><span></span></Row>
        {groups.map((g, i) => <Row key={i}>
          <input className="finput sm" placeholder="config" value={g.name} onChange={e => setG(i, {name: e.target.value})} />
          <input className="finput sm" placeholder="Secret, ConfigMap" value={(g.includedResourceTypes || []).join(", ")} onChange={e => setG(i, {includedResourceTypes: e.target.value.split(",").map(x => x.trim()).filter(Boolean)})} />
          <input className="finput sm mono" placeholder="app.kubernetes.io/name=postgres" value={selText((g.labelSelector || {}).matchLabels)} onChange={e => setG(i, {labelSelector: {matchLabels: parseSel(e.target.value)}})} />
          <Del onClick={() => set({groups: groups.filter((_, j) => j !== i)})} />
        </Row>)}
        {!groups.length && <span className="fhint">No groups: every object in the namespace is one implicit group.</span>}
      </div>

      <div className="rcph"><span className="flabel">Hooks <em>({hooks.length})</em></span>
        <button type="button" className="chip" onClick={() => set({hooks: hooks.concat({name: "", type: "exec", selectResource: "pod", labelSelector: {matchLabels: {}}, ops: [{name: "run", command: "", timeout: 300, onError: "fail"}], chks: []})})}><Icon n="plus" s={11} />Add hook</button></div>
      <div className="schedbox">
        <Row className="hk"><span className="sl">name</span><span className="sl">type</span><span className="sl">on</span><span className="sl">label selector</span><span className="sl">command / condition</span><span className="sl">timeout s</span><span className="sl">on error</span><span></span></Row>
        {hooks.map((h, i) => { const op = opOf(h); return <div className="schedrow rcp hk" key={i}>
          <input className="finput sm" placeholder="quiesce" value={h.name} onChange={e => setH(i, {name: e.target.value})} />
          <select className="finput sm" value={h.type} onChange={e => { const t = e.target.value; setH(i, {type: t, ops: t === "exec" ? [Object.assign({name: "run", timeout: 300, onError: "fail"}, op, {condition: undefined})] : [], chks: t === "check" ? [Object.assign({name: "ready", timeout: 300, onError: "fail"}, op, {command: undefined})] : []}); }}>
            <option value="exec">exec</option><option value="check">check</option></select>
          <select className="finput sm" value={h.selectResource || "pod"} onChange={e => setH(i, {selectResource: e.target.value})}>{HOOK_RES.map(x => <option key={x}>{x}</option>)}</select>
          <input className="finput sm mono" placeholder="app=postgres" value={selText((h.labelSelector || {}).matchLabels)} onChange={e => setH(i, {labelSelector: {matchLabels: parseSel(e.target.value)}})} />
          <input className="finput sm mono" placeholder={h.type === "exec" ? "psql -c CHECKPOINT" : "{$.status.readyReplicas} == {$.spec.replicas}"} value={h.type === "exec" ? (op.command || "") : (op.condition || "")} onChange={e => setOp(i, h.type === "exec" ? {command: e.target.value} : {condition: e.target.value})} />
          <input className="finput sm" type="number" min="1" value={op.timeout || 300} onChange={e => setOp(i, {timeout: Number(e.target.value)})} />
          <select className="finput sm" value={op.onError || "fail"} onChange={e => setOp(i, {onError: e.target.value})}><option value="fail">fail</option><option value="continue">continue</option></select>
          <Del onClick={() => set({hooks: hooks.filter((_, j) => j !== i)})} />
        </div>; })}
        {!hooks.length && <span className="fhint">exec runs a command in the selected pods; check waits for a condition on the selected resource.</span>}
      </div>

      <div className="rcph">
        <div className="seg" style={{height: 26}}><button type="button" className={tab === "recover" ? "on" : ""} onClick={() => setTab("recover")}>Recover workflow</button><button type="button" className={tab === "capture" ? "on" : ""} onClick={() => setTab("capture")}>Capture workflow</button></div>
        <select className="finput sm" style={{width: "auto", flex: "none"}} value={wf.failOn || "any-error"} onChange={e => setWf({failOn: e.target.value})}>{FAIL_ON.map(o => <option key={o.v} value={o.v}>{o.l}</option>)}</select>
        <button type="button" className="chip" disabled={!stepOptions.length} onClick={() => setWf({sequence: seq.concat(fromKey(stepOptions[0].v))})}><Icon n="plus" s={11} />Add step</button>
      </div>
      <div className="schedbox">
        {seq.map((s, i) => <div className="schedrow rcp sq" key={i}>
          <span className="sl mono">{i + 1}</span>
          <select className="finput sm" value={stepKey(s)} onChange={e => setWf({sequence: seq.map((x, j) => j === i ? fromKey(e.target.value) : x)})}>
            {!stepOptions.some(o => o.v === stepKey(s)) && <option value={stepKey(s)}>{stepLabel(s)} (undefined)</option>}
            {stepOptions.map(o => <option key={o.v} value={o.v}>{o.l}</option>)}</select>
          <span style={{display: "flex", gap: 2}}>
            <button type="button" className="kebab" disabled={i === 0} onClick={() => move(i, -1)}><Icon n="chevu" s={11} /></button>
            <button type="button" className="kebab" disabled={i === seq.length - 1} onClick={() => move(i, 1)}><Icon n="chevd" s={11} /></button>
            <Del onClick={() => setWf({sequence: seq.filter((_, j) => j !== i)})} />
          </span>
        </div>)}
        {!seq.length && <span className="fhint">{tab === "recover" ? "Empty: objects are restored in one pass, no ordering." : "Empty: objects are captured as they are, no quiesce."}</span>}
      </div>
    </div>
  );
}

Object.assign(window, {RecipeCard, RecipeField, FAIL_ON});

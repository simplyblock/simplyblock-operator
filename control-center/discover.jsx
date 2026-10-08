// ---------------------------------------------------------------------------
// AI-ASSISTED DISCOVERY — dr-hub ADR 0023 (design: simplyblock-dr
// docs/design/ai-assisted-discovery.md, sections 7 and 10).
//
// dr-hub builds a dependency graph per site from what the sites' dr-agents
// report (DiscoveryGraph; its nodes, edges and evidence in compressed
// ConfigMap shards in dr-hub's namespace), derives one bundle per application
// (DRProposal) and runs discoveries on request (DiscoveryRun). Approval is a
// GitOps pull request: merging approves, reverting rolls back. The console
// shows the graph, the candidates and the bundles, starts runs, and asks
// dr-hub for a pull request or a rejection; only when no GitOps target is
// configured does it ask for an approval or a rollback itself. dr-hub owns
// every status: a request is an annotation on the bundle, admitted by
// dr-hub's webhook against the caller's RBAC (approve / rollback verbs).
// Questions are answered in the pull request (task-list items) and are
// read-only here.
// ---------------------------------------------------------------------------
const DISC_ANN = {request: "dr.simplyblock.io/request", reason: "dr.simplyblock.io/request-reason"};
const PROP_REQUESTS = {
  "open-pr": {label: "Open pull request", done: "dr-hub opens the pull request"},
  reject: {label: "Reject", done: "Rejected — dr-hub closes the pull request"},
  approve: {label: "Approve", done: "Approved — dr-hub applies the bundle"},
  rollback: {label: "Roll back", done: "Rollback requested — dr-hub restores the previous objects and labels"}
};
// A bundle is final once it can change no more; Applied can still roll back.
const PROP_FINAL = ["Rejected", "Superseded", "RolledBack"];
const RUN_FINAL = ["Succeeded", "Failed", "BudgetExceeded", "Cancelled"];

[["Proposed", "var(--info)", 1, "proposed"], ["PROpened", "var(--accent)", 1, "PR open"], ["Merged", "var(--info)", 1, "merged", true],
  ["WaitingForApplications", "var(--info)", 1, "waiting for its applications", true], ["Applied", "var(--ok)", 0, "applied"],
  ["Rejected", "var(--dim2)", 2, "rejected"], ["Superseded", "var(--dim2)", 2, "superseded"], ["Stale", "var(--warn)", 3, "stale"],
  ["BudgetExceeded", "var(--warn)", 3, "budget exceeded"], ["Cancelled", "var(--dim2)", 2, "cancelled"]]
  .forEach(([k, c, rank, label, blink]) => { if (!STATUS_META[k]) STATUS_META[k] = Object.assign({c, rank, label}, blink ? {blink: true} : {}); });

const confPct = n => n == null || isNaN(n) ? "—" : `${Math.round(Number(n) / 10)}%`;
const discMeta = o => o.metadata || {};
const discBase = (o, kind) => ({
  kind, id: discMeta(o).uid || `${kind}:${discMeta(o).namespace || ""}/${discMeta(o).name}`, name: discMeta(o).name, namespace: discMeta(o).namespace || "",
  createdAt: discMeta(o).creationTimestamp, labels: discMeta(o).labels || {}, annotations: discMeta(o).annotations || {},
  conditions: (((o.status || {}).conditions) || []).map(c => ({type: c.type, status: c.status, reason: c.reason, message: c.message, since: c.lastTransitionTime})), raw: o
});

// ---- normalizers ------------------------------------------------------------------
function normDGraph(o) {
  const sp = o.spec || {}, st = o.status || {}, c = st.counts || {};
  const site = sp.site || discMeta(o).name;
  return REG_put(Object.assign(discBase(o, "dgraph"), {
    site, built: st.built || null, observedReport: st.observedReport || "", shards: st.shards || [],
    candidates: (st.candidates || []).map(x => ({id: x.id, name: x.name || x.id, namespaces: x.namespaces || [], members: x.members || [], score: x.score || 0, adopted: x.adopted || ""})),
    interApp: st.interApp || [],
    counts: {nodes: c.nodes || 0, edges: c.edges || 0, evidence: c.evidence || 0, candidates: c.candidates || (st.candidates || []).length},
    truncated: c.truncated || [],
    status: st.built ? ((st.conditions || []).some(x => x.status === "False") ? "Degraded" : "Ready") : "Pending"
  }));
}

function normDRProp(o) {
  const sp = o.spec || {}, st = o.status || {};
  const objects = (sp.objects || []).map(x => ({apiVersion: x.apiVersion, kind: x.kind, name: x.name, namespace: x.namespace || "", operation: x.operation || "create",
    spec: x.spec || {}, fields: Object.entries(x.fields || {}).map(([f, b]) => ({field: f, evidence: b.evidence || [], confidence: b.confidence || 0, note: b.note || ""}))}));
  const target = objects.find(x => x.kind === "ProtectedApplication") || objects.find(x => x.kind === "RecoveryPlan") || objects[0] || null;
  const answers = Object.fromEntries((st.answers || []).map(a => [a.question, a]));
  const questions = (sp.questions || []).map(q => Object.assign({id: q.id, text: q.text, options: q.options || [], field: q.field || "", blocking: !!q.blocking},
    answers[q.id] ? {answer: answers[q.id].option, by: answers[q.id].by || ""} : {}));
  const dry = st.dryRun || null;
  const checks = (dry && dry.checks) || [];
  const phase = st.phase || "Proposed";
  const source = sp.source || "rules";
  const ann = discMeta(o).annotations || {};
  const evidence = new Set();
  objects.forEach(x => x.fields.forEach(f => f.evidence.forEach(e => evidence.add(e))));
  (sp.labels || []).forEach(l => (l.evidence || []).forEach(e => evidence.add(e)));
  return REG_put(Object.assign(discBase(o, "drprop"), {
    scope: sp.scope || "Application", site: sp.site || "", candidate: sp.candidate || "", source, ai: source.indexOf("ai:") === 0,
    run: source.indexOf("ai:") === 0 ? source.slice(3) : "", baseline: sp.baseline || "",
    title: sp.candidate || (target ? target.name : discMeta(o).name), target, objects,
    labels: (sp.labels || []).map(l => ({cluster: l.cluster, change: l.change || {}, reason: l.reason || "", evidence: l.evidence || [], effect: l.effect || ""})),
    migrations: (sp.migrations || []).map(m => ({cluster: m.cluster, namespace: m.namespace, pvc: m.pvc, group: m.group, reason: m.reason || ""})),
    dependsOn: sp.dependsOn || [], questions, summary: sp.summary || "", confidence: sp.confidence || 0,
    openQuestions: questions.filter(q => !q.answer).length, openBlocking: questions.filter(q => q.blocking && !q.answer).length,
    dryRun: dry ? {verdict: dry.verdict || "Unknown", checks} : null,
    dryBlocking: checks.filter(c => c.blocking && c.status === "Fail").length,
    diff: st.diff || "", gitOps: st.gitOps || null, approvedBy: st.approvedBy || "", appliedAt: st.appliedAt || null,
    supersededBy: st.supersededBy || "", labelRequests: st.labelRequests || [], evidenceIds: [...evidence],
    request: ann[DISC_ANN.request] || "", requestReason: ann[DISC_ANN.reason] || "",
    phase, status: phase, final: PROP_FINAL.includes(phase)
  }));
}

function normDRun(o) {
  const sp = o.spec || {}, st = o.status || {}, sc = sp.scope || {}, u = st.usage || {};
  const phase = st.phase || "Pending";
  return REG_put(Object.assign(discBase(o, "drun"), {
    site: sc.site || "", namespaces: sc.namespaces || [], proposal: sc.proposal || "", mode: sp.mode || "Rules", provider: sp.provider || "",
    instructions: sp.instructions || "", phase, status: phase, final: RUN_FINAL.includes(phase), progress: st.progress || "",
    started: st.started || null, completed: st.completed || null,
    usage: {input: u.inputTokens || 0, output: u.outputTokens || 0, toolCalls: u.toolCalls || 0, cost: u.costEstimate || ""},
    toolLog: (st.toolLog || []).map(t => ({time: t.time, tool: t.tool, summary: t.summary || "", result: t.result || ""})),
    proposals: st.proposals || [], rejected: st.rejected || []
  }));
}
function REG_put(vm) { REG[vm.id] = vm; return vm; }

// ---- graph data: gzip+JSON ConfigMap shards ---------------------------------------
// Shard i of n carries annotations shard-index / shard-count / shard-generation
// (sha256 of the uncompressed JSON) and binaryData["data.gz"]; the gzip stream
// is the concatenation of the chunks in index order (internal/shard).
const SHARD_ANN = {gen: "dr.simplyblock.io/shard-generation", index: "dr.simplyblock.io/shard-index", count: "dr.simplyblock.io/shard-count"};
const GRAPH_CACHE = {};
const b64bytes = s => { const bin = atob(s || ""); const out = new Uint8Array(bin.length); for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i); return out; };
async function gunzipText(bytes) {
  if (typeof DecompressionStream === "undefined") throw new Error("this browser cannot decompress the graph data (no DecompressionStream)");
  const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip"));
  return await new Response(stream).text();
}
async function sha256hex(text) {
  if (!(window.crypto && crypto.subtle)) return "";
  const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
  return Array.from(new Uint8Array(d)).map(b => b.toString(16).padStart(2, "0")).join("");
}
async function loadGraphData(g) {
  if (!g || !g.shards.length) return {nodes: [], edges: [], evidence: [], missing: true};
  const key = `${g.site}|${g.shards.join(",")}|${g.observedReport}`;
  if (GRAPH_CACHE[key]) return GRAPH_CACHE[key];
  const ns = DR_HUB_NS();
  let cms;
  try { cms = await Promise.all(g.shards.map(n => k8s.get("ConfigMap", n, {namespace: ns}))); }
  catch (e) {
    if (e && e.status === 403) throw new Error(`Reading the graph data needs get on configmaps in ${ns} (the shards ${g.shards.join(", ")}).`);
    if (e && e.status === 404) throw new Error(`A shard of the graph is gone (being rewritten?) — ${e.message}`);
    throw e;
  }
  const ann = cm => (cm.metadata && cm.metadata.annotations) || {};
  const gen = ann(cms[0])[SHARD_ANN.gen], count = Number(ann(cms[0])[SHARD_ANN.count]);
  if (count !== cms.length) throw new Error(`${cms.length} of ${count} graph shards present — dr-hub is rewriting the graph; reload in a moment.`);
  if (cms.some(cm => ann(cm)[SHARD_ANN.gen] !== gen)) throw new Error("The graph shards belong to different writes — reload in a moment.");
  const parts = new Array(count);
  cms.forEach(cm => { parts[Number(ann(cm)[SHARD_ANN.index])] = b64bytes(((cm.binaryData || {})["data.gz"]) || ""); });
  if (parts.some(p => !p)) throw new Error("A graph shard has a bad index.");
  const all = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let off = 0; parts.forEach(p => { all.set(p, off); off += p.length; });
  const text = await gunzipText(all);
  if (gen) { const h = await sha256hex(text); if (h && h !== gen) throw new Error("The graph data does not match its generation — reload in a moment."); }
  const d = JSON.parse(text);
  const out = {nodes: d.nodes || [], edges: d.edges || [], evidence: d.evidence || [], generation: gen};
  out.byId = Object.fromEntries(out.nodes.map(n => [n.id, n]));
  out.evById = Object.fromEntries(out.evidence.map(e => [e.id, e]));
  GRAPH_CACHE[key] = out;
  return out;
}

// ---- reads and writes ----------------------------------------------------------------
const discList = (kind, norm) => k8s.list(kind, RESOURCES[kind].namespaced ? {allNamespaces: true} : {}).then(xs => xs.map(norm));
const discById = (kind, norm, label) => id => discList(kind, norm).then(xs => { const hit = xs.find(x => x.id === id); if (!hit) throw new ApiError(404, `${label} not found`, kind, "NotFound"); return hit; });
const newestFirst = (a, b) => Date.parse(b.createdAt || 0) - Date.parse(a.createdAt || 0);
const discovery = {
  graphs: () => discList("DiscoveryGraph", normDGraph),
  graph: discById("DiscoveryGraph", normDGraph, "DiscoveryGraph"),
  graphBySite: site => discovery.graphs().then(gs => gs.find(g => g.site === site) || null),
  graphData: loadGraphData,
  proposals: () => discList("DRProposal", normDRProp).then(xs => xs.sort(newestFirst)),
  proposal: discById("DRProposal", normDRProp, "DRProposal"),
  runs: () => discList("DiscoveryRun", normDRun).then(xs => xs.sort(newestFirst)),
  run: discById("DiscoveryRun", normDRun, "DiscoveryRun"),
  // DRConfig.spec.discovery: GitOps target (nil: console approval fallback),
  // model providers (none: AI runs not available), flow opt-outs.
  config: () => drhub.configs().then(cs => {
    const c = cs.find(x => x.name === "default") || cs[0] || null;
    const d = (c && c.raw && c.raw.spec && c.raw.spec.discovery) || {};
    const agents = (c && c.raw && c.raw.status && c.raw.status.agents) || [];
    return {present: !!c, enabled: !!d.enabled, gitOps: d.gitOps || null, providers: d.providers || [], defaultProvider: d.defaultProvider || "",
      flows: d.flows || {}, optOut: ((d.flows || {}).optOut) || [], agents};
  }).catch(() => ({present: false, enabled: false, gitOps: null, providers: [], defaultProvider: "", flows: {}, optOut: [], agents: []})),
  // the newest bundle that would create or change an object (application,
  // recovery plan, site profile) — the forms link to it
  latestFor: (kind, name, namespace) => discovery.proposals().then(ps => ps.find(p => !p.final && p.objects.some(x =>
    x.kind === kind && x.name === name && (!namespace || !x.namespace || x.namespace === namespace))) || null).catch(() => null),
  startRun: ({site, namespaces, proposal, mode, provider, instructions, maxToolCalls, maxDuration}) => k8s.create("DiscoveryRun", {
    apiVersion: DR_API_GROUP, kind: "DiscoveryRun",
    metadata: {name: dns63(`discovery-${site}-${Date.now().toString(36)}`), namespace: DR_NS()},
    spec: Object.assign({scope: Object.assign({site}, namespaces && namespaces.length ? {namespaces} : {}, proposal ? {proposal} : {}), mode},
      mode === "AI" && provider ? {provider} : {}, mode === "AI" && instructions ? {instructions} : {},
      mode === "AI" && (maxToolCalls || maxDuration) ? {budget: Object.assign({}, maxToolCalls ? {maxToolCalls: Number(maxToolCalls)} : {}, maxDuration ? {maxDuration} : {})} : {})
  }, {namespace: DR_NS()}),
  // A request dr-hub carries out and records in the bundle's status.
  request: (p, action, reason) => k8s.patch("DRProposal", p.name,
    {metadata: {annotations: {[DISC_ANN.request]: action, [DISC_ANN.reason]: reason || null}}}, {namespace: p.namespace}),
  cancelRun: r => k8s.remove("DiscoveryRun", r.name, {namespace: r.namespace})
};
Object.assign(GETTER, {dgraph: discovery.graph, drprop: discovery.proposal, drun: discovery.run});

// ---- small pieces -----------------------------------------------------------------------
const Table_ = (p) => React.createElement(window.DrTable, p);
const M = ({children, dim}) => React.createElement(window.DrMono, {dim}, children);
const Conf = ({v}) => {
  const n = Math.max(0, Math.min(1000, Number(v) || 0));
  const c = n >= 800 ? "var(--ok)" : n >= 500 ? "var(--warn)" : "var(--bad)";
  return <span className="confbar" title={`confidence ${confPct(n)}`} style={{display: "inline-flex", alignItems: "center", gap: 6, whiteSpace: "nowrap"}}>
    <span style={{display: "inline-block", width: 42, height: 5, borderRadius: 3, background: "var(--line)", overflow: "hidden"}}>
      <span style={{display: "block", width: `${n / 10}%`, height: "100%", background: c}}></span></span>
    <span className="mono" style={{fontSize: 11}}>{confPct(n)}</span></span>;
};
const SourceBadge = ({p}) => p.ai
  ? <span className="badge" style={{color: "var(--accent)", borderColor: "color-mix(in srgb,var(--accent) 45%,transparent)"}} title={`AI run ${p.run}`}>AI · {p.run}</span>
  : <span className="badge">rules</span>;
const PRLink = ({g}) => !g ? <span style={{color: "var(--dim2)"}}>—</span>
  : g.pr ? <a href={g.pr} target="_blank" rel="noopener noreferrer" className="mono" style={{color: "var(--accent)"}}>{g.pr.replace(/^https?:\/\/[^/]+\//, "")}{g.state ? ` · ${g.state}` : ""}</a>
  : <M dim>{g.branch || "—"}</M>;
const QCount = ({p}) => !p.questions.length ? <span style={{color: "var(--dim2)"}}>—</span>
  : <span title={p.questions.filter(q => !q.answer).map(q => q.text).join("\n")}>{p.openQuestions}/{p.questions.length} open{p.openBlocking ? <b style={{color: "var(--bad)"}}> · {p.openBlocking} blocking</b> : null}</span>;
const DryVerdict = ({p}) => !p.dryRun ? <span style={{color: "var(--dim2)"}}>not run</span>
  : <span style={{display: "inline-flex", gap: 6, alignItems: "center"}}><VerdictBadge v={p.dryRun.verdict} sm />{p.dryBlocking ? <b style={{color: "var(--bad)", fontSize: 11}}>{p.dryBlocking} blocking</b> : null}</span>;
const PhaseCell = ({p}) => <span style={{display: "inline-flex", gap: 6, alignItems: "center", flexWrap: "wrap"}}><TrafficLight status={p.phase} sm />
  {p.request && !p.final ? <span className="badge" title="requested from the console; dr-hub carries it out">{p.request} requested</span> : null}</span>;

function ProposalTable({props, nav, empty}) {
  return <Table_ cols={["Bundle", "Site", "Scope", "Source", "Confidence", "Phase", "Dry run", "Questions", "Pull request"]} empty={empty || "No proposal yet."}
    rows={props.map(p => [
      <span style={{display: "inline-flex", flexDirection: "column", minWidth: 0}}><Ref label={p.title} onClick={() => nav.detail(p)} /><M dim>{p.name}</M></span>,
      <M>{p.site}</M>, <span className="badge">{p.scope}</span>, <SourceBadge p={p} />, <Conf v={p.confidence} />,
      <PhaseCell p={p} />, <DryVerdict p={p} />, <QCount p={p} />, <PRLink g={p.gitOps} />])} />;
}

function RunTable({runs, limit}) {
  const [open, setOpen] = React.useState(null);
  const rows = runs.slice(0, limit || runs.length);
  return <>
    <Table_ cols={["Run", "Site", "Mode", "Phase", "Progress", "Started", "Proposals", "Tool calls"]} empty="No discovery run yet."
      rows={rows.map(r => [
        <button className="runopen mono" style={{color: "var(--accent)", fontFamily: "inherit"}} onClick={() => setOpen(open === r.id ? null : r.id)} title="Show the run's tool log">{r.name}</button>,
        <M>{r.site}</M>, <span className="badge">{r.mode === "AI" ? `rules + AI${r.provider ? ` · ${r.provider}` : ""}` : "rules"}</span>,
        <TrafficLight status={r.phase} sm />, <span style={{color: "var(--dim)"}}>{r.progress || (r.final ? "" : "waiting for dr-hub")}</span>,
        r.started ? fmtAgo(r.started) : "", r.proposals.length ? <span>{r.proposals.length}{r.rejected.length ? <span style={{color: "var(--warn)"}}> · {r.rejected.length} rejected</span> : null}</span> : "",
        r.mode === "AI" ? `${r.usage.toolCalls}` : ""])} />
    {rows.filter(r => r.id === open).map(r => <div key={r.id} className="card runlog" style={{marginTop: 8}}><h3>Run {r.name}</h3><div className="bd">
      <Props rows={[["Scope", <M>{r.site}{r.namespaces.length ? ` · ${r.namespaces.join(", ")}` : ""}{r.proposal ? ` · bundle ${r.proposal}` : ""}</M>],
        ["Mode", r.mode], ["Completed", r.completed ? fmtDate(r.completed) : "—"],
        r.mode === "AI" ? ["Usage", `${r.usage.input} in · ${r.usage.output} out tokens${r.usage.cost ? ` · ${r.usage.cost}` : ""}`] : null,
        r.instructions ? ["Instructions", r.instructions] : null].filter(Boolean)} />
      {!!r.rejected.length && <><div className="sl" style={{margin: "8px 0 4px"}}>Outputs dr-hub rejected</div>
        <Table_ cols={["Proposal", "Reason"]} rows={r.rejected.map(x => [<M>{x.proposal || "—"}</M>, x.reason])} /></>}
      <div className="sl" style={{margin: "8px 0 4px"}}>Tool log (newest last)</div>
      <Table_ cols={["Time", "Tool", "Summary", "Result"]} empty={r.mode === "AI" ? "No tool call yet." : "A rules run calls no tools."}
        rows={r.toolLog.map(t => [fmtDate(t.time), <M>{t.tool}</M>, t.summary, <span style={{color: /^(ok|success)/i.test(t.result) ? "var(--ok)" : "var(--dim)"}}>{t.result}</span>])} />
    </div></div>)}
  </>;
}

// ---- dialogs --------------------------------------------------------------------------------
// Run discovery: rules always; rules + AI once a model provider is configured
// (phase 2 of ADR 0023 ships the agent runtime).
const runDiscoveryDialog = (sites, cfg, preset) => ({
  title: preset && preset.proposal ? `Refine ${preset.proposal} with AI` : "Run discovery", confirm: "Start run", done: "Discovery run started",
  desc: "dr-hub rebuilds the dependency graph of the site from its dr-agent's report and derives one bundle per application candidate. Nothing is applied: every bundle goes to approval.",
  fields: v => {
    const ai = (cfg.providers || []).length > 0;
    const site = v.site || (preset && preset.site) || (sites[0] || {}).name;
    const s = sites.find(x => x.name === site) || {namespaces: []};
    return [
      {k: "site", label: "Site", type: "select", required: true, def: preset && preset.site, options: sites.map(x => ({v: x.name, l: `${x.name}${x.built ? ` · graph ${fmtAgo(x.built)}` : " · no graph yet"}`}))},
      !(preset && preset.proposal) && {k: "namespaces", label: "Namespaces (none: the whole site)", type: "multiselect", options: (s.namespaces || []).map(n => ({v: n, l: n})), empty: "The site's graph lists no namespaces yet."},
      {k: "mode", label: "Mode", type: "select", def: preset && preset.mode, options: (preset && preset.mode === "AI" ? [] : [{v: "Rules", l: "rules (deterministic baseline)"}]).concat(ai ? [{v: "AI", l: "rules + AI refinement"}] : [])},
      !ai && {k: "n1", type: "note", label: "Rules + AI comes with phase 2: no model provider is configured (DRConfig spec.discovery.providers)."},
      ai && v.mode === "AI" && {k: "provider", label: "Model provider", type: "select", def: cfg.defaultProvider,
        options: cfg.providers.map(p => ({v: p.name, l: `${p.name} · ${p.type}${p.model ? ` · ${p.model}` : ""}`}))},
      ai && v.mode === "AI" && {k: "instructions", label: "Instructions for the agent (optional)", type: "text", placeholder: "e.g. treat the reporting namespace as part of the shop"},
      ai && v.mode === "AI" && {k: "maxToolCalls", label: "Tool-call budget (optional)", type: "number", min: 1},
      ai && v.mode === "AI" && {k: "n2", type: "note", label: "Only metadata leaves the hub, to the provider's model endpoint. The agent can only propose; every bundle still needs approval."}
    ].filter(Boolean);
  },
  run: v => discovery.startRun({site: v.site, namespaces: v.namespaces, proposal: preset && preset.proposal, mode: v.mode || "Rules",
    provider: v.provider, instructions: (v.instructions || "").trim(), maxToolCalls: v.maxToolCalls})
});
const rejectDialog = p => ({
  title: `Reject the bundle for ${p.title}?`, confirm: "Reject", danger: true, done: PROP_REQUESTS.reject.done,
  desc: p.gitOps && p.gitOps.pr ? `dr-hub closes ${p.gitOps.pr} with this reason. The next discovery proposes again only if the graph changes.` : "dr-hub marks the bundle Rejected with this reason. The next discovery proposes again only if the graph changes.",
  fields: [{k: "reason", label: "Reason", type: "text", required: true, placeholder: "why this bundle is wrong", validate: x => x && x.trim().length < 10 ? "at least 10 characters" : null}],
  run: v => discovery.request(p, "reject", v.reason.trim())
});
const requestDialog = (p, action) => ({
  title: `${PROP_REQUESTS[action].label}: ${p.title}?`, confirm: PROP_REQUESTS[action].label, danger: action === "rollback", done: PROP_REQUESTS[action].done,
  desc: action === "approve" ? `dr-hub applies the bundle: ${p.objects.length} object(s) and ${p.labels.length} label change(s), recorded with you as the approver. This is the approval only because no GitOps target is configured.`
    : action === "rollback" ? "dr-hub restores the objects and the labels as they were before this bundle was applied."
    : "dr-hub renders the bundle as manifests and opens a pull request in the GitOps repository. Merging it approves the bundle.",
  fields: [],
  run: () => discovery.request(p, action)
});

// ---- graph view -------------------------------------------------------------------------------
// Layered SVG: columns by node kind (external endpoints and Services left,
// workloads in the middle, volumes and networks right), edges as curves whose
// width follows the weight; click an edge or a node for its evidence.
const NODE_COLS = {External: 0, Service: 1, Workload: 2, VirtualMachine: 2, Pod: 2, PVC: 3, NAD: 4, Namespace: 5};
const NODE_C = {External: "var(--dim)", Service: "var(--info)", Workload: "var(--accent)", VirtualMachine: "var(--accent)", Pod: "var(--accent)", PVC: "var(--ok)", NAD: "var(--warn)", Namespace: "var(--dim2)"};
const EDGE_KINDS = ["owns", "mounts", "selects", "references", "connects", "attaches", "packagedWith"];
const EDGE_C = {owns: "var(--dim2)", mounts: "var(--ok)", selects: "var(--info)", references: "var(--accent)", connects: "var(--warn)", attaches: "var(--warn)", packagedWith: "var(--dim)"};
const GRAPH_CAP = 500;
const nodeLabel = n => n ? `${n.kind === "External" ? "" : (n.namespace ? n.namespace + "/" : "")}${n.name}` : "";

function scopeNodes(data, scope) {
  if (!data || !scope) return {ids: new Set(), core: new Set()};
  const core = new Set(scope.type === "candidate" ? scope.members : data.nodes.filter(n => n.namespace === scope.value).map(n => n.id));
  const ids = new Set(core);
  data.edges.forEach(e => { if (core.has(e.from)) ids.add(e.to); if (core.has(e.to)) ids.add(e.from); });
  return {ids, core};
}

function GraphView({data, scope, kinds, picked, onPick}) {
  const {ids, core} = scopeNodes(data, scope);
  if (!ids.size) return <div className="nolim">Nothing in this scope.</div>;
  if (ids.size > GRAPH_CAP) return <div className="nolim">{ids.size} nodes in this scope — more than {GRAPH_CAP}; narrow it to one candidate or namespace.</div>;
  const nodes = data.nodes.filter(n => ids.has(n.id)).sort((a, b) => (a.namespace || "").localeCompare(b.namespace || "") || a.name.localeCompare(b.name));
  const cols = {};
  nodes.forEach(n => { const c = NODE_COLS[n.kind] != null ? NODE_COLS[n.kind] : 2; (cols[c] = cols[c] || []).push(n); });
  const used = Object.keys(cols).map(Number).sort((a, b) => a - b);
  const W = 168, H = 22, GX = 64, GY = 10, PAD = 8;
  const pos = {};
  used.forEach((c, ci) => cols[c].forEach((n, ri) => { pos[n.id] = {x: PAD + ci * (W + GX), y: PAD + ri * (H + GY)}; }));
  const width = PAD * 2 + used.length * W + (used.length - 1) * GX;
  const height = PAD * 2 + Math.max(...used.map(c => cols[c].length)) * (H + GY) - GY;
  const edges = data.edges.filter(e => pos[e.from] && pos[e.to] && kinds.includes(e.kind));
  const isPicked = x => picked && picked.type === "edge" && picked.e === x;
  return (
    <div className="graphwrap" style={{overflow: "auto", maxHeight: 560, border: "1px solid var(--line)", borderRadius: 6, background: "var(--panel)"}}>
      <svg className="dgraph" width={width} height={height} style={{display: "block", fontFamily: "var(--mono, monospace)"}}>
        {edges.map((e, i) => {
          const a = pos[e.from], b = pos[e.to];
          const same = a.x === b.x;
          const x1 = same ? a.x + W : (a.x < b.x ? a.x + W : a.x), x2 = same ? b.x + W : (a.x < b.x ? b.x : b.x + W);
          const y1 = a.y + H / 2, y2 = b.y + H / 2;
          const dx = same ? 40 : (x2 - x1) / 2;
          const d = `M${x1},${y1} C${x1 + dx},${y1} ${x2 - (same ? -40 : dx)},${y2} ${x2},${y2}`;
          const w = 1 + Math.max(0, Math.min(1000, e.weight || 0)) / 1000 * 3;
          return <path key={i} d={d} fill="none" stroke={EDGE_C[e.kind] || "var(--dim)"} strokeWidth={isPicked(e) ? w + 2 : w} strokeOpacity={isPicked(e) ? 1 : 0.65}
            style={{cursor: "pointer"}} onClick={() => onPick({type: "edge", e})}>
            <title>{`${e.kind}${e.port ? ` :${e.port}` : ""} · weight ${confPct(e.weight)} · ${nodeLabel(data.byId[e.from])} → ${nodeLabel(data.byId[e.to])}`}</title></path>;
        })}
        {nodes.map(n => { const p = pos[n.id], sel = picked && picked.type === "node" && picked.n === n; return (
          <g key={n.id} transform={`translate(${p.x},${p.y})`} style={{cursor: "pointer"}} onClick={() => onPick({type: "node", n})} opacity={core.has(n.id) ? 1 : 0.55}>
            <rect width={W} height={H} rx={4} fill="var(--bg2, var(--panel))" stroke={NODE_C[n.kind] || "var(--dim)"} strokeWidth={sel ? 2.2 : 1} />
            <rect width={4} height={H} rx={2} fill={NODE_C[n.kind] || "var(--dim)"} />
            <text x={9} y={H / 2 + 4} fontSize={10.5} fill="var(--text)">{(n.role ? `${n.role} · ` : "") + nodeLabel(n)}</text>
            <title>{`${n.kind} ${nodeLabel(n)}${n.role ? ` (role ${n.role})` : ""}`}</title>
          </g>); })}
      </svg>
    </div>
  );
}

function EvidenceRows({ids, data}) {
  return <Table_ cols={["Evidence", "Source", "Object", "Field", "Detail", "Observed"]} empty="No evidence recorded."
    rows={(ids || []).map(id => { const e = data && data.evById ? data.evById[id] : null; return e
      ? [<M>{e.id}</M>, <span className="badge">{e.source}</span>, <M>{e.object}</M>, <M dim>{e.field || ""}</M>, e.detail || "", e.observed ? fmtAgo(e.observed) : ""]
      : [<M>{id}</M>, <span style={{color: "var(--dim2)"}}>not in the current graph</span>, "", "", "", ""]; })} />;
}

function PickedPanel({picked, data}) {
  if (!picked) return <p className="mdesc" style={{margin: "8px 0 0"}}>Click an edge or a node for its evidence.</p>;
  if (picked.type === "node") {
    const n = picked.n;
    return <div style={{marginTop: 8}}><Props rows={[["Node", <M>{n.kind} {nodeLabel(n)}</M>], n.role ? ["Role", n.role] : null,
      ...Object.entries(n.facts || {}).slice(0, 8).map(([k, v]) => [k, <M dim>{v}</M>]),
      Object.keys(n.labels || {}).length ? ["Labels", <M dim>{Object.entries(n.labels).map(([k, v]) => `${k}=${v}`).join(", ")}</M>] : null].filter(Boolean)} /></div>;
  }
  const e = picked.e;
  return <div style={{marginTop: 8}}>
    <Props rows={[["Edge", <M>{nodeLabel(data.byId[e.from])} → {nodeLabel(data.byId[e.to])}</M>], ["Kind", <span className="badge">{e.kind}{e.port ? ` :${e.port}` : ""}</span>], ["Weight", <Conf v={e.weight} />]]} />
    <EvidenceRows ids={e.evidence} data={data} />
  </div>;
}

function GraphCard({g, data, error, loading, scope, setScope}) {
  const [kinds, setKinds] = React.useState(EDGE_KINDS.filter(k => k !== "owns"));
  const [picked, setPicked] = React.useState(null);
  const namespaces = data ? [...new Set(data.nodes.map(n => n.namespace).filter(Boolean))].sort() : [];
  const options = g.candidates.map(c => ({v: `c:${c.id}`, l: `candidate ${c.name}`})).concat(namespaces.map(n => ({v: `n:${n}`, l: `namespace ${n}`})));
  const cur = scope ? (scope.type === "candidate" ? `c:${scope.id}` : `n:${scope.value}`) : "";
  const choose = v => {
    setPicked(null);
    if (!v) return setScope(null);
    if (v.indexOf("c:") === 0) { const c = g.candidates.find(x => x.id === v.slice(2)); setScope(c ? {type: "candidate", id: c.id, members: c.members} : null); }
    else setScope({type: "namespace", value: v.slice(2)});
  };
  return <div className="card graphcard"><h3>Dependency graph</h3><div className="bd">
    <div className="graphbar" style={{display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap", marginBottom: 8}}>
      <select className="finput sm graphscope" value={cur} onChange={e => choose(e.target.value)} style={{maxWidth: 280}}>
        <option value="">— choose a candidate or namespace —</option>
        {options.map(o => <option key={o.v} value={o.v}>{o.l}</option>)}
      </select>
      <span className="sl" style={{marginLeft: 6}}>edges</span>
      {EDGE_KINDS.map(k => <button key={k} className={"chip edgekind" + (kinds.includes(k) ? " on" : "")} style={{borderColor: EDGE_C[k], color: kinds.includes(k) ? EDGE_C[k] : "var(--dim2)"}}
        onClick={() => setKinds(kinds.includes(k) ? kinds.filter(x => x !== k) : kinds.concat([k]))}>{k}</button>)}
    </div>
    {error ? <div className="banner"><Icon n="alert" s={15} /><span><b>Graph data unavailable.</b> {error.message} The candidates above come from the graph's status and stay usable.</span></div>
      : loading ? <div className="skel" style={{height: 160}}></div>
      : !data || data.missing ? <div className="nolim">dr-hub has not written graph data for {g.site} yet.</div>
      : !scope ? <div className="nolim">Choose a candidate or a namespace to draw its graph ({data.nodes.length} nodes, {data.edges.length} edges on the site).</div>
      : <><GraphView data={data} scope={scope} kinds={kinds} picked={picked} onPick={setPicked} /><PickedPanel picked={picked} data={data} /></>}
  </div></div>;
}

// ---- discovery home (DR → Discovery) -----------------------------------------------------
// The sites: every site with a graph, plus the managed clusters and agents
// that have none yet.
const discSites = (graphs, cfg, mcs) => {
  const names = new Set(graphs.map(g => g.site).concat((cfg.agents || []).map(a => a.cluster), (mcs || []).map(m => (m.metadata || {}).name)).filter(Boolean));
  return [...names].sort().map(name => { const g = graphs.find(x => x.site === name) || null; const a = (cfg.agents || []).find(x => x.cluster === name) || null;
    return {name, graph: g, built: g ? g.built : null, agent: a, flowsOff: (cfg.optOut || []).includes(name),
      namespaces: g ? [...new Set(g.candidates.flatMap(c => c.namespaces))].sort() : []}; });
};
const flowsText = s => s.flowsOff ? "off (opted out)" : "on";
const openProps = ps => ps.filter(p => !p.final && p.phase !== "Applied");

function DiscoveryHome({nav}) {
  const r = useResource("disc.home", () => Promise.all([discovery.graphs(), discovery.proposals(), discovery.runs(), discovery.config(), drhub.managedClusters()]), 8000);
  const acc = useAccess();
  const [graphs, props, runs, cfg, mcs] = r.data || [[], [], [], {providers: [], optOut: [], agents: []}, []];
  const sites = discSites(graphs, cfg, mcs);
  const mayRun = acc.can("create", "drhub", {kind: "drun", namespace: DR_NS()});
  const open = openProps(props);
  const run = <button className="btn primary rundisc" disabled={!mayRun || !sites.length} title={mayRun ? "" : acc.why("create", "drhub", {kind: "drun", namespace: DR_NS()})}
    onClick={() => window.__ui.dialog(runDiscoveryDialog(sites, cfg), {kind: "drun", id: "new"})}><Icon n="plus" s={12} />Run discovery</button>;
  return <div>
    <div className="dhead"><div style={{minWidth: 0, flex: 1}}><h1>Discovery</h1>
      <div className="dsub">dr-hub builds a dependency graph per site from its dr-agent's report and proposes whole applications as bundles: membership, labels, tiers, probes, recovery order and site mappings. Every bundle needs approval{cfg.gitOps ? " — a merged pull request" : ""}.</div></div>
      <div style={{display: "flex", gap: 8}}><button className="btn" onClick={() => nav.drLayer("proposals")}><Icon n="list" s={12} />Proposals</button>{run}</div></div>
    {r.error && <div className="banner"><Icon n="alert" s={15} /><span><b>Cannot read discovery.</b> {r.error.message}{r.error.status === 404 ? " — this DR hub predates AI-assisted discovery (no DiscoveryGraph CRD)." : ""}</span></div>}
    {cfg.present && !cfg.enabled && <div className="banner info"><Icon n="alert" s={15} /><span>Discovery is off on this hub (DRConfig <span className="mono">spec.discovery.enabled</span>); runs started here still build graphs and bundles once dr-hub serves them.</span></div>}
    {cfg.present && !cfg.gitOps && <div className="banner info"><Icon n="check" s={15} /><span>No GitOps target is configured: bundles are approved in this console (fallback). With a target, merging the pull request approves.</span></div>}
    <div className="stats">
      <Stat k="Sites with a graph" v={`${graphs.length}/${sites.length}`} />
      <Stat k="Candidates" v={graphs.reduce((n, g) => n + g.counts.candidates, 0)} s={`${graphs.reduce((n, g) => n + g.counts.nodes, 0)} nodes · ${graphs.reduce((n, g) => n + g.counts.edges, 0)} edges`} />
      <Stat k="Open bundles" v={open.length} s={`${open.filter(p => p.openBlocking).length} with blocking questions`} c={open.some(p => p.openBlocking || p.dryBlocking) ? "var(--warn)" : null} />
      <Stat k="Runs running" v={runs.filter(x => !x.final).length} s={`${runs.length} in total`} c={runs.some(x => !x.final) ? "var(--info)" : null} />
      <Stat k="AI refinement" v={cfg.providers.length ? "available" : "phase 2"} s={cfg.providers.length ? cfg.providers.map(p => p.name).join(", ") : "no model provider"} />
    </div>
    <div className="sech"><h2>Sites</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}>
      <Table_ cols={["Site", "Graph built", "Nodes · edges", "Candidates", "Flows", "Agent", "Open bundles"]} empty={r.loading ? "Loading…" : "No managed site reports yet."}
        rows={sites.map(s => [s.graph ? <Ref label={s.name} onClick={() => nav.detail(s.graph)} /> : <M>{s.name}</M>,
          s.built ? fmtAgo(s.built) : <span style={{color: "var(--dim2)"}}>no graph yet</span>,
          s.graph ? `${s.graph.counts.nodes} · ${s.graph.counts.edges}` : "", s.graph ? s.graph.counts.candidates : "",
          <span style={{color: s.flowsOff ? "var(--dim)" : "var(--ok)"}}>{flowsText(s)}</span>,
          s.agent ? <span title={s.agent.lastSeen ? `last seen ${fmtAgo(s.agent.lastSeen)}` : ""}>{s.agent.available ? "reporting" : "not reporting"}{s.agent.version ? <M dim> {s.agent.version}</M> : null}</span> : <span style={{color: "var(--dim2)"}}>—</span>,
          openProps(props.filter(p => p.site === s.name)).length || ""])} />
    </div></div>
    <div className="sech"><h2>Recent runs</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}><RunTable runs={runs} limit={10} /></div></div>
  </div>;
}

// ---- one site (DiscoveryGraph detail) --------------------------------------------------------
function DGraphDetail({o: g, nav}) {
  const r = useResource("disc.site." + g.id, () => Promise.all([discovery.proposals(), discovery.runs(), discovery.config(), drhub.managedClusters()]), 8000);
  const d = useResource("disc.data." + g.id + "." + g.observedReport, () => discovery.graphData(g), 0);
  const acc = useAccess();
  const [scope, setScope] = React.useState(null);
  const [props, runs, cfg, mcs] = r.data || [[], [], {providers: [], optOut: [], agents: []}, []];
  const siteProps = props.filter(p => p.site === g.site), siteRuns = runs.filter(x => x.site === g.site);
  const site = discSites([g], cfg, mcs).find(s => s.name === g.site) || {name: g.site, namespaces: [], flowsOff: false};
  const mayRun = acc.can("create", "drhub", {kind: "drun", namespace: DR_NS()});
  const bundleOf = c => siteProps.find(p => p.candidate === c.id && !p.final) || siteProps.find(p => p.candidate === c.id) || null;
  const failing = g.conditions.filter(c => c.status === "False");
  return <div>
    <DetailHead obj={g} title={`Discovery · ${g.site}`} sub={<span className="mono" style={{color: "var(--dim)"}}>DiscoveryGraph {g.name}{g.observedReport ? ` · report ${g.observedReport.slice(0, 12)}` : ""}</span>}
      badge={<button className="btn primary rundisc" disabled={!mayRun} title={mayRun ? "" : acc.why("create", "drhub", {kind: "drun", namespace: DR_NS()})}
        onClick={() => window.__ui.dialog(runDiscoveryDialog(discSites([g], cfg, mcs), cfg, {site: g.site}), {kind: "drun", id: "new"})}><Icon n="plus" s={12} />Run discovery</button>} />
    {!!g.truncated.length && <div className="banner"><Icon n="alert" s={15} /><span><b>The site's report was truncated:</b> {g.truncated.join(", ")}. Candidates may miss members; narrow the scope or raise the agent's budget.</span></div>}
    {failing.map(c => <div key={c.type} className="banner"><Icon n="alert" s={15} /><span><b>{c.type}:</b> {c.message || c.reason}</span></div>)}
    <div className="stats">
      <Stat k="Graph built" v={g.built ? fmtAgo(g.built) : "not yet"} s={g.built ? fmtDate(g.built) : "waiting for the first report"} />
      <Stat k="Nodes · edges" v={`${g.counts.nodes} · ${g.counts.edges}`} s={`${g.counts.evidence} evidence records`} />
      <Stat k="Candidates" v={g.counts.candidates} s={`${g.candidates.filter(c => c.adopted).length} adopted from existing protection`} />
      <Stat k="Flows" v={flowsText(site)} s={site.flowsOff ? "no flow evidence on this site" : `eBPF, ${(cfg.flows && cfg.flows.udp) ? "TCP + UDP" : "TCP"}`} />
      <Stat k="Bundles" v={siteProps.length} s={`${openProps(siteProps).length} open`} />
    </div>
    <div className="sech"><h2>Application candidates</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}>
      <Table_ cols={["Candidate", "Namespaces", "Members", "Score", "Adopted application", "Bundle"]} empty="No candidate yet."
        rows={g.candidates.map(c => { const b = bundleOf(c); return [
          <button className="candpick" style={{color: "var(--accent)", fontFamily: "inherit"}} onClick={() => setScope({type: "candidate", id: c.id, members: c.members})} title="Draw this candidate's graph">{c.name}</button>,
          <M dim>{c.namespaces.join(", ")}</M>, c.members.length, <Conf v={c.score} />, c.adopted ? <M>{c.adopted}</M> : "",
          b ? <span style={{display: "inline-flex", gap: 6, alignItems: "center"}}><TrafficLight status={b.phase} sm /><Ref label={b.name} onClick={() => nav.detail(b)} /></span> : <span style={{color: "var(--dim2)"}}>none</span>]; })} />
    </div></div>
    <GraphCard g={g} data={d.data} error={d.error} loading={d.loading} scope={scope} setScope={setScope} />
    <div className="sech"><h2>Bundles for {g.site}</h2><span className="ln"></span><button className="chip" onClick={() => nav.drLayer("proposals")}>All proposals</button></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}><ProposalTable props={siteProps} nav={nav} empty="No bundle for this site yet." /></div></div>
    <div className="sech"><h2>Runs</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}><RunTable runs={siteRuns} /></div></div>
    {React.createElement(window.DrConditions, {o: g})}
  </div>;
}

// ---- proposals (DR → Proposals) -------------------------------------------------------------
const PROP_FILTERS = {open: p => !p.final && p.phase !== "Applied", applied: p => p.phase === "Applied", all: () => true};
function ProposalsView({nav}) {
  const r = useResource("disc.props", () => Promise.all([discovery.proposals(), discovery.config()]), 8000);
  const [f, setF] = React.useState("open");
  const [props, cfg] = r.data || [[], {}];
  const shown = props.filter(PROP_FILTERS[f]);
  return <div>
    <div className="dhead"><div style={{minWidth: 0, flex: 1}}><h1>Proposals</h1>
      <div className="dsub">One bundle per application: the objects, the labels on its volumes and workloads, its place in a recovery plan and the site mappings it needs. {cfg.gitOps ? "Approve by merging the bundle's pull request; reverting the merge rolls it back." : "No GitOps target is configured: approve and roll back here."}</div></div>
      <button className="btn" onClick={() => nav.drLayer("aidisc")}><Icon n="k8s" s={12} />Discovery</button></div>
    {r.error && <div className="banner"><Icon n="alert" s={15} /><span><b>Cannot read proposals.</b> {r.error.message}</span></div>}
    <div style={{display: "flex", gap: 6, margin: "4px 0 10px"}}>
      {Object.keys(PROP_FILTERS).map(k => <button key={k} className={"chip propfilter" + (f === k ? " on" : "")} onClick={() => setF(k)}>{k} · {props.filter(PROP_FILTERS[k]).length}</button>)}</div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}><ProposalTable props={shown} nav={nav} empty={r.loading ? "Loading…" : f === "open" ? "No open bundle." : "Nothing here."} /></div></div>
  </div>;
}

// ---- one bundle (DRProposal detail) ---------------------------------------------------------
const changeText = c => c.remove ? `remove ${c.key}` : `${c.key}=${c.value}`;
const objRef = x => `${x.namespace ? x.namespace + "/" : ""}${x.name}`;
function DRPropDetail({o: p, nav}) {
  const r = useResource("disc.prop." + p.id, () => Promise.all([discovery.config(), discovery.graphBySite(p.site).catch(() => null), discovery.proposals()]), 8000);
  const [cfg, graph, all] = r.data || [{providers: [], gitOps: null}, null, []];
  const d = useResource("disc.propdata." + p.id + "." + (graph ? graph.observedReport : ""), () => graph ? discovery.graphData(graph) : Promise.resolve(null), 0);
  const acc = useAccess();
  const [openSpec, setOpenSpec] = React.useState({});
  const gitOps = !!(cfg && cfg.gitOps);
  const byName = n => all.find(x => x.name === n && x.namespace === p.namespace) || null;
  const may = op => acc.can(op, "drhub", {kind: "drprop", namespace: p.namespace});
  const why = op => acc.why(op, "drhub", {kind: "drprop", namespace: p.namespace});
  const mayRun = acc.can("create", "drhub", {kind: "drun", namespace: DR_NS()});
  const btn = (cls, label, icon, ok, reason, dialog) => <button className={"btn " + cls} disabled={!ok} title={ok ? "" : reason} onClick={() => window.__ui.dialog(dialog, p)}><Icon n={icon} s={12} />{label}</button>;
  const pending = p.request && !p.final;
  const notApplyable = p.dryBlocking ? `${p.dryBlocking} blocking dry-run check(s) fail` : p.openBlocking ? `${p.openBlocking} blocking question(s) open` : "";
  const actions = <div style={{display: "flex", gap: 6, flexWrap: "wrap"}}>
    {gitOps && p.phase === "Proposed" && !(p.gitOps && p.gitOps.pr) && btn("openpr", "Open pull request", "link", may("patch") && !pending, pending ? `${p.request} requested` : why("patch"), requestDialog(p, "open-pr"))}
    {!p.final && btn("refine", "Refine with AI", "camera", cfg.providers.length > 0 && mayRun && !p.final,
      !cfg.providers.length ? "Phase 2: no model provider is configured (DRConfig spec.discovery.providers)" : p.final ? "the bundle is final" : acc.why("create", "drhub", {kind: "drun", namespace: DR_NS()}),
      runDiscoveryDialog([{name: p.site, built: graph && graph.built, namespaces: []}], cfg, {site: p.site, proposal: p.name, mode: "AI"}))}
    {!gitOps && ["Proposed", "Stale"].includes(p.phase) && btn("primary approve", "Approve", "check", may("approve") && !notApplyable && !pending,
      pending ? `${p.request} requested` : notApplyable || why("approve"), requestDialog(p, "approve"))}
    {!gitOps && p.phase === "Applied" && btn("rollback", "Roll back", "swap", may("rollback") && !pending, pending ? `${p.request} requested` : why("rollback"), requestDialog(p, "rollback"))}
    {!p.final && !["Applied", "Merged", "WaitingForApplications"].includes(p.phase) && btn("reject", "Reject", "x", may("patch") && !pending, pending ? `${p.request} requested` : why("patch"), rejectDialog(p))}
  </div>;
  return <div>
    <DetailHead obj={p} title={`Bundle · ${p.title}`} sub={<span className="mono" style={{color: "var(--dim)"}}>DRProposal {p.namespace}/{p.name} · {p.scope} · site {p.site}</span>} badge={actions} />
    {pending && <div className="banner info"><Icon n="clock" s={15} /><span><b>{(PROP_REQUESTS[p.request] || {label: p.request}).label} requested</b>{p.requestReason ? ` (${p.requestReason})` : ""} — dr-hub carries it out and records the result in the bundle's phase.</span></div>}
    {!!p.dryBlocking && !p.final && <div className="banner"><Icon n="alert" s={15} /><span><b>{p.dryBlocking} blocking dry-run check(s) fail.</b> {gitOps ? "dr-hub still opens the pull request; fix it on the branch or wait for a revision." : "Approval stays disabled until a revision passes."}</span></div>}
    {!!p.openBlocking && !p.final && <div className="banner"><Icon n="alert" s={15} /><span><b>{p.openBlocking} blocking question(s) open.</b> {gitOps ? "Answer them in the pull request by checking one option each." : "Answer them before approving (see Questions)."}</span></div>}
    {p.phase === "Superseded" && <div className="banner info"><Icon n="swap" s={15} /><span>Superseded by {byName(p.supersededBy) ? <Ref label={p.supersededBy} onClick={() => nav.detail(byName(p.supersededBy))} /> : <span className="mono">{p.supersededBy || "a newer bundle"}</span>}.</span></div>}
    <div className="stats">
      <Stat k="Phase" v={<TrafficLight status={p.phase} />} s={p.approvedBy ? `approved by ${p.approvedBy}` : p.appliedAt ? `applied ${fmtAgo(p.appliedAt)}` : ""} />
      <Stat k="Confidence" v={confPct(p.confidence)} s={`${p.evidenceIds.length} evidence record(s)`} />
      <Stat k="Source" v={p.ai ? "AI" : "rules"} s={p.ai ? `run ${p.run}${p.baseline ? ` · baseline ${p.baseline}` : ""}` : "deterministic baseline"} />
      <Stat k="Dry run" v={p.dryRun ? <VerdictBadge v={p.dryRun.verdict} /> : "not run"} s={p.dryRun ? `${p.dryRun.checks.length} checks` : ""} />
      <Stat k="Pull request" v={p.gitOps && p.gitOps.pr ? <PRLink g={p.gitOps} /> : gitOps ? "not opened" : "no GitOps target"} s={p.gitOps && p.gitOps.mergeCommit ? `merged ${p.gitOps.mergeCommit.slice(0, 10)}` : ""} />
    </div>
    <div className="card"><h3>Summary</h3><div className="bd">
      <p style={{margin: 0}}>{p.summary || <span style={{color: "var(--dim2)"}}>No summary.</span>}</p>
      {(p.dependsOn.length > 0 || p.baseline) && <Props rows={[p.dependsOn.length ? ["Depends on", <span style={{display: "inline-flex", gap: 8, flexWrap: "wrap"}}>{p.dependsOn.map(n => byName(n) ? <Ref key={n} label={n} onClick={() => nav.detail(byName(n))} /> : <M key={n}>{n}</M>)}</span>] : null,
        p.baseline ? ["Rules baseline", byName(p.baseline) ? <Ref label={p.baseline} onClick={() => nav.detail(byName(p.baseline))} /> : <M>{p.baseline}</M>] : null].filter(Boolean)} />}
      {p.scope === "RecoveryPlan" && <p className="mdesc" style={{margin: "8px 0 0"}}>After its merge, dr-hub applies a recovery-plan bundle only once every application bundle it names is applied.</p>}
    </div></div>
    <div className="sech"><h2>Objects</h2><span className="ln"></span></div>
    {p.objects.map((x, i) => <div className="card propobj" key={i}><h3><span className="badge">{x.operation}</span> {x.kind} <span className="mono" style={{color: "var(--dim)"}}>{objRef(x)}</span>
      <button className="chip" style={{marginLeft: "auto"}} onClick={() => setOpenSpec(Object.assign({}, openSpec, {[i]: !openSpec[i]}))}>{openSpec[i] ? "hide spec" : "show spec"}</button></h3><div className="bd" style={{overflowX: "auto"}}>
      <Table_ cols={["Field", "Confidence", "Evidence", "Note"]} empty="No field carries its own basis."
        rows={x.fields.map(f => [<M>{f.field}</M>, <Conf v={f.confidence} />, <M dim>{f.evidence.join(", ")}</M>, f.note])} />
      {openSpec[i] && <pre className="mono propspec" style={{margin: "8px 0 0", fontSize: 11, maxHeight: 320, overflow: "auto", background: "var(--bg2, var(--panel))", padding: 8, borderRadius: 4}}>{JSON.stringify(x.spec, null, 2)}</pre>}
    </div></div>)}
    {!!p.diff && <><div className="sech"><h2>Diff against the current objects</h2><span className="ln"></span></div>
      <div className="card"><div className="bd"><pre className="mono propdiff" style={{margin: 0, fontSize: 11, maxHeight: 360, overflow: "auto"}}>{p.diff}</pre></div></div></>}
    <div className="sech"><h2>Labels</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}>
      <Table_ cols={["Site", "Object", "Change", "Effect", "Reason", "Evidence"]} empty="The bundle changes no label."
        rows={p.labels.map(l => [<M>{l.cluster}</M>, <M>{l.change.kind} {objRef(l.change)}</M>, <M>{changeText(l.change)}</M>,
          l.effect ? <span className="badge">{l.effect}</span> : "", l.reason, <M dim>{l.evidence.join(", ")}</M>])} />
      <p className="mdesc" style={{margin: "8px 0 0"}}>Applied by each site's dr-agent through allow-listed label requests; a rollback restores the previous values.</p>
    </div></div>
    {!!p.migrations.length && <><div className="sech"><h2>Volumes that would join a consistency group</h2><span className="ln"></span></div>
      <div className="card"><div className="bd" style={{overflowX: "auto"}}>
        <Table_ cols={["Site", "Volume", "Group", "Why"]} rows={p.migrations.map(m => [<M>{m.cluster}</M>, <M>{m.namespace}/{m.pvc}</M>, <M>{m.group}</M>, m.reason])} />
        <p className="mdesc" style={{margin: "8px 0 0"}}>Listed for information only. A consistency group is fixed when a volume is created, so these existing volumes are not moved; they join once the storage refactor allows forming groups later.</p>
      </div></div></>}
    <div className="sech"><h2>Questions</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}>
      <Table_ cols={["Question", "Options", "Blocking", "Answer"]} empty="Nothing left to decide."
        rows={p.questions.map(q => [q.text, <span style={{display: "inline-flex", gap: 4, flexWrap: "wrap"}}>{q.options.map(op => <span key={op} className="badge" style={op === q.answer ? {color: "var(--ok)", borderColor: "var(--ok)"} : {}}>{op}</span>)}</span>,
          q.blocking ? <b style={{color: "var(--bad)"}}>yes</b> : <span style={{color: "var(--dim)"}}>no</span>,
          q.answer ? <span><b>{q.answer}</b>{q.by ? <span style={{color: "var(--dim)"}}> · {q.by}</span> : null}</span> : <span style={{color: "var(--warn)"}}>open</span>])} />
      {!!p.questions.length && <p className="mdesc propqnote" style={{margin: "8px 0 0"}}>{gitOps
        ? <>Read-only here: answer in the pull request by checking one option of each question{p.gitOps && p.gitOps.pr ? <> (<a href={p.gitOps.pr} target="_blank" rel="noopener noreferrer" style={{color: "var(--accent)"}}>open it</a>)</> : null}; dr-hub reads the answers back.</>
        : "Read-only here: without a GitOps target dr-hub takes answers from the bundle's annotations (dr.simplyblock.io/answer.<question id>), set with kubectl until the console offers them."}</p>}
    </div></div>
    <div className="sech"><h2>Evidence</h2><span className="ln"></span>{graph && <button className="chip" onClick={() => nav.detail(graph)}>Open the graph of {p.site}</button>}</div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}>
      {d.error && <p className="mdesc" style={{margin: "0 0 8px", color: "var(--warn)"}}>Graph data unavailable ({d.error.message}); evidence is shown by id only.</p>}
      <EvidenceRows ids={p.evidenceIds} data={d.data} />
    </div></div>
    <div className="sech"><h2>Dry run</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{overflowX: "auto"}}>{p.dryRun ? React.createElement(window.DrCheckTable, {checks: p.dryRun.checks}) : <div className="nolim">dr-hub has not dry-run this bundle yet.</div>}</div></div>
    {React.createElement(window.DrConditions, {o: p})}
  </div>;
}

// ---- links from the existing pages -------------------------------------------------------------
// Protect application, Edit tiers & probes, Edit bindings stay; the pages
// they live on point at the newest bundle that would change the object.
function ProposalLink({kind, name, namespace, nav}) {
  const r = useResource(`disc.latest.${kind}.${namespace || ""}.${name}`, () => discovery.latestFor(kind, name, namespace), 15000);
  const p = r.data;
  if (!p) return null;
  return <div className="banner info proplink"><Icon n="list" s={15} /><span>A {p.ai ? "AI-refined" : "discovered"} bundle proposes {p.objects.find(x => x.kind === kind && x.name === name) ? (p.objects.find(x => x.kind === kind && x.name === name).operation === "create" ? "this object" : "changes to this object") : "changes"} ({confPct(p.confidence)} confidence, {(STATUS_META[p.phase] || {label: p.phase}).label}).</span>
    <button className="chip" style={{marginLeft: "auto"}} onClick={() => nav.detail(p)}>Open latest proposal</button></div>;
}

Object.assign(window, {discovery, normDGraph, normDRProp, normDRun, DiscoveryHome, DGraphDetail, DRPropDetail, ProposalsView, ProposalTable, RunTable,
  GraphView, ProposalLink, runDiscoveryDialog, rejectDialog, requestDialog, DISC_ANN, confPct});

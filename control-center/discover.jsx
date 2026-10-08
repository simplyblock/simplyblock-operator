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

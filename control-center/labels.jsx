// ---------------------------------------------------------------------------
// Labelling (Kubernetes section): the labels DR selects by, on the hub's and
// the managed sites' StorageClasses, nodes, PVCs and workloads.
//
// Keys come from the allow-list dr-hub and dr-agent enforce (LabelRequest,
// dr ADR 0021); values from what the cluster reports, a new value only
// behind the override. A site is written through a LabelRequest that its
// dr-agent applies (the console never writes to a site), kept 7 days on the
// hub as the audit record; the hub itself is patched directly and the change
// recorded as an Event. Self-contained: one dialog, opened from the
// Kubernetes section.
// ---------------------------------------------------------------------------

const LABEL_KEYS = {
  StorageClass: ["simplyblock.io/replicated", "simplyblock.io/dr", "simplyblock.io/stretch"],
  Node: ["topology.kubernetes.io/zone", "topology.kubernetes.io/region"],
  PersistentVolumeClaim: ["app", "storage.simplyblock.io/consistency-group"],
  Workload: ["app", "dr.simplyblock.io/tier"]
};
const LABEL_KIND_OPTIONS = [{v: "StorageClass", l: "StorageClasses"}, {v: "Node", l: "nodes"}, {v: "PersistentVolumeClaim", l: "PVCs"},
  {v: "Workload", l: "workloads (VMs, Deployments, StatefulSets; their pods follow)"}];
const LABEL_VALUE_RE = /^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$/;
const HUB = "__hub__";
const CG_KEY = "storage.simplyblock.io/consistency-group";

// The objects of a kind on a site, as its dr-agent reports them.
function siteObjects(disc, cluster, kind) {
  const d = discOf(disc, cluster);
  if (!d) return [];
  if (kind === "StorageClass") return (d.storageClasses || []).map(s => ({kind, name: s.name, labels: s.labels || {}}));
  if (kind === "Node") {
    return (d.nodes || []).map(n => ({kind, name: n.name, labels: Object.assign({}, n.zone ? {"topology.kubernetes.io/zone": n.zone} : {}, n.region ? {"topology.kubernetes.io/region": n.region} : {})}));
  }
  const nss = d.namespaces || [];
  if (kind === "PersistentVolumeClaim") return nss.flatMap(n => (n.pvcs || []).map(p => ({kind, namespace: n.namespace, name: p.name, labels: p.labels || {}})));
  return nss.flatMap(n => (n.workloads || []).filter(w => ["VirtualMachine", "Deployment", "StatefulSet"].includes(w.kind))
    .map(w => ({kind: w.kind, namespace: n.namespace, name: w.name, labels: w.objectLabels || {}})));
}
// The same, read from the hub's own API server.
async function hubObjects(kind) {
  const meta = o => ({namespace: o.metadata.namespace || "", name: o.metadata.name, labels: o.metadata.labels || {}});
  if (kind === "Workload") {
    const lists = await Promise.all(["Deployment", "StatefulSet", "VirtualMachine"].map(k => k8s.list(k, {allNamespaces: true}).then(xs => xs.map(o => Object.assign({kind: k}, meta(o)))).catch(() => [])));
    return lists.flat();
  }
  const xs = await k8s.list(kind, RESOURCES[kind] && RESOURCES[kind].namespaced ? {allNamespaces: true} : {}).catch(() => []);
  return xs.map(o => Object.assign({kind}, meta(o)));
}
const objId = o => `${o.kind}/${o.namespace ? o.namespace + "/" : ""}${o.name}`;
const objLabel = (o, keys) => `${o.kind === "StorageClass" || o.kind === "Node" || o.kind === "PersistentVolumeClaim" ? "" : o.kind + " "}${o.namespace ? o.namespace + "/" : ""}${o.name}` +
  ` — ${keys.map(k => o.labels[k] !== undefined ? `${k.split("/").pop()}=${o.labels[k]}` : null).filter(Boolean).join(", ") || "no DR labels"}`;
// The values a key has anywhere the hub can see: this cluster's objects, and
// for topology keys every site's zones and regions.
function labelValues(disc, objs, key) {
  const vals = objs.map(o => o.labels[key]).filter(Boolean);
  if (key === "topology.kubernetes.io/zone") ((disc && disc.clusters) || []).forEach(c => vals.push(...(c.zones || [])));
  if (key === "topology.kubernetes.io/region") ((disc && disc.clusters) || []).forEach(c => vals.push(...(c.regions || [])));
  if (/replicated|\/dr$|stretch/.test(key)) vals.push("true");
  return uniqSorted(vals);
}
// The changes the form asks for, without the ones already as requested.
function labelChanges(objs, ids, key, value, remove) {
  return objs.filter(o => ids.includes(objId(o))).map(o => ({o, unchanged: remove ? o.labels[key] === undefined : o.labels[key] === value}))
    .map(({o, unchanged}) => ({unchanged, change: Object.assign({kind: o.kind, name: o.name, key}, o.namespace ? {namespace: o.namespace} : {}, remove ? {remove: true} : {value})}));
}
// A site's changes go through a LabelRequest, read until dr-agent answered;
// the request stays on the hub as the audit record.
async function requestLabels(cluster, changes) {
  const name = `console-labels-${Date.now().toString(36)}`, namespace = DR_NS();
  try {
    await k8s.create("LabelRequest", {apiVersion: "dr.simplyblock.io/v1alpha1", kind: "LabelRequest",
      metadata: {name, namespace, labels: {"app.kubernetes.io/created-by": "console"}, annotations: {"dr.simplyblock.io/created-by": (window.access && window.access.state && window.access.state.user) || "console"}},
      spec: {cluster, changes}}, {namespace});
  } catch (e) {
    if (e && e.status === 404) throw new Error("This DR hub cannot label a site's objects yet (dr-hub before LabelRequest).");
    if (e && e.status === 403) throw new Error("Labelling a site needs the dr-admin role: create on labelrequests in dr.simplyblock.io.");
    throw e;
  }
  for (let i = 0; i < 180; i++) {
    await new Promise(r => setTimeout(r, 1000));
    const o = await k8s.get("LabelRequest", name, {namespace});
    const st = (o && o.status) || {};
    if (["Passed", "Failed", "Error"].includes(st.phase)) return st;
  }
  throw new Error("dr-agent did not answer within 3 minutes; the LabelRequest stays on the hub.");
}
// The hub's own objects are patched directly, with an Event as the record.
async function patchHubLabels(changes) {
  const out = [];
  for (const [i, c] of changes.entries()) {
    const kinds = c.kind === "VirtualMachine" || c.kind === "Deployment" || c.kind === "StatefulSet";
    const body = {metadata: {labels: {[c.key]: c.remove ? null : c.value}}};
    if (kinds) body.spec = {template: {metadata: {labels: {[c.key]: c.remove ? null : c.value}}}};
    try {
      await k8s.patch(c.kind, c.name, body, c.namespace ? {namespace: c.namespace} : {});
      out.push({index: i, result: "Applied", message: c.remove ? `removed ${c.key}` : `set ${c.key}=${c.value}`});
      k8s.create("Event", {apiVersion: "v1", kind: "Event", metadata: {generateName: `${c.name}.dr-label-`, namespace: c.namespace || "default"},
        involvedObject: Object.assign({kind: c.kind, name: c.name}, c.namespace ? {namespace: c.namespace} : {}), reason: "DRLabelChanged", type: "Normal",
        message: `simplyblock console: ${c.remove ? `removed ${c.key}` : `set ${c.key}=${c.value}`}`, source: {component: "control-center"}}, {namespace: c.namespace || "default"}).catch(() => {});
    } catch (e) { out.push({index: i, result: "Failed", message: e.message}); }
  }
  return {phase: out.some(r => r.result === "Failed") ? "Failed" : "Passed", results: out};
}

// pre: {cluster, kind, objects} to open on one object (the per-object
// "Label…" actions of PVCs and their workloads)
const labelDialog = (pre = {}) => ({
  title: pre.title || "Label for DR", confirm: "Apply", done: "Labels applied",
  desc: "The labels DR selects by: the replicated StorageClasses, the zones and regions of nodes, the app label and consistency group of PVCs, and the app label and tier of workloads. A managed site's objects are labelled by its dr-agent through a LabelRequest, kept 7 days on the hub as the record.",
  prepare: () => drhub.discovery().then(async disc => {
    const hub = {};
    for (const k of Object.keys(LABEL_KEYS)) hub[k] = await hubObjects(k);
    return {disc, hub};
  }),
  fields: (v, prep) => {
    const {disc, hub} = prep || {};
    const sites = ((disc && disc.clusters) || []).filter(c => c.reported);
    const kind = v.kind || "StorageClass";
    const objs = v.cluster === HUB ? ((hub || {})[kind] || []) : siteObjects(disc, v.cluster, kind);
    const keys = LABEL_KEYS[kind];
    const key = keys.includes(v.key) ? v.key : keys[0];
    const values = labelValues(disc, objs, key);
    const remove = v.op === "remove";
    const value = v.valueOverride ? (v.valueText || "").trim() : v.value;
    const plan = labelChanges(objs, v.objects || [], key, value, remove);
    const todo = plan.filter(p => !p.unchanged);
    const boundCg = kind === "PersistentVolumeClaim" && key === CG_KEY && !remove ? todo.length : 0;
    return [
      {k: "cluster", label: "Cluster", type: "select", required: true, def: pre.cluster || (sites[0] || {}).name || HUB,
        options: sites.map(c => ({v: c.name, l: `${c.name} (managed site, through its dr-agent)`})).concat([{v: HUB, l: "the hub (this cluster)"}])},
      {k: "kind", label: "Objects", type: "select", required: true, def: pre.kind || "StorageClass", options: LABEL_KIND_OPTIONS},
      {k: "objects", label: `${LABEL_KIND_OPTIONS.find(o => o.v === kind).l} — with their DR labels now`, type: "multiselect", required: true, def: pre.objects || [],
        options: objs.map(o => ({v: objId(o), l: objLabel(o, keys)})), empty: v.cluster === HUB ? "None on the hub." : "The site's dr-agent reports none."},
      {k: "key", label: "Label", type: "select", required: true, options: keys.map(k => ({v: k, l: k}))},
      {k: "op", label: "Change", type: "select", def: "set", options: [{v: "set", l: "set the value"}, {v: "remove", l: "remove the label"}]},
      !remove && {k: "valueOverride", label: "A new value", type: "checkbox", def: false},
      !remove && (v.valueOverride
        ? {k: "valueText", label: "Value", type: "text", required: true, validate: x => x && (x.length > 63 || !LABEL_VALUE_RE.test(x)) ? "A label value: at most 63 letters, digits, \"-\", \"_\" and \".\", starting and ending with a letter or digit." : null}
        : {k: "value", label: "Value", type: "select", required: true, options: values.map(x => ({v: x, l: `${x}${objs.filter(o => o.labels[key] === x).length ? ` (on ${objs.filter(o => o.labels[key] === x).length})` : ""}`})),
          empty: `No value of ${key} is in use: tick "A new value".`}),
      boundCg > 0 && {k: "nCg", type: "note", label: `${CG_KEY} is honoured when a volume is created. ${boundCg === 1 ? "This PVC exists" : `These ${boundCg} PVCs exist`} already: for ${boundCg === 1 ? "it" : "them"} the group is a late join, which takes effect only where the volume is already on the group's storage node, or after a live migration (consistency-group co-location). Nothing is moved by this change; dr-agent reports it per PVC.`},
      kind === "Workload" && key === "dr.simplyblock.io/tier" && {k: "nTier", type: "note", icon: "check", label: "The tier is set on the workload and on its pod template, so its pods carry it after their next restart; bare pods are not offered (their controller would revert the label)."},
      (v.objects || []).length > 0 && {k: "nPlan", type: "note", icon: todo.length ? "check" : "alert",
        label: todo.length ? `${remove ? "Removes" : `Sets ${key}=${value || "…"} on`} ${todo.length} object${todo.length === 1 ? "" : "s"}${plan.length > todo.length ? `; ${plan.length - todo.length} already as requested` : ""}.` : "Every selected object is as requested already."}
    ].filter(Boolean);
  },
  run: async v => {
    const prep = v.__prep || {};
    const kind = v.kind || "StorageClass";
    const key = LABEL_KEYS[kind].includes(v.key) ? v.key : LABEL_KEYS[kind][0];
    const disc = window.__lastDisc;
    const objs = v.cluster === HUB ? await hubObjects(kind) : siteObjects(disc, v.cluster, kind);
    const remove = v.op === "remove";
    const value = v.valueOverride ? (v.valueText || "").trim() : v.value;
    if (!remove && !value) throw new Error("Choose the value.");
    const changes = labelChanges(objs, v.objects || [], key, value, remove).filter(p => !p.unchanged).map(p => p.change);
    if (!changes.length) throw new Error("Every selected object is as requested already.");
    const st = v.cluster === HUB ? await patchHubLabels(changes) : await requestLabels(v.cluster, changes);
    if (st.phase === "Error") throw new Error(st.message || "the change could not be applied");
    const failed = (st.results || []).filter(r => r.result === "Failed");
    if (failed.length) throw new Error(`${failed.length} of ${changes.length} changes failed: ${failed.map(f => `${changes[f.index].name}: ${f.message}`).join("; ")}`);
    const late = (st.results || []).filter(r => /late join/.test(r.message || ""));
    if (late.length) window.__toast(`${late.length} PVC${late.length === 1 ? "" : "s"}: the consistency group is a late join (see the LabelRequest)`);
  }
});

// The cluster a Kubernetes-tab object lives on, as the dialog names it: a
// managed site by name, anything else the hub.
function labelClusterOf(k8sClusterId) {
  const c = k8sClusterId && REG[k8sClusterId];
  const name = c ? c.name : k8sClusterId;
  return name && name !== "hub" && name !== "local-cluster" && (!c || c.source !== "hub") ? name : HUB;
}
const WORKLOAD_KINDS = {virtualmachine: "VirtualMachine", deployment: "Deployment", statefulset: "StatefulSet"};
// The per-object "Label…" actions of a PVC and of the workload that mounts it.
function labelActionsForPvc(o) {
  const cluster = labelClusterOf(o.k8sClusterId);
  const out = [{label: "Label…", icon: "list", op: "create", entity: "labelrequest",
    dialog: labelDialog({title: `Label ${o.namespace}/${o.name}`, cluster, kind: "PersistentVolumeClaim",
      objects: [`PersistentVolumeClaim/${o.namespace}/${o.name}`]})}];
  const wk = WORKLOAD_KINDS[String(o.workloadKind || "").toLowerCase()];
  if (o.workload && wk) out.push({label: `Label ${wk === "VirtualMachine" ? "VM" : wk.toLowerCase()} ${o.workload}…`, icon: "list", op: "create", entity: "labelrequest",
    dialog: labelDialog({title: `Label ${wk} ${o.namespace}/${o.workload}`, cluster, kind: "Workload",
      objects: [`${wk}/${o.namespace}/${o.workload}`]})});
  return out;
}

Object.assign(window, {labelDialog, LABEL_KEYS, siteObjects, labelChanges, labelValues, labelActionsForPvc, labelClusterOf});

// ---------------------------------------------------------------------------
// CONTEXTS: the Kubernetes clusters the console reads
//
// The console can manage storage across several Kubernetes clusters. Each
// one is a named context: the browser reaches its API server through the
// same-origin proxy at the context's `k8sBase` (`/k8s` for the local
// cluster, `/k8s/<name>` for another one), rendered into config.js by the
// console pod's entrypoint. A context carries free-form `labels` the
// administrator chose when configuring the console (a lifecycle stage, a
// tenant, a site); the fleet dashboard can group by any of them, and says
// that the value came from the console's configuration rather than from an
// object.
//
// This file loads first: every other module reads CONTEXTS.
// ---------------------------------------------------------------------------
const SBC = window.SB_CONFIG || {};

const LOCAL_DEFAULT = {name: "local", local: true, labels: {}, k8sBase: SBC.k8sBase || "/k8s", namespaces: "all"};

function normalizeContext(c, i) {
  const o = Object.assign({}, c);
  o.name = String(o.name || (i === 0 ? "local" : `ctx-${i}`));
  o.local = i === 0;
  o.labels = Object.assign({}, o.labels || {});
  o.k8sBase = o.k8sBase || (o.local ? LOCAL_DEFAULT.k8sBase : `/k8s/${o.name}`);
  if (o.namespaces !== "all" && !Array.isArray(o.namespaces)) o.namespaces = "all";
  if (Array.isArray(o.namespaces) && o.namespaces.length === 0) o.namespaces = "all";
  return o;
}

// The first context is always the local cluster, so a config.js written
// before contexts existed still boots with exactly what it had.
const CONTEXTS = (Array.isArray(SBC.contexts) && SBC.contexts.length ? SBC.contexts : [LOCAL_DEFAULT]).map(normalizeContext);
const LOCAL_CTX = CONTEXTS[0].name;
const MULTI_CTX = CONTEXTS.length > 1;

const ctxByName = name => CONTEXTS.find(c => c.name === (name || LOCAL_CTX)) || null;
const ctxLabels = name => (ctxByName(name) || LOCAL_DEFAULT).labels || {};
const ctxNamespaces = name => (ctxByName(name) || LOCAL_DEFAULT).namespaces;
// the context an object belongs to, read from the registry a normalizer fills
const ctxOf = id => ((window.REG || {})[id] || {}).ctx || LOCAL_CTX;
// every label key any context declares, for the dashboard's grouping switch
const ctxLabelKeys = () => Array.from(new Set(CONTEXTS.flatMap(c => Object.keys(c.labels || {})))).sort();

Object.assign(window, {CONTEXTS, LOCAL_CTX, MULTI_CTX, ctxByName, ctxLabels, ctxNamespaces, ctxOf, ctxLabelKeys});

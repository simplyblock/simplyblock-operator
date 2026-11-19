// ---------------------------------------------------------------------------
// CONTEXTS AND FEATURE FLAGS
//
// The console manages storage across several Kubernetes clusters. Each one is
// a named context: the browser reaches its API server through the same-origin
// proxy at `k8sBase` (/k8s for the local cluster, /k8s/<name> for a remote
// one) and its simplyblock control plane through `cpBase` (/cp/<name>), both
// rendered by the console pod's entrypoint from a contexts file. A context
// without `cpBase` has no control plane reachable from the console: CRD views
// still work, and anything only the control plane knows (tasks, the event log,
// live I/O) degrades with a clear message instead of an error.
//
// A context carries labels {site, stage, tenant} that the fleet dashboard
// groups by when a StorageCluster or its Namespace does not say otherwise
// (see fleet-model.jsx for the precedence).
//
// Feature flags gate the views that have no real API behind them yet. They
// are off in a deployment and on in the design preview, where the in-browser
// fixture backend still serves them.
//
// This file loads first: every other module reads CONTEXTS.
// ---------------------------------------------------------------------------
const SBC = window.SB_CONFIG || {};

// The local cluster reads every namespace its role grants: a tenant-per-
// namespace layout keeps each tenant's clusters in its own namespace, and the
// console's own namespace (SB_CONFIG.namespace) is only where it creates
// objects that name no other home.
const LOCAL_DEFAULT = {
  name: "local", local: true,
  labels: {},
  k8sBase: SBC.k8sBase || "/k8s",
  cpBase: SBC.cpBase || "",
  namespaces: "all"
};

function normalizeContext(c, i) {
  const o = Object.assign({}, c);
  o.name = String(o.name || (i === 0 ? "local" : `ctx-${i}`));
  o.local = i === 0;
  o.labels = Object.assign({site: "", stage: "", tenant: ""}, o.labels || {});
  o.k8sBase = o.k8sBase || (o.local ? LOCAL_DEFAULT.k8sBase : `/k8s/${o.name}`);
  o.cpBase = o.cpBase === undefined ? (o.local ? LOCAL_DEFAULT.cpBase : `/cp/${o.name}`) : (o.cpBase || "");
  if (o.namespaces !== "all" && !Array.isArray(o.namespaces)) o.namespaces = "all";
  if (Array.isArray(o.namespaces) && o.namespaces.length === 0) o.namespaces = "all";
  return o;
}

// The first context is always the local cluster, so a config.js written
// before contexts existed still boots with exactly what it had.
const CONTEXTS = (Array.isArray(SBC.contexts) && SBC.contexts.length ? SBC.contexts : [LOCAL_DEFAULT])
  .map(normalizeContext);
const LOCAL_CTX = CONTEXTS[0].name;

const ctxByName = name => CONTEXTS.find(c => c.name === (name || LOCAL_CTX)) || null;
const ctxLabels = name => (ctxByName(name) || LOCAL_DEFAULT).labels || {};
const hasControlPlane = name => !!((ctxByName(name) || {}).cpBase);
const ctxNamespaces = name => (ctxByName(name) || LOCAL_DEFAULT).namespaces;
// the context an object belongs to, read from the registry a normalizer fills
const ctxOf = id => ((window.REG || {})[id] || {}).ctx || LOCAL_CTX;

const FEATURES = Object.assign({
  zones: false, buckets: false, cgroups: false, migrationPaths: false,
  fileStorage: false, objectStorage: false, fdbBackups: false, accessRoles: false
}, SBC.features || {});
const feature = name => !!FEATURES[name];

// Reasons a read could not be served, shared by every surface so a panel can
// render the right empty state from `error.reason` alone.
const REASON_NO_CONTROL_PLANE = "NoControlPlane";
const REASON_FEATURE_DISABLED = "FeatureDisabled";

Object.assign(window, {CONTEXTS, LOCAL_CTX, ctxByName, ctxLabels, hasControlPlane, ctxNamespaces, ctxOf,
  FEATURES, feature, REASON_NO_CONTROL_PLANE, REASON_FEATURE_DISABLED});

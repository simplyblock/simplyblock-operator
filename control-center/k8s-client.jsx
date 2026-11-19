// ---------------------------------------------------------------------------
// KUBERNETES TRANSPORT (per context), plus the Helm release view
//
// Every read and write of a Kubernetes object goes through here. A call names
// the context it addresses with `opts.ctx` (contexts.jsx); a call without one
// addresses the local cluster exactly as before, which is what keeps the DR
// section and the replication views untouched. The browser never holds a
// credential: the console pod's nginx injects the right token per context.
//
// The control plane REST API is a separate client (cp-client.jsx). The
// operator exposes no HTTP API; what it knows is in CRD status.
// ---------------------------------------------------------------------------
const SB = window.SB_CONFIG;
const GROUP = "storage.simplyblock.io";
const VERSION = "v1alpha1";
const API_GROUP = `${GROUP}/${VERSION}`;
const DR_GROUP = "dr.simplyblock.io";
const DR_API_GROUP = `${DR_GROUP}/v1alpha1`;
const SITEMAP_API_GROUP = "sitemap.simplyblock.io/v1alpha1";
const RAMEN_API_GROUP = "ramendr.openshift.io/v1alpha1";
const OCM_CLUSTER_API_GROUP = "cluster.open-cluster-management.io/v1";

class ApiError extends Error {
  constructor(status, message, path, reason) {
    super(message); this.status = status; this.path = path; this.reason = reason;
  }
}

// ---- resource registry -----------------------------------------------------
// Every kind the console reads, with the plural the API server routes on and the
// short name from the design (§7.11). `core: true` means it is not our group.
const RESOURCES = {
  // entities
  StorageCluster:          {plural: "storageclusters",          namespaced: true},
  StorageNodeSet:          {plural: "storagenodesets",          namespaced: true},
  StorageNode:             {plural: "storagenodes",             short: "sn",   namespaced: true},
  StorageDevice:           {plural: "storagedevices",           short: "sd",   namespaced: true},
  StoragePool:             {plural: "storagepools",             namespaced: true},
  StorageBackup:           {plural: "storagebackups",           namespaced: true},
  BackupPolicy:            {plural: "backuppolicies",           namespaced: true, creatable: true},
  BackupRestore:           {plural: "backuprestores",           short: "br",   namespaced: true, creatable: true},
  BackupImport:            {plural: "backupimports",            short: "bi",   namespaced: true, creatable: true},
  ControlPlane:            {plural: "controlplanes",            namespaced: true},
  Task:                    {plural: "tasks",                    namespaced: true},
  SimplyblockDriver:       {plural: "simplyblockdrivers",       short: "sbdrv", namespaced: true},
  ClusterDeploymentConfig: {plural: "clusterdeploymentconfigs", short: "cdc",  namespaced: true, version: "v1alpha2"},
  // replication: real kinds in v1alpha1. A slot is created by the operator from
  // a PVC annotation, so it is read-only here; the other three are creatable.
  ReplicationPair:   {plural: "replicationpairs",    short: "relpair", namespaced: true, creatable: true},
  ReplicationPolicy: {plural: "replicationpolicies", short: "repl",    namespaced: true, creatable: true},
  ReplicationSlot:   {plural: "replicationslots",    short: "relslot", namespaced: true},
  ReplicationOps:    {plural: "replicationops",      short: "replops", namespaced: true, creatable: true},
  // one-shot operations: the kinds that exist, with the entity each acts on.
  // A migration is the per-volume operation kind of today's API; OperatorOps
  // acts on the operator itself and names no target.
  StorageClusterOps:  {plural: "storageclusterops",  short: "scops", namespaced: true, ops: "StorageCluster"},
  StorageNodeOps:     {plural: "storagenodeops",     short: "snops", namespaced: true, ops: "StorageNode"},
  VolumeMigration:    {plural: "volumemigrations",   short: "vmig",  namespaced: true, ops: "PersistentVolume"},
  OperatorOps:        {plural: "operatorops",        short: "oops",  namespaced: true, ops: null, version: "v1alpha2"},
  // ---- DR hub (dr-simplyblock) — dr.simplyblock.io/v1alpha1 ----------------
  // The hub has no REST API: every screen is a CR. Cluster-scoped kinds have
  // no namespace segment; namespaced ones are listed across all namespaces
  // unless a namespace is given (a ProtectedApplication lives in Ramen's ops
  // namespace or in the application's own namespace).
  ProtectionPlan:       {plural: "protectionplans",       short: "pplan",    core: DR_API_GROUP, namespaced: false, dr: true},
  DRPath:               {plural: "drpaths",               short: "drpath",   core: DR_API_GROUP, namespaced: false, dr: true},
  DRConfig:             {plural: "drconfigs",             short: "drconfig", core: DR_API_GROUP, namespaced: false, dr: true},
  ProtectedApplication: {plural: "protectedapplications", short: "papp",     core: DR_API_GROUP, namespaced: true, dr: true},
  RecoveryPlan:         {plural: "recoveryplans",         short: "rplan",    core: DR_API_GROUP, namespaced: true, dr: true},
  RecoveryAction:       {plural: "recoveryactions",       short: "ract",     core: DR_API_GROUP, namespaced: true, dr: true},
  TestBubble:           {plural: "testbubbles",           short: "tbub",     core: DR_API_GROUP, namespaced: true, dr: true},
  TestSchedule:         {plural: "testschedules",         short: "tsched",   core: DR_API_GROUP, namespaced: true, dr: true},
  RestoreAction:        {plural: "restoreactions",        short: "rsa",      core: DR_API_GROUP, namespaced: true, dr: true},
  SiteProfile:          {plural: "siteprofiles",          short: "sprof",    core: SITEMAP_API_GROUP, namespaced: false, dr: true},
  DHCPServer:           {plural: "dhcpservers",           short: "dhcps",    core: SITEMAP_API_GROUP, namespaced: false, dr: true},
  // Ramen and OCM objects the hub derives — instances only, read-only here
  DRPolicy:              {plural: "drpolicies",             core: RAMEN_API_GROUP, namespaced: false},
  DRCluster:             {plural: "drclusters",             core: RAMEN_API_GROUP, namespaced: false},
  DRPlacementControl:    {plural: "drplacementcontrols",    core: RAMEN_API_GROUP, namespaced: true},
  ManagedCluster:        {plural: "managedclusters",        core: OCM_CLUSTER_API_GROUP, namespaced: false},
  // access reviews: the API server answers what the caller may do
  SelfSubjectAccessReview: {plural: "selfsubjectaccessreviews", core: "authorization.k8s.io/v1", namespaced: false},
  SelfSubjectRulesReview:  {plural: "selfsubjectrulesreviews",  core: "authorization.k8s.io/v1", namespaced: false},
  SelfSubjectReview:       {plural: "selfsubjectreviews",       core: "authentication.k8s.io/v1", namespaced: false},
  // core Kubernetes
  Namespace:             {plural: "namespaces",             core: "v1", namespaced: false},
  Node:                  {plural: "nodes",                  core: "v1", namespaced: false},
  Pod:                   {plural: "pods",                   core: "v1", namespaced: true},
  PersistentVolume:      {plural: "persistentvolumes",      core: "v1", namespaced: false},
  PersistentVolumeClaim: {plural: "persistentvolumeclaims", core: "v1", namespaced: true},
  Secret:                {plural: "secrets",                core: "v1", namespaced: true},
  Event:                 {plural: "events",                 core: "v1", namespaced: true},
  StorageClass:          {plural: "storageclasses",         core: "storage.k8s.io/v1", namespaced: false},
  VolumeSnapshot:        {plural: "volumesnapshots",        core: "snapshot.storage.k8s.io/v1", namespaced: true, creatable: true},
  VolumeSnapshotContent: {plural: "volumesnapshotcontents", core: "snapshot.storage.k8s.io/v1", namespaced: false},
  VolumeSnapshotClass:   {plural: "volumesnapshotclasses",   core: "snapshot.storage.k8s.io/v1", namespaced: false},
  Job:                   {plural: "jobs",                   core: "batch/v1", namespaced: true},
  // the schema of a kind, read for its immutability rules and descriptions
  CustomResourceDefinition: {plural: "customresourcedefinitions", core: "apiextensions.k8s.io/v1", namespaced: false},
  // application resources read for Ramen recipes
  Deployment:            {plural: "deployments",            core: "apps/v1", namespaced: true},
  StatefulSet:           {plural: "statefulsets",           core: "apps/v1", namespaced: true},
  Service:               {plural: "services",               core: "v1", namespaced: true},
  ConfigMap:             {plural: "configmaps",             core: "v1", namespaced: true},
  Ingress:               {plural: "ingresses",              core: "networking.k8s.io/v1", namespaced: true},
  VirtualMachine:        {plural: "virtualmachines",        core: "kubevirt.io/v1", namespaced: true},
  Recipe:                {plural: "recipes",                core: RAMEN_API_GROUP, namespaced: true}
};

const NS = () => SB.namespace || "simplyblock";
// Ramen's ops namespace on the hub: where discovered ProtectedApplications live
const DR_NS = () => SB.drNamespace || "ramen-ops";

// Build the API server path for a kind. Core group is /api/v1, everything else
// /apis/<group>/<version>. A namespaced kind is scoped to the console's
// namespace unless `namespace` is given; `allNamespaces: true` lists it across
// the cluster (the DR hub's namespaced kinds live in many namespaces).
function pathFor(kind, opts) {
  const o = opts || {};
  const r = RESOURCES[kind];
  if (!r) throw new Error("unknown kind " + kind);
  const prefix = !r.core ? `/apis/${GROUP}/${r.version || VERSION}`
    : r.core === "v1" ? "/api/v1"
    : `/apis/${r.core}`;
  const ns = r.namespaced && !o.allNamespaces ? `/namespaces/${o.namespace || (r.dr ? DR_NS() : NS())}` : "";
  const name = o.name ? `/${o.name}` : "";
  const sub = o.subresource ? `/${o.subresource}` : "";
  return `${prefix}${ns}/${r.plural}${name}${sub}`;
}
// apiVersion string for a kind, for the objects the console authors
const apiVersionOf = kind => { const r = RESOURCES[kind]; return !r.core ? `${GROUP}/${r.version || VERSION}` : r.core; };

// ---- transport -------------------------------------------------------------
// The API server base for a context. A call without a context goes to the
// local cluster, so every existing call site keeps its meaning.
const baseFor = ctx => {
  if (!ctx) return SB.k8sBase;
  const c = window.ctxByName ? window.ctxByName(ctx) : null;
  if (!c) throw new ApiError(0, `unknown context ${ctx}`, "", "UnknownContext");
  return c.k8sBase;
};

async function call(base, path, init) {
  let res;
  const headers = Object.assign({
    Accept: init && init.raw ? "text/plain" : "application/json",
    Authorization: `Bearer ${SB.token}`
  }, (init && init.headers) || {});
  try {
    res = await fetch(base + path, Object.assign({}, init, {headers}));
  } catch (e) {
    throw new ApiError(0, "Cannot reach the Kubernetes API", path, "Unreachable");
  }
  // A text subresource (pods/log) is returned as is; a failure still arrives as
  // a Status object, which is why the body is parsed before it is trusted.
  const text = await res.text();
  let body = null;
  try { body = JSON.parse(text); } catch (e) {}
  // A Kubernetes failure is a Status object. Trust the body, not just the code:
  // a Failure Status served with a 2xx is still a failure, and treating it as an
  // empty collection would show "no objects" during an outage.
  const failed = body && body.kind === "Status" && body.status === "Failure";
  if (!res.ok || failed) {
    const msg = (body && body.message) || res.statusText || "Request failed";
    throw new ApiError((body && body.code) || res.status, msg, path, (body && body.reason) || null);
  }
  return init && init.raw ? text : body;
}

// A namespaced list in a context whose namespaces are known lists each of them
// rather than the whole cluster, so a context restricted to one tenant never
// reads another's objects. "all" lists cluster-wide. Without a context the
// console's own namespace applies, as it always did.
function listScopes(kind, o) {
  const r = RESOURCES[kind];
  if (!r.namespaced || o.allNamespaces || o.namespace || !o.ctx || r.dr) return [o];
  const nss = window.ctxNamespaces ? window.ctxNamespaces(o.ctx) : null;
  if (nss === "all") return [Object.assign({}, o, {allNamespaces: true})];
  if (Array.isArray(nss) && nss.length) return nss.map(ns => Object.assign({}, o, {namespace: ns}));
  return [o];
}

const k8s = {
  // list: returns items[], never the List envelope
  list: (kind, opts) => {
    const o = opts || {};
    const scopes = listScopes(kind, o);
    return Promise.all(scopes.map(s => call(baseFor(s.ctx), pathFor(kind, s) + qs(s))
      .then(r => (r && r.items) || []))).then(rs => rs.flat());
  },
  get: (kind, name, opts) => call(baseFor((opts || {}).ctx), pathFor(kind, Object.assign({name}, opts)) + qs(opts)),
  create: (kind, obj, opts) => call(baseFor((opts || {}).ctx), pathFor(kind, opts) + qs(opts), {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify(obj)
  }),
  // spec edits go through a merge patch, which is what kubectl edit does.
  // `dryRun: "All"` asks the API server (webhooks and CEL rules included) to
  // validate without persisting, which is how the console checks an edit
  // before it lands.
  patch: (kind, name, patch, opts) => call(baseFor((opts || {}).ctx), pathFor(kind, Object.assign({name}, opts)) + qs(opts), {
    method: "PATCH",
    headers: {"Content-Type": "application/merge-patch+json"},
    body: JSON.stringify(patch)
  }),
  remove: (kind, name, opts) => call(baseFor((opts || {}).ctx), pathFor(kind, Object.assign({name}, opts)) + qs(opts), {method: "DELETE"}),
  // pod logs are the Kubernetes API, not a side channel; the body is text
  logs: (pod, opts) => call(baseFor((opts || {}).ctx),
    pathFor("Pod", Object.assign({name: pod, subresource: "log"}, opts)) + qs(Object.assign({tailLines: 200}, opts)), {raw: true}),
  // the API server's version, which doubles as the reachability probe of a context
  version: ctx => call(baseFor(ctx), "/version"),
  // A watch is the API server's own change stream: newline-delimited events
  // {type: ADDED|MODIFIED|DELETED|BOOKMARK|ERROR, object}. The caller keeps the
  // list it seeded from and applies events; `EXPIRED` (410) means the
  // resourceVersion is too old and the caller lists again. Returns a handle
  // with cancel() and the last resourceVersion seen.
  watch: (kind, opts, onEvent) => {
    const o = Object.assign({}, opts, {watch: true, allowWatchBookmarks: true});
    const ctrl = new AbortController();
    let rv = o.resourceVersion || null;
    (async () => {
      let res;
      try {
        res = await fetch(baseFor(o.ctx) + pathFor(kind, o) + qs(o), {
          headers: {Accept: "application/json", Authorization: `Bearer ${SB.token}`}, signal: ctrl.signal});
      } catch (e) {
        if (!ctrl.signal.aborted) onEvent({type: "ERROR", error: e});
        return;
      }
      if (!res.ok || !res.body) { onEvent({type: res.status === 410 ? "EXPIRED" : "ERROR", status: res.status}); return; }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      try {
        for (;;) {
          const {value, done} = await reader.read();
          if (done) break;
          buf += dec.decode(value, {stream: true});
          let i;
          while ((i = buf.indexOf("\n")) >= 0) {
            const line = buf.slice(0, i).trim();
            buf = buf.slice(i + 1);
            if (!line) continue;
            let ev;
            try { ev = JSON.parse(line); } catch (e) { continue; }
            if (ev.type === "ERROR") {
              const code = ev.object && ev.object.code;
              onEvent({type: code === 410 ? "EXPIRED" : "ERROR", object: ev.object});
              return;
            }
            const m = ev.object && ev.object.metadata;
            if (m && m.resourceVersion) rv = m.resourceVersion;
            onEvent(ev);
          }
        }
        onEvent({type: "CLOSED"});
      } catch (e) {
        if (!ctrl.signal.aborted) onEvent({type: "ERROR", error: e});
      }
    })();
    return {cancel: () => ctrl.abort(), rv: () => rv};
  }
};

function qs(opts) {
  const o = Object.assign({}, opts);
  delete o.namespace; delete o.name; delete o.subresource; delete o.allNamespaces;
  delete o.ctx; delete o.raw;
  const parts = Object.entries(o).filter(([, v]) => v !== undefined && v !== null && v !== "")
    .map(([k, v]) => `${k}=${encodeURIComponent(v)}`);
  return parts.length ? "?" + parts.join("&") : "";
}

// ---- access reviews ----------------------------------------------------------
// The API server is the policy decision point. A SelfSubjectAccessReview asks
// one (verb, group, resource[, namespace, name]) question; a
// SelfSubjectRulesReview returns every rule that applies to the caller in a
// namespace, including cluster-wide ones; a SelfSubjectReview says who the
// caller is. All three are POSTs that create nothing.
// Each takes the context as its last argument; without one it asks the local
// API server, as before.
const review = {
  can: (verb, group, resource, ns, name, ctx) => k8s.create("SelfSubjectAccessReview", {
    apiVersion: "authorization.k8s.io/v1", kind: "SelfSubjectAccessReview",
    spec: {resourceAttributes: Object.assign({verb, group: group || "", resource},
      ns ? {namespace: ns} : {}, name ? {name} : {})}
  }, {ctx}).then(r => !!(r.status && r.status.allowed)),
  rules: (ns, ctx) => k8s.create("SelfSubjectRulesReview", {
    apiVersion: "authorization.k8s.io/v1", kind: "SelfSubjectRulesReview", spec: {namespace: ns}
  }, {ctx}).then(r => r.status || {resourceRules: [], incomplete: true}),
  whoami: ctx => k8s.create("SelfSubjectReview", {apiVersion: "authentication.k8s.io/v1", kind: "SelfSubjectReview"}, {ctx})
    .then(r => (r.status && r.status.userInfo) || {})
};

// label selector helpers — the ownership tree is expressed in labels
const sel = o => Object.entries(o).map(([k, v]) => `${k}=${v}`).join(",");
const ownedBy = (kind, name) => ({labelSelector: sel({
  [`${GROUP}/owner-kind`]: kind, [`${GROUP}/owner-name`]: name})});
const inCluster = name => ({labelSelector: sel({[`${GROUP}/cluster`]: name})});

// ---- operator API ----------------------------------------------------------
// The operator serves no HTTP API today. These two reads stay as the one
// place the console would ask it anything, and fail with the proxy's 503
// Status until it does.
const operator = {
  releases: () => call(SB.operatorBase, "/releases"),
  health: () => call(SB.operatorBase, "/healthz")
};

// ---- Helm ------------------------------------------------------------------
const helm = {
  releases: () => call(SB.helmBase, "/releases"),
  release: name => call(SB.helmBase, `/releases/${name}`),
  values: name => call(SB.helmBase, `/releases/${name}/values`)
};

// ---- Ops helpers -----------------------------------------------------------
// An action is not a verb against an entity: it is an <Entity>Ops object whose
// spec names the target and the action. Each kind spells that differently, and
// the shapes below are the ones the CRDs validate (operator/api/v1alpha1 and
// v1alpha2). Aborting is a patch of the one mutable field, on the kinds that
// have one (OperatorOps and VolumeMigration today).
function opsKindFor(entityKind) {
  const hit = Object.entries(RESOURCES).find(([, r]) => r.ops === entityKind);
  return hit ? hit[0] : null;
}

// spec builders per Ops kind: (targetName, action, payload) -> spec
const OPS_SPEC = {
  StorageClusterOps: (t, a, p) => Object.assign({clusterRef: t, action: a}, p || {}),
  StorageNodeOps: (t, a, p) => Object.assign({storageNodeRef: t, action: a}, p || {}),
  // a migration has no action field: moving the volume is the whole kind
  VolumeMigration: (t, a, p) => Object.assign({pvName: t}, p || {}),
  OperatorOps: (t, a, p) => Object.assign({action: a}, p ? {[a.charAt(0).toLowerCase() + a.slice(1)]: p} : {})
};

// submitOps(entityKind, targetName, action, payload, {ctx, namespace})
// `action` is spelled as the CRD enum spells it (lowercase for the v1alpha1
// kinds, PascalCase for OperatorOps); `payload` holds the action's own fields.
function submitOps(entityKind, targetName, action, payload, opts) {
  const kind = opsKindFor(entityKind);
  if (!kind) throw new Error("no Ops kind for " + entityKind);
  const o = opts || {};
  const stamp = Date.now().toString(36);
  const slug = s => String(s || "").toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "");
  const body = {
    apiVersion: apiVersionOf(kind), kind,
    metadata: {
      name: [slug(targetName || "operator"), slug(action || kind), stamp].filter(Boolean).join("-").slice(0, 63),
      namespace: o.namespace || NS(),
      labels: Object.assign({[`${GROUP}/action`]: slug(action || kind)},
        targetName ? {[`${GROUP}/target`]: slug(targetName)} : {})
    },
    spec: OPS_SPEC[kind](targetName, action, payload)
  };
  return k8s.create(kind, body, {ctx: o.ctx, namespace: o.namespace});
}
const abortOps = (opsKind, name, opts) => k8s.patch(opsKind, name, {spec: {abort: true}}, opts);
// the kinds whose spec has an abort field; everything else runs to completion
const ABORTABLE_KINDS = ["OperatorOps", "VolumeMigration"];

// Terminal phases, per the design's phase vocabulary (PascalCase).
const OPS_TERMINAL = ["Succeeded", "Failed", "Aborted"];
const opsRunning = o => o && o.status && !OPS_TERMINAL.includes(o.status.phase);

Object.assign(window, {k8s, operator, helm, review, ApiError, RESOURCES, API_GROUP, GROUP, VERSION, DR_GROUP, DR_API_GROUP,
  pathFor, apiVersionOf, baseFor, ownedBy, inCluster, sel, submitOps, abortOps, opsKindFor, opsRunning, OPS_TERMINAL,
  ABORTABLE_KINDS, NS, DR_NS});

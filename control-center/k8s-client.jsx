// ---------------------------------------------------------------------------
// KUBERNETES / OPERATOR / HELM TRANSPORT
//
// The console talks to exactly three things and nothing else:
//   · the Kubernetes API   — CRDs in storage.simplyblock.io/v1alpha1, the DR
//                            hub's dr.simplyblock.io/v1alpha1 (plus the Ramen
//                            and OCM objects it derives), and core objects
//                            (Node, Pod, PVC, StorageClass, Secret, Event)
//   · the operator API     — what the operator exposes beyond CRDs
//   · Helm                 — release state for the chart
//
// It does NOT call the control plane REST API. Anything the control plane knows
// reaches the UI through a CRD status written by a controller.
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
  StorageCluster:          {plural: "storageclusters",          short: "sbc",  namespaced: true},
  StorageNode:             {plural: "storagenodes",             short: "sbn",  namespaced: true},
  StorageDevice:           {plural: "storagedevices",           short: "sbd",  namespaced: true},
  StoragePool:             {plural: "storagepools",             short: "sbp",  namespaced: true},
  StorageBackup:           {plural: "storagebackups",           short: "sbbk", namespaced: true},
  ControlPlane:            {plural: "controlplanes",            short: "sbcp", namespaced: true},
  SimplyblockDriver:       {plural: "simplyblockdrivers",       short: "sbdrv", namespaced: true},
  ClusterDeploymentConfig: {plural: "clusterdeploymentconfigs", short: "sbcdc", namespaced: true},
  // replication: real kinds in v1alpha1. A slot is created by the operator from
  // a PVC annotation, so it is read-only here; the other three are creatable.
  ReplicationPair:   {plural: "replicationpairs",    short: "relpair", namespaced: true, creatable: true},
  ReplicationPolicy: {plural: "replicationpolicies", short: "repl",    namespaced: true, creatable: true},
  ReplicationSlot:   {plural: "replicationslots",    short: "relslot", namespaced: true},
  ReplicationOps:    {plural: "replicationops",      short: "replops", namespaced: true, creatable: true},
  // one-shot operations
  StorageClusterOps:  {plural: "storageclusterops",  short: "sbco",  namespaced: true, ops: "StorageCluster"},
  StorageNodeOps:     {plural: "storagenodeops",     short: "sbno",  namespaced: true, ops: "StorageNode"},
  StorageDeviceOps:   {plural: "storagedeviceops",   short: "sbdo",  namespaced: true, ops: "StorageDevice"},
  StoragePoolOps:     {plural: "storagepoolops",     short: "sbpo",  namespaced: true, ops: "StoragePool"},
  StorageBackupOps:   {plural: "storagebackupops",   short: "sbbo",  namespaced: true, ops: "StorageBackup"},
  ControlPlaneOps:    {plural: "controlplaneops",    short: "sbcpo", namespaced: true, ops: "ControlPlane"},
  PersistentVolumeOps: {plural: "persistentvolumeops", short: "sbpvo", namespaced: true, ops: "PersistentVolume"},
  OperatorOps:        {plural: "operatorops",        short: "sbop",  namespaced: true, ops: null},
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
  // on-demand probes behind the forms' Test buttons (dr-hub ADR 0021): created,
  // read until answered, deleted; never listed in a view
  S3ProbeRequest:       {plural: "s3proberequests",       short: "s3probe",  core: DR_API_GROUP, namespaced: true, dr: true},
  HealthProbeRequest:   {plural: "healthproberequests",   short: "hprobe",   core: DR_API_GROUP, namespaced: true, dr: true},
  // Ramen and OCM objects the hub derives — instances only, read-only here
  DRPolicy:              {plural: "drpolicies",             core: RAMEN_API_GROUP, namespaced: false},
  DRCluster:             {plural: "drclusters",             core: RAMEN_API_GROUP, namespaced: false},
  DRPlacementControl:    {plural: "drplacementcontrols",    core: RAMEN_API_GROUP, namespaced: true},
  ManagedCluster:        {plural: "managedclusters",        core: OCM_CLUSTER_API_GROUP, namespaced: false},
  // a managed site's storage deployment, requested from the hub (operator,
  // storage.simplyblock.io/v1alpha2); the operator carries it to the site
  StorageSiteDeployment: {plural: "storagesitedeployments", short: "sbsd", core: "storage.simplyblock.io/v1alpha2", namespaced: true},
  // access reviews: the API server answers what the caller may do
  SelfSubjectAccessReview: {plural: "selfsubjectaccessreviews", core: "authorization.k8s.io/v1", namespaced: false},
  SelfSubjectRulesReview:  {plural: "selfsubjectrulesreviews",  core: "authorization.k8s.io/v1", namespaced: false},
  SelfSubjectReview:       {plural: "selfsubjectreviews",       core: "authentication.k8s.io/v1", namespaced: false},
  // core Kubernetes
  Node:                  {plural: "nodes",                  core: "v1", namespaced: false},
  Pod:                   {plural: "pods",                   core: "v1", namespaced: true},
  PersistentVolume:      {plural: "persistentvolumes",      core: "v1", namespaced: false},
  PersistentVolumeClaim: {plural: "persistentvolumeclaims", core: "v1", namespaced: true},
  Secret:                {plural: "secrets",                core: "v1", namespaced: true},
  Event:                 {plural: "events",                 core: "v1", namespaced: true},
  StorageClass:          {plural: "storageclasses",         core: "storage.k8s.io/v1", namespaced: false},
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
  const prefix = !r.core ? `/apis/${API_GROUP}`
    : r.core === "v1" ? "/api/v1"
    : `/apis/${r.core}`;
  const ns = r.namespaced && !o.allNamespaces ? `/namespaces/${o.namespace || (r.dr ? DR_NS() : NS())}` : "";
  const name = o.name ? `/${o.name}` : "";
  const sub = o.subresource ? `/${o.subresource}` : "";
  return `${prefix}${ns}/${r.plural}${name}${sub}`;
}
// apiVersion string for a kind, for the objects the console authors
const apiVersionOf = kind => { const r = RESOURCES[kind]; return !r.core ? API_GROUP : r.core; };

// ---- transport -------------------------------------------------------------
async function call(base, path, init) {
  let res;
  const headers = Object.assign({
    Accept: "application/json",
    Authorization: `Bearer ${SB.token}`
  }, (init && init.headers) || {});
  try {
    res = await fetch(base + path, Object.assign({}, init, {headers}));
  } catch (e) {
    throw new ApiError(0, "Cannot reach the Kubernetes API", path, "Unreachable");
  }
  let body = null;
  try { body = await res.json(); } catch (e) {}
  // A Kubernetes failure is a Status object. Trust the body, not just the code:
  // a Failure Status served with a 2xx is still a failure, and treating it as an
  // empty collection would show "no objects" during an outage.
  const failed = body && body.kind === "Status" && body.status === "Failure";
  if (!res.ok || failed) {
    const msg = (body && body.message) || res.statusText || "Request failed";
    throw new ApiError((body && body.code) || res.status, msg, path, (body && body.reason) || null);
  }
  return body;
}

const k8s = {
  // list: returns items[], never the List envelope
  list: (kind, opts) => call(SB.k8sBase, pathFor(kind, opts) + qs(opts))
    .then(r => (r && r.items) || []),
  get: (kind, name, opts) => call(SB.k8sBase, pathFor(kind, Object.assign({name}, opts))),
  create: (kind, obj, opts) => call(SB.k8sBase, pathFor(kind, opts), {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify(obj)
  }),
  // spec edits go through a merge patch, which is what kubectl edit does
  patch: (kind, name, patch, opts) => call(SB.k8sBase, pathFor(kind, Object.assign({name}, opts)), {
    method: "PATCH",
    headers: {"Content-Type": "application/merge-patch+json"},
    body: JSON.stringify(patch)
  }),
  remove: (kind, name, opts) => call(SB.k8sBase, pathFor(kind, Object.assign({name}, opts)), {method: "DELETE"}),
  // pod logs are the Kubernetes API, not a side channel
  logs: (pod, opts) => call(SB.k8sBase,
    pathFor("Pod", {name: pod, subresource: "log"}) + qs(Object.assign({tailLines: 200}, opts)))
};

function qs(opts) {
  const o = Object.assign({}, opts);
  delete o.namespace; delete o.name; delete o.subresource; delete o.allNamespaces;
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
const review = {
  can: (verb, group, resource, ns, name) => k8s.create("SelfSubjectAccessReview", {
    apiVersion: "authorization.k8s.io/v1", kind: "SelfSubjectAccessReview",
    spec: {resourceAttributes: Object.assign({verb, group: group || "", resource},
      ns ? {namespace: ns} : {}, name ? {name} : {})}
  }).then(r => !!(r.status && r.status.allowed)),
  rules: ns => k8s.create("SelfSubjectRulesReview", {
    apiVersion: "authorization.k8s.io/v1", kind: "SelfSubjectRulesReview", spec: {namespace: ns}
  }).then(r => r.status || {resourceRules: [], incomplete: true}),
  whoami: () => k8s.create("SelfSubjectReview", {apiVersion: "authentication.k8s.io/v1", kind: "SelfSubjectReview"})
    .then(r => (r.status && r.status.userInfo) || {})
};

// label selector helpers — the ownership tree is expressed in labels
const sel = o => Object.entries(o).map(([k, v]) => `${k}=${v}`).join(",");
const ownedBy = (kind, name) => ({labelSelector: sel({
  [`${GROUP}/owner-kind`]: kind, [`${GROUP}/owner-name`]: name})});
const inCluster = name => ({labelSelector: sel({[`${GROUP}/cluster`]: name})});

// ---- operator API ----------------------------------------------------------
// What the operator serves that is not a CRD: the release/compat matrix it
// mirrors from install.simplyblock.io, and the SSE stream.
const operator = {
  releases: () => call(SB.operatorBase, "/releases"),
  health: () => call(SB.operatorBase, "/healthz"),
  // ?watch=true rows in the design are SSE subscriptions
  watch: (kind, onEvent) => {
    const url = SB.operatorBase + "/watch" + pathFor(kind);
    let es;
    try { es = new EventSource(url); } catch (e) { return () => {}; }
    es.onmessage = e => { try { onEvent(JSON.parse(e.data)); } catch (x) {} };
    return () => es.close();
  }
};

// ---- Helm ------------------------------------------------------------------
const helm = {
  releases: () => call(SB.helmBase, "/releases"),
  release: name => call(SB.helmBase, `/releases/${name}`),
  values: name => call(SB.helmBase, `/releases/${name}/values`)
};

// ---- Ops helpers -----------------------------------------------------------
// An action is not a verb against an entity: it is an <Entity>Ops object whose
// spec.action names it. Aborting is a patch of the one mutable field.
function opsKindFor(entityKind) {
  const hit = Object.entries(RESOURCES).find(([, r]) => r.ops === entityKind);
  return hit ? hit[0] : null;
}

function submitOps(entityKind, targetName, action, payload) {
  const kind = opsKindFor(entityKind);
  if (!kind) throw new Error("no Ops kind for " + entityKind);
  const stamp = Date.now().toString(36);
  const body = {
    apiVersion: API_GROUP, kind,
    metadata: {
      name: `${targetName}-${action.toLowerCase()}-${stamp}`.slice(0, 63),
      namespace: NS(),
      labels: {[`${GROUP}/target`]: targetName, [`${GROUP}/action`]: action}
    },
    spec: Object.assign({action}, targetName ? {targetRef: {name: targetName}} : {},
      payload ? {[lowerFirst(action)]: payload} : {})
  };
  return k8s.create(kind, body);
}
const abortOps = (opsKind, name) => k8s.patch(opsKind, name, {spec: {abort: true}});
const lowerFirst = s => s.charAt(0).toLowerCase() + s.slice(1);

// Terminal phases, per the design's phase vocabulary (PascalCase).
const OPS_TERMINAL = ["Succeeded", "Failed", "Aborted"];
const opsRunning = o => o && o.status && !OPS_TERMINAL.includes(o.status.phase);

Object.assign(window, {k8s, operator, helm, review, ApiError, RESOURCES, API_GROUP, GROUP, VERSION, DR_GROUP, DR_API_GROUP,
  pathFor, apiVersionOf, ownedBy, inCluster, sel, submitOps, abortOps, opsKindFor, opsRunning, OPS_TERMINAL, NS, DR_NS});

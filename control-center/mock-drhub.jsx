// ---------------------------------------------------------------------------
// MOCK — DR hub fixtures (dr.simplyblock.io / sitemap.simplyblock.io).
//
// A small, coherent world for the design preview: one cross-cluster plan with
// two declared directions, one stretch-cluster (sync) plan, applications in
// every readiness state, finished and running runs, a schedule, a restore, two
// site profiles and the DRConfig singleton. Writes persist in memory; created
// runs advance through their phases with age. Also answers the three access
// reviews the console asks the API server. Stripped from production images.
// ---------------------------------------------------------------------------
(function () {
  const U = () => window.SB_UTIL;
  const now = () => new Date();
  const iso = d => new Date(d).toISOString();
  const agoIso = mins => iso(Date.now() - mins * 60000);
  const uid = () => U().uuid();
  const meta = (name, ns, extra) => Object.assign({name, uid: uid(), creationTimestamp: agoIso(60 * 24 * 3), generation: 1}, ns ? {namespace: ns} : {}, extra || {});
  const cond = (type, ok, reason, message, mins) => ({type, status: ok ? "True" : "False", reason, message, lastTransitionTime: agoIso(mins || 120)});
  const check = (name, status, blocking, reason, message) => ({name, status, blocking, reason, message});
  const OPS = "ramen-ops";

  const store = {ProtectionPlan: [], DRPath: [], ProtectedApplication: [], RecoveryPlan: [], RecoveryAction: [], TestBubble: [], TestSchedule: [], RestoreAction: [], SiteProfile: [], DRConfig: [], DHCPServer: [], StorageSiteDeployment: [],
    S3ProbeRequest: [], HealthProbeRequest: [], DHCPProbeRequest: [], LabelRequest: [], ManagedCluster: [], ManagedClusterView: []};
  const api = (kind, group) => ({apiVersion: group || "dr.simplyblock.io/v1alpha1", kind});

  // ---- plans ---------------------------------------------------------------
  store.ProtectionPlan.push(Object.assign(api("ProtectionPlan"), {
    metadata: meta("fra"),
    spec: {sites: [{name: "fra-a", cluster: "cluster-a", region: "eu-central"}, {name: "fra-b", cluster: "cluster-b", region: "eu-central"}],
      storageProfile: {storageClassSelector: {matchLabels: {"simplyblock.io/dr": "true"}}, consistencyGroups: "Disabled"},
      methods: [{name: "primary", type: "async", schedulingInterval: "5m"}, {name: "vault", type: "async-s3-backup", schedulingInterval: "15m", s3Backup: {interval: "1h", retention: 48}}],
      s3Profiles: [{site: "fra-a", bucket: "dr-fra-a", endpoint: "https://s3.eu-central-1.amazonaws.com", region: "eu-central-1", secretRef: {name: "ramen-s3-secret"}},
        {site: "fra-b", bucket: "dr-fra-b", endpoint: "https://s3.eu-central-1.amazonaws.com", region: "eu-central-1", secretRef: {name: "ramen-s3-secret"}}],
      autoRestart: {enabled: true, stableFor: "2m"}},
    status: {observedGeneration: 1, sites: [{name: "fra-a", drCluster: "cluster-a", classesApplied: true, agentAvailable: true}, {name: "fra-b", drCluster: "cluster-b", classesApplied: true, agentAvailable: true}],
      pairs: [{sites: ["fra-a", "fra-b"], paths: ["fra-a-to-fra-b", "fra-b-to-fra-a"], drPolicies: ["fra-primary-5m", "fra-vault-15m"], peerClassesResolved: true}],
      s3Stores: [{site: "fra-a", bucket: "dr-fra-a", ok: true, checkedAt: new Date(Date.now() - 120000).toISOString()},
        {site: "fra-b", bucket: "dr-fra-b", ok: true, checkedAt: new Date(Date.now() - 120000).toISOString()}],
      conditions: [cond("Derived", true, "Derived", "DRClusters, DRPolicies and classes applied"), cond("InventoryReady", true, "Reported", "both sites reported"), cond("S3ProfileResolved", true, "Resolved", "profiles sb-3f9a, sb-71c0"), cond("Ready", true, "Ready", "")]}
  }));
  store.ProtectionPlan.push(Object.assign(api("ProtectionPlan"), {
    metadata: meta("metro"),
    spec: {sites: [{name: "metro-1a", cluster: "stretch", zone: "eu-central-1a"}, {name: "metro-1c", cluster: "stretch", zone: "eu-central-1c"}],
      storageProfile: {storageClassSelector: {matchLabels: {"simplyblock.io/stretch": "true"}}, consistencyGroups: "Enabled"},
      methods: [{name: "sync", type: "sync"}]},
    status: {observedGeneration: 1, sites: [{name: "metro-1a", classesApplied: true, agentAvailable: true}, {name: "metro-1c", classesApplied: true, agentAvailable: false}],
      pairs: [{sites: ["metro-1a", "metro-1c"], paths: ["metro-1a-to-1c"], drPolicies: [], peerClassesResolved: true}],
      conditions: [cond("Derived", true, "Derived", "stretch plan: no Ramen objects derived"), cond("InventoryReady", true, "Reported", ""), cond("Ready", false, "AgentUnavailable", "dr-agent on stretch/eu-central-1c has not reported for 12m", 12)]}
  }));

  // A plan whose store on lab-b the S3 service refuses: dr-hub's probe keeps
  // the service's own answer, per site.
  store.ProtectionPlan.push(Object.assign(api("ProtectionPlan"), {
    metadata: meta("lab"),
    spec: {sites: [{name: "lab-a", cluster: "cluster-a"}, {name: "lab-b", cluster: "cluster-b"}],
      storageProfile: {storageClassSelector: {matchLabels: {"simplyblock.io/dr": "true"}}, consistencyGroups: "Disabled"},
      methods: [{name: "primary", type: "async", schedulingInterval: "5m"}],
      s3Profiles: [{site: "lab-a", bucket: "dr-lab-a", endpoint: "https://s3.eu-central-1.amazonaws.com", region: "eu-central-1", secretRef: "ramen-s3-secret"},
        {site: "lab-b", bucket: "dr-lab-b", endpoint: "https://s3.eu-central-1.amazonaws.com", region: "eu-central-1", secretRef: "ramen-s3-secret"}]},
    status: {observedGeneration: 1, sites: [{name: "lab-a", classesApplied: false, agentAvailable: true}, {name: "lab-b", classesApplied: false, agentAvailable: true}], pairs: [],
      s3Stores: [{site: "lab-a", bucket: "dr-lab-a", ok: true, checkedAt: new Date(Date.now() - 60000).toISOString()},
        {site: "lab-b", bucket: "dr-lab-b", ok: false, step: "list", code: "NoSuchBucket", message: "The specified bucket does not exist (HTTP 404, request id 7Q2X9K1M)", checkedAt: new Date(Date.now() - 60000).toISOString()}],
      conditions: [cond("Derived", false, "NoPaths", "no valid DRPath connects two sites of the plan"), cond("InventoryReady", true, "Reported", ""),
        cond("S3ProfileResolved", false, "S3StoreRejected", "the S3 store refused: lab-b (bucket dr-lab-b, eu-central-1): NoSuchBucket: The specified bucket does not exist (HTTP 404, request id 7Q2X9K1M) (list)"),
        cond("Ready", false, "Waiting", "waiting for Derived, S3ProfileResolved")]}
  }));

  // ---- paths ---------------------------------------------------------------
  const path = (name, from, to, plan, actions, extra, status) => Object.assign(api("DRPath"), {metadata: meta(name),
    spec: Object.assign({from, to, planRef: plan, actions, announcementHandover: false}, extra || {}),
    status: Object.assign({drPolicies: plan === "fra" ? ["fra-primary-5m", "fra-vault-15m"] : [], profileConsistency: "Consistent",
      profileComparison: ["inventory-reported", "logical-networks", "guest-networks", "address-pools", "domains", "registry-mirror", "storage-classes", "zones", "test-target"].map(f => ({field: f, status: "Pass", message: ""})),
      conditions: [cond("Valid", true, "Valid", "")]}, status || {})});
  store.DRPath.push(path("fra-a-to-fra-b", "fra-a", "fra-b", "fra", ["Failover", "Relocate", "Test"], {test: {mode: "bubble", isolatedNad: "dr-test/isolated", quotas: {maxCloneCapacity: "2Ti"}, recentWithin: "720h"}},
    {applications: [`${OPS}/shop`, `${OPS}/ledger`, "payments/payments"], lastActions: [{kind: "Test", ref: `${OPS}/test-shop-w4`, phase: "Completed", completionTime: agoIso(60 * 30)}, {kind: "Relocate", ref: `${OPS}/relocate-ledger-1`, phase: "Completed", completionTime: agoIso(60 * 24 * 9)}]}));
  store.DRPath.push(path("fra-b-to-fra-a", "fra-b", "fra-a", "fra", ["Relocate"], {},
    {applications: [`${OPS}/shop`, `${OPS}/ledger`, "payments/payments"], profileConsistency: "Inconsistent",
      profileComparison: [["inventory-reported", "Pass", ""], ["logical-networks", "Warn", "role backend bound on fra-a only"], ["storage-classes", "Pass", ""], ["zones", "Pass", ""], ["test-target", "NotApplicable", "no Test declared"]].map(([f, s, m]) => ({field: f, status: s, message: m}))}));
  store.DRPath.push(path("metro-1a-to-1c", "metro-1a", "metro-1c", "metro", ["Relocate", "Failover"], {}, {applications: [`${OPS}/vm-erp`]}));

  // ---- applications ------------------------------------------------------------
  const readyChecks = (test, extra) => [check("path-declared", "Pass", true, "Declared", ""), check("protection-bound", "Pass", true, "Bound", "DRPC Deployed"),
    check("at-path-source", "Pass", true, "AtSource", ""), check("ramen-healthy", "Pass", true, "Healthy", ""), check("storage-replicating", "Warn", false, "Phase1", "replication lag not reported by the driver yet"),
    check("recipe-valid", "Pass", true, "Valid", ""), check("executor-ready", "Pass", true, "InCluster", ""),
    check("test-prereqs", test ? "Pass" : "NotApplicable", true, test ? "Ready" : "NoTest", ""), check("test-recent", test ? "Pass" : "NotApplicable", false, test ? "Recent" : "NoTest", test ? "passed 30m ago" : ""),
    check("profile-consistent", "Pass", false, "Consistent", "")].concat(extra || []);
  const app = (name, ns, plan, source, target, kind, extra, paths, status) => Object.assign(api("ProtectedApplication"), {metadata: meta(name, ns, (extra || {}).meta),
    spec: Object.assign({planRef: plan, source, target, kind}, (extra || {}).spec || {}),
    status: Object.assign({currentCluster: source === "fra-a" ? "cluster-a" : source === "fra-b" ? "cluster-b" : "stretch", paths, conditions: [cond("Bound", true, "Bound", ""), cond("Protected", true, "Protected", "")]}, status || {})});
  store.ProtectedApplication.push(app("shop", OPS, "fra", "fra-a", "fra-b", "discovered",
    {spec: {method: "primary", discovered: {protectedNamespaces: ["shop"], pvcSelector: {matchLabels: {app: "shop"}}}, tiers: [{name: "db", selector: {resourceTypes: ["statefulsets"], matchLabels: {tier: "db"}}, ready: [{type: "statefulSetsReady"}]}, {name: "web", selector: {resourceTypes: ["deployments"]}, ready: [{type: "deploymentsReady"}]}],
      health: {probes: [{name: "storefront", type: "http", target: "http://shop.shop.svc/healthz", expectStatus: 200}]}}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "Degraded", checks: readyChecks(true), lastTransitionTime: agoIso(600)}},
     {name: "fra-b-to-fra-a", from: "fra-b", to: "fra-a", actions: ["Relocate"], readiness: {verdict: "Degraded", checks: readyChecks(false, [check("at-path-source", "Fail", true, "NotAtSource", "application runs on cluster-a, path starts at fra-b")]).map(c => c.name === "at-path-source" && c.status === "Pass" ? null : c).filter(Boolean), lastTransitionTime: agoIso(600)}}],
    {drpc: `${OPS}/shop`, placement: `${OPS}/shop-placement`, drPolicy: "fra-primary-5m", recipe: {name: "shop", namespace: OPS, generated: true, hash: "9c1e2f"}, lastAction: `${OPS}/test-shop-w4`, siteMapping: "NotApplicable"}));
  // a KubeVirt application on the cross-cluster plan: one NAD resolved by role, one open; guests on the backend role (ADR 0020)
  store.ProtectedApplication.push(app("erp-vms", OPS, "fra", "fra-a", "fra-b", "discovered",
    {spec: {method: "primary", discovered: {protectedNamespaces: ["erp"], pvcSelector: {matchLabels: {app: "erp"}}}, tiers: [{name: "db", selector: {resourceTypes: ["virtualmachines"], matchLabels: {tier: "db"}}, ready: [{type: "vmRunning"}]}, {name: "app", selector: {resourceTypes: ["virtualmachines"], matchLabels: {tier: "app"}}, ready: [{type: "vmRunning"}]}]}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "NotReady", checks: readyChecks(true).concat([check("findings-resolved", "Fail", true, "Open", "erp/erp-app network mgmt: role mgmt unbound on fra-b"), check("artifacts-current", "Pass", false, "Current", "sitemap-live 7c1d2e on cluster-b")]), lastTransitionTime: agoIso(40)}},
     {name: "fra-b-to-fra-a", from: "fra-b", to: "fra-a", actions: ["Relocate"], readiness: {verdict: "NotReady", checks: [check("at-path-source", "Fail", true, "NotAtSource", "application runs on cluster-a")], lastTransitionTime: agoIso(600)}}],
    {drpc: `${OPS}/erp-vms`, drPolicy: "fra-primary-5m", recipe: {name: "erp-vms", namespace: OPS, generated: true, hash: "e0e0aa"}, siteMapping: "Open",
      renderings: {"velero/sitemap-live@cluster-b": "7c1d2e", "dhcp/sitemap-hosts@cluster-b": "91ab00"},
      mapping: {site: "fra-a", counts: {open: 2, resolved: 3},
        findings: [
          {vm: "erp/erp-db", network: "backend", index: 1, category: "nad", value: "apps/backend", networkName: "apps/backend", paths: [{path: "fra-a-to-fra-b", site: "fra-b", cluster: "cluster-b", result: "Resolved", strategy: "map", role: "backend", to: "apps/vlan210-backend"}]},
          {vm: "erp/erp-app", network: "backend", index: 1, category: "nad", value: "apps/backend", networkName: "apps/backend", paths: [{path: "fra-a-to-fra-b", site: "fra-b", cluster: "cluster-b", result: "Resolved", strategy: "map", role: "backend", to: "apps/vlan210-backend"}]},
          {vm: "erp/erp-app", network: "mgmt", index: 2, category: "nad", value: "apps/mgmt", networkName: "mgmt", paths: [{path: "fra-a-to-fra-b", site: "fra-b", cluster: "cluster-b", result: "Open", reason: "role mgmt is not bound on the target profile", candidates: ["apps/vlan220-mgmt", "infra/mgmt-b"]}]}],
        guests: [
          {vm: "erp/erp-db", network: "backend", mac: "52:54:00:a1:b2:01", role: "backend", currentSite: "fra-a", ips: {"fra-a": "192.168.110.21", "fra-b": "192.168.210.21"}, result: "Resolved",
            reservations: [{site: "fra-a", cluster: "cluster-a", ip: "192.168.110.21", dhcpServer: "dhcp-cluster-a", result: "Resolved"}, {site: "fra-b", cluster: "cluster-b", path: "fra-a-to-fra-b", ip: "192.168.210.21", dhcpServer: "dhcp-cluster-b", result: "Resolved"}]},
          {vm: "erp/erp-app", network: "backend", mac: "", role: "backend", currentSite: "fra-a", ips: {"fra-a": "192.168.110.22"}, result: "Open", reason: "MAC not pinned: KubeVirt would assign a new one on restore",
            reservations: [{site: "fra-a", cluster: "cluster-a", ip: "192.168.110.22", dhcpServer: "dhcp-cluster-a", result: "Open", reason: "MAC not pinned"}, {site: "fra-b", cluster: "cluster-b", path: "fra-a-to-fra-b", result: "Open", reason: "MAC not pinned"}]}]}}));
  store.ProtectedApplication.push(app("ledger", OPS, "fra", "fra-a", "fra-b", "discovered",
    {spec: {method: "vault", discovered: {protectedNamespaces: ["ledger"], pvcSelector: {matchLabels: {app: "ledger"}}, recipeRef: {name: "ledger-recipe"}}}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "NotReady", checks: readyChecks(true).map(c => c.name === "ramen-healthy" ? check("ramen-healthy", "Fail", true, "VRGUnhealthy", "VolumeReplicationGroup ledger: DataProtected=False (lastGroupSyncTime 2h old)") : c), lastTransitionTime: agoIso(115)}},
     {name: "fra-b-to-fra-a", from: "fra-b", to: "fra-a", actions: ["Relocate"], readiness: {verdict: "NotReady", checks: [check("at-path-source", "Fail", true, "NotAtSource", "application runs on cluster-a")], lastTransitionTime: agoIso(600)}}],
    {drpc: `${OPS}/ledger`, drPolicy: "fra-vault-15m", recipe: {name: "ledger-recipe", namespace: OPS, generated: false}, lastAction: `${OPS}/relocate-ledger-1`,
      conditions: [cond("Bound", true, "Bound", ""), cond("Protected", false, "ReplicationUnhealthy", "VRG DataProtected=False", 115)]}));
  store.ProtectedApplication.push(app("payments", "payments", "fra", "fra-a", "fra-b", "managed",
    {spec: {method: "primary", managed: {placementRef: {name: "payments"}, pvcSelector: {matchLabels: {app: "payments"}}}, dependsOn: ["ledger"]}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "Ready", checks: readyChecks(true).map(c => c.name === "storage-replicating" ? check("storage-replicating", "Pass", false, "Replicating", "lag 42s") : c), lastTransitionTime: agoIso(3000)}},
     {name: "fra-b-to-fra-a", from: "fra-b", to: "fra-a", actions: ["Relocate"], readiness: {verdict: "NotReady", checks: [check("at-path-source", "Fail", true, "NotAtSource", "application runs on cluster-a")], lastTransitionTime: agoIso(3000)}}],
    {drpc: "payments/payments", placement: "payments/payments", drPolicy: "fra-primary-5m", recipe: {name: "payments", namespace: "payments", generated: true, hash: "0b77aa"}}));
  // a consistency-group relocate stuck in the target's restore (2026-10-03): Ramen keeps retrying, the action ended
  store.ProtectedApplication.push(app("wiki", OPS, "fra", "fra-a", "fra-b", "discovered",
    {spec: {method: "primary", discovered: {protectedNamespaces: ["wiki"], pvcSelector: {matchLabels: {app: "wiki"}}}}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "NotReady",
      checks: readyChecks(true).concat([check("move-settled", "Fail", true, "MoveStuck", "relocate fra-a→fra-b since 2026-10-03T23:17:24Z is stuck: ClusterDataReady: Failed to restore PVs/PVCs: destination volume ID is empty for VGRC vgrcontent-a5b8. Resume it once its cause is fixed, or Revert it to fra-a")]), lastTransitionTime: agoIso(40)}},
     {name: "fra-b-to-fra-a", from: "fra-b", to: "fra-a", actions: ["Relocate"], readiness: {verdict: "NotReady", checks: [check("at-path-source", "Fail", true, "NotAtSource", "application runs on cluster-a")], lastTransitionTime: agoIso(40)}}],
    {drpc: `${OPS}/wiki`, drPolicy: "fra-primary-5m", recipe: {name: "wiki", namespace: OPS, generated: true, hash: "51aa0e"}, lastAction: `${OPS}/relocate-wiki-1`,
      move: {action: "Relocate", from: "fra-a", to: "fra-b", phase: "Stuck", since: agoIso(55), progression: "WaitForReadiness",
        blocking: "ClusterDataReady: Failed to restore PVs/PVCs: destination volume ID is empty for VGRC vgrcontent-a5b8", revertible: true},
      conditions: [cond("Bound", true, "Bound", ""), cond("Protected", false, "Error", "VolumeReplicationGroup on cluster-b is reporting errors", 40)]}));
  store.ProtectedApplication.push(app("vm-erp", OPS, "metro", "metro-1a", "metro-1c", "discovered",
    {spec: {discovered: {protectedNamespaces: ["erp"], pvcSelector: {matchLabels: {"kubevirt.io/domain": "erp"}}}, tiers: [{name: "vm", selector: {resourceTypes: ["virtualmachines"]}, ready: [{type: "vmRunning"}]}]}},
    [{name: "metro-1a-to-1c", from: "metro-1a", to: "metro-1c", actions: ["Relocate", "Failover"], readiness: {verdict: "NotReady", checks: [check("path-declared", "Pass", true, "Declared", ""), check("zone-protected", "Pass", true, "Bound", "zone binding eu-central-1a"), check("executor-ready", "Fail", true, "AgentUnavailable", "dr-agent on stretch has not reported for 12m"), check("recipe-valid", "Pass", true, "Valid", "")], lastTransitionTime: agoIso(12)}}],
    {zoneBinding: `dr-zone-binding-${OPS}-vm-erp`, currentCluster: "stretch/eu-central-1a", recipe: {name: "vm-erp", namespace: OPS, generated: true, hash: "44d1c9"}, siteMapping: "Unknown"}));
  store.ProtectedApplication.push(app("archive", OPS, "fra", "fra-a", "fra-b", "discovered",
    {meta: {annotations: {"dr.simplyblock.io/awaiting-restore": "2026-09-28T07:12:00Z"}}, spec: {method: "vault", discovered: {protectedNamespaces: ["archive"], pvcSelector: {matchLabels: {app: "archive"}}}}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "NotReady", checks: [check("protection-bound", "Fail", true, "AwaitingRestore", "volumes not restored yet")], lastTransitionTime: agoIso(300)}}],
    {conditions: [cond("Bound", false, "AwaitingRestore", "", 300), cond("Protected", false, "AwaitingRestore", "", 300)]}));

  // ---- recovery plan --------------------------------------------------------
  store.RecoveryPlan.push(Object.assign(api("RecoveryPlan"), {metadata: meta("tier-1", OPS),
    spec: {pathRef: "fra-a-to-fra-b", applications: [{name: "ledger", priority: 1}, {name: "shop", priority: 2, dependsOn: ["ledger"]}], gates: {betweenPriorities: "allHealthy"}},
    status: {readiness: {verdict: "NotReady", checks: [check("plan-order", "Pass", true, "Valid", ""), check("app/ledger", "Fail", true, "NotReady", "ramen-healthy failed"), check("app/shop", "Warn", false, "Degraded", "storage-replicating")]}, conditions: [cond("Valid", true, "Valid", "")]}}));

  // ---- runs -----------------------------------------------------------------------
  const step = (name, result, startMin, durS, message, logRef) => ({name, result, startTime: agoIso(startMin), endTime: result === "Running" ? undefined : iso(Date.now() - startMin * 60000 + durS * 1000), message, logRef, idempotencyKey: U().hex(8)});
  // status.log entries (dr-hub ADR 0022): [minutes ago, severity, step, source, message]
  const logOf = rows => rows.map(([min, severity, stepName, source, message]) => ({time: agoIso(min), severity, step: stepName, source, message}));
  store.RecoveryAction.push(Object.assign(api("RecoveryAction"), {metadata: meta("relocate-ledger-1", OPS, {annotations: {"dr.simplyblock.io/created-by": "alice@example.com"}, creationTimestamp: agoIso(60 * 24 * 9 + 20)}),
    spec: {kind: "Relocate", pathRef: "fra-b-to-fra-a", applicationRef: {name: "ledger"}, timeout: "30m"},
    status: {phase: "Completed", startTime: agoIso(60 * 24 * 9 + 20), completionTime: agoIso(60 * 24 * 9), sourceCluster: "cluster-b", targetCluster: "cluster-a",
      steps: [step("pre-flight", "Succeeded", 60 * 24 * 9 + 20, 4, "Ready"), step("pre-source hooks", "Succeeded", 60 * 24 * 9 + 19, 41, "quiesce-db ok", "cluster-b/simplyblock-dr-agent/task-4f1a"), step("ramen relocate", "Succeeded", 60 * 24 * 9 + 18, 612, "DRPC Relocated"),
        step("target starting", "Succeeded", 60 * 24 * 9 + 8, 210, "tiers db, web ready"), step("post-target hooks", "Succeeded", 60 * 24 * 9 + 4, 12, ""), step("confirming", "Succeeded", 60 * 24 * 9 + 3, 30, "probes passed")],
      report: {operator: "alice@example.com", rtoSeconds: 1190, achievedRPOSeconds: 0, probes: [{name: "ledger-api", passed: true, message: "200 in 140ms", time: agoIso(60 * 24 * 9)}], hooks: [{point: "preSource", name: "quiesce-db", result: "Succeeded", durationSeconds: 41}], warnings: [], preFlight: {verdict: "Ready", checks: readyChecks(false)}},
      reportKey: "dr/reports/ramen-ops/recoveryaction/2026/09/20260920T101200Z-relocate-ledger-1-8f2a1c0d.json", conditions: [cond("Completed", true, "Completed", "", 60 * 24 * 9)]}}));
  store.RecoveryAction.push(Object.assign(api("RecoveryAction"), {metadata: meta("failover-payments-x7", "payments", {annotations: {"dr.simplyblock.io/created-by": "bob@example.com"}, creationTimestamp: agoIso(60 * 24 * 2)}),
    spec: {kind: "Failover", pathRef: "fra-a-to-fra-b", applicationRef: {name: "payments"}, override: {reason: "storage-replicating advisory only; site A network partitioned, business decision to fail over"}, timeout: "30m"},
    status: {phase: "Failed", startTime: agoIso(60 * 24 * 2), completionTime: agoIso(60 * 24 * 2 - 14), sourceCluster: "cluster-a", targetCluster: "cluster-b",
      steps: [step("pre-flight", "Succeeded", 60 * 24 * 2, 3, "Degraded, overridden"), step("ramen failover", "Succeeded", 60 * 24 * 2 - 1, 480, "DRPC FailedOver"), step("target starting", "Failed", 60 * 24 * 2 - 9, 300, "the application did not pass its health probes on cluster-b: task deadline of 900s passed; root cause: Pod payments/payments-web-6c9 is ImagePullBackOff: web: Back-off pulling image \"quay.io/acme/payments-web:4.2\": registry mirror not bound on fra-b | recent: 09:12:40 probes: probes on cluster-b: 0 of 1 passing after 58 attempts; VMs 0, pods 1/2 ready, PVCs 2 / 09:13:10 pods: Pod payments/payments-web-6c9 is ImagePullBackOff")],
      log: logOf([[60 * 24 * 2, "Info", "pre-flight", "workflow", "started (PreFlight)"], [60 * 24 * 2, "Warning", "", "workflow", "started against readiness Degraded (storage-replicating), overridden: storage-replicating advisory only"],
        [60 * 24 * 2 - 1, "Info", "ramen failover", "ramen", "Ramen: DRPC payments FailingOver, WaitingForResourceRestore"], [60 * 24 * 2 - 8, "Info", "ramen failover", "workflow", "succeeded: Ramen reports FailedOver on cluster-b"],
        [60 * 24 * 2 - 9, "Info", "target starting", "probes", "probes on cluster-b: 0 of 1 passing after 3 attempts; VMs 0, pods 1/2 ready, PVCs 2"],
        [60 * 24 * 2 - 9, "Warning", "target starting", "pods", "Pod payments/payments-web-6c9 is ImagePullBackOff: web: Back-off pulling image \"quay.io/acme/payments-web:4.2\": registry mirror not bound on fra-b"],
        [60 * 24 * 2 - 10, "Warning", "target starting", "events", "Pod/payments-web-6c9 Failed: Failed to pull image: dial tcp: lookup mirror.fra-b.example.com: no such host"],
        [60 * 24 * 2 - 12, "Warning", "target starting", "workflow", "stuck: Pod payments/payments-web-6c9 is ImagePullBackOff: web: Back-off pulling image \"quay.io/acme/payments-web:4.2\": registry mirror not bound on fra-b"],
        [60 * 24 * 2 - 14, "Error", "target starting", "workflow", "failed: the application did not pass its health probes on cluster-b: task deadline of 900s passed"],
        [60 * 24 * 2 - 14, "Error", "", "workflow", "Failed: the application did not pass its health probes on cluster-b (Ramen moved it; no rollback)"]]),
      report: {operator: "bob@example.com", overrideReason: "storage-replicating advisory only; site A network partitioned, business decision to fail over", rtoSeconds: null, achievedRPOSeconds: 240, probes: [], hooks: [], warnings: ["registry mirror role unbound on target site profile"], preFlight: {verdict: "Degraded", checks: readyChecks(true)},
        guests: [{vm: "payments/pay-vm-0", network: "backend", expectedIP: "192.168.210.40", observedIP: "192.168.210.40", match: true}, {vm: "payments/pay-vm-1", network: "backend", expectedIP: "192.168.210.41", observedIP: "192.168.210.133", match: false}]},
      reportKey: "dr/reports/payments/recoveryaction/2026/09/20260927T091500Z-failover-payments-x7-1a2b3c4d.json", conditions: [cond("Completed", false, "Failed", "target starting failed", 60 * 24 * 2 - 14)]}}));
  store.RecoveryAction.push(Object.assign(api("RecoveryAction"), {metadata: meta("relocate-shop-live", OPS, {annotations: {"dr.simplyblock.io/created-by": "alice@example.com"}, creationTimestamp: agoIso(6)}),
    spec: {kind: "Relocate", pathRef: "fra-a-to-fra-b", applicationRef: {name: "shop"}, timeout: "30m"},
    status: {phase: "TargetStarting", startTime: agoIso(6), sourceCluster: "cluster-a", targetCluster: "cluster-b",
      steps: [step("pre-flight", "Succeeded", 6, 3, "Degraded"), step("pre-source hooks", "Succeeded", 6, 20, ""),
        Object.assign(step("ramen-move", "Running", 5, 0, ""), {phase: "TargetStarting", deadline: iso(Date.now() + 25 * 60000), lastProgressTime: agoIso(4),
          progress: "Ramen reports Relocated on cluster-b; its restore of the application's objects is not done: Failed to restore kube objects: kube objects restore error, will retry",
          blocker: "no progress for 4m0s: Ramen's restore waits for hook tools-0-deployments: selector shop-tools= matches nothing in shop on cluster-b (a label with an empty value: was a key=value meant?); the tiers after it are not restored until it passes"})],
      log: logOf([[6, "Info", "pre-flight", "workflow", "started (PreFlight)"], [6, "Info", "pre-flight", "workflow", "succeeded: Relocate cluster-a→cluster-b, readiness Degraded"],
        [6, "Info", "pre-source hooks", "agent", "hook 1 of 1 quiesce running on cluster-a: flushing caches"], [5, "Info", "ramen-move", "ramen", "Ramen: DRPC shop Relocating, RunningFinalSync"],
        [4, "Info", "ramen-move", "ramen", "Ramen reports Relocated on cluster-b; its restore of the application's objects is not done: Failed to restore kube objects: kube objects restore error, will retry"],
        [4, "Error", "ramen-move", "ramen", "Ramen's restore waits for hook tools-0-deployments: selector shop-tools= matches nothing in shop on cluster-b (a label with an empty value: was a key=value meant?); the tiers after it are not restored until it passes"],
        [4, "Error", "ramen-move", "ramen", "Ramen could not restore the application's objects on cluster-b: Failed to restore kube objects: kube objects restore error, will retry"],
        [0.5, "Warning", "ramen-move", "workflow", "stuck: Ramen's restore waits for hook tools-0-deployments: selector shop-tools= matches nothing in shop on cluster-b (a label with an empty value: was a key=value meant?); the tiers after it are not restored until it passes"]]),
      conditions: []}}));
  // A test that waits in its restore on an exec gate whose pod selector matches nothing in the bubble.
  store.TestBubble.push(Object.assign(api("TestBubble"), {metadata: meta("test-ledger-restoring", OPS, {annotations: {"dr.simplyblock.io/created-by": "carol@example.com"}, creationTimestamp: agoIso(14)}),
    spec: {pathRef: "fra-b-to-fra-a", applicationRef: {name: "ledger"}, cloneSource: "latest-replicated-snapshot", maxLifetime: "24h"},
    status: {phase: "Restoring", testID: "r7-2b1e", sourceCluster: "cluster-b", targetCluster: "cluster-a", startTime: agoIso(14), clonesReadyTime: agoIso(10),
      applications: [{name: "ledger", priority: 1, phase: "Restoring"}], bubbleNamespaces: ["ledger-drtest-r72b1e"],
      steps: [step("pre-flight", "Succeeded", 14, 2, "1 applications, cluster-b→cluster-a"), step("clone", "Succeeded", 13, 180, "2 clones ready (1 TestFailovers), consistency per-group"),
        step("isolate", "Succeeded", 10, 20, "1 namespaces isolated"),
        Object.assign(step("restore/ledger", "Running", 9, 0, ""), {phase: "Restoring", deadline: iso(Date.now() + 21 * 60000), lastProgressTime: agoIso(7),
          progress: "step 3 of 5 exec/db-1 (attempt 41): no running pod selected in ledger-drtest-r72b1e yet; VMs 1, pods 0, PVCs 2",
          blocker: "no progress for 7m0s: restore step exec/db-1 on cluster-a waits: no running pod selected in ledger-drtest-r72b1e yet [run \"pg_isready -h ledger-db\" in a running pod selected by labels app=wordpress-tools in ledger-drtest-r72b1e (timeout 900s)]"})],
      log: logOf([[14, "Info", "pre-flight", "workflow", "started (Pending)"], [13, "Info", "clone", "clones", "0 of 1 TestFailovers Ready on cluster-a; waiting: tf-r72b1e-ledger: Cloning (2 of 2 volumes)"],
        [10, "Info", "clone", "workflow", "succeeded: 2 clones ready (1 TestFailovers), consistency per-group"],
        [9, "Info", "restore/ledger", "agent", "step 1 of 5 group/config: 14 objects restored"], [8, "Info", "restore/ledger", "agent", "step 2 of 5 group/db: VirtualMachine ledger-db restored"],
        [7, "Info", "restore/ledger", "agent", "step 3 of 5 exec/db-1 (attempt 2): no running pod selected in ledger-drtest-r72b1e yet; VMs 1, pods 0, PVCs 2"],
        [3, "Warning", "restore/ledger", "gates", "restore step exec/db-1 on cluster-a waits: no running pod selected in ledger-drtest-r72b1e yet [run \"pg_isready -h ledger-db\" in a running pod selected by labels app=wordpress-tools in ledger-drtest-r72b1e (timeout 900s)]"],
        [3, "Warning", "restore/ledger", "workflow", "stuck: restore step exec/db-1 on cluster-a waits: no running pod selected in ledger-drtest-r72b1e yet [run \"pg_isready -h ledger-db\" in a running pod selected by labels app=wordpress-tools in ledger-drtest-r72b1e (timeout 900s)]"]]),
      invariants: [], checks: [], conditions: []}}));
  store.TestBubble.push(Object.assign(api("TestBubble"), {metadata: meta("test-shop-w4", OPS, {labels: {"dr.simplyblock.io/schedule": "shop-weekly"}, annotations: {"dr.simplyblock.io/created-by": "system:serviceaccount:simplyblock-dr:dr-hub"}, creationTimestamp: agoIso(60 * 30 + 25)}),
    spec: {pathRef: "fra-a-to-fra-b", applicationRef: {name: "shop"}, cloneSource: "latest-replicated-snapshot", maxLifetime: "24h"},
    status: {phase: "Completed", testID: "w4-7f21", sourceCluster: "cluster-a", targetCluster: "cluster-b", startTime: agoIso(60 * 30 + 25), clonesReadyTime: agoIso(60 * 30 + 21), completionTime: agoIso(60 * 30),
      applications: [{name: "shop", priority: 1, phase: "Restored", readyTime: agoIso(60 * 30 + 12)}], bubbleNamespaces: ["dr-test-w4-7f21-shop"],
      steps: [step("clone volumes", "Succeeded", 60 * 30 + 25, 240, "3 PVCs cloned from replicated snapshot"), step("restore objects", "Succeeded", 60 * 30 + 21, 300, "Velero restore from capture 19"), step("validate", "Succeeded", 60 * 30 + 16, 200, "tiers ready, probes passed"), step("tear down", "Succeeded", 60 * 30 + 3, 180, "")],
      invariants: [{object: "ramendr.openshift.io/DRPlacementControl ramen-ops/shop", field: "status.phase", before: "Deployed", after: "Deployed"}, {object: "v1/Deployment shop/shop-web", field: "status.readyReplicas", before: "3", after: "3"}],
      checks: [{name: "tiers-ready", status: "Pass", message: ""}, {name: "probes", status: "Pass", message: "storefront 200"}, {name: "isolation", status: "Pass", message: "bubble NAD only"}],
      report: {operator: "system:serviceaccount:simplyblock-dr:dr-hub", outcome: "Passed", testPoint: agoIso(60 * 30 + 30), achievedRPOSeconds: 300, estimatedRTOSeconds: 720, consistency: "crash-consistent", coverage: {exercised: ["volumes", "kube-objects", "tiers", "probes"], notExercised: ["external hooks", "announcement hand-over"]}, warnings: []},
      reportKey: "dr/reports/ramen-ops/testbubble/2026/09/20260928T031500Z-test-shop-w4-77aa11bb.json", conditions: [cond("Outcome", true, "Passed", "", 60 * 30), cond("Completed", true, "Completed", "", 60 * 30)]}}));
  store.TestBubble.push(Object.assign(api("TestBubble"), {metadata: meta("test-payments-hold", "payments", {annotations: {"dr.simplyblock.io/created-by": "carol@example.com"}, creationTimestamp: agoIso(40)}),
    spec: {pathRef: "fra-a-to-fra-b", applicationRef: {name: "payments"}, cloneSource: "latest-replicated-snapshot", holdFor: "2h", maxLifetime: "24h"},
    status: {phase: "Holding", testID: "h1-0c9d", sourceCluster: "cluster-a", targetCluster: "cluster-b", startTime: agoIso(40), clonesReadyTime: agoIso(36),
      applications: [{name: "payments", priority: 1, phase: "Restored", readyTime: agoIso(28)}], bubbleNamespaces: ["dr-test-h1-0c9d-payments"],
      steps: [step("clone volumes", "Succeeded", 40, 200, ""), step("restore objects", "Succeeded", 36, 280, ""), step("validate", "Succeeded", 31, 150, ""), step("hold", "Running", 28, 0, "held for manual verification until 2h")],
      invariants: [{object: "ramendr.openshift.io/DRPlacementControl payments/payments", field: "status.phase", before: "Deployed", after: "Deployed"}], checks: [{name: "tiers-ready", status: "Pass", message: ""}], conditions: []}}));
  store.TestSchedule.push(Object.assign(api("TestSchedule"), {metadata: meta("shop-weekly", OPS),
    spec: {schedule: "0 3 * * 0", template: {pathRef: "fra-a-to-fra-b", applicationRef: {name: "shop"}, cloneSource: "latest-replicated-snapshot"}, retention: {keepLast: 8, keepFor: "1440h"}},
    status: {lastScheduleTime: agoIso(60 * 30 + 25), lastSuccessfulTime: agoIso(60 * 30), conditions: [cond("Valid", true, "Valid", "")]}}));
  store.RestoreAction.push(Object.assign(api("RestoreAction"), {metadata: meta("restore-archive-1", OPS, {creationTimestamp: agoIso(200)}),
    spec: {applicationRef: {name: "archive"}, timeout: "30m"},
    status: {phase: "Restoring", cluster: "cluster-a", capture: {number: 31, s3Profile: "sb-3f9a", startTime: agoIso(260)}, volumes: [{pvc: "archive/data-archive-0", point: agoIso(260)}, {pvc: "archive/wal-archive-0", point: agoIso(260)}], restorePoint: agoIso(260),
      steps: [{name: "locate capture", result: "Succeeded", message: "capture 31 in dr-fra-a"}, {name: "restore volumes", result: "Running", message: "2 of 2 PVCs restoring"}], checks: [], dataLoss: "58m", startTime: agoIso(200)}}));

  // ---- site profiles + config ---------------------------------------------------
  const sprof = (name, zones, extra) => Object.assign(api("SiteProfile", "sitemap.simplyblock.io/v1alpha1"), {metadata: meta(name),
    spec: extra && extra.spec || {},
    status: {reportedAt: agoIso(9), inventory: Object.assign({clusterID: U().uuid(), zones,
      nodes: zones.flatMap((z, i) => [0, 1, 2].map(n => ({name: `${name}-${z.slice(-2)}-w${n}`, zone: z, ready: !(extra && extra.notReady && i === 0 && n === 2), podCIDRs: [`10.${i * 10 + n}.0.0/24`]}))),
      nads: [{namespace: "apps", name: "backend", type: "macvlan", master: "bond0.120", vlan: 120, ipamType: "whereabouts", ipamRanges: ["10.120.0.0/22"]}, {namespace: "dr-test", name: "isolated", type: "bridge", bridge: "br-test", ipamType: "static"}],
      storageClasses: [{name: "sb-dr", driver: "csi.simplyblock.io", labels: {"simplyblock.io/dr": "true"}}, {name: "sb-fast", driver: "csi.simplyblock.io"}], volumeSnapshotClasses: [{name: "sb-snap", driver: "csi.simplyblock.io"}],
      ingressClasses: [{name: "nginx"}], gatewayClasses: [], ipAddressPools: [{ns: "metallb", name: "public", addresses: ["203.0.113.0/26"], autoAssign: true}], ingressDomains: [`apps.${name}.example.com`], baseDomain: `${name}.example.com`,
      registryMirrors: [{source: "quay.io", mirrors: [`mirror.${name}.example.com`]}], podCIDRs: ["10.128.0.0/14"], serviceCIDRs: ["172.30.0.0/16"]}, (extra && extra.inv) || {}), conditions: [cond("InventoryReported", true, "Reported", "", 9)]}});
  store.SiteProfile.push(sprof("cluster-a", ["eu-central-1a"], {spec: {logicalNetworks: [{role: "backend", nad: "apps/backend"}, {role: "mgmt", nad: "apps/mgmt"}], guestNetworks: [{role: "backend", cidr: "192.168.110.0/24", gateway: "192.168.110.1", reservedHostIDs: [1, 2, 254], dhcpServerRef: "dhcp-cluster-a"}], registryMirror: "mirror.cluster-a.example.com"}}));
  store.SiteProfile.push(sprof("cluster-b", ["eu-central-1b"], {notReady: true, spec: {logicalNetworks: [{role: "backend", nad: "apps/vlan210-backend"}], guestNetworks: [{role: "backend", cidr: "192.168.210.0/24", gateway: "192.168.210.1", reservedHostIDs: [1, 2, 254], dhcpServerRef: "dhcp-cluster-b"}]},
    inv: {nads: [{namespace: "apps", name: "vlan210-backend", type: "macvlan", master: "bond0.210", vlan: 210, ipamType: "whereabouts", ipamRanges: ["192.168.210.0/24"]}, {namespace: "apps", name: "vlan220-mgmt", type: "macvlan", master: "bond0.220", vlan: 220}, {namespace: "infra", name: "mgmt-b", type: "bridge", bridge: "br-mgmt"}, {namespace: "dr-test", name: "isolated", type: "bridge", bridge: "br-test", ipamType: "static"}]}}));
  store.SiteProfile[0].status.renderings = {"velero/sitemap-live": "0d0d0d", "dhcp/sitemap-hosts": "5e5e5e"};
  store.SiteProfile[1].status.renderings = {"velero/sitemap-live": "7c1d2e", "dhcp/sitemap-hosts": "91ab00"};
  const dhcp = (name, site, ns, cm, n, gen) => Object.assign(api("DHCPServer", "sitemap.simplyblock.io/v1alpha1"), {metadata: meta(name), spec: {site, type: "dnsmasq", dnsmasq: {namespace: ns, configMap: cm}},
    status: {reservations: n, generation: gen, conditions: [cond("Rendered", true, "Rendered", `${n} reservations`, 30)]}});
  store.DHCPServer.push(dhcp("dhcp-cluster-a", "cluster-a", "dhcp", "sitemap-hosts", 3, "5e5e5e"));
  store.DHCPServer.push(dhcp("dhcp-cluster-b", "cluster-b", "dhcp", "sitemap-hosts", 3, "91ab00"));

  // ---- managed clusters and what their dr-agents report (the forms' choices) ----
  const mc = (name, region, zone, available) => Object.assign(api("ManagedCluster", "cluster.open-cluster-management.io/v1"), {metadata: meta(name, null, {labels: {name, cloud: "Amazon"}}),
    spec: {hubAcceptsClient: true}, status: {clusterClaims: [{name: "region.open-cluster-management.io", value: region}].concat(zone ? [{name: "topology.kubernetes.io/zone", value: zone}] : []),
      conditions: [cond("ManagedClusterConditionAvailable", available !== false, available !== false ? "ManagedClusterAvailable" : "ManagedClusterLeaseUpdateStopped", "")]}});
  store.ManagedCluster.push(mc("cluster-a", "eu-central-1"), mc("cluster-b", "eu-central-1"), mc("stretch", "eu-central-1"), mc("cluster-c", "eu-west-1", null, false));
  const wl = (kind, name, labels, objectLabels) => ({kind, name, labels, objectLabels});
  const pvc = (name, labels) => ({name, labels, storageClass: "sb-dr"});
  const svc = (name, ...ports) => ({name, ports: ports.map(p => ({name: p[0], port: p[1], protocol: "TCP"}))});
  const nsw = (namespace, prot, workloads, services, pvcs) => ({namespace, protected: prot, workloads, services: services.map(s => s.name), serviceDetails: services, pvcs});
  const tierL = t => ({"dr.simplyblock.io/tier": t});
  const agentStatus = (cluster, o) => ({cluster, version: "c4395f0", heartbeat: agoIso(1), veleroNamespace: o.velero,
    inventory: {storageClasses: o.classes, site: {zones: o.zones, regions: ["eu-central-1"], nodes: o.zones.map((z, i) => ({name: `${cluster}-w${i}`, zone: z, region: "eu-central-1", ready: true})), nads: o.nads}},
    workloads: o.workloads, virtualMachines: o.vms || [], dhcpServers: o.dhcp || []});
  const STATUS = {
    "cluster-a": agentStatus("cluster-a", {velero: "velero", zones: ["eu-central-1a"],
      classes: [{name: "sb-dr", driver: "csi.simplyblock.io", labels: {"simplyblock.io/dr": "true", "simplyblock.io/replicated": "true"}}, {name: "sb-fast", driver: "csi.simplyblock.io"}],
      nads: [{namespace: "apps", name: "backend", type: "macvlan", master: "bond0.120", vlan: 120}, {namespace: "apps", name: "mgmt", type: "macvlan", master: "bond0.130", vlan: 130},
        {namespace: "dr-test", name: "isolated", type: "bridge", bridge: "br-test", ipamType: "static"}],
      workloads: [
        nsw("shop", true, [wl("StatefulSet", "shop-db", {app: "shop-db"}, Object.assign({app: "shop"}, {tier: "db"})), wl("Deployment", "shop-web", {app: "shop-web"}, {app: "shop"}), wl("Deployment", "shop-tools", {app: "shop-tools"}, {app: "shop-tools"})],
          [svc("shop-db", ["pg", 5432]), svc("shop", ["http", 80])], [pvc("data-shop-db-0", {app: "shop"})]),
        nsw("erp", true, [wl("VirtualMachine", "erp-db", {"kubevirt.io/domain": "erp-db"}, Object.assign({app: "erp"}, tierL("db"))), wl("VirtualMachine", "erp-app", {"kubevirt.io/domain": "erp-app"}, Object.assign({app: "erp"}, tierL("app")))],
          [svc("erp-db", ["mysql", 3306])], [pvc("erp-db-disk", {app: "erp"}), pvc("erp-app-disk", {app: "erp"})]),
        // not protected yet: what a new protection is chosen from
        nsw("crm", false, [wl("Deployment", "crm-tools", {app: "crm-tools"}, {app: "crm-tools"}),
          wl("VirtualMachine", "crm-db", {"kubevirt.io/domain": "crm-db"}, Object.assign({app: "crm"}, tierL("db"))), wl("VirtualMachine", "crm-web", {"kubevirt.io/domain": "crm-web"}, Object.assign({app: "crm"}, tierL("web")))],
          [svc("crm-db", ["pg", 5432]), svc("crm-web", ["http", 80], ["metrics", 9100])],
          [pvc("crm-db-data", {app: "crm", "storage.simplyblock.io/consistency-group": "crm"}), pvc("crm-web-data", {app: "crm", "storage.simplyblock.io/consistency-group": "crm"}), pvc("scratch", {app: "crm-scratch"})]),
        nsw("ledger", true, [wl("StatefulSet", "ledger", {app: "ledger"}, {app: "ledger"})], [svc("ledger", ["http", 8080])], [pvc("data-ledger-0", {app: "ledger"})]),
        nsw("wiki", true, [wl("Deployment", "wiki", {app: "wiki"}, {app: "wiki"})], [svc("wiki", ["http", 80])], [pvc("wiki-data", {app: "wiki"})])],
      vms: [{namespace: "erp", name: "erp-db", running: true, networks: [{name: "backend", index: 1, networkName: "apps/backend", nad: "apps/backend", mac: "52:54:00:a1:b2:01", ips: ["192.168.110.21"]}]},
        {namespace: "erp", name: "erp-app", running: true, networks: [{name: "backend", index: 1, networkName: "apps/backend", nad: "apps/backend", ips: ["192.168.110.22"]}, {name: "mgmt", index: 2, networkName: "mgmt", nad: "apps/mgmt", ips: ["10.130.0.40"]}]},
        {namespace: "crm", name: "crm-db", running: true, networks: [{name: "app", index: 1, networkName: "apps/backend", nad: "apps/backend", mac: "52:54:00:c0:00:01", ips: ["192.168.110.31"]}]}],
      dhcp: [{namespace: "dhcp", pod: "dnsmasq-6d9f-x2k", owner: "Deployment/dnsmasq", software: "dnsmasq", nads: [{nad: "dhcp/dnsmasq-backend", interface: "app0", ips: ["192.168.110.2"]}],
        ranges: ["192.168.110.100,192.168.110.199,255.255.255.0,1h"], hostsConfigMap: "sitemap-hosts", hostsKey: "sitemap.hosts", configMaps: ["dnsmasq-config", "sitemap-hosts"]},
        {namespace: "dhcp-mgmt", pod: "dnsmasq-mgmt-0", owner: "StatefulSet/dnsmasq-mgmt", software: "dnsmasq", nads: [{nad: "apps/mgmt", interface: "net1", ips: ["10.130.0.2"]}],
          ranges: ["10.130.0.100,10.130.0.200,12h"], hostsConfigMap: "mgmt-hosts", hostsKey: "hosts", configMaps: ["mgmt-hosts"]}]}),
    "cluster-b": agentStatus("cluster-b", {velero: "velero", zones: ["eu-central-1b"],
      classes: [{name: "sb-dr", driver: "csi.simplyblock.io", labels: {"simplyblock.io/dr": "true"}}],
      nads: [{namespace: "apps", name: "vlan210-backend", type: "macvlan", master: "bond0.210", vlan: 210, ipamType: "whereabouts", ipamRanges: ["192.168.210.0/24"]},
        {namespace: "apps", name: "vlan220-mgmt", type: "macvlan", master: "bond0.220", vlan: 220}, {namespace: "dr-test", name: "isolated", type: "bridge", bridge: "br-test", ipamType: "static"}],
      workloads: [nsw("crm-drtest-crm10051200", false, [], [], [])],
      vms: [{namespace: "erp", name: "erp-db", running: false, networks: [{name: "backend", index: 1, networkName: "apps/vlan210-backend", nad: "apps/vlan210-backend", ips: []}]}]}),
    "stretch": agentStatus("stretch", {velero: "openshift-adp", zones: ["eu-central-1a", "eu-central-1c"], classes: [{name: "sb-stretch", driver: "csi.simplyblock.io", labels: {"simplyblock.io/stretch": "true"}}],
      nads: [], workloads: [nsw("erp", true, [], [], [pvc("erp-disk", {"kubevirt.io/domain": "erp"})])]})
  };
  Object.entries(STATUS).forEach(([cluster, st]) => store.ManagedClusterView.push(Object.assign(api("ManagedClusterView", "view.open-cluster-management.io/v1beta1"),
    {metadata: meta("dr-agent-status", cluster), spec: {scope: {kind: "ConfigMap", name: "dr-agent-status", namespace: "dr-agent"}},
      status: {result: {apiVersion: "v1", kind: "ConfigMap", metadata: {name: "dr-agent-status", namespace: "dr-agent"}, data: {"status.json": JSON.stringify(st)}}}})));
  // what a gate test answers: an exec gate passes when its pod and the
  // service:port it reaches exist; a kind gate when its selector matches
  const gateAnswer = (cluster, ns, g, i) => {
    const st = STATUS[cluster] || {workloads: []};
    const w = (st.workloads || []).find(x => x.namespace === ns);
    const sel = g.selector || {};
    const match = labels => Object.entries(sel).every(([k, v]) => (labels || {})[k] === v);
    if (!w) return {index: i, type: g.type, passed: false, message: `namespace ${ns} is not on ${cluster}`};
    if (g.type === "exec") {
      const pod = w.workloads.find(x => match(x.labels));
      if (!pod) return {index: i, type: g.type, passed: false, message: `no running pod in ${ns} matches ${Object.entries(sel).map(([k, v]) => `${k}=${v}`).join(",")} (0 pods match, none running)`};
      const cmd = g.command || [], text = cmd.join(" ");
      const hit = (w.serviceDetails || []).some(s => s.ports.some(p => text.includes(s.name) && text.includes(String(p.port))));
      const code = hit ? 0 : 1;
      return {index: i, type: g.type, passed: hit, pod: `${ns}/${pod.name}-5d9-x`, exitCode: code, durationMillis: hit ? 41 : 3012,
        output: hit ? "" : `nc: ${cmd[cmd.length - 2] || "host"} (${cmd[cmd.length - 1] || "?"}): Connection refused`, message: `${text} exited ${code} in ${ns}/${pod.name}-5d9-x`};
    }
    const kind = {vmRunning: "VirtualMachine", deploymentsReady: "Deployment", statefulSetsReady: "StatefulSet"}[g.type] || "Pod";
    const objs = w.workloads.filter(x => x.kind === kind && match(kind === "Pod" ? x.labels : x.objectLabels));
    return objs.length ? {index: i, type: g.type, passed: true, message: `${objs.length} ${kind}s in ${ns} are ready`}
      : {index: i, type: g.type, passed: false, message: `no ${kind} in ${ns} matches ${Object.entries(sel).map(([k, v]) => `${k}=${v}`).join(",")}`};
  };

  // ---- site storage (StorageSiteDeployment, storage.simplyblock.io/v1alpha2) ----
  const nodeSets = hosts => [{name: "default", groups: [{name: "all", workers: hosts}]}];
  const tpl = name => ({name, vcpuCount: 8, minHugePagesSize: "8G", maxSubsystemCount: 30, stripe: {dataChunks: 1, parityChunks: 1}, enableDriveFormat: true});
  const ssd = (site, spec, status) => Object.assign(api("StorageSiteDeployment", "storage.simplyblock.io/v1alpha2"), {metadata: meta(site, "simplyblock"),
    spec: Object.assign({cluster: site, siteNamespace: "simplyblock", draftName: "site-draft", discover: {enableControlPlaneNodes: true}, approved: false}, spec), status});
  store.StorageSiteDeployment.push(ssd("cluster-a", {sizing: tpl("sb-cluster-a"), approved: true}, {phase: "Online", message: "StorageCluster sb-cluster-a is Online (3 node(s))", workName: "sbsd-1a2b3c",
    draft: {name: "site-draft", phase: "Expanded", approved: true, cluster: tpl("sb-cluster-a"), nodeSets: nodeSets(["a-1", "a-2", "a-3"]), nodeRefs: ["sn-a-1", "sn-a-2", "sn-a-3"]},
    storageCluster: {name: "sb-cluster-a", uuid: uid(), phase: "Online", pool: "sb-cluster-a-pool", nodes: ["a-1", "a-2", "a-3"].map(h => ({name: "sn-" + h, phase: "Online", hostname: h}))},
    conditions: [cond("Delivered", true, "Applied", "the work is applied on the site"), cond("Discovered", true, "Nodes", "3 node(s) in the draft"), cond("Approved", true, "SiteDraft", "the site's draft approved=true"), cond("Ready", true, "Online", "the StorageCluster is Online")]}));
  store.StorageSiteDeployment.push(ssd("cluster-b", {sizing: tpl("sb-cluster-b")}, {phase: "Drafted", message: "the draft awaits approval", workName: "sbsd-4d5e6f",
    draft: {name: "site-draft", phase: "Draft", approved: false, cluster: tpl("sb-cluster-b"), nodeSets: nodeSets(["b-1", "b-2", "b-3"])},
    conditions: [cond("Delivered", true, "Applied", "the work is applied on the site"), cond("Discovered", true, "Nodes", "3 node(s) in the draft"), cond("Approved", false, "SiteDraft", "the site's draft approved=false")]}));
  store.SiteProfile.push(sprof("stretch", ["eu-central-1a", "eu-central-1c"], {spec: {dhcpServerRef: "stretch",
    guestNetworks: [{role: "backend", cidr: "192.168.130.0/24", reservedHostIDs: [1, 2]}]}}));
  store.DHCPServer.push(dhcp("dnsmasq", "stretch", "dhcp", "sitemap-hosts", 0, ""));
  store.DRConfig.push(Object.assign(api("DRConfig"), {metadata: meta("default"),
    spec: {executor: {default: "inCluster"}, agent: {namespace: "simplyblock-dr-agent", statusInterval: "15s", hookImageAllowList: ["quay.io/simplyblock-io/*"]}, ramen: {namespace: "ramen-system", configMapName: "ramen-hub-operator-config", managed: true, veleroNamespace: "velero", opsNamespace: OPS},
      archive: {endpoint: "https://s3.eu-central-1.amazonaws.com", bucket: "dr-fra-a", region: "eu-central-1", prefix: "dr/", credentialsSecretRef: {name: "dr-archive"}, bundle: {interval: "5m", retainDays: 30, signingKeySecretRef: {name: "dr-bundle-key"}}, reportRetainDays: 365},
      bootstrap: {imageRegistry: ""}, retention: {days: 90, keepPerApplication: 10}, featureGates: {csiAddonsReplication: true}},
    status: {observedGeneration: 1, agents: [{cluster: "cluster-a", available: true, version: "0.1.0-dev", veleroNamespace: "velero", lastSeen: agoIso(0)}, {cluster: "cluster-b", available: true, version: "0.1.0-dev", veleroNamespace: "velero", lastSeen: agoIso(1)}, {cluster: "stretch", available: false, version: "0.1.0-dev", veleroNamespace: "velero", lastSeen: agoIso(12)}],
      stack: [{cluster: "cluster-a", version: "0.1.0-dev", applied: true}, {cluster: "cluster-b", version: "0.1.0-dev", applied: true}, {cluster: "stretch", version: "0.1.0-dev", applied: true}],
      lastBundle: {generation: 412, key: "dr/bundle/412.tar", time: agoIso(4), objects: 19}, conditions: [cond("RamenConfigured", true, "Configured", "3 S3 profiles, 2 DRClusters")]}}));

  // ---- simulator: created runs advance with age ------------------------------------
  const ACTION_SEQ = ["Pending", "PreFlight", "PreSource", "RamenHandoff", "TargetStarting", "PostTargetReady", "Confirming", "Completed"];
  const TEST_SEQ = ["Pending", "Cloning", "Provisioning", "Restoring", "Validating", "TearingDown", "Completed"];
  const advance = () => {
    const t = Date.now();
    store.RecoveryAction.filter(a => a.__sim).forEach(a => {
      const age = (t - Date.parse(a.metadata.creationTimestamp)) / 1000, i = Math.min(ACTION_SEQ.length - 1, Math.floor(age / 8));
      a.status.phase = ACTION_SEQ[i];
      a.status.steps = ACTION_SEQ.slice(1, i + 1).map((p, j) => {
        const running = !(j < i - 1 || i === ACTION_SEQ.length - 1), start = Date.parse(a.metadata.creationTimestamp) + (j + 1) * 8000;
        return {name: p, phase: p, result: running ? "Running" : "Succeeded", startTime: iso(start), endTime: running ? undefined : iso(start + 8000), message: running ? "" : p + " done",
          progress: running ? `${p}: waiting (${Math.round(age - (j + 1) * 8)}s)` : undefined, lastProgressTime: running ? iso(t) : undefined, deadline: running ? iso(start + 15 * 60000) : undefined};
      });
      a.status.log = a.status.steps.flatMap(st => [{time: st.startTime, severity: "Info", step: st.name, source: "workflow", message: `started (${st.phase})`}].concat(
        st.result === "Running" ? [{time: st.lastProgressTime, severity: "Info", step: st.name, source: "ramen", message: st.progress}] : [{time: st.endTime, severity: "Info", step: st.name, source: "workflow", message: "succeeded: " + st.message}]));
      if (i === ACTION_SEQ.length - 1 && !a.status.completionTime) { a.status.completionTime = iso(t); a.status.report = {operator: a.metadata.annotations["dr.simplyblock.io/created-by"], rtoSeconds: Math.round(age), achievedRPOSeconds: a.spec.kind === "Failover" ? 240 : 0, warnings: [], probes: [], hooks: []}; a.status.conditions = [cond("Completed", true, "Completed", "", 0)]; }
    });
    store.TestBubble.filter(b => b.__sim).forEach(b => {
      const age = (t - Date.parse(b.metadata.creationTimestamp)) / 1000, i = b.spec.abort ? TEST_SEQ.length - 1 : Math.min(TEST_SEQ.length - 1, Math.floor(age / 8));
      b.status.phase = TEST_SEQ[i];
      b.status.steps = TEST_SEQ.slice(1, i + 1).map((p, j) => {
        const running = !(j < i - 1 || i === TEST_SEQ.length - 1), start = Date.parse(b.metadata.creationTimestamp) + (j + 1) * 8000;
        return {name: p, phase: p, result: running ? "Running" : "Succeeded", startTime: iso(start), endTime: running ? undefined : iso(start + 8000), message: running ? "" : p + " done",
          progress: running ? `${p}: waiting (${Math.round(age - (j + 1) * 8)}s)` : undefined, lastProgressTime: running ? iso(t) : undefined, deadline: running ? iso(start + 30 * 60000) : undefined};
      });
      b.status.log = b.status.steps.flatMap(st => [{time: st.startTime, severity: "Info", step: st.name, source: "workflow", message: `started (${st.phase})`}].concat(
        st.result === "Running" ? [{time: st.lastProgressTime, severity: "Info", step: st.name, source: "agent", message: st.progress}] : [{time: st.endTime, severity: "Info", step: st.name, source: "workflow", message: "succeeded: " + st.message}]));
      if (i === TEST_SEQ.length - 1 && !b.status.completionTime) { b.status.completionTime = iso(t); b.status.report = {operator: b.metadata.annotations["dr.simplyblock.io/created-by"], outcome: b.spec.abort ? "Failed" : "Passed", testPoint: b.metadata.creationTimestamp, achievedRPOSeconds: 300, estimatedRTOSeconds: Math.round(age), consistency: "crash-consistent", coverage: {exercised: ["volumes", "kube-objects"], notExercised: []}, warnings: b.spec.abort ? ["aborted by operator"] : []}; }
    });
  };

  // ---- on-demand probes (dr-hub ADR 0021) ----------------------------------
  // The mock's S3: the Secrets in Ramen's namespace, and buckets/endpoints
  // whose names pick the S3 service's answer.
  const S3_SECRETS = ["ramen-s3-secret", "s3-fra"];
  const s3Answer = sp => {
    if (!S3_SECRETS.includes(sp.secretRef)) return {ok: false, code: "SecretNotFound", message: `Secret ramen-system/${sp.secretRef} not found: the store's credentials must be a Secret in Ramen's namespace on the hub`};
    if (/nowhere/.test(sp.endpoint)) return {ok: false, step: "list", code: "DNSError", message: `lookup ${sp.endpoint.replace(/^\w+:\/\//, "")}: no such host`};
    if (/missing/.test(sp.bucket)) return {ok: false, step: "list", code: "NoSuchBucket", message: "The specified bucket does not exist (HTTP 404, request id 17F2A0C3D)"};
    if (/readonly/.test(sp.bucket)) return {ok: false, step: "write", code: "AccessDenied", message: "Access Denied (HTTP 403, request id 9C1B77E2)"};
    return {ok: true};
  };
  const probeOne = p => {
    const name = p.name || p.type;
    if (/down|refused/.test(p.target || "")) return {name, passed: false, message: `dial tcp 10.43.0.12:${p.type === "http" ? 80 : 3306}: connect: connection refused`, time: iso(Date.now())};
    if (p.type === "http") return p.expectStatus && p.expectStatus !== 200 ? {name, passed: false, message: `${p.target} answered 200`, time: iso(Date.now())}
      : {name, passed: true, message: `${p.target} answered 200`, time: iso(Date.now())};
    if (p.type === "tcp") return {name, passed: true, message: `connected to ${p.target}`, time: iso(Date.now())};
    return {name, passed: true, message: "all 2 Running", time: iso(Date.now())};
  };
  const answerProbes = t => {
    store.S3ProbeRequest.filter(o => !o.status.completedAt && t - Date.parse(o.metadata.creationTimestamp) > 800).forEach(o => {
      const a = s3Answer(o.spec), at = iso(t);
      o.status = {phase: a.ok ? "Passed" : "Failed", message: a.ok ? "the store accepts a list, a write and a delete" : `${a.code}: ${a.message}${a.step ? ` (${a.step})` : ""}`, completedAt: at,
        secretNamespace: "ramen-system", result: Object.assign({site: o.spec.site || "", bucket: o.spec.bucket, endpoint: o.spec.endpoint, region: o.spec.region || "", checkedAt: at}, a)};
    });
    store.HealthProbeRequest.filter(o => !o.status.completedAt && t - Date.parse(o.metadata.creationTimestamp) > 1200).forEach(o => {
      const sp = o.spec, at = iso(t);
      const done = (phase, message, extra) => { o.status = Object.assign({}, o.status, {phase, message, completedAt: at}, extra || {}); };
      const app = sp.applicationRef ? findRef("ProtectedApplication", o.metadata.namespace, sp.applicationRef.name) : null;
      if (sp.applicationRef && !app) return done("Error", `ProtectedApplication ${o.metadata.namespace}/${sp.applicationRef.name} not found`);
      const plan = findRef("ProtectionPlan", "", sp.planRef || (app && app.spec.planRef));
      if (!plan) return done("Error", `ProtectionPlan ${sp.planRef || (app && app.spec.planRef)} not found`);
      let site = sp.site, cluster;
      if (site) {
        const s = plan.spec.sites.find(x => x.name === site);
        if (!s) return done("Error", `site ${site} is not a site of ProtectionPlan ${plan.metadata.name}`);
        cluster = s.cluster;
      } else {
        cluster = app.status.currentCluster;
        if (!cluster) return done("Error", "the application does not run anywhere yet (no current cluster); name the site to probe");
        site = (plan.spec.sites.find(x => x.cluster === cluster) || {}).name || "";
      }
      if (sp.gates && sp.gates.length) {
        const nss = sp.namespaces && sp.namespaces.length ? sp.namespaces : app ? ((app.spec.discovered || {}).protectedNamespaces || [app.metadata.namespace]) : [];
        const gates = sp.gates.map((g, i) => gateAnswer(cluster, g.namespace || nss[0], g, i)), failed = gates.filter(g => !g.passed).length;
        return done(failed ? "Failed" : "Passed", failed ? `${failed} of ${gates.length} checks failed on ${cluster}` : `${gates.length} checks passed on ${cluster}`, {site, cluster, gates});
      }
      const probes = (sp.probes && sp.probes.length ? sp.probes : app ? ((app.spec.health || {}).probes || []) : []);
      if (!probes.length) return done("Error", "the application has no health probes", {site, cluster});
      const res = probes.map(probeOne), failed = res.filter(r => !r.passed).length;
      done(failed ? "Failed" : "Passed", failed ? `${failed} of ${res.length} probes failed on ${cluster}` : `${res.length} probes passed on ${cluster}`, {site, cluster, probes: res});
    });
  };

  const answerDHCP = t => store.DHCPProbeRequest.filter(o => !o.status.completedAt && t - Date.parse(o.metadata.creationTimestamp) > 1500).forEach(o => {
    const at = iso(t), sp = o.spec;
    if (!STATUS[sp.cluster]) { o.status = {phase: "Error", message: `ManagedCluster ${sp.cluster} is not managed by this hub`, completedAt: at}; return; }
    // apps/backend answers from the in-cluster dnsmasq, apps/mgmt from a corporate server outside the cluster
    const offers = {"apps/backend": [{serverID: "192.168.110.2", address: "192.168.110.142", subnet: "255.255.255.0", dns: ["192.168.110.2"], domain: "app.lan", leaseSeconds: 3600}],
      "apps/mgmt": [{serverID: "10.130.0.250", address: "10.130.0.77", subnet: "255.255.255.0", router: ["10.130.0.1"], dns: ["10.0.0.53"], domain: "corp.example", leaseSeconds: 86400}]}[sp.nad] || [];
    o.status = offers.length ? {phase: "Passed", message: `${offers.length} DHCP servers answered on ${sp.nad}`, completedAt: at, offers}
      : {phase: "Failed", message: `no DHCP server answered on ${sp.nad} within 6s`, completedAt: at};
  });

  // a LabelRequest relabels the site's reported objects, as dr-agent would
  const LABEL_ALLOW = {StorageClass: ["simplyblock.io/replicated", "simplyblock.io/dr", "simplyblock.io/stretch"], Node: ["topology.kubernetes.io/zone", "topology.kubernetes.io/region"],
    PersistentVolumeClaim: ["app", "storage.simplyblock.io/consistency-group"], VirtualMachine: ["app", "dr.simplyblock.io/tier"], Deployment: ["app", "dr.simplyblock.io/tier"], StatefulSet: ["app", "dr.simplyblock.io/tier"]};
  const answerLabels = t => store.LabelRequest.filter(o => !o.status.completedAt && t - Date.parse(o.metadata.creationTimestamp) > 1200).forEach(o => {
    const at = iso(t), st = STATUS[o.spec.cluster];
    if (!st) { o.status = {phase: "Error", message: `ManagedCluster ${o.spec.cluster} is not managed by this hub`, completedAt: at}; return; }
    const bad = o.spec.changes.map((c, i) => (LABEL_ALLOW[c.kind] || []).includes(c.key) ? null : `change ${i}: ${c.key} on a ${c.kind}`).filter(Boolean);
    if (bad.length) { o.status = {phase: "Error", message: `refused: ${bad.join("; ")}`, completedAt: at}; return; }
    const results = o.spec.changes.map((c, i) => {
      let obj = null;
      if (c.kind === "StorageClass") obj = (st.inventory.storageClasses || []).find(s => s.name === c.name);
      else if (c.kind === "PersistentVolumeClaim") obj = ((st.workloads.find(n => n.namespace === c.namespace) || {}).pvcs || []).find(p => p.name === c.name);
      else if (c.kind !== "Node") { const w = ((st.workloads.find(n => n.namespace === c.namespace) || {}).workloads || []).find(x => x.kind === c.kind && x.name === c.name); if (w) { w.objectLabels = w.objectLabels || {}; obj = {get labels() { return w.objectLabels; }, set labels(x) { w.objectLabels = x; }}; } }
      else { const n = st.inventory.site.nodes.find(x => x.name === c.name); if (n) obj = {labels: {}, node: n}; }
      if (!obj) return {index: i, result: "Failed", message: `${c.kind} ${c.namespace ? c.namespace + "/" : ""}${c.name} not found`};
      if (obj.node) { const f = c.key.endsWith("zone") ? "zone" : "region"; const prev = obj.node[f] || ""; if (c.remove) delete obj.node[f]; else obj.node[f] = c.value; return {index: i, result: "Applied", previous: prev, message: c.remove ? `removed ${c.key}` : `set ${c.key}=${c.value}`}; }
      const labels = Object.assign({}, obj.labels || {}); const prev = labels[c.key] || "";
      if (c.remove) delete labels[c.key]; else labels[c.key] = c.value;
      obj.labels = labels;
      return {index: i, result: "Applied", previous: prev, message: (c.remove ? `removed ${c.key}` : `set ${c.key}=${c.value}`) +
        (c.kind === "PersistentVolumeClaim" && c.key === "storage.simplyblock.io/consistency-group" && !c.remove ? "; the volume exists already: the consistency group is a late join, which takes effect only where the volume is on the group's storage node already, or after a live migration" : "")};
    });
    const view = store.ManagedClusterView.find(v => v.metadata.namespace === o.spec.cluster);
    if (view) view.status.result.data["status.json"] = JSON.stringify(st);
    const failed = results.filter(r => r.result === "Failed").length;
    o.status = {phase: failed ? "Failed" : "Passed", message: failed ? `${failed} of ${results.length} changes failed on ${o.spec.cluster}` : `${results.length} of ${results.length} changes applied or unchanged`,
      completedAt: at, results, requestedBy: ((o.metadata.annotations || {})["dr.simplyblock.io/created-by"]) || ""};
  });


  // ---- AI-assisted discovery (ADR 0023) ------------------------------------------------
  // One site graph (cluster-a) with its data in two gzip shards, bundles in
  // every phase, finished and running runs. localStorage "sb.mock.gitops" =
  // "off" drops the GitOps target (console approval fallback); "sb.mock.ai" =
  // "on" adds a model provider (rules + AI runs).
  const HUBNS = "dr-simplyblock";
  const crcT = (() => { const t = []; for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xEDB88320 ^ (c >>> 1) : c >>> 1; t[n] = c >>> 0; } return t; })();
  const crc32 = b => { let c = 0xFFFFFFFF; for (let i = 0; i < b.length; i++) c = crcT[(c ^ b[i]) & 0xFF] ^ (c >>> 8); return (c ^ 0xFFFFFFFF) >>> 0; };
  // gzip with stored (uncompressed) deflate blocks: valid gzip any gunzip reads
  const gzipStored = text => {
    const data = new TextEncoder().encode(text), out = [0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff];
    for (let i = 0; i < data.length || i === 0; i += 65535) {
      const chunk = data.subarray(i, Math.min(i + 65535, data.length)), last = i + 65535 >= data.length;
      out.push(last ? 1 : 0, chunk.length & 0xff, chunk.length >>> 8, ~chunk.length & 0xff, (~chunk.length >>> 8) & 0xff);
      for (let j = 0; j < chunk.length; j++) out.push(chunk[j]);
      if (!data.length) break;
    }
    const crc = crc32(data), n = data.length;
    out.push(crc & 0xff, (crc >>> 8) & 0xff, (crc >>> 16) & 0xff, crc >>> 24, n & 0xff, (n >>> 8) & 0xff, (n >>> 16) & 0xff, n >>> 24);
    return new Uint8Array(out);
  };
  const b64 = bytes => { let s = ""; for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]); return btoa(s); };
  const gnode = (kind, ns, name, extra) => Object.assign({id: `${kind}/${ns ? ns + "/" : ""}${name}`, kind, namespace: ns || undefined, name}, extra || {});
  const N = {
    web: gnode("Workload", "shop", "web", {role: "web", labels: {"app.kubernetes.io/name": "shop"}, facts: {kind: "Deployment", image: "nginx:1.27"}}),
    db: gnode("Workload", "shop", "db", {role: "db", labels: {"app.kubernetes.io/name": "shop"}, facts: {kind: "StatefulSet", image: "postgres:16", fingerprint: "postgresql"}}),
    svcWeb: gnode("Service", "shop", "web", {facts: {ports: "80"}}), svcDb: gnode("Service", "shop", "db", {facts: {ports: "5432"}}),
    pvcDb: gnode("PVC", "shop", "db-data", {labels: {app: "shop"}}), pvcUp: gnode("PVC", "shop", "web-uploads"),
    erpDb: gnode("VirtualMachine", "erp", "erp-db", {role: "db", facts: {os: "rhel9", fingerprint: "mariadb"}}),
    erpApp: gnode("VirtualMachine", "erp", "erp-app", {role: "app", facts: {os: "rhel9"}}),
    svcErp: gnode("Service", "erp", "erp-app", {facts: {ports: "8080"}}),
    pvcErpDb: gnode("PVC", "erp", "erp-db-disk"), pvcErpApp: gnode("PVC", "erp", "erp-app-disk"),
    nad: gnode("NAD", "apps", "backend", {facts: {vlan: "110"}}),
    rep: gnode("Workload", "reporting", "report-runner", {facts: {kind: "Deployment", image: "metabase:0.49"}}),
    s3: gnode("External", "", "s3.eu-central-1.amazonaws.com:443")
  };
  const ev = (id, source, object, field, detail) => ({id, source, object, field, detail, observed: agoIso(9)});
  const EV = [
    ev("ev-own-web", "inventory", "Deployment shop/web", "spec.template", "owns 2 pods"),
    ev("ev-mnt-db", "inventory", "StatefulSet shop/db", "spec.volumeClaimTemplates[0]", "mounts db-data"),
    ev("ev-mnt-up", "inventory", "Deployment shop/web", "spec.template.spec.volumes[1]", "mounts web-uploads"),
    ev("ev-sel-web", "inventory", "Service shop/web", "spec.selector", "app.kubernetes.io/name=shop,component=web"),
    ev("ev-sel-db", "inventory", "Service shop/db", "spec.selector", "app.kubernetes.io/name=shop,component=db"),
    ev("ev-ref-db", "configScan", "Deployment shop/web", "env.DATABASE_URL", "postgres://db.shop.svc:5432 (host:port only)"),
    ev("ev-flow-db", "flows", "Deployment shop/web", "", "1,284 connections to db.shop:5432 in 7d"),
    ev("ev-fp-db", "fingerprint", "StatefulSet shop/db", "image", "postgres:16 → role db"),
    ev("ev-pkg-shop", "packaging", "Deployment shop/web", "metadata.labels", "app.kubernetes.io/part-of=shop"),
    ev("ev-flow-s3", "flows", "Deployment shop/web", "", "outbound to s3.eu-central-1.amazonaws.com:443"),
    ev("ev-erp-att", "inventory", "VirtualMachine erp/erp-db", "spec.template.spec.networks[1]", "multus apps/backend"),
    ev("ev-erp-att2", "inventory", "VirtualMachine erp/erp-app", "spec.template.spec.networks[1]", "multus apps/backend"),
    ev("ev-erp-mnt", "inventory", "VirtualMachine erp/erp-db", "spec.template.spec.volumes[0]", "dataVolume erp-db-disk"),
    ev("ev-erp-mnt2", "inventory", "VirtualMachine erp/erp-app", "spec.template.spec.volumes[0]", "dataVolume erp-app-disk"),
    ev("ev-erp-flow", "flows", "VirtualMachine erp/erp-app", "", "3,912 connections to 192.168.110.21:3306 (erp-db) in 7d"),
    ev("ev-erp-fp", "fingerprint", "VirtualMachine erp/erp-db", "guest", "mariadb listening on 3306 (guest agent)"),
    ev("ev-rep-flow", "flows", "Deployment reporting/report-runner", "", "214 connections to db.shop:5432 in 7d")
  ];
  const ge = (from, to, kind, weight, evidence, port) => Object.assign({from: from.id, to: to.id, kind, weight, evidence}, port ? {port} : {});
  const EDGES = [
    ge(N.web, N.pvcUp, "mounts", 950, ["ev-mnt-up"]), ge(N.db, N.pvcDb, "mounts", 980, ["ev-mnt-db"]),
    ge(N.svcWeb, N.web, "selects", 900, ["ev-sel-web"], 80), ge(N.svcDb, N.db, "selects", 900, ["ev-sel-db"], 5432),
    ge(N.web, N.svcDb, "references", 820, ["ev-ref-db"], 5432), ge(N.web, N.svcDb, "connects", 870, ["ev-flow-db"], 5432),
    ge(N.web, N.db, "packagedWith", 600, ["ev-pkg-shop"]), ge(N.web, N.s3, "connects", 350, ["ev-flow-s3"], 443),
    ge(N.erpDb, N.nad, "attaches", 700, ["ev-erp-att"]), ge(N.erpApp, N.nad, "attaches", 700, ["ev-erp-att2"]),
    ge(N.erpDb, N.pvcErpDb, "mounts", 980, ["ev-erp-mnt"]), ge(N.erpApp, N.pvcErpApp, "mounts", 980, ["ev-erp-mnt2"]),
    ge(N.erpApp, N.erpDb, "connects", 810, ["ev-erp-flow", "ev-erp-fp"], 3306), ge(N.svcErp, N.erpApp, "selects", 880, []),
    ge(N.rep, N.svcDb, "connects", 420, ["ev-rep-flow"], 5432)
  ];
  const GRAPH = {nodes: Object.values(N), edges: EDGES, evidence: EV};
  const gz = gzipStored(JSON.stringify(GRAPH)), half = Math.ceil(gz.length / 2);
  const SHARDS = [gz.subarray(0, half), gz.subarray(half)].map((b, i, all) => ({apiVersion: "v1", kind: "ConfigMap",
    metadata: {name: `dr-graph-cluster-a-${i}`, namespace: HUBNS, uid: uid(), creationTimestamp: agoIso(9),
      annotations: {"dr.simplyblock.io/shard-generation": "", "dr.simplyblock.io/shard-index": String(i), "dr.simplyblock.io/shard-count": String(all.length)}},
    binaryData: {"data.gz": b64(b)}}));
  store.DiscoveryGraph = [Object.assign(api("DiscoveryGraph"), {metadata: meta("cluster-a"), spec: {site: "cluster-a"},
    status: {observedReport: "a41f0c2e9b7d", built: agoIso(9), shards: SHARDS.map(s => s.metadata.name),
      counts: {nodes: GRAPH.nodes.length, edges: EDGES.length, evidence: EV.length, candidates: 3},
      candidates: [
        {id: "cand-shop", name: "shop", namespaces: ["shop"], members: [N.web.id, N.db.id, N.svcWeb.id, N.svcDb.id, N.pvcDb.id, N.pvcUp.id], score: 910, adopted: `${OPS}/shop`},
        {id: "cand-erp", name: "erp", namespaces: ["erp"], members: [N.erpDb.id, N.erpApp.id, N.svcErp.id, N.pvcErpDb.id, N.pvcErpApp.id], score: 840},
        {id: "cand-reporting", name: "reporting", namespaces: ["reporting"], members: [N.rep.id], score: 420}],
      interApp: [ge(N.rep, N.svcDb, "connects", 420, ["ev-rep-flow"], 5432)],
      conditions: [cond("Built", true, "Built", "graph built from the report of 9m ago", 9)]}}),
    Object.assign(api("DiscoveryGraph"), {metadata: meta("cluster-b"), spec: {site: "cluster-b"},
      status: {counts: {truncated: ["workloads: 100 namespaces cap reached"]}, conditions: [cond("Built", false, "WaitingForReport", "the site's discovery report is incomplete (chunk 3 of 4 missing)", 3)]}})];
  const qs = (id, text, options, blocking, field) => Object.assign({id, text, options}, blocking ? {blocking: true} : {}, field ? {field} : {});
  const fb = (evidence, confidence, note) => Object.assign({evidence, confidence}, note ? {note} : {});
  const dry = (verdict, checks) => ({verdict, checks, lastTransitionTime: agoIso(8)});
  const prop = (name, spec, status) => Object.assign(api("DRProposal"), {metadata: meta(name, OPS, {creationTimestamp: agoIso(status.__age || 8)}), spec, status: Object.assign({}, status, {__age: undefined})});
  const papp = (name, tiers, fields) => ({apiVersion: "dr.simplyblock.io/v1alpha1", kind: "ProtectedApplication", name, namespace: OPS, operation: "create",
    spec: {planRef: "fra", source: "fra-a", target: "fra-b", kind: "discovered", discovered: {protectedNamespaces: [name], pvcSelector: {matchLabels: {app: name}}}, tiers}, fields});
  const gitopsOn = () => (localStorage.getItem("sb.mock.gitops") || "") !== "off";
  const ghRef = (n, state) => ({branch: `dr/bundles/${n}`, pr: `https://github.com/acme/dr-gitops/pull/${n}`, headCommit: "3f9c2d1", state});
  store.DRProposal = [
    prop("shop-cluster-a-r1", {scope: "Application", site: "cluster-a", candidate: "cand-shop", source: "rules", confidence: 900,
      summary: "Adopts the hand-made protection of shop and adds the db → web boot order, a TCP check on db:5432 and the labels its volumes need.",
      objects: [Object.assign(papp("shop", [{name: "db", selector: {matchLabels: {"dr.simplyblock.io/tier": "db"}}}, {name: "web", selector: {matchLabels: {"dr.simplyblock.io/tier": "web"}}}],
        {"tiers[0]": fb(["ev-fp-db", "ev-mnt-db"], 950, "postgres fingerprint"), "tiers[1].ready[0]": fb(["ev-ref-db", "ev-flow-db"], 880, "TCP check db:5432")}), {operation: "update"})],
      labels: [{cluster: "cluster-a", change: {kind: "PersistentVolumeClaim", namespace: "shop", name: "web-uploads", key: "app", value: "shop"}, reason: "member of shop", evidence: ["ev-mnt-up"], effect: "selects"},
        {cluster: "cluster-a", change: {kind: "StatefulSet", namespace: "shop", name: "db", key: "dr.simplyblock.io/tier", value: "db"}, reason: "data service", evidence: ["ev-fp-db"], effect: "tier"}],
      questions: [qs("q1", "Include the outbound S3 endpoint as an external dependency of shop?", ["yes", "no"], false)]},
      {__age: 50, phase: "PROpened", gitOps: ghRef(41, "open"), answers: [{question: "q1", option: "no", by: "alice"}],
        dryRun: dry("Ready", [check("tiers-resolvable", "Pass", true, "Resolved", "2 tiers select 3 objects"), check("pvc-selector-matches", "Pass", true, "Matched", "2 PVCs")]),
        diff: "--- ProtectedApplication ramen-ops/shop (current)\n+++ proposed\n@@ tiers @@\n+- name: db\n+  selector: {matchLabels: {dr.simplyblock.io/tier: db}}\n   - name: web\n+    ready: [{type: exec, command: [nc, -z, db, \"5432\"]}]"}),
    prop("erp-cluster-a-r1", {scope: "Application", site: "cluster-a", candidate: "cand-erp", source: "rules", confidence: 700, summary: "Protects the two ERP VMs.",
      objects: [papp("erp", [{name: "vms", selector: {matchLabels: {app: "erp"}}}], {"tiers[0]": fb(["ev-erp-mnt"], 700)})]},
      {__age: 140, phase: "Superseded", supersededBy: "erp-cluster-a-r2"}),
    prop("erp-cluster-a-r2", {scope: "Application", site: "cluster-a", candidate: "cand-erp", source: "ai:discovery-cluster-a-ai", baseline: "erp-cluster-a-r1", confidence: 820,
      summary: "Splits the ERP VMs into db and app tiers (MariaDB in erp-db, seen through the guest agent and 7 days of flows) and proposes a consistency group for both disks.",
      objects: [papp("erp", [{name: "db", selector: {matchLabels: {"dr.simplyblock.io/tier": "db"}}}, {name: "app", selector: {matchLabels: {"dr.simplyblock.io/tier": "app"}}}],
        {"tiers[0]": fb(["ev-erp-fp", "ev-erp-flow"], 860, "mariadb on 3306"), "tiers[1]": fb(["ev-erp-flow"], 800)})],
      labels: [{cluster: "cluster-a", change: {kind: "VirtualMachine", namespace: "erp", name: "erp-db", key: "dr.simplyblock.io/tier", value: "db"}, reason: "database VM", evidence: ["ev-erp-fp"], effect: "tier"},
        {cluster: "cluster-a", change: {kind: "PersistentVolumeClaim", namespace: "erp", name: "erp-db-disk", key: "storage.simplyblock.io/consistency-group", value: "erp"}, reason: "ERP disks belong together", evidence: ["ev-erp-flow"], effect: "late-join-needs-migration"}],
      migrations: [{cluster: "cluster-a", namespace: "erp", pvc: "erp-db-disk", group: "erp", reason: "bound volume; the group is fixed at creation"},
        {cluster: "cluster-a", namespace: "erp", pvc: "erp-app-disk", group: "erp", reason: "bound volume; the group is fixed at creation"}],
      questions: [qs("q1", "Quiesce MariaDB in erp-db before each replication snapshot?", ["yes, FLUSH TABLES WITH READ LOCK", "no"], true, "tiers[0].hooks"),
        qs("q2", "Is erp-app stateless (its disk may be recreated)?", ["yes", "no"], false)]},
      {__age: 6, phase: "Proposed", dryRun: dry("Degraded", [check("tiers-resolvable", "Pass", true, "Resolved", "2 tiers select 2 VMs"),
        check("pvc-selector-matches", "Fail", true, "NoMatch", "selector app=erp matches 0 PVCs before the labels are applied"), check("consistency-groups", "Warn", false, "LateJoin", "2 bound volumes listed only")])}),
    prop("fra-recovery-r1", {scope: "RecoveryPlan", site: "cluster-a", source: "rules", confidence: 760, dependsOn: ["shop-cluster-a-r1", "erp-cluster-a-r2"],
      summary: "Orders shop before reporting (reporting reads shop's database).",
      objects: [{apiVersion: "dr.simplyblock.io/v1alpha1", kind: "RecoveryPlan", name: "fra-apps", namespace: OPS, operation: "create", spec: {pathRef: "fra-a-to-fra-b", applications: [{name: "shop", priority: 1}, {name: "erp", priority: 2}]},
        fields: {"applications[0].priority": fb(["ev-rep-flow"], 760, "reporting depends on shop")}}]},
      {__age: 30, phase: "WaitingForApplications", gitOps: ghRef(39, "merged"), approvedBy: "bob"}),
    prop("sitemap-cluster-a-b", {scope: "SiteMapping", site: "cluster-a", source: "rules", confidence: 930, summary: "Pairs apps/backend (VLAN 110) with apps/vlan210-backend.",
      objects: [{apiVersion: "sitemap.simplyblock.io/v1alpha1", kind: "SiteProfile", name: "cluster-a", operation: "update", spec: {logicalNetworks: [{role: "backend", nad: "apps/backend"}]}, fields: {"logicalNetworks[0]": fb(["ev-erp-att"], 930)}}]},
      {__age: 300, phase: "Applied", gitOps: ghRef(35, "merged"), approvedBy: "alice", appliedAt: agoIso(280)}),
    prop("reporting-cluster-a-r1", {scope: "Application", site: "cluster-a", candidate: "cand-reporting", source: "rules", confidence: 420, summary: "A single reporting job; low confidence.",
      objects: [papp("reporting", [{name: "app", selector: {matchLabels: {"app.kubernetes.io/name": "metabase"}}}], {})]},
      {__age: 200, phase: "Rejected"})
  ];
  const tc = (mins, tool, summary, result) => ({time: agoIso(mins), tool, summary, result});
  store.DiscoveryRun = [
    Object.assign(api("DiscoveryRun"), {metadata: meta("discovery-cluster-a-rules", OPS, {creationTimestamp: agoIso(52)}), spec: {scope: {site: "cluster-a"}, mode: "Rules"},
      status: {phase: "Succeeded", progress: "3 candidates, 4 bundles", started: agoIso(52), completed: agoIso(51), proposals: ["shop-cluster-a-r1", "erp-cluster-a-r1", "fra-recovery-r1", "reporting-cluster-a-r1"]}}),
    Object.assign(api("DiscoveryRun"), {metadata: meta("discovery-cluster-a-ai", OPS, {creationTimestamp: agoIso(7)}), spec: {scope: {site: "cluster-a", namespaces: ["erp"]}, mode: "AI", provider: "anthropic", instructions: "check the ERP VMs"},
      status: {phase: "Succeeded", progress: "1 bundle revised", started: agoIso(7), completed: agoIso(6), usage: {inputTokens: 48210, outputTokens: 3120, toolCalls: 9, costEstimate: "$0.31"},
        toolLog: [tc(7, "get_graph", "site cluster-a, namespace erp", "ok"), tc(7, "explain_edge", "erp-app → erp-db :3306", "ok"), tc(7, "search_logs", "erp-db: mysqld ready", "ok"), tc(6, "dry_run", "erp-cluster-a-r2", "ok · Degraded"), tc(6, "propose", "erp-cluster-a-r2", "ok")],
        proposals: ["erp-cluster-a-r2"], rejected: [{proposal: "erp-cluster-a-r2-draft", reason: "field tiers[2] cites ev-erp-missing, which is not in the graph"}]}}),
    Object.assign(api("DiscoveryRun"), {metadata: meta("discovery-cluster-b-rules", OPS, {creationTimestamp: agoIso(2)}), spec: {scope: {site: "cluster-b"}, mode: "Rules"},
      status: {phase: "Running", progress: "waiting for the site's complete report", started: agoIso(2)}})
  ];
  const dcfg = store.DRConfig[0];
  const discoveryCfg = () => Object.assign({enabled: true, flows: {optOut: ["stretch"], retention: "168h"}},
    gitopsOn() ? {gitOps: {provider: "github", url: "https://github.com/acme/dr-gitops", baseBranch: "main", path: "dr/bundles", credentialsSecretRef: {name: "dr-gitops"}}} : {},
    (localStorage.getItem("sb.mock.ai") || "") === "on" ? {providers: [{name: "anthropic", type: "anthropic", model: "claude-opus-5-5", credentialsSecretRef: {name: "anthropic-key"}}], defaultProvider: "anthropic"} : {});
  // dr-hub, simulated: requests on bundles and created runs advance with age
  const advanceDisc = () => {
    dcfg.spec.discovery = discoveryCfg();
    const t = Date.now();
    store.DRProposal.forEach(p => {
      const a = p.metadata.annotations || {}, req = a["dr.simplyblock.io/request"];
      if (!req || t - (p.__reqAt || 0) < 1200) return;
      if (req === "open-pr") { p.status.phase = "PROpened"; p.status.gitOps = ghRef(42 + store.DRProposal.indexOf(p), "open"); }
      if (req === "reject") { p.status.phase = "Rejected"; p.status.conditions = [cond("Rejected", true, "ByUser", a["dr.simplyblock.io/request-reason"] || "", 0)]; }
      if (req === "approve") { p.status.phase = "Applied"; p.status.approvedBy = "you@example.com"; p.status.appliedAt = iso(t); }
      if (req === "rollback") p.status.phase = "RolledBack";
      delete a["dr.simplyblock.io/request"]; delete a["dr.simplyblock.io/request-reason"]; delete p.__reqAt;
    });
    store.DiscoveryRun.filter(r => r.__sim).forEach(r => {
      const age = t - Date.parse(r.metadata.creationTimestamp);
      r.status = age < 1500 ? {phase: "Pending"} : age < 4000 ? {phase: "Running", progress: "building the graph", started: r.metadata.creationTimestamp}
        : {phase: "Succeeded", progress: "graph unchanged; no new bundle", started: r.metadata.creationTimestamp, completed: iso(t)};
    });
  };

  const KINDS = Object.keys(store);
  const strip = o => { const c = JSON.parse(JSON.stringify(o)); delete c.__sim; delete c.__reqAt; return c; };
  window.DR_MOCK = {
    has: kind => KINDS.includes(kind),
    list: kind => { advance(); advanceDisc(); answerProbes(Date.now()); answerDHCP(Date.now()); answerLabels(Date.now()); return store[kind].map(strip); },
    // the discovery graph's data shards, in dr-hub's namespace
    configMaps: ns => ns === HUBNS ? SHARDS.map(x => JSON.parse(JSON.stringify(x))) : []
  };
  const viewer = () => (localStorage.getItem("sb.viewas") || "").includes("reader") ? "viewer" : "admin";
  const findRef = (kind, ns, name) => store[kind].find(o => o.metadata.name === name && (!ns || o.metadata.namespace === ns));
  const create = kind => body => {
    const m = body.metadata || {};
    if (!m.name) return {err: "metadata.name is required", reason: "Invalid"};
    // The API server's schema: these references are plain names, not {name}
    // objects. Rejected the way the real server rejects them, so a form that
    // sends the wrong shape fails here too (2026-10-03, DRPath planRef).
    const sp = body.spec || {};
    const mustBeName = {DRPath: ["planRef"], ProtectedApplication: ["planRef"], RecoveryPlan: ["pathRef"], RecoveryAction: ["pathRef"], TestBubble: ["pathRef"]}[kind] || [];
    if (kind === "RecoveryAction" && ["Restart", "Resume", "Revert"].includes(sp.kind) && sp.pathRef !== undefined)
      return {err: `RecoveryAction.dr.simplyblock.io "${m.name}" is invalid: spec: a Restart, Resume or Revert names an application and no path`, reason: "Invalid"};
    for (const f of mustBeName)
      if (sp[f] !== undefined && typeof sp[f] !== "string")
        return {err: `${kind}.dr.simplyblock.io "${m.name}" is invalid: spec.${f}: Invalid value: "object": spec.${f} in body must be of type string: "object"`, reason: "Invalid"};
    if (findRef(kind, m.namespace, m.name)) return {err: `${kind.toLowerCase()}s "${m.name}" already exists`, reason: "AlreadyExists"};
    if (kind === "DHCPProbeRequest" && (!sp.cluster || !/^[a-z0-9-]+\/[a-z0-9.-]+$/.test(sp.nad || ""))) return {err: `DHCPProbeRequest.dr.simplyblock.io "${m.name}" is invalid: spec.cluster and spec.nad (<namespace>/<name>) are required`, reason: "Invalid"};
    if (kind === "S3ProbeRequest" || kind === "HealthProbeRequest") {
      if (viewer() !== "admin") return {err: `${kind.toLowerCase()}s.dr.simplyblock.io is forbidden: User "reader@example.com" cannot create resource "${kind.toLowerCase()}s"`, reason: "Forbidden"};
      if (kind === "S3ProbeRequest" && (!sp.bucket || !sp.endpoint || !sp.secretRef)) return {err: `S3ProbeRequest.dr.simplyblock.io "${m.name}" is invalid: spec.bucket, spec.endpoint and spec.secretRef are required`, reason: "Invalid"};
      if (kind === "HealthProbeRequest" && !sp.applicationRef && !(sp.planRef && sp.site)) return {err: `HealthProbeRequest.dr.simplyblock.io "${m.name}" is invalid: spec: Invalid value: "object": name an applicationRef, or a planRef and a site`, reason: "Invalid"};
    }
    const obj = Object.assign({}, body, {metadata: Object.assign({}, m, {uid: uid(), creationTimestamp: iso(Date.now()), generation: 1,
      annotations: Object.assign({}, m.annotations || {}, ["RecoveryAction", "TestBubble"].includes(kind) ? {"dr.simplyblock.io/created-by": "you@example.com"} : {})}), status: {}});
    if (kind === "RecoveryAction") {
      if (body.spec.override && viewer() !== "admin") return {err: 'admission webhook "vrecoveryaction.dr.simplyblock.io" denied the request: a readiness override needs the "override" verb on recoveryactions, which only dr-admin has', reason: "Forbidden"};
      const app = body.spec.applicationRef && findRef("ProtectedApplication", m.namespace, body.spec.applicationRef.name);
      // Resume and Revert act on the application's in-flight move, not on a path (dr-hub's webhook)
      if (app && (body.spec.kind === "Resume" || body.spec.kind === "Revert")) {
        if (!app.status.move) return {err: `admission webhook denied the request: ProtectedApplication ${app.metadata.name} has no move in progress to ${body.spec.kind.toLowerCase()}`, reason: "Forbidden"};
        if (body.spec.kind === "Revert" && !app.status.move.revertible) return {err: `admission webhook denied the request: the move cannot be reverted: ${app.status.move.revertBlocked}`, reason: "Forbidden"};
      }
      if (app && !["Restart", "Resume", "Revert"].includes(body.spec.kind)) {
        const p = (app.status.paths || []).find(x => x.name === body.spec.pathRef);
        if (!p) return {err: `application ${app.metadata.name} is not on path ${body.spec.pathRef}`, reason: "Invalid"};
        if (p.readiness.verdict === "NotReady" && !body.spec.override) return {err: `admission webhook denied the request: readiness on ${p.name} is NotReady (${p.readiness.checks.filter(c => c.blocking && c.status === "Fail").map(c => c.name).join(", ")}); an override with a reason is required`, reason: "Forbidden"};
      }
      obj.__sim = true; obj.status = {phase: "Pending", startTime: obj.metadata.creationTimestamp, sourceCluster: "cluster-a", targetCluster: body.spec.kind === "Restart" || body.spec.kind === "Revert" ? "cluster-a" : "cluster-b", steps: [], conditions: []};
    }
    if (kind === "TestBubble") { obj.__sim = true; obj.status = {phase: "Pending", testID: U().hex(6), startTime: obj.metadata.creationTimestamp, sourceCluster: "cluster-a", targetCluster: "cluster-b", applications: [{name: (body.spec.applicationRef || body.spec.planRef).name, priority: 1, phase: "Pending"}], bubbleNamespaces: [], steps: [], invariants: [], checks: [], conditions: []}; }
    if (kind === "RestoreAction") obj.status = {phase: "Pending", startTime: obj.metadata.creationTimestamp, steps: [], checks: [], volumes: []};
    if (kind === "TestSchedule") obj.status = {conditions: [cond("Valid", true, "Valid", "", 0)]};
    if (kind === "ProtectionPlan") obj.status = {sites: (body.spec.sites || []).map(s => ({name: s.name, classesApplied: false})), pairs: [], conditions: [cond("Derived", false, "Deriving", "deriving Ramen objects", 0), cond("Ready", false, "Deriving", "", 0)]};
    if (kind === "DRPath") obj.status = {applications: [], drPolicies: [], profileConsistency: "Unknown", profileComparison: [], conditions: [cond("Valid", true, "Valid", "", 0)]};
    if (kind === "ProtectedApplication") obj.status = {paths: [], conditions: [cond("Bound", false, "Binding", "waiting for the DRPC", 0), cond("Protected", false, "Binding", "", 0)]};
    if (kind === "RecoveryPlan") obj.status = {readiness: {verdict: "Unknown", checks: []}, conditions: [cond("Valid", true, "Valid", "", 0)]};
    if (kind === "DHCPServer") obj.status = {reservations: 0, conditions: []};
    if (kind === "DiscoveryRun") { obj.__sim = true; obj.status = {phase: "Pending"}; }
    if (kind === "S3ProbeRequest" || kind === "HealthProbeRequest" || kind === "DHCPProbeRequest" || kind === "LabelRequest") obj.status = {phase: "Running"};
    if (kind === "StorageSiteDeployment") obj.status = {phase: "Discovering", message: `waiting for site ${body.spec.cluster} to write draft simplyblock/site-draft`, conditions: [cond("Delivered", false, "Pending", "the work is not applied on the site yet", 0)]};
    store[kind].push(obj);
    return {obj: strip(obj)};
  };
  const patch = kind => (name, body, ns) => {
    const o = findRef(kind, ns, name);
    if (!o) return {err: `${kind.toLowerCase()}s "${name}" not found`, reason: "NotFound"};
    if (body.spec) {
      if (kind === "TestBubble" && Object.keys(body.spec).some(k => !["abort", "holdFor"].includes(k))) return {err: "only spec.abort and spec.holdFor may change after creation", reason: "Invalid"};
      if (kind === "RecoveryAction") return {err: "the spec of a RecoveryAction is immutable", reason: "Invalid"};
      Object.assign(o.spec, body.spec);
      if (kind === "StorageSiteDeployment" && body.spec.approved && o.status.phase === "Drafted") {
        o.status.phase = "Deploying"; o.status.message = `draft Expanding, StorageCluster ${(o.spec.sizing || {}).name || o.spec.cluster} not reported yet`;
      }
    }
    // dr-hub (simulated) carries out a bundle request a moment after it lands
    if (kind === "DRProposal" && body.metadata && body.metadata.annotations && body.metadata.annotations["dr.simplyblock.io/request"]) o.__reqAt = Date.now();
    if (body.metadata && body.metadata.annotations) { o.metadata.annotations = o.metadata.annotations || {}; Object.entries(body.metadata.annotations).forEach(([k, v]) => { if (v === null) delete o.metadata.annotations[k]; else o.metadata.annotations[k] = v; }); }
    o.metadata.generation = (o.metadata.generation || 1) + 1;
    return {obj: strip(o)};
  };
  const remove = kind => (name, ns) => {
    const o = findRef(kind, ns, name);
    if (!o) return {err: `${kind.toLowerCase()}s "${name}" not found`, reason: "NotFound"};
    const inUse = kind === "ProtectionPlan" ? store.DRPath.some(p => p.spec.planRef === name) : kind === "DRPath" ? store.ProtectedApplication.some(a => (a.status.paths || []).some(p => p.name === name)) : false;
    if (inUse && (o.metadata.annotations || {})["dr.simplyblock.io/confirm-delete"] !== "true") return {err: `admission webhook denied the request: ${kind} ${name} is still in use; annotate dr.simplyblock.io/confirm-delete=true to confirm`, reason: "Forbidden"};
    if ((kind === "RecoveryAction" || kind === "TestBubble") && !["Completed", "Failed", "RolledBack"].includes(o.status.phase)) return {err: "a running run cannot be deleted; abort it first", reason: "Forbidden"};
    store[kind] = store[kind].filter(x => x !== o);
    return {obj: {kind: "Status", status: "Success"}};
  };
  window.CRD_CREATE = Object.assign(window.CRD_CREATE || {}, Object.fromEntries(KINDS.map(k => [k, create(k)])), {
    // access reviews: the viewer switcher stands in for identity — a "reader" sees everything and may write nothing
    SelfSubjectAccessReview: body => { const a = body.spec.resourceAttributes || {}; const ro = ["get", "list", "watch"].includes(a.verb); return {obj: Object.assign({}, body, {status: {allowed: viewer() === "admin" || ro}})}; },
    SelfSubjectRulesReview: body => ({obj: Object.assign({}, body, {status: {incomplete: false, resourceRules: viewer() === "admin"
      ? [{apiGroups: ["dr.simplyblock.io", "sitemap.simplyblock.io", "ramendr.openshift.io", "storage.simplyblock.io", "simplyblock.io"], resources: ["*"], verbs: ["*"]}, {apiGroups: [""], resources: ["*"], verbs: ["get", "list", "watch"]}]
      : [{apiGroups: ["dr.simplyblock.io", "sitemap.simplyblock.io", "ramendr.openshift.io", "storage.simplyblock.io", "simplyblock.io"], resources: ["*"], verbs: ["get", "list", "watch"]}], nonResourceRules: []}})}),
    SelfSubjectReview: body => ({obj: Object.assign({}, body, {status: {userInfo: viewer() === "admin" ? {username: "system:serviceaccount:simplyblock-dr:dr-console", groups: ["system:serviceaccounts"]} : {username: "reader@example.com", groups: ["dr-viewers"]}}})})
  });
  window.CRD_PATCH = Object.assign(window.CRD_PATCH || {}, Object.fromEntries(KINDS.map(k => [k, patch(k)])));
  window.CRD_DELETE = Object.assign(window.CRD_DELETE || {}, Object.fromEntries(KINDS.map(k => [k, remove(k)])));
})();

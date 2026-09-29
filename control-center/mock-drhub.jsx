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

  const store = {ProtectionPlan: [], DRPath: [], ProtectedApplication: [], RecoveryPlan: [], RecoveryAction: [], TestBubble: [], TestSchedule: [], RestoreAction: [], SiteProfile: [], DRConfig: []};
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

  // ---- paths ---------------------------------------------------------------
  const path = (name, from, to, plan, actions, extra, status) => Object.assign(api("DRPath"), {metadata: meta(name),
    spec: Object.assign({from, to, planRef: {name: plan}, actions, announcementHandover: false}, extra || {}),
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
    spec: Object.assign({planRef: {name: plan}, source, target, kind}, (extra || {}).spec || {}),
    status: Object.assign({currentCluster: source === "fra-a" ? "cluster-a" : source === "fra-b" ? "cluster-b" : "stretch", paths, conditions: [cond("Bound", true, "Bound", ""), cond("Protected", true, "Protected", "")]}, status || {})});
  store.ProtectedApplication.push(app("shop", OPS, "fra", "fra-a", "fra-b", "discovered",
    {spec: {method: "primary", discovered: {protectedNamespaces: ["shop"], pvcSelector: {matchLabels: {app: "shop"}}}, tiers: [{name: "db", selector: {resourceTypes: ["statefulsets"], matchLabels: {tier: "db"}}, ready: [{type: "statefulSetsReady"}]}, {name: "web", selector: {resourceTypes: ["deployments"]}, ready: [{type: "deploymentsReady"}]}],
      health: {probes: [{name: "storefront", type: "http", target: "http://shop.shop.svc/healthz", expectStatus: 200}]}}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "Degraded", checks: readyChecks(true), lastTransitionTime: agoIso(600)}},
     {name: "fra-b-to-fra-a", from: "fra-b", to: "fra-a", actions: ["Relocate"], readiness: {verdict: "Degraded", checks: readyChecks(false, [check("at-path-source", "Fail", true, "NotAtSource", "application runs on cluster-a, path starts at fra-b")]).map(c => c.name === "at-path-source" && c.status === "Pass" ? null : c).filter(Boolean), lastTransitionTime: agoIso(600)}}],
    {drpc: `${OPS}/shop`, placement: `${OPS}/shop-placement`, drPolicy: "fra-primary-5m", recipe: {name: "shop", namespace: OPS, generated: true, hash: "9c1e2f"}, lastAction: `${OPS}/test-shop-w4`}));
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
  store.ProtectedApplication.push(app("vm-erp", OPS, "metro", "metro-1a", "metro-1c", "discovered",
    {spec: {discovered: {protectedNamespaces: ["erp"], pvcSelector: {matchLabels: {"kubevirt.io/domain": "erp"}}}, tiers: [{name: "vm", selector: {resourceTypes: ["virtualmachines"]}, ready: [{type: "vmRunning"}]}]}},
    [{name: "metro-1a-to-1c", from: "metro-1a", to: "metro-1c", actions: ["Relocate", "Failover"], readiness: {verdict: "NotReady", checks: [check("path-declared", "Pass", true, "Declared", ""), check("zone-protected", "Pass", true, "Bound", "zone binding eu-central-1a"), check("executor-ready", "Fail", true, "AgentUnavailable", "dr-agent on stretch has not reported for 12m"), check("recipe-valid", "Pass", true, "Valid", "")], lastTransitionTime: agoIso(12)}}],
    {zoneBinding: `dr-zone-binding-${OPS}-vm-erp`, currentCluster: "stretch/eu-central-1a", recipe: {name: "vm-erp", namespace: OPS, generated: true, hash: "44d1c9"}}));
  store.ProtectedApplication.push(app("archive", OPS, "fra", "fra-a", "fra-b", "discovered",
    {meta: {annotations: {"dr.simplyblock.io/awaiting-restore": "2026-09-28T07:12:00Z"}}, spec: {method: "vault", discovered: {protectedNamespaces: ["archive"], pvcSelector: {matchLabels: {app: "archive"}}}}},
    [{name: "fra-a-to-fra-b", from: "fra-a", to: "fra-b", actions: ["Failover", "Relocate", "Test"], readiness: {verdict: "NotReady", checks: [check("protection-bound", "Fail", true, "AwaitingRestore", "volumes not restored yet")], lastTransitionTime: agoIso(300)}}],
    {conditions: [cond("Bound", false, "AwaitingRestore", "", 300), cond("Protected", false, "AwaitingRestore", "", 300)]}));

  // ---- recovery plan --------------------------------------------------------
  store.RecoveryPlan.push(Object.assign(api("RecoveryPlan"), {metadata: meta("tier-1", OPS),
    spec: {pathRef: {name: "fra-a-to-fra-b"}, applications: [{name: "ledger", priority: 1}, {name: "shop", priority: 2, dependsOn: ["ledger"]}], gates: {betweenPriorities: "allHealthy"}},
    status: {readiness: {verdict: "NotReady", checks: [check("plan-order", "Pass", true, "Valid", ""), check("app/ledger", "Fail", true, "NotReady", "ramen-healthy failed"), check("app/shop", "Warn", false, "Degraded", "storage-replicating")]}, conditions: [cond("Valid", true, "Valid", "")]}}));

  // ---- runs -----------------------------------------------------------------------
  const step = (name, result, startMin, durS, message, logRef) => ({name, result, startTime: agoIso(startMin), endTime: result === "Running" ? undefined : iso(Date.now() - startMin * 60000 + durS * 1000), message, logRef, idempotencyKey: U().hex(8)});
  store.RecoveryAction.push(Object.assign(api("RecoveryAction"), {metadata: meta("relocate-ledger-1", OPS, {annotations: {"dr.simplyblock.io/created-by": "alice@example.com"}, creationTimestamp: agoIso(60 * 24 * 9 + 20)}),
    spec: {kind: "Relocate", pathRef: {name: "fra-b-to-fra-a"}, applicationRef: {name: "ledger"}, timeout: "30m"},
    status: {phase: "Completed", startTime: agoIso(60 * 24 * 9 + 20), completionTime: agoIso(60 * 24 * 9), sourceCluster: "cluster-b", targetCluster: "cluster-a",
      steps: [step("pre-flight", "Succeeded", 60 * 24 * 9 + 20, 4, "Ready"), step("pre-source hooks", "Succeeded", 60 * 24 * 9 + 19, 41, "quiesce-db ok", "cluster-b/simplyblock-dr-agent/task-4f1a"), step("ramen relocate", "Succeeded", 60 * 24 * 9 + 18, 612, "DRPC Relocated"),
        step("target starting", "Succeeded", 60 * 24 * 9 + 8, 210, "tiers db, web ready"), step("post-target hooks", "Succeeded", 60 * 24 * 9 + 4, 12, ""), step("confirming", "Succeeded", 60 * 24 * 9 + 3, 30, "probes passed")],
      report: {operator: "alice@example.com", rtoSeconds: 1190, achievedRPOSeconds: 0, probes: [{name: "ledger-api", passed: true, message: "200 in 140ms", time: agoIso(60 * 24 * 9)}], hooks: [{point: "preSource", name: "quiesce-db", result: "Succeeded", durationSeconds: 41}], warnings: [], preFlight: {verdict: "Ready", checks: readyChecks(false)}},
      reportKey: "dr/reports/ramen-ops/recoveryaction/2026/09/20260920T101200Z-relocate-ledger-1-8f2a1c0d.json", conditions: [cond("Completed", true, "Completed", "", 60 * 24 * 9)]}}));
  store.RecoveryAction.push(Object.assign(api("RecoveryAction"), {metadata: meta("failover-payments-x7", "payments", {annotations: {"dr.simplyblock.io/created-by": "bob@example.com"}, creationTimestamp: agoIso(60 * 24 * 2)}),
    spec: {kind: "Failover", pathRef: {name: "fra-a-to-fra-b"}, applicationRef: {name: "payments"}, override: {reason: "storage-replicating advisory only; site A network partitioned, business decision to fail over"}, timeout: "30m"},
    status: {phase: "Failed", startTime: agoIso(60 * 24 * 2), completionTime: agoIso(60 * 24 * 2 - 14), sourceCluster: "cluster-a", targetCluster: "cluster-b",
      steps: [step("pre-flight", "Succeeded", 60 * 24 * 2, 3, "Degraded, overridden"), step("ramen failover", "Succeeded", 60 * 24 * 2 - 1, 480, "DRPC FailedOver"), step("target starting", "Failed", 60 * 24 * 2 - 9, 300, "tier web: deployment payments-web not ready after 5m (ImagePullBackOff: registry mirror not bound on fra-b)")],
      report: {operator: "bob@example.com", overrideReason: "storage-replicating advisory only; site A network partitioned, business decision to fail over", rtoSeconds: null, achievedRPOSeconds: 240, probes: [], hooks: [], warnings: ["registry mirror role unbound on target site profile"], preFlight: {verdict: "Degraded", checks: readyChecks(true)}},
      reportKey: "dr/reports/payments/recoveryaction/2026/09/20260927T091500Z-failover-payments-x7-1a2b3c4d.json", conditions: [cond("Completed", false, "Failed", "target starting failed", 60 * 24 * 2 - 14)]}}));
  store.RecoveryAction.push(Object.assign(api("RecoveryAction"), {metadata: meta("relocate-shop-live", OPS, {annotations: {"dr.simplyblock.io/created-by": "alice@example.com"}, creationTimestamp: agoIso(6)}),
    spec: {kind: "Relocate", pathRef: {name: "fra-a-to-fra-b"}, applicationRef: {name: "shop"}, timeout: "30m"},
    status: {phase: "TargetStarting", startTime: agoIso(6), sourceCluster: "cluster-a", targetCluster: "cluster-b",
      steps: [step("pre-flight", "Succeeded", 6, 3, "Degraded"), step("pre-source hooks", "Succeeded", 6, 20, ""), step("ramen relocate", "Succeeded", 5, 200, "DRPC Relocated"), step("target starting", "Running", 2, 0, "tier db ready; waiting for tier web")], conditions: []}}));
  store.TestBubble.push(Object.assign(api("TestBubble"), {metadata: meta("test-shop-w4", OPS, {labels: {"dr.simplyblock.io/schedule": "shop-weekly"}, annotations: {"dr.simplyblock.io/created-by": "system:serviceaccount:simplyblock-dr:dr-hub"}, creationTimestamp: agoIso(60 * 30 + 25)}),
    spec: {pathRef: {name: "fra-a-to-fra-b"}, applicationRef: {name: "shop"}, cloneSource: "latest-replicated-snapshot", maxLifetime: "24h"},
    status: {phase: "Completed", testID: "w4-7f21", sourceCluster: "cluster-a", targetCluster: "cluster-b", startTime: agoIso(60 * 30 + 25), clonesReadyTime: agoIso(60 * 30 + 21), completionTime: agoIso(60 * 30),
      applications: [{name: "shop", priority: 1, phase: "Restored", readyTime: agoIso(60 * 30 + 12)}], bubbleNamespaces: ["dr-test-w4-7f21-shop"],
      steps: [step("clone volumes", "Succeeded", 60 * 30 + 25, 240, "3 PVCs cloned from replicated snapshot"), step("restore objects", "Succeeded", 60 * 30 + 21, 300, "Velero restore from capture 19"), step("validate", "Succeeded", 60 * 30 + 16, 200, "tiers ready, probes passed"), step("tear down", "Succeeded", 60 * 30 + 3, 180, "")],
      invariants: [{object: "ramendr.openshift.io/DRPlacementControl ramen-ops/shop", field: "status.phase", before: "Deployed", after: "Deployed"}, {object: "v1/Deployment shop/shop-web", field: "status.readyReplicas", before: "3", after: "3"}],
      checks: [{name: "tiers-ready", status: "Pass", message: ""}, {name: "probes", status: "Pass", message: "storefront 200"}, {name: "isolation", status: "Pass", message: "bubble NAD only"}],
      report: {operator: "system:serviceaccount:simplyblock-dr:dr-hub", outcome: "Passed", testPoint: agoIso(60 * 30 + 30), achievedRPOSeconds: 300, estimatedRTOSeconds: 720, consistency: "crash-consistent", coverage: {exercised: ["volumes", "kube-objects", "tiers", "probes"], notExercised: ["external hooks", "announcement hand-over"]}, warnings: []},
      reportKey: "dr/reports/ramen-ops/testbubble/2026/09/20260928T031500Z-test-shop-w4-77aa11bb.json", conditions: [cond("Outcome", true, "Passed", "", 60 * 30), cond("Completed", true, "Completed", "", 60 * 30)]}}));
  store.TestBubble.push(Object.assign(api("TestBubble"), {metadata: meta("test-payments-hold", "payments", {annotations: {"dr.simplyblock.io/created-by": "carol@example.com"}, creationTimestamp: agoIso(40)}),
    spec: {pathRef: {name: "fra-a-to-fra-b"}, applicationRef: {name: "payments"}, cloneSource: "latest-replicated-snapshot", holdFor: "2h", maxLifetime: "24h"},
    status: {phase: "Holding", testID: "h1-0c9d", sourceCluster: "cluster-a", targetCluster: "cluster-b", startTime: agoIso(40), clonesReadyTime: agoIso(36),
      applications: [{name: "payments", priority: 1, phase: "Restored", readyTime: agoIso(28)}], bubbleNamespaces: ["dr-test-h1-0c9d-payments"],
      steps: [step("clone volumes", "Succeeded", 40, 200, ""), step("restore objects", "Succeeded", 36, 280, ""), step("validate", "Succeeded", 31, 150, ""), step("hold", "Running", 28, 0, "held for manual verification until 2h")],
      invariants: [{object: "ramendr.openshift.io/DRPlacementControl payments/payments", field: "status.phase", before: "Deployed", after: "Deployed"}], checks: [{name: "tiers-ready", status: "Pass", message: ""}], conditions: []}}));
  store.TestSchedule.push(Object.assign(api("TestSchedule"), {metadata: meta("shop-weekly", OPS),
    spec: {schedule: "0 3 * * 0", template: {pathRef: {name: "fra-a-to-fra-b"}, applicationRef: {name: "shop"}, cloneSource: "latest-replicated-snapshot"}, retention: {keepLast: 8, keepFor: "1440h"}},
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
  store.SiteProfile.push(sprof("cluster-a", ["eu-central-1a"], {spec: {logicalNetworks: [{role: "backend", nad: "apps/backend"}], registryMirror: "mirror.cluster-a.example.com"}}));
  store.SiteProfile.push(sprof("cluster-b", ["eu-central-1b"], {notReady: true}));
  store.SiteProfile.push(sprof("stretch", ["eu-central-1a", "eu-central-1c"]));
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
      a.status.steps = ACTION_SEQ.slice(1, i + 1).map((p, j) => ({name: p, result: j < i - 1 || i === ACTION_SEQ.length - 1 ? "Succeeded" : "Running", startTime: iso(Date.parse(a.metadata.creationTimestamp) + (j + 1) * 8000), message: ""}));
      if (i === ACTION_SEQ.length - 1 && !a.status.completionTime) { a.status.completionTime = iso(t); a.status.report = {operator: a.metadata.annotations["dr.simplyblock.io/created-by"], rtoSeconds: Math.round(age), achievedRPOSeconds: a.spec.kind === "Failover" ? 240 : 0, warnings: [], probes: [], hooks: []}; a.status.conditions = [cond("Completed", true, "Completed", "", 0)]; }
    });
    store.TestBubble.filter(b => b.__sim).forEach(b => {
      const age = (t - Date.parse(b.metadata.creationTimestamp)) / 1000, i = b.spec.abort ? TEST_SEQ.length - 1 : Math.min(TEST_SEQ.length - 1, Math.floor(age / 8));
      b.status.phase = TEST_SEQ[i];
      b.status.steps = TEST_SEQ.slice(1, i + 1).map((p, j) => ({name: p, result: j < i - 1 || i === TEST_SEQ.length - 1 ? "Succeeded" : "Running", startTime: iso(Date.parse(b.metadata.creationTimestamp) + (j + 1) * 8000), message: ""}));
      if (i === TEST_SEQ.length - 1 && !b.status.completionTime) { b.status.completionTime = iso(t); b.status.report = {operator: b.metadata.annotations["dr.simplyblock.io/created-by"], outcome: b.spec.abort ? "Failed" : "Passed", testPoint: b.metadata.creationTimestamp, achievedRPOSeconds: 300, estimatedRTOSeconds: Math.round(age), consistency: "crash-consistent", coverage: {exercised: ["volumes", "kube-objects"], notExercised: []}, warnings: b.spec.abort ? ["aborted by operator"] : []}; }
    });
  };

  const KINDS = Object.keys(store);
  const strip = o => { const c = JSON.parse(JSON.stringify(o)); delete c.__sim; return c; };
  window.DR_MOCK = {
    has: kind => KINDS.includes(kind),
    list: kind => { advance(); return store[kind].map(strip); }
  };
  const viewer = () => (localStorage.getItem("sb.viewas") || "").includes("reader") ? "viewer" : "admin";
  const findRef = (kind, ns, name) => store[kind].find(o => o.metadata.name === name && (!ns || o.metadata.namespace === ns));
  const create = kind => body => {
    const m = body.metadata || {};
    if (!m.name) return {err: "metadata.name is required", reason: "Invalid"};
    if (findRef(kind, m.namespace, m.name)) return {err: `${kind.toLowerCase()}s "${m.name}" already exists`, reason: "AlreadyExists"};
    const obj = Object.assign({}, body, {metadata: Object.assign({}, m, {uid: uid(), creationTimestamp: iso(Date.now()), generation: 1,
      annotations: Object.assign({}, m.annotations || {}, ["RecoveryAction", "TestBubble"].includes(kind) ? {"dr.simplyblock.io/created-by": "you@example.com"} : {})}), status: {}});
    if (kind === "RecoveryAction") {
      if (body.spec.override && viewer() !== "admin") return {err: 'admission webhook "vrecoveryaction.dr.simplyblock.io" denied the request: a readiness override needs the "override" verb on recoveryactions, which only dr-admin has', reason: "Forbidden"};
      const app = body.spec.applicationRef && findRef("ProtectedApplication", m.namespace, body.spec.applicationRef.name);
      if (app && body.spec.kind !== "Restart") {
        const p = (app.status.paths || []).find(x => x.name === (body.spec.pathRef || {}).name);
        if (!p) return {err: `application ${app.metadata.name} is not on path ${(body.spec.pathRef || {}).name}`, reason: "Invalid"};
        if (p.readiness.verdict === "NotReady" && !body.spec.override) return {err: `admission webhook denied the request: readiness on ${p.name} is NotReady (${p.readiness.checks.filter(c => c.blocking && c.status === "Fail").map(c => c.name).join(", ")}); an override with a reason is required`, reason: "Forbidden"};
      }
      obj.__sim = true; obj.status = {phase: "Pending", startTime: obj.metadata.creationTimestamp, sourceCluster: "cluster-a", targetCluster: body.spec.kind === "Restart" ? "cluster-a" : "cluster-b", steps: [], conditions: []};
    }
    if (kind === "TestBubble") { obj.__sim = true; obj.status = {phase: "Pending", testID: U().hex(6), startTime: obj.metadata.creationTimestamp, sourceCluster: "cluster-a", targetCluster: "cluster-b", applications: [{name: (body.spec.applicationRef || body.spec.planRef).name, priority: 1, phase: "Pending"}], bubbleNamespaces: [], steps: [], invariants: [], checks: [], conditions: []}; }
    if (kind === "RestoreAction") obj.status = {phase: "Pending", startTime: obj.metadata.creationTimestamp, steps: [], checks: [], volumes: []};
    if (kind === "TestSchedule") obj.status = {conditions: [cond("Valid", true, "Valid", "", 0)]};
    if (kind === "ProtectionPlan") obj.status = {sites: (body.spec.sites || []).map(s => ({name: s.name, classesApplied: false})), pairs: [], conditions: [cond("Derived", false, "Deriving", "deriving Ramen objects", 0), cond("Ready", false, "Deriving", "", 0)]};
    if (kind === "DRPath") obj.status = {applications: [], drPolicies: [], profileConsistency: "Unknown", profileComparison: [], conditions: [cond("Valid", true, "Valid", "", 0)]};
    if (kind === "ProtectedApplication") obj.status = {paths: [], conditions: [cond("Bound", false, "Binding", "waiting for the DRPC", 0), cond("Protected", false, "Binding", "", 0)]};
    if (kind === "RecoveryPlan") obj.status = {readiness: {verdict: "Unknown", checks: []}, conditions: [cond("Valid", true, "Valid", "", 0)]};
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
    }
    if (body.metadata && body.metadata.annotations) { o.metadata.annotations = o.metadata.annotations || {}; Object.entries(body.metadata.annotations).forEach(([k, v]) => { if (v === null) delete o.metadata.annotations[k]; else o.metadata.annotations[k] = v; }); }
    o.metadata.generation = (o.metadata.generation || 1) + 1;
    return {obj: strip(o)};
  };
  const remove = kind => (name, ns) => {
    const o = findRef(kind, ns, name);
    if (!o) return {err: `${kind.toLowerCase()}s "${name}" not found`, reason: "NotFound"};
    const inUse = kind === "ProtectionPlan" ? store.DRPath.some(p => p.spec.planRef.name === name) : kind === "DRPath" ? store.ProtectedApplication.some(a => (a.status.paths || []).some(p => p.name === name)) : false;
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

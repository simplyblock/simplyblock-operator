// ---------------------------------------------------------------------------
// CR HEADLINES: what matters first on each kind, and how kinds relate
//
// A custom resource has dozens of fields. Which spec fields the first tier
// of a detail shows comes from the CRD, in an order of its own (cr-view.jsx);
// the spec tables here only decorate the fields they name: a label, how to
// format it, a link to a related object, and whether the console offers an
// edit. They decide the spec rows only when the CRD cannot be read. The
// status tables still choose the status rows, which the operator writes and
// the schema cannot rank. A path is spelled exactly as the CRD spells it. Every path
// is one of storage.simplyblock.io/v1alpha2 on main (or the core kind's own
// schema); descriptions and enums come from the CRD at run time (cr-view.jsx)
// and are never restated here. Immutability is read from the CRD too;
// STATIC_IMMUTABLE is the fallback for a CRD the console cannot read.
//
// RELATIONS lists the drill-down cards a kind offers beyond what its curated
// detail already shows.
// ---------------------------------------------------------------------------
const F = {
  bytes: v => (v === null || v === undefined ? null : typeof v === "number" ? fmtBytes(v) : v),
  bool: v => (v === null || v === undefined ? null : v ? "true" : "false"),
  date: v => (v ? fmtDate(v) : null),
  ago: v => (v ? fmtAgo(v) : null),
  list: v => (Array.isArray(v) ? (v.length ? v.join(", ") : "[]") : v),
  map: v => (v && typeof v === "object" ? Object.entries(v).map(([k, x]) => `${k}=${x}`).join(", ") || "{}" : v),
  num: v => (v === null || v === undefined ? null : fmtNum(v)),
  raw: v => (v === null || v === undefined ? null : typeof v === "object" ? JSON.stringify(v) : String(v))
};
// the label is the field's own name, so the UI says what the backend says
const row = (path, opts) => Object.assign({path, label: path.replace(/^(spec|status)\./, ""), fmt: F.raw, mutable: true, type: "text"}, opts || {});
const ro = (path, opts) => row(path, Object.assign({mutable: false}, opts || {}));

const HEADLINES = {
  StorageCluster: {
    spec: [
      ro("spec.clusterMode"), ro("spec.deviceClass"), ro("spec.fabricType"),
      ro("spec.stripe.dataChunks", {type: "number"}), ro("spec.stripe.parityChunks", {type: "number"}),
      row("spec.vcpuCount", {type: "number"}), row("spec.maxSubsystemCount", {type: "number"}), row("spec.minHugePagesSize"),
      ro("spec.nvmfBasePort", {type: "number"}), ro("spec.rpcBasePort", {type: "number"}), ro("spec.snodeApiPort", {type: "number"}),
      ro("spec.enableFailureDomains", {type: "bool", fmt: F.bool}), ro("spec.enableSyncReplication", {type: "bool", fmt: F.bool}),
      ro("spec.enableNodeAffinity", {type: "bool", fmt: F.bool}), ro("spec.enableMetadataJournal", {type: "bool", fmt: F.bool}),
      ro("spec.enableChecksumValidation", {type: "bool", fmt: F.bool}), ro("spec.enableAtomicity4K", {type: "bool", fmt: F.bool}),
      row("spec.enableArbitration", {type: "bool", fmt: F.bool}), row("spec.enableVolumeAutoPlacement", {type: "bool", fmt: F.bool}),
      row("spec.disablePlacement", {type: "bool", fmt: F.bool}), row("spec.disableDataRealignment", {type: "bool", fmt: F.bool}),
      row("spec.maxConcurrentWorkerRestarts", {type: "number"}), row("spec.clientDataIfname"),
      row("spec.warningThreshold.capacity", {type: "number"}), row("spec.warningThreshold.provisionedCapacity", {type: "number"}),
      row("spec.criticalThreshold.capacity", {type: "number"}), row("spec.criticalThreshold.provisionedCapacity", {type: "number"}),
      ro("spec.storageNodes.mgmtInterface"), ro("spec.storageNodes.dataInterfaces", {type: "list", fmt: F.list}),
      ro("spec.storageNodes.nodesPerSocket", {type: "number"}), row("spec.storageNodes.nodeProvisioningBudget", {type: "number"}),
      row("spec.storageNodes.image"), row("spec.storageNodes.imagePullPolicy", {type: "select"}),
      ro("spec.storageNodes.enableJournalDevice", {type: "bool", fmt: F.bool}), ro("spec.storageNodes.enableFormat4K", {type: "bool", fmt: F.bool}),
      row("spec.storageNodes.enableCpuTopology", {type: "bool", fmt: F.bool}), row("spec.storageNodes.reservedSystemCPU"),
      row("spec.storageNodes.socketsToUse", {type: "list", fmt: F.list}), ro("spec.storageNodes.network.spdkOnHostNetwork", {type: "bool", fmt: F.bool}),
      ro("spec.storageNodes.network.podNetwork", {type: "bool", fmt: F.bool}),
      row("spec.backup.endpoint"), row("spec.backup.bucket"), row("spec.backup.region"), row("spec.backup.credentialsSecretRef.name"),
      row("spec.volumeAutoPlacement.metricsBackend", {type: "select"}), row("spec.volumeAutoPlacement.evaluationInterval"),
      row("spec.volumeAutoPlacement.imbalanceThreshold", {type: "number"}), row("spec.volumeAutoPlacement.maxVolumeMigrationsPerCycle", {type: "number"}),
      row("spec.volumeMigrationSettings.dataRealignment.interval"), row("spec.volumeMigrationSettings.dataRealignment.minMoves", {type: "number"}),
      ro("spec.stretched.site"), ro("spec.stretched.peerSite"), row("spec.stretched.role", {type: "select"}), row("spec.stretched.peerCluster"),
      ro("spec.superclusterRef"), row("spec.supercluster.placement.strategy", {type: "select"}),
      ro("spec.kms.vault.endpoint"), ro("spec.kms.kmip.endpoint")
    ],
    status: [
      row("status.uuid"), row("status.clusterName"), row("status.phase"), row("status.status"), row("status.message"), row("status.step.state"),
      row("status.configured", {fmt: F.bool}), row("status.nqn"), row("status.erasureCodingScheme"), row("status.maxFaultTolerance"),
      row("status.rebalancing", {fmt: F.bool}), row("status.activeOpsRef"), row("status.maxConcurrentWorkerRestarts"),
      row("status.volumeMoveGeneration"), row("status.realignedGeneration"), row("status.lastDataRealignmentAt", {fmt: F.date}),
      row("status.role"), row("status.memberCount"), row("status.observedGeneration"),
      row("status.stretched.role"), row("status.stretched.siteLink"), row("status.stretched.lostSite"), row("status.stretched.peerNodes"), row("status.stretched.peerNodesOnline"), row("status.stretched.siteRecord"),
      row("status.arbitration.state"), row("status.arbitration.epoch"), row("status.arbitration.fencedNodes", {fmt: F.list}), row("status.arbitration.observedAt", {fmt: F.ago}),
      row("status.rebalancingMetrics.imbalancePercent"), row("status.rebalancingMetrics.avgDeviationPct"), row("status.rebalancingMetrics.maxDeviationPct"),
      row("status.rebalancingMetrics.hottestNodeUUID"), row("status.rebalancingMetrics.coolestNodeUUID"), row("status.rebalancingMetrics.lastEvaluatedAt", {fmt: F.ago}), row("status.rebalancingMetrics.lastMigrationAt", {fmt: F.ago}),
      row("status.workloadNames.nodeAPIService"), row("status.workloadNames.spdkProxyService"), row("status.workloadNames.nodeAPITLSSecret"), row("status.workloadNames.spdkProxyTLSSecret"), row("status.workloadNames.nodeLabelSuffix")
    ],
    tables: [
      {path: "status.tasks", title: "status.tasks: the control plane's running and pending jobs", columns: ["id", "type", "status", "retry"]},
      {path: "status.provisioningSlots", title: "status.provisioningSlots: workers whose node add is outstanding", columns: ["worker", "node", "takenAt"]},
      {path: "status.failureDomains", title: "status.failureDomains", columns: ["name", "index"]},
      {path: "status.serviceClasses", title: "status.serviceClasses", columns: ["id", "name", "weight"]},
      {path: "status.members", title: "status.members: the supercluster's members", columns: ["name", "uuid", "phase", "placementEligible"]},
      {path: "status.arbitration.hostFences", title: "status.arbitration.hostFences", columns: ["node", "state", "epoch", "winner"]},
      {path: "status.rebalancingMetrics.nodeMetrics", title: "status.rebalancingMetrics.nodeMetrics", columns: ["nodeUUID", "latencyDeviationPct", "volumeCount", "lastUpdated"]}
    ]
  },
  StorageNode: {
    spec: [
      ro("spec.clusterRef", {link: "cluster"}), row("spec.workerNode", {link: "worker"}), ro("spec.slot", {type: "number"}), row("spec.socketId"), row("spec.nodeIndex", {type: "number"}), ro("spec.nodeSet"),
      row("spec.config.sizing.vcpuCount", {type: "number"}), row("spec.config.sizing.minHugePagesSize"),
      ro("spec.config.mgmtInterface"), ro("spec.config.dataInterfaces", {type: "list", fmt: F.list}),
      row("spec.config.pcieAllowList", {type: "list", fmt: F.list}), ro("spec.config.pcieDenyList", {type: "list", fmt: F.list}), ro("spec.config.pcieModel"), ro("spec.config.driveSizeRange"), ro("spec.config.deviceNames", {type: "list", fmt: F.list}),
      ro("spec.config.failureDomain"), ro("spec.config.site"), ro("spec.config.expand", {type: "bool", fmt: F.bool}),
      ro("spec.config.journalManager.count", {type: "number"}), ro("spec.config.journalManager.percentPerDevice", {type: "number"}),
      row("spec.config.reservedSystemCPU"), row("spec.config.spdkImage"), row("spec.config.spdkImagePullPolicy", {type: "select"}),
      row("spec.config.spdkProxyImage"), row("spec.config.spdkProxyImagePullPolicy", {type: "select"}), ro("spec.stretched.remote", {type: "bool", fmt: F.bool})
    ],
    status: [
      row("status.uuid"), row("status.hostname"), row("status.phase"), row("status.status"), row("status.health", {fmt: F.bool}), row("status.message"), row("status.step.state"),
      row("status.activeOpsRef"), row("status.failureDomain"), row("status.uptime"), row("status.nodeAddTaskID"), row("status.observedGeneration"),
      row("status.ports.management"), row("status.ports.nvmeof"), row("status.ports.rpc"), row("status.ports.lvol"),
      row("status.resources.cpu"), row("status.resources.memory"), row("status.resources.volumes"), row("status.resources.devices.online"), row("status.resources.devices.total"),
      row("status.resources.capacity.totalBytes", {fmt: F.bytes}), row("status.resources.capacity.usedBytes", {fmt: F.bytes}), row("status.resources.capacity.sampledAt", {fmt: F.ago}),
      row("status.latencyMetrics.baselineP50NS", {fmt: F.num}), row("status.latencyMetrics.baselineP99NS", {fmt: F.num}), row("status.latencyMetrics.baselineMeasuredAt", {fmt: F.ago}),
      row("status.remediation.nodeNotReady", {fmt: F.bool}), row("status.remediation.outOfService", {fmt: F.bool}), row("status.remediation.reportedFenced", {fmt: F.bool}), row("status.remediation.storageFencedEpoch"), row("status.remediation.observedAt", {fmt: F.ago})
    ],
    tables: [{path: "status.network", title: "status.network: what the SPDK pod reported per storage network", columns: ["network", "phase", "address", "assigned", "interface", "resource", "hostReachable"]}]
  },
  StorageDevice: {
    spec: [ro("spec.nodeRef", {link: "node"}), ro("spec.deviceID")],
    status: [
      row("status.phase"), row("status.deviceStatus"), row("status.role"), row("status.message"), row("status.activeOpsRef"),
      row("status.capacity.totalBytes", {fmt: F.bytes}), row("status.clusterID"), row("status.nodeID"), row("status.site"), row("status.remote", {fmt: F.bool}), row("status.observedGeneration"),
      row("status.hardware.model"), row("status.hardware.serialNumber"), row("status.hardware.pciAddress"), row("status.hardware.nvmeController"),
      row("status.health.smart.verdict"), row("status.health.smart.reason"), row("status.health.smart.checkedAt", {fmt: F.ago}), row("status.health.smart.source"),
      row("status.health.smart.temperatureCelsius"), row("status.health.smart.percentageUsed"), row("status.health.smart.availableSparePercent"), row("status.health.smart.availableSpareThresholdPercent"),
      row("status.health.smart.mediaErrors"), row("status.health.smart.criticalWarning"), row("status.health.smart.powerOnHours", {fmt: F.num}), row("status.health.smart.unsafeShutdowns"),
      row("status.health.smart.reallocatedSectors"), row("status.health.smart.pendingSectors"), row("status.health.smart.smartPassed", {fmt: F.bool}),
      row("status.health.performance.verdict"), row("status.health.performance.reason"), row("status.health.performance.since", {fmt: F.ago}), row("status.health.performance.assessedAt", {fmt: F.ago}),
      row("status.health.performance.latencyMicroseconds"), row("status.health.performance.fleetLatencyMicroseconds"), row("status.health.performance.latencyRatioPercent"), row("status.health.performance.iopsRatioPercent")
    ]
  },
  StoragePool: {
    spec: [
      ro("spec.clusterRef", {link: "cluster"}), row("spec.limits.capacity"), row("spec.limits.maxVolumeSize"), row("spec.limits.iops", {type: "number"}),
      row("spec.limits.throughput.readWrite", {type: "number"}), row("spec.limits.throughput.read", {type: "number"}), row("spec.limits.throughput.write", {type: "number"}),
      row("spec.allowedNodes", {type: "list", fmt: F.list}),
      ro("spec.volumeDefaults.filesystem"), ro("spec.volumeDefaults.fabric"), ro("spec.volumeDefaults.iops", {type: "number"}),
      ro("spec.volumeDefaults.throughput.readWrite", {type: "number"}), ro("spec.volumeDefaults.throughput.read", {type: "number"}), ro("spec.volumeDefaults.throughput.write", {type: "number"}),
      ro("spec.volumeDefaults.enableCompression", {type: "bool", fmt: F.bool}), ro("spec.volumeDefaults.enableClientCompression", {type: "bool", fmt: F.bool}), ro("spec.volumeDefaults.enableClientDeduplication", {type: "bool", fmt: F.bool}),
      ro("spec.volumeDefaults.enableReplication", {type: "bool", fmt: F.bool}), ro("spec.volumeDefaults.enableDHCHAP", {type: "bool", fmt: F.bool}),
      ro("spec.volumeDefaults.maxNamespacesPerSubsystem", {type: "number"}), ro("spec.volumeDefaults.priorityClass"), ro("spec.volumeDefaults.stripeWidth", {type: "number"}), ro("spec.volumeDefaults.stripeSize"), ro("spec.volumeDefaults.tune2fsReservedBlocks")
    ],
    status: [
      row("status.uuid"), row("status.phase"), row("status.status"), row("status.message"), row("status.activeOpsRef"), row("status.observedGeneration"),
      row("status.storageClassNames", {fmt: F.list}), row("status.defaultStorageClassName", {link: "storageclass"}), row("status.allowedNodes", {fmt: F.list}),
      row("status.limits.capacity"), row("status.limits.iops"), row("status.limits.host"), row("status.limits.throughput.readWrite"), row("status.limits.throughput.read"), row("status.limits.throughput.write")
    ],
    tables: [{path: "status.members", title: "status.members: the pool in each supercluster member", columns: ["cluster", "uuid", "phase", "message"]}]
  },
  ControlPlane: {
    spec: [
      row("spec.source.local.image"), row("spec.source.local.imagePullPolicy", {type: "select"}), row("spec.source.local.replicas", {type: "number"}),
      row("spec.source.local.foundationDB.replicas", {type: "number"}), row("spec.source.local.foundationDB.storageClassName"),
      ro("spec.source.local.tls.enableTLS", {type: "bool", fmt: F.bool}), ro("spec.source.local.tls.enableMutualTLS", {type: "bool", fmt: F.bool}), ro("spec.source.local.tls.provider"),
      row("spec.source.local.observability.enableMonitoring", {type: "bool", fmt: F.bool}), row("spec.source.local.adminTokenSecretRef.name"), row("spec.source.local.eksRoleARN"),
      row("spec.source.managed.endpoint"), row("spec.source.managed.credentialsSecretRef.name"), row("spec.source.managed.caBundleSecretRef.name"), row("spec.source.managed.storageNodeImage")
    ],
    status: [row("status.phase"), row("status.message"), row("status.endpoint"), row("status.version"), row("status.lastChecked", {fmt: F.ago}), row("status.step.state"), row("status.activeOpsRef"), row("status.storageClassName"), row("status.observedGeneration")],
    tables: [{path: "status.components", title: "status.components: the per-component readiness the phase is derived from", columns: ["name", "ready", "desired", "essential"]}]
  },
  SimplyblockDriver: {
    spec: [ro("spec.driverName"), row("spec.image"), row("spec.imagePullPolicy", {type: "select"}), row("spec.controllerReplicas", {type: "number"}), row("spec.enableVolumeSnapshots", {type: "bool", fmt: F.bool}),
      row("spec.enableCSIAddons", {type: "bool", fmt: F.bool}), row("spec.enableNVMeTCPModuleInstall", {type: "bool", fmt: F.bool}), row("spec.enableServiceAccountAuth", {type: "bool", fmt: F.bool}), row("spec.disableDeviceMapperIndirection", {type: "bool", fmt: F.bool}),
      row("spec.tls.enableTLS", {type: "bool", fmt: F.bool}), row("spec.tls.enableMutualTLS", {type: "bool", fmt: F.bool}), row("spec.tls.provider", {type: "select"})],
    status: [row("status.phase"), row("status.message"), row("status.version"), row("status.controllerReady", {fmt: F.bool}), row("status.nodesReady"), row("status.nodesTotal"), row("status.snapshotSupport"), row("status.origin"), row("status.observedGeneration")]
  },
  ClusterDeploymentConfig: {
    spec: [row("spec.approved", {type: "bool", fmt: F.bool}), ro("spec.clusterRef"), ro("spec.environment"), row("spec.edgeCluster", {type: "bool", fmt: F.bool}),
      row("spec.cluster.name"), row("spec.cluster.vcpuCount", {type: "number"}), row("spec.cluster.maxSubsystemCount", {type: "number"}), row("spec.cluster.minHugePagesSize"), row("spec.cluster.fabricType"), row("spec.cluster.clusterMode", {type: "select"}),
      row("spec.cluster.stripe.dataChunks", {type: "number"}), row("spec.cluster.stripe.parityChunks", {type: "number"}), row("spec.cluster.nodesPerSocket", {type: "number"}), row("spec.cluster.nodeProvisioningBudget", {type: "number"}),
      row("spec.cluster.enableFailureDomains", {type: "bool", fmt: F.bool}), row("spec.cluster.enableJournalDevice", {type: "bool", fmt: F.bool}), row("spec.cluster.enableDriveFormat", {type: "bool", fmt: F.bool}),
      row("spec.hostOS.family"), row("spec.hostOS.distro"), row("spec.images.spdk.image"), row("spec.images.spdkProxy.image"), row("spec.images.nodeAgent.image")],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.clusterRef", {link: "cluster"}), row("status.nodeRefs", {fmt: F.list}), row("status.expansionStartedAt", {fmt: F.date}), row("status.observedGeneration")],
    tables: [{path: "status.members", title: "status.members", columns: ["nodeSet", "cluster"]}]
  },
  StorageBackup: {
    spec: [ro("spec.source.claimName", {link: "pvc"}), ro("spec.clusterRef", {link: "cluster"})],
    status: [row("status.phase"), row("status.apiStatus"), row("status.message"), row("status.activeOpsRef"), row("status.clusterID"),
      row("status.backup.backupID"), row("status.backup.previousBackupID"), row("status.backup.size", {fmt: F.bytes}), row("status.backup.s3ID"), row("status.backup.startedAt", {fmt: F.date}), row("status.backup.completedAt", {fmt: F.date}),
      row("status.source.claimName"), row("status.source.claimNamespace"), row("status.source.persistentVolumeName", {link: "pv"}), row("status.source.lvolID"), row("status.source.lvolName"), row("status.source.poolName"), row("status.source.poolUUID"),
      row("status.source.clusterUUID"), row("status.source.nodeID"), row("status.source.fsType"), row("status.source.volumeMode"), row("status.source.snapshotID"), row("status.source.snapshotName"),
      row("status.source.group.consistencyGroupName"), row("status.source.group.generation"), row("status.source.group.groupBackupID"), row("status.source.group.memberCount"), row("status.observedGeneration")]
  },
  StorageBackupPolicy: {
    spec: [ro("spec.clusterRef", {link: "cluster"}), ro("spec.schedule"), row("spec.consistencyGroupName"), row("spec.claimSelector.matchLabels", {fmt: F.map})],
    status: [row("status.phase"), row("status.message"), row("status.policyID"), row("status.clusterID"), row("status.retention.maxVersions"), row("status.retention.maxAge"), row("status.lastBackupAt", {fmt: F.ago}), row("status.observedGeneration")],
    tables: [{path: "status.attachedClaims", title: "status.attachedClaims: the claims the policy covers", columns: ["name", "persistentVolumeName", "lvolID", "clusterID", "attachedAt"]},
      {path: "status.members", title: "status.members", columns: ["cluster", "clusterID", "policyID"]}]
  },
  StorageClusterOps: {
    spec: [ro("spec.clusterRef", {link: "cluster"}), ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool}), ro("spec.rollingRestart.refreshSNodeAPI", {fmt: F.bool}), ro("spec.cancelTask.taskID")],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}),
      row("status.rollingRestart.nodeIndex"), row("status.rollingRestart.nodes", {fmt: F.list}), row("status.rollingRestart.skipped", {fmt: F.list}), row("status.observedGeneration")]
  },
  StorageNodeOps: {
    spec: [ro("spec.nodeRef", {link: "node"}), ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool}), ro("spec.force", {fmt: F.bool}), ro("spec.reattachVolume", {fmt: F.bool}),
      ro("spec.migrate.targetWorkerNode", {link: "worker"}), ro("spec.migrate.newSsdPcie", {fmt: F.list}), ro("spec.remove.systemVolumeFilterRegex")],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}),
      row("status.drain.volumesMigrated"), row("status.drain.volumesTotal"),
      row("status.removal.nodeStatus"), row("status.removal.devices", {fmt: F.map}), row("status.removal.prepareAttempts"), row("status.removal.lastPrepareTime", {fmt: F.ago}), row("status.removal.lastProgressTime", {fmt: F.ago}), row("status.observedGeneration")]
  },
  StorageDeviceOps: {
    spec: [ro("spec.deviceRef"), ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool})],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.deviceStatusBefore"), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}), row("status.observedGeneration")]
  },
  StoragePoolOps: {
    spec: [ro("spec.poolRef", {link: "pool"}), ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool})],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}), row("status.observedGeneration")]
  },
  PersistentVolumeOps: {
    spec: [ro("spec.persistentVolumeName", {link: "pv"}), ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool}),
      ro("spec.migrate.targetNodeRef.namespace"), ro("spec.migrate.targetNodeRef.name", {link: "node"}), ro("spec.relocate.targetClusterRef", {link: "cluster"}),
      ro("spec.joinConsistencyGroup.groupName"), ro("spec.joinConsistencyGroup.suspendTimeoutSeconds"),
      ro("spec.creatorRef.kind"), ro("spec.creatorRef.namespace"), ro("spec.creatorRef.name"), ro("spec.creatorRef.uid")],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.deferredSince", {fmt: F.ago}), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}),
      row("status.migration.migrationUUID"), row("status.migration.clusterUUID"), row("status.migration.poolUUID"), row("status.migration.volumeUUID"), row("status.migration.subsystemNQN"),
      row("status.migration.sourceNodeUUID"), row("status.migration.targetNodeUUID"), row("status.migration.memberCount"), row("status.migration.continuedAt", {fmt: F.ago}),
      row("status.relocate.sourceClusterUUID"), row("status.relocate.targetClusterUUID"), row("status.relocate.targetPoolUUID"), row("status.relocate.committed", {fmt: F.bool}), row("status.relocate.nodesMissingPaths", {fmt: F.list}),
      row("status.joinConsistencyGroup.groupUUID"), row("status.joinConsistencyGroup.joined", {fmt: F.bool}), row("status.joinConsistencyGroup.targetNQN"), row("status.joinConsistencyGroup.namespacesAt"), row("status.joinConsistencyGroup.rollbackReason"), row("status.observedGeneration")],
    tables: [{path: "status.migration.connections", title: "status.migration.connections: the paths published on the target", columns: ["nqn", "address", "port", "transport", "nrIOQueues", "ctrlLossTimeoutSeconds"]},
      {path: "status.migration.validationJobs", title: "status.migration.validationJobs", columns: ["namespace", "name", "node", "succeeded"]},
      {path: "status.relocate.volumes", title: "status.relocate.volumes", columns: ["volumeUUID", "persistentVolumeName", "taskID", "proceeded", "cutOver", "targetVolumeUUID"]}]
  },
  ControlPlaneOps: {
    spec: [ro("spec.controlPlaneRef"), ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool}), ro("spec.restart.components", {fmt: F.list}), ro("spec.upgrade.image"), ro("spec.backup.backupName"), ro("spec.backup.blobStore"), ro("spec.hostMaintenance.workerNode", {link: "worker"})],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.backupRef"), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}),
      row("status.hostMaintenance.zone"), row("status.hostMaintenance.coordinatorPlacement"), row("status.hostMaintenance.coordinators", {fmt: F.list}), row("status.hostMaintenance.excludedProcesses", {fmt: F.list}), row("status.hostMaintenance.processGroups", {fmt: F.list}),
      row("status.hostMaintenance.drainObservedAt", {fmt: F.ago}), row("status.hostMaintenance.drainObservedBy"), row("status.hostMaintenance.maintenanceUntil", {fmt: F.date}), row("status.observedGeneration")]
  },
  OperatorOps: {
    spec: [ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool}), ro("spec.discover.configName"), ro("spec.discover.clusterRef", {link: "cluster"}), ro("spec.discover.workers", {fmt: F.list}), ro("spec.discover.nodeSelector", {fmt: F.map}),
      ro("spec.discover.enableControlPlaneNodes", {fmt: F.bool}), ro("spec.discover.enableFailureDomains", {fmt: F.bool}), ro("spec.discover.enableLogicalBlockDevices", {fmt: F.bool}), ro("spec.discover.failureDomainLabel"),
      ro("spec.discover.deviceFilter.pcieAllowList", {fmt: F.list}), ro("spec.discover.deviceFilter.pcieDenyList", {fmt: F.list}), ro("spec.discover.deviceFilter.pcieModel"), ro("spec.discover.deviceFilter.driveSizeRange")],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.configRef"), row("status.environment"), row("status.workers", {fmt: F.list}), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}), row("status.observedGeneration")]
  },
  StorageBackupOps: {
    spec: [ro("spec.action"), row("spec.abort", {type: "bool", fmt: F.bool}), ro("spec.clusterRef.name", {link: "cluster"}), ro("spec.backupRef.name"), ro("spec.backupRef.namespace"), ro("spec.groupBackupID"), ro("spec.groupBackupRef.name"),
      ro("spec.restore.claimName"), ro("spec.restore.targetPool", {link: "pool"}), ro("spec.restore.volumeMode")],
    status: [row("status.phase"), row("status.step.state"), row("status.message"), row("status.claimName"), row("status.persistentVolumeName", {link: "pv"}), row("status.restoredLvolID"), row("status.backupID"), row("status.clusterID"), row("status.poolUUID"), row("status.restoredGroupID"), row("status.startedAt", {fmt: F.date}), row("status.completedAt", {fmt: F.date}), row("status.observedGeneration")],
    tables: [{path: "status.restoredGroupMembers", title: "status.restoredGroupMembers", columns: ["memberName", "lvolID", "claimName"]}]
  },
  PersistentVolume: {
    spec: [ro("spec.capacity.storage"), ro("spec.accessModes", {fmt: F.list}), ro("spec.storageClassName", {link: "storageclass"}), row("spec.persistentVolumeReclaimPolicy", {type: "select", options: ["Retain", "Delete"]}), ro("spec.volumeMode"),
      ro("spec.csi.driver"), ro("spec.csi.volumeHandle"), ro("spec.csi.fsType"), ro("spec.claimRef.namespace"), ro("spec.claimRef.name", {link: "pvc"})],
    status: [row("status.phase"), row("status.reason"), row("status.message")]
  },
  PersistentVolumeClaim: {
    spec: [ro("spec.storageClassName", {link: "storageclass"}), row("spec.resources.requests.storage"), ro("spec.accessModes", {fmt: F.list}), ro("spec.volumeMode"), ro("spec.volumeName", {link: "pv"}), ro("spec.dataSource", {fmt: F.raw}), ro("spec.dataSourceRef", {fmt: F.raw})],
    status: [row("status.phase"), row("status.capacity.storage"), row("status.accessModes", {fmt: F.list}), row("status.allocatedResources.storage")],
    annotations: ["simplyblock.io/backup-policy", "storage.simplyblock.io/replication-policy", "simplyblock.io/selected-storage-node", "simplyblock.io/qos-rw-iops", "simplyblock.io/qos-rw-mbps"]
  },
  StorageClass: {
    spec: [ro("provisioner"), ro("parameters.cluster_id"), ro("parameters.pool_name", {link: "pool"}), ro("parameters.fabric"), ro("parameters.csi.storage.k8s.io/fstype"),
      ro("parameters.max_iops"), ro("parameters.max_mbytes_per_sec"), ro("parameters.max_read_mbytes_per_sec"), ro("parameters.max_write_mbytes_per_sec"),
      ro("parameters.compression"), ro("parameters.client_compression"), ro("parameters.client_deduplication"), ro("parameters.replicate"), ro("parameters.priority_class"), ro("parameters.max_namespace_per_subsys"), ro("parameters.stripe_width"), ro("parameters.stripe_size"),
      ro("reclaimPolicy"), ro("volumeBindingMode"), row("allowVolumeExpansion", {type: "bool", fmt: F.bool}), row("mountOptions", {fmt: F.list})],
    status: []
  },
  VolumeSnapshot: {
    spec: [ro("spec.volumeSnapshotClassName"), ro("spec.source.persistentVolumeClaimName", {link: "pvc"}), ro("spec.source.volumeSnapshotContentName")],
    status: [row("status.readyToUse", {fmt: F.bool}), row("status.creationTime", {fmt: F.date}), row("status.restoreSize"), row("status.boundVolumeSnapshotContentName", {link: "vsc"}), row("status.error.message")]
  },
  VolumeSnapshotContent: {
    spec: [ro("spec.driver"), row("spec.deletionPolicy", {type: "select", options: ["Retain", "Delete"]}), ro("spec.volumeSnapshotRef.namespace"), ro("spec.volumeSnapshotRef.name")],
    status: [row("status.readyToUse", {fmt: F.bool}), row("status.snapshotHandle"), row("status.restoreSize", {fmt: F.bytes}), row("status.creationTime")]
  },
  Node: {
    spec: [row("spec.unschedulable", {fmt: F.bool}), row("spec.taints", {fmt: F.raw})],
    status: [row("status.nodeInfo.kubeletVersion"), row("status.nodeInfo.osImage"), row("status.nodeInfo.kernelVersion"), row("status.nodeInfo.containerRuntimeVersion"), row("status.capacity.cpu"), row("status.capacity.memory"), row("status.capacity.pods")]
  },
  Pod: {
    spec: [ro("spec.nodeName"), ro("spec.serviceAccountName"), ro("spec.hostNetwork", {fmt: F.bool})],
    status: [row("status.phase"), row("status.podIP"), row("status.hostIP"), row("status.startTime", {fmt: F.date})]
  }
};

// static immutability, from the CRDs' `self == oldSelf` rules on main, used
// only when the CRD itself cannot be read
const STATIC_IMMUTABLE = {
  StorageCluster: ["spec.deviceClass", "spec.fabricType", "spec.stripe", "spec.nvmfBasePort", "spec.rpcBasePort", "spec.snodeApiPort", "spec.enableFailureDomains", "spec.enableSyncReplication", "spec.enableNodeAffinity",
    "spec.enableChecksumValidation", "spec.enableAtomicity4K", "spec.enableMetadataJournal", "spec.storageNodes.mgmtInterface", "spec.storageNodes.dataInterfaces", "spec.storageNodes.network", "spec.storageNodes.nodesPerSocket",
    "spec.storageNodes.enableJournalDevice", "spec.storageNodes.enableFormat4K", "spec.storageNodes.enableBlockFormat", "spec.stretched.site", "spec.stretched.peerSite", "spec.superclusterRef", "spec.kms"],
  StorageNode: ["spec.clusterRef", "spec.nodeSet", "spec.slot", "spec.config.journalManager", "spec.config.deviceNames", "spec.config.mgmtInterface", "spec.config.dataInterfaces", "spec.config.pcieDenyList", "spec.config.pcieModel", "spec.config.driveSizeRange", "spec.config.failureDomain", "spec.config.site", "spec.config.expand", "spec.stretched.remote"],
  StorageDevice: ["spec.nodeRef", "spec.deviceID"],
  StoragePool: ["spec.clusterRef", "spec.volumeDefaults"],
  ControlPlane: ["spec.source.local.tls"],
  SimplyblockDriver: ["spec.driverName"],
  ClusterDeploymentConfig: ["spec.clusterRef", "spec.environment"],
  StorageBackupPolicy: ["spec.clusterRef", "spec.schedule"],
  StorageBackup: ["spec.clusterRef", "spec.source"],
  StorageClusterOps: ["spec.clusterRef", "spec.action", "spec.cancelTask.taskID"], StorageNodeOps: ["spec.nodeRef", "spec.action", "spec.migrate.targetWorkerNode"],
  StorageDeviceOps: ["spec.deviceRef", "spec.action"], StoragePoolOps: ["spec.poolRef", "spec.action"],
  PersistentVolumeOps: ["spec.persistentVolumeName", "spec.action", "spec.migrate.targetNodeRef", "spec.relocate.targetClusterRef", "spec.creatorRef"],
  ControlPlaneOps: ["spec.controlPlaneRef", "spec.action", "spec.upgrade", "spec.restart", "spec.backup", "spec.hostMaintenance"],
  OperatorOps: ["spec.action"], StorageBackupOps: ["spec.action", "spec.clusterRef", "spec.backupRef", "spec.groupBackupID", "spec.groupBackupRef", "spec.restore"],
  ReplicationPair: ["spec.targetCluster"], ReplicationSlot: ["spec.policyRef", "spec.pvcRef", "spec.volumeID"],
  PersistentVolume: ["spec.capacity", "spec.accessModes", "spec.storageClassName", "spec.volumeMode", "spec.csi", "spec.claimRef"],
  PersistentVolumeClaim: ["spec.storageClassName", "spec.accessModes", "spec.volumeMode", "spec.volumeName", "spec.dataSource", "spec.dataSourceRef"],
  StorageClass: ["provisioner", "parameters", "reclaimPolicy", "volumeBindingMode"],
  VolumeSnapshot: ["spec.volumeSnapshotClassName", "spec.source"], VolumeSnapshotContent: ["spec.driver", "spec.volumeSnapshotRef", "spec.source"]
};

// the map from a view-model kind to its CR kind (what the two tiers render)
const VM_CR_KIND = {cluster: "StorageCluster", node: "StorageNode", device: "StorageDevice", pool: "StoragePool", volume: "PersistentVolume", pvc: "PersistentVolumeClaim",
  storageclass: "StorageClass", snapshot: "VolumeSnapshot", backup: "StorageBackup", policy: "StorageBackupPolicy", host: "Node",
  deployconfig: "ClusterDeploymentConfig", slot: "ReplicationSlot", pair: "ReplicationPair", rpolicy: "ReplicationPolicy", replops: "ReplicationOps", controlplane: "ControlPlane", driver: "SimplyblockDriver"};

// drill-down cards a kind offers beyond its curated detail; each resolves to a nav call
const RELATIONS = {
  cluster: [
    {go: "monitor", icon: "clock", title: "Operations", sub: "the Ops objects on this cluster and its nodes, and the control plane's task window"},
    {go: "alerts", icon: "bell", title: "Alerts", sub: "open and recently resolved"}
  ],
  node: [
    {go: "monitor", icon: "clock", title: "Operations", sub: "restarts, drains, and migrations of this node"}
  ],
  pool: [{layer: "pvcs", icon: "volume", title: "Claims", sub: "PVCs provisioned through this pool's classes"}],
  volume: [{layer: "snapshots", icon: "camera", title: "Snapshots", sub: "VolumeSnapshots of this claim"}]
};

Object.assign(window, {HEADLINES, STATIC_IMMUTABLE, VM_CR_KIND, RELATIONS, F});

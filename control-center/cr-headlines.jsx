// ---------------------------------------------------------------------------
// CR HEADLINES — what matters first on each kind, and how kinds relate
//
// A custom resource has dozens of fields. The first tier of a detail shows
// the ones an operator reads or changes most, in the order they are asked
// about; the second tier shows everything. The tables here are that first
// tier: a path into spec or status, a label, how to format it, and whether
// it may be edited in place. Immutability is read from the CRD's own
// validation rules at runtime (cr-view.jsx); `mutable: false` here is the
// fallback when the schema cannot be read, and `mutable: "ops"` marks a field
// that changes only through an operation.
//
// RELATIONS lists the drill-down cards a kind offers beyond what its curated
// detail already shows.
// ---------------------------------------------------------------------------
const F = {
  bytes: v => (v === null || v === undefined ? null : typeof v === "number" ? fmtBytes(v) : v),
  bool: v => (v === null || v === undefined ? null : v ? "yes" : "no"),
  date: v => (v ? fmtDate(v) : null),
  ago: v => (v ? fmtAgo(v) : null),
  list: v => (Array.isArray(v) ? (v.length ? v.join(", ") : "none") : v),
  map: v => (v && typeof v === "object" ? Object.entries(v).map(([k, x]) => `${k}=${x}`).join(", ") || "none" : v),
  pct: v => (v === null || v === undefined ? null : `${v}%`),
  num: v => (v === null || v === undefined ? null : fmtNum(v)),
  raw: v => (v === null || v === undefined ? null : typeof v === "object" ? JSON.stringify(v) : String(v))
};
const row = (path, label, opts) => Object.assign({path, label, fmt: F.raw, mutable: true, type: "text"}, opts || {});

const HEADLINES = {
  StorageCluster: {
    spec: [
      row("spec.vcpuCount", "vCPUs per node", {type: "number", hint: "at least 6"}),
      row("spec.maxSubsystemCount", "Max subsystems", {type: "number", hint: "10 to 75"}),
      row("spec.maxHugePagesSize", "Hugepages floor"),
      row("spec.maxConcurrentWorkerRestarts", "Concurrent worker restarts", {type: "number"}),
      row("spec.enableNodeAffinity", "Node affinity", {type: "bool", fmt: F.bool, mutable: false}),
      row("spec.enableFailureDomains", "Failure domains", {type: "bool", fmt: F.bool, mutable: false}),
      row("spec.warningThreshold.capacity", "Capacity warning", {type: "number", fmt: F.pct}),
      row("spec.criticalThreshold.capacity", "Capacity critical", {type: "number", fmt: F.pct}),
      row("spec.warningThreshold.provisionedCapacity", "Provisioned warning", {type: "number", fmt: F.pct}),
      row("spec.criticalThreshold.provisionedCapacity", "Provisioned critical", {type: "number", fmt: F.pct}),
      row("spec.clientDataIfname", "Client data interface"),
      row("spec.backup.localEndpoint", "Backup endpoint"),
      row("spec.backup.snapshotBackups", "Snapshot backups", {type: "bool", fmt: F.bool}),
      row("spec.backup.withCompression", "Backup compression", {type: "bool", fmt: F.bool}),
      row("spec.volumeMigrationSettings.enabled", "Volume migration", {type: "bool", fmt: F.bool}),
      row("spec.volumeAutoPlacement.enabled", "Automatic placement", {type: "bool", fmt: F.bool}),
      row("spec.volumeAutoPlacement.metricsBackend", "Placement metrics", {type: "select", options: ["controlplane", "prometheus", "uniform"]}),
      row("spec.fabricType", "Fabric", {mutable: false}),
      row("spec.stripe.dataChunks", "Data chunks", {mutable: false, type: "number"}),
      row("spec.stripe.parityChunks", "Parity chunks", {mutable: false, type: "number"}),
      row("spec.nvmfBasePort", "NVMe-oF base port", {mutable: false, type: "number"}),
      row("spec.rpcBasePort", "RPC base port", {mutable: false, type: "number"}),
      row("spec.snodeApiPort", "Node API port", {mutable: false, type: "number"}),
      row("spec.hashicorpVaultSettings.baseURL", "Vault", {mutable: false})
    ],
    status: [
      row("status.uuid", "Cluster UUID"), row("status.status", "Backend status"), row("status.phase", "Phase"), row("status.subPhase", "Sub-phase"),
      row("status.nqn", "NQN"), row("status.rebalancing", "Rebalancing", {fmt: F.bool}), row("status.erasureCodingScheme", "Erasure coding"),
      row("status.maxFaultTolerance", "Fault tolerance"), row("status.configured", "Configured", {fmt: F.bool}), row("status.activeOpsRef", "Active operation"),
      row("status.volumeMoveGeneration", "Volume move generation"), row("status.realignedGeneration", "Realigned generation"), row("status.lastDataRealignmentAt", "Last realignment", {fmt: F.date}),
      row("status.rebalancingMetrics.imbalancePercent", "Imbalance", {fmt: F.pct}), row("status.rebalancingMetrics.lastEvaluatedAt", "Placement evaluated", {fmt: F.ago})
    ]
  },
  StorageNode: {
    spec: [
      row("spec.storageNodeSetRef", "Node set", {mutable: false, link: "nodeset"}),
      row("spec.workerNode", "Worker node", {mutable: false, link: "worker", hint: "changed only by a migrate operation"}),
      row("spec.socketId", "NUMA socket", {mutable: false, type: "number"}), row("spec.nodeIndex", "Node index", {mutable: false, type: "number"}), row("spec.socketIndex", "Socket index", {mutable: false, type: "number"}),
      row("spec.overrides.spdkSystemMemory", "SPDK system memory"), row("spec.overrides.reservedSystemCPU", "Reserved system CPUs"),
      row("spec.overrides.enableCpuTopology", "CPU topology", {type: "bool", fmt: F.bool}), row("spec.overrides.failureDomain", "Failure domain override", {type: "number"}),
      row("spec.overrides.pcieAllowList", "PCIe allow list", {type: "list", fmt: F.list}), row("spec.overrides.pcieDenyList", "PCIe deny list", {type: "list", fmt: F.list}),
      row("spec.overrides.driveSizeRange", "Drive size range"), row("spec.overrides.deviceNames", "Device names", {type: "list", fmt: F.list})
    ],
    status: [
      row("status.uuid", "Node UUID"), row("status.status", "Backend status"), row("status.health", "Health", {fmt: F.bool}), row("status.hostname", "Hostname"), row("status.uptime", "Uptime"),
      row("status.failureDomain", "Failure domain"), row("status.activeOpsRef", "Active operation"),
      row("status.resources.cpu", "CPUs"), row("status.resources.memory", "Memory"), row("status.resources.volumes", "Volumes"), row("status.resources.devices", "Devices (online/total)"),
      row("status.resources.capacity.totalBytes", "Capacity", {fmt: F.bytes}), row("status.resources.capacity.usedBytes", "Used", {fmt: F.bytes}), row("status.resources.capacity.sampledAt", "Sampled", {fmt: F.ago}),
      row("status.ports.management", "Management address"), row("status.ports.nvmeof", "NVMe-oF port"), row("status.ports.lvol", "Volume port"), row("status.ports.rpc", "RPC port"),
      row("status.latencyMetrics.baselineP50NS", "Latency p50 (ns)", {fmt: F.num}), row("status.latencyMetrics.baselineP99NS", "Latency p99 (ns)", {fmt: F.num}), row("status.postedAt", "Last posted", {fmt: F.ago})
    ]
  },
  StorageDevice: {
    spec: [row("spec.nodeRef", "Node", {mutable: false, link: "node"}), row("spec.deviceID", "Device ID", {mutable: false})],
    status: [
      row("status.phase", "Phase"), row("status.deviceStatus", "Backend status"), row("status.role", "Role"),
      row("status.capacity.totalBytes", "Capacity", {fmt: F.bytes}), row("status.capacity.usedBytes", "Used", {fmt: F.bytes}),
      row("status.hardware.pciAddress", "PCI address"), row("status.hardware.serialNumber", "Serial"), row("status.hardware.model", "Model"), row("status.hardware.nvmeController", "Controller"),
      row("status.clusterID", "Cluster UUID"), row("status.nodeID", "Node UUID"), row("status.message", "Message"), row("status.observedGeneration", "Observed generation")
    ]
  },
  StoragePool: {
    spec: [
      row("spec.clusterName", "Cluster", {mutable: false, link: "cluster"}),
      row("spec.capacityLimit", "Capacity limit"), row("spec.logicalVolumeMaxSize", "Max volume size"),
      row("spec.qos.iops", "QoS IOPS", {type: "number"}), row("spec.qos.throughput.read", "QoS read MB/s", {type: "number"}), row("spec.qos.throughput.write", "QoS write MB/s", {type: "number"}), row("spec.qos.throughput.readWrite", "QoS read+write MB/s", {type: "number"}),
      row("spec.allowedNodes", "Allowed nodes", {type: "list", fmt: F.list}), row("spec.dhchap", "DH-CHAP", {type: "bool", fmt: F.bool, mutable: false}),
      row("spec.storageClassParameters.filesystem", "Filesystem", {mutable: false}), row("spec.storageClassParameters.encryption", "Encryption", {mutable: false}),
      row("spec.storageClassParameters.qosRwIops", "Class QoS IOPS", {mutable: false}), row("spec.storageClassParameters.qosRwMbytes", "Class QoS MB/s", {mutable: false}),
      row("spec.storageClassParameters.maxNamespacePerSubsys", "Namespaces per subsystem", {mutable: false}), row("spec.storageClassParameters.tune2fsReservedBlocks", "Reserved blocks", {mutable: false})
    ],
    status: [row("status.uuid", "Pool UUID"), row("status.status", "Backend status"), row("status.qos.host", "QoS host"), row("status.qos.iops", "Applied IOPS"), row("status.allowedNodes", "Applied allowed nodes", {fmt: F.list})]
  },
  StorageNodeSet: {
    spec: [
      row("spec.clusterName", "Cluster", {mutable: false, link: "cluster"}), row("spec.workerNodes", "Worker nodes", {type: "list", fmt: F.list, hint: "adding a worker adds a node; removing one drains it"}),
      row("spec.clusterImage", "Cluster image"), row("spec.spdkImage", "SPDK image"), row("spec.spdkProxyImage", "SPDK proxy image"),
      row("spec.mgmtIfname", "Management interface", {mutable: false}), row("spec.dataIfname", "Data interfaces", {type: "list", fmt: F.list}),
      row("spec.nodesPerSocket", "Nodes per socket", {mutable: false, type: "number"}), row("spec.socketsToUse", "Sockets to use", {type: "list", fmt: F.list}),
      row("spec.maxParallelNodeAdds", "Parallel node adds", {type: "number"}), row("spec.enableJournalDevice", "Journal device", {type: "bool", fmt: F.bool, mutable: false}),
      row("spec.journalManager.count", "Journal managers", {type: "number"}), row("spec.journalManager.percentPerDevice", "Journal percent per device", {type: "number"}),
      row("spec.pcieAllowList", "PCIe allow list", {type: "list", fmt: F.list}), row("spec.pcieDenyList", "PCIe deny list", {type: "list", fmt: F.list}), row("spec.driveSizeRange", "Drive size range"),
      row("spec.nodeFailureDomains", "Failure domains", {type: "kv", fmt: F.map}), row("spec.reservedSystemCPU", "Reserved system CPUs"), row("spec.spdkSystemMemory", "SPDK system memory"),
      row("spec.enableCpuTopology", "CPU topology", {type: "bool", fmt: F.bool}), row("spec.forceFormat4K", "Force 4K format", {type: "bool", fmt: F.bool, mutable: false}), row("spec.expand", "Expand", {type: "bool", fmt: F.bool}),
      row("spec.imagePullPolicy", "Image pull policy", {type: "select", options: ["Always", "Never", "IfNotPresent"]})
    ],
    status: [
      row("status.totalNodes", "Nodes"), row("status.onlineNodes", "Online"), row("status.offlineNodes", "Offline"), row("status.suspendedNodes", "Suspended"), row("status.creatingNodes", "Creating"), row("status.removedNodes", "Removed"),
      row("status.pendingNodeAdds", "Pending node adds", {fmt: F.map}), row("status.schedulingFailedWorkers", "Scheduling failed", {fmt: F.map})
    ],
    tables: [
      {path: "status.nodes", title: "Nodes reported by the control plane", columns: ["hostname", "uuid", "status", "health", "mgmtIp", "cpu", "memory", "volumes", "devices", "failureDomain", "postedAt"]},
      {path: "status.drainCoordination", title: "Drains in progress", columns: ["hostname", "phase", "activeNodeUUID", "startedAt", "message"]},
      {path: "status.latencyMetrics", title: "Latency baselines", columns: ["nodeUUID", "baselineP50NS", "baselineP99NS", "baselineMeasuredAt"]}
    ]
  },
  ControlPlane: {
    spec: [row("spec.image", "Image")],
    status: [row("status.phase", "Phase"), row("status.message", "Message"), row("status.lastChecked", "Last checked", {fmt: F.ago})]
  },
  PersistentVolume: {
    spec: [
      row("spec.capacity.storage", "Capacity", {mutable: false}), row("spec.accessModes", "Access modes", {fmt: F.list, mutable: false}), row("spec.storageClassName", "Storage class", {mutable: false, link: "storageclass"}),
      row("spec.persistentVolumeReclaimPolicy", "Reclaim policy", {type: "select", options: ["Retain", "Delete"]}), row("spec.volumeMode", "Volume mode", {mutable: false}),
      row("spec.csi.driver", "CSI driver", {mutable: false}), row("spec.csi.volumeHandle", "Volume handle", {mutable: false}), row("spec.csi.fsType", "Filesystem", {mutable: false}),
      row("spec.claimRef.namespace", "Claim namespace", {mutable: false}), row("spec.claimRef.name", "Claim", {mutable: false, link: "pvc"})
    ],
    status: [row("status.phase", "Phase"), row("status.reason", "Reason"), row("status.message", "Message")]
  },
  PersistentVolumeClaim: {
    spec: [
      row("spec.storageClassName", "Storage class", {mutable: false, link: "storageclass"}), row("spec.resources.requests.storage", "Requested", {hint: "grow only"}),
      row("spec.accessModes", "Access modes", {fmt: F.list, mutable: false}), row("spec.volumeMode", "Volume mode", {mutable: false}), row("spec.volumeName", "Volume", {mutable: false, link: "pv"}),
      row("spec.dataSource", "Data source", {fmt: F.raw, mutable: false}), row("spec.dataSourceRef", "Data source ref", {fmt: F.raw, mutable: false})
    ],
    status: [row("status.phase", "Phase"), row("status.capacity.storage", "Capacity"), row("status.accessModes", "Access modes", {fmt: F.list}), row("status.allocatedResources.storage", "Allocated")],
    annotations: [LABELS.backupPolicy, LABELS.replicationPolicy, LABELS.selectedNode, LABELS.onDiskFs, "simplyblock.io/qos-rw-iops", "simplyblock.io/qos-rw-mbps", "simplyblock.io/pod-affinity", "simplyblock.io/lvol-id"]
  },
  StorageClass: {
    spec: [
      row("provisioner", "Provisioner", {mutable: false}), row("parameters.cluster_id", "Cluster UUID", {mutable: false}), row("parameters.pool_name", "Pool", {mutable: false, link: "pool"}),
      row("parameters.qos_rw_iops", "QoS IOPS", {mutable: false}), row("parameters.qos_rw_mbytes", "QoS MB/s", {mutable: false}), row("parameters.encryption", "Encryption", {mutable: false}),
      row("parameters.fabric", "Fabric", {mutable: false}), row("parameters.max_namespace_per_subsys", "Namespaces per subsystem", {mutable: false}), row("parameters.csi.storage.k8s.io/fstype", "Filesystem", {mutable: false}),
      row("reclaimPolicy", "Reclaim policy", {mutable: false}), row("volumeBindingMode", "Binding mode", {mutable: false}), row("allowVolumeExpansion", "Volume expansion", {type: "bool", fmt: F.bool}),
      row("mountOptions", "Mount options", {fmt: F.list})
    ],
    status: []
  },
  VolumeSnapshot: {
    spec: [row("spec.volumeSnapshotClassName", "Snapshot class", {mutable: false}), row("spec.source.persistentVolumeClaimName", "Source claim", {mutable: false, link: "pvc"}), row("spec.source.volumeSnapshotContentName", "Source content", {mutable: false})],
    status: [row("status.readyToUse", "Ready to use", {fmt: F.bool}), row("status.creationTime", "Created", {fmt: F.date}), row("status.restoreSize", "Restore size"), row("status.boundVolumeSnapshotContentName", "Content", {link: "vsc"}), row("status.error.message", "Error")]
  },
  VolumeSnapshotContent: {
    spec: [row("spec.driver", "Driver", {mutable: false}), row("spec.deletionPolicy", "Deletion policy", {type: "select", options: ["Retain", "Delete"]}), row("spec.volumeSnapshotRef.namespace", "Snapshot namespace", {mutable: false}), row("spec.volumeSnapshotRef.name", "Snapshot", {mutable: false})],
    status: [row("status.readyToUse", "Ready to use", {fmt: F.bool}), row("status.snapshotHandle", "Snapshot handle"), row("status.restoreSize", "Restore size", {fmt: F.bytes}), row("status.creationTime", "Created")]
  },
  StorageClusterOps: {spec: [row("spec.clusterRef", "Cluster", {mutable: false, link: "cluster"}), row("spec.action", "Action", {mutable: false}), row("spec.nodeRollingRestart.refreshSNodeAPI", "Refresh node API", {fmt: F.bool, mutable: false})],
    status: [row("status.phase", "Phase"), row("status.message", "Message"), row("status.startedAt", "Started", {fmt: F.date}), row("status.completedAt", "Completed", {fmt: F.date}), row("status.nodeRollingRestartStatus.nodePhase", "Node phase"), row("status.nodeRollingRestartStatus.pendingNodes", "Pending nodes", {fmt: F.list}), row("status.nodeRollingRestartStatus.processedNodes", "Processed nodes", {fmt: F.list})]},
  StorageNodeOps: {spec: [row("spec.storageNodeRef", "Node", {mutable: false, link: "node"}), row("spec.action", "Action", {mutable: false}), row("spec.targetWorkerNode", "Target worker", {mutable: false}), row("spec.force", "Force", {fmt: F.bool, mutable: false}), row("spec.reattachVolume", "Reattach volumes", {fmt: F.bool, mutable: false})],
    status: [row("status.phase", "Phase"), row("status.subPhase", "Sub-phase"), row("status.message", "Message"), row("status.volumesMigrated", "Volumes migrated"), row("status.volumesPending", "Volumes pending"), row("status.startedAt", "Started", {fmt: F.date}), row("status.completedAt", "Completed", {fmt: F.date})]},
  VolumeMigration: {spec: [row("spec.pvName", "Volume", {mutable: false, link: "pv"}), row("spec.targetNodeUUID", "Target node", {mutable: false}), row("spec.abort", "Abort", {type: "bool", fmt: F.bool})],
    status: [row("status.phase", "Phase"), row("status.migrationUUID", "Migration UUID"), row("status.sourceNodeUUID", "Source node"), row("status.subsystemNQN", "Subsystem"), row("status.memberCount", "Volumes in subsystem"), row("status.errorMessage", "Error"), row("status.startedAt", "Started", {fmt: F.date}), row("status.completedAt", "Completed", {fmt: F.date})]},
  OperatorOps: {spec: [row("spec.action", "Action", {mutable: false}), row("spec.abort", "Abort", {type: "bool", fmt: F.bool}), row("spec.discover.configName", "Config name", {mutable: false}), row("spec.discover.clusterRef", "Grows cluster", {mutable: false}), row("spec.discover.nodeSelector", "Node selector", {fmt: F.map, mutable: false})],
    status: [row("status.phase", "Phase"), row("status.step.state", "Step"), row("status.configRef", "Wrote config"), row("status.workers", "Workers", {fmt: F.list}), row("status.environment", "Environment"), row("status.message", "Message"), row("status.observedGeneration", "Observed generation")]},
  ClusterDeploymentConfig: {spec: [row("spec.approved", "Approved", {type: "bool", fmt: F.bool, hint: "one-way"}), row("spec.environment", "Environment", {mutable: false}), row("spec.edgeCluster", "Edge cluster", {fmt: F.bool}), row("spec.clusterRef", "Grows cluster", {mutable: false}), row("spec.cluster.name", "Cluster name"), row("spec.cluster.maxSubsystemCount", "Max subsystems", {type: "number"}), row("spec.cluster.vcpuCount", "vCPUs", {type: "number"})],
    status: [row("status.phase", "Phase"), row("status.step.state", "Step"), row("status.clusterRef", "Cluster"), row("status.nodeRefs", "Nodes", {fmt: F.list}), row("status.message", "Message")]},
  BackupPolicy: {spec: [row("spec.clusterName", "Cluster", {mutable: false, link: "cluster"}), row("spec.schedule", "Schedule", {mutable: false, hint: "interval,keep pairs"}), row("spec.maxVersions", "Max versions", {mutable: false}), row("spec.maxAge", "Max age", {mutable: false})],
    status: [row("status.phase", "Phase"), row("status.message", "Message"), row("status.policyID", "Policy ID"), row("status.clusterUUID", "Cluster UUID")]},
  StorageBackup: {spec: [row("spec.clusterName", "Cluster", {mutable: false}), row("spec.pvcRef.namespace", "Claim namespace", {mutable: false}), row("spec.pvcRef.name", "Claim", {mutable: false, link: "pvc"}), row("spec.snapshotName", "Snapshot", {mutable: false})],
    status: [row("status.phase", "Phase"), row("status.message", "Message"), row("status.backupID", "Backup ID"), row("status.prevBackupID", "Previous backup"), row("status.size", "Size", {fmt: F.bytes}), row("status.poolName", "Pool"), row("status.lvolID", "Volume UUID"), row("status.nodeID", "Node UUID"), row("status.createdAt", "Created", {fmt: F.date}), row("status.completedAt", "Completed", {fmt: F.date})]},
  BackupRestore: {spec: [row("spec.clusterName", "Cluster", {mutable: false}), row("spec.backupRef.name", "Backup", {mutable: false}), row("spec.targetPool", "Target pool", {mutable: false}), row("spec.targetNode", "Target node", {mutable: false})],
    status: [row("status.phase", "Phase"), row("status.message", "Message"), row("status.pvcName", "Restored claim"), row("status.pvName", "Restored volume"), row("status.startedAt", "Started", {fmt: F.date}), row("status.completedAt", "Completed", {fmt: F.date})]},
  ReplicationSlot: {spec: [row("spec.policyRef", "Policy", {mutable: false}), row("spec.pvcRef", "Claim", {mutable: false}), row("spec.volumeID", "Volume handle", {mutable: false})],
    status: [row("status.state", "State"), row("status.direction", "Direction"), row("status.sourceLvolID", "Source volume"), row("status.targetLvolID", "Target volume"), row("status.lastReplicatedAt", "Last replicated", {fmt: F.ago}), row("status.message", "Message")]},
  Node: {spec: [row("spec.unschedulable", "Unschedulable", {fmt: F.bool}), row("spec.taints", "Taints", {fmt: F.raw})],
    status: [row("status.nodeInfo.kubeletVersion", "Kubelet"), row("status.nodeInfo.osImage", "OS"), row("status.nodeInfo.kernelVersion", "Kernel"), row("status.nodeInfo.containerRuntimeVersion", "Runtime"), row("status.capacity.cpu", "CPUs"), row("status.capacity.memory", "Memory"), row("status.capacity.pods", "Pods")]},
  Pod: {spec: [row("spec.nodeName", "Worker", {mutable: false}), row("spec.serviceAccountName", "Service account", {mutable: false}), row("spec.hostNetwork", "Host network", {fmt: F.bool, mutable: false})],
    status: [row("status.phase", "Phase"), row("status.podIP", "Pod IP"), row("status.hostIP", "Host IP"), row("status.startTime", "Started", {fmt: F.date})]}
};

// static immutability, used when the CRD cannot be read
const STATIC_IMMUTABLE = {
  StorageCluster: ["spec.fabricType", "spec.hashicorpVaultSettings", "spec.stripe", "spec.nvmfBasePort", "spec.rpcBasePort", "spec.snodeApiPort", "spec.enableNodeAffinity", "spec.enableFailureDomains"],
  StorageNode: ["spec.storageNodeSetRef", "spec.socketId", "spec.nodeIndex", "spec.socketIndex"],
  StorageDevice: ["spec.nodeRef", "spec.deviceID"],
  StoragePool: ["spec.clusterName", "spec.dhchap", "spec.storageClassParameters"],
  StorageNodeSet: ["spec.clusterName", "spec.mgmtIfname", "spec.enableJournalDevice", "spec.nodesPerSocket", "spec.forceFormat4K"],
  StorageClusterOps: ["spec.clusterRef", "spec.action"], StorageNodeOps: ["spec.storageNodeRef", "spec.action", "spec.targetWorkerNode"],
  VolumeMigration: ["spec.pvName", "spec.targetNodeUUID"], OperatorOps: ["spec.action"],
  BackupPolicy: ["spec.clusterName", "spec.maxVersions", "spec.maxAge", "spec.schedule"], StorageBackup: ["spec.clusterName"], BackupRestore: ["spec.clusterName", "spec.backupRef"],
  ReplicationPair: ["spec.targetCluster"], ReplicationSlot: ["spec.policyRef", "spec.pvcRef", "spec.volumeID"],
  PersistentVolume: ["spec.capacity", "spec.accessModes", "spec.storageClassName", "spec.volumeMode", "spec.csi", "spec.claimRef"],
  PersistentVolumeClaim: ["spec.storageClassName", "spec.accessModes", "spec.volumeMode", "spec.volumeName", "spec.dataSource", "spec.dataSourceRef"],
  StorageClass: ["provisioner", "parameters", "reclaimPolicy", "volumeBindingMode"],
  VolumeSnapshot: ["spec.volumeSnapshotClassName", "spec.source"], VolumeSnapshotContent: ["spec.driver", "spec.volumeSnapshotRef", "spec.source"]
};

// the map from a view-model kind to its CR kind (what the two tiers render)
const VM_CR_KIND = {cluster: "StorageCluster", node: "StorageNode", device: "StorageDevice", pool: "StoragePool", volume: "PersistentVolume", pvc: "PersistentVolumeClaim",
  storageclass: "StorageClass", snapshot: "VolumeSnapshot", backup: "StorageBackup", policy: "BackupPolicy", host: "Node", nodeset: "StorageNodeSet",
  deployconfig: "ClusterDeploymentConfig", slot: "ReplicationSlot", pair: "ReplicationPair", rpolicy: "ReplicationPolicy", replops: "ReplicationOps"};

// drill-down cards a kind offers beyond its curated detail; each resolves to a nav call
const RELATIONS = {
  cluster: [
    {layer: "nodesets", icon: "node", title: "Node sets", sub: "the StorageNodeSets that own this cluster's nodes"},
    {go: "served", icon: "k8s", title: "Kubernetes consumers", sub: "namespaces and claims served by this cluster"},
    {go: "monitor", icon: "clock", title: "Operations", sub: "running and past operations on this cluster"},
    {go: "logs", icon: "list", title: "Logs", sub: "storage plane, control plane, event log, events"},
    {go: "alerts", icon: "bell", title: "Alerts", sub: "open and recently resolved"}
  ],
  node: [
    {layer: "nodevolumes", icon: "volume", title: "Volumes served", sub: "primary on this node (needs the control plane)"},
    {go: "pods", icon: "host", title: "Pods", sub: "the SPDK and node API pods on its worker"},
    {go: "monitor", icon: "clock", title: "Operations", sub: "restarts, drains, migrations of this node"}
  ],
  pool: [{layer: "pvcs", icon: "volume", title: "Claims", sub: "PVCs provisioned through this pool's class"}],
  volume: [{layer: "snapshots", icon: "camera", title: "Snapshots", sub: "VolumeSnapshots of this claim"}, {layer: "clones", icon: "copy", title: "Clones", sub: "claims cloned from this one"}, {go: "pods", icon: "host", title: "Pods", sub: "workloads mounting the claim"}]
};

Object.assign(window, {HEADLINES, STATIC_IMMUTABLE, VM_CR_KIND, RELATIONS, F});

// ---------------------------------------------------------------------------
// OPERATIONS MODEL: the step graphs of every Ops kind, transcribed from the
// operator's reconcilers on main (operator/internal/controllers/*/graphs.go)
// and recorded in OPERATIONS.md. It lives here, before api.jsx, because the
// normalizers, the monitor, and the action dialogs all read it: which steps an
// action walks, in which order, and at which of them spec.abort is honored.
// Nothing in this file is a guess; a step the operator does not declare is not
// drawn.
// ---------------------------------------------------------------------------

// steps per kind and action, in graph order; `abortable` lists the steps whose
// state declares Abortable in the graph. A step absent from `steps` can still
// appear in status.step.state when an action's graph branches (RollingRestart
// skips RefreshingPod and AwaitingPod unless spec.rollingRestart.refreshSNodeAPI
// is set; a Relocate loops AwaitingCutover and Proceeding per volume).
const OPS_GRAPHS = {
  StorageClusterOps: {
    Activate:       {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Expand:         {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Shutdown:       {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Start:          {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    CancelTask:     {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Restart:        {steps: ["ShuttingDown", "Starting"], abortable: []},
    RollingRestart: {steps: ["CheckingPeers", "ShuttingDownNode", "RefreshingPod", "AwaitingPod", "RestartingNode", "Rebalancing"],
      abortable: ["CheckingPeers", "Rebalancing"], perNode: true}
  },
  StorageNodeOps: {
    Shutdown:        {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Suspend:         {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Resume:          {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Restart:         {steps: ["Requesting", "Departing", "Awaiting"], abortable: ["Requesting"]},
    Remove:          {steps: ["Validating", "ShuttingDown", "MigratingDevices", "MigratingVolumes", "Verifying", "Removing", "AwaitingRemoval"], abortable: ["Validating"]},
    Migrate:         {steps: ["Preparing", "Relocating", "AwaitingNode", "Promoting"], abortable: ["Preparing"]},
    HostMaintenance: {steps: ["Holding", "ShuttingDown", "Releasing", "AwaitingHost", "Restarting", "Cleanup"], abortable: ["Holding"]}
  },
  StorageDeviceOps: {
    Restart:     {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    Fail:        {steps: ["Removing", "Failing", "Awaiting"], abortable: ["Removing"]},
    CheckHealth: {steps: ["Requesting"], abortable: ["Requesting"]}
  },
  StoragePoolOps: {
    Rebalance: {steps: ["Validating", "Migrating"], abortable: []}
  },
  PersistentVolumeOps: {
    Migrate:  {steps: ["Validating", "Migrating", "Verifying"], abortable: ["Validating", "Migrating"]},
    Relocate: {steps: ["Validating", "Replicating", "Committing", "AwaitingCutover", "AttachingPaths", "Proceeding", "Verifying"], abortable: ["Validating", "Replicating"]},
    JoinConsistencyGroup: {steps: ["Planning", "Leaving", "Migrating", "Joining", "SelectingSubsystem", "PreparingTarget", "AttachingPaths", "Suspending", "MovingNamespaces", "Resuming", "Committing", "ReleasingPaths"],
      abortable: ["Planning", "Migrating", "SelectingSubsystem", "PreparingTarget", "AttachingPaths"]}
  },
  ControlPlaneOps: {
    Restart:         {steps: ["Draining", "Restarting", "Awaiting"], abortable: ["Draining"]},
    Upgrade:         {steps: ["Preflight", "Draining", "Applying", "Awaiting", "Verifying"], abortable: ["Preflight", "Draining"]},
    Backup:          {steps: ["Requesting", "Awaiting"], abortable: ["Requesting"]},
    HostMaintenance: {steps: ["Holding", "EnteringMaintenance", "MovingCoordinators", "ExcludingProcesses", "Releasing", "AwaitingHost", "IncludingProcesses", "LeavingMaintenance", "Cleanup"], abortable: ["Holding"]}
  },
  OperatorOps: {
    Discover: {steps: ["Inspecting", "Probing", "Writing"], abortable: []}
  },
  StorageBackupOps: {
    Restore: {steps: ["Validating", "Restoring", "AwaitingVolume", "AwaitingVolumes", "Binding"], abortable: []}
  }
};

// entity machines that are not Ops objects but walk the same step shape
const ENTITY_GRAPHS = {
  // StorageNode provisioning (a node add); Adopting is the exit a node takes
  // when a backend node already exists for its worker
  StorageNode: {steps: ["CheckingHost", "CheckingConfig", "CheckingNetwork", "AwaitingSlot", "Posting", "Resolving", "AwaitingWorker", "Adopting"]},
  // ClusterDeploymentConfig expansion
  ClusterDeploymentConfig: {steps: ["Validating", "CreatingCluster", "AwaitingCluster", "CreatingNodes", "Activating"]},
  // StorageCluster creation
  StorageCluster: {steps: ["Claiming", "CheckingControlPlane", "ResolvingConfig", "Creating", "Adopting", "Persisting"]},
  // ControlPlane installation
  ControlPlane: {steps: ["ApplyingFoundationDB", "AwaitingFoundationDB", "BuildingIndices", "ApplyingDatastore", "ApplyingAPI", "AwaitingAPI", "Installed"]}
};

const OPS_PHASES = ["Pending", "Running", "Succeeded", "Failed", "Aborted"];
const OPS_TERMINAL_PHASES = ["Succeeded", "Failed", "Aborted"];

// the control plane's task types (shared/openapi.json, TaskDTO.function_name)
const TASK_TYPES = ["device_restart", "node_restart", "device_migration", "failed_device_migration", "new_device_migration",
  "node_add", "node_removal", "port_allow", "balancing_on_restart", "balancing_on_dev_rem", "balancing_on_dev_add",
  "jc_comp_resume", "snapshot_replication", "lvol_sync_del", "lvol_sync_op", "lvol_migration", "lvol_batch_migration",
  "s3_backup", "s3_backup_restore", "s3_backup_merge", "group_backup_commit", "group_backup_delete", "cluster_expand",
  "replication_final", "fdb_backup", "sync_promote", "sync_resync", "sync_site_fence"];
const TASK_STATUSES = ["new", "running", "suspended", "done"];
// the task types the operator itself reads as "the cluster is rebalancing"
const REBALANCING_TASK_TYPES = ["node_add", "cluster_expand", "new_device_migration"];

// The graph for one object: the Ops kind's action, or the entity's machine.
function graphOf(kind, action) {
  const g = OPS_GRAPHS[kind];
  if (g) return (action && g[action]) || null;
  return ENTITY_GRAPHS[kind] || null;
}
// Where a step stands in its graph: index, whether the operator honors an
// abort there, and the ordered list with the current one marked. A step the
// graph does not list (a branch the action took) is appended as observed.
function stepPosition(kind, action, state) {
  const g = graphOf(kind, action);
  if (!g) return {index: -1, steps: state ? [state] : [], abortable: false};
  const steps = g.steps.slice();
  if (state && state !== "Completed" && !steps.includes(state)) steps.push(state);
  const index = state === "Completed" ? steps.length : steps.indexOf(state);
  return {index, steps, abortable: !!(g.abortable || []).includes(state), perNode: !!g.perNode};
}

Object.assign(window, {OPS_GRAPHS, ENTITY_GRAPHS, OPS_PHASES, OPS_TERMINAL_PHASES, TASK_TYPES, TASK_STATUSES, REBALANCING_TASK_TYPES, graphOf, stepPosition});

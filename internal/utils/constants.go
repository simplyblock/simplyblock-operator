package utils

const (
	FinalizerPool                = "storage.simplyblock.io/pool-finalizer"
	FinalizerLvol                = "storage.simplyblock.io/lvol-finalizer"
	FinalizerTask                = "storage.simplyblock.io/task-finalizer"
	FinalizerDevice              = "storage.simplyblock.io/device-finalizer"
	FinalizerSnapshotReplication = "storage.simplyblock.io/snapshotreplication-finalizer"
	FinalizerStorageNode         = "storage.simplyblock.io/storagenode-finalizer"
	FinalizerStorageCluster      = "storage.simplyblock.io/cluster-finalizer"

	ClusterActionActivate    = "activate"
	ClusterActionExpand      = "expand"
	ClusterActionShutdown    = "shutdown"
	ClusterActionStart       = "start"
	ClusterActionRestart     = "restart"
	ClusterActionNodeRecycle = "node-recycle"

	// NodeRecycle per-node phases
	NodeRecyclePhaseSnodeRefresh     = "snode-refresh"
	NodeRecyclePhaseSnodeRefreshWait = "snode-refresh-wait"
	NodeRecyclePhaseShuttingDown     = "shutting-down"
	NodeRecyclePhaseRestarting       = "restarting"
	NodeRecyclePhaseRebalancing      = "rebalancing"

	ActionStateRunning = "running"
	ActionStateSuccess = "success"
	ActionStateFailed  = "failed"

	TaskStateDone = "done"

	ClusterStatusActive    = "active"
	ClusterStatusSuspended = "suspended"

	NodeStatusOnline      = "online"
	NodeStatusOffline     = "offline"
	NodeStatusInRestart   = "in_restart"
	NodeStatusUnreachable = "unreachable"

	DeviceActionRemove  = "remove"
	DeviceActionRestart = "restart"

	DeviceStatusRemoved = "removed"
	DeviceStatusOnline  = "online"

	ENDPOINT       = "http://simplyblock-webappapi:5000"
	CSIProvisioner = "csi.simplyblock.io"
)

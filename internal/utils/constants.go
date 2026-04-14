package utils

const (
	FinalizerPool           = "storage.simplyblock.io/pool-finalizer"
	FinalizerLvol           = "storage.simplyblock.io/lvol-finalizer"
	FinalizerTask           = "storage.simplyblock.io/task-finalizer"
	FinalizerDevice         = "storage.simplyblock.io/device-finalizer"
	FinalizerStorageNode    = "storage.simplyblock.io/storagenode-finalizer"
	FinalizerStorageCluster = "storage.simplyblock.io/cluster-finalizer"

	ClusterActionActivate = "activate"
	ClusterActionExpand   = "expand"

	ActionStateRunning = "running"
	ActionStateSuccess = "success"
	ActionStateFailed  = "failed"

	TaskStateDone = "done"

	ClusterStatusActive    = "active"
	ClusterStatusSuspended = "suspended"

	NodeStatusOnline      = "online"
	NodeStatusOffline     = "offline"
	NodeStatusUnreachable = "unreachable"

	DeviceActionRemove  = "remove"
	DeviceActionRestart = "restart"

	DeviceStatusRemoved = "removed"
	DeviceStatusOnline  = "online"

	ENDPOINT = "http://simplyblock-webappapi:5000"
)

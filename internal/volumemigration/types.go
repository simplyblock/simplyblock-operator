package volumemigration

type StorageNode struct {
	UUID        string
	PoolUUID    string
	ClusterUUID string
}

type StorageNodeCandidate struct {
	StorageNode
	Score float64
}

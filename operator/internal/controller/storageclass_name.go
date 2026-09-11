// The name of the StorageClass a pool generates, as this package spells it.
//
// The formula itself is atlas-lib's, because the upgrade tool checks names
// against it and a producer and a checker that disagree would report a collision
// nothing can reproduce. This is the local name for it, kept so that the call
// sites in this package read the way they always did.

package controller

import atlaskube "github.com/simplyblock/atlas/kube"

func simplyblockStorageClassName(namespace, clusterName, poolName string) string {
	return atlaskube.StorageClassNameFor(namespace, clusterName, poolName)
}

package controller

import "fmt"

func simplyblockStorageClassName(namespace, clusterName, poolName string) string {
	return fmt.Sprintf("simplyblock-%s-%s-%s", namespace, clusterName, poolName)
}

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// The storage cluster most controller tests migrate, replicate or rebalance against.
const testClusterUUID = "cluster-uuid"

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func testCluster(namespace, clusterName, uuid string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
		Spec: simplyblockv1alpha1.StorageClusterSpec{},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID: uuid,
		},
	}
}

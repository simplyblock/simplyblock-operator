package utils

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
)

func ResolvePoolUUID(
	ctx context.Context,
	c client.Client,
	namespace string,
	clusterName string,
	poolName string,
) (string, error) {

	var pools simplyblockv1alpha1.SimplyBlockPoolList
	if err := c.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return "", err
	}

	for _, p := range pools.Items {
		if p.Spec.ClusterName == clusterName &&
			p.Spec.Name == poolName &&
			p.Status.UUID != "" {
			return p.Status.UUID, nil
		}
	}

	return "", fmt.Errorf("pool %q not found or UUID not ready", poolName)
}

func ResolveClusterUUID(
	ctx context.Context,
	c client.Client,
	namespace string,
	clusterName string,
) (string, error) {

	var clusters simplyblockv1alpha1.SimplyBlockStorageClusterList
	if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return "", err
	}

	for _, cluster := range clusters.Items {
		if cluster.Name == clusterName && cluster.Status.UUID != "" {
			return cluster.Status.UUID, nil
		}
	}

	return "", fmt.Errorf("cluster %q not found or UUID not ready", clusterName)
}

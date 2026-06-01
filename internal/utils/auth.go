package utils

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetClusterUUID retrieves the cluster UUID for a given cluster name from its Kubernetes secret.
func GetClusterUUID(ctx context.Context, cli client.Client, namespace, clusterIdentifier string) (string, error) {
	var clusterName string
	if IsUUID(clusterIdentifier) {
		name, err := GetClusterNameByUUID(ctx, cli, namespace, clusterIdentifier)
		if err != nil {
			return "", err
		}
		clusterName = name
	} else {
		clusterName = clusterIdentifier
	}

	secretName := fmt.Sprintf("simplyblock-cluster-%s", clusterName)
	secret := &corev1.Secret{}
	if err := cli.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: namespace,
	}, secret); err != nil {
		return "", fmt.Errorf("failed to get cluster secret '%s': %w", secretName, err)
	}

	uuidBytes, ok := secret.Data["uuid"]
	if !ok {
		return "", fmt.Errorf("secret %s missing 'uuid'", secretName)
	}

	return string(uuidBytes), nil
}

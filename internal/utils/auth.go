package utils

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetClusterAuth retrieves UUID and secret for a given cluster name.
// 'cli' is a controller-runtime client (e.g., r.Client from a reconciler)
func GetClusterAuth(ctx context.Context, cli client.Client, namespace, clusterIdentifier string) (string, string, error) {
	var clusterName string
	if IsUUID(clusterIdentifier) {
		name, err := GetClusterNameByUUID(ctx, cli, namespace, clusterIdentifier)
		if err != nil {
			return "", "", err
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
		return "", "", fmt.Errorf("failed to get cluster secret '%s': %w", secretName, err)
	}

	uuidBytes, ok := secret.Data["uuid"]
	if !ok {
		return "", "", fmt.Errorf("secret %s missing 'uuid'", secretName)
	}

	secretBytes, ok := secret.Data["secret"]
	if !ok {
		return "", "", fmt.Errorf("secret %s missing 'secret'", secretName)
	}

	return string(uuidBytes), string(secretBytes), nil
}

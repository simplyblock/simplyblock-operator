package utils

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"

	"github.com/simplyblock/simplyblock-manager/internal/webapi"
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
		if cluster.Spec.ClusterName == clusterName && cluster.Status.UUID != "" {
			return cluster.Status.UUID, nil
		}
	}

	return "", fmt.Errorf("cluster %q not found or UUID not ready", clusterName)
}

func ExistingClusterUUID(
	ctx context.Context,
	c client.Client,
	namespace string,
) (exists bool, uuid string, clusterName string, err error) {

	var clusters simplyblockv1alpha1.SimplyBlockStorageClusterList

	if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return false, "", "", err
	}

	for _, cluster := range clusters.Items {
		if cluster.Status.UUID != "" {
			return true, cluster.Status.UUID, cluster.Spec.ClusterName, nil
		}
	}

	return false, "", "", nil
}

func CountOnlineHealthyNodes(
	nodes []simplyblockv1alpha1.NodeStatus,
) int {
	count := 0
	for _, n := range nodes {
		if n.Status == "online" && n.Health {
			count++
		}
	}
	return count
}

func ShouldActivateCluster(
	mod int,
	onlineHealthy int,
	workerNodes []string,
) bool {

	required := mod + 1

	return onlineHealthy == len(workerNodes) &&
		onlineHealthy >= required
}

func ClusterAlreadyActive(cluster *simplyblockv1alpha1.SimplyBlockStorageCluster) bool {
	return cluster.Status.Status == "active"
}

func ActivateCluster(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	clusterSecret string,
) error {

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/activate", clusterUUID)

	body, status, err := apiClient.Do(
		ctx,
		clusterSecret,
		http.MethodPost,
		endpoint,
		nil,
	)

	if err != nil {
		return err
	}

	if status >= 300 {
		return fmt.Errorf(
			"cluster activation failed: status=%d body=%s",
			status,
			string(body),
		)
	}

	return nil
}

func RequiredNodesFromMOD(mod string) (int, error) {
	parts := strings.Split(mod, "x")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid MOD format: %s", mod)
	}

	ndcs, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}

	npcs, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}

	return ndcs + npcs, nil
}

package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

type ClusterGetResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type NodeStatusResponse struct {
	UUID   string `json:"id"`
	Status string `json:"status"`
	IP     string `json:"mgmt_ip"`
}

type Lvol struct {
	UUID        string `json:"id"`
	Name        string `json:"name"`
	DoReplicate bool   `json:"do_replicate"`
	NQN         string `json:"nqn"`
	Status      string `json:"status"`

	RepInfo *ReplicationInfo `json:"rep_info,omitempty"`
}

type ReplicationInfo struct {
	LastSnapshotUUID        string    `json:"last_snapshot_id,omitempty"`
	LastReplicationTime     *FlexTime `json:"last_replication_time,omitempty"`
	LastReplicationDuration string    `json:"last_replication_duration,omitempty"`
	ReplicatedCount         int64     `json:"replicated_count,omitempty"`
}

type SnapshotTask struct {
	UUID         string `json:"id"`
	Status       string `json:"status"`
	FunctionName string `json:"function_name"`
	CreatedDT    string `json:"create_dt,omitempty"`
}

type lvolActiveSidesResponse struct {
	Source bool `json:"from_source"`
}

var ErrLvolNotFound = errors.New("lvol not found")

func ResolvePoolUUID(
	ctx context.Context,
	c client.Client,
	namespace string,
	clusterName string,
	poolName string,
) (string, error) {

	var pools simplyblockv1alpha1.PoolList
	if err := c.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return "", err
	}

	for _, p := range pools.Items {
		if p.Spec.ClusterName == clusterName &&
			p.Name == poolName &&
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

	var clusters simplyblockv1alpha1.StorageClusterList
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

func ResolveClusterIdentifier(ctx context.Context, k8sClient client.Client, namespace, cluster string) (string, error) {
	if IsUUID(cluster) {
		return cluster, nil
	}
	return ResolveClusterUUID(ctx, k8sClient, namespace, cluster)
}

func ResolvePoolIdentifier(ctx context.Context, k8sClient client.Client, namespace, cluster, pool string) (string, error) {
	if pool == "" {
		return "", nil
	}
	if IsUUID(pool) {
		return pool, nil
	}
	return ResolvePoolUUID(ctx, k8sClient, namespace, cluster, pool)
}

func ResolveClusterCR(
	ctx context.Context,
	c client.Client,
	namespace string,
	clusterName string,
) (*simplyblockv1alpha1.StorageCluster, error) {

	var clusters simplyblockv1alpha1.StorageClusterList
	if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for i := range clusters.Items {
		cluster := &clusters.Items[i]

		if cluster.Name == clusterName {
			return cluster, nil
		}
	}

	return nil, fmt.Errorf("cluster %q not found", clusterName)
}

func ExistingClusterUUID(
	ctx context.Context,
	c client.Client,
) (exists bool, uuid string, clusterName string, clusterNamespace string, err error) {

	var clusters simplyblockv1alpha1.StorageClusterList

	if err := c.List(ctx, &clusters); err != nil {
		return false, "", "", "", err
	}

	for _, cluster := range clusters.Items {
		if cluster.Status.UUID != "" {
			return true, cluster.Status.UUID, cluster.Name, cluster.Namespace, nil
		}
	}

	return false, "", "", "", nil
}

func GetClusterNameByUUID(ctx context.Context, cli client.Client, namespace, uuid string) (string, error) {
	clusterList := &simplyblockv1alpha1.StorageClusterList{}
	if err := cli.List(ctx, clusterList, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("failed to list clusters: %w", err)
	}

	for _, c := range clusterList.Items {
		if c.Status.UUID == uuid {
			return c.Name, nil
		}
	}
	return "", fmt.Errorf("no cluster found with UUID %s", uuid)
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

// ExpectedNodesPerHost returns how many backend storage-nodes the control-plane
// will create for a single Kubernetes worker when socketsToUse is configured.
// Without socketsToUse it is always 1.
func ExpectedNodesPerHost(snCR *simplyblockv1alpha1.StorageNode) int {
	if len(snCR.Spec.SocketsToUse) == 0 {
		return 1
	}
	nodesPerSocket := 1
	if snCR.Spec.NodesPerSocket != nil && *snCR.Spec.NodesPerSocket > 0 {
		nodesPerSocket = int(*snCR.Spec.NodesPerSocket)
	}
	return len(snCR.Spec.SocketsToUse) * nodesPerSocket
}

func ShouldActivateCluster(
	mod int,
	onlineHealthy int,
	snCR *simplyblockv1alpha1.StorageNode,
) bool {

	required := mod + 1

	expected := len(snCR.Spec.WorkerNodes) * ExpectedNodesPerHost(snCR)

	return onlineHealthy == expected &&
		onlineHealthy >= required
}

func ClusterAlreadyActive(cluster *simplyblockv1alpha1.StorageCluster) bool {
	return cluster.Status.Status == "active"
}

func ClusterInExpansion(cluster *simplyblockv1alpha1.StorageCluster) bool {
	return cluster.Status.Status == "in_expansion"
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

// ClusterListEntry is a single item returned by GET /api/v2/clusters/.
type ClusterListEntry struct {
	UUID   string `json:"id"`
	Secret string `json:"secret"`
	Name   string `json:"name"`
	NQN    string `json:"nqn"`
	Status string `json:"status"`
	NDCS   int    `json:"distr_ndcs"`
	NPCS   int    `json:"distr_npcs"`
}

// GetClusterByName lists all clusters and returns the one matching name.
// Returns nil if no match is found.
func GetClusterByName(ctx context.Context, apiClient *webapi.Client, clusterSecret, name string) (*ClusterListEntry, error) {
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, "/api/v2/clusters/", nil)
	if err != nil || status >= 300 {
		return nil, fmt.Errorf("list clusters failed, status %d: %v, body: %s", status, err, string(body))
	}
	var entries []ClusterListEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("failed to parse cluster list: %w", err)
	}
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i], nil
		}
	}
	return nil, nil
}

func IsClusterActive(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
) (bool, string, error) {

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)

	body, status, err := apiClient.Do(
		ctx,
		clusterSecret,
		http.MethodGet,
		endpoint,
		nil,
	)
	if err != nil {
		return false, "", err
	}

	if status >= 300 {
		return false, "", fmt.Errorf(
			"failed to get cluster status: status=%d body=%s",
			status,
			string(body),
		)
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		return false, "", err
	}

	return resp.Status == "active", resp.Status, nil
}

func WaitForClusterActive(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	timeout time.Duration,
	pollInterval time.Duration,
) error {

	log := logf.FromContext(ctx)
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for cluster %s to become active", clusterUUID)
		}

		active, status, err := IsClusterActive(
			ctx,
			apiClient,
			clusterSecret,
			clusterUUID,
		)
		if err != nil {
			log.Error(err, "Failed to check cluster activation status")
		} else {
			log.Info("Waiting for cluster activation",
				"clusterUUID", clusterUUID,
				"status", status,
			)

			if active {
				log.Info("Cluster is now active", "clusterUUID", clusterUUID)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func ActivateClusterAndWait(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
) error {

	if err := ActivateCluster(ctx, apiClient, clusterUUID, clusterSecret); err != nil {
		return err
	}

	return WaitForClusterActive(
		ctx,
		apiClient,
		clusterSecret,
		clusterUUID,
		5*time.Minute,  // total timeout
		10*time.Second, // poll interval
	)
}

func ClusterSuspended(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID string) (bool, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		return false, fmt.Errorf("failed to get cluster %s, status %d: %v, body: %s", clusterUUID, status, err, string(body))
	}

	var resp ClusterGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, fmt.Errorf("failed to unmarshal cluster response: %w", err)
	}

	return strings.EqualFold(resp.Status, ClusterStatusSuspended), nil
}

func AllStorageNodesUnreachable(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret,
	clusterUUID string,
) (bool, error) {

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		return false, fmt.Errorf(
			"failed to list storage nodes for cluster %s, status %d: %v, body: %s",
			clusterUUID, status, err, string(body),
		)
	}

	var nodes []NodeStatusResponse
	if err := json.Unmarshal(body, &nodes); err != nil {
		return false, fmt.Errorf("failed to unmarshal storage nodes response: %w", err)
	}

	if len(nodes) == 0 {
		return false, nil
	}

	for _, n := range nodes {
		if !strings.EqualFold(n.Status, NodeStatusUnreachable) {
			return false, nil
		}
	}

	return true, nil
}

func RequiredNodesFromErasureCodingScheme(scheme string) (int, error) {
	parts := strings.Split(scheme, "x")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid erasureCodingScheme format: expected <dataChunks>x<parityChunks> like %q, got %q", "2x1", scheme)
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

func GetPoolUUIDs(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID string) ([]string, error) {
	log := logf.FromContext(ctx)
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		return nil, fmt.Errorf("failed to list pools, status %d: %v, body: %s", status, err, string(body))
	}

	log.Info("GetPoolUUIDs API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	var pools []struct {
		UUID string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pools: %w", err)
	}

	uuids := make([]string, 0, len(pools))
	for _, p := range pools {
		uuids = append(uuids, p.UUID)
	}
	return uuids, nil
}

func GetLvols(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID string) ([]Lvol, error) {
	log := logf.FromContext(ctx)
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/",
		clusterUUID,
		poolUUID,
	)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		return nil, fmt.Errorf("failed to list lvols for pool %s, status %d: %v, body: %s", poolUUID, status, err, string(body))
	}

	log.Info("GetLvols API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	var lvols []Lvol
	if err := json.Unmarshal(body, &lvols); err != nil {
		return nil, fmt.Errorf("failed to unmarshal lvols: %w", err)
	}

	return lvols, nil
}

func GetLvol(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolUUID string,
) (*Lvol, error) {
	log := logf.FromContext(ctx)

	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s",
		clusterUUID,
		poolUUID,
		lvolUUID,
	)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get lvol %s for pool %s: %w",
			lvolUUID, poolUUID, err,
		)
	}

	if status == http.StatusNotFound {
		return nil, fmt.Errorf("%w: lvol %s in pool %s", ErrLvolNotFound, lvolUUID, poolUUID)
	}

	if status >= 300 {
		return nil, fmt.Errorf(
			"failed to get lvol %s for pool %s, status %d: body: %s",
			lvolUUID, poolUUID, status, string(body),
		)
	}

	log.Info("GetLvol API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	var lvol Lvol
	if err := json.Unmarshal(body, &lvol); err != nil {
		return nil, fmt.Errorf("failed to unmarshal lvol %s: %w", lvolUUID, err)
	}

	return &lvol, nil
}

func ShouldFailoverToRepCluster(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret,
	clusterUUID string,
) (bool, error) {

	suspended, err := ClusterSuspended(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil || !suspended {
		return suspended, err
	}

	allUnreachable, err := AllStorageNodesUnreachable(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		return false, err
	}

	return allUnreachable, nil
}

func GetSnapshotTasks(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolUUID string,
) ([]SnapshotTask, error) {

	log := logf.FromContext(ctx)

	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/list_replication_tasks/",
		clusterUUID,
		poolUUID,
		lvolUUID,
	)

	body, status, err := apiClient.Do(
		ctx,
		clusterSecret,
		http.MethodGet,
		endpoint,
		nil,
	)

	if err != nil || status >= 300 {
		return nil, fmt.Errorf(
			"failed to list snapshot tasks for lvol %s, status %d: %v, body: %s",
			lvolUUID,
			status,
			err,
			string(body),
		)
	}

	log.Info("GetSnapshotTasks API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	var tasks []SnapshotTask
	if err := json.Unmarshal(body, &tasks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshot tasks: %w", err)
	}

	return tasks, nil
}

func GetLastSnapshotTaskDoneStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolUUID string,
) (bool, *SnapshotTask, error) {
	tasks, err := GetSnapshotTasks(
		ctx,
		apiClient,
		clusterSecret,
		clusterUUID,
		poolUUID,
		lvolUUID,
	)
	if err != nil {
		return false, nil, err
	}

	if len(tasks) == 0 {
		return true, &SnapshotTask{
			Status: "empty",
		}, nil
	}

	lastTask := tasks[len(tasks)-1]
	done := strings.EqualFold(lastTask.Status, TaskStateDone)
	return done, &lastTask, nil
}

func GetReplicationActiveSides(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolUUID string,
) (bool, error) {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/",
		clusterUUID,
		poolUUID,
		lvolUUID,
	)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("failed to get lvol %s: %w", lvolUUID, err)
	}
	if status >= 300 {
		return false, fmt.Errorf("failed to get lvol %s, status %d: %s", lvolUUID, status, string(body))
	}

	var resp lvolActiveSidesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, fmt.Errorf("failed to unmarshal lvol %s active side fields: %w", lvolUUID, err)
	}

	return resp.Source, nil
}

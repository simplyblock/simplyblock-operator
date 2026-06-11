/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// benchmarkPoolName is used by WebAPIBenchmarkProvisioner to name the benchmark storage pool.
const benchmarkPoolName = "fio-benchmark"
// defaultBenchmarkVolumeSizeBytes is the size of each per-node benchmark volume (1 GiB).
// Sufficient for 4K random-write fio workloads without wasting storage.
const defaultBenchmarkVolumeSizeBytes int64 = 1024 * 1024 * 1024

// BenchmarkProvisioner manages the lifecycle of the storage pool and per-node logical
// volumes used by the StorageNodeLatencyReconciler to run fio-based NVMe-oF latency
// benchmarks.
//
// EnsurePool and EnsureVolume are idempotent: they create the resource if absent and
// return the existing UUID if already present. Auth (cluster UUID + secret) is resolved
// internally so callers only need to supply the Kubernetes namespace and cluster name.
//
// Two implementations are provided:
//   - AutomaticBenchmarkProvisioner: no-op for production clusters where the storage pool
//     and benchmark volumes are created automatically during cluster setup. The benchmark
//     volume's logical-volume ID equals the storage node UUID, so EnsureVolume returns
//     nodeUUID unchanged and EnsurePool returns "".
//   - WebAPIBenchmarkProvisioner: explicitly creates resources via the SimplyBlock REST API
//     for test environments where auto-provisioning is not available.
type BenchmarkProvisioner interface {
	// EnsurePool ensures a benchmark storage pool exists in the given cluster.
	// Returns the pool UUID, or "" if the pool is managed automatically.
	EnsurePool(ctx context.Context, namespace, clusterName string) (poolUUID string, err error)

	// EnsureVolume ensures a benchmark volume exists for the given storage node.
	// volumeName is the human-readable label; nodeUUID identifies the target node.
	// Returns the volume UUID used to construct the NQN via BenchmarkNQN.
	// Production implementations return nodeUUID unchanged (the auto-created benchmark
	// volume's lvol ID equals the storage node UUID).
	EnsureVolume(ctx context.Context, namespace, clusterName, poolUUID, volumeName, nodeUUID string) (volumeUUID string, err error)

	// BenchmarkNQN returns the NVMe-oF NQN for the benchmark volume.
	// Both implementations use the same formula: "{clusterNQN}:lvol:{volumeUUID}".
	BenchmarkNQN(clusterNQN, volumeUUID string) string
}

// AutomaticBenchmarkProvisioner is the no-op production implementation.
// It assumes the storage pool and per-node benchmark volumes are created automatically
// during cluster setup. The benchmark volume's logical-volume ID equals the storage node
// UUID, so EnsureVolume returns nodeUUID and EnsurePool returns "".
type AutomaticBenchmarkProvisioner struct{}

func (*AutomaticBenchmarkProvisioner) EnsurePool(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

func (*AutomaticBenchmarkProvisioner) EnsureVolume(_ context.Context, _, _, _, _, nodeUUID string) (string, error) {
	return nodeUUID, nil
}

func (*AutomaticBenchmarkProvisioner) BenchmarkNQN(clusterNQN, volumeUUID string) string {
	return fmt.Sprintf("%s:lvol:%s", clusterNQN, volumeUUID)
}

// WebAPIBenchmarkProvisioner creates benchmark resources via the SimplyBlock REST API.
// Intended for test environments where the storage pool and benchmark volumes are not
// automatically provisioned during cluster setup.
//
// EnsurePool and EnsureVolume are idempotent: they check for an existing resource by
// name before issuing a create request.
type WebAPIBenchmarkProvisioner struct {
	APIClient *webapi.Client
	K8sClient client.Client
}

// EnsurePool lists existing pools and returns the matching one's UUID; creates a new pool
// if no pool with poolName exists.
func (p *WebAPIBenchmarkProvisioner) EnsurePool(ctx context.Context, namespace, clusterName string) (string, error) {
	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, p.K8sClient, namespace, clusterName)
	if err != nil {
		return "", fmt.Errorf("get cluster auth: %w", err)
	}
	pools, err := p.APIClient.GetStoragePools(ctx, clusterSecret, clusterUUID)
	if err != nil {
		return "", fmt.Errorf("list pools: %w", err)
	}
	for _, pool := range pools {
		if pool.Name == benchmarkPoolName {
			return pool.UUID, nil
		}
	}
	created, err := p.APIClient.CreatePool(ctx, clusterSecret, clusterUUID, webapi.StoragePoolCreateParams{Name: benchmarkPoolName})
	if err != nil {
		return "", fmt.Errorf("create benchmark pool %q: %w", benchmarkPoolName, err)
	}
	return created.UUID, nil
}

// EnsureVolume lists existing volumes in poolUUID and returns the matching one's UUID;
// creates a new volume pinned to nodeUUID if no volume with volumeName exists.
func (p *WebAPIBenchmarkProvisioner) EnsureVolume(ctx context.Context, namespace, clusterName, poolUUID, volumeName, nodeUUID string) (string, error) {
	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, p.K8sClient, namespace, clusterName)
	if err != nil {
		return "", fmt.Errorf("get cluster auth: %w", err)
	}
	volumes, err := p.APIClient.GetPoolVolumes(ctx, clusterSecret, clusterUUID, poolUUID)
	if err != nil {
		return "", fmt.Errorf("list volumes: %w", err)
	}
	for _, vol := range volumes {
		if vol.Name == volumeName {
			return vol.UUID, nil
		}
	}
	created, err := p.APIClient.CreateVolume(ctx, clusterSecret, clusterUUID, poolUUID, webapi.VolumeCreateParams{
		Name:   volumeName,
		Size:   defaultBenchmarkVolumeSizeBytes,
		HostID: nodeUUID,
	})
	if err != nil {
		return "", fmt.Errorf("create benchmark volume %q on node %s: %w", volumeName, nodeUUID, err)
	}
	return created.UUID, nil
}

func (*WebAPIBenchmarkProvisioner) BenchmarkNQN(clusterNQN, volumeUUID string) string {
	return fmt.Sprintf("%s:lvol:%s", clusterNQN, volumeUUID)
}

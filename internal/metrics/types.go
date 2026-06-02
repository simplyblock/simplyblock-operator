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

package metrics

import (
	"context"
	"time"
)

// IOOperation distinguishes read and write traffic.
type IOOperation string

const (
	IOOperationRead  IOOperation = "read"
	IOOperationWrite IOOperation = "write"
)

// ErasureScheme identifies the erasure coding configuration of a volume.
type ErasureScheme string

const (
	ErasureScheme1Plus1 ErasureScheme = "1+1"
	ErasureScheme2Plus1 ErasureScheme = "2+1"
	ErasureScheme4Plus1 ErasureScheme = "4+1"
	ErasureScheme1Plus2 ErasureScheme = "1+2"
	ErasureScheme2Plus2 ErasureScheme = "2+2"
	ErasureScheme4Plus2 ErasureScheme = "4+2"
)

// BlockSizeIOMetrics holds throughput data for one (blocksize, operation, erasure scheme) tuple.
// All values represent primary traffic only.
type BlockSizeIOMetrics struct {
	BlockSizeBytes int64
	Operation      IOOperation
	ErasureScheme  ErasureScheme
	IOPS           float64
	BytesPerSecond float64
}

// NodeMetrics is the complete metrics snapshot for a single storage node at a single point in time.
type NodeMetrics struct {
	NodeUUID    string
	CollectedAt time.Time
	IO          []BlockSizeIOMetrics
}

// NodeMetricsProvider is the single abstraction through which the VolumeRebalancerReconciler
// obtains I/O data. All implementations must be safe for concurrent use.
type NodeMetricsProvider interface {
	// GetNodeMetrics returns a point-in-time snapshot for a single node.
	GetNodeMetrics(ctx context.Context, clusterUUID, nodeUUID string) (*NodeMetrics, error)

	// GetClusterMetrics returns metrics for every online node in the cluster.
	// The returned map is keyed by node UUID.
	GetClusterMetrics(ctx context.Context, clusterUUID string) (map[string]*NodeMetrics, error)
}

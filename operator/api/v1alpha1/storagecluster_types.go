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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CapacityThresholdSpec struct {
	// Capacity defines the absolute capacity threshold value.
	Capacity *int32 `json:"capacity,omitempty"`
	// ProvisionedCapacity defines the provisioned-capacity threshold value.
	ProvisionedCapacity *int32 `json:"provisionedCapacity,omitempty"`
}

type StripeSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Data Chunks"
	// DataChunks defines the number of data chunks in the erasure-coding layout.
	DataChunks *int32 `json:"dataChunks,omitempty"`
	// ParityChunks defines the number of parity chunks in the erasure-coding layout.
	ParityChunks *int32 `json:"parityChunks,omitempty"`
}

// VolumeMigrationSettings carries cluster-level settings for volume migration.
// Automatic load-based rebalancing is configured separately via
// StorageClusterSpec.VolumeAutoPlacement, keeping the manual-migration controls
// separate from the rebalancing policy.
type VolumeMigrationSettings struct {
	// Enabled turns on volume migration for this cluster. When false, the operator
	// will not act on VolumeMigration resources for this cluster. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// RebalancerImage is the container image used for the volume-migration path
	// validation Job and the rebalancer latency/baseline Jobs. The image must include
	// nvme-cli (and, for rebalancing, fio + jq).
	// +optional
	RebalancerImage *string `json:"rebalancerImage,omitempty"`
	// DataRealignment configures the periodic control-plane data realignment that
	// runs after volumes have been moved. Realignment re-aligns the cluster's internal
	// data structures to the current volume placement so fault-tolerance (FTT) and
	// node-affinity guarantees are preserved. It applies to *all* volume moves —
	// auto-rebalancing, manual VolumeMigrations, and drain/removal-triggered moves —
	// so it lives here rather than under AutoRebalancing. Enabled by default.
	// +optional
	DataRealignment *DataRealignmentSettings `json:"dataRealignment,omitempty"`
}

// DataRealignmentSettings controls the periodic, post-migration control-plane data
// realignment. After one or more volumes have been moved the operator asks the
// control plane to re-align its internal data structures to the new placement,
// restoring fault-tolerance (FTT) and node-affinity guarantees.
type DataRealignmentSettings struct {
	// Enabled activates automatic post-migration data realignment for this cluster.
	// Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Interval is how often the operator checks whether a realignment is pending
	// (i.e. at least one volume has moved since the last successful realignment) and,
	// if so, triggers it. Explicit triggers (see the
	// simplyblock.io/trigger-realignment annotation) bypass this spacing. Defaults to
	// 10m.
	//
	// Note that this is a floor on the spacing between realignment *requests*, not a
	// ceiling on how long one takes: a realignment blocks all volume migrations for as
	// long as the control plane needs, which on a busy cluster has been measured at
	// tens of minutes. An interval shorter than that means the next realignment is
	// requested as soon as the previous one finishes and any volume has moved, which is
	// what MinMoves exists to damp.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
	// MinMoves is how many volume moves must accumulate before a realignment is
	// triggered. Defaults to 1: every completed migration schedules a realignment.
	//
	// Raise it to batch. Because the control plane refuses new migrations while a
	// realignment runs, a value of 1 makes the two alternate — one migration completes,
	// a realignment follows and blocks migrations until it is done. On a cluster where
	// realignment takes tens of minutes that is most of the available time, so a run
	// that migrates continuously spends the majority of it waiting. A higher value
	// trades realignment promptness (data structures stay unaligned for longer, so
	// fault-tolerance and node-affinity guarantees are restored later) for migration
	// throughput.
	//
	// Explicit triggers (the simplyblock.io/trigger-realignment annotation) ignore this
	// threshold, so a drain or node removal still realigns immediately.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinMoves *int32 `json:"minMoves,omitempty"`
}

// MetricsBackend selects the NodeMetricsProvider implementation.
// +kubebuilder:validation:Enum=controlplane;prometheus;uniform
type MetricsBackend string

const (
	MetricsBackendControlPlane MetricsBackend = "controlplane"
	MetricsBackendPrometheus   MetricsBackend = "prometheus"
	// MetricsBackendUniform returns IOPS=1 for every node, disabling
	// IOPS-based scoring while keeping capacity/volume-count balancing active.
	MetricsBackendUniform MetricsBackend = "uniform"
)

// VolumeAutoPlacementSettings controls the automatic, latency-driven volume rebalancing
// behaviour. It is configured under StorageClusterSpec.VolumeAutoPlacement.
type VolumeAutoPlacementSettings struct {
	// Enabled activates automatic rebalancing for this cluster. Defaults to false.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// MigrationEnabled controls whether the rebalancer actually creates VolumeMigration
	// CRs. When false the rebalancer still runs every cycle — evaluating load, computing
	// deviations, selecting candidates and emitting metrics — but discards the migrations
	// instead of creating them (dry-run). Defaults to true.
	// +optional
	MigrationEnabled *bool `json:"migrationEnabled,omitempty"`
	// EvaluationInterval is how often the rebalancer evaluates load. Defaults to 60s.
	// +optional
	EvaluationInterval *metav1.Duration `json:"evaluationInterval,omitempty"`
	// ImbalanceThreshold is the minimum latency deviation from baseline (in percent)
	// that a node must exhibit before it is considered a rebalancing source. Defaults to 80.
	// +optional
	ImbalanceThreshold *int32 `json:"imbalanceThreshold,omitempty"`
	// MinHotColdDifferencePct is the minimum latency-deviation gap (in percentage points)
	// that a candidate target node must be below the hot source node before a migration is
	// performed. Prevents migrating between near-equally-loaded nodes. Defaults to 20.
	// +optional
	MinHotColdDifferencePct *int32 `json:"minHotColdDifferencePct,omitempty"`
	// DefaultCoolDownSeconds is the cool-down period (seconds) applied to a volume after
	// it has been migrated. Defaults to 600.
	// +optional
	DefaultCoolDownSeconds *int32 `json:"defaultCoolDownSeconds,omitempty"`
	// MaxVolumeMigrationsPerCycle is the maximum number of volumes moved per cycle. Defaults to 10.
	// +optional
	MaxVolumeMigrationsPerCycle *int32 `json:"maxVolumeMigrationsPerCycle,omitempty"`
	// StorageNodeCandidateCount is the number of top-loaded nodes evaluated each cycle to
	// find the best migration source. Defaults to 3.
	// +optional
	StorageNodeCandidateCount *int32 `json:"storageNodeCandidateCount,omitempty"`
	// MetricsBackend selects the data source for I/O metrics. Defaults to "prometheus".
	// +optional
	MetricsBackend *MetricsBackend `json:"metricsBackend,omitempty"`
	// PrometheusURL is required when MetricsBackend is "prometheus".
	// +optional
	PrometheusURL *string `json:"prometheusURL,omitempty"`
	// LatencyBenchmarkEnabled enables fio-based NVMe-oF latency measurement via Kubernetes Jobs.
	// Defaults to false; set to true once a RebalancerImage is configured.
	// +optional
	LatencyBenchmarkEnabled *bool `json:"latencyBenchmarkEnabled,omitempty"`
	// LatencyBenchmarkInterval is how often fio benchmark Jobs run against each storage node.
	// Defaults to 5m.
	// +optional
	LatencyBenchmarkInterval *metav1.Duration `json:"latencyBenchmarkInterval,omitempty"`
	// IOPSWeight is the weight applied to per-volume IOPS in the volume IO score. Defaults to 1.0.
	// +optional
	IOPSWeight *float64 `json:"iopsWeight,omitempty"`
	// ThroughputWeight is the weight applied to per-volume throughput (MB/s) in the volume
	// IO score. Defaults to 0.1.
	// +optional
	ThroughputWeight *float64 `json:"throughputWeight,omitempty"`
}

// NodeLoadMetrics holds the latency deviation state for a single storage node.
type NodeLoadMetrics struct {
	NodeUUID            string      `json:"nodeUUID"`
	LatencyDeviationPct float64     `json:"latencyDeviationPct"`
	VolumeCount         int         `json:"volumeCount"`
	LastUpdated         metav1.Time `json:"lastUpdated"`
}

// RebalancingMetrics is written by the VolumeRebalancerReconciler each evaluation cycle.
type RebalancingMetrics struct {
	// AvgDeviationPct is the mean latency deviation across all nodes.
	AvgDeviationPct float64 `json:"avgDeviationPct"`
	// MaxDeviationPct is the highest per-node latency deviation (used as ImbalancePercent).
	MaxDeviationPct  float64           `json:"maxDeviationPct"`
	HottestNodeUUID  string            `json:"hottestNodeUUID"`
	CoolestNodeUUID  string            `json:"coolestNodeUUID"`
	ImbalancePercent float64           `json:"imbalancePercent"`
	LastEvaluatedAt  *metav1.Time      `json:"lastEvaluatedAt,omitempty"`
	LastMigrationAt  *metav1.Time      `json:"lastMigrationAt,omitempty"`
	NodeMetrics      []NodeLoadMetrics `json:"nodeMetrics,omitempty"`
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type BackupCredentialsSecretRef struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup Credentials Secret"
	// Name is the name of the Secret in the same namespace as the cluster CR.
	Name string `json:"name"`
}

// HashicorpVaultSettings configures the HashiCorp Vault endpoint the cluster uses to store keys.
type HashicorpVaultSettings struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Vault Base URL"
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// BaseURL is the HashiCorp Vault endpoint (e.g. https://vault.example.com:8200).
	BaseURL string `json:"baseURL,omitempty"`
}

type BackupSpec struct {
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	LocalEndpoint string `json:"localEndpoint,omitempty"`
	// +optional
	SnapshotBackups *bool `json:"snapshotBackups,omitempty"`
	// +optional
	WithCompression *bool `json:"withCompression,omitempty"`
	// +optional
	SecondaryTarget *int32 `json:"secondaryTarget,omitempty"`
	// +optional
	LocalTesting *bool `json:"localTesting,omitempty"`
	// CredentialsSecretRef points to the Secret holding access_key_id and secret_access_key.
	CredentialsSecretRef BackupCredentialsSecretRef `json:"credentialsSecretRef"`
}

// StorageClusterSpec defines the desired state of StorageCluster
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.fabricType) || self.fabricType == oldSelf.fabricType",message="fabricType is immutable once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.hashicorpVaultSettings) || self.hashicorpVaultSettings == oldSelf.hashicorpVaultSettings",message="hashicorpVaultSettings is immutable once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.stripe) || self.stripe == oldSelf.stripe",message="stripe is immutable once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.nvmfBasePort) || self.nvmfBasePort == oldSelf.nvmfBasePort",message="nvmfBasePort is immutable once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.rpcBasePort) || self.rpcBasePort == oldSelf.rpcBasePort",message="rpcBasePort is immutable once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.snodeApiPort) || self.snodeApiPort == oldSelf.snodeApiPort",message="snodeApiPort is immutable once set"
type StorageClusterSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Enable Node Affinity"
	// EnableNodeAffinity enables node-affinity placement for storage components.
	EnableNodeAffinity *bool `json:"enableNodeAffinity,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Stripe"
	// StripeSpec configures erasure-coding data/parity chunk counts.
	StripeSpec *StripeSpec `json:"stripe,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Fabric Type"
	// FabricType defines the storage fabric type.
	FabricType string `json:"fabricType,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Client Data Interface"
	// ClientDataIfname defines the client data network interface.
	ClientDataIfname string `json:"clientDataIfname,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="NVMf Base Port"
	// NvmfBasePort defines the base NVMf service port.
	NvmfBasePort *int32 `json:"nvmfBasePort,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="RPC Base Port"
	// RpcBasePort defines the base RPC service port.
	RpcBasePort *int32 `json:"rpcBasePort,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Storage Node API Port"
	// SnodeApiPort defines the storage-node API port.
	SnodeApiPort *int32 `json:"snodeApiPort,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Concurrent Worker Restarts"
	// MaxConcurrentWorkerRestarts is the maximum number of Kubernetes worker nodes the operator
	// may drain and restart simultaneously. The effective concurrency applied by the drain
	// coordinator is min(MaxConcurrentWorkerRestarts, MaxFaultTolerance).
	// Defaults to 1 when unset.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxConcurrentWorkerRestarts *int32 `json:"maxConcurrentWorkerRestarts,omitempty"`

	// The three fields below configure how every storage node in this cluster
	// sizes its SPDK deployment. They live on the StorageCluster rather than the
	// StorageNodeSet because the control plane assumes them to be uniform: huge
	// pages are sized from maxSubsystemCount together with the isolated core
	// count, and a node that disagrees with its peers ends up with a huge-page
	// and core layout the cluster cannot place erasure-coding chunks across
	// evenly. Making them cluster-scoped removes the per-node and per-set
	// override paths that allowed such a divergence.

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Subsystem Count"
	// MaxSubsystemCount is the maximum number of NVMe-oF subsystems per storage
	// node. Applies to every storage node in the cluster. Required: it sizes huge
	// pages, and a node that receives no value fails config generation outright
	// rather than falling back to a default.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=75
	MaxSubsystemCount *int32 `json:"maxSubsystemCount"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Huge Pages Size"
	// MaxHugePagesSize is the maximum allocatable size of huge pages on each
	// storage node (e.g. "100G", "1T"; a bare number is interpreted as GB). It is
	// a floor, not a cap: the effective huge-page allocation is the larger of this
	// value and the minimum the node's device and subsystem count requires. When
	// omitted the computed minimum is used.
	// +optional
	MaxHugePagesSize string `json:"maxHugePagesSize,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="vCPU Count"
	// VCPUCount is the number of vCPUs allocated to SPDK on each storage node.
	// This is an explicit core count, not a percentage. Required: the core layout
	// it produces must match across the cluster, so it is stated rather than left
	// to a per-node heuristic.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=6
	VCPUCount *int32 `json:"vcpuCount"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Warning Threshold"
	// WarningThresholdSpec defines warning-level capacity thresholds.
	WarningThresholdSpec *CapacityThresholdSpec `json:"warningThreshold,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Critical Threshold"
	// CriticalThresholdSpec defines critical-level capacity thresholds.
	CriticalThresholdSpec *CapacityThresholdSpec `json:"criticalThreshold,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Backup"
	// Backup specifies the specification for backup to S3 configuration
	Backup *BackupSpec `json:"backup,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="HashiCorp Vault Settings"
	// HashicorpVaultSettings configures the Vault endpoint used by the cluster for key storage.
	HashicorpVaultSettings *HashicorpVaultSettings `json:"hashicorpVaultSettings,omitempty"`
	// VolumeMigrationSettings controls volume migration for this cluster.
	// +optional
	VolumeMigrationSettings *VolumeMigrationSettings `json:"volumeMigrationSettings,omitempty"`

	// VolumeAutoPlacement configures automatic, latency-driven volume rebalancing. When
	// nil/disabled the operator performs only manually-triggered VolumeMigrations.
	// +optional
	VolumeAutoPlacement *VolumeAutoPlacementSettings `json:"volumeAutoPlacement,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Enable Failure Domains"
	// EnableFailureDomains opts the cluster into failure-domain mode. When enabled, every
	// storage node must declare a failure-domain group so the control plane can spread
	// erasure-coding chunks across independent fault groups. Immutable once set — failure-
	// domain mode cannot be toggled on a live cluster.
	// +k8s:immutable
	// +optional
	EnableFailureDomains *bool `json:"enableFailureDomains,omitempty"`
}

// StorageClusterStatus defines the observed state of StorageCluster.
type StorageClusterStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Cluster UUID"
	// UUID is the backend cluster UUID.
	UUID string `json:"uuid,omitempty"`
	// Phase tracks the cluster creation lifecycle to prevent concurrent reconcilers
	// from creating duplicate clusters. Set to "creation" while a creation is in
	// progress and cleared once the cluster UUID is persisted.
	Phase string `json:"phase,omitempty"`
	// SubPhase tracks the step within the current Phase. Reserved for future
	// sub-state machine expansion; currently only "creating" is used.
	SubPhase string `json:"subPhase,omitempty"`
	// ClusterName is the resolved backend cluster name.
	ClusterName string `json:"clusterName,omitempty"`
	// MgmtNodes is the number of management nodes.
	// FIXME: Unused for now (API update required?)
	MgmtNodes *int32 `json:"mgmtNodes,omitempty"`
	// StorageNodes is the number of storage nodes.
	// FIXME: Unused for now (API update required?)
	StorageNodes *int32 `json:"storageNodes,omitempty"`
	// NQN is the cluster NVM subsystem qualified name.
	NQN string `json:"nqn,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Status"
	// Status is the backend-reported lifecycle status.
	Status string `json:"status,omitempty"`
	// Rebalancing indicates whether cluster rebalancing is currently active.
	Rebalancing *bool `json:"rebalancing,omitempty"`
	// VolumeMoveGeneration counts completed volume moves. Every migration that reaches
	// Completed increments it, and nothing else writes it, so it only ever grows.
	// +optional
	VolumeMoveGeneration *int64 `json:"volumeMoveGeneration,omitempty"`
	// RealignedGeneration is the VolumeMoveGeneration that the last successfully
	// requested realignment covers. A realignment is outstanding while
	// VolumeMoveGeneration exceeds it.
	//
	// This is recorded from the value read *before* the request is sent, because that
	// is what the realignment can actually account for: a migration completing while
	// the request is in flight raises VolumeMoveGeneration past it and so correctly
	// leaves another realignment outstanding, instead of being swallowed by the one
	// already running.
	// +optional
	RealignedGeneration *int64 `json:"realignedGeneration,omitempty"`
	// LastDataRealignmentAt is the time of the last successful control-plane data
	// realignment. It is used to space realignments by DataRealignment.Interval and to
	// avoid re-running at the end of an interval when nothing is pending.
	// +optional
	LastDataRealignmentAt *metav1.Time `json:"lastDataRealignmentAt,omitempty"`
	// ErasureCodingScheme is the active erasure-coding layout, for example "2x1".
	ErasureCodingScheme string `json:"erasureCodingScheme,omitempty"`
	// LastUpdated is the last backend update timestamp.
	// FIXME: Unused for now (API update required?)
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
	// Created is the backend creation timestamp.
	// FIXME: Unused for now (API update required?)
	Created *metav1.Time `json:"created,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Configured"
	// Configured indicates whether initial cluster setup completed.
	Configured bool `json:"configured,omitempty"`
	// MaxFaultTolerance is the backend-reported maximum number of nodes that can
	// be simultaneously offline (failed, drained, or restarted) without violating
	// the cluster's redundancy guarantees.
	MaxFaultTolerance *int32 `json:"maxFaultTolerance,omitempty"`
	// MaxConcurrentWorkerRestarts is the effective concurrent-restart limit applied
	// by the drain coordinator: min(spec.MaxConcurrentWorkerRestarts, MaxFaultTolerance).
	// Defaults to 1. Exposed here so controllers and tooling can read a single
	// authoritative value without re-computing it.
	// +optional
	MaxConcurrentWorkerRestarts *int32 `json:"maxConcurrentWorkerRestarts,omitempty"`
	// ActiveOpsRef is the name of the currently active ClusterOps on this cluster.
	// Empty when no operation is in progress.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`
	// RebalancingMetrics is updated by the auto-rebalancer each evaluation cycle.
	// +optional
	RebalancingMetrics *RebalancingMetrics `json:"rebalancingMetrics,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.status",description="Backend-reported cluster lifecycle status"
// +kubebuilder:printcolumn:name="UUID",type="string",JSONPath=".status.uuid",description="Backend cluster UUID"
// +kubebuilder:printcolumn:name="Configured",type="boolean",JSONPath=".status.configured",description="Whether initial cluster setup has completed"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Cluster"

// StorageCluster is the Schema for the storageclusters API
type StorageCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StorageCluster
	// +required
	Spec StorageClusterSpec `json:"spec"`

	// status defines the observed state of StorageCluster
	// +optional
	Status StorageClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageClusterList contains a list of StorageCluster
type StorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageCluster{}, &StorageClusterList{})
}

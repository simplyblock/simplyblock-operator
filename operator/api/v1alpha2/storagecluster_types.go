// StorageCluster in the shape design-storagecluster.md Appendix A specifies:
// one simplyblock backend cluster, with its layout split from its policy and
// both split from what the control plane reports back.
//
// What moves against the registered v1alpha1 type is §12 of that document, and
// the conversion in api/v1alpha1/storagecluster_conversion.go is the whole of
// the translation. The headline changes are spec.maxHugePagesSize becoming
// spec.minHugePagesSize, spec.hashicorpVaultSettings regrouping under spec.kms,
// spec.backup gaining a bucket and losing the four fields that described how a
// copy is taken, the three bare `enabled` toggles becoming two spec-level
// `enable` fields and one removal, and status gaining a typed phase, a creation
// step, the control plane's task window, and observedGeneration.
//
// One field of Appendix A is deliberately absent. spec.storageNodes is the
// Kubernetes workload the cluster's nodes run as, and its type belongs to
// design-storagenode.md Appendix C, which has not been written yet: StorageNode
// is still v1alpha1 and StorageNodeSet still owns the workload. It lands with
// that kind's move rather than here, where it could only be an empty block.

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// StorageClusterPhase is where the operator has got to with this cluster. The
// first two values are the operator's own creation path; the rest are its
// reading of the lifecycle status.status carries in the control plane's own
// spelling.
// +kubebuilder:validation:Enum=Pending;Creating;Online;Degraded;Unavailable;Suspended
type StorageClusterPhase string

const (
	// StorageClusterPhasePending: the object exists and nothing has been
	// claimed for it yet.
	StorageClusterPhasePending StorageClusterPhase = "Pending"

	// StorageClusterPhaseCreating: the creation machine is running.
	StorageClusterPhaseCreating StorageClusterPhase = "Creating"

	// StorageClusterPhaseOnline: the control plane reports the cluster active
	// and serving.
	StorageClusterPhaseOnline StorageClusterPhase = "Online"

	// StorageClusterPhaseDegraded: serving, with less than the redundancy it
	// was built for.
	StorageClusterPhaseDegraded StorageClusterPhase = "Degraded"

	// StorageClusterPhaseUnavailable: not serving, and not because anybody
	// asked.
	StorageClusterPhaseUnavailable StorageClusterPhase = "Unavailable"

	// StorageClusterPhaseSuspended: shut down deliberately, which is where a
	// Shutdown operation leaves it.
	StorageClusterPhaseSuspended StorageClusterPhase = "Suspended"
)

// StorageClusterStep is one step of the creation path. There is one graph
// rather than a MultiConfig, because an entity has no spec.action to key one
// on.
// +kubebuilder:validation:Enum=Claiming;CheckingControlPlane;ResolvingConfig;Creating;Adopting;Persisting
type StorageClusterStep string

const (
	StorageClusterStepClaiming             StorageClusterStep = "Claiming"
	StorageClusterStepCheckingControlPlane StorageClusterStep = "CheckingControlPlane"
	StorageClusterStepResolvingConfig      StorageClusterStep = "ResolvingConfig"
	StorageClusterStepCreating             StorageClusterStep = "Creating"
	StorageClusterStepAdopting             StorageClusterStep = "Adopting"
	StorageClusterStepPersisting           StorageClusterStep = "Persisting"
)

// StripeSpec is the erasure-coding layout: how many data chunks a stripe
// carries and how many parity chunks protect them.
type StripeSpec struct {
	// DataChunks is the number of data chunks per stripe (ndcs).
	// +kubebuilder:validation:Minimum=1
	// +optional
	DataChunks *int32 `json:"dataChunks,omitempty"`

	// ParityChunks is the number of parity chunks per stripe (npcs), and
	// therefore how many chunk losses a stripe survives.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ParityChunks *int32 `json:"parityChunks,omitempty"`
}

// CapacityThresholdSpec is one capacity alarm level, as a figure for used
// capacity and one for provisioned capacity.
type CapacityThresholdSpec struct {
	// Capacity is the used-capacity threshold.
	// +optional
	Capacity *int64 `json:"capacity,omitempty"`

	// ProvisionedCapacity is the provisioned-capacity threshold.
	// +optional
	ProvisionedCapacity *int64 `json:"provisionedCapacity,omitempty"`
}

// VaultKMS configures the HashiCorp Vault key store.
type VaultKMS struct {
	// BaseURL is the Vault endpoint, for example, https://vault.example.com:8200.
	// Rejected unless it resolves to an external address.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	BaseURL string `json:"baseURL"`
}

// KMSSpec selects where the cluster stores volume encryption keys. It is a
// block with one member per provider so that the providers are siblings, which
// is what makes choosing between them expressible.
type KMSSpec struct {
	// Vault stores keys in HashiCorp Vault.
	// +optional
	Vault *VaultKMS `json:"vault,omitempty"`
}

// BackupStoreSpec is the S3 location a cluster's backups live in, and the
// credentials to reach it. It is a location and nothing else: how backups are
// taken and what they contain are the control plane's, and this block only says
// where they go. It is both the target copies are written to and the inventory
// the operator walks to produce StorageBackup objects.
//
// The whole block is mutable. A cluster can be created without a store and
// given one later, and what changes when it changes is which backups have
// objects, since the object set is derived from the location rather than
// accumulated.
type BackupStoreSpec struct {
	// Endpoint is the S3 endpoint, for example, https://s3.example.com.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// Bucket is the bucket backups are written to and read from.
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// Prefix narrows the store to one key prefix, so that several clusters can
	// share a bucket without each walking the others' backups.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the bucket's region, for endpoints that do not imply one.
	// +optional
	Region string `json:"region,omitempty"`

	// CredentialsSecretRef names the Secret holding the access key and the
	// secret key. It is a reference rather than the values, because a spec is
	// readable by anybody who can read the object.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// StorageClusterDeviceClass is the class of backend storage a cluster is built
// out of. The values are the two classes simplyblock accepts, spelled as the
// standards that name them are, which is the exception design-crd-model.md §7.8
// carries for a word this group did not invent.
// +kubebuilder:validation:Enum=NVMe;LogicalBlock
type StorageClusterDeviceClass string

const (
	// StorageClusterDeviceClassNVMe is a cluster whose nodes hand over NVMe
	// devices, named by PCI address.
	StorageClusterDeviceClassNVMe StorageClusterDeviceClass = "NVMe"

	// StorageClusterDeviceClassLogicalBlock is a cluster whose nodes hand over
	// logical block devices, named by path. The backend accepts them from 26.4.
	StorageClusterDeviceClassLogicalBlock StorageClusterDeviceClass = "LogicalBlock"
)

// MetricsBackend selects where the rebalancer reads a node's I/O load from.
// The values are PascalCase, as every enum this group defines is; v1alpha1
// spelled them lowercase and the conversion maps between the two.
// +kubebuilder:validation:Enum=ControlPlane;Prometheus;Uniform
type MetricsBackend string

const (
	MetricsBackendControlPlane MetricsBackend = "ControlPlane"
	MetricsBackendPrometheus   MetricsBackend = "Prometheus"

	// MetricsBackendUniform reports IOPS=1 for every node, which disables
	// IOPS-based scoring while leaving capacity and volume-count balancing
	// active.
	MetricsBackendUniform MetricsBackend = "Uniform"
)

// BaselineStrategy selects how the per-node latency baseline, the denominator
// of the rebalancing deviation signal, is derived.
// +kubebuilder:validation:Enum=benchmark;rollingWindow
type BaselineStrategy string

const (
	// BaselineStrategyBenchmark uses the one-shot fio measurement taken on a
	// fresh cluster and frozen on the node's status. It is simple and tends to
	// read too low, because an idle cluster is far faster than a loaded one and
	// every loaded node then shows a large deviation.
	BaselineStrategyBenchmark BaselineStrategy = "benchmark"

	// BaselineStrategyRollingWindow derives the baseline from a rolling window
	// of the probe sidecar's latency series, using an outlier-rejecting
	// estimator. It reflects each node's recent operating latency rather than
	// an idle measurement, and is the default.
	BaselineStrategyRollingWindow BaselineStrategy = "rollingWindow"
)

// BaselineColdStartPolicy selects what happens to a node with fewer than
// BaselineMinSamples samples in the window: a freshly onboarded node, or one
// whose probe sidecar has just started.
// +kubebuilder:validation:Enum=defer;partialWindow
type BaselineColdStartPolicy string

const (
	// BaselineColdStartDefer omits an under-sampled node from the cycle: it is
	// neither a migration source nor a target until it has accumulated enough
	// samples, which avoids acting on a noisy baseline.
	BaselineColdStartDefer BaselineColdStartPolicy = "defer"

	// BaselineColdStartPartialWindow computes the baseline from whatever
	// samples exist, accepting a noisier baseline early on so that rebalancing
	// engages sooner. It is the default.
	BaselineColdStartPartialWindow BaselineColdStartPolicy = "partialWindow"
)

// DataRealignmentSettings tunes the post-migration control-plane data
// realignment, which re-aligns the control plane's internal structures to where
// volumes now are and restores the fault-tolerance and node-affinity guarantees
// a move invalidated.
//
// Whether it runs at all is StorageClusterSpec.EnableDataRealignment, a field
// of the spec rather than of this block: a toggle named for its subject repeats
// itself when the subject is also its parent.
type DataRealignmentSettings struct {
	// Interval is a floor on the spacing between realignment requests, and not
	// a ceiling on how long one takes: a realignment blocks every volume
	// migration for as long as the control plane needs, measured at tens of
	// minutes on a loaded cluster. The trigger annotation bypasses it. Defaults
	// to 10m.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// MinMoves is how many volume moves accumulate before a realignment is
	// requested. It defaults to 1, which makes migration and realignment
	// alternate, and is the field to raise in order to batch: a higher value
	// trades how promptly the structures are realigned for migration
	// throughput. The trigger annotation bypasses it.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinMoves *int32 `json:"minMoves,omitempty"`
}

// VolumeMigrationSettings controls how volume migration and the post-migration
// realignment behave, not whether they happen. It is separate from
// VolumeAutoPlacementSettings because realignment applies to every volume move,
// whether it came from the auto-rebalancer, a manual migration, or a drain, so
// it cannot sit under the rebalancing policy that is only one of its three
// sources.
//
// There is no switch here. Migration cannot be turned off: a drain, a
// rebalance, and a device replacement are all performed by moving volumes.
type VolumeMigrationSettings struct {
	// RebalancerImage is the container image the migration path-validation Job
	// and the rebalancer's latency and baseline Jobs run. It must carry
	// nvme-cli, and for rebalancing also fio and jq.
	// +optional
	RebalancerImage *string `json:"rebalancerImage,omitempty"`

	// DataRealignment tunes the post-migration realignment.
	// +optional
	DataRealignment *DataRealignmentSettings `json:"dataRealignment,omitempty"`
}

// VolumeAutoPlacementSettings configures automatic, latency-driven volume
// rebalancing. Whether it runs at all is
// StorageClusterSpec.EnableVolumeAutoPlacement.
type VolumeAutoPlacementSettings struct {
	// DisableMigration stops the rebalancer creating PersistentVolume
	// migrations while leaving every other part of a cycle running: load is
	// evaluated, deviations are computed, candidates are selected, and metrics
	// are emitted, and the migrations are discarded rather than created. It is
	// the dry run, and it is spelled as a disable because the behavior it
	// governs is on by default.
	// +optional
	DisableMigration *bool `json:"disableMigration,omitempty"`

	// EvaluationInterval is how often the rebalancer evaluates load. Defaults
	// to 60s.
	// +optional
	EvaluationInterval *metav1.Duration `json:"evaluationInterval,omitempty"`

	// ImbalanceThreshold is the latency deviation from baseline, in percent, a
	// node must exceed before it is considered a rebalancing source. Defaults
	// to 80.
	// +optional
	ImbalanceThreshold *int32 `json:"imbalanceThreshold,omitempty"`

	// MinHotColdDifferencePct is how far below the hot source, in percentage
	// points of deviation, a candidate target must be before a volume is moved.
	// It is what stops a migration between two near-equally loaded nodes.
	// Defaults to 20.
	// +optional
	MinHotColdDifferencePct *int32 `json:"minHotColdDifferencePct,omitempty"`

	// DefaultCoolDownSeconds is how long a volume is left alone after it has
	// been migrated. Defaults to 600.
	// +optional
	DefaultCoolDownSeconds *int32 `json:"defaultCoolDownSeconds,omitempty"`

	// MaxVolumeMigrationsPerCycle caps how many volumes one cycle moves.
	// Defaults to 10.
	// +optional
	MaxVolumeMigrationsPerCycle *int32 `json:"maxVolumeMigrationsPerCycle,omitempty"`

	// StorageNodeCandidateCount is how many of the most loaded nodes are
	// evaluated each cycle to find the best source. Defaults to 3.
	// +optional
	StorageNodeCandidateCount *int32 `json:"storageNodeCandidateCount,omitempty"`

	// MetricsBackend selects the source of I/O metrics. Defaults to Prometheus.
	// +optional
	MetricsBackend *MetricsBackend `json:"metricsBackend,omitempty"`

	// PrometheusURL is required when MetricsBackend is Prometheus.
	// +optional
	PrometheusURL *string `json:"prometheusURL,omitempty"`

	// EnableLatencyBenchmark turns on fio-based NVMe-oF latency measurement
	// through Kubernetes Jobs. It is off unless a RebalancerImage is
	// configured.
	// +optional
	EnableLatencyBenchmark *bool `json:"enableLatencyBenchmark,omitempty"`

	// LatencyBenchmarkInterval is how often those Jobs run against each storage
	// node. It also sets the step of the rolling-window baseline query, which
	// is the cadence at which the probe sidecar publishes samples. Defaults to
	// 5m.
	// +optional
	LatencyBenchmarkInterval *metav1.Duration `json:"latencyBenchmarkInterval,omitempty"`

	// BaselineStrategy selects how the per-node baseline is derived. Defaults
	// to rollingWindow.
	// +optional
	BaselineStrategy *BaselineStrategy `json:"baselineStrategy,omitempty"`

	// BaselineWindow is the look-back the rollingWindow strategy reduces.
	// Defaults to 6h.
	// +optional
	BaselineWindow *metav1.Duration `json:"baselineWindow,omitempty"`

	// BaselineColdStart selects what happens to an under-sampled node. Defaults
	// to partialWindow.
	// +optional
	BaselineColdStart *BaselineColdStartPolicy `json:"baselineColdStart,omitempty"`

	// BaselineMinSamples is the sample count below which a node counts as
	// under-sampled. Defaults to 6.
	// +optional
	BaselineMinSamples *int32 `json:"baselineMinSamples,omitempty"`

	// BaselineOutlierK is the Hampel-identifier threshold: a sample is rejected
	// when it lies more than k·1.4826·MAD from the window median, so a lower
	// value rejects more. Defaults to 3.0.
	// +optional
	BaselineOutlierK *float64 `json:"baselineOutlierK,omitempty"`

	// IOPSWeight weights per-volume IOPS in the volume I/O score. Defaults to
	// 1.0.
	// +optional
	IOPSWeight *float64 `json:"iopsWeight,omitempty"`

	// ThroughputWeight weights per-volume throughput, in MB/s, in the volume
	// I/O score. Defaults to 0.1.
	// +optional
	ThroughputWeight *float64 `json:"throughputWeight,omitempty"`
}

// NodeLoadMetrics is one storage node's latency deviation, as the last
// evaluation cycle measured it.
type NodeLoadMetrics struct {
	NodeUUID            string      `json:"nodeUUID"`
	LatencyDeviationPct float64     `json:"latencyDeviationPct"`
	VolumeCount         int         `json:"volumeCount"`
	LastUpdated         metav1.Time `json:"lastUpdated"`
}

// RebalancingMetrics is written by the auto-rebalancer each evaluation cycle.
type RebalancingMetrics struct {
	// AvgDeviationPct is the mean latency deviation across the cluster's nodes.
	AvgDeviationPct float64 `json:"avgDeviationPct"`

	// MaxDeviationPct is the highest per-node latency deviation, which is what
	// ImbalancePercent reports.
	MaxDeviationPct  float64           `json:"maxDeviationPct"`
	HottestNodeUUID  string            `json:"hottestNodeUUID"`
	CoolestNodeUUID  string            `json:"coolestNodeUUID"`
	ImbalancePercent float64           `json:"imbalancePercent"`
	LastEvaluatedAt  *metav1.Time      `json:"lastEvaluatedAt,omitempty"`
	LastMigrationAt  *metav1.Time      `json:"lastMigrationAt,omitempty"`
	NodeMetrics      []NodeLoadMetrics `json:"nodeMetrics,omitempty"`
}

// ClusterTask is one asynchronous job the control plane is running, as of the
// last frame of the task stream. It is a window rather than a record: a task
// that reaches a terminal outcome leaves status.tasks, and what remains of it
// is an event.
//
// It carries what the control plane's own TaskDTO carries and nothing more.
// The design's Appendix A also declares a progress figure and a creation date,
// and that schema has neither, so both are absent rather than declared and
// never written (design-crd-model.md §7.9). Their absence is what makes the
// list's order the control plane's own rather than newest first.
type ClusterTask struct {
	// ID is the control plane's identifier, and it is how a CancelTask
	// operation names the task. A position in the list is not an identity,
	// because the next frame may order it differently.
	// +kubebuilder:validation:Required
	ID string `json:"id"`

	// Type is what kind of job it is, in the control plane's own spelling for
	// the reason design-crd-model.md §7.8 gives: the value is the backend's
	// rather than this group's.
	// +optional
	Type string `json:"type,omitempty"`

	// Status is the control plane's own status string, and it is why this entry
	// carries no phase: the operator adds nothing to what the backend reports.
	// Its values are new, running, suspended, and done, of which only the
	// first three appear here.
	// +optional
	Status string `json:"status,omitempty"`

	// Retry is how many times the control plane has restarted this task. It is
	// the one number that separates a task that is slow from one that is
	// failing, and it is the closest thing the schema has to progress.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Retry int32 `json:"retry,omitempty"`
}

// StorageClusterSpec is the desired state of one simplyblock backend cluster.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.kms) || self.kms == oldSelf.kms",message="kms is immutable once set"
// +kubebuilder:validation:XValidation:rule="!(has(self.enableAtomic4kWrites) && self.enableAtomic4kWrites) || (has(self.enableChecksumValidation) && self.enableChecksumValidation)",message="enableAtomic4kWrites requires enableChecksumValidation to be true"
type StorageClusterSpec struct {
	// MaxSubsystemCount is the maximum number of NVMe-oF subsystems per storage
	// node. It is the cluster's and no node carries a copy: every node's
	// configuration is generated from this field, so changing it reaches the
	// nodes that already exist as each of them next restarts.
	// Required: it sizes huge pages, and a node that receives no value fails
	// config generation outright rather than falling back to a default.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=75
	MaxSubsystemCount *int32 `json:"maxSubsystemCount"`

	// VCPUCount is the number of vCPUs allocated to SPDK on each storage node,
	// as an explicit core count rather than a percentage. Unlike
	// MaxSubsystemCount it is stamped onto a node when the node is created,
	// because it describes the host that node runs on. Required: the core
	// layout it produces must match across the cluster in steady state, so it
	// is stated rather than left to a per-node heuristic.
	//
	// The floor is 4 rather than a hardware limit: a node must carry one core
	// beyond this budget for the system, and the control plane's core layout
	// assigns no NVMe-oF poller core at all for a 2-vCPU budget.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=4
	VCPUCount *int32 `json:"vcpuCount"`

	// MinHugePagesSize is the smallest huge-page allocation each storage node
	// makes, as a size string such as 100G or 1T, where a bare number is
	// gigabytes. It is a floor and not a limit: the effective allocation is the
	// larger of this value and the minimum the node's device and subsystem
	// count requires, so simplyblock takes more when it needs more. Omitted,
	// the computed minimum is used.
	// +optional
	MinHugePagesSize string `json:"minHugePagesSize,omitempty"`

	// Stripe is the erasure-coding layout every volume in the cluster is
	// written with. It describes on-disk layout, so it cannot change under a
	// live cluster.
	// +optional
	// +k8s:immutable
	Stripe *StripeSpec `json:"stripe,omitempty"`

	// FabricType is the storage fabric the cluster serves volumes over. It
	// describes on-wire layout, so it cannot change under a live cluster.
	// +optional
	// +k8s:immutable
	FabricType string `json:"fabricType,omitempty"`

	// ClientDataIfname is the network interface clients reach the data plane
	// on.
	// +optional
	ClientDataIfname string `json:"clientDataIfname,omitempty"`

	// NvmfBasePort is the base of the NVMe-oF port range every node binds.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +optional
	// +k8s:immutable
	NvmfBasePort *int32 `json:"nvmfBasePort,omitempty"`

	// RpcBasePort is the base of the RPC port range every node binds.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +optional
	// +k8s:immutable
	RpcBasePort *int32 `json:"rpcBasePort,omitempty"`

	// SnodeApiPort is the port each node's storage-node API listens on.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +optional
	// +k8s:immutable
	SnodeApiPort *int32 `json:"snodeApiPort,omitempty"`

	// EnableFailureDomains opts the cluster into failure-domain mode, where
	// every node must label the fault group it belongs to so the control plane
	// can spread erasure-coding chunks across independent ones.
	// +optional
	// +k8s:immutable
	EnableFailureDomains *bool `json:"enableFailureDomains,omitempty"`

	// EnableNodeAffinity selects affinity-based placement for storage
	// components.
	// +optional
	// +k8s:immutable
	EnableNodeAffinity *bool `json:"enableNodeAffinity,omitempty"`

	// EnableChecksumValidation turns on inline CRC validation of every I/O, for
	// silent-data-error protection. The backend bakes the checksum method into
	// each device at cluster-create time and never re-applies it, so it cannot
	// change under a live cluster.
	// +kubebuilder:default=false
	// +optional
	// +k8s:immutable
	EnableChecksumValidation *bool `json:"enableChecksumValidation,omitempty"`

	// EnableAtomic4kWrites declares that the cluster's devices guarantee 4K
	// write atomicity even with a smaller logical block size, as AWS NVMe does
	// at 512 bytes, which lets checksum fallback mode run on them despite the
	// data plane's usual 4K minimum. It means nothing unless
	// EnableChecksumValidation is set, and it cannot change under a live
	// cluster.
	// +kubebuilder:default=false
	// +optional
	// +k8s:immutable
	EnableAtomic4kWrites *bool `json:"enableAtomic4kWrites,omitempty"`

	// DeviceClass is the class of backend storage every node in this cluster
	// hands over: NVMe devices named by PCI address, or logical block devices
	// named by path. A cluster is built out of one of them, because an
	// erasure-coding stripe placed across both is written and rebuilt at the
	// slower class's rate. It describes on-disk layout, so it cannot change
	// under a live cluster, and it defaults to NVMe because that is the only
	// class the backend accepted before 26.4.
	// +kubebuilder:validation:Enum=NVMe;LogicalBlock
	// +kubebuilder:default=NVMe
	// +k8s:immutable
	DeviceClass StorageClusterDeviceClass `json:"deviceClass,omitempty"`

	// KMS selects where the cluster stores volume encryption keys. Switching
	// providers on a live cluster is at least as unsupportable as changing one
	// provider's endpoint, which is why the whole block is immutable rather
	// than its members.
	// +optional
	KMS *KMSSpec `json:"kms,omitempty"`

	// WarningThreshold is the capacity level at which the cluster warns.
	// +optional
	WarningThreshold *CapacityThresholdSpec `json:"warningThreshold,omitempty"`

	// CriticalThreshold is the capacity level at which the cluster alarms.
	// +optional
	CriticalThreshold *CapacityThresholdSpec `json:"criticalThreshold,omitempty"`

	// MaxConcurrentWorkerRestarts caps how many Kubernetes workers the operator
	// may drain and restart at once. The effective value is the smaller of this
	// and status.maxFaultTolerance, published as
	// status.maxConcurrentWorkerRestarts so tooling reads one authoritative
	// number rather than recomputing it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentWorkerRestarts *int32 `json:"maxConcurrentWorkerRestarts,omitempty"`

	// Backup is the S3 location this cluster's backups live in, and it is both
	// the target copies are written to and the inventory the operator walks to
	// produce StorageBackup objects. Mutable: a cluster that has never had a
	// store gains one by acquiring the field, and every backup already in that
	// bucket becomes visible.
	// +optional
	Backup *BackupStoreSpec `json:"backup,omitempty"`

	// EnableDataRealignment turns on the post-migration data realignment. It is
	// a field of the spec rather than of the block it governs, because
	// volumeMigrationSettings.dataRealignment.enableDataRealignment says the
	// same word twice. There is no EnableVolumeMigration beside it: migration
	// cannot be turned off, since a drain, a rebalance, and a device
	// replacement are all performed by moving volumes.
	// +optional
	EnableDataRealignment *bool `json:"enableDataRealignment,omitempty"`

	// EnableVolumeAutoPlacement turns on automatic, latency-driven rebalancing.
	// +optional
	EnableVolumeAutoPlacement *bool `json:"enableVolumeAutoPlacement,omitempty"`

	// VolumeMigrationSettings controls how volume migration and the
	// post-migration realignment behave, not whether they happen. It is
	// separate from volumeAutoPlacement because realignment applies to every
	// volume move, whatever asked for it.
	// +optional
	VolumeMigrationSettings *VolumeMigrationSettings `json:"volumeMigrationSettings,omitempty"`

	// VolumeAutoPlacement configures automatic, latency-driven rebalancing.
	// +optional
	VolumeAutoPlacement *VolumeAutoPlacementSettings `json:"volumeAutoPlacement,omitempty"`
}

// StorageClusterStatus is the observed state of one backend cluster.
type StorageClusterStatus struct {
	// Phase is the operator's own view of this cluster.
	// +optional
	Phase StorageClusterPhase `json:"phase,omitempty"`

	// Step is the position of the creation machine, as the shared
	// statemachine.KubeSnapshot. The rule is what an Enum marker would do if a
	// marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Claiming','CheckingControlPlane','ResolvingConfig','Creating','Adopting','Persisting']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// UUID is the backend cluster UUID. Empty means the cluster has not been
	// created or adopted, and non-empty means steady state.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// ClusterName is the resolved backend name.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// NQN is the cluster subsystem qualified name.
	// +optional
	NQN string `json:"nqn,omitempty"`

	// ErasureCodingScheme is the active layout, rendered as ndcs, an x, and
	// npcs: a two-plus-one cluster reads 2x1.
	// +optional
	ErasureCodingScheme string `json:"erasureCodingScheme,omitempty"`

	// Status is the lifecycle the control plane reports, and its values are the
	// control plane's, which is why they are neither PascalCase nor constrained
	// by an Enum here.
	// +optional
	Status string `json:"status,omitempty"`

	// Configured records that initial setup completed.
	// +optional
	Configured bool `json:"configured,omitempty"`

	// Rebalancing is the control plane's report that a rebalance is in
	// progress, which is one of the two conditions that hold a node operation.
	// +optional
	Rebalancing *bool `json:"rebalancing,omitempty"`

	// MaxFaultTolerance is how many nodes may be simultaneously offline without
	// violating redundancy, as the control plane reports it.
	// +optional
	MaxFaultTolerance *int32 `json:"maxFaultTolerance,omitempty"`

	// MaxConcurrentWorkerRestarts is the effective limit, the smaller of
	// spec.maxConcurrentWorkerRestarts and maxFaultTolerance.
	// +optional
	MaxConcurrentWorkerRestarts *int32 `json:"maxConcurrentWorkerRestarts,omitempty"`

	// VolumeMoveGeneration is incremented by every migration reaching its
	// successful terminal phase and by nothing else, so it only grows.
	// +optional
	VolumeMoveGeneration *int64 `json:"volumeMoveGeneration,omitempty"`

	// RealignedGeneration is the generation the last successfully requested
	// realignment covers. A realignment is outstanding while
	// volumeMoveGeneration exceeds it. The value recorded is the one read
	// before the request was sent, which is what that realignment can actually
	// account for: a migration completing while the request is in flight raises
	// volumeMoveGeneration past it and correctly leaves another realignment
	// outstanding.
	// +optional
	RealignedGeneration *int64 `json:"realignedGeneration,omitempty"`

	// LastDataRealignmentAt is when a realignment was last requested, and it is
	// what the configured interval spaces requests against.
	// +optional
	LastDataRealignmentAt *metav1.Time `json:"lastDataRealignmentAt,omitempty"`

	// Tasks are the control plane's running and pending jobs, newest first and
	// capped at twenty. Completed and canceled tasks are not here: they leave
	// the list and become events, so the length tracks concurrency rather than
	// history.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Tasks []ClusterTask `json:"tasks,omitempty"`

	// ActiveOpsRef names the StorageClusterOps currently allowed to operate on
	// this cluster. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// RebalancingMetrics is written by the auto-rebalancer each evaluation
	// cycle.
	// +optional
	RebalancingMetrics *RebalancingMetrics `json:"rebalancingMetrics,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the cluster moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// v1alpha2 is the storage version in the manifests this repository ships. A
// fresh install stores this shape from the first write and converts nothing; an
// upgrade of an existing cluster applies the same CRD with storage held at
// v1alpha1 and flips it with the storage rewrite once the conversion webhook is
// serving.
//
// The short name is stc because sc is StorageClass's. Two kinds may declare the
// same short name and the RESTMapper resolves it by discovery order, so
// `kubectl get sc` reaches storageclasses.storage.k8s.io and never this kind,
// and the operator writes a StorageClass per pool, which puts both kinds in
// every cluster this runs in.
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=stc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="EC",type=string,JSONPath=".status.erasureCodingScheme"
// +kubebuilder:printcolumn:name="FTT",type=integer,JSONPath=".status.maxFaultTolerance",priority=1
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageCluster is one simplyblock backend cluster. It owns the storage nodes
// beneath it, the pools carved out of it, and the Kubernetes workload its nodes
// run as.
type StorageCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClusterSpec   `json:"spec,omitempty"`
	Status StorageClusterStatus `json:"status,omitempty"`
}

// Hub marks this version as the conversion hub for StorageCluster.
func (*StorageCluster) Hub() {}

// +kubebuilder:object:root=true

// StorageClusterList contains a list of StorageCluster.
type StorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageCluster{}, &StorageClusterList{})
}

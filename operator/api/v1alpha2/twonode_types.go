package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Two-node arbitration (sbcli docs/design/two-node-arbitration.md §9): a
// cluster of exactly two storage nodes on an edge site, with the central
// control plane as the witness that decides which node continues when the two
// lose each other.

// TaintStorageFenced is the taint the operator puts on the Kubernetes node of a
// storage node the arbiter fenced. Its value is the arbiter's epoch, its effect
// NoExecute, so KubeVirt and every other workload move to the surviving node.
const TaintStorageFenced = "storage.simplyblock.io/fenced"

// TaintOutOfService is Kubernetes' own taint for a node that is known to be
// shut down. The operator reads it as positive fencing evidence, except where its
// own StorageFenceRemediation set it (AnnoOutOfServiceBy): that one follows the
// arbiter's verdict and is not new evidence about the node.
const TaintOutOfService = "node.kubernetes.io/out-of-service"

// TaintOutOfServiceValue is the value the storage fence remediation gives the
// out-of-service taint, the one the Kubernetes documentation uses.
const TaintOutOfServiceValue = "nodeshutdown"

// AnnoOutOfServiceBy marks a Node whose out-of-service taint the storage fence
// remediation set, with OutOfServiceByStorageFence as its value, so that only
// that remediation removes it and the operator does not report it to the
// arbiter as fencing evidence.
const AnnoOutOfServiceBy = "storage.simplyblock.io/out-of-service-by"

// OutOfServiceByStorageFence is the AnnoOutOfServiceBy value of the storage
// fence remediation.
const OutOfServiceByStorageFence = "storage-fence-remediation"

// ConditionStorageFenced is the Node condition the operator sets on the
// Kubernetes node of a storage node whose host the control plane fenced: its
// host NQNs are revoked on the peer, so nothing on the node can write to the
// cluster any more (sbcli docs/design/two-node-kubernetes-fencing.md §3.4). A
// NodeHealthCheck remediates on it.
const ConditionStorageFenced = "StorageFenced"

// Reasons of ConditionStorageFenced.
const (
	// StorageFencedReasonWitness: the arbiter fenced the node's host.
	StorageFencedReasonWitness = "WitnessVerdict"
	// StorageFencedReasonReleased: no fence in force for the node.
	StorageFencedReasonReleased = "NotFenced"
)

// ArbitrationSpec tunes the control plane's arbitration for a cluster of
// exactly two storage nodes. All fields are optional; omitted values take the
// arbiter's defaults.
// +kubebuilder:validation:XValidation:rule="!has(self.holdMs) || !has(self.leaseTtlMs) || self.leaseTtlMs < self.holdMs",message="leaseTtlMs must be shorter than holdMs"
type ArbitrationSpec struct {
	// HoldMs is how long a node holds writes after losing its peer before it
	// acts on its own. It must stay below the hosts' NVMe keep-alive timeout
	// (5000 ms), or clients fail I/O before the verdict arrives.
	// +optional
	// +kubebuilder:validation:Minimum=500
	// +kubebuilder:validation:Maximum=4500
	HoldMs *int32 `json:"holdMs,omitempty"`

	// LeaseTtlMs is how long a lease from the arbiter stays valid. It only
	// matters while a node is holding; a healthy pair is never fenced by a
	// late renewal.
	// +optional
	// +kubebuilder:validation:Minimum=500
	// +kubebuilder:validation:Maximum=10000
	LeaseTtlMs *int32 `json:"leaseTtlMs,omitempty"`

	// HostFencing lets Kubernetes follow the arbiter's fence of a node whose
	// state is unknown. The control plane revokes the node's NVMe host
	// identities on its peer, the operator sets the StorageFenced condition on
	// its Kubernetes node, and a NodeHealthCheck with a
	// StorageFenceRemediationTemplate applies the out-of-service taint, so its
	// pods and VMs restart on the peer within seconds. A partition then has a
	// winner instead of stopping I/O on both nodes. Requires the NodeHealthCheck
	// operator.
	// +optional
	HostFencing *bool `json:"hostFencing,omitempty"`
}

// HostFencingEnabled reports whether the cluster asks for host fencing: a
// Dual cluster with arbitration and spec.arbitration.hostFencing on.
func (c *StorageCluster) HostFencingEnabled() bool {
	return c.Spec.ClusterMode == ClusterModeDual &&
		c.Spec.EnableArbitration != nil && *c.Spec.EnableArbitration &&
		c.Spec.Arbitration != nil && c.Spec.Arbitration.HostFencing != nil && *c.Spec.Arbitration.HostFencing
}

// HostFenceStatus is one host fence of the control plane, as the operator last
// read it.
type HostFenceStatus struct {
	// Node is the Kubernetes node whose host is fenced.
	Node string `json:"node"`

	// State is the fence's state: revoking, revoked, releasable, restoring or
	// unavailable (no host NQN known for the node).
	State string `json:"state"`

	// Winner is the Kubernetes node the fence was applied on.
	// +optional
	Winner string `json:"winner,omitempty"`

	// Epoch is the arbiter's epoch of the verdict that fenced the node.
	// +optional
	Epoch int64 `json:"epoch,omitempty"`
}

// ArbitrationStatus is the operator's view of the arbiter's record for the
// cluster, as last read from the control plane.
type ArbitrationStatus struct {
	// State is the arbiter's state: steady, deciding, partitioned, degraded or
	// healing.
	// +optional
	State string `json:"state,omitempty"`

	// Epoch is the arbiter's current epoch.
	// +optional
	Epoch int64 `json:"epoch,omitempty"`

	// FencedNodes are the Kubernetes nodes that carry the storage-fenced taint
	// on the arbiter's request.
	// +optional
	FencedNodes []string `json:"fencedNodes,omitempty"`

	// HostFences are the control plane's host fences (spec.arbitration.hostFencing).
	// +optional
	// +listType=map
	// +listMapKey=node
	HostFences []HostFenceStatus `json:"hostFences,omitempty"`

	// ObservedAt is when the operator last read the arbiter's record.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// Message explains why the record could not be read or applied.
	// +optional
	Message string `json:"message,omitempty"`
}

// NodeRemediationStatus reports what the Kubernetes side knows about a storage
// node's host, for the arbiter to use as positive fencing evidence.
type NodeRemediationStatus struct {
	// OutOfService is true when the Kubernetes node carries the
	// node.kubernetes.io/out-of-service taint.
	// +optional
	OutOfService bool `json:"outOfService,omitempty"`

	// NodeNotReady is true when the Kubernetes node's Ready condition is not
	// True (Node Health Check and remediation act on the same signal).
	// +optional
	NodeNotReady bool `json:"nodeNotReady,omitempty"`

	// StorageFencedEpoch is the epoch in the storage-fenced taint the operator
	// set on the node, or zero when the node is not fenced.
	// +optional
	StorageFencedEpoch int64 `json:"storageFencedEpoch,omitempty"`

	// ReportedFenced is the fencing evidence the control plane last accepted
	// for this node (PUT .../arbitration/remediation): true while the node is
	// out of service. Unset until the first report succeeds; a failed report
	// leaves it unchanged, so the next reconcile retries.
	// +optional
	ReportedFenced *bool `json:"reportedFenced,omitempty"`

	// ReportedHostNQNs are the NVMe host NQNs of the Kubernetes node the
	// control plane last accepted for this storage node: the kernel default the
	// CSI node plugin publishes, and the simplyblock form derived from the node's
	// UID. A host fence revokes all of them on the peer.
	// +optional
	ReportedHostNQNs []string `json:"reportedHostNQNs,omitempty"`

	// ObservedAt is when the operator last looked at the node.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

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
// shut down. The operator only reads it, as positive fencing evidence; it never
// sets it (that is the job of OpenShift's remediation or an administrator).
const TaintOutOfService = "node.kubernetes.io/out-of-service"

// TwoNodeSpec configures arbitration for a cluster of exactly two storage
// nodes. All fields are optional; omitted values take the arbiter's defaults.
// +kubebuilder:validation:XValidation:rule="!has(self.holdMs) || !has(self.leaseTtlMs) || self.leaseTtlMs < self.holdMs",message="leaseTtlMs must be shorter than holdMs"
type TwoNodeSpec struct {
	// Arbitration turns the control plane's two-node arbitration on for this
	// cluster: nodes hold writes when they lose their peer, and only a node the
	// arbiter grants may continue alone. Off, the cluster keeps today's
	// behaviour (dual-node tolerance without arbitration).
	// +optional
	Arbitration bool `json:"arbitration,omitempty"`

	// PreferredNode is the Kubernetes node whose storage node continues alone
	// when neither the control plane nor the peer is reachable. Empty leaves the
	// choice to the arbiter.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PreferredNode string `json:"preferredNode,omitempty"`

	// HoldMs is how long a node holds writes after losing its peer before it
	// acts on its own. It must stay below the hosts' NVMe keep-alive timeout
	// (5000 ms), or clients fail IO before the verdict arrives.
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

	// PreferredNode is the Kubernetes node the arbiter treats as preferred.
	// +optional
	PreferredNode string `json:"preferredNode,omitempty"`

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

	// ObservedAt is when the operator last looked at the node.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

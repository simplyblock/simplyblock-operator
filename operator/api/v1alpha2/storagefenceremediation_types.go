// StorageFenceRemediation and its template: the NodeHealthCheck external
// remediation of a storage-fenced node on a two-node cluster.
//
// NodeHealthCheck (medik8s) creates a StorageFenceRemediation, named after the
// node, from the template a NodeHealthCheck names once the node's
// StorageFenced condition holds. The remediation skips power fencing: the
// control plane has already revoked the node's NVMe host identities on its
// peer, so the node cannot write to the cluster, and the remediation applies
// node.kubernetes.io/out-of-service directly (non-graceful node shutdown). Pods
// are force-deleted, VolumeAttachments force-detached, and the workloads restart
// on the peer (sbcli docs/design/two-node-kubernetes-fencing.md §1).

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageFenceRemediationSpec is what one remediation does.
type StorageFenceRemediationSpec struct {
	// AllowForeignVolumes lets the remediation apply the out-of-service taint
	// although a volume of another CSI driver is attached to the node. The
	// taint force-detaches every volume of the node, and only simplyblock
	// volumes are fenced, so by default such a node is left to an administrator.
	// +optional
	AllowForeignVolumes bool `json:"allowForeignVolumes,omitempty"`
}

// StorageFenceRemediationPhase is where a remediation stands.
// +kubebuilder:validation:Enum=Pending;Blocked;Tainted
type StorageFenceRemediationPhase string

const (
	// StorageFenceRemediationPending: the node is not (yet) confirmed fenced by
	// the control plane; nothing applied.
	StorageFenceRemediationPending StorageFenceRemediationPhase = "Pending"
	// StorageFenceRemediationBlocked: the node is fenced, but a check refuses the
	// taint (a foreign volume is attached); status.message says which.
	StorageFenceRemediationBlocked StorageFenceRemediationPhase = "Blocked"
	// StorageFenceRemediationTainted: the out-of-service taint is on the node.
	StorageFenceRemediationTainted StorageFenceRemediationPhase = "Tainted"
)

// Condition types NodeHealthCheck reads on an external remediation.
const (
	// RemediationConditionProcessing is True while the remediation works.
	RemediationConditionProcessing = "Processing"
	// RemediationConditionSucceeded is True once the node is remediated.
	RemediationConditionSucceeded = "Succeeded"
)

// StorageFenceRemediationStatus is the remediation's progress.
type StorageFenceRemediationStatus struct {
	// Phase is where the remediation stands.
	// +optional
	Phase StorageFenceRemediationPhase `json:"phase,omitempty"`

	// Node is the Kubernetes node remediated.
	// +optional
	Node string `json:"node,omitempty"`

	// Epoch is the arbiter's epoch of the host fence the taint was applied for.
	// +optional
	Epoch int64 `json:"epoch,omitempty"`

	// TaintedAt is when the out-of-service taint was applied.
	// +optional
	TaintedAt *metav1.Time `json:"taintedAt,omitempty"`

	// Message explains a Pending or Blocked phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions are Processing and Succeeded, which NodeHealthCheck reads.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sfr
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".status.node"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=".status.epoch"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageFenceRemediation applies the out-of-service taint to a node whose
// host the control plane fenced. NodeHealthCheck creates and deletes it.
type StorageFenceRemediation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageFenceRemediationSpec   `json:"spec,omitempty"`
	Status StorageFenceRemediationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageFenceRemediationList is a list of StorageFenceRemediation.
type StorageFenceRemediationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageFenceRemediation `json:"items"`
}

// StorageFenceRemediationTemplateResource is the remediation NodeHealthCheck
// creates from a template.
type StorageFenceRemediationTemplateResource struct {
	Spec StorageFenceRemediationSpec `json:"spec"`
}

// StorageFenceRemediationTemplateSpec holds the template, in the shape every
// NodeHealthCheck external remediation template has.
type StorageFenceRemediationTemplateSpec struct {
	Template StorageFenceRemediationTemplateResource `json:"template"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=sfrt

// StorageFenceRemediationTemplate is what a NodeHealthCheck names as its
// remediationTemplate to remediate storage-fenced nodes.
type StorageFenceRemediationTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec StorageFenceRemediationTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// StorageFenceRemediationTemplateList is a list of StorageFenceRemediationTemplate.
type StorageFenceRemediationTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageFenceRemediationTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageFenceRemediation{}, &StorageFenceRemediationList{},
		&StorageFenceRemediationTemplate{}, &StorageFenceRemediationTemplateList{})
}

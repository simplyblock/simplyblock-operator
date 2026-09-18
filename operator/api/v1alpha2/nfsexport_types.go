// NFSExport is one pNFS export: the RWX volume it serves, the MDS host serving
// it, and the address clients mount. It is the authoritative record of the
// binding between a volume and the single host that may have its filesystem
// mounted, which is the invariant that keeps two hosts from mounting one XFS.
//
// The record is a CRD rather than a control-plane object because every consumer
// is in-cluster: the CSI controller creates it while provisioning, csi-node
// reads it while staging, and the operator rewrites it while draining or failing
// over a host. A CRD gives those consumers a watch, and single-writer semantics
// on the bound host come free from resourceVersion. The control plane has no NFS
// concept at all, so putting the record there would mean inventing a backend
// domain object for something that only exists inside Kubernetes.
//
// The kind is declared in v1alpha2 rather than v1alpha1 because it is new:
// nothing ever shipped a v1alpha1 spelling, so there is no older representation
// to convert from. That is why this file carries no conversion and the type
// implements no hub interface. The design document names v1alpha1 only because
// it predates the group's move.
//
// The type follows design-pnfs-rwx.md section 7.1. Where it departs, the reason
// is a convention the design predates, and each departure is commented at the
// field.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NFSExportPhase is the lifecycle position of an export.
// +kubebuilder:validation:Enum=Pending;Assembling;Ready;Degraded;Deleting
type NFSExportPhase string

const (
	// NFSExportPhasePending is an export with no MDS host bound yet. A newly
	// created record starts here, and the reconciler leaves it here while no
	// eligible host exists, which is a wait rather than a failure.
	NFSExportPhasePending NFSExportPhase = "Pending"
	// NFSExportPhaseAssembling is an export whose bound host is building it:
	// attaching the namespace, making or finding the filesystem, mounting, and
	// publishing the export.
	NFSExportPhaseAssembling NFSExportPhase = "Assembling"
	// NFSExportPhaseReady is an export a client can mount.
	NFSExportPhaseReady NFSExportPhase = "Ready"
	// NFSExportPhaseDegraded is an export that exists but cannot serve its
	// purpose, and that the operator will not act further on without help. A
	// failover whose fence could not be confirmed lands here deliberately: a
	// stalled export is recoverable and a double-mounted one is not.
	NFSExportPhaseDegraded NFSExportPhase = "Degraded"
	// NFSExportPhaseDeleting is an export being torn down, with the finalizer
	// still held.
	NFSExportPhaseDeleting NFSExportPhase = "Deleting"
)

// Condition types an NFSExport reports. A phase says where the export is; these
// say why, which a phase alone cannot express.
const (
	// NFSExportConditionAssembled is whether the filesystem is mounted on the
	// bound host.
	NFSExportConditionAssembled = "Assembled"
	// NFSExportConditionExported is whether the mount is published to clients.
	NFSExportConditionExported = "Exported"
)

// NFSExportFinalizer keeps the record alive until the export it describes is
// gone. The record is the only description of an external mount, an
// /etc/exports entry, a Service, and an attached namespace, so a direct delete
// without it orphans every one of them with nothing left to name them.
const NFSExportFinalizer = "storage.simplyblock.io/nfsexport"

// NFSExportClientPolicyMode is how the effective client set is derived.
// +kubebuilder:validation:Enum=NodeScoped;Subnet;Open
type NFSExportClientPolicyMode string

const (
	// NFSExportClientPolicyModeNodeScoped restricts the export to the addresses
	// of nodes currently running a pod that consumes the volume.
	NFSExportClientPolicyModeNodeScoped NFSExportClientPolicyMode = "NodeScoped"
	// NFSExportClientPolicyModeSubnet restricts the export to the subnets named
	// in Subnets.
	NFSExportClientPolicyModeSubnet NFSExportClientPolicyMode = "Subnet"
	// NFSExportClientPolicyModeOpen exports to every host that can reach the
	// MDS. It is what the proof of concept did and is not a default.
	NFSExportClientPolicyModeOpen NFSExportClientPolicyMode = "Open"
)

// NFSExportClientPolicy constrains which clients may mount, as policy rather
// than as a membership list. The effective set is observed in status, because
// it changes as pods are scheduled and must not bump generation.
type NFSExportClientPolicy struct {
	// Mode is how the effective client set is derived.
	// +kubebuilder:default=NodeScoped
	// +optional
	Mode NFSExportClientPolicyMode `json:"mode,omitempty"`

	// Subnets are the CIDRs allowed when Mode is Subnet. Ignored otherwise.
	// +optional
	Subnets []string `json:"subnets,omitempty"`
}

// NFSExportSpec is the desired state: which volume is exported, and under what
// policy. The bound MDS host is deliberately not here. It is an observed
// binding the operator owns, so it lives in status where a user edit cannot
// race the failover machine.
type NFSExportSpec struct {
	// VolumeRef is the CSI volume handle this export serves, in the four-part
	// pNFS form nfs:{clusterID}:{poolID}:{exportUUID}.
	//
	// It names no Kubernetes object, so it is validated by pattern rather than
	// resolved by a webhook.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^nfs:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:[^:]+:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +k8s:immutable
	VolumeRef string `json:"volumeRef"`

	// ExportPath is the server-side mount point. It carries namespace and UID
	// information because a PVC name is unique only within a namespace, and two
	// same-named PVCs must not collide on one MDS host.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/mnt/[A-Za-z0-9._-]+$`
	// +k8s:immutable
	ExportPath string `json:"exportPath"`

	// FSID is the NFS fsid this export is published under. It is the export
	// UUID: exports(5) accepts a UUID of 32 hex digits with arbitrary
	// punctuation, which makes the value unique cluster-wide and stable for the
	// export's life by construction, with no allocator to run and no collision
	// to handle. Stability is what lets a re-materialized export reproduce the
	// file handles clients already hold.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	FSID string `json:"fsid"`

	// ClientPolicy constrains which clients may mount.
	// +optional
	ClientPolicy *NFSExportClientPolicy `json:"clientPolicy,omitempty"`
}

// NFSExportStatus is what the export currently is. Everything the operator
// decides lives here.
type NFSExportStatus struct {
	// Phase is the export's lifecycle position.
	// +optional
	Phase NFSExportPhase `json:"phase,omitempty"`

	// PhaseDeadline is when the current phase must be given up on. The phase
	// machine carries a per-state bound, and the bound has to outlive the
	// process the way the phase itself does: an operator restarted mid-assembly
	// would otherwise grant the phase a fresh deadline on every pass and never
	// time out.
	// +optional
	PhaseDeadline *metav1.Time `json:"phaseDeadline,omitempty"`

	// StorageNodeRef names the StorageNode currently acting as MDS. It is
	// operator-owned and is the serialization point for the one-MDS-per-export
	// invariant: no second host is a candidate until this field is rewritten.
	// +optional
	StorageNodeRef string `json:"storageNodeRef,omitempty"`

	// MDSNodeIP is the node address currently behind that Service, recorded for
	// diagnosis rather than for clients to use.
	// +optional
	MDSNodeIP string `json:"mdsNodeIP,omitempty"`

	// LVolID identifies the backing logical volume.
	// +optional
	LVolID string `json:"lvolID,omitempty"`

	// NGUID is the backing namespace's globally unique id. It is what a client
	// resolves the local block device by, and what the MDS names in the layout,
	// so it is recorded rather than re-derived at stage time.
	// +optional
	NGUID string `json:"nguid,omitempty"`

	// AllowedClients is the effective client set written into the export,
	// derived from ClientPolicy and current pod placement.
	// +optional
	AllowedClients []string `json:"allowedClients,omitempty"`

	// Conditions carry why an export is not Ready. The types are Assembled and
	// Exported, which are the two steps assembly has; a condition type is added
	// here when something sets it, not before.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Message is a human-readable note on the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// Without it a stale status and a disagreeing one are the same observation,
	// so a spec edit cannot be waited on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=nfsexp
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".spec.volumeRef"
// +kubebuilder:printcolumn:name="MDS",type=string,JSONPath=".status.storageNodeRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// NFSExport is one pNFS export: a volume, the MDS host serving it, and the
// address clients mount. The operator owns the binding and the failover.
type NFSExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NFSExportSpec   `json:"spec,omitempty"`
	Status NFSExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NFSExportList contains a list of NFSExport.
type NFSExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NFSExport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NFSExport{}, &NFSExportList{})
}

// NFSExport is one pNFS export: the volume it serves, the host serving it, and
// the address clients mount. Its job is to record the binding between a volume
// and the single host that may mount its filesystem.
//
// v1alpha2 because the kind is new, so there is nothing to convert from.
// Follows design-pnfs-rwx.md §7.1.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NFSExportPhase is the lifecycle position of an export.
// +kubebuilder:validation:Enum=Pending;Assembling;Ready;Degraded
type NFSExportPhase string

const (
	// Pending is an export with no host bound yet: a wait, not a failure.
	NFSExportPhasePending NFSExportPhase = "Pending"
	// Assembling is the bound host attaching, formatting, mounting, publishing.
	NFSExportPhaseAssembling NFSExportPhase = "Assembling"
	// Ready is an export a client can mount.
	NFSExportPhaseReady NFSExportPhase = "Ready"
	// Degraded is an export the operator will not act further on. Reached by
	// declining to act: a stalled export is recoverable, a double-mounted one
	// is not.
	NFSExportPhaseDegraded NFSExportPhase = "Degraded"
)

// NFSExportFinalizer keeps the record alive until the export is gone. The
// record is the only description of a mount, an exports entry, and an attached
// namespace, so deleting it first orphans all three.
const NFSExportFinalizer = "storage.simplyblock.io/nfsexport"

// NFSExportSpec is the desired state. The bound host is deliberately not here:
// it is an observed binding the operator owns, so it lives in status where a
// user edit cannot race it.
type NFSExportSpec struct {
	// VolumeRef is the CSI volume handle of the volume this export serves, in
	// the ordinary {clusterID}:{poolID}:{lvolID} form. It names no Kubernetes
	// object, so it is validated by pattern rather than resolved by a webhook.
	//
	// It is the whole identity of the export. The backing namespace UUID and
	// the NFS fsid are both the volume's own id, so they are read from here
	// rather than stored beside it, where they could disagree with it.
	//
	// Deliberately not a form of its own. A pNFS volume is an lvol with an
	// export in front of it, so its claim keeps the lvol's handle and every
	// other CSI call -- snapshot, clone, expand -- addresses it without having
	// to know an export is there.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:[^:]+:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +k8s:immutable
	VolumeRef string `json:"volumeRef"`

	// ExportPath is the server-side mount point. It carries the namespace,
	// because a PVC name is unique only within one.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/var/lib/simplyblock/exports/[A-Za-z0-9._-]+$`
	// +k8s:immutable
	ExportPath string `json:"exportPath"`

	// Encrypted says the control plane stacks a crypto bdev under this volume,
	// which makes an unwritten block read as pseudo-random plaintext rather
	// than as zeros. Assembly needs it: without it an empty encrypted volume
	// is mistaken for an occupied one, never formatted, and never mounts.
	//
	// No omitempty: the field is immutable once set (below), and the CRD
	// enforces that by requiring it stay present once it is. The CSI
	// controller's unstructured Create always writes it explicitly, so an
	// unencrypted export's record already carries `encrypted: false` in
	// etcd; a typed client's own writes have to carry it too; omitempty
	// drops a false value from the JSON entirely, which reads to the CRD as
	// the field being removed and every subsequent write from this type --
	// starting with the reconciler's own finalizer add -- is then refused,
	// permanently, before the export can even begin assembling.
	// +optional
	// +k8s:immutable
	Encrypted bool `json:"encrypted"`

	// SizeBytes is the capacity the backing volume was last grown to.
	//
	// It is not a request: the control plane has already resized the volume by
	// the time this is written. It is here so that growing one bumps the
	// record's generation, which is what tells the operator to re-assemble and
	// run xfs_growfs on the host -- the one machine that can, and the one no
	// CSI call ever reaches.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// NFSExportStatus is what the export currently is.
type NFSExportStatus struct {
	// Phase is the export's lifecycle position.
	// +optional
	Phase NFSExportPhase `json:"phase,omitempty"`

	// PhaseDeadline is when the current phase must be given up on. Persisted
	// because an operator restarted mid-assembly would otherwise grant a fresh
	// deadline on every pass and never time out.
	// +optional
	PhaseDeadline *metav1.Time `json:"phaseDeadline,omitempty"`

	// MDSNodeName is the Kubernetes node serving this export, which is the one
	// running the csi-node pod that assembled it. It is not a StorageNode: an
	// MDS reaches the volume over NVMe-oF exactly as a client does, so it need
	// not hold any storage itself.
	//
	// The serialization point for one-MDS-per-export: no second host is a
	// candidate until this field is rewritten.
	// +optional
	MDSNodeName string `json:"mdsNodeName,omitempty"`

	// MDSNodeIP is the bound MDS host's own address. Not what a client mounts:
	// it is what the export's Service's EndpointSlice points at, so a client's
	// mount address (ServiceAddress) does not have to change when this does.
	// +optional
	MDSNodeIP string `json:"mdsNodeIP,omitempty"`

	// ServiceAddress is the ClusterIP of the Service fronting this export,
	// which is the address a client mounts. It outlives any one MDSNodeIP: the
	// operator repoints the Service's EndpointSlice at whichever host is bound
	// rather than changing this value, so a client's mount survives the export
	// moving to a different host (design-pnfs-rwx.md §13.3).
	// +optional
	ServiceAddress string `json:"serviceAddress,omitempty"`

	// AllowedClients is what goes into the exports(5) entry: the internal
	// address of every node that could run a pod using this volume.
	// +optional
	AllowedClients []string `json:"allowedClients,omitempty"`

	// Message is a human-readable note on the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=nfsexp
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".spec.volumeRef"
// +kubebuilder:printcolumn:name="MDS",type=string,JSONPath=".status.mdsNodeName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// NFSExport is one pNFS export: a volume, the host serving it, and the address
// clients mount.
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

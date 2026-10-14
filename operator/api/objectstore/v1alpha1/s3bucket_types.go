// s3bucket_types.go defines the S3Bucket CRD: the desired existence of one S3
// bucket, its backing PersistentVolumeClaim, its pNFS export, and the
// per-node mount deliveries that make its gateway's pods see it
// (design-bucket-provisioning.md §4, Appendix A).

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// S3BucketFinalizer keeps the record alive until its PVC is released and its
// per-node mount deliveries are withdrawn. Without it a direct delete orphans
// a mounted export and a volume nothing describes any longer.
const S3BucketFinalizer = "objectstore.simplyblock.io/s3bucket"

// S3BucketPhase is the lifecycle phase of a bucket's provisioning.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Deleting
type S3BucketPhase string

const (
	S3BucketPhasePending      S3BucketPhase = "Pending"
	S3BucketPhaseProvisioning S3BucketPhase = "Provisioning"
	S3BucketPhaseReady        S3BucketPhase = "Ready"
	S3BucketPhaseDegraded     S3BucketPhase = "Degraded"
	S3BucketPhaseDeleting     S3BucketPhase = "Deleting"
)

// S3BucketSpec is the desired state of one S3 bucket's backing volume.
type S3BucketSpec struct {
	// ObjectStoreRef names the ObjectStore whose gateway Deployment serves
	// this bucket. Immutable: a bucket does not move between gateways.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ObjectStoreRef string `json:"objectStoreRef"`

	// BucketName is the S3 bucket name as presented to clients, validated
	// against the S3 bucket-naming grammar rather than the Kubernetes
	// object-name grammar, since the two disagree on underscores and case.
	// Immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`
	// +k8s:immutable
	BucketName string `json:"bucketName"`

	// CapacityBytes is the backing volume's requested size. An increase
	// reconciles to a PVC expansion. A decrease is rejected, the same
	// restriction the PVC it wraps already carries.
	// +kubebuilder:validation:Required
	CapacityBytes int64 `json:"capacityBytes"`

	// StorageClassName names the StorageClass the backing PVC is created
	// against. It must select the pnfs fstype, or the bucket never leaves
	// Pending: versitygw's --bucketlinks mapping depends on every gateway
	// pod sharing this volume RWX. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	StorageClassName string `json:"storageClassName"`
}

// S3BucketStatus is what the bucket's provisioning currently is.
type S3BucketStatus struct {
	// Phase is the bucket's lifecycle position.
	// +optional
	Phase S3BucketPhase `json:"phase,omitempty"`

	// PersistentVolumeClaimName is the PVC this reconciler created for the
	// bucket's backing volume, recorded rather than recomputed so a later
	// change to the naming scheme cannot orphan an existing bucket's volume.
	// +optional
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`

	// NFSExportName is the NFSExport CR the CSI controller created for the
	// PVC, found by matching its spec.volumeRef against the bound
	// PersistentVolume's CSI volume handle. Empty until the PVC is Bound.
	// +optional
	NFSExportName string `json:"nfsExportName,omitempty"`

	// MountDeliveredNodes lists the nodes whose CSI node plugin has
	// confirmed this bucket staged and published, mirrored from the
	// ObjectStore's gateway Deployment's pod placement. Observed, not
	// desired.
	// +optional
	MountDeliveredNodes []string `json:"mountDeliveredNodes,omitempty"`

	// Conditions carries why a bucket is Degraded, which a phase alone
	// cannot express. Expected types: VolumeBound, ExportReady,
	// MountDelivered.
	// +optional
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=s3bkt
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=".spec.bucketName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=".status.persistentVolumeClaimName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// S3Bucket is the desired existence of one S3 bucket: its backing volume,
// its pNFS export, and the per-node mount deliveries that make its gateway's
// pods see it.
type S3Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   S3BucketSpec   `json:"spec,omitempty"`
	Status S3BucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type S3BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []S3Bucket `json:"items"`
}

func init() {
	SchemeBuilder.Register(&S3Bucket{}, &S3BucketList{})
}

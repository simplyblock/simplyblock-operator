// objectstore_types.go defines the ObjectStore CRD: the desired existence of
// one S3 gateway, its Deployment, its Service, and that Service's serving
// certificate (design-objectstore.md §4, Appendix A).
//
// Types and generated code only, matching design-objectstore.md's appendix
// field for field (plus PhaseDeadline, added for the same reason
// S3BucketStatus carries one: §6's deadline has to survive an operator
// restart, and the appendix did not have a field for it). The reconciler
// that turns this into a Deployment, a Service, and a certificate is
// objectstore_controller.go.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectStorePhase is the lifecycle phase of a gateway's provisioning.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded
type ObjectStorePhase string

const (
	ObjectStorePhasePending      ObjectStorePhase = "Pending"
	ObjectStorePhaseProvisioning ObjectStorePhase = "Provisioning"
	ObjectStorePhaseReady        ObjectStorePhase = "Ready"
	ObjectStorePhaseDegraded     ObjectStorePhase = "Degraded"
)

// ObjectStoreSpec is the desired state of one S3 gateway's workload.
type ObjectStoreSpec struct {
	// Image is the gateway image: embedgw plus the simplyblock POSIX backend.
	// Required: unlike the CSI driver and the operator, this repository and
	// the gateway's are not released together, so there is no sensible
	// default.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?$`
	Image string `json:"image"`

	// ImagePullPolicy controls when that image is pulled. Defaults to Always:
	// the default tag is a moving one, and IfNotPresent against a tag that
	// moved leaves workers at different versions invisibly.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// Port is the gateway's listening port, used as both the container port
	// and the Service's port.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Replicas is the gateway Deployment's replica count.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// NodeSelector restricts which workers run gateway pods. Empty means
	// every schedulable worker: ordinary Deployment scheduling, no
	// gateway-specific node-pool concept.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let gateway pods schedule onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Resources are the gateway container's requests and limits. Unset
	// enforces no limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// EnvFrom is injected into the gateway container's environment
	// unmodified: the gateway's own credentials and Vault-backed IAM
	// configuration. This operator reads none of it.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// TLSProvider selects how the Service's serving certificate is issued.
	// Always issued: an S3 gateway is assumed to always serve over TLS.
	// +kubebuilder:validation:Enum=OpenShift;cert-manager
	// +kubebuilder:default=OpenShift
	// +optional
	TLSProvider string `json:"tlsProvider,omitempty"`
}

// ObjectStoreStatus is what the gateway's provisioning currently is.
type ObjectStoreStatus struct {
	// Phase is the operator's own view of the deployment.
	// +optional
	Phase ObjectStorePhase `json:"phase,omitempty"`

	// ReadyReplicas mirrors Deployment.Status.ReadyReplicas, so a reader
	// does not have to look at the owned Deployment to answer "how many
	// gateway pods are actually serving."
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Conditions carries why an ObjectStore is Degraded, which a phase
	// alone cannot express. Expected types: DeploymentAvailable,
	// CertificateReady.
	// +optional
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// PhaseDeadline is when the current phase gives up waiting and degrades,
	// carried in status rather than only in memory so a restarted operator
	// resumes the same deadline instead of granting a fresh one (§6, a design
	// gap the same field closed for S3BucketStatus: see
	// design-bucket-provisioning.md's own appendix note).
	// +optional
	PhaseDeadline *metav1.Time `json:"phaseDeadline,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=s3store
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ObjectStore is the desired existence of one S3 gateway: its Deployment,
// its Service, and that Service's serving certificate. Every gateway pod
// carries the label objectstore.simplyblock.io/instance=<name>, which is how
// S3BucketReconciler finds them without reading this object.
type ObjectStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStoreSpec   `json:"spec,omitempty"`
	Status ObjectStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ObjectStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectStore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ObjectStore{}, &ObjectStoreList{})
}

// The CSI driver's deployment as a Kubernetes resource: the node plugin, the
// controller plugin, their RBAC, the configuration both mount, and the core
// CSIDriver registration they produce.
//
// The kind exists because the driver's version was a property of a Helm release
// while the control plane's was something else entirely, so the two moved on two
// cadences that nothing compared. A driver ahead of the control plane it calls
// fails in the data path, at attach time, on a workload's pod. Both versions are
// fields on one object here, and the operator compares them.
//
// A Kubernetes cluster holds one of these. On every cluster that ran simplyblock
// before the kind existed, the first one adopts the deployment the chart
// installed rather than creating one.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md,
// whose Appendix A is this file.

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SimplyblockDriverPhase is where the operator has got to with the CSI driver.
// Installing covers the applies of §4.1, and the three values after it are
// decided by what the node plugins and the controller plugin report (§4.2).
// +kubebuilder:validation:Enum=Installing;Ready;Degraded;Unavailable
type SimplyblockDriverPhase string

const (
	// SimplyblockDriverPhaseInstalling is a deployment whose objects are not all
	// applied yet.
	SimplyblockDriverPhaseInstalling SimplyblockDriverPhase = "Installing"
	// SimplyblockDriverPhaseReady is every plugin pod serving.
	SimplyblockDriverPhaseReady SimplyblockDriverPhase = "Ready"
	// SimplyblockDriverPhaseDegraded is a node plugin restarting while the
	// controller plugin still provisions, which strands one worker's volumes
	// rather than the namespace's.
	SimplyblockDriverPhaseDegraded SimplyblockDriverPhase = "Degraded"
	// SimplyblockDriverPhaseUnavailable is a controller plugin that is not
	// running, which is when provisioning stops.
	SimplyblockDriverPhaseUnavailable SimplyblockDriverPhase = "Unavailable"
)

// SidecarImages overrides the CSI sidecar images this deployment runs. An unset
// field takes the version this operator release ships, which is the combination
// it was tested against, and the fields exist so that a pin a Helm release made
// survives the adoption of that release's deployment.
//
// Every field carries the registry pattern Image carries. The node plugin is
// privileged and mounts /dev, /sys, and the kubelet's plugin directory from the
// host, so a sidecar beside it runs with the same access.
type SidecarImages struct {
	// Provisioner is csi-provisioner, on the controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Provisioner string `json:"provisioner,omitempty"`

	// Attacher is csi-attacher, on the controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Attacher string `json:"attacher,omitempty"`

	// Resizer is csi-resizer, on the controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Resizer string `json:"resizer,omitempty"`

	// Snapshotter is csi-snapshotter, on the controller plugin. It is this
	// driver's sidecar and not the cluster's snapshot-controller, whose image
	// is not overridable here because that component belongs to the cluster.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Snapshotter string `json:"snapshotter,omitempty"`

	// HealthMonitor is csi-external-health-monitor-controller, on the
	// controller plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	HealthMonitor string `json:"healthMonitor,omitempty"`

	// NodeDriverRegistrar is node-driver-registrar, on the node plugin.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	NodeDriverRegistrar string `json:"nodeDriverRegistrar,omitempty"`
}

// SimplyblockDriverSpec is the CSI driver deployment: the node plugin, the
// controller plugin, their RBAC, and the CSIDriver registration they produce.
type SimplyblockDriverSpec struct {
	// Image is the CSI driver image, used by both plugins. Unset takes the
	// operator's own registry and tag with the CSI driver's repository, so a
	// deployment that states nothing runs the driver belonging to the operator
	// reconciling it, which is the pairing the release was tested as.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy controls when that image is pulled.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// DriverName is the CSI driver name a StorageClass provisions with. Every
	// PersistentVolume the driver created records it in spec.csi.driver and
	// every VolumeAttachment records it too, so changing it orphans every volume
	// in the namespace rather than renaming anything.
	// +kubebuilder:default=csi.simplyblock.io
	// +optional
	// +k8s:immutable
	DriverName string `json:"driverName,omitempty"`

	// ControllerReplicas is the number of controller-plugin instances.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	ControllerReplicas *int32 `json:"controllerReplicas,omitempty"`

	// NodeSelector restricts which workers run the node plugin. Empty means
	// every schedulable worker, which is the usual case: a node that cannot
	// attach a volume cannot run a workload that needs one.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are applied to the node plugin, which usually needs to run
	// where workloads run rather than where the operator does.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// ControllerNodeSelector and ControllerTolerations place the controller
	// plugin. The unprefixed pair above is the node plugin's, because that
	// placement decides which workers can attach a volume, and this pair is
	// ordinary pod placement for the one workload that provisions them.
	// +optional
	ControllerNodeSelector map[string]string `json:"controllerNodeSelector,omitempty"`
	// +optional
	ControllerTolerations []corev1.Toleration `json:"controllerTolerations,omitempty"`

	// ControllerResources and NodeResources set requests and limits for the two
	// plugins. Unset enforces no limits.
	// +optional
	ControllerResources corev1.ResourceRequirements `json:"controllerResources,omitempty"`
	// +optional
	NodeResources corev1.ResourceRequirements `json:"nodeResources,omitempty"`

	// SidecarImages overrides the six CSI sidecars, one field each. Unset takes
	// the version this operator release ships.
	// +optional
	SidecarImages SidecarImages `json:"sidecarImages,omitempty"`

	// EnableServiceAccountAuth makes both plugins authenticate to the management
	// API with their pod's Kubernetes service-account token instead of the
	// static cluster secret. The control plane has to list those accounts in
	// SB_K8S_ADMIN_SERVICE_ACCOUNTS for it to work, which is why this is a
	// deployment-wide switch rather than a per-plugin one.
	// +kubebuilder:default=false
	// +optional
	EnableServiceAccountAuth *bool `json:"enableServiceAccountAuth,omitempty"`

	// EnableVolumeSnapshots decides whether snapshot support is part of this
	// deployment: the VolumeSnapshotClass for DriverName, and the CRDs and a
	// controller where the cluster serves neither. False applies none of them.
	// +kubebuilder:default=true
	// +optional
	EnableVolumeSnapshots *bool `json:"enableVolumeSnapshots,omitempty"`
}

// SnapshotSupportOrigin is where the cluster's snapshot support came from.
// +kubebuilder:validation:Enum=Detected;Installed
type SnapshotSupportOrigin string

const (
	// SnapshotSupportOriginDetected is a cluster that already served
	// snapshot.storage.k8s.io/v1, so the operator applied no CRDs and no
	// controller.
	SnapshotSupportOriginDetected SnapshotSupportOrigin = "Detected"
	// SnapshotSupportOriginInstalled is a cluster where the operator applied
	// them. They are cluster-scoped and shared, so they carry no controller
	// reference and outlive this object (§4.1).
	SnapshotSupportOriginInstalled SnapshotSupportOrigin = "Installed"
)

// SimplyblockDriverOrigin is where the running deployment came from. Every
// cluster upgraded from a chart install reads Adopted, because the chart had
// applied the objects before this kind existed.
// +kubebuilder:validation:Enum=Created;Adopted
type SimplyblockDriverOrigin string

const (
	// SimplyblockDriverOriginCreated is a deployment whose objects the operator
	// applied from nothing.
	SimplyblockDriverOriginCreated SimplyblockDriverOrigin = "Created"
	// SimplyblockDriverOriginAdopted is a deployment the operator took over in
	// place, taking field ownership from Helm and removing the release's
	// metadata once the handover was verified.
	SimplyblockDriverOriginAdopted SimplyblockDriverOrigin = "Adopted"
)

// SimplyblockDriverStatus is the observed state of the CSI driver deployment.
type SimplyblockDriverStatus struct {
	// Phase is the operator's own view of the deployment.
	// +optional
	Phase SimplyblockDriverPhase `json:"phase,omitempty"`

	// SnapshotSupport is whether the cluster already had snapshot support or the
	// operator installed it, which is what says whether other drivers depend on
	// what this one applied.
	// +optional
	SnapshotSupport SnapshotSupportOrigin `json:"snapshotSupport,omitempty"`

	// Origin is whether the first reconcile created this deployment's objects
	// or met ones it did not create. It is decided once and never revised,
	// because what it records is where the running deployment came from rather
	// than what the controller did most recently.
	// +optional
	Origin SimplyblockDriverOrigin `json:"origin,omitempty"`

	// Version is the version the deployed driver reports, published so that a
	// skew against ControlPlane.status.version is visible on one screen.
	// +optional
	Version string `json:"version,omitempty"`

	// NodesReady is how many workers run a ready node plugin, and NodesTotal how
	// many are expected to. Neither takes omitempty: zero ready plugins is the
	// condition worth seeing.
	// +kubebuilder:validation:Minimum=0
	NodesReady int32 `json:"nodesReady"`
	// +kubebuilder:validation:Minimum=0
	NodesTotal int32 `json:"nodesTotal"`

	// ControllerReady is whether the controller plugin is serving.
	// +optional
	ControllerReady bool `json:"controllerReady,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the deployment moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbd
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="NodesReady",type=integer,JSONPath=".status.nodesReady"
// +kubebuilder:printcolumn:name="NodesTotal",type=integer,JSONPath=".status.nodesTotal"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SimplyblockDriver is the deployment of simplyblock's CSI driver: the node
// plugin, the controller plugin, their RBAC, and the core CSIDriver registration
// they produce. It is named for the brand rather than the interface because
// CSIDriver is already a kind in core storage.k8s.io/v1, and the two are not the
// same object: the core kind is the cluster's registration record, and this one
// is the deployment that produces it.
type SimplyblockDriver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SimplyblockDriverSpec   `json:"spec,omitempty"`
	Status SimplyblockDriverStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SimplyblockDriverList contains a list of SimplyblockDriver.
type SimplyblockDriverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SimplyblockDriver `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyblockDriver{}, &SimplyblockDriverList{})
}

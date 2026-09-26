// ControlPlane in the shape design-controlplane.md settles: FoundationDB
// together with the management API, either installed here by the operator or
// owned by a remote control plane, expressed as one object.
//
// Two things distinguish it from the registered kind it replaces. spec.source
// says which control plane the object means, so naming a remote one is a field
// rather than the SIMPLYBLOCK_WEBAPI_BASE_URL environment variable (§5.2). And status.endpoint publishes the resolved base URL, which the
// operator's control-plane clients resolve per call, so one object answers where
// the control plane is and a change to it reaches every caller without a
// Deployment rollout (§3.3).
//
// design-controlplane.md Appendix A is the specification for this file.

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// ControlPlanePhase is where the operator has got to with this control plane.
// +kubebuilder:validation:Enum=Installing;Available;Degraded;Unavailable
type ControlPlanePhase string

const (
	// ControlPlanePhaseInstalling is a control plane that has not worked yet.
	ControlPlanePhaseInstalling ControlPlanePhase = "Installing"
	// ControlPlanePhaseAvailable is one whose readiness probe passes and whose
	// workload pods are settled.
	ControlPlanePhaseAvailable ControlPlanePhase = "Available"
	// ControlPlanePhaseDegraded is one whose readiness probe passes while a
	// management API or FoundationDB pod is restarting. It answers every
	// request, so nothing holds on it and it exists to be read by a person. A
	// control plane the operator does not manage never reaches it, because the
	// operator owns no pods there to watch.
	ControlPlanePhaseDegraded ControlPlanePhase = "Degraded"
	// ControlPlanePhaseUnavailable is one whose readiness probe fails: it
	// worked and stopped, which is a different situation from one that never
	// started, and it is what downstream controllers hold on. The word claims
	// only what the probe observed, since a failing probe cannot tell a crashed
	// process from a wedged one or from a partition.
	ControlPlanePhaseUnavailable ControlPlanePhase = "Unavailable"
)

// ControlPlaneStep is one step of the installation path. There is one graph
// rather than a MultiConfig, because an entity has no spec.action to key one on.
// +kubebuilder:validation:Enum=ApplyingFoundationDB;AwaitingFoundationDB;BuildingIndices;ApplyingDatastore;ApplyingAPI;AwaitingAPI
type ControlPlaneStep string

const (
	// ControlPlaneStepApplyingFoundationDB applies the FoundationDBCluster, the
	// accounts and roles its pods need, and the FoundationDB operator itself
	// where the Kubernetes cluster does not already run one.
	ControlPlaneStepApplyingFoundationDB ControlPlaneStep = "ApplyingFoundationDB"
	// ControlPlaneStepAwaitingFoundationDB holds until that cluster reports
	// itself available, which is the step that can take the longest and the one
	// whose deadline matters.
	ControlPlaneStepAwaitingFoundationDB ControlPlaneStep = "AwaitingFoundationDB"
	// ControlPlaneStepBuildingIndices runs the control plane's own index
	// backfill against the database that just became available, and holds until
	// it reports success. It sits here because the secondary indices belong to
	// the database rather than to a cluster, and a deployment whose tables are
	// still empty is the one point where every index is complete the moment it
	// is declared ready.
	ControlPlaneStepBuildingIndices ControlPlaneStep = "BuildingIndices"
	// ControlPlaneStepApplyingDatastore applies the object store the control
	// plane keeps its long-term data in.
	ControlPlaneStepApplyingDatastore ControlPlaneStep = "ApplyingDatastore"
	// ControlPlaneStepApplyingAPI applies the management API's workload, the
	// services beside it, its account and configuration, and its Service.
	ControlPlaneStepApplyingAPI ControlPlaneStep = "ApplyingAPI"
	// ControlPlaneStepAwaitingAPI holds until the readiness probe succeeds,
	// which is what ends the installation.
	ControlPlaneStepAwaitingAPI ControlPlaneStep = "AwaitingAPI"
)

// FoundationDBSpec is the sizing of the FoundationDB the operator installs.
type FoundationDBSpec struct {
	// Replicas is the number of coordinators. Three is the smallest count that
	// survives one loss, which is why it is the default.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// StorageClassName is the class the coordinators' volumes are provisioned
	// from. It cannot be a class this operator provides, because the control
	// plane has to exist before any simplyblock volume can.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Resources sets requests and limits for the coordinator pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ControlPlaneTLSProvider is who issues the control plane's serving certificate.
//
// The two values are each product's own spelling rather than this group's
// PascalCase, because they are not this group's words: they are what
// internal/utils/tls.go already matches on and what the control-plane image
// reads out of SB_TLS_PROVIDER.
// +kubebuilder:validation:Enum=cert-manager;OpenShift
type ControlPlaneTLSProvider string

const (
	// ControlPlaneTLSCertManager issues through cert-manager, from the CA the
	// deployment's ClusterIssuer mints.
	ControlPlaneTLSCertManager ControlPlaneTLSProvider = "cert-manager"

	// ControlPlaneTLSOpenShift issues through the OpenShift service CA, which
	// signs from a Service annotation rather than from an object of its own.
	ControlPlaneTLSOpenShift ControlPlaneTLSProvider = "OpenShift"
)

// ControlPlaneTLS is how a control plane this operator installs serves, and what
// it requires of the clients that reach it.
//
// The field names are DriverTLS's, because they are the same three decisions
// about the same connection seen from its two ends, and a reader who knows one
// should not have to learn the other. What differs is the default: the CSI
// driver's fields were added to describe deployments that already existed and
// default off, and these default on. An installed control plane holds every
// cluster definition, node registration, and volume record, and is reached over
// the pod network by the operator, the CSI driver, and the metrics scrape alike,
// so plaintext is a decision to state rather than the state a deployment lands
// in by leaving the block out.
type ControlPlaneTLS struct {
	// EnableTLS serves the management API over TLS. Unset is on.
	// +kubebuilder:default=true
	// +optional
	EnableTLS *bool `json:"enableTLS,omitempty"`

	// EnableMutualTLS additionally requires a caller to present a certificate
	// of its own, rather than reaching the control plane anonymously over the
	// encrypted connection EnableTLS alone provides. Ignored when EnableTLS is
	// false, the same as on DriverTLS. Unset is on.
	// +kubebuilder:default=true
	// +optional
	EnableMutualTLS *bool `json:"enableMutualTLS,omitempty"`

	// Provider issues the serving certificate.
	// +kubebuilder:default=cert-manager
	// +optional
	Provider ControlPlaneTLSProvider `json:"provider,omitempty"`
}

// ServesTLS reports whether the installed control plane serves over TLS.
//
// It answers from the unset value rather than from a defaulted field, because an
// object built in a test or read through a cache never went past admission, and
// a TLS decision that depends on the API server having run is one that reads as
// plaintext exactly where it is not checked.
func (l *LocalControlPlane) ServesTLS() bool {
	return l == nil || l.TLS.EnableTLS == nil || *l.TLS.EnableTLS
}

// RequiresClientCertificate reports whether a caller has to present one. Mutual
// TLS is meaningless without serving TLS, so disabling the serving disables this
// too rather than leaving the two to contradict each other.
func (l *LocalControlPlane) RequiresClientCertificate() bool {
	if !l.ServesTLS() {
		return false
	}
	return l == nil || l.TLS.EnableMutualTLS == nil || *l.TLS.EnableMutualTLS
}

// TLSProvider is the issuer to use, with the default applied.
func (l *LocalControlPlane) TLSProvider() ControlPlaneTLSProvider {
	if l == nil || l.TLS.Provider == "" {
		return ControlPlaneTLSCertManager
	}
	return l.TLS.Provider
}

// LocalControlPlane is a control plane this cluster hosts, installed and owned
// by the operator. Its objects carry a controller reference to the ControlPlane,
// so the ownership spine starts at a real edge rather than at a Helm release.
type LocalControlPlane struct {
	// Image is the management API and control-plane image.
	// Must reference one of the trusted registries (`quay.io/simplyblock-io`,
	// `docker.io/simplyblock`, `public.ecr.aws/simply-block`); digest pinning
	// (@sha256:...) is recommended.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// ImagePullPolicy controls when that image is pulled.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// FoundationDB sizes the FoundationDB the management API stores its state in.
	// +optional
	FoundationDB *FoundationDBSpec `json:"foundationDB,omitempty"`

	// Replicas is the number of management API instances. Two is what the chart
	// ships and what the phases assume: a single instance makes Degraded
	// unreachable for this component and every restart an outage (§5.1).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources sets requests and limits for the management API pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Tolerations are applied to every pod the operator installs for the control
	// plane.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector pins every pod the operator installs for the control plane.
	// It is a selector rather than an affinity term because that is what the
	// chart it replaces took, and a deployment migrating off the chart has the
	// value already written down.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// TLS is how this control plane serves and what it asks of its callers.
	//
	// The block is defaulted to its own zero value rather than left absent, so
	// that the field defaults inside it are applied to an object that does not
	// mention TLS at all.
	// +kubebuilder:default={}
	// +optional
	TLS ControlPlaneTLS `json:"tls,omitempty"`

	// AdminTokenSecretRef names a Secret in this namespace holding a static
	// admin bearer token this control plane accepts, under the `token` key, in
	// addition to this deployment's own Kubernetes identity
	// (SB_K8S_ADMIN_SERVICE_ACCOUNTS). It is what lets a cluster this control
	// plane manages remotely (spec.source.managed there,
	// ManagedControlPlane.CredentialsSecretRef naming the same value)
	// authenticate a CreateCluster call, since a Kubernetes TokenReview can
	// never cross a cluster boundary.
	//
	// The Secret is projected into the management API container's environment
	// with secretKeyRef, so this operator never itself reads the plaintext.
	// Absent grants no credential beyond the operator's own service account.
	// +optional
	AdminTokenSecretRef *corev1.LocalObjectReference `json:"adminTokenSecretRef,omitempty"`
}

// ManagedControlPlane is a control plane somewhere else, which this cluster's
// storage is managed by rather than hosting. The operator installs nothing and
// owns nothing here: it resolves the endpoint, probes it, and reports.
//
// The word is about what manages the storage clusters rather than about who
// runs the operator. A deployment in this mode registers its clusters with a
// control plane it does not host, so the fleet is administered from there.
type ManagedControlPlane struct {
	// Endpoint is the management API's base URL. It is validated against the
	// same outbound-URL guard every other outbound endpoint in this group uses,
	// so a loopback or link-local address is rejected.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// CredentialsSecretRef names a Secret in this namespace holding the bearer
	// token the operator authenticates with. It is a reference rather than a
	// field because a token in a spec is a token in every `kubectl get -o yaml`.
	//
	// Absent means the endpoint is reached without one, which is the in-cluster
	// case: a control plane the Helm chart installed answers on a ClusterIP
	// Service in this namespace and does not require a token for the readiness
	// read. Naming a Secret that does not exist stays an error, because naming
	// one is a statement that the control plane needs it.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`

	// CABundleSecretRef names a Secret holding the CA certificate the endpoint
	// is verified against. Absent means the system trust store.
	// +optional
	CABundleSecretRef *corev1.LocalObjectReference `json:"caBundleSecretRef,omitempty"`

	// StorageNodeImage is the storage-node image a StorageCluster on this
	// Kubernetes cluster defaults to when its own spec.storageNodes.image is
	// unset. A local control plane's own spec.source.local.image doubles as
	// this default (StorageNodeWorkloadReconciler.image), because a
	// self-hosted deployment's control plane and its storage nodes are one
	// release. A managed one is a different Kubernetes cluster's install and
	// says nothing about what this cluster's storage nodes should run, so
	// there is no equivalent to fall back to without this field -- every
	// StorageCluster on a managed deployment must get an image from here or
	// from its own spec.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	StorageNodeImage string `json:"storageNodeImage,omitempty"`
}

// ControlPlaneSource selects whether this cluster hosts its control plane or is
// managed by one elsewhere. Exactly one member is set, which is what makes the
// two modes siblings rather than two unrelated top-level fields, and which
// member it is cannot change afterward.
//
// Both rules are declared here rather than on the field that carries the block,
// for two separate reasons.
//
// The immutability is the interesting one. What is frozen is the choice between
// the two modes and not the block, because the members have to stay editable:
// changing spec.source.local.image is an ordinary edit, and it is what a
// ControlPlaneOps upgrade performs. Spelling it +k8s:immutable on the field
// would emit self == oldSelf over the whole struct, which freezes the image with
// it and makes that operation impossible to complete.
//
// The placement is the dull one. controller-gen v0.21.0 emits a single field's
// marker-derived rules and its injected immutability rule into one list in an
// order that varies between runs, so a field carrying both produces a CRD that
// differs from itself and a drift check that fails at random. Two rules of the
// same kind on a type are emitted in source order.
// +kubebuilder:validation:XValidation:rule="(has(self.local) ? 1 : 0) + (has(self.managed) ? 1 : 0) == 1",message="set exactly one of local or managed"
// +kubebuilder:validation:XValidation:rule="has(self.local) == has(oldSelf.local) && has(self.managed) == has(oldSelf.managed)",message="spec.source is immutable: a control plane the operator installed and one it did not are different deployments, and the clusters and their volumes live in the FoundationDB behind the old one"
type ControlPlaneSource struct {
	// Local is a control plane the operator installs.
	// +optional
	Local *LocalControlPlane `json:"local,omitempty"`

	// Managed is a control plane that already exists.
	// +optional
	Managed *ManagedControlPlane `json:"managed,omitempty"`
}

// ControlPlaneSpec is the desired state of the simplyblock control plane for one
// namespace.
type ControlPlaneSpec struct {
	// Source selects whether this cluster hosts its control plane or is managed
	// by one elsewhere. Switching a live deployment between the two is not a
	// reconfiguration, because the clusters and their volumes live in the
	// FoundationDB behind the old one, so which mode is chosen is frozen at
	// creation. What is inside the chosen mode stays editable.
	//
	// Both rules are declared on ControlPlaneSource rather than here. See the
	// type for why.
	// +kubebuilder:validation:Required
	Source ControlPlaneSource `json:"source"`
}

// ControlPlaneComponentStatus is one workload of a managed control plane and how
// much of it is running. The phase is the worst verdict across these and the
// readiness probe, and only an essential component at zero ready can make it
// Unavailable.
type ControlPlaneComponentStatus struct {
	// Name is the workload's name, as applied.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Desired is how many replicas the component should have. For the component
	// carrying its own operator it is that resource's own count, because a
	// FoundationDBCluster reports quorum rather than replicas.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Desired int32 `json:"desired"`

	// Ready is how many of them are.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Ready int32 `json:"ready"`

	// Essential states whether this component at zero ready makes the control
	// plane Unavailable rather than Degraded. It is decided by the table in
	// §4.3 rather than by a user, and it is reported here so that a phase can be
	// explained without reading the operator's source.
	// +optional
	Essential bool `json:"essential,omitempty"`
}

// ControlPlaneStatus is the observed state of the control plane.
type ControlPlaneStatus struct {
	// Phase is the operator's own view of the control plane.
	// +optional
	Phase ControlPlanePhase `json:"phase,omitempty"`

	// Step is the position of the installation machine within Installing. The
	// rule repeats the ControlPlaneStep enum because a marker cannot reach a
	// field of the shared snapshot type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['ApplyingFoundationDB','AwaitingFoundationDB','BuildingIndices','ApplyingDatastore','ApplyingAPI','AwaitingAPI']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Endpoint is the resolved management API base URL, derived in the local
	// case and echoed in the managed one. It is what every controller in the
	// operator reads to reach the control plane, so that one object answers
	// where it is.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Version is the version the management API reports.
	// +optional
	Version string `json:"version,omitempty"`

	// LastChecked is when the readiness probe last ran.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Components is the per-component readiness the phase is derived from
	// (§4.3), one entry per workload the managed install applies. It is empty
	// for a remote control plane, which has no components the operator owns.
	// Without it a Degraded phase says that something is wrong and not what.
	// +optional
	// +listType=map
	// +listMapKey=name
	Components []ControlPlaneComponentStatus `json:"components,omitempty"`

	// ActiveOpsRef names the ControlPlaneOps currently allowed to act on this
	// control plane. Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the control plane moves, and never a log. On a failed probe it is the
	// control plane's own error rather than a paraphrase of it.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// v1alpha2 is the storage version in the manifests this repository ships, which
// are the ones a fresh install applies. A cluster installed today stores this
// shape from the first write and never converts anything, so the conversion
// webhook is inert there and is not deployed.
//
// An upgrade of an existing cluster is the other path, and it does not take this
// value. The upgrade tool applies these same CRDs with storage held at v1alpha1,
// because a server-side apply overwrites the live storage version and moving it
// before the conversion webhook is serving breaks every write. It flips to
// v1alpha2 with the storage rewrite once the migration has run
// (design-api-upgrade.md §24, design-property-renames.md §3.8).
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cp
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ControlPlane is the simplyblock control plane for one Kubernetes cluster:
// FoundationDB together with the management API, either installed by the
// operator or already existing. It is a singleton named `simplyblock`, and it is
// the root of the ownership spine: nothing else in this API group reconciles
// meaningfully before it reports Available.
type ControlPlane struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec ControlPlaneSpec `json:"spec,omitempty"`

	// +optional
	Status ControlPlaneStatus `json:"status,omitempty"`
}

// Hub marks this version as the conversion hub for ControlPlane. It carries no
// behavior: its presence is what tells controller-runtime which version every
// other one converts through.
func (*ControlPlane) Hub() {}

// +kubebuilder:object:root=true

// ControlPlaneList contains a list of ControlPlane resources.
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControlPlane{}, &ControlPlaneList{})
}

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

	"github.com/simplyblock/atlas/crdtypes"
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
// +kubebuilder:validation:Enum=ApplyingFoundationDB;AwaitingFoundationDB;BuildingIndices;ApplyingDatastore;ApplyingAPI;AwaitingAPI;Installed
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
	// ControlPlaneStepInstalled is the terminal step: the installation is
	// complete, and the phase reports the control plane's health from here on.
	ControlPlaneStepInstalled ControlPlaneStep = "Installed"
)

// StretchedControlPlaneRole is which of the two sites bootstraps the shared
// FoundationDB.
// +kubebuilder:validation:Enum=Bootstrap;Join
type StretchedControlPlaneRole string

const (
	// StretchedControlPlaneRoleBootstrap creates the database: its FoundationDB
	// comes up alone, and its region is the one with priority, so the database
	// prefers to run its transaction system there.
	StretchedControlPlaneRoleBootstrap StretchedControlPlaneRole = "Bootstrap"
	// StretchedControlPlaneRoleJoin joins the database the Bootstrap site
	// created, reached through the connection string in
	// seedConnectionStringSecretRef, and adds the second region to it.
	StretchedControlPlaneRoleJoin StretchedControlPlaneRole = "Join"
)

// StretchedControlPlaneSpec is one site of a stretched control plane. The two
// sites' specs mirror each other: each names itself and the other, and the one
// that bootstraps the database is the same on both, which is what makes both
// operators render the same FoundationDB region configuration.
// +kubebuilder:validation:XValidation:rule="self.site != self.peerSite",message="site and peerSite must differ"
// +kubebuilder:validation:XValidation:rule="self.role != 'Join' || has(self.seedConnectionStringSecretRef)",message="a Join site needs seedConnectionStringSecretRef: the connection string of the database the Bootstrap site created"
// +kubebuilder:validation:XValidation:rule="!has(self.witnessSite) || (self.witnessSite != self.site && self.witnessSite != self.peerSite)",message="witnessSite must differ from site and peerSite"
type StretchedControlPlaneSpec struct {
	// Site is the name of the site this control plane runs in. Every
	// control-plane pod learns it, storage nodes added from this Kubernetes
	// cluster carry it, and the StorageCluster of this site names the same
	// site in spec.stretched.site.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	Site string `json:"site"`

	// PeerSite is the other site's name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	PeerSite string `json:"peerSite"`

	// Role is Bootstrap on the site that creates the database and Join on the
	// one that joins it.
	// +kubebuilder:validation:Required
	Role StretchedControlPlaneRole `json:"role"`

	// APIEndpoint is the URL of this site's management API as the other site
	// and its clients reach it (a load balancer or node port, not the
	// in-cluster Service). The other site's standby control plane redirects
	// mutating calls to it while this site leads, and the operator records it
	// on the cluster at creation (Bootstrap) or registration (Join).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+`
	APIEndpoint string `json:"apiEndpoint"`

	// SeedConnectionStringSecretRef names a Secret in the operator's namespace
	// whose "cluster-file" key holds the connection string of the database the
	// Bootstrap site created (its FoundationDB ConfigMap's cluster-file).
	// Required on a Join site, ignored on the Bootstrap site.
	// +optional
	SeedConnectionStringSecretRef *corev1.LocalObjectReference `json:"seedConnectionStringSecretRef,omitempty"`

	// WitnessSite names a third site that holds FoundationDB coordinators and
	// satellite logs but no control plane and no storage. With it, losing
	// either site leaves the coordinator majority, so the survivor's database
	// keeps answering and a takeover needs no FoundationDB recovery first.
	// Once the witness's processes have joined (status.stretch.witnessJoined)
	// both regions name it as their satellite. Both sites must name the same
	// witness. The witness's own FoundationDBCluster joins through the
	// Bootstrap site's connection string, runs log processes only, and does
	// not configure the database (automationOptions.configureDatabase: false).
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	WitnessSite string `json:"witnessSite,omitempty"`
}

// IsJoin reports whether this is the site joining the database another
// created.
func (s *StretchedControlPlaneSpec) IsJoin() bool {
	return s != nil && s.Role == StretchedControlPlaneRoleJoin
}

// BootstrapSite is the site that created the database, whichever side asks.
func (s *StretchedControlPlaneSpec) BootstrapSite() string {
	if s.IsJoin() {
		return s.PeerSite
	}
	return s.Site
}

// SeedConnectionStringKey is the key of the Secret
// seedConnectionStringSecretRef names, the same key the FoundationDB
// ConfigMap uses.
const SeedConnectionStringKey = "cluster-file"

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

	// NodeSelector pins the database's own processes, and nothing else the
	// control plane runs. Unset leaves them wherever the control plane as a
	// whole is placed.
	//
	// It is stated apart from that placement because the database is the part
	// whose host matters to something else: adding a storage node reboots its
	// worker, and a worker running a FoundationDB process is added on its own
	// however many nodes the cluster's provisioning budget allows at once.
	// Confining the database to a few workers is what leaves the rest of the
	// fleet free to be added together.
	//
	// It has to select at least as many workers as
	// spec.source.local.foundationDB.replicas, because the worker is the
	// database's fault domain. The anti-affinity between processes of one class
	// is a preference rather than a requirement, so a narrower selector places
	// the coordinators anyway, several to a worker, where one worker's loss
	// takes more of them than the redundancy mode was configured to survive.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are applied to the database's own processes, and nothing else
	// the control plane runs. Unset leaves them with the control plane's
	// tolerations, spec.source.local.tolerations. When set, they replace those
	// for the database rather than adding to them, the rule NodeSelector
	// follows: the database confined to tainted workers tolerates their taint,
	// and the rest of the control plane does not follow it there.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// ControlPlaneTLSProvider is who issues the control plane's serving certificate.
//
// The two values are each product's own spelling rather than this group's
// PascalCase, because they are not this group's words.
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
// +kubebuilder:validation:XValidation:rule="has(self.stretch) == has(oldSelf.stretch)",message="stretch is immutable: a site is a property of the installation, and the database behind a stretched control plane is laid out for two sites"
type LocalControlPlane struct {
	// Image is the management API and control-plane image.
	// Any registry is accepted, mirrors included, as long as the value is a
	// well-formed reference carrying a tag, a digest, or both. Digest pinning
	// (@sha256:...) is recommended.
	// +kubebuilder:validation:Required
	Image crdtypes.ImageReference `json:"image"`

	// ImagePullPolicy controls when that image is pulled.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// FoundationDB sizes the FoundationDB the management API stores its state in.
	// +optional
	FoundationDB *FoundationDBSpec `json:"foundationDB,omitempty"`

	// Stretch makes this control plane one site of a stretched control plane:
	// two Kubernetes clusters, one per site, whose control planes share one
	// FoundationDB laid out as two regions (one per site, three copies in
	// each), one site leading at a time (docs/designs/design-stretched-control-plane.md).
	// Unset, the control plane is an ordinary single-site installation.
	// Immutable: a site is a property of the installation.
	// +optional
	Stretch *StretchedControlPlaneSpec `json:"stretch,omitempty"`

	// Replicas is the number of management API instances. Two is what the chart
	// ships and what the phases assume: a single instance makes Degraded
	// unreachable for this component and every restart an outage.
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
	//
	// It cannot change once the object exists. The database and every storage
	// node are built with it, so turning TLS off on a live control plane leaves
	// peers that still demand it and callers that no longer present anything.
	// Changing it means recreating the control plane, which deletes the
	// FoundationDB behind it.
	// +kubebuilder:default={}
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tls is immutable once the control plane exists"
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
	// Observability connects this control plane to the monitoring stack
	// installed beside it: Graylog, Grafana, and OpenSearch. Absent leaves
	// monitoring off.
	// +optional
	Observability *ControlPlaneObservability `json:"observability,omitempty"`

	// Prerequisites says which cluster add-ons the operator installs when the
	// cluster lacks them. One left false is checked for and never installed.
	// +kubebuilder:default={}
	// +optional
	Prerequisites ControlPlanePrerequisites `json:"prerequisites,omitempty"`

	// EKSRoleARN is the IAM role this control plane's pods and every
	// StorageCluster's storage-node pods in this namespace assume, through a
	// service account annotated with it (eks.amazonaws.com/role-arn). Amazon
	// EKS's pod-identity webhook reads that annotation and injects the
	// credentials the AWS SDK's default provider chain picks up from there,
	// which is what lets a StorageCluster's backup.credentials name
	// ProviderChain on EKS.
	//
	// Left unset, the service accounts this operator creates carry no such
	// annotation, which is correct off Amazon EKS and on an EKS cluster whose
	// pods authenticate another way.
	// +kubebuilder:validation:Pattern=`^arn:aws[a-zA-Z0-9-]*:iam::[0-9]{12}:role/.+$`
	// +optional
	EKSRoleARN string `json:"eksRoleARN,omitempty"`
}

// ControlPlanePrerequisites lists the add-ons a control plane needs before
// installing. Each is detected first and installed only when it is missing and
// its flag is on; with the flag off, a missing add-on holds the install.
// Installed add-ons are cluster-wide and outlive the ControlPlane.
type ControlPlanePrerequisites struct {
	// InstallFoundationDBCRDs applies the apps.foundationdb.org CRDs when
	// apps.foundationdb.org/v1beta2 is not served. Unset is on.
	// +kubebuilder:default=true
	// +optional
	InstallFoundationDBCRDs *bool `json:"installFoundationDBCRDs,omitempty"`

	// InstallCertManager installs cert-manager when cert-manager.io/v1 is not
	// served. It is consulted only when the control plane serves TLS issued by
	// cert-manager. Unset is on.
	// +kubebuilder:default=true
	// +optional
	InstallCertManager *bool `json:"installCertManager,omitempty"`

	// InstallLocalPathProvisioner installs local-path-provisioner when no
	// StorageClass uses rancher.io/local-path. Its class is made the default
	// only when the cluster has no default class. Unset is on.
	// +kubebuilder:default=true
	// +optional
	InstallLocalPathProvisioner *bool `json:"installLocalPathProvisioner,omitempty"`

	// InstallSnapshotController installs the CSI snapshot CRDs and
	// snapshot-controller when snapshot.storage.k8s.io/v1 is not served. Unset
	// is on.
	// +kubebuilder:default=true
	// +optional
	InstallSnapshotController *bool `json:"installSnapshotController,omitempty"`

	// InstallEBSCSIDriver installs the AWS EBS CSI driver on Amazon EKS when
	// the cluster has no CSIDriver ebs.csi.aws.com, neither the EKS add-on nor
	// a driver installed some other way. It is consulted only on EKS, and only
	// while the control plane's volumes name no StorageClass. The node role, or
	// the role in eksRoleARN, needs the AWS managed policy
	// AmazonEBSCSIDriverPolicy. Unset is on.
	// +kubebuilder:default=true
	// +optional
	InstallEBSCSIDriver *bool `json:"installEBSCSIDriver,omitempty"`

	// InstallEBSStorageClass creates the gp3 StorageClass simplyblock-gp3 on
	// Amazon EKS when no StorageClass of the EBS CSI driver provisions gp3
	// volumes. An existing one, under any name, is used instead. The class is
	// made the default only when the cluster has no default class. Unset is on.
	// +kubebuilder:default=true
	// +optional
	InstallEBSStorageClass *bool `json:"installEBSStorageClass,omitempty"`
}

// on reads a defaulted-true flag, treating unset as true.
func on(b *bool) bool { return b == nil || *b }

// InstallsFoundationDBCRDs reports whether missing FoundationDB CRDs are installed.
func (p ControlPlanePrerequisites) InstallsFoundationDBCRDs() bool {
	return on(p.InstallFoundationDBCRDs)
}

// InstallsCertManager reports whether a missing cert-manager is installed.
func (p ControlPlanePrerequisites) InstallsCertManager() bool { return on(p.InstallCertManager) }

// InstallsLocalPathProvisioner reports whether a missing local-path-provisioner is installed.
func (p ControlPlanePrerequisites) InstallsLocalPathProvisioner() bool {
	return on(p.InstallLocalPathProvisioner)
}

// InstallsSnapshotController reports whether a missing snapshot controller is installed.
func (p ControlPlanePrerequisites) InstallsSnapshotController() bool {
	return on(p.InstallSnapshotController)
}

// InstallsEBSCSIDriver reports whether a missing EBS CSI driver is installed on EKS.
func (p ControlPlanePrerequisites) InstallsEBSCSIDriver() bool { return on(p.InstallEBSCSIDriver) }

// InstallsEBSStorageClass reports whether a missing gp3 StorageClass is created on EKS.
func (p ControlPlanePrerequisites) InstallsEBSStorageClass() bool {
	return on(p.InstallEBSStorageClass)
}

// NeedsCertManager reports whether this control plane's TLS is issued by cert-manager.
func (l *LocalControlPlane) NeedsCertManager() bool {
	return l.ServesTLS() && l.TLSProvider() == ControlPlaneTLSCertManager
}

// ControlPlaneObservability is whether the control plane provisions the
// monitoring stack, and the credential it does that with.
//
// The control plane provisions it once, when the first storage cluster is
// created: it opens the Graylog input the log collector ships to, creates the
// Grafana user every cluster's dashboards use, and widens the OpenSearch result
// window. It records the decision with the deployment, so enabling monitoring
// after the first cluster exists sets the environment but does not provision
// anything.
// +kubebuilder:validation:XValidation:rule="!has(self.secretRef) || (has(self.secretRef.name) && size(self.secretRef.name) > 0)",message="secretRef.name must not be empty: a pod reading a Secret with no name does not start"
// +kubebuilder:validation:XValidation:rule="!has(self.enableMonitoring) || !self.enableMonitoring || has(self.secretRef)",message="enableMonitoring requires secretRef: the control plane cannot provision the monitoring stack without its admin password"
type ControlPlaneObservability struct {
	// EnableMonitoring provisions the monitoring stack when the first storage
	// cluster is created. Unset is off.
	// +optional
	EnableMonitoring bool `json:"enableMonitoring,omitempty"`

	// SecretRef names a Secret in this namespace holding the monitoring stack's
	// admin password under the key MONITORING_SECRET. Required when
	// EnableMonitoring is set, and its name must not be empty. A pod naming a
	// Secret that does not exist does not start.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// MonitoringEnabled reports whether the control plane is to provision the
// monitoring stack.
func (l *LocalControlPlane) MonitoringEnabled() bool {
	return l != nil && l.Observability != nil && l.Observability.EnableMonitoring
}

// MonitoringSecretRef is the Secret holding the monitoring stack's admin
// password, or nil when none is named.
func (l *LocalControlPlane) MonitoringSecretRef() *corev1.LocalObjectReference {
	if l == nil || l.Observability == nil {
		return nil
	}
	return l.Observability.SecretRef
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
	// +optional
	StorageNodeImage crdtypes.ImageReference `json:"storageNodeImage,omitempty"`
}

// ControlPlaneSource selects whether this cluster hosts its control plane or is
// managed by one elsewhere. Exactly one member is set, which is what makes the
// two modes siblings rather than two unrelated top-level fields.
//
// Which member it is cannot change afterward, while the members themselves stay
// editable: changing spec.source.local.image is an ordinary edit, and it is what
// a ControlPlaneOps upgrade performs.
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
	// plane Unavailable rather than Degraded. It is decided by the operator
	// rather than by a user, and it is reported here so that a phase can be
	// explained.
	// +optional
	Essential bool `json:"essential,omitempty"`
}

// ControlPlaneStatus is the observed state of the control plane.
type ControlPlaneStatus struct {
	// Phase is the operator's own view of the control plane.
	// +optional
	Phase ControlPlanePhase `json:"phase,omitempty"`

	// Step is the position of the installation machine, and Installed once the
	// installation is complete. The rule repeats the ControlPlaneStep enum
	// because a marker cannot reach a field of the shared snapshot type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['ApplyingFoundationDB','AwaitingFoundationDB','BuildingIndices','ApplyingDatastore','ApplyingAPI','AwaitingAPI','Installed']",message="unknown step"
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

	// StorageClassName is the StorageClass the operator chose for the control
	// plane's volumes (FoundationDB, the object store, and Prometheus) when
	// spec.source.local.foundationDB.storageClassName names none: on Amazon
	// EKS, a gp3 class of the EBS CSI driver. It is chosen once, before the
	// database is first applied, and never changed afterwards, because the
	// volumes cannot move to another class.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// LastChecked is when the readiness probe last ran.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Components is the per-component readiness the phase is derived from, one
	// entry per workload the managed install applies. It is empty for a remote
	// control plane, which has no components the operator owns. Without it a
	// Degraded phase says that something is wrong and not what.
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

	// Stretch is the state of this site of a stretched control plane; unset on
	// an ordinary one.
	// +optional
	Stretch *StretchedControlPlaneStatus `json:"stretch,omitempty"`
}

// StretchedControlPlaneStatus is what one site of a stretched control plane
// has learned about the other.
type StretchedControlPlaneStatus struct {
	// PeerJoined is whether the other site's FoundationDB processes have been
	// seen in the database. Until then the database is configured with this
	// site's region alone (the other at a negative priority, so processes
	// that join take no data); from then on with both regions usable. It is
	// never unset: a peer that disappears later is a region outage, which the
	// database rides out, not a reason to drop the region.
	// +optional
	PeerJoined bool `json:"peerJoined,omitempty"`

	// PeerJoinedAt is when PeerJoined was first observed.
	// +optional
	PeerJoinedAt *metav1.Time `json:"peerJoinedAt,omitempty"`

	// WitnessJoined is whether the witness site's FoundationDB processes have
	// been seen in the database. From then on both regions name the witness
	// as their satellite. Never unset, for the same reason as PeerJoined.
	// +optional
	WitnessJoined bool `json:"witnessJoined,omitempty"`

	// WitnessJoinedAt is when WitnessJoined was first observed.
	// +optional
	WitnessJoinedAt *metav1.Time `json:"witnessJoinedAt,omitempty"`
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

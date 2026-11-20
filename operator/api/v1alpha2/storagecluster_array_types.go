// The multi-attach mode of a StorageCluster and the storage array its LUNs live
// on. It is a file of its own because the array block is the one part of the
// cluster spec that carries a credential reference and a lifecycle of its own
// (registration, credential rotation) beside the cluster's creation.

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// MultiAttachSpec makes a cluster run on LUNs of an external storage array that
// are presented to several storage nodes at once. Each node's LUN is also
// presented to the nodes that back it up, and when a node fails one of them
// takes the LUN over after the array has cut the failed node off. Data is
// protected by the array, so the cluster stores one copy (1+0).
type MultiAttachSpec struct {
	// FaultTolerance is how many storage-node failures the cluster survives:
	// 1 or 2. It is fixed when the cluster is created.
	// +kubebuilder:validation:Enum=1;2
	// +kubebuilder:validation:Required
	FaultTolerance int32 `json:"faultTolerance"`

	// Array is the storage array simplyblock creates, presents, grows, and
	// cuts off the cluster's LUNs on. Omitted, the LUNs are presented to the
	// workers by other means, and a failed node's LUN is never taken over,
	// because nothing can cut the failed node off first.
	// +optional
	Array *StorageArraySpec `json:"array,omitempty"`
}

// StorageArrayProtocol is the storage protocol the workers reach the array's
// LUNs over. The values are the control plane's own names for them.
// +kubebuilder:validation:Enum=iscsi;fc;nvme-tcp;nvme-rdma;nvme-fc;sdc
type StorageArrayProtocol string

const (
	StorageArrayProtocolISCSI    StorageArrayProtocol = "iscsi"
	StorageArrayProtocolFC       StorageArrayProtocol = "fc"
	StorageArrayProtocolNVMeTCP  StorageArrayProtocol = "nvme-tcp"
	StorageArrayProtocolNVMeRDMA StorageArrayProtocol = "nvme-rdma"
	StorageArrayProtocolNVMeFC   StorageArrayProtocol = "nvme-fc"
	// StorageArrayProtocolSDC is Dell PowerFlex's own client.
	StorageArrayProtocolSDC StorageArrayProtocol = "sdc"
)

// StorageArraySpec is how simplyblock reaches and uses the cluster's array.
type StorageArraySpec struct {
	// Plugin names the array family: pure-flasharray, netapp-ontap,
	// dell-powerstore, dell-powermax, dell-powerflex, hpe-wsapi, hpe-nimble,
	// ibm-flashsystem, hitachi-vsp, huawei-dorado, infinidat-infinibox, or
	// aws-ebs. A plugin installed into the control plane separately is named
	// the same way.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:Required
	Plugin string `json:"plugin"`

	// Endpoint is the array's management URL, e.g.
	// https://pure01.example.com. Plugins that need none (aws-ebs) leave it
	// out.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Protocol is how the workers reach the LUNs. Each worker is registered on
	// the array with its initiator for this protocol: its iSCSI IQN, FC port
	// names, NVMe host NQN, or PowerFlex SDC GUID. Required by every plugin but
	// aws-ebs.
	// +optional
	Protocol StorageArrayProtocol `json:"protocol,omitempty"`

	// LunSize is the size of the LUN created for each storage node. All
	// nodes' LUNs are created at this size and grown together.
	// +kubebuilder:validation:Required
	LunSize resource.Quantity `json:"lunSize"`

	// CABundle is the PEM certificate (or bundle) the array's management
	// endpoint is verified against, for an array whose certificate no public
	// authority signed.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	CABundle string `json:"caBundle,omitempty"`

	// Options are the plugin's own settings, such as the pool, storage
	// virtual machine, CPG, or port group the LUNs are created in. Each
	// plugin documents the keys it reads.
	// +kubebuilder:validation:MaxProperties=64
	// +optional
	Options map[string]string `json:"options,omitempty"`

	// Credentials says where the array's login comes from.
	// +kubebuilder:validation:Required
	Credentials StorageArrayCredentials `json:"credentials"`

	// Journal is where each storage node keeps its journal. Omitted, every
	// node's journal is a LUN of its own on the array.
	// +optional
	Journal *StorageArrayJournal `json:"journal,omitempty"`
}

// StorageArrayJournalPlacement is where a storage node's journal is kept.
// +kubebuilder:validation:Enum=LUN;Local
type StorageArrayJournalPlacement string

const (
	// StorageArrayJournalLUN: a LUN of its own, created on the array when the
	// node is added, next to its data LUN, and presented to that node only.
	// The workers need no local device.
	StorageArrayJournalLUN StorageArrayJournalPlacement = "LUN"
	// StorageArrayJournalLocal: a local disk or partition of the worker,
	// named per storage node in the deployment document and used whole.
	StorageArrayJournalLocal StorageArrayJournalPlacement = "Local"
)

// StorageArrayJournal is where the storage nodes keep their journals, and how
// a journal LUN is created.
// +kubebuilder:validation:XValidation:rule="!has(self.placement) || self.placement == 'LUN' || (!has(self.size) && !has(self.options))",message="size and options apply to the placement LUN only"
type StorageArrayJournal struct {
	// Placement is LUN (a LUN of its own on the array) or Local (a local disk
	// or partition of the worker).
	// +kubebuilder:default=LUN
	// +optional
	Placement StorageArrayJournalPlacement `json:"placement,omitempty"`

	// Size is the size of each node's journal LUN. Omitted, it is 3% of
	// lunSize, at least 2 GiB.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// Options are plugin settings for the journal LUNs, applied over the
	// array's options, such as a pool of higher performance or priority to
	// create them in.
	// +kubebuilder:validation:MaxProperties=64
	// +optional
	Options map[string]string `json:"options,omitempty"`
}

// JournalOnLUN reports whether the storage nodes keep their journals on LUNs
// of the array, which the control plane creates as it adds them. A cluster
// without an array keeps them locally.
func (m *MultiAttachSpec) JournalOnLUN() bool {
	if m == nil || m.Array == nil {
		return false
	}
	return m.Array.Journal == nil || m.Array.Journal.Placement != StorageArrayJournalLocal
}

// StorageArrayCredentialsSource selects where an array's login is kept.
// +kubebuilder:validation:Enum=None;Secret;VaultKV;KMIPSecret
type StorageArrayCredentialsSource string

const (
	// StorageArrayCredentialsSourceNone: the plugin needs no login of its
	// own (aws-ebs on the nodes' instance role).
	StorageArrayCredentialsSourceNone StorageArrayCredentialsSource = "None"
	// StorageArrayCredentialsSourceSecret: a Secret beside the cluster. Its
	// keys are the plugin's login fields (username and password, api_token,
	// ...). The control plane encrypts them with the cluster's key store; the
	// Secret can be deleted once the cluster reports the array registered.
	StorageArrayCredentialsSourceSecret StorageArrayCredentialsSource = "Secret"
	// StorageArrayCredentialsSourceVaultKV: a HashiCorp Vault KV v2 secret the
	// control plane reads with the cluster's Vault connection. Kubernetes
	// never holds the login.
	StorageArrayCredentialsSourceVaultKV StorageArrayCredentialsSource = "VaultKV"
	// StorageArrayCredentialsSourceKMIPSecret: a KMIP secret-data object the
	// control plane reads from the cluster's KMIP server. Kubernetes never
	// holds the login.
	StorageArrayCredentialsSourceKMIPSecret StorageArrayCredentialsSource = "KMIPSecret"
)

// StorageArrayCredentials is where an array's login is kept. Source selects
// it, and exactly the matching field is set.
// +kubebuilder:validation:XValidation:rule="(self.source == 'Secret') == has(self.secretRef)",message="secretRef is set exactly when source is Secret"
// +kubebuilder:validation:XValidation:rule="(self.source == 'VaultKV') == has(self.vaultKV)",message="vaultKV is set exactly when source is VaultKV"
// +kubebuilder:validation:XValidation:rule="(self.source == 'KMIPSecret') == has(self.kmipSecret)",message="kmipSecret is set exactly when source is KMIPSecret"
type StorageArrayCredentials struct {
	// Source selects where the login is kept: None, Secret, VaultKV, or
	// KMIPSecret.
	// +kubebuilder:validation:Required
	Source StorageArrayCredentialsSource `json:"source"`

	// SecretRef names the Secret, in the cluster's namespace, whose keys are
	// the plugin's login fields. Editing the Secret rotates the login.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// VaultKV names the Vault KV v2 secret holding the login fields.
	// +optional
	VaultKV *StorageArrayVaultKVSecret `json:"vaultKV,omitempty"`

	// KMIPSecret names the KMIP secret-data object holding the login.
	// +optional
	KMIPSecret *StorageArrayKMIPSecret `json:"kmipSecret,omitempty"`
}

// StorageArrayVaultKVSecret is a Vault KV v2 secret.
type StorageArrayVaultKVSecret struct {
	// Mount is the KV v2 secrets engine's mount path, e.g., secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Required
	Mount string `json:"mount"`

	// Path is the secret's path within the mount, e.g., arrays/pure01.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Required
	Path string `json:"path"`

	// Version pins one version of the secret. Omitted, the newest is read
	// every time, so rotating the secret in Vault rotates the login.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Version *int32 `json:"version,omitempty"`
}

// StorageArrayKMIPSecret is a KMIP secret-data object.
type StorageArrayKMIPSecret struct {
	// UID is the object's unique identifier on the KMIP server.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Required
	UID string `json:"uid"`

	// Field takes the object's whole value as the one login field of this
	// name (an API token). Omitted, the value is read as a JSON object of
	// login fields.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Field string `json:"field,omitempty"`
}

// StorageArrayStatus is the array registration as the control plane holds it.
type StorageArrayStatus struct {
	// ID is the control plane's id of the registered array.
	// +optional
	ID string `json:"id,omitempty"`

	// Registered is true once the control plane has connected to the array
	// with the current spec and login and saved the registration. Storage
	// nodes are added only after that, because adding one creates its LUN.
	// +optional
	Registered bool `json:"registered,omitempty"`

	// Protocols are the protocols the array reported it can serve.
	// +optional
	Protocols []string `json:"protocols,omitempty"`

	// ObservedGeneration is the cluster generation last registered.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CredentialsVersion is the resource version of the credentials Secret
	// last registered; a newer one is registered again.
	// +optional
	CredentialsVersion string `json:"credentialsVersion,omitempty"`

	// Message is why the last registration was refused; empty once the array
	// is registered.
	// +optional
	Message string `json:"message,omitempty"`
}

// ArrayReady reports whether the cluster's storage nodes may be added: always
// for a cluster without a storage array, and once the array is registered for
// one with. Adding a node creates its LUN on the array, which needs the
// registration first.
func (c *StorageCluster) ArrayReady() bool {
	ma := c.Spec.MultiAttach
	if ma == nil || ma.Array == nil {
		return true
	}
	return c.Status.Array != nil && c.Status.Array.Registered
}

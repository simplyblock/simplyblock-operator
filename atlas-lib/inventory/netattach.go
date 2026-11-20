// Which NetworkAttachmentDefinitions a namespace holds, and what each one's CNI
// configuration says about the host it attaches a pod to.
//
// A NetworkAttachmentDefinition is Multus's: a named CNI configuration a pod
// asks for by annotation, which gives it an interface beside its default one.
// It is the alternative to host networking for a storage node, and whether a
// cluster offers it at all is the first question: the kind is a
// CustomResourceDefinition that only a cluster running Multus registers.
//
// It lives here rather than in atlas/kube for the reason the environment does.
// The kind is read from the Kubernetes API, but what a definition states is a
// fact about the machines: which host interface the plugin builds on, which is
// what decides whether a worker can be attached at all. A discovery run plans a
// deployment against it, alongside the interfaces the same worker reports.
//
// The kind is read unstructured. This library does not take a dependency on
// Multus's API module for one field of one kind, and the operator reads the
// same kind the same way.
//
// Nothing here fails. A cluster that does not serve the kind, a namespace that
// holds none, and a list that was refused are three answers, and each is
// reported as one: the last in particular must not read as the first, or a
// fleet that has Multus is planned onto host networking without a word.

package inventory

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// NetworkAttachmentDefinitionResource is the kind's resource, as the dynamic
// client and the discovery API name it.
var NetworkAttachmentDefinitionResource = schema.GroupVersionResource{
	Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions",
}

// NetworkAttachmentResourceNameAnnotation is where the SR-IOV and other device
// plugins' resource is named. It is on the object rather than in the CNI
// configuration because it is the scheduler's input, not the plugin's.
const NetworkAttachmentResourceNameAnnotation = "k8s.v1.cni.cncf.io/resourceName"

// NetworkAttachmentsState is what a reading of the definitions found.
type NetworkAttachmentsState string

const (
	// NetworkAttachmentsServed is a cluster that serves the kind. Definitions
	// holds what the namespace has, which may be none.
	NetworkAttachmentsServed NetworkAttachmentsState = "Served"

	// NetworkAttachmentsNotServed is a cluster that does not register the kind,
	// which is a cluster without Multus.
	NetworkAttachmentsNotServed NetworkAttachmentsState = "NotServed"

	// NetworkAttachmentsUnread is a reading that failed. Detail says why, and
	// nothing is concluded about the cluster.
	NetworkAttachmentsUnread NetworkAttachmentsState = "Unread"
)

// NetworkAttachments is what one namespace offers.
//
// The zero value is a reading that was not taken, which is what a collection
// without a cluster source has. It is not NetworkAttachmentsNotServed.
type NetworkAttachments struct {
	State NetworkAttachmentsState

	// Detail is why the reading failed, and is empty otherwise.
	Detail string

	// Namespace is the namespace that was listed, and is empty for a listing
	// across all of them.
	Namespace string

	// Definitions are the namespace's definitions, ascending by namespace and
	// name.
	Definitions []NetworkAttachment
}

// NetworkAttachment is one NetworkAttachmentDefinition.
type NetworkAttachment struct {
	Namespace string
	Name      string

	// ResourceName is the device plugin resource the definition asks the
	// scheduler for, from NetworkAttachmentResourceNameAnnotation, and is empty
	// for a definition that needs none.
	ResourceName string

	// Config is what its CNI configuration says.
	Config NetworkAttachmentConfig
}

// NetworkAttachmentConfig is what a CNI configuration says about the host.
type NetworkAttachmentConfig struct {
	// Type is the plugin that creates the interface: ipvlan, macvlan,
	// host-device, bridge, sriov. For a plugin list it is the first plugin's,
	// since the ones after it change an interface that already exists.
	Type string

	// HostInterface is the host interface the plugin builds on: the master of
	// an ipvlan, macvlan, or VLAN plugin, the device a host-device moves into the pod,
	// or the bridge a bridge plugin attaches to. It is empty for a plugin that
	// names none, such as sriov, whose device comes from ResourceName.
	HostInterface string

	// PCIAddress is the slot a host-device names instead of an interface.
	PCIAddress string

	// Mode is the ipvlan or macvlan mode, l2 or bridge, and empty for a plugin
	// without one.
	Mode string

	// IPAM is the address plugin: static, whereabouts, host-local, or DHCP.
	IPAM string

	// Problem says why nothing could be read from the configuration, and is
	// empty when it could.
	Problem string
}

// NetworkAttachmentSource is where the definitions are read from.
//
// Dynamic is the only field required. Discovery answers whether the kind is
// served without needing the right to list it; without it, the list's own 404
// is the answer.
type NetworkAttachmentSource struct {
	Discovery discovery.DiscoveryInterface
	Dynamic   dynamic.Interface

	// Namespace is the namespace to list. Empty lists every namespace, which
	// needs a cluster-wide grant a probe does not have.
	Namespace string
}

// configured reports whether a source was given at all.
func (s NetworkAttachmentSource) configured() bool { return s.Dynamic != nil }

// CollectNetworkAttachments reads whether the cluster serves the kind and, when
// it does, the definitions the namespace holds.
func CollectNetworkAttachments(ctx context.Context, source NetworkAttachmentSource) NetworkAttachments {
	out := NetworkAttachments{Namespace: source.Namespace}
	if !source.configured() {
		out.State = NetworkAttachmentsUnread
		out.Detail = "no Kubernetes client to read them with"
		return out
	}

	if source.Discovery != nil {
		served, err := kindServed(source.Discovery)
		switch {
		case err != nil:
			out.State, out.Detail = NetworkAttachmentsUnread, fmt.Sprintf("ask whether the cluster serves %s: %v",
				NetworkAttachmentDefinitionResource.GroupResource(), err)
			return out
		case !served:
			out.State = NetworkAttachmentsNotServed
			return out
		}
	}

	list, err := source.Dynamic.Resource(NetworkAttachmentDefinitionResource).
		Namespace(source.Namespace).List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// The API server answers 404 for a resource it does not serve; a
		// namespace that does not exist lists empty instead.
		out.State = NetworkAttachmentsNotServed
		return out
	case err != nil:
		out.State, out.Detail = NetworkAttachmentsUnread, fmt.Sprintf("list %s: %v",
			NetworkAttachmentDefinitionResource.GroupResource(), err)
		return out
	}

	out.State = NetworkAttachmentsServed
	for _, item := range list.Items {
		out.Definitions = append(out.Definitions, networkAttachmentOf(item))
	}
	slices.SortFunc(out.Definitions, func(a, b NetworkAttachment) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// kindServed asks the discovery API whether the kind's group version lists the
// resource.
func kindServed(disco discovery.DiscoveryInterface) (bool, error) {
	resources, err := disco.ServerResourcesForGroupVersion(NetworkAttachmentDefinitionResource.GroupVersion().String())
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool {
		return r.Name == NetworkAttachmentDefinitionResource.Resource
	}), nil
}

func networkAttachmentOf(item unstructured.Unstructured) NetworkAttachment {
	config, _, _ := unstructured.NestedString(item.Object, "spec", "config")
	return NetworkAttachment{
		Namespace:    item.GetNamespace(),
		Name:         item.GetName(),
		ResourceName: item.GetAnnotations()[NetworkAttachmentResourceNameAnnotation],
		Config:       ParseNetworkAttachmentConfig(config),
	}
}

// cniPlugin is the part of one plugin's configuration this reads. The fields
// are the plugins' own spellings.
type cniPlugin struct {
	Type     string `json:"type"`
	Master   string `json:"master"`
	Device   string `json:"device"`
	PCIBusID string `json:"pciBusID"`
	Bridge   string `json:"bridge"`
	Mode     string `json:"mode"`
	IPAM     struct {
		Type string `json:"type"`
	} `json:"ipam"`
}

// ParseNetworkAttachmentConfig reads a definition's spec.config, a single
// plugin or a plugin list.
func ParseNetworkAttachmentConfig(config string) NetworkAttachmentConfig {
	if config == "" {
		return NetworkAttachmentConfig{Problem: "the definition carries no configuration, so Multus reads the one " +
			"named after it from each node's CNI directory, which the API cannot be asked about"}
	}

	var doc struct {
		cniPlugin
		Plugins []cniPlugin `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(config), &doc); err != nil {
		return NetworkAttachmentConfig{Problem: fmt.Sprintf("the configuration does not parse: %v", err)}
	}

	plugin := doc.cniPlugin
	if len(doc.Plugins) > 0 {
		plugin = doc.Plugins[0]
	}
	if plugin.Type == "" {
		return NetworkAttachmentConfig{Problem: "the configuration names no plugin type"}
	}
	return NetworkAttachmentConfig{
		Type:          plugin.Type,
		HostInterface: cmp.Or(plugin.Master, plugin.Device, plugin.Bridge),
		PCIAddress:    plugin.PCIBusID,
		Mode:          plugin.Mode,
		IPAM:          plugin.IPAM.Type,
	}
}

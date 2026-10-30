// The attachment list of a storage node: what its SPDK pod attaches to beside
// the pod network, built from the cluster's declaration and the node's assigned
// addresses. The builder is pure and byte-stable, because a retried node-add
// has to send the same list.
//
// Specified by operator/docs/designs/design-storage-network.md §5.1.

package node

import (
	"encoding/json"
	"errors"
	"fmt"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// errNoAddress means a network attached through a definition has no address
// assigned to the node yet.
var errNoAddress = errors.New("the node has no assigned address")

const (
	// dataInterfacePrefix is what the data networks' interface names start with.
	// It is explicit so that no name depends on the order Multus assigns.
	dataInterfacePrefix = "data"

	// maxDataNetworks is the number of data networks a cluster may declare.
	maxDataNetworks = 8
)

// Attachment is one network the SPDK pod attaches to.
type Attachment struct {
	// Network is the name of the cluster network this attachment serves, which
	// is the key the node's addresses and status use. It is not a Multus field.
	Network string `json:"-"`

	// Name is the NetworkAttachmentDefinition's name, and Namespace the one it
	// is in.
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`

	// Interface is the interface's name inside the pod.
	Interface string `json:"interface"`

	// IPs are the addresses the pod takes on the interface, each with its prefix
	// length, which the static IPAM plugin reads from the network selection
	// element.
	IPs []string `json:"ips,omitempty"`
}

// assignedAddresses is the addresses the operator assigned a node, by network
// name, read from its status.
func assignedAddresses(status []simplyblockv1alpha2.NodeNetworkStatus) map[string]string {
	assigned := map[string]string{}
	for _, n := range status {
		if n.Assigned != "" {
			assigned[n.Network] = n.Assigned
		}
	}
	return assigned
}

// assignedResources is the device resource recorded for each network that has
// one, by network name.
func assignedResources(status []simplyblockv1alpha2.NodeNetworkStatus) map[string]string {
	resources := map[string]string{}
	for _, n := range status {
		if n.Resource != "" {
			resources[n.Network] = n.Resource
		}
	}
	return resources
}

// BuildAttachments turns the declaration and a node's assigned addresses into
// its attachment list: mgmt0 first, then data0 onward in the declared order. A
// host interface network has no element.
func BuildAttachments(
	namespace string, network *simplyblockv1alpha2.StorageNetworkSpec,
	assigned map[string]string,
) ([]Attachment, error) {
	if network == nil {
		return nil, nil
	}
	if len(network.Data) > maxDataNetworks {
		return nil, fmt.Errorf("%d data networks are declared and at most %d are attached",
			len(network.Data), maxDataNetworks)
	}

	var attachments []Attachment
	add := func(n *simplyblockv1alpha2.StorageNetwork, iface string) error {
		ref := &n.NetworkAttachmentRef
		if ref == nil {
			return nil
		}
		address, found := assigned[n.Name()]
		if !found {
			return fmt.Errorf("network %q: %w", n.Name(), errNoAddress)
		}
		attachments = append(attachments, Attachment{
			Network: n.Name(), Name: ref.Name, Namespace: namespace, Interface: iface,
			IPs: []string{address},
		})
		return nil
	}

	for i := range network.Data {
		if err := add(&network.Data[i], fmt.Sprintf("%s%d", dataInterfacePrefix, i)); err != nil {
			return nil, err
		}
	}
	return attachments, nil
}

// AnnotationValue is the attachments as the JSON list Multus reads from the
// pod's networks annotation.
func AnnotationValue(attachments []Attachment) (string, error) {
	encoded, err := json.Marshal(attachments)
	if err != nil {
		return "", fmt.Errorf("encoding the networks annotation: %w", err)
	}
	return string(encoded), nil
}

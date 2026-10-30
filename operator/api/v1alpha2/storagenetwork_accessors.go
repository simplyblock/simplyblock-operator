// Accessors of a data network's Multus reference. They live beside the types so
// that the node controller reads a network's key and pool one way.

package v1alpha2

// Name is the NetworkAttachmentDefinition's name, which is the key the nodes'
// addresses and status use for the network.
func (n StorageNetwork) Name() string { return n.NetworkAttachmentRef.Name }

// Pool is the range the node's address on the network is taken from, or nil for a
// network that takes the address its NIC holds on the worker.
func (n StorageNetwork) Pool() *NetworkAddressPool { return n.NetworkAttachmentRef.AddressPool }

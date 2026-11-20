// The identifiers a VLAN and a VXLAN carry, which sysfs does not publish.
//
// A VLAN and a VXLAN export the generic net-device attributes and a DEVTYPE,
// and nothing else: the tag of the one and the network identifier of the other
// are in the kernel's netlink answer for the link and nowhere in the tree. They
// are read through a seam for the same reason the addresses are. The netlink
// answer is scoped to the network namespace of the process asking, so a pod
// without the host's network would report its own links, and a fixture has no
// kernel behind it to ask.

package inventory

// VLANTag is the 802.1Q or 802.1ad tag a VLAN interface puts on its frames.
type VLANTag struct {
	// ID is the VLAN identifier, 0 to 4094.
	ID int

	// Protocol is the tag's ethertype as iproute2 spells it, `802.1Q` or
	// `802.1ad`, and is empty for an ethertype that is neither.
	Protocol string
}

// VXLANOverlay is what identifies the overlay a VXLAN interface belongs to.
type VXLANOverlay struct {
	// VNI is the VXLAN network identifier, 0 to 16777215.
	VNI int
}

// LinkIdentity is what netlink adds to one interface beyond what sysfs holds.
// At most one of its fields is set, and an interface that is neither a VLAN nor
// a VXLAN has neither.
type LinkIdentity struct {
	VLAN  *VLANTag
	VXLAN *VXLANOverlay
}

// LinkReader answers which tag or network identifier each interface carries,
// by interface name. It is the netlink counterpart of AddressReader and carries
// the same namespace caveat.
type LinkReader func() (map[string]LinkIdentity, error)

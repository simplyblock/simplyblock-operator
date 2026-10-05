// That a VLAN's tag and a VXLAN's network identifier reach the interfaces they
// belong to, and that nothing else is given one.
//
// sysfs carries neither: a VLAN and a VXLAN export the generic net-device
// attributes and a DEVTYPE, and the identifiers are only in the kernel's netlink
// answer. These tests therefore drive the reading through its link reader, the
// same seam the addresses come through, over the stacked host the kind tests
// read.

package inventory

import (
	"errors"
	"testing"
)

func TestTheTagAndTheNetworkIdentifierComeFromTheLinkReader(t *testing.T) {
	cfg := syntheticHost(stackedNetHost().write(t))
	cfg.InterfaceLinks = func() (map[string]LinkIdentity, error) {
		return map[string]LinkIdentity{
			"bond0.100":    {VLAN: &VLANTag{ID: 100, Protocol: "802.1Q"}},
			"vxlan.calico": {VXLAN: &VXLANOverlay{VNI: 4096}},
		}, nil
	}

	ifaces, err := ReadInterfaces(cfg)
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	vlan := byName(t, ifaces, "bond0.100")
	if vlan.VLAN == nil || vlan.VLAN.ID != 100 || vlan.VLAN.Protocol != "802.1Q" {
		t.Errorf("the VLAN carries the tag %+v, want 100 over 802.1Q", vlan.VLAN)
	}
	if vlan.VXLAN != nil {
		t.Errorf("the VLAN carries an overlay identity %+v", vlan.VXLAN)
	}

	overlay := byName(t, ifaces, "vxlan.calico")
	if overlay.VXLAN == nil || overlay.VXLAN.VNI != 4096 {
		t.Errorf("the overlay carries %+v, want VNI 4096", overlay.VXLAN)
	}
	if overlay.VLAN != nil {
		t.Errorf("the overlay carries a VLAN tag %+v", overlay.VLAN)
	}

	for _, name := range []string{"bond0", "eth0", "br0", "lo"} {
		if iface := byName(t, ifaces, name); iface.VLAN != nil || iface.VXLAN != nil {
			t.Errorf("%s carries a tag %+v or an overlay identity %+v it was never given",
				name, iface.VLAN, iface.VXLAN)
		}
	}
}

// A reader that fails costs the identifiers and nothing else, the same way a
// failed address read does: the kind and the stack still come from sysfs.
func TestAFailedLinkReadStillReportsTheInterfaces(t *testing.T) {
	cfg := syntheticHost(stackedNetHost().write(t))
	cfg.InterfaceLinks = func() (map[string]LinkIdentity, error) {
		return nil, errors.New("no permission to open a netlink socket")
	}

	ifaces, err := ReadInterfaces(cfg)
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	vlan := byName(t, ifaces, "bond0.100")
	if vlan.Kind != LinkVLAN {
		t.Errorf("the VLAN reads as kind %q without its tag, want %q", vlan.Kind, LinkVLAN)
	}
	if vlan.VLAN != nil {
		t.Errorf("a tag was invented: %+v", vlan.VLAN)
	}
}

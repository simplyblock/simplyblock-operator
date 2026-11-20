//go:build linux

// That LocalLinks reads the tag and the network identifier a real kernel holds.
//
// The test creates a VLAN and a VXLAN in a network namespace of its own, so it
// changes nothing on the machine running it, and it needs CAP_SYS_ADMIN for the
// namespace and CAP_NET_ADMIN for the links. Without them it is skipped: the
// fixture tests cover everything above the netlink call, and this is the one
// assertion that only a kernel can answer.

package inventory

import (
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestLocalLinksReadsTheTagAndTheNetworkIdentifierFromTheKernel(t *testing.T) {
	// A network namespace belongs to a thread, so the test stays on the one it
	// switches and switches it back before the thread is released.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	original, err := netns.Get()
	if err != nil {
		t.Fatalf("read the current network namespace: %v", err)
	}
	defer func() { _ = original.Close() }()

	scratch, err := netns.New()
	if err != nil {
		t.Skipf("no network namespace of our own (needs CAP_SYS_ADMIN): %v", err)
	}
	defer func() { _ = scratch.Close() }()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Fatalf("return to the original network namespace: %v", err)
		}
	}()

	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "probe0"}}
	if err := netlink.LinkAdd(parent); err != nil {
		t.Skipf("no link can be created here (needs CAP_NET_ADMIN): %v", err)
	}
	links := []netlink.Link{
		&netlink.Vlan{
			LinkAttrs:    netlink.LinkAttrs{Name: "probe0.777", ParentIndex: linkIndex(t, "probe0")},
			VlanId:       777,
			VlanProtocol: netlink.VLAN_PROTOCOL_8021AD,
		},
		&netlink.Vxlan{
			LinkAttrs: netlink.LinkAttrs{Name: "vxprobe"},
			VxlanId:   4242,
			Port:      4789,
		},
	}
	for _, link := range links {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatalf("create %s: %v", link.Attrs().Name, err)
		}
	}

	got, err := LocalLinks()
	if err != nil {
		t.Fatalf("read the links: %v", err)
	}

	if vlan := got["probe0.777"].VLAN; vlan == nil || *vlan != (VLANTag{ID: 777, Protocol: "802.1ad"}) {
		t.Errorf("the VLAN reads as %+v, want 777 over 802.1ad", vlan)
	}
	if vxlan := got["vxprobe"].VXLAN; vxlan == nil || vxlan.VNI != 4242 {
		t.Errorf("the overlay reads as %+v, want VNI 4242", vxlan)
	}
	for _, name := range []string{"probe0", "lo"} {
		if identity, found := got[name]; found {
			t.Errorf("%s, which is neither, reads as %+v", name, identity)
		}
	}
}

// linkIndex is the index of the named link in the current namespace.
func linkIndex(t *testing.T, name string) int {
	t.Helper()
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return link.Attrs().Index
}

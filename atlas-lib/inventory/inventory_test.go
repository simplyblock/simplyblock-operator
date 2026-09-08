// What Collect gathers, what it does when one of its readers fails, and how the
// per-machine readings are joined per memory node.
//
// Partial is the answer to a failure, and it is the whole reason Collect exists
// rather than callers calling the five readers themselves. A discovery run
// inspecting twenty workers reports what each one has; a worker whose CPU tree
// is unreadable still has disks and NICs worth reporting, and a Collect that
// returned nothing but the first error would hide them.

package inventory

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/blockdev"
)

// wholeHost merges the per-reader fixtures into the one tree a live host
// presents, which is what Collect walks.
func wholeHost() fixture {
	whole := fixture{files: map[string]string{}, links: map[string]string{}}
	for _, part := range []fixture{smtHost(), hugePageHost(), netHost(), diskHost()} {
		maps.Copy(whole.files, part.files)
		maps.Copy(whole.links, part.links)
		whole.dirs = append(whole.dirs, part.dirs...)
	}
	return whole
}

// collect runs a collection over a fixture with the disk seams a test needs.
func collect(t *testing.T, f fixture, prober *blockdev.Prober) (Inventory, error) {
	t.Helper()
	root := f.write(t)
	return Collect(context.Background(), Config{
		SysfsRoot: root,
		ProcRoot:  root,
		Prober:    prober,
		Exclusive: handsOverEveryDevice,
	})
}

func TestCollectFillsEveryReading(t *testing.T) {
	inv, err := collect(t, wholeHost(), blankDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	if inv.CPU.OnlineCount != 16 {
		t.Errorf("read %d online CPUs, want 16", inv.CPU.OnlineCount)
	}
	if len(inv.HugePages.Pools) != 2 {
		t.Errorf("read %d huge-page pools, want 2", len(inv.HugePages.Pools))
	}
	if len(inv.Interfaces) != 4 {
		t.Errorf("read %d interfaces, want 4", len(inv.Interfaces))
	}
	// Every block device, refused ones included: the boot disk and its
	// partition are part of what a worker has, and a reader has to be able to
	// say why they are not candidates.
	if len(inv.Devices) != 4 {
		t.Errorf("read %d block devices, want 4: %+v", len(inv.Devices), inv.Devices)
	}
}

func TestCollectFillsTheEnvironmentFromTheKubernetesSources(t *testing.T) {
	root := wholeHost().write(t)

	inv, err := Collect(context.Background(), Config{
		SysfsRoot: root,
		ProcRoot:  root,
		Prober:    blankDisks(),
		Exclusive: handsOverEveryDevice,
		Kubernetes: KubernetesSources{
			Nodes: []corev1.Node{node("worker-1", kubelet("v1.31.4+k3s1"))},
		},
	})
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	if inv.Environment.Distribution != DistributionK3s {
		t.Errorf("concluded %q, want %q", inv.Environment.Distribution, DistributionK3s)
	}
	if len(inv.Environment.Evidence) == 0 {
		t.Error("concluded a distribution and kept none of the evidence")
	}
	// The machine's own readings are unaffected by the cluster's.
	if inv.CPU.OnlineCount != 16 || len(inv.Devices) != 4 {
		t.Errorf("read %d CPUs and %d devices beside the environment, want 16 and 4",
			inv.CPU.OnlineCount, len(inv.Devices))
	}
}

func TestCollectLeavesTheEnvironmentUnsetWhenNothingWasAsked(t *testing.T) {
	// A caller inspecting a machine outside a cluster gets no distribution, and
	// an empty Distribution is not Vanilla: one means nothing was asked, and
	// the other means the cluster was read and carried no marker.
	inv, err := collect(t, wholeHost(), blankDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	if inv.Environment.Distribution != "" {
		t.Errorf("concluded %q with no Kubernetes sources given, want no conclusion",
			inv.Environment.Distribution)
	}
	if inv.Environment.Distribution == DistributionVanilla {
		t.Error("reported Vanilla for a cluster nothing looked at")
	}
}

func TestCollectAvailableDevicesIsTheFreeDisksAlone(t *testing.T) {
	inv, err := collect(t, wholeHost(), blankDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	free := inv.AvailableDevices()
	names := make([]string, 0, len(free))
	for _, c := range free {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"nvme0n1", "nvme1n1"}) {
		t.Errorf("the free disks are %v, want [nvme0n1 nvme1n1]: the boot disk "+
			"carries the root filesystem and its partition is a partition", names)
	}
}

func TestCollectReportsWhatItReadBesideWhatItCouldNot(t *testing.T) {
	f := wholeHost()
	// Strip the CPU topology, leaving the rest of the host intact.
	for path := range f.files {
		if strings.HasPrefix(path, "devices/system/cpu/") {
			delete(f.files, path)
		}
	}

	inv, err := collect(t, f, blankDisks())
	if err == nil {
		t.Fatal("collected an inventory from a host with no CPU topology without saying so")
	}
	if !strings.Contains(err.Error(), "CPU") {
		t.Errorf("the error is %q, which does not name what could not be read", err)
	}
	if len(inv.Interfaces) != 4 {
		t.Errorf("read %d interfaces beside the failure, want 4: a reader that "+
			"failed must not take the others down with it", len(inv.Interfaces))
	}
	if len(inv.HugePages.Pools) != 2 {
		t.Errorf("read %d huge-page pools beside the failure, want 2", len(inv.HugePages.Pools))
	}
	if len(inv.Devices) != 4 {
		t.Errorf("read %d block devices beside the failure, want 4", len(inv.Devices))
	}
}

func TestCollectReportsADeviceItCouldNotReadRatherThanDroppingIt(t *testing.T) {
	// A prober that cannot open anything is a host whose disks are all
	// unreadable. That is a finding per device and not a collection failure:
	// the CPUs, the huge pages, and the NICs are still the worker's inventory.
	inv, err := collect(t, wholeHost(), unreadableDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	if len(inv.Devices) != 4 {
		t.Fatalf("read %d block devices, want 4", len(inv.Devices))
	}
	if len(inv.AvailableDevices()) != 0 {
		t.Error("handed over a disk whose content could not be read")
	}
	for _, name := range []string{"nvme0n1", "nvme1n1"} {
		device := deviceNamed(t, inv, name)
		if !device.RejectedFor(blockdev.ReasonUnreadable) {
			t.Errorf("%s was rejected for %v, want %s", name, device.Rejections,
				blockdev.ReasonUnreadable)
		}
	}
	if inv.CPU.OnlineCount != 16 {
		t.Errorf("read %d online CPUs beside the unreadable disks, want 16", inv.CPU.OnlineCount)
	}
}

func TestByNUMANodeJoinsTheFourReadingsOnOneNode(t *testing.T) {
	// This is what the per-node readings are for. A storage node is pinned to a
	// socket, and it needs its cores, its huge pages, its NIC, and its disks
	// all on the same side of the interconnect; the join is what lets a caller
	// see whether that is possible before anything is deployed.
	inv, err := collect(t, wholeHost(), blankDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	// Two memory nodes, and a third entry for the bridge and loopback that hang
	// off no bus at all.
	nodes := inv.ByNUMANode()
	if len(nodes) != 3 {
		t.Fatalf("read %d entries, want 2 nodes and the unplaced one: %+v", len(nodes), nodes)
	}

	first := nodes[0]
	if first.Node != 0 {
		t.Errorf("the first node is %d, want 0", first.Node)
	}
	if len(first.CPUs.OnlineCPUs) != 8 || first.CPUs.PhysicalCores != 4 {
		t.Errorf("node 0 has %+v, want 8 CPUs over 4 cores", first.CPUs)
	}
	if len(first.HugePages) != 2 {
		t.Errorf("node 0 has %d huge-page entries, want one per size", len(first.HugePages))
	}
	if !hasInterface(first.Interfaces, "eth0") {
		t.Errorf("node 0's interfaces are %v, and the NIC in slot 0000:3b:00.0 is missing",
			interfaceNames(first.Interfaces))
	}
	if !hasDevice(first.Devices, "nvme0n1") {
		t.Errorf("node 0's devices are %v, and the disk in slot 0000:5e:00.0 is missing",
			deviceNames(first.Devices))
	}

	second := nodes[1]
	if second.Node != 1 {
		t.Errorf("the second node is %d, want 1", second.Node)
	}
	if !hasInterface(second.Interfaces, "eth1") {
		t.Errorf("node 1's interfaces are %v, want eth1 among them", interfaceNames(second.Interfaces))
	}
	if !hasDevice(second.Devices, "nvme1n1") {
		t.Errorf("node 1's devices are %v, want nvme1n1 among them", deviceNames(second.Devices))
	}
	// Nothing may be counted twice: a caller summing the nodes must not find
	// more capacity than the host has.
	if hasDevice(second.Devices, "nvme0n1") {
		t.Error("node 0's disk is also on node 1")
	}
}

func TestByNUMANodePlacesWhatItCannotPlaceUnderTheUnknownNode(t *testing.T) {
	// A bridge and loopback hang off no bus, so no node owns them. Dropping
	// them would make the join a silent filter, and a caller reading the
	// rollup as the whole inventory would be missing part of it.
	inv, err := collect(t, wholeHost(), blankDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	nodes := inv.ByNUMANode()
	unplaced := nodes[len(nodes)-1]
	if unplaced.Node != NUMANodeUnknown {
		t.Fatalf("the last entry is node %d, and the virtual interfaces belong "+
			"under node %d", unplaced.Node, NUMANodeUnknown)
	}
	for _, name := range []string{"cni0", "lo"} {
		if !hasInterface(unplaced.Interfaces, name) {
			t.Errorf("%s hangs off no bus and is not under the unknown node: %v",
				name, interfaceNames(unplaced.Interfaces))
		}
	}
	if len(unplaced.CPUs.OnlineCPUs) != 0 {
		t.Errorf("the unknown node owns CPUs %v, and every online CPU belongs to "+
			"a real node", unplaced.CPUs.OnlineCPUs)
	}
}

func TestByNUMANodeOmitsTheUnknownNodeWhenEverythingIsPlaced(t *testing.T) {
	// The extra entry is a finding, so it appears only when there is something
	// to report: a host whose every device and NIC sits on a known node has as
	// many entries as it has nodes.
	f := wholeHost()
	for _, virtual := range []string{"cni0", "lo"} {
		delete(f.links, "class/net/"+virtual)
	}

	inv, err := collect(t, f, blankDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	nodes := inv.ByNUMANode()
	if len(nodes) != 2 {
		t.Fatalf("read %d nodes, want 2: %+v", len(nodes), nodes)
	}
	for _, node := range nodes {
		if node.Node == NUMANodeUnknown {
			t.Error("reported an unknown node on a host where everything is placed")
		}
	}
}

func TestConfigDefaultsToTheLiveHostsRoots(t *testing.T) {
	var cfg Config
	if cfg.sysfs() != DefaultSysfsRoot {
		t.Errorf("an unset SysfsRoot resolves to %q, want %q", cfg.sysfs(), DefaultSysfsRoot)
	}
	if cfg.proc() != DefaultProcRoot {
		t.Errorf("an unset ProcRoot resolves to %q, want %q", cfg.proc(), DefaultProcRoot)
	}
	if cfg.dev() != blockdev.DefaultDevRoot {
		t.Errorf("an unset DevRoot resolves to %q, want %q", cfg.dev(), blockdev.DefaultDevRoot)
	}
}

// The finders below keep the assertions above about what is on a node rather
// than about how to look for it.

func deviceNamed(t *testing.T, inv Inventory, name string) blockdev.Candidate {
	t.Helper()
	for _, c := range inv.Devices {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s is missing from the inventory", name)
	return blockdev.Candidate{}
}

func hasInterface(ifaces []Interface, name string) bool {
	return slices.ContainsFunc(ifaces, func(i Interface) bool { return i.Name == name })
}

func hasDevice(devices []blockdev.Candidate, name string) bool {
	return slices.ContainsFunc(devices, func(c blockdev.Candidate) bool { return c.Name == name })
}

func interfaceNames(ifaces []Interface) []string {
	names := make([]string, 0, len(ifaces))
	for _, i := range ifaces {
		names = append(names, i.Name)
	}
	return names
}

func deviceNames(devices []blockdev.Candidate) []string {
	names := make([]string, 0, len(devices))
	for _, c := range devices {
		names = append(names, c.Name)
	}
	return names
}

// The builders every case is written with, and the objects a case directory
// holds.
//
// A case says one thing about a fleet and inherits the rest, so the builders
// default to a plausible machine and take options for the part that matters:
// two memory nodes, sixteen cores, a data NIC on each, which is the host this
// product was developed against. A case that is about a memory layout says only
// the layout, and one about a NIC says only the NIC.
//
// The objects are built through the same types the probe and the operator use,
// so a fixture that does not decode is a fixture that never gets written.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// Sizes a case names a disk or a machine by.
const (
	mb = uint64(1) << 20
	gb = uint64(1) << 30
	tb = uint64(1) << 40
)

// namespace is where every case's objects live, matching the chart's default.
const namespace = "simplyblock"

// probedAt is when every report says it was collected.
//
// It is fixed rather than taken from the clock so that regenerating the tree
// produces no diff: a timestamp that moved on every run would put 171 changed
// files in front of a reviewer looking for the one case that actually changed.
var probedAt = metav1.NewTime(time.Date(2026, time.September, 18, 9, 0, 0, 0, time.UTC))

// Case is one case's inputs.
type Case struct {
	// Family is the directory the case is filed under, and Slug names it
	// within that directory.
	Family string
	Slug   string

	// Gap is the finding this case records today's behavior for, when it
	// records one.
	Gap string

	// Note is anything a reader of the directory needs that the document's row
	// does not say.
	Note string

	// Reports are the workers the probes reported, and Nodes is what
	// Kubernetes says about the same machines. Nodes may be empty, which is a
	// run whose workers carry no node object.
	Reports []nodeprobe.Report
	Nodes   []corev1.Node

	// Workers overrides which workers the run settled on. Empty is every
	// worker a report was written for, which is what an ordinary run produces.
	Workers []string

	// Discover is the run's own configuration, and Environment is what the run
	// concluded the distribution is.
	Discover    *simplyblockv1alpha2.DiscoverSpec
	Environment simplyblockv1alpha2.KubernetesEnvironment

	// Amend rewrites the ConfigMaps after they are built, for a case that is
	// about the object rather than about the fleet in it.
	Amend func(run string, maps []*corev1.ConfigMap) []*corev1.ConfigMap
}

// Directory is the case's directory name: the identifier and a slug of the
// mutation, so that a failure names the case without anybody looking it up.
func (c Case) Directory(id string) string {
	return strings.ToLower(id) + "-" + c.Slug
}

// runName is what the run is called, and is both the OperatorOps name and the
// label its reports are selected by.
func runName(id string) string { return "discover-" + strings.ToLower(id) }

// operatorOpsFor is the run a case is driven from.
func operatorOpsFor(id string, c Case) *simplyblockv1alpha2.OperatorOps {
	workers := c.Workers
	if workers == nil {
		for _, report := range c.Reports {
			workers = append(workers, report.Node)
		}
	}

	environment := c.Environment
	if environment == "" {
		environment = simplyblockv1alpha2.KubernetesEnvironmentK3s
	}

	return &simplyblockv1alpha2.OperatorOps{
		TypeMeta: metav1.TypeMeta{
			APIVersion: simplyblockv1alpha2.GroupVersion.String(),
			Kind:       "OperatorOps",
		},
		ObjectMeta: metav1.ObjectMeta{Name: runName(id), Namespace: namespace},
		Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action:   simplyblockv1alpha2.OperatorOpsActionDiscover,
			Discover: c.Discover,
		},
		Status: simplyblockv1alpha2.OperatorOpsStatus{
			Workers:     workers,
			Environment: environment,
		},
	}
}

// reportConfigMap is the object a probe would have written for one report.
func reportConfigMap(id string, report nodeprobe.Report) (*corev1.ConfigMap, error) {
	cm, err := nodeprobe.ConfigMap(namespace, runName(id), nil, report)
	if err != nil {
		return nil, err
	}
	cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	return cm, nil
}

// --- reports ---------------------------------------------------------------

// hostOpt changes one thing about the default machine.
type hostOpt func(*nodeprobe.Report)

// host is a worker: two memory nodes, eight cores each with two threads, 256
// GiB of memory, sixteen 1 GiB huge pages free per node, and a 25G data NIC on
// each node holding a routable address.
//
// Everything a case does not name, it inherits from here.
func host(name string, opts ...hostOpt) nodeprobe.Report {
	report := nodeprobe.Report{
		Version:   nodeprobe.ReportVersion,
		Node:      name,
		ProbedAt:  probedAt,
		CPU:       cpuTopology(2, 8, 2),
		Memory:    memoryOf(256*gb, 240*gb, 128*gb, 128*gb),
		HugePages: []nodeprobe.HugePagePool{pagePool(gb, 16, 16)},
		Interfaces: []nodeprobe.Interface{
			nic("eth0", inventory.LinkPhysical, at(25000), on(0), holding(managementAddress(name))),
			nic("eth1", inventory.LinkPhysical, at(25000), on(1), holding(dataAddress(name))),
		},
	}
	for _, opt := range opts {
		opt(&report)
	}
	return report
}

// managementAddress and dataAddress are the two addresses a worker holds, keyed
// off the digits in its name so that a fleet's machines do not all claim one
// address and the node object can name the same one the report does.
func managementAddress(node string) string { return fmt.Sprintf("10.10.10.%d", hostNumber(node)) }
func dataAddress(node string) string       { return fmt.Sprintf("10.10.11.%d", hostNumber(node)) }

// hostNumber is the trailing number of a worker's name, and 1 for a name that
// ends in none.
func hostNumber(node string) int {
	digits := ""
	for i := len(node) - 1; i >= 0 && node[i] >= '0' && node[i] <= '9'; i-- {
		digits = string(node[i]) + digits
	}
	number, err := strconv.Atoi(digits)
	if err != nil || number == 0 {
		return 1
	}
	return number
}

// cpuTopology is a processor inventory of the shape given: one memory node per
// socket, with the CPU identifiers handed out node by node the way a kernel
// numbers them.
func cpuTopology(sockets, coresPerNode, threadsPerCore int) nodeprobe.CPU {
	cpu := nodeprobe.CPU{
		Sockets:        sockets,
		PhysicalCores:  sockets * coresPerNode,
		ThreadsPerCore: threadsPerCore,
		HyperThreading: threadsPerCore > 1,
		OnlineCPUs:     sockets * coresPerNode * threadsPerCore,
	}
	next := 0
	for node := 0; node < sockets; node++ {
		ids := make([]int, 0, coresPerNode*threadsPerCore)
		for i := 0; i < coresPerNode*threadsPerCore; i++ {
			ids = append(ids, next)
			next++
		}
		cpu.NUMANodes = append(cpu.NUMANodes, nodeprobe.NUMACPUs{
			Node: node, OnlineCPUs: ids, PhysicalCores: coresPerNode,
		})
	}
	return cpu
}

// `cpu` sets the processor inventory.
func cpu(sockets, coresPerNode, threadsPerCore int) hostOpt {
	return func(r *nodeprobe.Report) { r.CPU = cpuTopology(sockets, coresPerNode, threadsPerCore) }
}

// cpuNodes sets an asymmetric processor tree, for a machine whose memory nodes
// do not carry the same number of cores.
func cpuNodes(threadsPerCore int, coresPerNode ...int) hostOpt {
	return func(r *nodeprobe.Report) {
		out := nodeprobe.CPU{Sockets: len(coresPerNode), ThreadsPerCore: threadsPerCore,
			HyperThreading: threadsPerCore > 1}
		next := 0
		for node, cores := range coresPerNode {
			ids := make([]int, 0, cores*threadsPerCore)
			for i := 0; i < cores*threadsPerCore; i++ {
				ids = append(ids, next)
				next++
			}
			out.PhysicalCores += cores
			out.OnlineCPUs += len(ids)
			out.NUMANodes = append(out.NUMANodes, nodeprobe.NUMACPUs{
				Node: node, OnlineCPUs: ids, PhysicalCores: cores,
			})
		}
		r.CPU = out
	}
}

// noNUMATopology is a machine whose cores the probe read and could not attribute
// to a memory node.
func noNUMATopology(sockets, coresPerNode, threadsPerCore int) hostOpt {
	return func(r *nodeprobe.Report) {
		out := cpuTopology(sockets, coresPerNode, threadsPerCore)
		out.NUMANodes = nil
		r.CPU = out
	}
}

// noCPUTopology is a machine whose processor tree the probe could not read.
func noCPUTopology() hostOpt {
	return func(r *nodeprobe.Report) { r.CPU = nodeprobe.CPU{} }
}

// memoryOf is a memory reading, with the per-node split given.
func memoryOf(total, available uint64, perNode ...uint64) nodeprobe.Memory {
	out := nodeprobe.Memory{TotalBytes: total, AvailableBytes: available, FreeBytes: available / 4}
	for node, bytes := range perNode {
		out.NUMANodes = append(out.NUMANodes, nodeprobe.NUMAMemory{
			Node: node, TotalBytes: bytes, FreeBytes: bytes / 2,
		})
	}
	return out
}

// mem sets the machine's memory.
func mem(total, available uint64, perNode ...uint64) hostOpt {
	return func(r *nodeprobe.Report) { r.Memory = memoryOf(total, available, perNode...) }
}

// reserved records memory already held as huge pages, which meminfo reports
// separately because it is out of the general pool.
func reserved(bytes uint64) hostOpt {
	return func(r *nodeprobe.Report) { r.Memory.HugePagesBytes = bytes }
}

// swap records the swap a host has and how much of it is gone.
func swap(total, free uint64) hostOpt {
	return func(r *nodeprobe.Report) { r.Memory.SwapTotalBytes, r.Memory.SwapFreeBytes = total, free }
}

// pagePool is one huge-page size's allocation, with the per-node split given as
// the pages free on each. Every page is counted as free, which is a host whose
// reservation nothing has taken yet.
func pagePool(size uint64, freePerNode ...uint64) nodeprobe.HugePagePool {
	pool := nodeprobe.HugePagePool{SizeBytes: size}
	for node, free := range freePerNode {
		pool.Total += free
		pool.Free += free
		pool.NUMANodes = append(pool.NUMANodes, nodeprobe.NUMAHugePages{
			Node: node, Total: free, Free: free,
		})
	}
	return pool
}

// pages sets the huge-page pools.
func pages(pools ...nodeprobe.HugePagePool) hostOpt {
	return func(r *nodeprobe.Report) { r.HugePages = pools }
}

// noPages is a host with nothing set aside, which is every host before anybody
// prepared it.
func noPages() hostOpt {
	return func(r *nodeprobe.Report) { r.HugePages = nil }
}

// ifaces replaces the machine's network interfaces.
func ifaces(list ...nodeprobe.Interface) hostOpt {
	return func(r *nodeprobe.Report) { r.Interfaces = list }
}

// disks sets the machine's block devices.
func disks(list ...nodeprobe.Device) hostOpt {
	return func(r *nodeprobe.Report) { r.Devices = list }
}

// controllers sets the NVMe controllers on the machine's PCI bus, which is what
// tells a reader about disks the kernel presents no block device for.
func controllers(list ...nodeprobe.Controller) hostOpt {
	return func(r *nodeprobe.Report) { r.NVMeControllers = list }
}

// unreadable records what the probe could not read.
func unreadable(sentences ...string) hostOpt {
	return func(r *nodeprobe.Report) { r.Unreadable = sentences }
}

// --- interfaces ------------------------------------------------------------

// ifaceOpt changes one thing about an interface.
type ifaceOpt func(*nodeprobe.Interface)

// `nic` is one network interface of the kind given, up and on memory node 0
// unless a case says otherwise.
func nic(name string, kind inventory.LinkKind, opts ...ifaceOpt) nodeprobe.Interface {
	iface := nodeprobe.Interface{
		Name:     name,
		Kind:     string(kind),
		State:    "up",
		MTU:      1500,
		NUMANode: inventory.NUMANodeUnknown,
		Virtual:  kind != inventory.LinkPhysical,
		Bridge:   kind == inventory.LinkBridge,
		Loopback: kind == inventory.LinkLoopback,
	}
	if kind == inventory.LinkPhysical {
		iface.NUMANode = 0
		iface.Driver = "mlx5_core"
	}
	for _, opt := range opts {
		opt(&iface)
	}
	return iface
}

// at is the interface's negotiated link speed in megabits per second.
func at(speed int) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.SpeedMbps = speed }
}

// holding is the addresses the interface carries.
func holding(addresses ...string) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.Addresses = addresses }
}

// on is the memory node the interface's hardware hangs off.
func on(node int) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.NUMANode = node }
}

// linkState overrides the kernel's operstate for the interface.
func linkState(state string) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.State = state }
}

// over is what the interface is built on: the members of a bond or a bridge, or
// the parent of a VLAN.
func over(lower ...string) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.Lower = lower }
}

// under is what is built on the interface.
func under(upper ...string) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.Upper = upper }
}

// frames is the interface's MTU, for a case about jumbo frames.
func frames(mtu int) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.MTU = mtu }
}

// slotted is the PCI address of the interface's hardware.
func slotted(address string) ifaceOpt {
	return func(i *nodeprobe.Interface) { i.PCIAddress = address }
}

// unkinded strips the kind, which is a probe that could not read the device
// type rather than an older schema.
func unkinded() ifaceOpt {
	return func(i *nodeprobe.Interface) { i.Kind = "" }
}

// --- devices ---------------------------------------------------------------

// devOpt changes one thing about a block device.
type devOpt func(*nodeprobe.Device)

// `nvme` is one free NVMe disk in a slot, on a memory node.
func nvme(name, address string, node int, size uint64, opts ...devOpt) nodeprobe.Device {
	return device(nodeprobe.Device{
		Name:       name,
		Path:       "/dev/" + name,
		PCIAddress: address,
		SizeBytes:  size,
		Kind:       string(blockdev.KindDisk),
		Transport:  string(blockdev.TransportNVMe),
		Model:      "SAMSUNG MZQL23T8HCLS-00A07",
		NUMANode:   node,
		Available:  true,
		Content:    "Blank",
	}, opts...)
}

// blk is one free disk of the other class: a virtio disk with a path and no PCI
// address a draft could name it by.
func blk(name string, node int, size uint64, opts ...devOpt) nodeprobe.Device {
	return device(nodeprobe.Device{
		Name:      name,
		Path:      "/dev/" + name,
		SizeBytes: size,
		Kind:      string(blockdev.KindDisk),
		Transport: string(blockdev.TransportVirtio),
		NUMANode:  node,
		Available: true,
		Content:   "Blank",
	}, opts...)
}

// device applies a device's options.
func device(d nodeprobe.Device, opts ...devOpt) nodeprobe.Device {
	for _, opt := range opts {
		opt(&d)
	}
	return d
}

// refused is a device the probe declined, on the grounds given.
func refused(reasons ...blockdev.Reason) devOpt {
	return func(d *nodeprobe.Device) {
		d.Available = false
		d.Content = ""
		for _, reason := range reasons {
			d.Rejections = append(d.Rejections, nodeprobe.Rejection{
				Reason: string(reason), Detail: "as the probe found it",
			})
		}
	}
}

// modeled overrides the device's model string.
func modeled(model string) devOpt {
	return func(d *nodeprobe.Device) { d.Model = model }
}

// spinning marks the device a rotational one.
func spinning() devOpt {
	return func(d *nodeprobe.Device) { d.Rotational = true; d.Model = "SEAGATE ST16000NM" }
}

// partOf makes the device a partition rather than a whole disk.
func partOf() devOpt {
	return func(d *nodeprobe.Device) { d.Kind = string(blockdev.KindPartition) }
}

// looped makes the device a loopback device, which is what a machine presents
// dozens of and a draft can use none of.
func looped() devOpt {
	return func(d *nodeprobe.Device) {
		d.Kind = string(blockdev.KindLoop)
		d.Transport = ""
		d.PCIAddress = ""
	}
}

// transported overrides the bus the device sits on.
func transported(transport blockdev.Transport) devOpt {
	return func(d *nodeprobe.Device) { d.Transport = string(transport) }
}

// controller is one NVMe controller on the PCI bus, bound to the driver given.
func controller(address, driver string, node int, opts ...func(*nodeprobe.Controller)) nodeprobe.Controller {
	c := nodeprobe.Controller{
		Address: address, Driver: driver, NUMANode: node,
		Vendor: "0x144d", Product: "0xa80a",
		// Checked and found free, which is the state a draft may claim.
		InUse: ptr.To(false),
	}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// held marks a controller something is driving, which is a disk in service
// rather than one to reclaim.
func held() func(*nodeprobe.Controller) {
	return func(c *nodeprobe.Controller) { c.InUse = ptr.To(true) }
}

// unchecked is a controller the probe could not ask about, which is neither
// held nor free.
func unchecked() func(*nodeprobe.Controller) {
	return func(c *nodeprobe.Controller) { c.InUse = nil }
}

// --- Kubernetes nodes ------------------------------------------------------

// nodeOpt changes one thing about a node object.
type nodeOpt func(*corev1.Node)

// kubeNode is what Kubernetes says about a worker: 32 cores and 256 GiB, of
// which the kubelet holds some back.
func kubeNode(name string, opts ...nodeOpt) corev1.Node {
	node := corev1.Node{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("32"),
				corev1.ResourceMemory: resource.MustParse("256Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("31500m"),
				corev1.ResourceMemory: resource.MustParse("250Gi"),
			},
		},
	}
	for _, opt := range opts {
		opt(&node)
	}
	return node
}

// role labels the node with what it is for, in Kubernetes' own convention.
func role(name string) nodeOpt {
	return func(n *corev1.Node) { n.Labels["node-role.kubernetes.io/"+name] = "" }
}

// labeled puts one label on the node.
func labeled(key, value string) nodeOpt {
	return func(n *corev1.Node) { n.Labels[key] = value }
}

// reachableAt is the address the cluster reaches the machine on.
func reachableAt(address string) nodeOpt {
	return func(n *corev1.Node) {
		n.Status.Addresses = append(n.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: address})
	}
}

// sized overrides what the kubelet found and what it will schedule against.
func sized(capacityCPU, capacityMemory, allocatableCPU, allocatableMemory string) nodeOpt {
	return func(n *corev1.Node) {
		n.Status.Capacity[corev1.ResourceCPU] = resource.MustParse(capacityCPU)
		n.Status.Capacity[corev1.ResourceMemory] = resource.MustParse(capacityMemory)
		n.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse(allocatableCPU)
		n.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse(allocatableMemory)
	}
}

// schedulableHugePages is a huge-page size the kubelet will schedule against.
func schedulableHugePages(size, quantity string) nodeOpt {
	return func(n *corev1.Node) {
		name := corev1.ResourceName("hugepages-" + size)
		n.Status.Capacity[name] = resource.MustParse(quantity)
		n.Status.Allocatable[name] = resource.MustParse(quantity)
	}
}

// cordoned marks the node unschedulable.
func cordoned() nodeOpt {
	return func(n *corev1.Node) { n.Spec.Unschedulable = true }
}

// tainted puts a taint on the node.
func tainted(key, value string, effect corev1.TaintEffect) nodeOpt {
	return func(n *corev1.Node) {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: key, Value: value, Effect: effect})
	}
}

// --- fleets ----------------------------------------------------------------

// fleet is n workers named worker-01 upward, each built by the function given.
//
// The names are padded because the draft's groups are numbered by their first
// worker's name and the ordering is lexicographic: worker-10 sorts before
// worker-2, and a fixture that made a reviewer work that out would be a fixture
// about nothing.
func fleet(n int, build func(index int, name string) nodeprobe.Report) []nodeprobe.Report {
	reports := make([]nodeprobe.Report, 0, n)
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("worker-%02d", i)
		reports = append(reports, build(i, name))
	}
	return reports
}

// kubeFleet is the node objects for a fleet, with each worker reachable on its
// own address.
func kubeFleet(reports []nodeprobe.Report, opts ...nodeOpt) []corev1.Node {
	nodes := make([]corev1.Node, 0, len(reports))
	for _, report := range reports {
		all := append([]nodeOpt{reachableAt(managementAddress(report.Node))}, opts...)
		nodes = append(nodes, kubeNode(report.Node, all...))
	}
	return nodes
}

// --- writing ---------------------------------------------------------------

// writeYAML writes one object.
func writeYAML(path string, object any) error {
	out, err := yaml.Marshal(object)
	if err != nil {
		return fmt.Errorf("render %s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o644)
}

// writeDocuments writes a list of objects as a multi-document file, which is
// what a reviewer expects of a file called nodes.yaml.
func writeDocuments[T any](path string, objects []T) error {
	var out strings.Builder
	for i := range objects {
		rendered, err := yaml.Marshal(objects[i])
		if err != nil {
			return fmt.Errorf("render %s: %w", path, err)
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(rendered)
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}

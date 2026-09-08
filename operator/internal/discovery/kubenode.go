// What Kubernetes thinks a worker has, which is not what the machine has.
//
// The probe reads the kernel and gets the truth about the hardware. The kubelet
// reports something different and equally real: capacity is what it found, and
// allocatable is what it will let a pod have after kube-reserved,
// system-reserved, and the eviction threshold come off the top. The difference
// between the two is the memory a storage node will never be scheduled against
// however much the machine has, and it is invisible from the host.
//
// Huge pages appear here too, and differently. The kernel reports them per size
// and per NUMA node; the kubelet reports them as schedulable resources named
// hugepages-2Mi and hugepages-1Gi, and a pod that asks for huge pages is
// scheduled against those. A node with 12 GiB reserved in the kernel and
// hugepages-2Mi missing from allocatable is a node where the reservation
// happened after the kubelet last looked.
//
// This is read from the Node object by the operator rather than by the probe.
// The probe holds two verbs on one kind by design, and giving it read access to
// nodes to fetch something the operator has already listed would spend that for
// nothing.

package discovery

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// KubeNode is one worker as Kubernetes describes it.
type KubeNode struct {
	// Name is the node's name, which is what a report and a NodeGroup name it
	// by.
	Name string

	// CapacityMemoryBytes is what the kubelet found, and
	// AllocatableMemoryBytes is what it will schedule against. The gap is the
	// reservations.
	CapacityMemoryBytes    int64
	AllocatableMemoryBytes int64

	// CapacityCPU and AllocatableCPU are the same for processors, in
	// milli-CPU: 32000 is thirty-two cores.
	CapacityCPUMilli    int64
	AllocatableCPUMilli int64

	// HugePages is the schedulable huge-page resources, ascending by page size.
	HugePages []KubeHugePages

	// Unschedulable reports whether the node is cordoned, and Taints are the
	// taints it carries, both recorded so that a draft can say why a worker
	// that looked fine was not used.
	Unschedulable bool
	Taints        []string
}

// ReservedMemoryBytes is capacity less allocatable: the memory the kubelet
// holds back for the system and for eviction headroom.
//
// It is a derived number rather than a reported one, because Kubernetes reports
// no such field: what it publishes is the two ends, and the reservation is the
// distance between them.
func (n KubeNode) ReservedMemoryBytes() int64 {
	if n.AllocatableMemoryBytes > n.CapacityMemoryBytes {
		return 0
	}
	return n.CapacityMemoryBytes - n.AllocatableMemoryBytes
}

// KubeHugePages is one huge-page size as a schedulable resource.
type KubeHugePages struct {
	// SizeBytes is the page size the resource name encodes: hugepages-2Mi is
	// 2 MiB.
	SizeBytes int64

	// CapacityBytes and AllocatableBytes are the totals for that size.
	CapacityBytes    int64
	AllocatableBytes int64
}

// hugePagesResourcePrefix is what Kubernetes names a huge-page resource with.
const hugePagesResourcePrefix = "hugepages-"

// KubeNodeOf reads what Kubernetes says about one worker.
func KubeNodeOf(node corev1.Node) KubeNode {
	out := KubeNode{
		Name:                   node.Name,
		CapacityMemoryBytes:    quantityValue(node.Status.Capacity, corev1.ResourceMemory),
		AllocatableMemoryBytes: quantityValue(node.Status.Allocatable, corev1.ResourceMemory),
		CapacityCPUMilli:       milliValue(node.Status.Capacity, corev1.ResourceCPU),
		AllocatableCPUMilli:    milliValue(node.Status.Allocatable, corev1.ResourceCPU),
		Unschedulable:          node.Spec.Unschedulable,
	}

	for _, taint := range node.Spec.Taints {
		out.Taints = append(out.Taints, fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect))
	}

	bySize := map[int64]*KubeHugePages{}
	for name, quantity := range node.Status.Capacity {
		size, ok := hugePageSizeOf(string(name))
		if !ok {
			continue
		}
		bySize[size] = &KubeHugePages{SizeBytes: size, CapacityBytes: quantity.Value()}
	}
	for name, quantity := range node.Status.Allocatable {
		size, ok := hugePageSizeOf(string(name))
		if !ok {
			continue
		}
		if entry, seen := bySize[size]; seen {
			entry.AllocatableBytes = quantity.Value()
			continue
		}
		bySize[size] = &KubeHugePages{SizeBytes: size, AllocatableBytes: quantity.Value()}
	}

	for _, entry := range bySize {
		out.HugePages = append(out.HugePages, *entry)
	}
	slices.SortFunc(out.HugePages, func(a, b KubeHugePages) int {
		return cmp.Compare(a.SizeBytes, b.SizeBytes)
	})
	return out
}

// KubeNodesOf reads a list of them, keyed by name for the planner to look up.
func KubeNodesOf(nodes []corev1.Node) map[string]KubeNode {
	out := make(map[string]KubeNode, len(nodes))
	for _, node := range nodes {
		out[node.Name] = KubeNodeOf(node)
	}
	return out
}

// hugePageSizeOf reads the page size out of a resource name such as
// hugepages-2Mi, and reports whether the name was one at all.
//
// The suffix is a Kubernetes quantity, so it is parsed as one rather than by
// matching the two sizes x86 has: an architecture with 16 KiB or 512 MiB pages
// names them the same way, and a reader that knew only 2Mi and 1Gi would report
// a machine's huge pages as none.
func hugePageSizeOf(name string) (int64, bool) {
	suffix, isHugePages := strings.CutPrefix(name, hugePagesResourcePrefix)
	if !isHugePages {
		return 0, false
	}
	size, err := resource.ParseQuantity(suffix)
	if err != nil {
		return 0, false
	}
	return size.Value(), true
}

// quantityValue reads one resource in its base unit, or zero when the node does
// not report it.
func quantityValue(list corev1.ResourceList, name corev1.ResourceName) int64 {
	quantity, reported := list[name]
	if !reported {
		return 0
	}
	return quantity.Value()
}

// milliValue reads one resource in thousandths, which is how a CPU count is
// expressed.
func milliValue(list corev1.ResourceList, name corev1.ResourceName) int64 {
	quantity, reported := list[name]
	if !reported {
		return 0
	}
	return quantity.MilliValue()
}

// What the Kubernetes reading says, and the gap it exists to expose.
//
// The number nobody can get from the host is the reservation: the kubelet holds
// memory back for the system and for eviction headroom, publishes only the two
// ends, and a storage node is scheduled against the lower one however much RAM
// the machine has.

package discovery

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// kubeWorker is a node the kubelet has published capacity and allocatable for,
// with a reservation between them and both huge-page sizes.
func kubeWorker(name string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("32"),
				corev1.ResourceMemory: resource.MustParse("256Gi"),
				"hugepages-2Mi":       resource.MustParse("8Gi"),
				"hugepages-1Gi":       resource.MustParse("16Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("31500m"),
				corev1.ResourceMemory: resource.MustParse("250Gi"),
				"hugepages-2Mi":       resource.MustParse("8Gi"),
				"hugepages-1Gi":       resource.MustParse("16Gi"),
			},
		},
	}
}

func TestKubeNodeOfExposesTheReservationNoHostReadingShows(t *testing.T) {
	node := KubeNodeOf(kubeWorker("worker-1"))

	if node.CapacityMemoryBytes != 256<<30 {
		t.Errorf("read capacity %d, want %d", node.CapacityMemoryBytes, int64(256)<<30)
	}
	if node.AllocatableMemoryBytes != 250<<30 {
		t.Errorf("read allocatable %d, want %d", node.AllocatableMemoryBytes, int64(250)<<30)
	}
	// The whole point: 6 GiB the machine has and a pod will never be given.
	if node.ReservedMemoryBytes() != 6<<30 {
		t.Errorf("read a reservation of %d, want %d: it is capacity less allocatable "+
			"and Kubernetes publishes no field for it",
			node.ReservedMemoryBytes(), int64(6)<<30)
	}
	if node.CapacityCPUMilli != 32000 || node.AllocatableCPUMilli != 31500 {
		t.Errorf("read CPU %d of %d milli", node.AllocatableCPUMilli, node.CapacityCPUMilli)
	}
}

func TestKubeNodeOfReadsHugePagesAsSchedulableResources(t *testing.T) {
	// The kernel reports huge pages per size and per NUMA node; the kubelet
	// reports them as resources a pod is scheduled against. Both are needed and
	// they can disagree.
	node := KubeNodeOf(kubeWorker("worker-1"))

	if len(node.HugePages) != 2 {
		t.Fatalf("read %d huge-page resources, want 2: %+v", len(node.HugePages), node.HugePages)
	}
	// Ascending by page size, so two readings of one node line up.
	if node.HugePages[0].SizeBytes != 2<<20 || node.HugePages[1].SizeBytes != 1<<30 {
		t.Errorf("read sizes %d and %d, want 2 MiB then 1 GiB",
			node.HugePages[0].SizeBytes, node.HugePages[1].SizeBytes)
	}
	if node.HugePages[1].CapacityBytes != 16<<30 || node.HugePages[1].AllocatableBytes != 16<<30 {
		t.Errorf("the 1 GiB resource is %+v", node.HugePages[1])
	}
}

func TestKubeNodeOfParsesAnyHugePageSizeRatherThanTheTwoOnX86(t *testing.T) {
	// An architecture with 16 KiB or 512 MiB pages names the resource the same
	// way, and a reader that knew only 2Mi and 1Gi would report such a
	// machine's huge pages as none.
	node := kubeWorker("worker-1")
	node.Status.Capacity = corev1.ResourceList{"hugepages-512Mi": resource.MustParse("4Gi")}
	node.Status.Allocatable = corev1.ResourceList{"hugepages-512Mi": resource.MustParse("4Gi")}

	read := KubeNodeOf(node)

	if len(read.HugePages) != 1 {
		t.Fatalf("read %+v", read.HugePages)
	}
	if read.HugePages[0].SizeBytes != 512<<20 {
		t.Errorf("read a page size of %d, want %d", read.HugePages[0].SizeBytes, int64(512)<<20)
	}
}

func TestKubeNodeOfRecordsWhyANodeMightNotBeUsed(t *testing.T) {
	node := kubeWorker("worker-1")
	node.Spec.Unschedulable = true
	node.Spec.Taints = []corev1.Taint{{
		Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule,
	}}

	read := KubeNodeOf(node)

	if !read.Unschedulable {
		t.Error("a cordoned node was not recorded as one")
	}
	if len(read.Taints) != 1 || read.Taints[0] == "" {
		t.Errorf("the taints are %v", read.Taints)
	}
}

func TestKubeNodeOfSurvivesANodeThatPublishesNothing(t *testing.T) {
	// A node that has not reported yet, which is what a machine looks like
	// between joining and its first kubelet status.
	read := KubeNodeOf(corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	if read.Name != "worker-1" {
		t.Errorf("lost the name: %+v", read)
	}
	if read.CapacityMemoryBytes != 0 || read.ReservedMemoryBytes() != 0 {
		t.Errorf("invented %d bytes of capacity for a node that published none",
			read.CapacityMemoryBytes)
	}
}

func TestKubeNodeOfNeverReportsANegativeReservation(t *testing.T) {
	// Allocatable above capacity is not a state a kubelet should publish, and
	// the subtraction underflowing into an enormous unsigned reservation would
	// be a worse answer than zero.
	node := kubeWorker("worker-1")
	node.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse("300Gi")

	if reserved := KubeNodeOf(node).ReservedMemoryBytes(); reserved != 0 {
		t.Errorf("read a reservation of %d from allocatable above capacity", reserved)
	}
}

func TestPlanAttachesTheKubernetesViewToEachWorker(t *testing.T) {
	// The planner takes them optionally: a plan built without them still works,
	// and one built with them lets a rule or a placement reach the numbers the
	// host cannot see.
	plan := Planner{
		KubeNodes: KubeNodesOf([]corev1.Node{kubeWorker("worker-1"), kubeWorker("worker-2")}),
	}.Plan(uniformFleet(2), nil)

	if len(plan.Workers) != 2 {
		t.Fatalf("planned %d workers", len(plan.Workers))
	}
	for _, worker := range plan.Workers {
		if worker.Kube.Name != worker.Name {
			t.Errorf("%s carries the Kubernetes view of %q", worker.Name, worker.Kube.Name)
		}
		if worker.Kube.ReservedMemoryBytes() != 6<<30 {
			t.Errorf("%s reports a reservation of %d", worker.Name, worker.Kube.ReservedMemoryBytes())
		}
	}

	// Without them the workers carry a zero value rather than the planner
	// failing, because the reading is a description and not a requirement.
	bare := Planner{}.Plan(uniformFleet(1), nil)
	if bare.Workers[0].Kube.Name != "" {
		t.Errorf("invented a Kubernetes view: %+v", bare.Workers[0].Kube)
	}
}

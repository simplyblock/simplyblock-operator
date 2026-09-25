// What the StorageNode conversion has to get right, beyond the mechanical
// assignment the round trip in hub_roundtrip_test.go covers.
//
// Four rows of design-storagenode.md §15.1 need more than a copy, and each has a
// test here because each is a place the two shapes genuinely disagree: the parent
// that is not on the stored object, the sizing the stored shape has no field for,
// the failure domain that changes type, and the device summary whose stored order
// is not the order its own documentation claimed.

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A node stored as v1alpha1 names a StorageNodeSet and nothing else, so the
// cluster the hub requires comes from the controller owner reference the
// upgrade's reparent step puts there.
func TestStorageNodeReadsItsClusterFromTheControllerOwner(t *testing.T) {
	stored := &StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production-7f3a9c",
			Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StorageNodeSet", Name: "rack-a", Controller: ptr.To(false)},
				{Kind: "StorageCluster", Name: testCluster, Controller: ptr.To(true)},
			},
		},
		Spec: StorageNodeSpec{StorageNodeSetRef: "rack-a", WorkerNode: "worker-3"},
	}

	var hub v1alpha2.StorageNode
	if err := stored.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if hub.Spec.ClusterRef != testCluster {
		t.Errorf("spec.clusterRef = %q, want the controlling StorageCluster %q",
			hub.Spec.ClusterRef, testCluster)
	}
	// The set's name survives as a label rather than as a reference, so a node can
	// still be traced back to the document that produced it.
	if hub.Spec.NodeSet != "rack-a" {
		t.Errorf("spec.nodeSet = %q, want the set the node was declared under", hub.Spec.NodeSet)
	}
}

// A node that has not been reparented yet converts with an empty cluster rather
// than failing. The object stays readable, which is what a read during an upgrade
// needs, and the field is Required so the next write of it is refused until the
// reparent has run.
func TestStorageNodeWithNoControllerConvertsWithNoCluster(t *testing.T) {
	stored := &StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "production-7f3a9c", Namespace: "simplyblock"},
		Spec:       StorageNodeSpec{StorageNodeSetRef: "rack-a", WorkerNode: "worker-3"},
	}

	var hub v1alpha2.StorageNode
	if err := stored.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if hub.Spec.ClusterRef != "" {
		t.Errorf("spec.clusterRef = %q, want it empty for a node nothing has reparented",
			hub.Spec.ClusterRef)
	}
}

// The stored device summary is total/online, which is the order both call sites
// rendered it in against a doc comment claiming the reverse. Reading it the
// documented way would report every degraded node as having more devices online
// than it has.
func TestStorageNodeDeviceSummaryIsReadInTheOrderItWasWritten(t *testing.T) {
	stored := &StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "sb"},
		Status: StorageNodeStatus{
			Resources: &StorageNodeResources{Devices: "4/3"},
		},
	}

	var hub v1alpha2.StorageNode
	if err := stored.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	want := &v1alpha2.StorageNodeDevices{Online: 3, Total: 4}
	if diff := cmp.Diff(want, hub.Status.Resources.Devices); diff != "" {
		t.Errorf("the device summary was read wrongly (-want +got):\n%s", diff)
	}
}

// A summary that is not two numbers is an absent block rather than two zeroes. A
// node the control plane has never reported on and one that genuinely has no
// devices are different answers, and the absent parent is how the hub tells them
// apart.
func TestStorageNodeUnparsableDeviceSummaryHasNoBlock(t *testing.T) {
	for _, summary := range []string{"", "unknown", "4", "4/x"} {
		stored := &StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "sb"},
			Status: StorageNodeStatus{
				Resources: &StorageNodeResources{Devices: summary},
			},
		}
		var hub v1alpha2.StorageNode
		if err := stored.ConvertTo(&hub); err != nil {
			t.Fatalf("ConvertTo(%q): %v", summary, err)
		}
		if hub.Status.Resources.Devices != nil {
			t.Errorf("summary %q produced %+v, want no device block",
				summary, hub.Status.Resources.Devices)
		}
	}
}

// The failure domain was an index on both the spec and the status and is a label
// on both here. The digits are what a mechanical conversion can carry, and a label
// that is not a number has nowhere to go on the way down, so it is stashed.
func TestStorageNodeFailureDomainLabelSurvivesTheTripDown(t *testing.T) {
	hub := &v1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "sb"},
		Spec: v1alpha2.StorageNodeSpec{
			ClusterRef: testCluster,
			WorkerNode: "worker-3",
			Config: v1alpha2.StorageNodeConfig{
				Sizing:        v1alpha2.StorageNodeSizing{VCPUCount: ptr.To(int32(8))},
				FailureDomain: "rack-b",
			},
		},
		Status: v1alpha2.StorageNodeStatus{FailureDomain: "rack-b"},
	}

	var stored StorageNode
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	// The int32 field cannot hold a label, so it stays absent rather than holding
	// a number nobody wrote.
	if stored.Spec.Overrides.FailureDomain != nil {
		t.Errorf("spec.overrides.failureDomain = %v, want it absent for a label",
			*stored.Spec.Overrides.FailureDomain)
	}

	var back v1alpha2.StorageNode
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if back.Spec.Config.FailureDomain != "rack-b" {
		t.Errorf("spec.config.failureDomain = %q, want the label back",
			back.Spec.Config.FailureDomain)
	}
	if back.Status.FailureDomain != "rack-b" {
		t.Errorf("status.failureDomain = %q, want the label back", back.Status.FailureDomain)
	}
}

// An index a real v1alpha1 object holds converts up to its digits, which is a
// valid label value and the only thing a mechanical conversion can say about it.
func TestStorageNodeFailureDomainIndexBecomesItsDigits(t *testing.T) {
	stored := &StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "sb"},
		Spec: StorageNodeSpec{
			WorkerNode: "worker-3",
			Overrides:  &StorageNodeOverrides{FailureDomain: ptr.To(int32(1))},
		},
		Status: StorageNodeStatus{FailureDomain: ptr.To(int32(1))},
	}

	var hub v1alpha2.StorageNode
	if err := stored.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if hub.Spec.Config.FailureDomain != "1" {
		t.Errorf("spec.config.failureDomain = %q, want the index's digits",
			hub.Spec.Config.FailureDomain)
	}
	if hub.Status.FailureDomain != "1" {
		t.Errorf("status.failureDomain = %q, want the index's digits", hub.Status.FailureDomain)
	}
}

// The four per-node fields that reached nothing move to the cluster, and they
// stash on the way up so a node converted back down carries what it carried.
func TestStorageNodeDeadPerNodeFieldsSurviveTheRoundTrip(t *testing.T) {
	stored := &StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "sb"},
		Spec: StorageNodeSpec{
			WorkerNode: "worker-3",
			Overrides: &StorageNodeOverrides{
				UbuntuHost:               ptr.To(true),
				SkipKubeletConfiguration: ptr.To(true),
				EnableCpuTopology:        ptr.To(true),
				ReservedSystemCPU:        "0-1",
			},
		},
	}

	var hub v1alpha2.StorageNode
	if err := stored.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	var back StorageNode
	if err := back.ConvertFrom(&hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	got := back.Spec.Overrides
	if got == nil {
		t.Fatal("spec.overrides is absent, want the four stashed fields back")
	}
	if !ptr.BoolFromOrFalse(got.UbuntuHost) ||
		!ptr.BoolFromOrFalse(got.SkipKubeletConfiguration) ||
		!ptr.BoolFromOrFalse(got.EnableCpuTopology) ||
		got.ReservedSystemCPU != "0-1" {
		t.Errorf("the four fields did not survive: %+v", got)
	}
}

// The sizing has no v1alpha1 spelling at all, so it stashes on the way down and
// comes back on the way up. A node the hub never wrote has none, which is what the
// upgrade's sizing stamp exists to fill.
func TestStorageNodeSizingSurvivesTheTripDown(t *testing.T) {
	hub := &v1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "sb"},
		Spec: v1alpha2.StorageNodeSpec{
			ClusterRef: testCluster,
			WorkerNode: "worker-3",
			Config: v1alpha2.StorageNodeConfig{
				Sizing: v1alpha2.StorageNodeSizing{
					VCPUCount:        ptr.To(int32(8)),
					MinHugePagesSize: "100G",
				},
			},
		},
	}

	var stored StorageNode
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageNode
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub.Spec.Config.Sizing, back.Spec.Config.Sizing); diff != "" {
		t.Errorf("the sizing did not survive (-want +got):\n%s", diff)
	}
}

// SpdkImagePullPolicy is a hub field this version has no home for: the v1alpha1
// overrides block names the SPDK image and says nothing about when it is pulled.
// A node whose document set it, read once at v1alpha1 and written back, has to
// still say Never, or the next read of it silently pulls.
func TestStorageNodeSpdkPullPolicySurvivesTheTripDown(t *testing.T) {
	hub := &v1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "sb"},
		Spec: v1alpha2.StorageNodeSpec{
			ClusterRef: testCluster,
			WorkerNode: "worker-3",
			Config: v1alpha2.StorageNodeConfig{
				SpdkImage:           "public.ecr.aws/simply-block/ultra:main-latest",
				SpdkImagePullPolicy: corev1.PullNever,
			},
		},
	}

	var stored StorageNode
	if err := stored.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	var back v1alpha2.StorageNode
	if err := stored.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}

	if diff := cmp.Diff(hub.Spec.Config, back.Spec.Config); diff != "" {
		t.Errorf("the config did not survive (-want +got):\n%s", diff)
	}
}

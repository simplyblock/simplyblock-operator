// The labels the device mirror writes, against the limit that binds them.
//
// A label value is 63 bytes and a StorageNode's name is built from a cluster
// name, a worker hostname, and a slot, so the name outgrows the label on any
// fleet whose machines have fully qualified hostnames. What that costs is not a
// truncated label: the API server refuses the whole object, so the mirror for
// every device on that node fails to reconcile and the devices are never
// published at all.

package node

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aNodeNamed is a StorageNode carrying the worker label the mirror copies.
func aNodeNamed(name, cluster, worker string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "simplyblock",
			Labels:    map[string]string{simplyblockv1alpha2.DeviceLabelWorker: worker},
		},
		Spec: simplyblockv1alpha2.StorageNodeSpec{ClusterRef: cluster},
	}
}

// Every label the mirror writes fits, whatever the names it is built from.
//
// The name below is the one this was found on: a cluster a discovery run named
// after itself, a worker with a fully qualified hostname, and a slot. It is 68
// bytes, and the API server refused it.
func TestTheDeviceLabelsFitTheLimit(t *testing.T) {
	r := &StorageDeviceReconciler{}
	node := aNodeNamed(
		"discovered-initial-discovery-cluster-vm03.simplyblock4.localdomain-0",
		"discovered-initial-discovery-cluster",
		"vm03.simplyblock4.localdomain",
	)

	for key, value := range r.deviceLabels(node) {
		if len(value) > atlaskube.MaxLabelValueLength {
			t.Errorf("%s is %d bytes: %s", key, len(value), value)
		}
		if errs := atlaskube.Validate(atlaskube.LabelValue, value); len(errs) > 0 {
			t.Errorf("%s is not a legal label value: %v", key, errs)
		}
	}
}

// A name that already fits is written exactly, because the labels exist for a
// person to select on and a value nobody can type is a selector nobody can
// write.
func TestALabelThatFitsIsNotRewritten(t *testing.T) {
	r := &StorageDeviceReconciler{}
	node := aNodeNamed("a-cluster-worker-1-0", "a-cluster", "worker-1")

	labels := r.deviceLabels(node)
	for key, want := range map[string]string{
		simplyblockv1alpha2.DeviceLabelNode:    "a-cluster-worker-1-0",
		simplyblockv1alpha2.DeviceLabelCluster: "a-cluster",
		simplyblockv1alpha2.DeviceLabelWorker:  "worker-1",
	} {
		if got := labels[key]; got != want {
			t.Errorf("%s = %q, want the name unchanged: %q", key, got, want)
		}
	}
}

// Two nodes that differ only past the limit get different labels, so a selector
// on one does not return the other's devices.
//
// This is the whole reason the truncation carries a digest. Cutting at 63 bytes
// would map every node of a long-named cluster to one value, and a person asking
// which devices are in a failed node would be handed the whole cluster's.
func TestTwoLongNamesDoNotCollide(t *testing.T) {
	r := &StorageDeviceReconciler{}
	prefix := strings.Repeat("a", 60)

	first := r.deviceLabels(aNodeNamed(prefix+"-vm03-0", "c", "w"))
	second := r.deviceLabels(aNodeNamed(prefix+"-vm04-0", "c", "w"))

	key := simplyblockv1alpha2.DeviceLabelNode
	if first[key] == second[key] {
		t.Errorf("two nodes share the label %q", first[key])
	}
}

// A worker label the node does not carry is still left out rather than invented,
// which is the behavior the mirror already had and the sanitizing must not lose.
func TestAnAbsentSourceIsStillLeftOut(t *testing.T) {
	r := &StorageDeviceReconciler{}
	node := aNodeNamed("a-cluster-worker-1-0", "", "")
	node.Labels = nil

	labels := r.deviceLabels(node)
	if _, present := labels[simplyblockv1alpha2.DeviceLabelWorker]; present {
		t.Error("a worker label was invented from a node that carries none")
	}
	if _, present := labels[simplyblockv1alpha2.DeviceLabelCluster]; present {
		t.Error("a cluster label was invented from a node that names none")
	}
}

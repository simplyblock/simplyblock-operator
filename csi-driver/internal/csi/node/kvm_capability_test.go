package node

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

func kvmOpens(string) error { return nil }

func kvmMissing(string) error { return errors.New("open /dev/kvm: no such file or directory") }

func kvmLabel(t *testing.T, client *kfake.Clientset) (string, bool) {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, managed := node.Annotations[kube.AnnoKVMCapableManagedBy]
	return node.Labels[kube.LabelKVMCapable], managed
}

// The metadata server pod schedules only onto nodes carrying the label, so a
// node whose /dev/kvm opens says so, and marks the label as the probe's own.
func TestAdvertiseKVMCapabilityLabelsANodeWhoseKVMOpens(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	if err := advertiseKVMCapability(context.Background(), client, "node-a", kvmOpens); err != nil {
		t.Fatalf("advertiseKVMCapability: %v", err)
	}
	if label, managed := kvmLabel(t, client); label != vdoCapableTrue || !managed {
		t.Errorf("label = %q, managed = %t, want true and managed", label, managed)
	}
}

func TestAdvertiseKVMCapabilityLabelsANodeWithoutKVMFalse(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	if err := advertiseKVMCapability(context.Background(), client, "node-a", kvmMissing); err != nil {
		t.Fatalf("advertiseKVMCapability: %v", err)
	}
	if label, _ := kvmLabel(t, client); label != vdoCapableFalse {
		t.Errorf("label = %q, want false", label)
	}
}

// A hand-set label is the override a golden-image node depends on.
func TestAdvertiseKVMCapabilityLeavesAnOperatorSetLabelAlone(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(map[string]string{kube.LabelKVMCapable: vdoCapableTrue}, nil))
	if err := advertiseKVMCapability(context.Background(), client, "node-a", kvmMissing); err != nil {
		t.Fatalf("advertiseKVMCapability: %v", err)
	}
	if label, managed := kvmLabel(t, client); label != vdoCapableTrue || managed {
		t.Errorf("label = %q, managed = %t, want the operator's true left untouched", label, managed)
	}
}

// A label the probe wrote is its own to overwrite, which is how a node that
// lost KVM stops attracting the metadata server.
func TestAdvertiseKVMCapabilityOverwritesItsOwnPriorLabel(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(
		map[string]string{kube.LabelKVMCapable: vdoCapableTrue},
		map[string]string{kube.AnnoKVMCapableManagedBy: kube.AnnoKVMCapableManagedByAutoDetect},
	))
	if err := advertiseKVMCapability(context.Background(), client, "node-a", kvmMissing); err != nil {
		t.Fatalf("advertiseKVMCapability: %v", err)
	}
	if label, _ := kvmLabel(t, client); label != vdoCapableFalse {
		t.Errorf("label = %q, want the probe's own label overwritten to false", label)
	}
}

// Tests for the sizing stamp.
//
// The property that matters is the one the conversion depends on: after the step,
// a node carries an annotation that decodes to a sizing with a core count in it.
// Every case here is either that, a reason the step must refuse rather than write
// something the next admission rejects, or the idempotence a rerun needs.

package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// sizedCluster is a cluster that states what its nodes were built with.
func sizedCluster(vcpus *int32, hugePages string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: theCluster, Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			VCPUCount:        vcpus,
			MinHugePagesSize: hugePages,
		},
	}
}

// reparentedNode is a node the ownership step has already moved onto its cluster,
// which is the state this step requires.
func reparentedNode(annotations map[string]string) *simplyblockv1alpha1.StorageNode {
	return &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "node-a",
			Namespace:       "simplyblock",
			Annotations:     annotations,
			OwnerReferences: ownedByCluster(),
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			StorageNodeSetRef: theSet,
			WorkerNode:        "worker-1",
		},
	}
}

// stampOf decodes the annotation the step writes.
func stampOf(t *testing.T, raw string) simplyblockv1alpha2.StorageNodeSizing {
	t.Helper()
	var sizing simplyblockv1alpha2.StorageNodeSizing
	if err := json.Unmarshal([]byte(raw), &sizing); err != nil {
		t.Fatalf("the stamp does not decode: %v", err)
	}
	return sizing
}

// The whole point: a node with no sizing gets its cluster's, in the annotation the
// conversion reads back into spec.config.sizing.
func TestTheSizingIsStampedFromTheCluster(t *testing.T) {
	node := reparentedNode(nil)
	scope := migration(t, sizedCluster(ptr.To(int32(8)), "100G"), node)
	subject := upgrade.Subject{Ref: scope.Ref(node), Object: node}
	step := stampNodeSizing{}

	if err := step.Validate(context.Background(), scope, subject); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := step.Apply(context.Background(), scope, subject); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := step.Verify(context.Background(), scope, subject); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	var fresh simplyblockv1alpha1.StorageNode
	if err := scope.Client.Get(context.Background(), subject.Ref.Key(), &fresh); err != nil {
		t.Fatalf("re-reading the node: %v", err)
	}
	sizing := stampOf(t, fresh.Annotations[annoNodeSizing])
	if sizing.VCPUCount == nil || *sizing.VCPUCount != 8 {
		t.Errorf("vcpuCount = %v, want the cluster's 8", sizing.VCPUCount)
	}
	if sizing.MinHugePagesSize != "100G" {
		t.Errorf("minHugePagesSize = %q, want the cluster's 100G", sizing.MinHugePagesSize)
	}
}

// Describe reports the outstanding work and nothing for a node already stamped, so
// a rerun's plan shrinks as the migration completes.
func TestTheStampIsDescribedOnceAndThenDone(t *testing.T) {
	node := reparentedNode(nil)
	scope := migration(t, sizedCluster(ptr.To(int32(8)), "100G"), node)
	subject := upgrade.Subject{Ref: scope.Ref(node), Object: node}
	step := stampNodeSizing{}

	action, err := step.Describe(context.Background(), scope, subject)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action == nil {
		t.Fatal("a node with no sizing described nothing")
	}
	if !strings.Contains(action.Detail, "vcpuCount=8") {
		t.Errorf("the plan does not show the numbers it would write: %s", action.Detail)
	}
	if done, _ := step.Done(context.Background(), scope, subject); done {
		t.Error("a node with no sizing reported done")
	}

	if err := step.Apply(context.Background(), scope, subject); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var fresh simplyblockv1alpha1.StorageNode
	if err := scope.Client.Get(context.Background(), subject.Ref.Key(), &fresh); err != nil {
		t.Fatalf("re-reading the node: %v", err)
	}
	stamped := upgrade.Subject{Ref: subject.Ref, Object: &fresh}

	action, err = step.Describe(context.Background(), scope, stamped)
	if err != nil {
		t.Fatalf("Describe after the stamp: %v", err)
	}
	if action != nil {
		t.Errorf("a stamped node still describes work: %s", action.Detail)
	}
	if done, _ := step.Done(context.Background(), scope, stamped); !done {
		t.Error("a stamped node did not report done")
	}
}

// A cluster that states no core count would produce a stamp the field's own
// minimum refuses, so the step says so rather than writing it.
func TestAClusterWithNoCoreCountIsRefused(t *testing.T) {
	node := reparentedNode(nil)
	scope := migration(t, sizedCluster(nil, "100G"), node)
	subject := upgrade.Subject{Ref: scope.Ref(node), Object: node}

	err := stampNodeSizing{}.Validate(context.Background(), scope, subject)
	if err == nil {
		t.Fatal("a cluster with no vcpuCount was accepted")
	}
	if !strings.Contains(err.Error(), "vcpuCount") {
		t.Errorf("the refusal does not name the missing field: %v", err)
	}
}

// The step reads the cluster off the controller owner reference, so a node the
// reparent has not reached has nothing to stamp from and says which step is owed.
func TestANodeThatWasNotReparentedIsRefused(t *testing.T) {
	node := reparentedNode(nil)
	node.OwnerReferences = ownedBySet()
	scope := migration(t, sizedCluster(ptr.To(int32(8)), "100G"), node)
	subject := upgrade.Subject{Ref: scope.Ref(node), Object: node}

	err := stampNodeSizing{}.Validate(context.Background(), scope, subject)
	if err == nil {
		t.Fatal("a node still owned by its set was accepted")
	}
	if !strings.Contains(err.Error(), string(IDReparentNodes)) {
		t.Errorf("the refusal does not name the step that is owed: %v", err)
	}
}

// A stamp somebody hand-edited into nonsense is rewritten rather than trusted,
// because the conversion reads an undecodable one as no sizing at all.
func TestAMalformedStampIsRewritten(t *testing.T) {
	node := reparentedNode(map[string]string{annoNodeSizing: "not json"})
	scope := migration(t, sizedCluster(ptr.To(int32(8)), "100G"), node)
	subject := upgrade.Subject{Ref: scope.Ref(node), Object: node}
	step := stampNodeSizing{}

	if done, _ := step.Done(context.Background(), scope, subject); done {
		t.Fatal("a node carrying an undecodable stamp reported done")
	}
	if err := step.Apply(context.Background(), scope, subject); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := step.Verify(context.Background(), scope, subject); err != nil {
		t.Errorf("Verify after rewriting the stamp: %v", err)
	}
}

// The huge-page floor is optional on the cluster, so a cluster that states none
// produces a stamp that states none either. Inventing a value here would write a
// floor nobody asked for onto every node of the fleet.
func TestAnUnstatedHugePageFloorIsNotInvented(t *testing.T) {
	node := reparentedNode(nil)
	scope := migration(t, sizedCluster(ptr.To(int32(6)), ""), node)
	subject := upgrade.Subject{Ref: scope.Ref(node), Object: node}
	step := stampNodeSizing{}

	if err := step.Validate(context.Background(), scope, subject); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := step.Apply(context.Background(), scope, subject); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := step.Verify(context.Background(), scope, subject); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	var fresh simplyblockv1alpha1.StorageNode
	if err := scope.Client.Get(context.Background(), subject.Ref.Key(), &fresh); err != nil {
		t.Fatalf("re-reading the node: %v", err)
	}
	sizing := stampOf(t, fresh.Annotations[annoNodeSizing])
	if sizing.MinHugePagesSize != "" {
		t.Errorf("minHugePagesSize = %q, want it unstated as the cluster left it",
			sizing.MinHugePagesSize)
	}
	if sizing.VCPUCount == nil || *sizing.VCPUCount != 6 {
		t.Errorf("vcpuCount = %v, want the cluster's 6", sizing.VCPUCount)
	}
}

// The step is about StorageNodes and describes nothing for anything else, which is
// what keeps it out of every other subject's plan.
func TestTheStampIgnoresEveryOtherKind(t *testing.T) {
	cluster := sizedCluster(ptr.To(int32(8)), "100G")
	scope := migration(t, cluster)
	subject := upgrade.Subject{Ref: scope.Ref(cluster), Object: cluster}

	action, err := stampNodeSizing{}.Describe(context.Background(), scope, subject)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if action != nil {
		t.Errorf("a StorageCluster described sizing work: %s", action.Detail)
	}
}

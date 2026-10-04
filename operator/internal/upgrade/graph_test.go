// Tests for the graph. The edge it records is the one §20 moves and the one
// Kubernetes garbage collection acts on, so what is asserted here is that the
// graph agrees with the API server about who depends on whom, including for an
// owner that was never discovered.

package upgrade

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// owned builds a ConfigMap owned by a StorageNodeSet, which is the edge the
// retirement of §16.1 has to move.
func owned(name, owner string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
	}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if owner != "" {
		cm.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "storage.simplyblock.io/v1alpha1",
			Kind:       "StorageNodeSet",
			Name:       owner,
		}}
	}
	return cm
}

// nodeSetID is the identity a StorageNodeSet owner reference resolves to.
func nodeSetID(name string) ObjectIdentity {
	return ObjectIdentity{
		Group:     "storage.simplyblock.io",
		Kind:      "StorageNodeSet",
		Namespace: "simplyblock",
		Name:      name,
	}
}

func TestGraph_RecordsBothDirectionsOfAnOwnerReference(t *testing.T) {
	g := NewGraph()
	g.Add(owned("per-node-config", "nodeset-a"), owned("certs", "nodeset-a"))

	dependents := g.Dependents(nodeSetID("nodeset-a"))
	if len(dependents) != 2 {
		t.Fatalf("the set has %d dependents, want 2: deleting it would garbage-collect "+
			"exactly this set", len(dependents))
	}

	owners := g.Owners(ObjectIdentity{Kind: "ConfigMap", Namespace: "simplyblock", Name: "certs"})
	if len(owners) != 1 || owners[0] != nodeSetID("nodeset-a") {
		t.Fatalf("owners = %v, want the set that owns it", owners)
	}
}

func TestGraph_KnowsADependentOfAnOwnerItNeverFound(t *testing.T) {
	// An owner reference naming an object no discoverer found still produces an
	// edge, which is how validation reports a reference to a resource that does
	// not exist rather than silently dropping it.
	g := NewGraph()
	g.Add(owned("orphan", "nodeset-that-is-gone"))

	if got := g.Dependents(nodeSetID("nodeset-that-is-gone")); len(got) != 1 {
		t.Fatalf("dependents = %v, want the orphan the graph should still report", got)
	}
	if _, found := g.Get(nodeSetID("nodeset-that-is-gone")); found {
		t.Fatal("the graph invented the owner it was told about")
	}
}

func TestGraph_ReReadingAnObjectReplacesItsEdges(t *testing.T) {
	g := NewGraph()
	g.Add(owned("per-node-config", "nodeset-a"))

	// The reparenting of §20 rewrites the owner, and a later read has to leave
	// the graph agreeing with the cluster rather than holding both edges.
	reparented := owned("per-node-config", "")
	reparented.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "storage.simplyblock.io/v1alpha1",
		Kind:       "StorageCluster",
		Name:       "cluster-a",
	}}
	g.Add(reparented)

	if got := g.Dependents(nodeSetID("nodeset-a")); len(got) != 0 {
		t.Fatalf("the old owner still has dependents %v, so deleting it would "+
			"garbage-collect an object that was moved away", got)
	}
	if g.Len() != 1 {
		t.Fatalf("the graph holds %d objects, want 1: a re-read is a refresh, "+
			"not a second object", g.Len())
	}
}

func TestGraph_TypedReturnsOnlyTheKindAsked(t *testing.T) {
	g := NewGraph()
	g.Add(owned("per-node-config", "nodeset-a"))

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "storage-node", Namespace: "simplyblock"}}
	ds.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"})
	g.Add(ds)

	maps := Typed[*corev1.ConfigMap](g, schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if len(maps) != 1 || maps[0].Name != "per-node-config" {
		t.Fatalf("Typed returned %d ConfigMaps, want the one that was added", len(maps))
	}
}

func TestPlan_SummarizesByVerbAndKind(t *testing.T) {
	nodeGVK := schema.GroupVersionKind{Group: "storage.simplyblock.io", Version: "v1alpha1", Kind: "StorageNode"}
	setGVK := schema.GroupVersionKind{Group: "storage.simplyblock.io", Version: "v1alpha1", Kind: "StorageNodeSet"}

	plan := Plan{Stage: StageMigrate}
	plan.Add(
		Task{Step: "reparent", Subtasks: []Action{
			{Verb: VerbReparent, Object: ObjectRef{GVK: nodeGVK, Name: "node-a"}},
			{Verb: VerbReparent, Object: ObjectRef{GVK: nodeGVK, Name: "node-b"}},
		}},
		Task{Step: "retire", Subtasks: []Action{
			{Verb: VerbDelete, Object: ObjectRef{GVK: setGVK, Name: "nodeset-a"}},
		}},
	)

	summary := strings.Join(plan.Summary(), "\n")
	for _, want := range []string{
		"1 StorageNodeSet will be deleted.",
		"2 StorageNodes will be reparented.",
		"2 tasks in total.",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, summary)
		}
	}
}

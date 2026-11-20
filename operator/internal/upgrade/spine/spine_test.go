// Tests for the spine builder. What matters is that it holds a malformed
// cluster rather than failing on one, since every fault it can record is a
// state a real cluster reaches and the point of the structure is to let a check
// report it.

package spine

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// scopeOver builds a scope whose graph holds these objects.
func scopeOver(t *testing.T, objects ...client.Object) *upgrade.Scope {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha1: %v", err)
	}

	c := upgrade.NewReadOnlyClient(fake.NewClientBuilder().WithScheme(scheme).Build())
	scope := upgrade.NewScope(c, "simplyblock", upgrade.StagePreflight,
		upgrade.Options{}, logf.Log, upgrade.DiscardReporter{})
	scope.Adopt(objects...)
	return scope
}

// ownedBySet is the owner reference a StorageNodeSet's controller writes.
func ownedBySet(name string, controller bool) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "storage.simplyblock.io/v1alpha1",
		Kind:       "StorageNodeSet",
		Name:       name,
		Controller: &controller,
	}
}

// cluster is the one StorageCluster every fixture here hangs off.
func cluster() *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "simplyblock"},
	}
}

func set(name, clusterName string) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: clusterName},
	}
}

// node is a StorageNode declaring it belongs to set-a, which every fixture
// here uses as the set under test.
func node(name string, owners ...metav1.OwnerReference) *simplyblockv1alpha1.StorageNode {
	return &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "simplyblock", OwnerReferences: owners,
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{StorageNodeSetRef: "set-a"},
	}
}

func TestBuild_AssemblesTheSpine(t *testing.T) {
	scope := scopeOver(t,
		cluster(),
		set("set-a", "cluster-a"),
		node("node-1", ownedBySet("set-a", true)),
		node("node-2", ownedBySet("set-a", true)),
	)

	graph := Build(scope)
	if len(graph.Clusters) != 1 || len(graph.Clusters[0].Sets) != 1 {
		t.Fatalf("the cluster holds %d sets, want 1", len(graph.Clusters[0].Sets))
	}
	if got := graph.Clusters[0].Sets[0]; len(got.Nodes) != 2 {
		t.Fatalf("the set holds %d nodes, want 2", len(got.Nodes))
	}
	if graph.Sets[0].Cluster == nil {
		t.Fatal("the set did not resolve its cluster")
	}
}

func TestBuild_HoldsASetWhoseClusterIsMissing(t *testing.T) {
	// Not an error to build. It is a state a check reports, and failing here
	// would leave the check with nothing to report it against.
	graph := Build(scopeOver(t, set("set-a", "cluster-that-is-gone")))

	if len(graph.Sets) != 1 {
		t.Fatalf("the spine holds %d sets, want the one that exists", len(graph.Sets))
	}
	if graph.Sets[0].Cluster != nil {
		t.Fatal("a set resolved a cluster that does not exist")
	}
}

func TestBuild_CountsANodeOwnedByTwoSets(t *testing.T) {
	scope := scopeOver(t,
		cluster(), set("set-a", "cluster-a"), set("set-b", "cluster-a"),
		node("node-1", ownedBySet("set-a", true), ownedBySet("set-b", false)),
	)

	graph := Build(scope)
	if got := graph.Nodes[0].OwningSets; got != 2 {
		t.Fatalf("OwningSets = %d, want 2: the reparenting would move it twice", got)
	}
	if graph.Nodes[0].Controller == nil || graph.Nodes[0].Controller.Ref.Name != "set-a" {
		t.Fatal("the controller owner was not the set that declared itself one")
	}
}

func TestBuild_RecordsAnOwnerThatIsNotANodeSet(t *testing.T) {
	foreign := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "something"}
	graph := Build(scopeOver(t,
		cluster(), set("set-a", "cluster-a"),
		node("node-1", ownedBySet("set-a", true), foreign),
	))

	if got := graph.Nodes[0].ForeignOwners; len(got) != 1 || got[0].Kind != "Deployment" {
		t.Fatalf("ForeignOwners = %v, want the Deployment the reparenting cannot place", got)
	}
	if graph.Nodes[0].OwningSets != 1 {
		t.Fatal("a foreign owner was counted as a StorageNodeSet")
	}
}

func TestBuild_ClassifiesTheWorkloadASetOwns(t *testing.T) {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: "simplyblock-storage-node-ds-set-a", Namespace: "simplyblock",
		OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
	}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "set-a-per-node-config", Namespace: "simplyblock",
		OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
	}}

	graph := Build(scopeOver(t, cluster(), set("set-a", "cluster-a"), ds, cm))

	dependents := graph.Sets[0].Dependents
	if len(dependents) != 2 {
		t.Fatalf("the set holds %d dependents, want 2:\n%+v", len(dependents), dependents)
	}
	for _, dependent := range dependents {
		if dependent.Rule == nil {
			t.Errorf("%s is owned by the set and no rule covers it", dependent.Ref)
		}
	}
}

func TestBuild_LeavesAnUncoveredKindUnclassified(t *testing.T) {
	// A Deployment is not a kind a StorageNodeSet owns today, so nothing says
	// what becomes of one, which is exactly what the check has to refuse on.
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "something", Namespace: "simplyblock",
		OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
	}}

	graph := Build(scopeOver(t, cluster(), set("set-a", "cluster-a"), deployment))

	if len(graph.Sets[0].Dependents) != 1 {
		t.Fatalf("the set holds %d dependents, want 1", len(graph.Sets[0].Dependents))
	}
	if graph.Sets[0].Dependents[0].Rule != nil {
		t.Fatal("a Deployment was classified, and no rule covers one")
	}
}

func TestBuild_TheNodesAreNotAlsoDependents(t *testing.T) {
	// The spine models StorageNodes as nodes, and counting them again as
	// dependents would report each one twice.
	graph := Build(scopeOver(t,
		cluster(), set("set-a", "cluster-a"),
		node("node-1", ownedBySet("set-a", true)),
	))

	if got := len(graph.Sets[0].Dependents); got != 0 {
		t.Fatalf("the set holds %d dependents, want 0: its only child is a node", got)
	}
}

func TestBuild_CountsTheOtherOwnersOfADependent(t *testing.T) {
	other := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "keeper"}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "shared", Namespace: "simplyblock",
		OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true), other},
	}}

	graph := Build(scopeOver(t, cluster(), set("set-a", "cluster-a"), cm))

	if got := graph.Sets[0].Dependents[0].OtherOwners; got != 1 {
		t.Fatalf("OtherOwners = %d, want 1: it survives the set's deletion on its own", got)
	}
}

func TestBuild_IsDeterministic(t *testing.T) {
	// Dependents come out of the graph across several kinds, and a report that
	// reorders itself between runs reads as a change.
	objects := make([]client.Object, 0, 8)
	objects = append(objects, cluster(), set("set-a", "cluster-a"))
	for _, name := range []string{"c", "a", "b"} {
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
		}})
		objects = append(objects, &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
		}})
	}
	scope := scopeOver(t, objects...)

	var first []string
	for run := range 10 {
		dependents := Build(scope).Sets[0].Dependents
		order := make([]string, 0, len(dependents))
		for _, dependent := range dependents {
			order = append(order, dependent.Ref.String())
		}
		if run == 0 {
			first = order
			continue
		}
		for i := range order {
			if order[i] != first[i] {
				t.Fatalf("two builds ordered the dependents differently:\n%v\n%v", first, order)
			}
		}
	}
}

func TestRules_CoverEveryKindTheSetControllerOwns(t *testing.T) {
	// The list is read from simplyblockstoragenodeset_controller.go, and a
	// kind added there without a disposition is what reparenting-is-safe
	// refuses on. This asserts the eight that are known today are all present.
	want := []string{
		"StorageNode", "DaemonSet", "Service", "EndpointSlice",
		"ServiceAccount", "ConfigMap", "Secret", "Certificate",
	}

	covered := make(map[string]bool)
	for _, rule := range Rules() {
		if rule.Does == "" {
			t.Errorf("the rule for %s declares no disposition", rule.Kind.Kind)
		}
		if rule.Why == "" {
			t.Errorf("the rule for %s says nothing about what the object is for", rule.Kind.Kind)
		}
		covered[rule.Kind.Kind] = true
	}

	for _, kind := range want {
		if !covered[kind] {
			t.Errorf("%s is owned by a StorageNodeSet and no rule covers it", kind)
		}
	}
}

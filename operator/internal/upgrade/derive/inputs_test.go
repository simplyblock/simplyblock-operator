// Tests for what each row reads out of the graph. The boundary tests prove the
// formulas, and these prove that a row is fed the names the operator would
// actually feed it, which is the half an arithmetic test cannot reach.

package derive

import (
	"testing"

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

// graphOver builds a scope whose graph already holds these objects, which is
// the state discovery leaves behind.
func graphOver(t *testing.T, objects ...client.Object) *upgrade.Scope {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha1: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	scope := upgrade.NewScope(c, "simplyblock", upgrade.StagePreflight,
		upgrade.Options{}, logf.Log, upgrade.DiscardReporter{})
	scope.Adopt(objects...)
	return scope
}

func testPool(name, cluster string) *simplyblockv1alpha1.StoragePool {
	return &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha1.StoragePoolSpec{ClusterName: cluster},
	}
}

func testNodeSet(name, cluster string) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: cluster},
	}
}

// derived runs a row over a scope and returns the values it produced.
func derived(t *testing.T, id upgrade.ID, scope *upgrade.Scope) []string {
	t.Helper()

	rule, ok := declared(t)[id]
	if !ok {
		t.Fatalf("%s is not declared", id)
	}
	inputs, err := rule.Inputs(t.Context(), scope)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}

	out := make([]string, 0, len(inputs))
	for _, input := range inputs {
		out = append(out, input.Derive(rule.Formula()).Value)
	}
	return out
}

func TestInputs_StorageClassNameJoinsNamespaceClusterAndPool(t *testing.T) {
	scope := graphOver(t, testPool("gold", "cluster-a"))

	got := derived(t, IDStorageClassName, scope)
	if len(got) != 1 || got[0] != "simplyblock-simplyblock-cluster-a-gold" {
		t.Fatalf("derived %v, want the name storageclass_name.go builds", got)
	}
}

func TestInputs_AmbiguousConcatenationIsVisibleAsTwoSourcesOneValue(t *testing.T) {
	// §19.8's first uniqueness route. The two pools derive one StorageClass
	// name, and the rule's job is to make that visible rather than to prevent
	// it.
	scope := graphOver(t, testPool("tier", "prod-gold"), testPool("gold-tier", "prod"))

	got := derived(t, IDStorageClassName, scope)
	if len(got) != 2 {
		t.Fatalf("derived %d names from two pools, want 2", len(got))
	}
	if got[0] != got[1] {
		t.Fatalf("derived %q and %q, want one name from both: this is the "+
			"collision the preflight has to report", got[0], got[1])
	}
}

func TestInputs_TwoNodeSetsOfOneClusterCollapseUnderTheTargetModel(t *testing.T) {
	// §19.8's third route. Today, the two DaemonSets are named per set and
	// coexist. Once the cluster is the parent they are one name.
	scope := graphOver(t, testNodeSet("set-a", "cluster-a"), testNodeSet("set-b", "cluster-a"))

	current := derived(t, IDStorageNodeDaemonSet, scope)
	if len(current) != 2 || current[0] == current[1] {
		t.Fatalf("the current model derived %v, want two distinct names", current)
	}

	target := derived(t, IDStorageNodeDaemonSetTarget, scope)
	if len(target) != 2 {
		t.Fatalf("the target model derived %d names from two sets, want 2", len(target))
	}
	if target[0] != target[1] {
		t.Fatalf("the target model derived %q and %q, want one name from both: "+
			"two storage-node workloads becoming one is what §19.8 refuses", target[0], target[1])
	}
}

func TestInputs_TwoNodeSetsOfDifferentClustersDoNotCollapse(t *testing.T) {
	scope := graphOver(t, testNodeSet("set-a", "cluster-a"), testNodeSet("set-b", "cluster-b"))

	target := derived(t, IDStorageNodeDaemonSetTarget, scope)
	if len(target) != 2 || target[0] == target[1] {
		t.Fatalf("derived %v, want two distinct names: sets of different clusters "+
			"do not collide", target)
	}
}

func TestInputs_TheTargetRowNamesTheObjectAUserWouldChange(t *testing.T) {
	// The part is the cluster's name and the source is the set, because the set
	// is what a user deletes or merges to resolve the collision.
	scope := graphOver(t, testNodeSet("set-a", "cluster-a"))

	rule := declared(t)[IDStorageNodeDaemonSetTarget]
	inputs, err := rule.Inputs(t.Context(), scope)
	if err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("got %d inputs, want 1", len(inputs))
	}
	if inputs[0].Source.String() != "StorageNodeSet simplyblock/set-a" {
		t.Fatalf("source = %q, want the set a user would change", inputs[0].Source)
	}
}

func TestInputs_PoolLabelKeyJoinsOnADot(t *testing.T) {
	scope := graphOver(t, testPool("gold", "cluster-a"))

	got := derived(t, IDPoolNodeLabelKey, scope)
	if len(got) != 1 || got[0] != "pool.simplyblock.cluster-a.gold" {
		t.Fatalf("derived %v, want the key simplyblockstoragepool_controller.go builds", got)
	}
}

func TestInputs_NodeTypeLabelIsTheClusterName(t *testing.T) {
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "simplyblock"},
	}
	scope := graphOver(t, cluster)

	got := derived(t, IDNodeTypeLabel, scope)
	if len(got) != 1 || got[0] != "simplyblock-storage-plane-cluster-a" {
		t.Fatalf("derived %v, want what kube.NodeTypeStoragePlaneValue builds", got)
	}
}

func TestInputs_AnEmptyClusterProducesNoInputs(t *testing.T) {
	// A preflight on an installation that holds nothing must report nothing
	// rather than deriving from empty strings.
	scope := graphOver(t)

	for id := range declared(t) {
		if got := derived(t, id, scope); len(got) != 0 {
			t.Errorf("%s derived %v from an empty cluster", id, got)
		}
	}
}

func TestInputs_ARowSkipsASourceThatCannotYetDeriveOne(t *testing.T) {
	// A StorageNodeSet with no cluster has nothing to derive a target-model
	// name from, and an empty part would report a collision against every other
	// incomplete set.
	scope := graphOver(t, testNodeSet("set-a", ""))

	if got := derived(t, IDStorageNodeDaemonSetTarget, scope); len(got) != 0 {
		t.Fatalf("derived %v from a set with no cluster", got)
	}
}

func TestInputs_ReplicationSlotJoinsThePolicyAndTheClaim(t *testing.T) {
	// The claim is in a workload namespace and reaches the row through the
	// cluster-wide graph, which is where a claim lives.
	scope := graphOver(t)
	scope.AdoptClusterWide(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data", Namespace: "team-a",
			Annotations: map[string]string{
				"storage.simplyblock.io/replication-policy": "nightly",
			},
		},
	})

	got := derived(t, IDReplicationSlot, scope)
	if len(got) != 1 || got[0] != "nightly-data" {
		t.Fatalf("derived %v, want the name replicationSlotName builds", got)
	}
}

func TestInputs_ReplicationSlotHonorsTheOldAnnotationSpelling(t *testing.T) {
	// §16.3's keys are mid-move, and a claim carrying only the old spelling
	// still produces a slot.
	scope := graphOver(t)
	scope.AdoptClusterWide(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data", Namespace: "team-a",
			Annotations: map[string]string{"simplyblock.io/replication-policy": "nightly"},
		},
	})

	if got := derived(t, IDReplicationSlot, scope); len(got) != 1 {
		t.Fatalf("derived %v from a claim carrying the old spelling, want one name", got)
	}
}

func TestInputs_AClaimWithNoPolicyProducesNoSlotName(t *testing.T) {
	scope := graphOver(t)
	scope.AdoptClusterWide(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team-a"},
	})

	if got := derived(t, IDReplicationSlot, scope); len(got) != 0 {
		t.Fatalf("derived %v from a claim that names no policy", got)
	}
}

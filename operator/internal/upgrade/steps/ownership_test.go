// Tests for §20's ownership steps. The one that matters most is §30.4's:
// reparent as §20 orders it, delete the StorageNodeSet, and assert that every
// dependent §16.1 lists survives. The fake client does not run garbage
// collection, so the test asserts the property that decides it, which is that
// nothing still names the set as an owner when it goes.

package steps

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The one cluster and the one set every fixture here is built around. Naming
// them as constants rather than passing them keeps the fixtures about the
// ownership edges, which is what these tests are for.
const (
	theCluster = "cluster-a"
	theSet     = "set-a"
)

// ownedBySet is the controller reference a StorageNodeSet's controller writes.
func ownedBySet() []metav1.OwnerReference {
	return controllerRef("StorageNodeSet", theSet)
}

// ownedByCluster is what the migration leaves behind.
func ownedByCluster() []metav1.OwnerReference {
	return controllerRef("StorageCluster", theCluster)
}

func controllerRef(kind, name string) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{
		APIVersion: "storage.simplyblock.io/v1alpha1",
		Kind:       kind,
		Name:       name,
		UID:        types.UID("uid-" + name),
		Controller: &controller,
	}}
}

func cluster() *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: theCluster, Namespace: "simplyblock", UID: types.UID("uid-" + theCluster),
		},
	}
}

func nodeSet(clusterName string) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: theSet, Namespace: "simplyblock", UID: types.UID("uid-" + theSet),
		},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: clusterName},
	}
}

func storageNode(name string, owners []metav1.OwnerReference) *simplyblockv1alpha1.StorageNode {
	return &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "simplyblock", OwnerReferences: owners,
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{StorageNodeSetRef: theSet},
	}
}

// The workload §16.1 lists, named as the set's controller names it.
const (
	theDaemonSet = "simplyblock-storage-node-ds-set-a"
	theConfigMap = "set-a-per-node-config"
)

func daemonSet(owners []metav1.OwnerReference) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: theDaemonSet, Namespace: "simplyblock", OwnerReferences: owners,
	}}
}

func configMap(owners []metav1.OwnerReference) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: theConfigMap, Namespace: "simplyblock", OwnerReferences: owners,
	}}
}

// migration builds a scope over a live fake cluster holding these objects, with
// the graph already populated as discovery would leave it.
func migration(t *testing.T, objects ...client.Object) *upgrade.Scope {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha1: %v", err)
	}
	// §16.2's renames create v1alpha2 kinds, so the steps that perform them read
	// and write objects of a version the source kinds never had.
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha2: %v", err)
	}
	// The CRD kind is in the scheme because the upgrade stage applies the CRDs
	// this binary carries (§11), and a plan of that stage reads every one of
	// them out of the cluster.
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering apiextensions: %v", err)
	}

	// The fake client routes a status write through the subresource tracker only
	// for a kind it was told has one, and §16.2's absorbed operation carries its
	// outcome in status. A real cluster has it from the CRD's marker.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha2.StorageBackupOps{}).
		WithObjects(objects...).
		Build()
	scope := upgrade.NewScope(c, "simplyblock", upgrade.StageMigrate,
		upgrade.Options{}, logf.Log, upgrade.DiscardReporter{})
	scope.Adopt(objects...)
	return scope
}

// runOwnership drives the three steps the way the migrate phase does.
func runOwnership(t *testing.T, scope *upgrade.Scope) error {
	t.Helper()

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Ownership()...)
	return upgrade.NewRunner(catalog, scope).ApplyAll(t.Context(), upgrade.StageMigrate)
}

// live re-reads an object from the cluster.
func live(t *testing.T, scope *upgrade.Scope, obj client.Object, name string) error {
	t.Helper()
	return scope.Client.Get(t.Context(), types.NamespacedName{Namespace: "simplyblock", Name: name}, obj)
}

// healthy is one cluster, one set, two nodes, and the workload §16.1 lists.
func healthy() []client.Object {
	return []client.Object{
		cluster(),
		nodeSet(theCluster),
		storageNode("node-1", ownedBySet()),
		storageNode("node-2", ownedBySet()),
		daemonSet(ownedBySet()),
		configMap(ownedBySet()),
	}
}

func TestOwnership_ReparentsEverythingAndRetiresTheSet(t *testing.T) {
	scope := migration(t, healthy()...)

	if err := runOwnership(t, scope); err != nil {
		t.Fatalf("running the ownership steps: %v", err)
	}

	for _, name := range []string{"node-1", "node-2"} {
		var node simplyblockv1alpha1.StorageNode
		if err := live(t, scope, &node, name); err != nil {
			t.Fatalf("%s is gone: %v", name, err)
		}
		if !ownedByClusterNamed(node.OwnerReferences) {
			t.Errorf("%s is owned by %v, want the cluster", name, node.OwnerReferences)
		}
	}

	var set simplyblockv1alpha1.StorageNodeSet
	if err := live(t, scope, &set, "set-a"); !apierrors.IsNotFound(err) {
		t.Errorf("the StorageNodeSet was not retired: %v", err)
	}
}

func TestOwnership_EveryDependentSurvivesTheSetsDeletion(t *testing.T) {
	// §30.4, and the test §20's ordering exists for. Garbage collection removes
	// a dependent when its owners are gone, so what decides survival is that
	// nothing still names the set when it goes.
	scope := migration(t, healthy()...)

	if err := runOwnership(t, scope); err != nil {
		t.Fatalf("running the ownership steps: %v", err)
	}

	for name, obj := range map[string]client.Object{
		theDaemonSet: &appsv1.DaemonSet{},
		theConfigMap: &corev1.ConfigMap{},
	} {
		if err := live(t, scope, obj, name); err != nil {
			t.Fatalf("%s did not survive the retirement: %v", name, err)
		}
		for _, ref := range obj.GetOwnerReferences() {
			if ref.Kind == "StorageNodeSet" {
				t.Errorf("%s still names the retired set, so garbage collection "+
					"would take it: %v", name, obj.GetOwnerReferences())
			}
		}
		if !ownedByClusterNamed(obj.GetOwnerReferences()) {
			t.Errorf("%s is owned by %v, want the cluster", name, obj.GetOwnerReferences())
		}
	}
}

func TestOwnership_TheSetIsNotDeletedWhileSomethingStillDependsOnIt(t *testing.T) {
	// The refusal that keeps the storage plane alive. The workload step is
	// skipped, so the DaemonSet still names the set when the retirement is
	// asked to run.
	scope := migration(t, healthy()...)
	scope.Options.Skip = []upgrade.ID{IDReparentWorkload}

	err := runOwnership(t, scope)
	if err == nil {
		t.Fatal("the set was retired while its DaemonSet still named it as owner")
	}
	if !strings.Contains(err.Error(), "garbage-collected") {
		t.Errorf("error = %q, want it to say what would be collected with it", err)
	}

	var set simplyblockv1alpha1.StorageNodeSet
	if err := live(t, scope, &set, "set-a"); err != nil {
		t.Fatalf("the set was deleted despite the refusal: %v", err)
	}
}

func TestOwnership_DescribesNothingForAlreadyReparentedObjects(t *testing.T) {
	// A cluster a previous run already migrated. The plan is empty, which is
	// what a rerun of a finished migration should report.
	scope := migration(t,
		cluster(),
		storageNode("node-1", ownedByCluster()),
	)

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Ownership()...)
	plan, err := upgrade.NewRunner(catalog, scope).Plan(t.Context(), upgrade.StageMigrate)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions()) != 0 {
		t.Fatalf("planned %d actions on an already migrated cluster:\n%v",
			len(plan.Actions()), plan.Actions())
	}
}

func TestOwnership_APartialRunResumesOnWhatIsLeft(t *testing.T) {
	// §22, at the granularity the design states it. One node was reparented
	// before the run was killed, and the other is the only one left.
	scope := migration(t,
		cluster(),
		nodeSet(theCluster),
		storageNode("node-1", ownedByCluster()),
		storageNode("node-2", ownedBySet()),
	)

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Ownership()...)
	plan, err := upgrade.NewRunner(catalog, scope).Plan(t.Context(), upgrade.StageMigrate)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var reparents []string
	for _, action := range plan.Actions() {
		if action.Verb == upgrade.VerbReparent {
			reparents = append(reparents, action.Object.Name)
		}
	}
	if len(reparents) != 1 || reparents[0] != "node-2" {
		t.Fatalf("planned reparenting %v, want the one node that was not already moved", reparents)
	}
}

func TestOwnership_ThePlanReadsAsTheMigrationWillRun(t *testing.T) {
	scope := migration(t, healthy()...)

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Ownership()...)
	plan, err := upgrade.NewRunner(catalog, scope).Plan(t.Context(), upgrade.StageMigrate)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Two nodes, the DaemonSet, and the ConfigMap move, and the set is retired.
	if len(plan.Actions()) != 5 {
		t.Fatalf("planned %d actions, want 4 moves and 1 retirement:\n%v",
			len(plan.Actions()), plan.Actions())
	}
	if last := plan.Actions()[len(plan.Actions())-1]; last.Verb != upgrade.VerbDelete {
		t.Errorf("the plan ends on %s, and the retirement has to come after the "+
			"moves it depends on", last.Verb)
	}

	summary := strings.Join(plan.Summary(), "\n")
	for _, want := range []string{"1 StorageNodeSet will be deleted."} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, summary)
		}
	}
}

func TestOwnership_LeavesANodeWhoseSetHasNoClusterAlone(t *testing.T) {
	// ownership-spine refuses that cluster before the migration runs. If it were
	// reached, the node has no target to name and must not be written to.
	scope := migration(t,
		nodeSet("cluster-that-is-gone"),
		storageNode("node-1", ownedBySet()),
	)

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Ownership()...)
	plan, err := upgrade.NewRunner(catalog, scope).Plan(t.Context(), upgrade.StageMigrate)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions()) != 0 {
		t.Fatalf("planned %v against a set with no cluster", plan.Actions())
	}
}

func TestOwnership_TheStepsAreOrderedByWhatTheyRequire(t *testing.T) {
	// The retirement must come after both moves, and the catalog is free to
	// list them in any order.
	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(Ownership()...)

	ordered, err := catalog.StepsFor(upgrade.StageMigrate, upgrade.Options{})
	if err != nil {
		t.Fatalf("ordering the steps: %v", err)
	}
	if ordered[len(ordered)-1].ID() != IDRetireNodeSets {
		t.Fatalf("the last step is %s, want the retirement", ordered[len(ordered)-1].ID())
	}
}

// ownedByClusterNamed reports whether the controller reference names this
// StorageCluster.
func ownedByClusterNamed(refs []metav1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.Kind == "StorageCluster" && ref.Name == theCluster && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func TestOwnership_AFinishedClusterReportsFinishedRatherThanUntouched(t *testing.T) {
	// The two halves of a nil Describe. A migrated cluster's objects are
	// finished, not objects nothing took responsibility for, and the coverage
	// of the migration is measured on that difference.
	scope := migration(t,
		cluster(),
		storageNode("node-1", ownedByCluster()),
		daemonSet(ownedByCluster()),
	)

	for _, step := range Ownership() {
		if step.ID() == IDRetireNodeSets {
			continue
		}
		covered, err := upgrade.Covered(t.Context(), scope, step)
		if err != nil {
			t.Fatalf("%s: %v", step.ID(), err)
		}
		if len(covered.Outstanding) != 0 {
			t.Errorf("%s has outstanding work on a migrated cluster: %v",
				step.ID(), covered.Outstanding)
		}
		if len(covered.Finished) != 1 {
			t.Errorf("%s claims %v as finished, want the one object it moved",
				step.ID(), covered.Finished)
		}
	}
}

func TestOwnership_TheMoveIsOneAtomicWrite(t *testing.T) {
	// An update is atomic, so the object is never persisted without an owner
	// and the garbage collector is never given a window to act in. What the
	// object must never carry is both owners at once, or two controllers.
	scope := migration(t, healthy()...)

	if err := runOwnership(t, scope); err != nil {
		t.Fatalf("running the ownership steps: %v", err)
	}

	var node simplyblockv1alpha1.StorageNode
	if err := live(t, scope, &node, "node-1"); err != nil {
		t.Fatalf("re-reading the node: %v", err)
	}
	if len(node.OwnerReferences) != 1 {
		t.Fatalf("the node carries %d owner references, want the cluster alone: %v",
			len(node.OwnerReferences), node.OwnerReferences)
	}

	controllers := 0
	for _, ref := range node.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			controllers++
		}
	}
	if controllers != 1 {
		t.Fatalf("the node has %d controller references, and Kubernetes allows one", controllers)
	}
}

func TestOwnership_TheNewOwnerReferenceResolves(t *testing.T) {
	// An owner reference with no UID names nothing, and garbage collection
	// treats a dependent whose owner cannot be resolved as an orphan.
	scope := migration(t, healthy()...)

	if err := runOwnership(t, scope); err != nil {
		t.Fatalf("running the ownership steps: %v", err)
	}

	var node simplyblockv1alpha1.StorageNode
	if err := live(t, scope, &node, "node-1"); err != nil {
		t.Fatalf("re-reading the node: %v", err)
	}
	ref := node.OwnerReferences[0]
	if ref.UID == "" {
		t.Error("the new owner reference carries no UID")
	}
	if ref.APIVersion != "storage.simplyblock.io/v1alpha1" {
		t.Errorf("the new owner reference names apiVersion %q", ref.APIVersion)
	}
}

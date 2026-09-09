// Tests for the discoverer, over the properties that are load bearing. An
// object is read into the graph with its kind on it, because a reference with
// no kind cannot be reported. The group's own kinds are read from every
// namespace, because that is where the operator reconciles them. The workload
// they own is read only where they are, because a cluster holds thousands of
// ConfigMaps and Secrets that have nothing to do with this. And a kind the API
// server does not serve leaves the rest of the preflight able to run.

package discover

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// scopeOver builds a scope reading a cluster holding these objects.
func scopeOver(t *testing.T, objects ...client.Object) *upgrade.Scope {
	t.Helper()
	return scopeWithClient(t, fakeClient(t, objects...))
}

func fakeClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha1: %v", err)
	}
	return scheme
}

func scopeWithClient(t *testing.T, c client.Client) *upgrade.Scope {
	t.Helper()
	return upgrade.NewScope(c, "simplyblock", upgrade.StagePreflight,
		upgrade.Options{}, logf.Log, upgrade.DiscardReporter{})
}

func pool(namespace, name string) *simplyblockv1alpha1.StoragePool {
	return &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

var poolGVK = schema.GroupVersionKind{
	Group:   "storage.simplyblock.io",
	Version: "v1alpha1",
	Kind:    "StoragePool",
}

func TestKind_AdoptsWhatItFound(t *testing.T) {
	scope := scopeOver(t, pool("simplyblock", "gold"), pool("simplyblock", "silver"))

	discoverer := Kind{
		RuleID:  "discover-storage-pools",
		Summary: "reads the pools",
		List:    &simplyblockv1alpha1.StoragePoolList{},
	}
	if err := discoverer.Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if got := len(scope.Graph.OfKind(poolGVK)); got != 2 {
		t.Fatalf("the graph holds %d pools, want 2", got)
	}
}

func TestKind_FillsInTheKindTheClientCleared(t *testing.T) {
	// A typed object read through a controller-runtime client comes back with
	// its TypeMeta cleared, and a reference with no kind cannot name the object
	// in a finding.
	scope := scopeOver(t, pool("simplyblock", "gold"))

	discoverer := Kind{
		RuleID:  "discover-storage-pools",
		Summary: "reads the pools",
		List:    &simplyblockv1alpha1.StoragePoolList{},
	}
	if err := discoverer.Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	found := scope.Graph.OfKind(poolGVK)
	if len(found) != 1 {
		t.Fatalf("the graph holds %d pools under %v, want 1: the kind was never filled in",
			len(found), poolGVK)
	}
	if ref := upgrade.RefOf(found[0]); ref.String() != "StoragePool simplyblock/gold" {
		t.Fatalf("reference = %q, want it to name the kind and the namespace", ref)
	}
}

func TestKind_ReadsTheGroupsKindsFromEveryNamespace(t *testing.T) {
	// The manager restricts its cache to no namespace and its RBAC is a
	// ClusterRole, so a StoragePool outside the operator's own namespace is
	// still one it reconciles. A discoverer that narrowed to the operator's
	// namespace left every later check reading an empty graph, which is how a
	// preflight passed on an installation it had not seen.
	scope := scopeOver(t, pool("simplyblock", "gold"), pool("default", "silver"))

	discoverer := Kind{
		RuleID:  IDStoragePools,
		Summary: "reads the pools",
		List:    &simplyblockv1alpha1.StoragePoolList{},
	}
	if err := discoverer.Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if got := len(scope.Graph.OfKind(poolGVK)); got != 2 {
		t.Fatalf("the graph holds %d pools, want both: the operator watches the "+
			"whole cluster, so a pool in any namespace is one it reconciles", got)
	}
}

func TestKind_AnOccupiedReachReadsOnlyWhereTheGroupsObjectsAre(t *testing.T) {
	// The workload a StorageNodeSet owns is created in the set's namespace, so
	// it follows the custom resources. Reading every ConfigMap in a large
	// cluster costs a great deal and returns almost nothing this migration is
	// about.
	scope := scopeOver(t,
		pool("default", "gold"),
		configMapIn("default", "wanted"),
		configMapIn("unrelated", "not-wanted"),
	)

	pools := Kind{RuleID: IDStoragePools, Summary: "reads the pools", List: &simplyblockv1alpha1.StoragePoolList{}}
	if err := pools.Discover(t.Context(), scope); err != nil {
		t.Fatalf("discovering the pools: %v", err)
	}

	maps := Kind{
		RuleID:  IDConfigMaps,
		Summary: "reads the config maps",
		List:    &corev1.ConfigMapList{},
		Where:   Occupied,
	}
	if err := maps.Discover(t.Context(), scope); err != nil {
		t.Fatalf("discovering the config maps: %v", err)
	}

	found := scope.Graph.OfKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if len(found) != 1 {
		t.Fatalf("the graph holds %d config maps, want the 1 in the namespace the "+
			"custom resources occupy:\n%v", len(found), found)
	}
	if found[0].GetName() != "wanted" {
		t.Fatalf("read %q, want the one beside the pool", found[0].GetName())
	}
}

func TestKind_AnOccupiedReachReadsNothingWhenTheGroupHasNoObjects(t *testing.T) {
	// Correct rather than a gap: the workload it would look for belongs to
	// custom resources that are not there.
	scope := scopeOver(t, configMapIn("unrelated", "not-wanted"))

	maps := Kind{
		RuleID:  IDConfigMaps,
		Summary: "reads the config maps",
		List:    &corev1.ConfigMapList{},
		Where:   Occupied,
	}
	if err := maps.Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if got := len(scope.Graph.OfKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})); got != 0 {
		t.Fatalf("the graph holds %d config maps on a cluster with no custom "+
			"resources at all", got)
	}
}

// configMapIn is a ConfigMap the occupied-reach tests look for.
func configMapIn(namespace, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

func TestKind_AClusterScopedKindIsReadWhole(t *testing.T) {
	scope := scopeOver(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "simplyblock"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "another-tenant"}},
	)

	discoverer := Kind{
		RuleID:  IDNamespaces,
		Summary: "reads the namespaces",
		List:    &corev1.NamespaceList{},
	}
	if err := discoverer.Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	if got := len(scope.Graph.OfKind(gvk)); got != 2 {
		t.Fatalf("the graph holds %d namespaces, want 2: a cluster-scoped list "+
			"must not be narrowed to one namespace", got)
	}
}

func TestKind_AKindTheAPIServerDoesNotServeIsNotAFailure(t *testing.T) {
	// A cluster missing one CRD must still get a preflight that reports on the
	// other seventeen. Whether the CRDs are installed is a check of its own.
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return &meta.NoKindMatchError{
					GroupKind: schema.GroupKind{Group: "storage.simplyblock.io", Kind: "BackupImport"},
				}
			},
		}).
		Build()

	discoverer := Kind{
		RuleID:  IDBackupImports,
		Summary: "reads the imports",
		List:    &simplyblockv1alpha1.BackupImportList{},
	}
	if err := discoverer.Discover(t.Context(), scopeWithClient(t, c)); err != nil {
		t.Fatalf("Discover reported %v, want nil: an absent kind is not a failed run", err)
	}
}

func TestKind_APIFailureStopsTheRun(t *testing.T) {
	// A graph missing a kind is a graph every later check would draw a wrong
	// conclusion from, so a read that failed for any other reason is fatal.
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("forbidden")
			},
		}).
		Build()

	discoverer := Kind{
		RuleID:  IDStoragePools,
		Summary: "reads the pools",
		List:    &simplyblockv1alpha1.StoragePoolList{},
	}
	if err := discoverer.Discover(t.Context(), scopeWithClient(t, c)); err == nil {
		t.Fatal("a read the cluster refused was treated as a kind with no objects")
	}
}

func TestKind_ThePrototypeIsNotReused(t *testing.T) {
	// The list is declared once in the catalog and used by every run, so a run
	// that read into it would leak its objects into the next one.
	discoverer := Kind{
		RuleID:  IDStoragePools,
		Summary: "reads the pools",
		List:    &simplyblockv1alpha1.StoragePoolList{},
	}

	if err := discoverer.Discover(t.Context(), scopeOver(t, pool("simplyblock", "gold"))); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	prototype, _ := discoverer.List.(*simplyblockv1alpha1.StoragePoolList)
	if len(prototype.Items) != 0 {
		t.Fatalf("the declared list holds %d items after a run, and the next run "+
			"would find another installation's objects in it", len(prototype.Items))
	}
}

func TestCatalog_EveryDiscovererHasADistinctIdentity(t *testing.T) {
	registry := upgrade.NewRegistry[upgrade.Discoverer]("discoverers")
	if err := registry.Register(declared()...); err != nil {
		t.Fatalf("the declared discoverers do not form a catalog: %v", err)
	}
}

func TestCatalog_EveryRequirementNamesADiscovererThatExists(t *testing.T) {
	// A requirement is satisfied when it names nothing, which is right for a
	// rule the command line skipped and wrong for one somebody misspelled.
	all := declared()
	known := make(map[upgrade.ID]bool, len(all))
	for _, discoverer := range all {
		known[discoverer.ID()] = true
	}

	for _, discoverer := range all {
		for _, required := range discoverer.Requires() {
			if !known[required] {
				t.Errorf("%s requires %s, which no discoverer declares", discoverer.ID(), required)
			}
		}
	}
}

// declared is every discoverer the catalog registers.
func declared() []upgrade.Discoverer {
	all := SimplyblockKinds()
	all = append(all, OwnedKinds()...)
	all = append(all, CoreKinds()...)
	return append(all, ClaimKinds()...)
}

func TestCatalog_EveryOwnedKindWaitsForTheCustomResources(t *testing.T) {
	// The workload a StorageNodeSet owns lives in the set's namespace, so a
	// discoverer that read it everywhere would list every ConfigMap and Secret
	// in the cluster, and one that read the operator's namespace alone would
	// miss all of it.
	for _, discoverer := range OwnedKinds() {
		kind, ok := discoverer.(Kind)
		if !ok {
			continue
		}
		if kind.Where != Occupied {
			t.Errorf("%s reads everywhere, and the workload it looks for is only "+
				"where the custom resources are", kind.RuleID)
		}
	}
}

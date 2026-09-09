// Tests for the discoverer, over the three properties that are load bearing. An
// object is read into the graph with its kind on it, because a reference with
// no kind cannot be reported. Discovery stays inside the installation's
// namespace, because a cluster may hold several. And a kind the API server does
// not serve leaves the rest of the preflight able to run.

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
		RuleID:     "discover-storage-pools",
		Summary:    "reads the pools",
		List:       &simplyblockv1alpha1.StoragePoolList{},
		Namespaced: true,
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
		RuleID:     "discover-storage-pools",
		Summary:    "reads the pools",
		List:       &simplyblockv1alpha1.StoragePoolList{},
		Namespaced: true,
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

func TestKind_StaysInsideTheInstallationNamespace(t *testing.T) {
	// A cluster may hold several independent installations, and a preflight
	// that reports on somebody else's is a preflight that blocks the wrong
	// upgrade.
	scope := scopeOver(t, pool("simplyblock", "gold"), pool("another-tenant", "gold"))

	discoverer := Kind{
		RuleID:     "discover-storage-pools",
		Summary:    "reads the pools",
		List:       &simplyblockv1alpha1.StoragePoolList{},
		Namespaced: true,
	}
	if err := discoverer.Discover(t.Context(), scope); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	found := scope.Graph.OfKind(poolGVK)
	if len(found) != 1 {
		t.Fatalf("the graph holds %d pools, want the 1 in the installation's namespace", len(found))
	}
	if found[0].GetNamespace() != "simplyblock" {
		t.Fatalf("read a pool from %q, which is another installation", found[0].GetNamespace())
	}
}

func TestKind_ClusterWideListIsNotNarrowedToTheNamespace(t *testing.T) {
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
		RuleID:     IDBackupImports,
		Summary:    "reads the imports",
		List:       &simplyblockv1alpha1.BackupImportList{},
		Namespaced: true,
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
		RuleID:     IDStoragePools,
		Summary:    "reads the pools",
		List:       &simplyblockv1alpha1.StoragePoolList{},
		Namespaced: true,
	}
	if err := discoverer.Discover(t.Context(), scopeWithClient(t, c)); err == nil {
		t.Fatal("a read the cluster refused was treated as a kind with no objects")
	}
}

func TestKind_ThePrototypeIsNotReused(t *testing.T) {
	// The list is declared once in the catalog and used by every run, so a run
	// that read into it would leak its objects into the next one.
	discoverer := Kind{
		RuleID:     IDStoragePools,
		Summary:    "reads the pools",
		List:       &simplyblockv1alpha1.StoragePoolList{},
		Namespaced: true,
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
	if err := registry.Register(append(SimplyblockKinds(), CoreKinds()...)...); err != nil {
		t.Fatalf("the declared discoverers do not form a catalog: %v", err)
	}
}

func TestCatalog_EveryRequirementNamesADiscovererThatExists(t *testing.T) {
	// A requirement is satisfied when it names nothing, which is right for a
	// rule the command line skipped and wrong for one somebody misspelled.
	declared := make(map[upgrade.ID]bool)
	all := append(SimplyblockKinds(), CoreKinds()...)
	for _, discoverer := range all {
		declared[discoverer.ID()] = true
	}

	for _, discoverer := range all {
		for _, required := range discoverer.Requires() {
			if !declared[required] {
				t.Errorf("%s requires %s, which no discoverer declares", discoverer.ID(), required)
			}
		}
	}
}

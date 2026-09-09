// Tests for the shipped catalog, end to end against a cluster built to break.
//
// The other packages test their rules in isolation, and what is left over is
// whether the catalog wires them together: whether a discoverer's objects reach
// the check that needs them, whether the two graphs are filled by the right
// discoverers, and whether a rule the command line skips is actually skipped
// once it is inside a registry rather than a slice.

package catalog_test

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/catalog"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/check"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/derive"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/discover"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/spine"
)

// brokenCluster is one installation carrying one of every violation the
// preflight can currently find, plus a second tenant it collides with.
func brokenCluster() []client.Object {
	return []client.Object{
		// Over the 37 characters the node-type label leaves.
		&simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "production-cluster-eu-central-1-primary", Namespace: "simplyblock"},
		},
		// Two pools whose names concatenate to one StorageClass name.
		&simplyblockv1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: "tier", Namespace: "simplyblock"},
			Spec:       simplyblockv1alpha1.StoragePoolSpec{ClusterName: "prod-gold"},
		},
		&simplyblockv1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: "gold-tier", Namespace: "simplyblock"},
			Spec:       simplyblockv1alpha1.StoragePoolSpec{ClusterName: "prod"},
		},
		// Two sets of one cluster, which collapse once the cluster is the parent.
		&simplyblockv1alpha1.StorageNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "set-a", Namespace: "simplyblock"},
			Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-a"},
		},
		&simplyblockv1alpha1.StorageNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "set-b", Namespace: "simplyblock"},
			Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-a"},
		},
		// Another tenant holding a same-named cluster, which is only visible
		// from the cluster-wide view.
		&simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "production-cluster-eu-central-1-primary", Namespace: "other-tenant"},
		},
	}
}

// preflight runs the shipped catalog over a cluster and returns what it found.
func preflight(t *testing.T, opts upgrade.Options, objects ...client.Object) upgrade.Findings {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha1: %v", err)
	}

	// The preflight promises it changes nothing, so the run is given a client
	// that fails the test rather than a cluster that quietly changed.
	c := upgrade.NewReadOnlyClient(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build())
	scope := upgrade.NewScope(c, "simplyblock", upgrade.StagePreflight, opts,
		logf.Log, upgrade.DiscardReporter{})

	runner := upgrade.NewRunner(catalog.Default(), scope)
	if err := runner.Discover(t.Context()); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	findings, err := runner.Check(t.Context(), upgrade.StagePreflight)
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	return findings
}

// raised reports the rules that produced a finding.
func raised(findings upgrade.Findings) map[upgrade.ID]bool {
	out := make(map[upgrade.ID]bool, len(findings))
	for _, finding := range findings {
		out[finding.Rule] = true
	}
	return out
}

func TestCatalog_FindsEveryViolationItCurrentlyCan(t *testing.T) {
	findings := preflight(t, upgrade.Options{}, brokenCluster()...)

	for _, want := range []upgrade.ID{
		derive.IDNodeTypeLabel,              // over the tightest limit in the product
		derive.IDStorageClassName,           // §19.8's ambiguous concatenation
		derive.IDStorageNodeDaemonSetTarget, // §19.8's node-set collapse
		check.IDNamespaceCollapse,           // §19.8's namespace-free cluster label
	} {
		if !raised(findings)[want] {
			t.Errorf("%s raised nothing on a cluster built to break it:\n%v", want, findings)
		}
	}
	if !findings.Blocked() {
		t.Error("a cluster with this much wrong with it did not block the upgrade")
	}
}

func TestCatalog_AHealthyClusterPassesCleanly(t *testing.T) {
	healthy := []client.Object{
		&simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "simplyblock"},
		},
		&simplyblockv1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: "gold", Namespace: "simplyblock"},
			Spec:       simplyblockv1alpha1.StoragePoolSpec{ClusterName: "cluster-a"},
		},
		&simplyblockv1alpha1.StorageNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "set-a", Namespace: "simplyblock"},
			Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-a"},
		},
	}

	if findings := preflight(t, upgrade.Options{}, healthy...); len(findings) != 0 {
		t.Fatalf("a healthy installation produced %d findings:\n%v", len(findings), findings)
	}
}

func TestCatalog_TheClusterWideViewReachesOnlyTheCheckThatNeedsIt(t *testing.T) {
	// The other tenant's cluster is over the node-type limit too, and it is
	// not this installation's problem. Only the collapse finding may name it.
	findings := preflight(t, upgrade.Options{}, brokenCluster()...)

	for _, finding := range findings {
		if finding.Rule == check.IDNamespaceCollapse {
			continue
		}
		if strings.Contains(finding.String(), "other-tenant") {
			t.Errorf("%s reported on another installation's object, which this "+
				"upgrade neither owns nor can fix:\n%s", finding.Rule, finding)
		}
	}
}

func TestCatalog_SkippingARuleSkipsItInsideTheCheck(t *testing.T) {
	opts := upgrade.Options{Skip: []upgrade.ID{derive.IDNodeTypeLabel}}

	if raised(preflight(t, opts, brokenCluster()...))[derive.IDNodeTypeLabel] {
		t.Fatal("a skipped naming rule still raised a finding, so --skip reaches " +
			"the rules command's output and not the check that walks it")
	}
}

func TestCatalog_EveryRegistryAccepts(t *testing.T) {
	// Default panics on a duplicate identity, so building it is the assertion.
	// The counts are here so a rule that quietly stopped being registered is a
	// failure rather than a catalog that got shorter.
	c := catalog.Default()

	if c.Discoverers.Len() == 0 {
		t.Error("no discoverers are registered, and every check would read an empty graph")
	}
	if c.Checks.Len() == 0 {
		t.Error("no checks are registered, and a preflight would pass on anything")
	}
	if c.Derivations.Len() == 0 {
		t.Error("no naming rules are registered, and the name checks would walk nothing")
	}
}

func TestCatalog_EveryReparentableKindIsAlsoDiscovered(t *testing.T) {
	// The reparenting check sees the kinds discovery reads. A kind added to
	// spine.Rules and not to the discoverers is a rule that never fires, and a
	// kind discovered and not ruled on is reported as unclassified on every
	// real cluster. Neither shows up as a failing test anywhere else, because
	// each half is correct on its own.
	discovered := make(map[string]bool)
	for _, discoverer := range catalog.Default().Discoverers.All() {
		kind, ok := discoverer.(discover.Kind)
		if !ok {
			continue
		}
		discovered[strings.TrimSuffix(listKind(kind), "List")] = true
	}

	for _, rule := range spine.Rules() {
		if !discovered[rule.Kind.Kind] {
			t.Errorf("spine.Rules covers %s and no discoverer reads it, so the rule "+
				"never fires and an object of that kind is invisible to the preflight",
				rule.Kind.Kind)
		}
	}
}

// listKind names the kind a discoverer's list prototype holds.
func listKind(kind discover.Kind) string {
	if gvk := kind.List.GetObjectKind().GroupVersionKind(); gvk.Kind != "" {
		return gvk.Kind
	}
	name := fmt.Sprintf("%T", kind.List)
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}

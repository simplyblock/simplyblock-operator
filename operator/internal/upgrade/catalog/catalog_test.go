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
// preflight can currently find. Its objects are spread over two namespaces,
// which is what a real installation looks like: the operator watches the whole
// cluster, and the custom resources live wherever whoever created them put
// them.
func brokenCluster() []client.Object {
	return []client.Object{
		&simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "simplyblock"},
		},
		// Over the 63 bytes the node-set label leaves.
		&simplyblockv1alpha1.StorageNodeSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "production-storage-nodes-eu-central-1-primary-rack-14-socket-01a",
				Namespace: "simplyblock",
			},
			Spec: simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-a"},
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
		// A second namespace holding a same-named set. The label a set claims
		// workers with lands on a Node, which has no namespace, so both sets
		// claim the same machines.
		&simplyblockv1alpha1.StorageNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "set-a", Namespace: "team-a"},
			Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "cluster-a"},
		},
		// Two VolumeMigrations of one name, which §16.2 absorbs into a
		// cluster-scoped kind that has no namespace to keep them apart.
		&simplyblockv1alpha1.VolumeMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "migrate-pv-1", Namespace: "simplyblock"},
		},
		&simplyblockv1alpha1.VolumeMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "migrate-pv-1", Namespace: "team-a"},
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
		derive.IDNodeSetLabel,               // over the 63 bytes a label value allows
		derive.IDStorageClassName,           // §19.8's ambiguous concatenation
		derive.IDStorageNodeDaemonSetTarget, // §19.8's node-set collapse
		check.IDNamespaceCollapse,           // a kind that becomes cluster-scoped
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

func TestCatalog_FindsObjectsOutsideTheOperatorNamespace(t *testing.T) {
	// The bug this replaced: discovery narrowed the group's kinds to the
	// operator's own namespace, so an installation whose StorageCluster lived
	// in default produced an empty graph and a preflight that passed on
	// everything.
	findings := preflight(t, upgrade.Options{}, brokenCluster()...)

	var named bool
	for _, finding := range findings {
		if strings.Contains(finding.String(), "team-a/") {
			named = true
		}
	}
	if !named {
		t.Fatalf("no finding names an object outside the operator namespace, and "+
			"half the fixture lives there:\n%v", findings)
	}
}

func TestCatalog_ANamespacedNameRepeatedInTwoNamespacesIsNotACollision(t *testing.T) {
	// Two sets of one name derive one ConfigMap name, and the two ConfigMaps
	// are in different namespaces, so nothing collides. Reporting it would
	// refuse every installation whose resources are spread over more than one
	// namespace, which is every real one.
	for _, finding := range preflight(t, upgrade.Options{}, brokenCluster()...) {
		switch finding.Rule {
		case derive.IDPerNodeConfigMap, derive.IDStorageNodeDaemonSet,
			derive.IDAPIEndpointSlice, derive.IDNodeRemoveOps:
			if strings.Contains(finding.Summary, "derive one") {
				t.Errorf("%s reported a collision between two namespaces, and its "+
					"value only has to be unique within one:\n%s", finding.Rule, finding)
			}
		}
	}
}

func TestCatalog_SkippingARuleSkipsItInsideTheCheck(t *testing.T) {
	opts := upgrade.Options{Skip: []upgrade.ID{derive.IDNodeSetLabel}}

	if raised(preflight(t, opts, brokenCluster()...))[derive.IDNodeSetLabel] {
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

// Tests for the name checks. What is asserted is that a violation is found and
// reported with enough for a user to act on, that a legal cluster produces
// nothing, and that the check writes nothing whatever it finds.

package check

import (
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
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/derive"
)

// scopeOver builds a read-only scope whose graph holds these objects. The
// client refuses every write, so a check that writes fails the test rather than
// changing a cluster the user was told nothing would change on.
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

// rows is the shipped naming catalog, which is what the checks run against in
// production and therefore what they are tested against here.
func rows(t *testing.T) *upgrade.Registry[upgrade.Derivation] {
	t.Helper()

	registry := upgrade.NewRegistry[upgrade.Derivation]("derivations")
	if err := registry.Register(append(derive.Labels(), derive.Names()...)...); err != nil {
		t.Fatalf("building the naming catalog: %v", err)
	}
	return registry
}

// run executes one of the two checks by identity.
func run(t *testing.T, id upgrade.ID, scope *upgrade.Scope) upgrade.Findings {
	t.Helper()

	for _, check := range Names(rows(t)) {
		if check.ID() != id {
			continue
		}
		findings, err := check.Check(t.Context(), scope)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return findings
	}
	t.Fatalf("%s is not one of the name checks", id)
	return nil
}

func cluster(name string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
	}
}

func pool(name, clusterName string) *simplyblockv1alpha1.StoragePool {
	return &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha1.StoragePoolSpec{ClusterName: clusterName},
	}
}

func nodeSet(name, clusterName string) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: clusterName},
	}
}

func TestFit_ALegalClusterProducesNoFindings(t *testing.T) {
	scope := scopeOver(t, cluster("cluster-a"), pool("gold", "cluster-a"), nodeSet("set-a", "cluster-a"))

	if findings := run(t, IDDerivedNamesFit, scope); len(findings) != 0 {
		t.Fatalf("a cluster whose names are all legal produced %d findings:\n%v",
			len(findings), findings)
	}
}

func TestFit_ASetNameOverSixtyThreeIsAnError(t *testing.T) {
	// The set name reaches the worker Nodes as io.simplyblock.storagenodeset,
	// carrying nothing else, so a label's 63 bytes is the whole budget.
	long := "production-storage-nodes-eu-central-1-primary-rack-14-socket-01a"
	scope := scopeOver(t, nodeSet(long, "cluster-a"))

	findings := run(t, IDDerivedNamesFit, scope)
	if len(findings) != 1 {
		t.Fatalf("a %d-character set name produced %d findings, want 1: an "+
			"overlong value is one violation, not one per way of measuring it:\n%v",
			len(long), len(findings), findings)
	}

	finding := findings[0]
	if !finding.Error() {
		t.Fatalf("severity = %q, want an error: §19.11 fails closed", finding.Severity)
	}
	if finding.Rule != derive.IDNodeSetLabel {
		t.Fatalf("rule = %q, want the node-set row", finding.Rule)
	}

	rendered := finding.String()
	for _, want := range []string{
		"StorageNodeSet simplyblock/" + long, // the object
		"io.simplyblock.storagenodeset",      // where the value lands
		"64 bytes against a limit of 63",     // the measurement
		"bound the input",                    // what resolves it
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the finding does not carry %q:\n%s", want, rendered)
		}
	}
}

func TestFit_SixtyThreeCharactersIsAccepted(t *testing.T) {
	scope := scopeOver(t, nodeSet(strings.Repeat("s", 63), "cluster-a"))

	if findings := run(t, IDDerivedNamesFit, scope); len(findings) != 0 {
		t.Fatalf("a 63-character set name produced %d findings, and the audit "+
			"says it is the longest that works:\n%v", len(findings), findings)
	}
}

func TestFit_ReportsWhetherTheClusterIsBrokenNowOrAfterTheMigration(t *testing.T) {
	// A violation of a current-model row is breaking a cluster today, and one
	// of a target-model row is not. A user deciding whether to upgrade this
	// week needs to be able to tell them apart.
	scope := scopeOver(t, nodeSet(strings.Repeat("s", 250), "cluster-a"))

	findings := run(t, IDDerivedNamesFit, scope)
	if len(findings) == 0 {
		t.Fatal("a 250-character StorageNodeSet name produced no findings")
	}

	var current, target int
	for _, finding := range findings {
		switch {
		case strings.Contains(finding.Detail, "derives today"):
			current++
		case strings.Contains(finding.Detail, "the cluster is correct today"):
			target++
		default:
			t.Errorf("a finding says nothing about which model it is about:\n%s", finding)
		}
	}
	if current == 0 {
		t.Error("no finding reports the rows that are breaking the cluster today")
	}
}

func TestUnique_TwoPoolsDerivingOneStorageClassNameIsAnError(t *testing.T) {
	// §19.8's first route: a separator that is legal inside all three names.
	scope := scopeOver(t, pool("tier", "prod-gold"), pool("gold-tier", "prod"))

	findings := run(t, IDDerivedNamesUnique, scope)
	if len(findings) == 0 {
		t.Fatal("two pools deriving one StorageClass name produced no findings, " +
			"and one of the two would silently take the other's class")
	}

	var found bool
	for _, finding := range findings {
		if finding.Rule != derive.IDStorageClassName {
			continue
		}
		found = true

		if len(finding.Objects) != 2 {
			t.Errorf("the finding names %d objects, want both pools", len(finding.Objects))
		}
		rendered := finding.String()
		for _, want := range []string{
			"StoragePool simplyblock/tier",
			"StoragePool simplyblock/gold-tier",
			"simplyblock-simplyblock-prod-gold-tier",
		} {
			if !strings.Contains(rendered, want) {
				t.Errorf("the finding does not carry %q:\n%s", want, rendered)
			}
		}
	}
	if !found {
		t.Errorf("no finding came from the StorageClass name row:\n%v", findings)
	}
}

func TestUnique_TwoNodeSetsOfOneClusterCollideOnlyUnderTheTargetModel(t *testing.T) {
	// §19.8's third route, and the one this migration introduces. Today, the
	// two DaemonSets are named per set and coexist.
	scope := scopeOver(t, nodeSet("set-a", "cluster-a"), nodeSet("set-b", "cluster-a"))

	findings := run(t, IDDerivedNamesUnique, scope)
	if len(findings) == 0 {
		t.Fatal("two node sets of one cluster produced no findings, and their " +
			"DaemonSets collapse onto one name once the cluster is the parent")
	}

	for _, finding := range findings {
		switch finding.Rule {
		case derive.IDStorageNodeDaemonSetTarget,
			derive.IDPerNodeConfigMapTarget,
			derive.IDAPIEndpointSliceTarget:
		default:
			t.Errorf("%s reported a collision, and only the target-model rows should:\n%s",
				finding.Rule, finding)
		}
	}
}

func TestUnique_TwoNodeSetsOfDifferentClustersDoNotCollide(t *testing.T) {
	scope := scopeOver(t, nodeSet("set-a", "cluster-a"), nodeSet("set-b", "cluster-b"))

	if findings := run(t, IDDerivedNamesUnique, scope); len(findings) != 0 {
		t.Fatalf("sets of two clusters were reported as colliding:\n%v", findings)
	}
}

func TestUnique_OneObjectYieldingSeveralValuesIsNotACollision(t *testing.T) {
	// The per-slot topology key enumerates a cross product of clusters and
	// nodes, so one node hands the row several inputs. Those are one object's
	// several values rather than two objects colliding.
	socket := int32(0)
	node := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{SocketIndex: &socket},
	}
	first := cluster("cluster-a")
	first.Status.UUID = "11111111-1111-1111-1111-111111111111"
	second := cluster("cluster-b")
	second.Status.UUID = "22222222-2222-2222-2222-222222222222"

	scope := scopeOver(t, first, second, node)

	for _, finding := range run(t, IDDerivedNamesUnique, scope) {
		if finding.Rule == derive.IDStorageNodeUUIDKey {
			t.Errorf("one node across two clusters was reported as a collision:\n%s", finding)
		}
	}
}

func TestNames_AnEmptyClusterProducesNothing(t *testing.T) {
	scope := scopeOver(t)

	for _, id := range []upgrade.ID{IDDerivedNamesFit, IDDerivedNamesUnique} {
		if findings := run(t, id, scope); len(findings) != 0 {
			t.Errorf("%s produced %d findings on an empty cluster:\n%v", id, len(findings), findings)
		}
	}
}

func TestNames_ASkippedRowIsNotWalked(t *testing.T) {
	// Skipping a naming rule has to skip it inside the check, not only in the
	// line the rules command prints.
	scope := scopeOver(t, nodeSet(strings.Repeat("s", 64), "cluster-a"))
	scope.Options.Skip = []upgrade.ID{derive.IDNodeSetLabel}

	if findings := run(t, IDDerivedNamesFit, scope); len(findings) != 0 {
		t.Fatalf("a skipped row still reported %d findings:\n%v", len(findings), findings)
	}
}

func TestNames_TheChecksAreDeterministic(t *testing.T) {
	// Two runs over one cluster report the same thing in the same order, or a
	// user diffing two preflights reads a map iteration as a change.
	scope := scopeOver(t, nodeSet("set-a", "cluster-a"), nodeSet("set-b", "cluster-a"))

	first := run(t, IDDerivedNamesUnique, scope).String()
	for range 10 {
		if got := run(t, IDDerivedNamesUnique, scope).String(); got != first {
			t.Fatalf("two runs reported different things:\n%s\n---\n%s", first, got)
		}
	}
}

// Tests for the namespace-collapse check. The property under test is that the
// check sees what no other check can, and that it does not block an upgrade on
// a collision between two installations that are both somebody else's.

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
)

// clusterWideScope builds a scope whose graph holds these objects, which is the
// state discovery leaves behind: every namespace, one graph.
func clusterWideScope(t *testing.T, objects ...client.Object) *upgrade.Scope {
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

func migrationIn(namespace, name string) *simplyblockv1alpha1.VolumeMigration {
	return &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func collapse(t *testing.T, scope *upgrade.Scope) upgrade.Findings {
	t.Helper()

	findings, err := NamespaceCollapse().Check(t.Context(), scope)
	if err != nil {
		t.Fatalf("NamespaceCollapse: %v", err)
	}
	return findings
}

func TestCollapse_TwoVolumeMigrationsBecomeOneClusterScopedObject(t *testing.T) {
	// §19.8's fourth route, and §19.10's fifth check. Nothing is being derived
	// here: the objects' own identities merge, which no naming rule models.
	scope := clusterWideScope(t,
		migrationIn("simplyblock", "migrate-pv-1"),
		migrationIn("other-tenant", "migrate-pv-1"),
	)

	findings := collapse(t, scope)
	if len(findings) != 1 {
		t.Fatalf("produced %d findings, want 1:\n%v", len(findings), findings)
	}

	rendered := findings[0].String()
	for _, want := range []string{
		"VolumeMigration other-tenant/migrate-pv-1",
		"VolumeMigration simplyblock/migrate-pv-1",
		"PersistentVolumeOps",
		"in simplyblock",
		"in other-tenant",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the finding does not carry %q:\n%s", want, rendered)
		}
	}
}

func TestCollapse_DifferentNamesInTwoNamespacesAreFine(t *testing.T) {
	scope := clusterWideScope(t, migrationIn("simplyblock", "prod"), migrationIn("other-tenant", "staging"))

	if findings := collapse(t, scope); len(findings) != 0 {
		t.Fatalf("two differently named migrations were reported as colliding:\n%v", findings)
	}
}

func TestCollapse_ACollisionAwayFromTheOperatorNamespaceIsStillOurs(t *testing.T) {
	// Neither object is in the operator's namespace, and both are reconciled by
	// the operator being upgraded: its cache is restricted to no namespace and
	// its RBAC is a ClusterRole. Skipping this pair would let the migration
	// walk into the collision it exists to refuse.
	scope := clusterWideScope(t, migrationIn("team-a", "prod"), migrationIn("team-b", "prod"))

	if findings := collapse(t, scope); len(findings) != 1 {
		t.Fatalf("a collision outside the operator namespace produced %d findings, "+
			"want 1:\n%v", len(findings), findings)
	}
}

func TestCollapse_ACollisionNeedsTwoNamespaces(t *testing.T) {
	// Two objects of one name in one namespace is a state the API server
	// refuses, so a kind whose key is not the object's name must not report a
	// pair that shares a namespace as a collapse.
	scope := clusterWideScope(t, migrationIn("simplyblock", "prod"))

	if findings := collapse(t, scope); len(findings) != 0 {
		t.Fatalf("one object was reported as colliding with itself:\n%v", findings)
	}
}

func TestCollapse_AnEmptyClusterProducesNothing(t *testing.T) {
	if findings := collapse(t, clusterWideScope(t)); len(findings) != 0 {
		t.Fatalf("an empty cluster produced %d findings:\n%v", len(findings), findings)
	}
}

func TestCollapse_IsDeterministic(t *testing.T) {
	scope := clusterWideScope(t,
		migrationIn("other-tenant", "prod"),
		migrationIn("simplyblock", "prod"),
		migrationIn("third-tenant", "prod"),
	)

	first := collapse(t, scope).String()
	for range 10 {
		if got := collapse(t, scope).String(); got != first {
			t.Fatalf("two runs reported different things:\n%s\n---\n%s", first, got)
		}
	}
}

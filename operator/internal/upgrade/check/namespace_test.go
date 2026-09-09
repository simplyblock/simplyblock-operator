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

// clusterWideScope builds a scope whose cluster-wide graph holds these objects,
// which is the state the escaping discoverers leave behind.
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
	scope.AdoptClusterWide(objects...)
	return scope
}

func setIn(namespace, name string) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
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

func TestCollapse_TwoNodeSetsOfOneNameClaimOneStoragePlane(t *testing.T) {
	// §19.8's second route. Both label the same workers
	// io.simplyblock.storagenodeset=prod, the Node they land on is
	// cluster-scoped, and neither object nor operator notices.
	scope := clusterWideScope(t, setIn("simplyblock", "prod"), setIn("other-tenant", "prod"))

	findings := collapse(t, scope)
	if len(findings) != 1 {
		t.Fatalf("two sets of one name in two namespaces produced %d findings, want 1:\n%v",
			len(findings), findings)
	}

	rendered := findings[0].String()
	for _, want := range []string{
		"StorageNodeSet simplyblock/prod",
		"StorageNodeSet other-tenant/prod",
		"one storage plane",
		"io.simplyblock.storagenodeset",
		"this installation",
		"another installation",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the finding does not carry %q:\n%s", want, rendered)
		}
	}
}

func TestCollapse_TwoVolumeMigrationsBecomeOneClusterScopedObject(t *testing.T) {
	// §19.8's fourth route, and §19.10's fifth check.
	scope := clusterWideScope(t,
		migrationIn("simplyblock", "migrate-pv-1"),
		migrationIn("other-tenant", "migrate-pv-1"),
	)

	findings := collapse(t, scope)
	if len(findings) != 1 {
		t.Fatalf("produced %d findings, want 1:\n%v", len(findings), findings)
	}
	if !strings.Contains(findings[0].String(), "PersistentVolumeOps") {
		t.Errorf("the finding does not say what they become:\n%s", findings[0])
	}
}

func TestCollapse_DifferentNamesInTwoNamespacesAreFine(t *testing.T) {
	scope := clusterWideScope(t, setIn("simplyblock", "prod"), setIn("other-tenant", "staging"))

	if findings := collapse(t, scope); len(findings) != 0 {
		t.Fatalf("two differently named sets were reported as colliding:\n%v", findings)
	}
}

func TestCollapse_ACollisionBetweenTwoOtherInstallationsIsNotOurs(t *testing.T) {
	// Real, and somebody else's. Blocking here would fail an upgrade on the
	// state of a cluster this installation does not own and cannot fix.
	scope := clusterWideScope(t, setIn("tenant-a", "prod"), setIn("tenant-b", "prod"))

	if findings := collapse(t, scope); len(findings) != 0 {
		t.Fatalf("a collision between two other installations blocked this one:\n%v", findings)
	}
}

func TestCollapse_IsInvisibleFromTheInstallationGraph(t *testing.T) {
	// The check is the one that reads ClusterWide, and it has to: the same
	// objects placed in the installation's graph say nothing, because that
	// graph holds one namespace and the collision needs two.
	scope := clusterWideScope(t)
	scope.Adopt(setIn("simplyblock", "prod"), setIn("other-tenant", "prod"))

	if findings := collapse(t, scope); len(findings) != 0 {
		t.Fatalf("the check read the installation graph, which is not the one that "+
			"can answer its question:\n%v", findings)
	}
}

func TestCollapse_AnEmptyClusterProducesNothing(t *testing.T) {
	if findings := collapse(t, clusterWideScope(t)); len(findings) != 0 {
		t.Fatalf("an empty cluster produced %d findings:\n%v", len(findings), findings)
	}
}

func TestCollapse_IsDeterministic(t *testing.T) {
	scope := clusterWideScope(t,
		setIn("other-tenant", "prod"),
		setIn("simplyblock", "prod"),
		setIn("third-tenant", "prod"),
	)

	first := collapse(t, scope).String()
	for range 10 {
		if got := collapse(t, scope).String(); got != first {
			t.Fatalf("two runs reported different things:\n%s\n---\n%s", first, got)
		}
	}
}

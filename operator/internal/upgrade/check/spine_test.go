// Tests for the spine and reparenting checks. Each fault §18 lists gets a case
// that produces it and a case that does not, since a check that fires on
// everything blocks every upgrade and one that fires on nothing lets the
// migration delete a StorageNodeSet that still holds something.

package check

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// ownedBySet is the owner reference a StorageNodeSet's controller writes.
func ownedBySet(name string, controller bool) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "storage.simplyblock.io/v1alpha1",
		Kind:       "StorageNodeSet",
		Name:       name,
		Controller: &controller,
	}
}

func storageNode(name, setName string, owners ...metav1.OwnerReference) *simplyblockv1alpha1.StorageNode {
	node := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "simplyblock", OwnerReferences: owners,
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{StorageNodeSetRef: setName},
	}
	node.Status.Status = "online"
	return node
}

// runCheck executes one check from a list by identity.
func runCheck(t *testing.T, checks []upgrade.Check, id upgrade.ID, scope *upgrade.Scope) upgrade.Findings {
	t.Helper()

	for _, c := range checks {
		if c.ID() != id {
			continue
		}
		findings, err := c.Check(t.Context(), scope)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return findings
	}
	t.Fatalf("%s is not in the list", id)
	return nil
}

// healthySpine is a cluster whose spine is exactly what the migration expects.
func healthySpine(t *testing.T) *upgrade.Scope {
	t.Helper()

	return scopeOver(t,
		cluster("cluster-a"),
		nodeSet("set-a", "cluster-a"),
		storageNode("node-1", "set-a", ownedBySet("set-a", true)),
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
			Name: "simplyblock-storage-node-ds-set-a", Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
		}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "set-a-per-node-config", Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
		}},
	)
}

func TestSpine_AHealthySpinePassesBothChecks(t *testing.T) {
	scope := healthySpine(t)

	for _, id := range []upgrade.ID{IDOwnershipSpine, IDReparentingSafe} {
		if findings := runCheck(t, Spine(), id, scope); len(findings) != 0 {
			t.Errorf("%s produced %d findings on a healthy spine:\n%v", id, len(findings), findings)
		}
	}
}

func TestSpine_ASetNamingAMissingClusterIsAnError(t *testing.T) {
	scope := scopeOver(t, nodeSet("set-a", "cluster-that-is-gone"))

	findings := runCheck(t, Spine(), IDOwnershipSpine, scope)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1:\n%v", len(findings), findings)
	}
	if !strings.Contains(findings[0].String(), "nowhere to reparent to") {
		t.Errorf("the finding does not say why it matters:\n%s", findings[0])
	}
}

func TestSpine_ASetWithNoClusterNameSaysSo(t *testing.T) {
	scope := scopeOver(t, nodeSet("set-a", ""))

	findings := runCheck(t, Spine(), IDOwnershipSpine, scope)
	if len(findings) != 1 || !strings.Contains(findings[0].String(), "no StorageCluster at all") {
		t.Fatalf("an empty spec.clusterName was not reported as its own case:\n%v", findings)
	}
}

func TestSpine_ANodeOwnedByTwoSetsIsAnError(t *testing.T) {
	scope := scopeOver(t,
		cluster("cluster-a"), nodeSet("set-a", "cluster-a"), nodeSet("set-b", "cluster-a"),
		storageNode("node-1", "set-a", ownedBySet("set-a", true), ownedBySet("set-b", false)),
	)

	findings := runCheck(t, Spine(), IDOwnershipSpine, scope)
	if len(findings) != 1 || !strings.Contains(findings[0].String(), "owned by 2 StorageNodeSets") {
		t.Fatalf("a duplicate relationship was not reported:\n%v", findings)
	}
}

func TestSpine_ANodeWhoseDeclaredSetDisagreesWithItsOwnerIsAnError(t *testing.T) {
	scope := scopeOver(t,
		cluster("cluster-a"), nodeSet("set-a", "cluster-a"), nodeSet("set-b", "cluster-a"),
		storageNode("node-1", "set-b", ownedBySet("set-a", true)),
	)

	findings := runCheck(t, Spine(), IDOwnershipSpine, scope)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1:\n%v", len(findings), findings)
	}
	if len(findings[0].Objects) != 2 {
		t.Errorf("the finding names %d objects, want the node and the set that owns it",
			len(findings[0].Objects))
	}
}

func TestSpine_ANodeOwnedByNoSetIsAnError(t *testing.T) {
	scope := scopeOver(t,
		cluster("cluster-a"), nodeSet("set-a", "cluster-a"),
		storageNode("orphan", "set-a"),
	)

	findings := runCheck(t, Spine(), IDOwnershipSpine, scope)
	if len(findings) != 1 || !strings.Contains(findings[0].String(), "owned by no StorageNodeSet") {
		t.Fatalf("an unowned node was not reported:\n%v", findings)
	}
}

func TestSpine_AForeignOwnerIsAnError(t *testing.T) {
	foreign := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "something"}
	scope := scopeOver(t,
		cluster("cluster-a"), nodeSet("set-a", "cluster-a"),
		storageNode("node-1", "set-a", ownedBySet("set-a", true), foreign),
	)

	findings := runCheck(t, Spine(), IDOwnershipSpine, scope)
	if len(findings) != 1 || !strings.Contains(findings[0].String(), "not a StorageNodeSet") {
		t.Fatalf("a foreign owner was not reported:\n%v", findings)
	}
}

func TestReparenting_AnUnclassifiedDependentRefuses(t *testing.T) {
	// The same refusal §12.2 makes about the Helm release: neither disposition
	// is safe as a default, so the migration stops rather than guessing.
	scope := scopeOver(t,
		cluster("cluster-a"), nodeSet("set-a", "cluster-a"),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "something", Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
		}},
	)

	findings := runCheck(t, Spine(), IDReparentingSafe, scope)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1:\n%v", len(findings), findings)
	}

	rendered := findings[0].String()
	for _, want := range []string{
		"Deployment simplyblock/something",
		"StorageNodeSet simplyblock/set-a",
		"garbage-collect it",
		"spine.Rules",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the finding does not carry %q:\n%s", want, rendered)
		}
	}
}

func TestReparenting_AnUnclassifiedDependentWithAnotherOwnerSaysItWouldSurvive(t *testing.T) {
	// A different outcome and a different sentence: the object outlives the
	// set and belongs to nothing this migration models.
	other := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "keeper"}
	scope := scopeOver(t,
		cluster("cluster-a"), nodeSet("set-a", "cluster-a"),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "something", Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true), other},
		}},
	)

	findings := runCheck(t, Spine(), IDReparentingSafe, scope)
	if len(findings) != 1 || !strings.Contains(findings[0].String(), "would survive") {
		t.Fatalf("the finding does not distinguish an object that outlives the set:\n%v", findings)
	}
}

func TestReparenting_EveryKindTheSetControllerOwnsIsClassified(t *testing.T) {
	// The workload §16.1 lists, all owned by one set. None of it may be
	// reported, or the preflight refuses every real cluster.
	owner := []metav1.OwnerReference{ownedBySet("set-a", true)}
	scope := scopeOver(t,
		cluster("cluster-a"), nodeSet("set-a", "cluster-a"),
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "simplyblock", OwnerReferences: owner}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "simplyblock", OwnerReferences: owner}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sa", Namespace: "simplyblock", OwnerReferences: owner}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "simplyblock", OwnerReferences: owner}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "simplyblock", OwnerReferences: owner}},
	)

	if findings := runCheck(t, Spine(), IDReparentingSafe, scope); len(findings) != 0 {
		t.Fatalf("the workload a set owns was reported as unclassified:\n%v", findings)
	}
}

func TestSpine_AnEmptyClusterProducesNothing(t *testing.T) {
	scope := scopeOver(t)

	for _, id := range []upgrade.ID{IDOwnershipSpine, IDReparentingSafe} {
		if findings := runCheck(t, Spine(), id, scope); len(findings) != 0 {
			t.Errorf("%s produced %d findings on an empty cluster:\n%v", id, len(findings), findings)
		}
	}
}

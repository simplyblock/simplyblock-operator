// Tests for the class-to-pool join, which is three labels and no reference.
//
// These are the U-01 to U-06 rows of docs/tests/test-plan-storagepool.md. The
// three labels together are the assignment and none of them is redundant, which
// is what the boundary rows exist to prove: a pool name is unique within a
// namespace and a cluster, so a class naming only the pool would match a pool of
// the same name somewhere else, and an operator that indexed it there would
// report a tenant's class against another tenant's capacity.

package pool

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// U-01 and U-02: a class carrying the three labels is indexed to the pool they
// name, and two classes naming one pool are both indexed to it.
func TestIndexesEveryClassAssignedToThePool(t *testing.T) {
	p := newPool("tenant-a")
	c := newClient(t,
		newClass("fast-xfs", assignmentLabels("tenant-a", false), nil),
		newClass("archive-ext4", assignmentLabels("tenant-a", false), nil),
	)

	_, names, err := AssignedClasses(context.Background(), c, p)
	if err != nil {
		t.Fatalf("AssignedClasses: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v, want both classes", names)
	}
	// Sorted, because the names are published as status and an order that came
	// from the API server's would rewrite the status on every list.
	if names[0] != "archive-ext4" || names[1] != "fast-xfs" {
		t.Errorf("names = %v, want them in name order", names)
	}
}

// U-03: the same pool name in two namespaces indexes each class to its own pool.
func TestThePoolNameAloneIsNotTheAssignment(t *testing.T) {
	here := newPool("tenant-a")
	elsewhere := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Namespace = "another-namespace"
	})
	labelsElsewhere := map[string]string{
		LabelNamespace: "another-namespace",
		LabelCluster:   testCluster,
		LabelPool:      "tenant-a",
	}
	c := newClient(t,
		newClass("ours", assignmentLabels("tenant-a", false), nil),
		newClass("theirs", labelsElsewhere, nil),
	)

	_, ours, err := AssignedClasses(context.Background(), c, here)
	if err != nil {
		t.Fatalf("AssignedClasses: %v", err)
	}
	if len(ours) != 1 || ours[0] != "ours" {
		t.Errorf("the pool in %s is assigned %v, want just its own class", testNamespace, ours)
	}

	_, theirs, err := AssignedClasses(context.Background(), c, elsewhere)
	if err != nil {
		t.Fatalf("AssignedClasses: %v", err)
	}
	if len(theirs) != 1 || theirs[0] != "theirs" {
		t.Errorf("the pool in another-namespace is assigned %v, want just its own class", theirs)
	}
}

// U-04: the same pool name in two clusters of one namespace, likewise.
func TestTheClusterLabelSeparatesTwoPoolsOfOneName(t *testing.T) {
	here := newPool("tenant-a")
	other := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Spec.ClusterRef = "staging"
	})
	stagingLabels := map[string]string{
		LabelNamespace: testNamespace,
		LabelCluster:   "staging",
		LabelPool:      "tenant-a",
	}
	c := newClient(t,
		newClass("production-class", assignmentLabels("tenant-a", false), nil),
		newClass("staging-class", stagingLabels, nil),
	)

	_, production, err := AssignedClasses(context.Background(), c, here)
	if err != nil {
		t.Fatalf("AssignedClasses: %v", err)
	}
	if len(production) != 1 || production[0] != "production-class" {
		t.Errorf("the production pool is assigned %v, want just its own class", production)
	}

	_, staging, err := AssignedClasses(context.Background(), c, other)
	if err != nil {
		t.Fatalf("AssignedClasses: %v", err)
	}
	if len(staging) != 1 || staging[0] != "staging-class" {
		t.Errorf("the staging pool is assigned %v, want just its own class", staging)
	}
}

// U-05: a class whose labels name no pool is indexed to nothing and fails no
// reconcile.
func TestAClassNamingNoPoolIsIndexedToNothing(t *testing.T) {
	p := newPool("tenant-a")
	c := newClient(t,
		newClass("orphan", assignmentLabels("a-pool-that-went-away", false), nil),
		newClass("unlabeled", nil, nil),
	)

	_, names, err := AssignedClasses(context.Background(), c, p)
	if err != nil {
		t.Fatalf("AssignedClasses: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want none", names)
	}
}

// U-06: a class named nothing like its pool is still indexed, which is the whole
// point of carrying the link in a label rather than in the one field that has to
// be unique.
func TestAClassNameCarriesNothing(t *testing.T) {
	p := newPool("tenant-a")
	c := newClient(t, newClass("whatever-its-author-called-it", assignmentLabels("tenant-a", false), nil))

	_, names, err := AssignedClasses(context.Background(), c, p)
	if err != nil {
		t.Fatalf("AssignedClasses: %v", err)
	}
	if len(names) != 1 || names[0] != "whatever-its-author-called-it" {
		t.Errorf("names = %v, want the class whatever it is called", names)
	}
}

// One label separates a class the operator may delete from one it may not, and
// the rule is symmetric: it deletes what it created and refuses on what it did
// not.
func TestOneLabelSeparatesTheOperatorsClassFromAnAuthoredOne(t *testing.T) {
	managed := newClass("generated", assignmentLabels("tenant-a", true), nil)
	authored := newClass("hand-written", assignmentLabels("tenant-a", false), nil)

	if !IsOperatorManaged(managed) {
		t.Error("the class the operator wrote is not recognized as its own")
	}
	if IsOperatorManaged(authored) {
		t.Error("a class the operator did not write is treated as its own, so it would be deleted")
	}
}

// A class the operator writes carries what its pool's defaults say and nothing
// invented: parameters are immutable, so a guessed filesystem or ceiling would be
// one nobody could edit afterward.
func TestGeneratedParametersInventNothing(t *testing.T) {
	bare := newPool("tenant-a")

	params := ClassParameters(bare, testClusterUUID)

	if params[kube.ParamClusterID] != testClusterUUID {
		t.Errorf("cluster_id = %q, want %q", params[kube.ParamClusterID], testClusterUUID)
	}
	if params[kube.ParamPool] != "tenant-a" {
		t.Errorf("pool_name = %q, want tenant-a", params[kube.ParamPool])
	}
	if len(params) != 2 {
		t.Errorf("a pool with no defaults produced %v, want only the two identity keys", params)
	}
}

// ConsumingClassName prefers the class the operator wrote for a default pool and
// otherwise takes the first assigned class. A pool with none is reported rather
// than answered with an invented name.
func TestConsumingClassName(t *testing.T) {
	ctx := context.Background()

	withDefault := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.DefaultStorageClassName = "the-default"
		p.Status.StorageClassNames = []string{"another", "the-default"}
	})
	name, err := ConsumingClassName(ctx, newClient(t, withDefault), testNamespace, testCluster, "tenant-a")
	if err != nil {
		t.Fatalf("ConsumingClassName: %v", err)
	}
	if name != "the-default" {
		t.Errorf("name = %q, want the operator's own class", name)
	}

	assignedOnly := newPool("tenant-b", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.StorageClassNames = []string{"archive-ext4", "fast-xfs"}
	})
	name, err = ConsumingClassName(ctx, newClient(t, assignedOnly), testNamespace, testCluster, "tenant-b")
	if err != nil {
		t.Fatalf("ConsumingClassName: %v", err)
	}
	if name != "archive-ext4" {
		t.Errorf("name = %q, want the first assigned class in name order", name)
	}

	unconsumed := newPool("tenant-c")
	if _, err := ConsumingClassName(
		ctx, newClient(t, unconsumed), testNamespace, testCluster, "tenant-c",
	); err == nil {
		t.Error("a pool with no class answered with a class name anyway")
	}
}

// Tests for the tree. What is asserted is that every object appears, that the
// second column says what becomes of it, and that the two kinds of loose end
// are drawn rather than dropped, since an object missing from the tree is one a
// user does not know the migration is about to touch.

package spine

import (
	"strings"
	"testing"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tree renders the spine built from these objects.
func tree(t *testing.T, objects ...client.Object) []string {
	t.Helper()
	return Render(Build(scopeOver(t, objects...)))
}

// owned is a dependent of set-a, of whatever kind the caller passes.
func ownedConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "simplyblock",
		OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
	}}
}

// find returns the one line naming this object, and fails when none or several
// do, since a tree that draws an object twice is one a reader double-counts.
func find(t *testing.T, lines []string, name string) string {
	t.Helper()

	var found []string
	for _, line := range lines {
		if strings.Contains(line, name) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d lines name %q, want 1:\n%s", len(found), name, strings.Join(lines, "\n"))
	}
	return found[0]
}

func TestRender_NestsSetsUnderTheirClusterAndChildrenUnderTheSet(t *testing.T) {
	lines := tree(t,
		cluster(),
		set("set-a", "cluster-a"),
		node("node-1", ownedBySet("set-a", true)),
	)

	if !strings.HasPrefix(lines[0], "StorageCluster simplyblock/cluster-a") {
		t.Fatalf("the tree does not open on the cluster:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(find(t, lines, "set-a"), branchLast) {
		t.Errorf("the set is not drawn as a child of the cluster:\n%s", find(t, lines, "set-a"))
	}
	if !strings.HasPrefix(find(t, lines, "node-1"), trunkLast+branchLast) {
		t.Errorf("the node is not drawn as a child of the set:\n%s", find(t, lines, "node-1"))
	}
}

func TestRender_SaysWhereEachObjectIsGoing(t *testing.T) {
	lines := tree(t,
		cluster(),
		set("set-a", "cluster-a"),
		node("node-1", ownedBySet("set-a", true)),
		ownedConfigMap("set-a-per-node-config"),
	)

	for _, name := range []string{"node-1", "set-a-per-node-config"} {
		if got := find(t, lines, name); !strings.Contains(got, "→ StorageCluster/cluster-a") {
			t.Errorf("%s does not say where it is going:\n%s", name, got)
		}
	}
	if got := find(t, lines, "StorageNodeSet set-a"); !strings.Contains(got, "retired") {
		t.Errorf("the set does not say it is being retired:\n%s", got)
	}
}

func TestRender_MarksADependentNothingClassifies(t *testing.T) {
	lines := tree(t,
		cluster(), set("set-a", "cluster-a"),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "stray", Namespace: "simplyblock",
			OwnerReferences: []metav1.OwnerReference{ownedBySet("set-a", true)},
		}},
	)

	if got := find(t, lines, "stray"); !strings.Contains(got, "nothing says what becomes of it") {
		t.Fatalf("an unclassified dependent is drawn as though it had a destination:\n%s", got)
	}
}

func TestRender_DrawsASetWhoseClusterIsMissing(t *testing.T) {
	// It appears under no cluster, so without a root of its own it would be
	// absent from the tree entirely.
	lines := tree(t, set("set-lost", "cluster-that-is-gone"))

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Sets whose StorageCluster does not exist") {
		t.Fatalf("the orphaned set has no section:\n%s", joined)
	}
	if got := find(t, lines, "set-lost"); !strings.Contains(got, "cluster-that-is-gone") {
		t.Errorf("the line does not name the cluster it looked for:\n%s", got)
	}
}

func TestRender_DrawsANodeNoSetOwns(t *testing.T) {
	lines := tree(t, cluster(), set("set-a", "cluster-a"), node("orphan"))

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Nodes no StorageNodeSet owns") {
		t.Fatalf("the unowned node has no section:\n%s", joined)
	}
	if got := find(t, lines, "orphan"); !strings.Contains(got, "no owner reference to move") {
		t.Errorf("the line does not say why it is a problem:\n%s", got)
	}
}

func TestRender_ADependentOfAClusterlessSetSaysThereIsNowhereToGo(t *testing.T) {
	lines := tree(t, set("set-lost", "cluster-that-is-gone"), ownedConfigMapOf("set-lost", "config"))

	if got := find(t, lines, "config"); !strings.Contains(got, "nowhere to reparent to") {
		t.Fatalf("a dependent of a set with no cluster is drawn as though it had a "+
			"destination:\n%s", got)
	}
}

func TestRender_DropsTheNamespaceOnlyWhereItIsShared(t *testing.T) {
	// Repeating it on every row pushes the column that carries the news off to
	// the right, and the root already establishes it.
	lines := tree(t, cluster(), set("set-a", "cluster-a"), node("node-1", ownedBySet("set-a", true)))

	if got := find(t, lines, "node-1"); strings.Contains(got, "simplyblock/node-1") {
		t.Errorf("the namespace is repeated on a child of the same namespace:\n%s", got)
	}
	if !strings.Contains(lines[0], "simplyblock/cluster-a") {
		t.Errorf("the root does not establish the namespace:\n%s", lines[0])
	}
}

func TestRender_AlignsTheSecondColumn(t *testing.T) {
	// The point of the column is that it can be scanned, which needs every
	// disposition to start at the same place.
	//
	// The fixture is what lets this test fail: the two nodes sit under
	// different sets, so one carries a "│   " trunk and the other four spaces.
	// Those are four runes each and six bytes against four, so padding measured
	// in bytes misaligns them by two while padding measured in runes does not.
	// Two nodes at the same depth would be shifted equally either way.
	lines := tree(t,
		cluster(), set("set-a", "cluster-a"), set("set-b", "cluster-a"),
		node("node-1", ownedBySet("set-a", true)),
		node("node-2", ownedBySet("set-b", true)),
	)

	var columns []int
	for _, line := range lines {
		idx := strings.Index(line, "→ StorageCluster/")
		if idx < 0 {
			continue
		}
		columns = append(columns, utf8.RuneCountInString(line[:idx]))
	}
	if len(columns) != 2 {
		t.Fatalf("expected two reparented nodes, found %d:\n%s", len(columns), strings.Join(lines, "\n"))
	}
	if columns[0] != columns[1] {
		t.Fatalf("the dispositions start at columns %d and %d:\n%s",
			columns[0], columns[1], strings.Join(lines, "\n"))
	}
}

func TestRender_CountsWhatItDrew(t *testing.T) {
	lines := tree(t,
		cluster(), set("set-a", "cluster-a"), set("set-b", "cluster-a"),
		node("node-1", ownedBySet("set-a", true)),
		ownedConfigMap("config"),
	)

	last := lines[len(lines)-1]
	for _, want := range []string{
		"1 StorageCluster", "2 StorageNodeSets", "1 StorageNode", "1 dependent",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, last)
		}
	}
}

func TestRender_AnEmptyClusterDrawsOnlyItsCounts(t *testing.T) {
	lines := tree(t)

	if got := strings.TrimSpace(strings.Join(lines, "")); got != "0 StorageClusters, 0 StorageNodeSets, 0 StorageNodes, 0 dependents" {
		t.Fatalf("an empty installation drew %q", got)
	}
}

// ownedConfigMapOf is ownedConfigMap for a set other than set-a.
func ownedConfigMapOf(setName, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "simplyblock",
		OwnerReferences: []metav1.OwnerReference{ownedBySet(setName, true)},
	}}
}

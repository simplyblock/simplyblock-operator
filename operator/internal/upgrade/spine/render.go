// Drawing the spine as a tree, with what becomes of each line beside it.
//
// §17 shows the shape and §27 shows the plan it grows into. What makes it worth
// printing is the second column: a list of objects says what a cluster holds,
// and a tree with a disposition against every row says what the migration is
// about to do to it. A user reading the findings afterward has already seen the
// structure they refer to.
//
// The lines are built here rather than by the reporter because a tree's
// alignment is a property of the whole tree. Nothing here writes to a terminal.

package spine

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The tree's box drawing. They are one column wide each despite being three
// bytes, which is why padding counts runes.
const (
	branchMore = "├── "
	branchLast = "└── "
	trunkMore  = "│   "
	trunkLast  = "    "
)

// row is one line of the tree: the label, and what becomes of what it names.
type row struct {
	label string

	// becomes is the disposition, drawn in a second column. It is empty for a
	// row that is a heading rather than an object with a future.
	becomes string
}

// Render draws the spine, one block of lines ready to print.
//
// Every object appears exactly once. A cluster's sets nest under it, a set's
// nodes and dependents nest under the set, and the two kinds of loose end get
// their own roots: a set whose cluster did not resolve, and a node no set owns.
// Both are states §18 refuses on, and showing them in place is what makes the
// finding legible.
func Render(s *Spine) []string {
	var rows []row

	for _, cluster := range s.Clusters {
		rows = append(rows, row{label: cluster.Ref.String(), becomes: "adopts what its sets held"})
		rows = append(rows, setRows(cluster.Sets, cluster.Ref.Name, cluster.Ref.Namespace, "")...)
	}

	if loose := orphanedSets(s); len(loose) > 0 {
		rows = append(rows, row{})
		rows = append(rows, row{label: "Sets whose StorageCluster does not exist"})
		for i, set := range loose {
			rows = append(rows, row{
				label:   branch(i, len(loose)) + set.Ref.String(),
				becomes: fmt.Sprintf("names %q, which does not exist", set.ClusterName),
			})
			rows = append(rows, childRows(set, "", set.Ref.Namespace, trunk(i, len(loose)))...)
		}
	}

	if loose := unownedNodes(s); len(loose) > 0 {
		rows = append(rows, row{})
		rows = append(rows, row{label: "Nodes no StorageNodeSet owns"})
		for i, node := range loose {
			rows = append(rows, row{
				label:   branch(i, len(loose)) + node.Ref.String(),
				becomes: "no owner reference to move",
			})
		}
	}

	return append(align(rows), "", summary(s))
}

// setRows draws a cluster's sets and their contents.
func setRows(sets []*NodeSet, cluster, namespace, indent string) []row {
	rows := make([]row, 0, len(sets))
	for i, set := range sets {
		rows = append(rows, row{
			label:   indent + branch(i, len(sets)) + shortRef(set.Ref, namespace),
			becomes: "retired once it is empty",
		})
		rows = append(rows, childRows(set, cluster, namespace, indent+trunk(i, len(sets)))...)
	}
	return rows
}

// childRows draws what one set holds: its nodes first, then its dependents,
// which is the order §20 moves them in.
func childRows(set *NodeSet, cluster, namespace, indent string) []row {
	type child struct {
		label   string
		becomes string
	}

	children := make([]child, 0, len(set.Nodes)+len(set.Dependents))
	for _, node := range set.Nodes {
		children = append(children, child{shortRef(node.Ref, namespace), reparentedTo(cluster)})
	}
	for _, dependent := range set.Dependents {
		children = append(children, child{shortRef(dependent.Ref, namespace), disposition(dependent, cluster)})
	}

	rows := make([]row, 0, len(children))
	for i, c := range children {
		rows = append(rows, row{
			label:   indent + branch(i, len(children)) + c.label,
			becomes: c.becomes,
		})
	}
	return rows
}

// shortRef drops the namespace from a reference that shares it with the root of
// its subtree, which is every object in an installation. Repeating it on every
// row of a tree pushes the column that carries the news off to the right.
func shortRef(ref ObjectRef, namespace string) string {
	if ref.Namespace != "" && ref.Namespace == namespace {
		kind := ref.GVK.Kind
		if kind == "" {
			kind = "Object"
		}
		return kind + " " + ref.Name
	}
	return ref.String()
}

// disposition says what becomes of one dependent.
func disposition(dependent Dependent, cluster string) string {
	switch {
	case dependent.Rule == nil:
		return "nothing says what becomes of it"
	case dependent.Rule.Does == Delete:
		return "deleted with the set"
	default:
		return reparentedTo(cluster)
	}
}

// reparentedTo is the arrow the whole migration is about.
func reparentedTo(cluster string) string {
	if cluster == "" {
		return "→ nowhere to reparent to"
	}
	return "→ StorageCluster/" + cluster
}

// align pads the labels so the dispositions line up, which is what lets a
// reader scan the second column rather than read every row.
func align(rows []row) []string {
	width := 0
	for _, r := range rows {
		if r.becomes != "" {
			width = max(width, utf8.RuneCountInString(r.label))
		}
	}

	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.becomes == "" {
			out = append(out, r.label)
			continue
		}
		pad := width - utf8.RuneCountInString(r.label)
		out = append(out, r.label+strings.Repeat(" ", pad)+"  "+r.becomes)
	}
	return out
}

// summary is the count line, so a tree too long to take in at once still says
// what it held.
func summary(s *Spine) string {
	dependents := 0
	for _, set := range s.Sets {
		dependents += len(set.Dependents)
	}

	return fmt.Sprintf("%s, %s, %s, %s",
		plural(len(s.Clusters), "StorageCluster"),
		plural(len(s.Sets), "StorageNodeSet"),
		plural(len(s.Nodes), "StorageNode"),
		plural(dependents, "dependent"))
}

// orphanedSets are the sets that appear under no cluster.
func orphanedSets(s *Spine) []*NodeSet {
	var out []*NodeSet
	for _, set := range s.Sets {
		if set.Cluster == nil {
			out = append(out, set)
		}
	}
	return out
}

// unownedNodes are the nodes that appear under no set.
func unownedNodes(s *Spine) []*Node {
	var out []*Node
	for _, node := range s.Nodes {
		if node.OwningSets == 0 {
			out = append(out, node)
		}
	}
	return out
}

// branch is the connector for item i of n.
func branch(i, n int) string {
	if i == n-1 {
		return branchLast
	}
	return branchMore
}

// trunk is what continues below item i of n.
func trunk(i, n int) string {
	if i == n-1 {
		return trunkLast
	}
	return trunkMore
}

// plural renders a count with its noun.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

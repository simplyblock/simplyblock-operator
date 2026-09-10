// Building the spine out of the discovered graph, and recording what would not
// resolve.
//
// The build never fails on a malformed cluster. A set whose cluster is missing,
// a node owned by two sets, and a dependent no rule covers are all states a real
// cluster can be in, and the point of the structure is to hold them so a check
// can report them. What the build refuses is a graph it was not given: it reads
// only what discovery adopted, and a kind nobody discovered simply has no
// dependents here.

package spine

import (
	"sort"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// Spine is the ownership spine as the cluster currently holds it.
type Spine struct {
	// Clusters are the StorageCluster objects, in discovery order.
	Clusters []*Cluster

	// Sets are every StorageNodeSet, including the ones whose cluster did not
	// resolve and which therefore appear under no Cluster.
	Sets []*NodeSet

	// Nodes are every StorageNode, including the ones no set owns.
	Nodes []*Node
}

// Cluster is one StorageCluster and the sets that name it.
type Cluster struct {
	Ref ObjectRef

	// Sets are the StorageNodeSets whose spec.clusterName resolved here. After
	// the retirement these are gone and their contents belong to this object.
	Sets []*NodeSet
}

// NodeSet is one StorageNodeSet, what it holds, and what it points at.
type NodeSet struct {
	Ref ObjectRef

	// ClusterName is spec.clusterName as written.
	ClusterName string

	// Cluster is what that resolved to, and is nil when nothing did. A set
	// with no cluster has nowhere to reparent its contents to, which is the
	// first thing that stops the migration.
	Cluster *Cluster

	// Nodes are the StorageNodes this set owns by controller reference.
	Nodes []*Node

	// Dependents is everything else the graph says this set owns, each with
	// the rule covering it or nothing where no rule does.
	Dependents []Dependent
}

// Node is one StorageNode and the owners it declares.
type Node struct {
	Ref ObjectRef

	// DeclaredSet is spec.storageNodeSetRef, which is the set the node says it
	// belongs to.
	DeclaredSet string

	// Controller is the set that controller-owns it, and is nil when no
	// StorageNodeSet does.
	Controller *NodeSet

	// OwningSets counts how many StorageNodeSets own it. More than one is a
	// duplicate relationship, and the migration would reparent it twice.
	OwningSets int

	// ForeignOwners are the owners that are not a StorageNodeSet, which the
	// reparenting does not know what to do with.
	ForeignOwners []upgrade.ObjectIdentity
}

// Dependent is one object a StorageNodeSet owns that is not a StorageNode.
type Dependent struct {
	Ref ObjectRef

	// Rule is what the migration does with it, and is nil when no rule covers
	// the kind. An uncovered dependent stops the upgrade: reparenting it
	// adopts an object the target model has no owner for, and leaving it
	// behind hands it to garbage collection when the set goes.
	Rule *Rule

	// OtherOwners counts the owners besides this set. A dependent with one
	// survives the set's deletion on its own, so it is the set's to move but
	// not the set's to lose.
	OtherOwners int
}

// ObjectRef is re-exported so a caller reading a spine does not import the
// framework only to name what it found.
type ObjectRef = upgrade.ObjectRef

// Build reads the discovered graph into a spine.
//
// It reads the installation's graph and not the cluster-wide one. The spine is
// what this upgrade owns, and another tenant's sets are neither this
// migration's to reparent nor its to refuse.
func Build(s *upgrade.Scope) *Spine {
	rules := Rules()

	spine := &Spine{}
	byName := indexClusters(s, spine)
	setsByID := indexSets(s, spine, byName)
	indexNodes(s, spine, setsByID)

	for _, set := range spine.Sets {
		set.Dependents = dependentsOf(s, set, rules)
	}
	return spine
}

// indexClusters reads the StorageClusters and returns them keyed by name, which
// is what a set's spec.clusterName is resolved against.
func indexClusters(s *upgrade.Scope, spine *Spine) map[string]*Cluster {
	byName := make(map[string]*Cluster)
	for _, obj := range upgrade.Typed[*simplyblockv1alpha1.StorageCluster](s.Graph, clusterGVK) {
		cluster := &Cluster{Ref: s.Ref(obj)}
		spine.Clusters = append(spine.Clusters, cluster)
		byName[obj.Name] = cluster
	}
	return byName
}

// indexSets reads the StorageNodeSets, resolves each against its cluster, and
// returns them keyed by identity for the node walk.
func indexSets(s *upgrade.Scope, spine *Spine, clusters map[string]*Cluster) map[upgrade.ObjectIdentity]*NodeSet {
	byID := make(map[upgrade.ObjectIdentity]*NodeSet)
	for _, obj := range upgrade.Typed[*simplyblockv1alpha1.StorageNodeSet](s.Graph, nodeSetGVK) {
		set := &NodeSet{Ref: s.Ref(obj), ClusterName: obj.Spec.ClusterName}
		if cluster, resolved := clusters[obj.Spec.ClusterName]; resolved {
			set.Cluster = cluster
			cluster.Sets = append(cluster.Sets, set)
		}
		spine.Sets = append(spine.Sets, set)
		byID[set.Ref.Identity()] = set
	}
	return byID
}

// indexNodes reads the StorageNodes and attaches each to the set that owns it.
func indexNodes(s *upgrade.Scope, spine *Spine, sets map[upgrade.ObjectIdentity]*NodeSet) {
	for _, obj := range upgrade.Typed[*simplyblockv1alpha1.StorageNode](s.Graph, nodeGVK) {
		node := &Node{Ref: s.Ref(obj), DeclaredSet: obj.Spec.StorageNodeSetRef}

		for _, owner := range obj.GetOwnerReferences() {
			id := upgrade.ObjectIdentity{
				Group:     groupOf(owner.APIVersion),
				Kind:      owner.Kind,
				Namespace: obj.Namespace,
				Name:      owner.Name,
			}
			set, isSet := sets[id]
			if !isSet {
				node.ForeignOwners = append(node.ForeignOwners, id)
				continue
			}

			node.OwningSets++
			if owner.Controller != nil && *owner.Controller {
				node.Controller = set
			}
			set.Nodes = append(set.Nodes, node)
		}
		spine.Nodes = append(spine.Nodes, node)
	}
}

// dependentsOf classifies everything the graph says this set owns, other than
// the StorageNodes the spine already models.
//
// The result is sorted, since the graph records dependents in discovery order
// across several kinds and a report that reorders itself between runs reads as
// a change.
func dependentsOf(s *upgrade.Scope, set *NodeSet, rules []Rule) []Dependent {
	var out []Dependent
	for _, id := range s.Graph.Dependents(set.Ref.Identity()) {
		if id.Kind == nodeGVK.Kind && id.Group == nodeGVK.Group {
			continue
		}

		obj, held := s.Graph.Get(id)
		if !held {
			// The graph knows the edge because something declared it, and does
			// not hold the object because no discoverer read that kind. It is
			// still a dependent, and one nothing can classify.
			out = append(out, Dependent{Ref: refFromIdentity(id)})
			continue
		}

		dependent := Dependent{Ref: s.Ref(obj), OtherOwners: len(obj.GetOwnerReferences()) - 1}
		if rule, covered := ruleFor(rules, dependent.Ref.GVK); covered {
			dependent.Rule = &rule
		}
		out = append(out, dependent)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref.GVK.Kind != out[j].Ref.GVK.Kind {
			return out[i].Ref.GVK.Kind < out[j].Ref.GVK.Kind
		}
		return out[i].Ref.Name < out[j].Ref.Name
	})
	return out
}

// refFromIdentity rebuilds a reference for an object the graph has an edge to
// and no copy of, so a finding can still name it.
func refFromIdentity(id upgrade.ObjectIdentity) ObjectRef {
	return ObjectRef{
		GVK:       gvk(id.Group, "", id.Kind),
		Namespace: id.Namespace,
		Name:      id.Name,
	}
}

// groupOf takes the group out of an apiVersion, tolerating the core group's
// bare "v1" and an unparsable value alike.
func groupOf(apiVersion string) string {
	for i := range apiVersion {
		if apiVersion[i] == '/' {
			return apiVersion[:i]
		}
	}
	return ""
}

// The kinds the spine is built from.
var (
	clusterGVK = gvk("storage.simplyblock.io", "v1alpha1", "StorageCluster")
	nodeSetGVK = gvk("storage.simplyblock.io", "v1alpha1", "StorageNodeSet")
	nodeGVK    = gvk("storage.simplyblock.io", "v1alpha1", "StorageNode")
)

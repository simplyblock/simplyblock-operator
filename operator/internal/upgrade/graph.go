// The graph the migration walks. §17 of the design requires that the migration
// list every resource relevant to it and build an explicit graph, rather than
// processing objects as it encounters them, because the ownership edges it is
// about to move are only safe to move once every dependent of an owner is
// known.

package upgrade

import (
	"sort"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Graph is what discovery produced: the objects, indexed by kind, and the
// ownership edges between them. It is filled by [Discoverer] implementations
// and read by every check and step afterward.
//
// A Graph is safe for concurrent reads and for concurrent writes from several
// discoverers, since discovery of one kind does not depend on another's.
type Graph struct {
	mu         sync.RWMutex
	byKind     map[schema.GroupVersionKind][]client.Object
	byID       map[ObjectIdentity]client.Object
	owners     map[ObjectIdentity][]ObjectIdentity
	dependents map[ObjectIdentity][]ObjectIdentity
}

// NewGraph builds an empty graph.
func NewGraph() *Graph {
	return &Graph{
		byKind:     make(map[schema.GroupVersionKind][]client.Object),
		byID:       make(map[ObjectIdentity]client.Object),
		owners:     make(map[ObjectIdentity][]ObjectIdentity),
		dependents: make(map[ObjectIdentity][]ObjectIdentity),
	}
}

// Add records objects and the ownership edges their owner references declare.
// An object already in the graph is replaced, so a discoverer that re-reads a
// kind refreshes it rather than duplicating it.
//
// The edges are recorded from what the object itself says. An owner reference
// naming an object no other discoverer found still produces an edge, which is
// how the validation of §18 reports a reference to a resource that does not
// exist rather than silently dropping it.
func (g *Graph) Add(objs ...client.Object) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, obj := range objs {
		ref := RefOf(obj)
		id := ref.Identity()
		if _, seen := g.byID[id]; !seen {
			g.byKind[ref.GVK] = append(g.byKind[ref.GVK], obj)
		} else {
			g.replaceLocked(ref.GVK, id, obj)
		}
		g.byID[id] = obj
		g.recordOwnersLocked(id, obj)
	}
}

// replaceLocked swaps an object already indexed under its kind.
func (g *Graph) replaceLocked(gvk schema.GroupVersionKind, id ObjectIdentity, obj client.Object) {
	for i, existing := range g.byKind[gvk] {
		if RefOf(existing).Identity() == id {
			g.byKind[gvk][i] = obj
			return
		}
	}
	g.byKind[gvk] = append(g.byKind[gvk], obj)
}

// recordOwnersLocked rebuilds the edges for one object from its owner
// references, dropping whatever it declared on a previous read.
func (g *Graph) recordOwnersLocked(id ObjectIdentity, obj client.Object) {
	for _, previous := range g.owners[id] {
		g.dependents[previous] = removeIdentity(g.dependents[previous], id)
	}
	g.owners[id] = nil

	for _, owner := range obj.GetOwnerReferences() {
		ownerID := ownerIdentity(owner, obj.GetNamespace())
		g.owners[id] = append(g.owners[id], ownerID)
		g.dependents[ownerID] = append(g.dependents[ownerID], id)
	}
}

// ownerIdentity resolves an owner reference against the namespace of the object
// that declares it. Kubernetes forbids a namespaced object from being owned
// across namespaces, so the dependent's namespace is the owner's.
func ownerIdentity(owner metav1.OwnerReference, namespace string) ObjectIdentity {
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil {
		// An unparsable apiVersion is a hand-edited object. Keeping the raw
		// string as the group leaves the edge visible to the validation that
		// reports it, rather than silently pointing it at the core group.
		gv = schema.GroupVersion{Group: owner.APIVersion}
	}
	return ObjectIdentity{
		Group:     gv.Group,
		Kind:      owner.Kind,
		Namespace: namespace,
		Name:      owner.Name,
	}
}

// OfKind returns every object discovered for this kind, in discovery order.
func (g *Graph) OfKind(gvk schema.GroupVersionKind) []client.Object {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]client.Object(nil), g.byKind[gvk]...)
}

// Namespaces returns the namespaces the graph holds an object of this API group
// in, sorted so two runs derive the same list. A cluster-scoped object
// contributes nothing, having no namespace.
func (g *Graph) Namespaces(group string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	seen := make(map[string]bool)
	for id := range g.byID {
		if id.Group == group && id.Namespace != "" {
			seen[id.Namespace] = true
		}
	}

	out := make([]string, 0, len(seen))
	for namespace := range seen {
		out = append(out, namespace)
	}
	sort.Strings(out)
	return out
}

// Kinds returns every kind the graph holds an object of.
func (g *Graph) Kinds() []schema.GroupVersionKind {
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := make([]schema.GroupVersionKind, 0, len(g.byKind))
	for gvk := range g.byKind {
		out = append(out, gvk)
	}
	return out
}

// SortedKinds returns the graph's kinds in a stable order. The kinds are
// indexed in a map, so a walk that took them as they came would reorder itself
// between runs and a user diffing two reports would read that as a change.
func (g *Graph) SortedKinds() []schema.GroupVersionKind {
	kinds := g.Kinds()
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].String() < kinds[j].String() })
	return kinds
}

// Objects returns every object in the graph, kinds in sorted order and objects
// in discovery order within a kind. It is the walk a per-object step is driven
// over.
func (g *Graph) Objects() []client.Object {
	out := make([]client.Object, 0, g.Len())
	for _, gvk := range g.SortedKinds() {
		out = append(out, g.OfKind(gvk)...)
	}
	return out
}

// Get returns the object with this identity.
func (g *Graph) Get(id ObjectIdentity) (client.Object, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	obj, ok := g.byID[id]
	return obj, ok
}

// Owners returns what this object declares as its owners. An owner the graph
// does not hold is still reported, because an owner reference naming an object
// that is gone is exactly what validation has to find.
func (g *Graph) Owners(id ObjectIdentity) []ObjectIdentity {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]ObjectIdentity(nil), g.owners[id]...)
}

// Dependents returns everything the graph holds that names this object as an
// owner. It is what the reparenting of §20 walks, and what makes deleting an
// old owner a decision rather than a hope: Kubernetes garbage collection
// removes exactly this set.
func (g *Graph) Dependents(id ObjectIdentity) []ObjectIdentity {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]ObjectIdentity(nil), g.dependents[id]...)
}

// Len reports how many objects the graph holds.
func (g *Graph) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.byID)
}

// Typed returns the objects of a kind as the concrete type a caller expects,
// skipping anything that is not that type. It is a free function rather than a
// method because Go has no generic methods.
func Typed[T client.Object](g *Graph, gvk schema.GroupVersionKind) []T {
	var out []T
	for _, obj := range g.OfKind(gvk) {
		if typed, ok := obj.(T); ok {
			out = append(out, typed)
		}
	}
	return out
}

// removeIdentity drops one identity from a slice, preserving order.
func removeIdentity(ids []ObjectIdentity, drop ObjectIdentity) []ObjectIdentity {
	out := ids[:0]
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

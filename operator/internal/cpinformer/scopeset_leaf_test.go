// Dropping a scope by the thing it is about, rather than by the whole key.
//
// A scope is added when a node resolves its id and dropped when the node goes,
// and the two happen at different moments with different things readable. The
// add knows the cluster's id because the node has just been placed in it; the
// drop is on a teardown path where the cluster may already be gone, and a caller
// that cannot name the cluster cannot build the key it added under.
//
// What that costs is a stream nobody closes. The control plane answers 404 for a
// node it no longer has, the manager treats that as a disconnect, and it
// reconnects on a backoff for the lifetime of the process — one leaked stream per
// removed node.

package cpinformer

import "testing"

func TestRemoveLeafDropsAScopeWithoutItsPrefix(t *testing.T) {
	set := newScopeSet()
	set.Add(Scope{"cluster-a", "node-1"})
	set.Add(Scope{"cluster-a", "node-2"})

	set.RemoveLeaf("node-1")

	got := set.list()
	if len(got) != 1 {
		t.Fatalf("the set holds %v, want only node-2", got)
	}
	if got[0].Key() != (Scope{"cluster-a", "node-2"}).Key() {
		t.Errorf("the set holds %v, want node-2", got[0])
	}
}

// Two clusters can hold a node id only by coincidence, and dropping both would
// close a stream that is still wanted. The leaf is the node, so every scope
// ending in it is about that node.
func TestRemoveLeafDropsEveryScopeForThatNode(t *testing.T) {
	set := newScopeSet()
	set.Add(Scope{"cluster-a", "node-1"})
	set.Add(Scope{"cluster-b", "node-1"})

	set.RemoveLeaf("node-1")

	if got := set.list(); len(got) != 0 {
		t.Errorf("the set still holds %v", got)
	}
}

// A leaf nothing matches changes nothing, so a teardown that runs twice is not a
// teardown that closes somebody else's stream.
func TestRemoveLeafIsIdempotentAndNarrow(t *testing.T) {
	set := newScopeSet()
	set.Add(Scope{"cluster-a", "node-1"})

	set.RemoveLeaf("node-2")
	set.RemoveLeaf("")

	if got := set.list(); len(got) != 1 {
		t.Errorf("the set holds %v, want the untouched scope", got)
	}
}

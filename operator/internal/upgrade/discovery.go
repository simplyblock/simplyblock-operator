// The discovery extension point. §17 requires the migration to build an
// explicit graph before it validates or changes anything, and a discoverer is
// one kind's contribution to that graph. Adding a kind to the migration is
// adding one of these to the catalog, and every check written afterward sees it
// without being changed.

package upgrade

import "context"

// Discoverer reads one part of the cluster into the scope's graph.
//
// A discoverer MUST be read-only, and it MUST record what it found through
// [Scope.Adopt] rather than returning it, because the objects one discoverer
// finds are the input another needs and the graph is where they meet.
type Discoverer interface {
	Rule

	// Requires names the discoverers whose objects this one reads. The runner
	// orders discovery by these edges, so a discoverer that walks the children
	// of a StorageNodeSet names the one that found the sets rather than
	// depending on catalog order.
	Requires() []ID

	// Discover reads and adopts. An error stops discovery, because a graph
	// missing a kind is a graph every later check would draw a wrong conclusion
	// from.
	Discover(ctx context.Context, s *Scope) error
}

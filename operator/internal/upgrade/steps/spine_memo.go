// One spine per run, shared by the steps that read it.
//
// A step's Describe is asked once per subject, and building the spine inside it
// would rebuild the whole ownership graph for every object of every step. The
// memo makes Describe a read rather than a traversal.
//
// It is deliberately not invalidated when a step writes. The spine describes
// the state the phase started from, which is what every target was resolved
// against, and rebuilding it mid-phase would leave a step unable to name the
// move it had just made. What has to stay current is the graph, which a step
// refreshes with what it wrote.

package steps

import (
	"sync"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/spine"
)

// spineMemo caches the spine of one scope.
type spineMemo struct {
	mu    sync.Mutex
	scope *upgrade.Scope
	built *spine.Spine
}

// of returns the spine for this scope, building it if the cache holds another
// scope's or none.
func (m *spineMemo) of(s *upgrade.Scope) *spine.Spine {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.built == nil || m.scope != s {
		m.scope, m.built = s, spine.Build(s)
	}
	return m.built
}

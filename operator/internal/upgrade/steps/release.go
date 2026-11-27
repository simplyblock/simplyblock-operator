// The CRD installation of a release upgrade (design-release-upgrade.md §6.5,
// step 3). It is the API upgrade's step with the release's identity and stage:
// the same embedded CRDs, written the same way, and refused the same way when a
// write would drop a version objects are still stored in. It lives here because
// the step it wraps is this package's.

package steps

import (
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// ReleaseCRDs is the CRD installation under the given identity and stage. It
// requires nothing: a release upgrade has no conversion webhook to smoke-test
// first, since the operator it upgrades already serves every version.
func ReleaseCRDs(id upgrade.ID, stage upgrade.Stage) upgrade.Step {
	return releaseCRDs{id: id, stage: stage}
}

type releaseCRDs struct {
	applyCRDs
	id    upgrade.ID
	stage upgrade.Stage
}

func (r releaseCRDs) ID() upgrade.ID       { return r.id }
func (r releaseCRDs) Stage() upgrade.Stage { return r.stage }
func (releaseCRDs) Requires() []upgrade.ID { return nil }

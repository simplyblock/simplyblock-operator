// The half of a step that a missing dependency blocks, which is never the half
// that says what would happen.
//
// Describing a change and performing it need different things. Saying that
// BackupPolicy nightly becomes StorageBackupPolicy nightly needs the
// BackupPolicy, which discovery has. Constructing the StorageBackupPolicy needs
// the v1alpha2 type, which §29.1 has not written. So a step whose target type
// does not exist still describes its work object by object, and the plan holds
// the real subjects rather than a placeholder line.
//
// What it must not do is quietly succeed. A step that described its work and
// then did nothing would let a run report success having changed nothing, so
// these three refuse, and the runner refuses the whole stage before reaching
// them.

package steps

import (
	"context"
	"fmt"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// described supplies the acting half of a step that cannot yet act.
type described struct {
	id      upgrade.ID
	blocked string
}

// BlockedBy is what the runner reads to refuse the stage.
func (d described) BlockedBy() string {
	return d.blocked
}

func (d described) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return d.refuse()
}
func (d described) Apply(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return d.refuse()
}
func (d described) Verify(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return d.refuse()
}

// refuse is what these return if they are ever reached, which would mean the
// runner's refusal had been bypassed.
func (d described) refuse() error {
	return fmt.Errorf("%s is described and not implemented: %s", d.id, d.blocked)
}

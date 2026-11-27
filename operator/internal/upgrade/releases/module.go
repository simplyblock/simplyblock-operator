// The release module contract and its selection: what one release needs when a
// deployment upgrades to it, and which modules an upgrade from one release to
// another runs (design-release-upgrade.md §6.2 to §6.4). It lives beside the
// catalog builder that places the modules' rules in a run, and apart from the
// modules themselves, which are one package each.

package releases

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// Version is a release, a chart version such as 26.4.0-beta6.2, compared as a
// semantic version with its prerelease.
type Version = *semver.Version

// ParseVersion reads a release. A leading v is accepted, because the tags and
// the image tags spell releases that way and the chart versions do not.
func ParseVersion(s string) (Version, error) {
	v, err := semver.StrictNewVersion(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if err != nil {
		return nil, fmt.Errorf("%q is not a release version: %w", s, err)
	}
	return v, nil
}

// MustVersion is ParseVersion for the literals a module declares.
func MustVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// HookPoint is where in the generic sequence a module's step runs (§6.3).
type HookPoint string

const (
	// BeforeCRDs runs in the prepare stage, after the checks and before the
	// target's CRDs are applied, while the old operator still runs.
	BeforeCRDs HookPoint = "BeforeCRDs"
	// AfterCRDs runs in the prepare stage, once the CRDs are applied and
	// established, before Helm upgrades the operator.
	AfterCRDs HookPoint = "AfterCRDs"
	// AfterOperator runs in the rollout, once the operator runs the target.
	AfterOperator HookPoint = "AfterOperator"
	// AfterControlPlane runs in the rollout, once the control plane runs the
	// target.
	AfterControlPlane HookPoint = "AfterControlPlane"
	// AfterStoragePlane runs in the rollout, once every cluster's rolling
	// restart has succeeded.
	AfterStoragePlane HookPoint = "AfterStoragePlane"
)

// HookPoints is every hook point, in the order a run reaches them.
var HookPoints = []HookPoint{BeforeCRDs, AfterCRDs, AfterOperator, AfterControlPlane, AfterStoragePlane}

// Stage is the stage a hook point belongs to.
func (h HookPoint) Stage() upgrade.Stage {
	if h == BeforeCRDs || h == AfterCRDs {
		return upgrade.StageReleasePrepare
	}
	return upgrade.StageReleaseRollout
}

// Module is what one release needs when a deployment upgrades to it. It runs
// when an upgrade crosses its release: source < Release <= target.
type Module struct {
	// Release is the release this module belongs to.
	Release Version

	// MinimumSource is the oldest release a deployment may upgrade from in one
	// run that crosses this release. An older source is refused with the
	// instruction to upgrade to MinimumSource first. Nil means no floor.
	MinimumSource Version

	// Checks run in the prepare stage. A finding of severity ERROR refuses the
	// upgrade.
	Checks []upgrade.Check

	// Steps run at their hook point, after the generic work of that point and
	// after the steps of every older module selected for the same run.
	Steps map[HookPoint][]upgrade.Step
}

// Selection is what a run from source to target does: the modules it crosses,
// in release order.
type Selection struct {
	Source  Version
	Target  Version
	Modules []Module
}

// Select picks the modules an upgrade from source to target runs, or says why
// the upgrade is refused.
//
// A source equal to the target selects nothing and is not an error: a rerun of
// a finished upgrade has nothing to do. A source newer than the target is a
// downgrade, which no module describes.
func Select(modules []Module, source, target Version) (Selection, error) {
	if source.GreaterThan(target) {
		return Selection{}, fmt.Errorf("the deployment runs %s, which is newer than %s: "+
			"a release upgrade does not downgrade", source, target)
	}

	selected := make([]Module, 0, len(modules))
	for _, m := range modules {
		if m.Release.GreaterThan(source) && !m.Release.GreaterThan(target) {
			selected = append(selected, m)
		}
	}
	slices.SortStableFunc(selected, func(a, b Module) int { return a.Release.Compare(b.Release) })

	var floor Version
	for _, m := range selected {
		if m.MinimumSource != nil && m.MinimumSource.GreaterThan(source) &&
			(floor == nil || m.MinimumSource.GreaterThan(floor)) {
			floor = m.MinimumSource
		}
	}
	if floor != nil {
		return Selection{}, fmt.Errorf("the deployment runs %s, and upgrading to %s needs at least %s: "+
			"upgrade to %s first", source, target, floor, floor)
	}
	return Selection{Source: source, Target: target, Modules: selected}, nil
}

// Releases names the selected modules, for a report.
func (s Selection) Releases() []string {
	out := make([]string, 0, len(s.Modules))
	for _, m := range s.Modules {
		out = append(out, m.Release.String())
	}
	return out
}

// Release selection and the catalog a selection builds: which modules an
// upgrade from one release to another runs, and where their steps land
// (design-release-upgrade.md §6.1 to §6.5, test plan U-35 to U-46).

package releases

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

func versions(t *testing.T, s ...string) []Version {
	t.Helper()
	out := make([]Version, 0, len(s))
	for _, v := range s {
		parsed, err := ParseVersion(v)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, parsed)
	}
	return out
}

func modulesAt(t *testing.T, releases ...string) []Module {
	t.Helper()
	out := make([]Module, 0, len(releases))
	for _, v := range versions(t, releases...) {
		out = append(out, Module{Release: v})
	}
	return out
}

func selected(s Selection) []string { return s.Releases() }

// U-39, U-40: the modules after the source and up to the target, in release
// order, whatever order they were registered in.
func TestSelectTakesTheModulesBetweenSourceAndTarget(t *testing.T) {
	all := modulesAt(t, "26.4.0-rc1", "26.4.0-beta8", "26.4.0-beta6.2", "26.4.0-beta7")
	v := versions(t, "26.4.0-beta6.2", "26.4.0-beta8")

	got, err := Select(all, v[0], v[1])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"26.4.0-beta7", "26.4.0-beta8"}; !slices.Equal(selected(got), want) {
		t.Errorf("selected %v, want %v: the source's own module is not run, the target's is", selected(got), want)
	}
}

// U-41: prerelease ordering decides selection.
func TestPrereleasesOrderBeforeTheirRelease(t *testing.T) {
	v := versions(t, "26.4.0-beta6.2", "26.4.0-beta7", "26.4.0-rc1", "26.4.0")
	for i := 0; i+1 < len(v); i++ {
		if !v[i].LessThan(v[i+1]) {
			t.Errorf("%s is not before %s", v[i], v[i+1])
		}
	}
	if _, err := ParseVersion("v26.4.0-beta6.2"); err != nil {
		t.Errorf("a tag's leading v is refused: %v", err)
	}
}

// U-37: nothing to do is not an error.
func TestSelectingTheReleaseTheDeploymentRunsSelectsNothing(t *testing.T) {
	v := versions(t, "26.4.0-beta7", "26.4.0-beta7")
	got, err := Select(modulesAt(t, "26.4.0-beta7"), v[0], v[1])
	if err != nil || len(got.Modules) != 0 {
		t.Errorf("got %v, %v; want no modules and no error", selected(got), err)
	}
}

// U-38: a downgrade is refused.
func TestSelectRefusesADowngrade(t *testing.T) {
	v := versions(t, "26.4.0-beta8", "26.4.0-beta7")
	if _, err := Select(nil, v[0], v[1]); err == nil || !strings.Contains(err.Error(), "does not downgrade") {
		t.Errorf("err = %v, want a downgrade refused", err)
	}
}

// U-42, U-43: a source older than a crossed module's floor is refused, naming
// the newest floor.
func TestSelectRefusesASourceOlderThanAFloor(t *testing.T) {
	all := []Module{
		{Release: MustVersion("26.4.0-beta7"), MinimumSource: MustVersion("26.4.0-beta5")},
		{Release: MustVersion("26.4.0-beta8"), MinimumSource: MustVersion("26.4.0-beta6")},
	}
	v := versions(t, "26.4.0-beta4", "26.4.0-beta8")
	_, err := Select(all, v[0], v[1])
	if err == nil || !strings.Contains(err.Error(), "upgrade to 26.4.0-beta6 first") {
		t.Errorf("err = %v, want the newest floor named", err)
	}

	between := versions(t, "26.4.0-beta6", "26.4.0-beta8")
	if _, err := Select(all, between[0], between[1]); err != nil {
		t.Errorf("a source at the floor was refused: %v", err)
	}
}

// fixedStep is a module step that only says who it is.
type fixedStep struct {
	step
}

func moduleStep(id upgrade.ID) upgrade.Step {
	return fixedStep{step{id: id, stage: upgrade.StageReleaseRollout,
		pending: func(context.Context, *upgrade.Scope) (*upgrade.Action, error) { return nil, nil },
		apply:   func(context.Context, *upgrade.Scope) error { return nil }}}
}

func stepIDs(t *testing.T, c *upgrade.Catalog, stage upgrade.Stage) []upgrade.ID {
	t.Helper()
	steps, err := c.StepsFor(stage, upgrade.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]upgrade.ID, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.ID())
	}
	return out
}

// U-45, U-46: module steps land at their hook points, in release order, and a
// step's hook point decides its stage.
func TestModuleStepsLandAtTheirHookPointsInReleaseOrder(t *testing.T) {
	selection := Selection{
		Source: MustVersion("26.4.0-beta6"), Target: MustVersion("26.4.0-beta8"),
		Modules: []Module{
			{Release: MustVersion("26.4.0-beta7"), Steps: map[HookPoint][]upgrade.Step{
				AfterCRDs:         {moduleStep("b7-after-crds")},
				AfterStoragePlane: {moduleStep("b7-after-storage")},
			}},
			{Release: MustVersion("26.4.0-beta8"), Steps: map[HookPoint][]upgrade.Step{
				AfterStoragePlane: {moduleStep("b8-after-storage")},
				AfterOperator:     {moduleStep("b8-after-operator")},
			}},
		},
	}
	c := Build(selection, Options{})

	prepare := stepIDs(t, c, upgrade.StageReleasePrepare)
	if want := []upgrade.ID{IDApplyCRDs, "b7-after-crds"}; !slices.Equal(prepare, want) {
		t.Errorf("prepare = %v, want %v", prepare, want)
	}
	rollout := stepIDs(t, c, upgrade.StageReleaseRollout)
	want := []upgrade.ID{IDAwaitOperator, "b8-after-operator", IDUpgradeControlPlane, IDRollStoragePlane,
		"b7-after-storage", "b8-after-storage", IDVerify}
	if !slices.Equal(rollout, want) {
		t.Errorf("rollout = %v, want %v", rollout, want)
	}
}

// A module's checks run in the prepare stage beside the generic ones.
func TestModuleChecksRunInThePrepareStage(t *testing.T) {
	check := NewCheck("b7-check", "a module check",
		func(context.Context, *upgrade.Scope) (upgrade.Findings, error) { return nil, nil })
	c := Build(Selection{Source: MustVersion("1.0.0"), Target: MustVersion("1.1.0"),
		Modules: []Module{{Release: MustVersion("1.1.0"), Checks: []upgrade.Check{check}}}}, Options{})

	checks := c.ChecksFor(upgrade.StageReleasePrepare, upgrade.Options{})
	ids := make([]upgrade.ID, 0, len(checks))
	for _, ch := range checks {
		ids = append(ids, ch.ID())
	}
	if !slices.Contains(ids, "b7-check") || !slices.Contains(ids, IDNodesOnline) {
		t.Errorf("prepare checks = %v, want the module's and the generic ones", ids)
	}
	if len(c.ChecksFor(upgrade.StageReleaseRollout, upgrade.Options{})) != 0 {
		t.Error("a check runs in the rollout, which changes the cluster under it")
	}
}

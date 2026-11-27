// The catalog a release upgrade runs: the generic sequence, with the selected
// modules' checks beside the generic ones and their steps placed at their hook
// points (design-release-upgrade.md §6.3 to §6.5). Placement is by ordering:
// every step requires the one before it, so the framework's ordering runs them
// as written here whatever a module's own steps require.

package releases

import (
	"context"
	"slices"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/steps"
)

// Build assembles the catalog for one selection.
func Build(selection Selection, o Options) *upgrade.Catalog {
	o = o.withDefaults()
	c := upgrade.NewCatalog()

	checks := genericChecks()
	for _, m := range selection.Modules {
		for _, check := range m.Checks {
			checks = append(checks, prepared{check})
		}
	}
	c.Checks.MustRegister(checks...)

	at := func(h HookPoint) []upgrade.Step {
		var out []upgrade.Step
		for _, m := range selection.Modules {
			out = append(out, m.Steps[h]...)
		}
		return out
	}

	sequence := make([]upgrade.Step, 0, 6+len(selection.Modules)*len(HookPoints))
	sequence = append(sequence, at(BeforeCRDs)...)
	sequence = append(sequence, steps.ReleaseCRDs(IDApplyCRDs, upgrade.StageReleasePrepare))
	sequence = append(sequence, at(AfterCRDs)...)
	sequence = append(sequence, awaitOperator(selection.Target, o))
	sequence = append(sequence, at(AfterOperator)...)
	sequence = append(sequence, upgradeControlPlane(selection.Target, o))
	sequence = append(sequence, at(AfterControlPlane)...)
	sequence = append(sequence, rollStoragePlane(selection.Target, o))
	sequence = append(sequence, at(AfterStoragePlane)...)
	sequence = append(sequence, verify())

	placedSteps := make([]upgrade.Step, 0, len(sequence))
	stageOf := hookStages(selection)
	var previous upgrade.ID
	for _, st := range sequence {
		stage := st.Stage()
		if hooked, ok := stageOf[st.ID()]; ok {
			stage = hooked
		}
		p := placed{Step: st, stage: stage}
		if previous != "" {
			p.after = previous
		}
		placedSteps = append(placedSteps, p)
		previous = st.ID()
	}
	c.Steps.MustRegister(placedSteps...)
	return c
}

// hookStages is the stage each module step runs in, which its hook point decides
// rather than the step.
func hookStages(selection Selection) map[upgrade.ID]upgrade.Stage {
	out := map[upgrade.ID]upgrade.Stage{}
	for _, m := range selection.Modules {
		for h, list := range m.Steps {
			for _, st := range list {
				out[st.ID()] = h.Stage()
			}
		}
	}
	return out
}

// placed is a step at its position in the sequence: in its hook point's stage,
// and after the step before it.
type placed struct {
	upgrade.Step
	stage upgrade.Stage
	after upgrade.ID
}

func (p placed) Stage() upgrade.Stage { return p.stage }

func (p placed) Requires() []upgrade.ID {
	out := slices.Clone(p.Step.Requires())
	if p.after != "" {
		out = append(out, p.after)
	}
	return out
}

// Subjects keeps a step's own subjects, which the wrapper would otherwise hide.
func (p placed) Subjects(ctx context.Context, s *upgrade.Scope) ([]upgrade.Subject, error) {
	if enumerator, ok := p.Step.(upgrade.Enumerator); ok {
		return enumerator.Subjects(ctx, s)
	}
	return s.Subjects(), nil
}

// BlockedBy keeps a step's report of what it is waiting for.
func (p placed) BlockedBy() string {
	if blocked, ok := p.Step.(upgrade.Blocked); ok {
		return blocked.BlockedBy()
	}
	return ""
}

// prepared is a module's check, run in the prepare stage whatever stages it
// names: a release check reads before the upgrade changes anything.
type prepared struct{ inner upgrade.Check }

func (p prepared) ID() upgrade.ID        { return p.inner.ID() }
func (p prepared) Description() string   { return p.inner.Description() }
func (prepared) Stages() []upgrade.Stage { return []upgrade.Stage{upgrade.StageReleasePrepare} }
func (p prepared) Check(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
	return p.inner.Check(ctx, s)
}

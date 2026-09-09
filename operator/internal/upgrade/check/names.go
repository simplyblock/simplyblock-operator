// The name and identity checks: §19.10's first three, over every naming rule
// the derivations registry holds.
//
// They are two checks rather than seventeen because the rows are declared as
// data. Whether a value fits its limit is a property of one formula and one
// input, and whether two values collide is a property of one formula and all of
// them, so each question is asked once and every row added later is asked it
// too.

package check

import (
	"context"
	"fmt"
	"sort"
	"strings"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// The identities of the name checks.
const (
	IDDerivedNamesFit    upgrade.ID = "derived-names-fit"
	IDDerivedNamesUnique upgrade.ID = "derived-names-unique"
)

// Names returns the checks that walk the naming rules. The registry is passed
// rather than a slice so that a rule the command line skipped is skipped here
// too, and so the report can name the rule that raised a finding.
func Names(rows *upgrade.Registry[upgrade.Derivation]) []upgrade.Check {
	return []upgrade.Check{
		derivedNamesFit(rows),
		derivedNamesUnique(rows),
	}
}

// everyStage is what the name checks run in. §19.10 puts them in all three:
// upgrade runs the preflight's code as a prerequisite, and migrate runs it
// again during validation, because an object created between the two phases has
// never been checked.
var everyStage = []upgrade.Stage{upgrade.StagePreflight, upgrade.StageUpgrade, upgrade.StageMigrate}

// derivedNamesFit is §19.10's checks 1 and 3: every derived name and label is
// within the limit that binds it, and is something the API server would accept.
//
// The two are one check because they are one question asked of one value. A
// name that is too long and a name carrying a character its syntax forbids are
// both refused by the same write, and both leave the reconciler requeueing
// against an object that reports nothing about the name that caused it.
//
// What is validated is the value the formula produces unbounded, because that
// is what the operator writes today. The bounded form is what a fix would
// produce, and reporting on it would report that every violation is already
// resolved.
func derivedNamesFit(rows *upgrade.Registry[upgrade.Derivation]) upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDDerivedNamesFit,
		Summary: "every derived name and label is within the limit that binds it, and is one the API server accepts",
		RunIn:   everyStage,
		Fn: func(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			var findings upgrade.Findings

			walk := applicable(rows, s)
			s.Report.Work(len(walk))

			for _, row := range walk {
				s.Report.Item(string(row.ID()))

				inputs, err := row.Inputs(ctx, s)
				if err != nil {
					return nil, fmt.Errorf("the naming rule %q could not read its inputs: %w", row.ID(), err)
				}

				for _, input := range inputs {
					derived := input.Derive(row.Formula())
					if !derived.Fits() {
						findings = append(findings, tooLong(row, input, derived))
						// The syntax check is not also run. Apimachinery
						// reports an overlong value as a syntax error of its
						// own, so asking both questions of one value reports
						// one violation twice, and the length is the more
						// actionable half. A value that is also malformed
						// surfaces that once its length is resolved.
						continue
					}
					if errs := atlaskube.Validate(derived.Kind, derived.Natural); len(errs) != 0 {
						findings = append(findings, malformed(row, input, derived, errs))
					}
				}
			}
			return findings, nil
		},
	}
}

// derivedNamesUnique is §19.10's check 2: no two source objects derive the same
// name.
//
// It covers three of §19.8's four routes: the ambiguous concatenations, the
// namespace-free value written onto a cluster-scoped object, and the
// StorageNodeSet retirement re-deriving from the cluster what is derived from
// the set today. The fourth is a kind becoming cluster-scoped, which is about
// an object's own identity rather than a derived name, and belongs to
// namespace-collapse.
//
// **A row's Space decides what a collision even is here**, and most rows are
// not in this check at all. A label that exists to be selected on is derived
// identically by every object it applies to, so a shared row is skipped: every
// worker of a set carries the same set label, and every storage node on a
// worker names the same worker. Of the rows that do have to be unique, the
// namespace decides the extent, since a ConfigMap name repeated in two
// namespaces collides with nothing while a cluster-scoped object's name does.
func derivedNamesUnique(rows *upgrade.Registry[upgrade.Derivation]) upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDDerivedNamesUnique,
		Summary: "no two objects derive one name, which two would silently become one object",
		RunIn:   everyStage,
		Fn: func(ctx context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			var findings upgrade.Findings

			walk := applicable(rows, s)
			s.Report.Work(len(walk))

			for _, row := range walk {
				s.Report.Item(string(row.ID()))
				if row.Space() == upgrade.SpaceShared {
					// A selector, which several objects are meant to derive
					// identically. Every worker of a set carries the same set
					// label, and reporting that is a finding about this check
					// rather than about the cluster.
					continue
				}

				inputs, err := row.Inputs(ctx, s)
				if err != nil {
					return nil, fmt.Errorf("the naming rule %q could not read its inputs: %w", row.ID(), err)
				}

				for _, group := range collisions(row, inputs) {
					findings = append(findings, collides(row, group))
				}
			}
			return findings, nil
		},
	}
}

// group is the sources that derived one value in one space.
type group struct {
	value   string
	sources []upgrade.Input
}

// scopedValue is what a collision is keyed on: the derived value, and the
// namespace it has to be unique within, which is empty for a value whose space
// is the whole cluster.
type scopedValue struct {
	value     string
	namespace string
}

// collisions returns the values more than one distinct source derived, sorted
// so two runs over one cluster report the same thing in the same order.
//
// Distinctness is by source object rather than by input. A row that enumerates
// a cross product, as the per-slot topology key does over clusters and nodes,
// hands one source several inputs, and those are one object's several values
// rather than a collision between two objects.
func collisions(row upgrade.Derivation, inputs []upgrade.Input) []group {
	byValue := make(map[scopedValue][]upgrade.Input)
	seen := make(map[scopedValue]map[upgrade.ObjectIdentity]bool)

	for _, input := range inputs {
		key := scopedValue{value: input.Derive(row.Formula()).Natural}
		if row.Space() == upgrade.SpaceNamespace {
			key.namespace = input.Source.Namespace
		}
		id := input.Source.Identity()

		if seen[key] == nil {
			seen[key] = make(map[upgrade.ObjectIdentity]bool)
		}
		if seen[key][id] {
			continue
		}
		seen[key][id] = true
		byValue[key] = append(byValue[key], input)
	}

	var out []group
	for key, sources := range byValue {
		if len(sources) > 1 {
			out = append(out, group{value: key.value, sources: sources})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].value < out[j].value })
	return out
}

// applicable returns the rows a run is to walk, leaving out the ones the
// command line named.
func applicable(rows *upgrade.Registry[upgrade.Derivation], s *upgrade.Scope) []upgrade.Derivation {
	return rows.Select(func(row upgrade.Derivation) bool {
		return !s.Options.Skipped(row.ID())
	})
}

// tooLong is the finding §19.11 prints for a value over its limit.
func tooLong(row upgrade.Derivation, input upgrade.Input, derived atlaskube.Derived) upgrade.Finding {
	return upgrade.Finding{
		Rule:     row.ID(),
		Severity: upgrade.SeverityError,
		Objects:  []upgrade.ObjectRef{input.Source},
		Summary: fmt.Sprintf("%s derives %s, which is %d bytes against a limit of %d",
			input.Source, row.Written(), len(derived.Natural), derived.Limit),
		PerObject:   []string{"from " + strings.Join(input.Parts, ", ")},
		Detail:      fmt.Sprintf("= %s\n%s", derived.Natural, why(row)),
		Remediation: string(row.Fix()),
	}
}

// malformed is the finding for a value the API server would refuse on syntax
// rather than on length.
func malformed(row upgrade.Derivation, input upgrade.Input, derived atlaskube.Derived, errs []string) upgrade.Finding {
	return upgrade.Finding{
		Rule:     row.ID(),
		Severity: upgrade.SeverityError,
		Objects:  []upgrade.ObjectRef{input.Source},
		Summary: fmt.Sprintf("%s derives %s, which the API server would refuse",
			input.Source, row.Written()),
		PerObject: []string{"from " + strings.Join(input.Parts, ", ")},
		Detail: fmt.Sprintf("= %s\n%s\n%s",
			derived.Natural, strings.Join(errs, "; "), why(row)),
		Remediation: string(row.Fix()),
	}
}

// collides is the finding for two objects reaching one value.
func collides(row upgrade.Derivation, g group) upgrade.Finding {
	sources := make([]upgrade.ObjectRef, 0, len(g.sources))
	from := make([]string, 0, len(g.sources))
	for _, source := range g.sources {
		sources = append(sources, source.Source)
		from = append(from, "from "+strings.Join(source.Parts, ", "))
	}

	return upgrade.Finding{
		Rule:      row.ID(),
		Severity:  upgrade.SeverityError,
		Objects:   sources,
		PerObject: from,
		Summary: fmt.Sprintf("%d objects derive one %s, which has to be unique in %s",
			len(g.sources), row.Written(), row.Space()),
		Detail:      fmt.Sprintf("= %s\n%s", g.value, why(row)),
		Remediation: collisionFix(row),
	}
}

// collisionFix is what resolves a collision, which is not always what resolves
// an overflow. Truncate-and-hash resolves both, because the digest covers the
// parts individually and two distinct inputs therefore reach two values.
// Bounding an input does not: two objects can be within the limit and still
// share a name, and renaming one of them is the only thing that separates them.
func collisionFix(row upgrade.Derivation) string {
	if row.Fix() == upgrade.FixTruncateAndHash {
		return string(upgrade.FixTruncateAndHash)
	}
	return "rename one of the objects, so the two stop deriving one value"
}

// why says whether the finding is about a cluster that is already broken or one
// the migration is about to break, which are not the same news to a user
// deciding whether to upgrade today.
func why(row upgrade.Derivation) string {
	if row.Model() == upgrade.ModelTarget {
		return "this is what the target model derives; the cluster is correct today"
	}
	return "this is what the operator derives today"
}

// §19.10's sixth check: no object carries both the old and the new spelling of
// a key with two different values.
//
// It is the one state §16.3's rewrite cannot resolve on the user's behalf.
// Everywhere else the rewrite is mechanical, since it writes the new key,
// preserves the value verbatim, and leaves the old key in place for the
// deprecation window. Two spellings that disagree are two answers to one
// question, and picking either would silently discard a value somebody set.
//
// Some keys have already half moved. The chart writes
// simplyblock.io/replication-policy while the operator reads
// storage.simplyblock.io/replication-policy, so a claim can carry both today,
// which is why this runs before the rewrite rather than as part of it.

package check

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/keys"
)

// IDAnnotationSpellings is the check's identity.
const IDAnnotationSpellings upgrade.ID = "annotation-spellings"

// AnnotationSpellings returns the check.
func AnnotationSpellings() upgrade.Check {
	return upgrade.CheckFunc{
		RuleID:  IDAnnotationSpellings,
		Summary: "no object carries two spellings of one key holding two different values",
		RunIn:   everyStage,
		Fn: func(_ context.Context, s *upgrade.Scope) (upgrade.Findings, error) {
			inventory := keys.Moved()
			s.Report.Work(len(inventory))

			findings := make(upgrade.Findings, 0, len(inventory))
			for _, key := range inventory {
				s.Report.Item(key.Old())
				findings = append(findings, conflictsFor(s, key)...)
			}
			return findings, nil
		},
	}
}

// conflictsFor examines every discovered object for one key.
//
// Every kind is walked rather than a list of the ones expected to carry a key.
// The keys sit on core objects as well as on custom resources, and the
// inventory records where each one is normally found without that being a
// promise about where it is.
func conflictsFor(s *upgrade.Scope, key keys.Key) upgrade.Findings {
	var findings upgrade.Findings
	for _, obj := range s.Graph.Objects() {
		for _, conflict := range key.Conflicts(obj.GetLabels(), obj.GetAnnotations()) {
			findings = append(findings, disagrees(s, obj, conflict))
		}
	}
	return findings
}

// disagrees renders one conflict.
func disagrees(s *upgrade.Scope, obj client.Object, conflict keys.Conflict) upgrade.Finding {
	return upgrade.Finding{
		Rule:     IDAnnotationSpellings,
		Severity: upgrade.SeverityError,
		Objects:  []upgrade.ObjectRef{s.Ref(obj)},
		Summary: fmt.Sprintf("%s carries two spellings of %s holding two different values",
			s.Ref(obj), conflict.Key.Name),
		Detail: fmt.Sprintf("%s = %s\n%s = %s\nthe rewrite writes the new spelling and cannot choose between two values",
			conflict.OldKey, conflict.OldValue, conflict.NewKey, conflict.NewValue),
		Remediation: fmt.Sprintf("remove whichever of the two is wrong; %s is normally what carries this key",
			conflict.Key.Carried),
	}
}

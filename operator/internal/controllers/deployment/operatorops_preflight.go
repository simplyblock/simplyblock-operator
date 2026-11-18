// Copies a discovery run's findings into its status and applies the waivers in
// spec.discover.ignoreFindings, so the result outlives the run's events.

package deployment

import (
	"cmp"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
)

// maxStatusFindings bounds status.findings to the CRD's MaxItems.
const maxStatusFindings = 512

// recordFindings copies the plan's findings into the run's status, Fail before
// Warn, and applies the waivers. A finding the planner gave no severity is a
// Warn: it is something for the reviewer to correct, not a blocker.
func recordFindings(ops *simplyblockv1alpha2.OperatorOps, found []discoverypkg.Finding) {
	findings := make([]simplyblockv1alpha2.PreflightFinding, 0, len(found))
	for _, f := range found {
		severity := simplyblockv1alpha2.PreflightSeverityWarn
		if f.Severity == discoverypkg.SeverityFail {
			severity = simplyblockv1alpha2.PreflightSeverityFail
		}
		findings = append(findings, simplyblockv1alpha2.PreflightFinding{
			Node: f.Node, Severity: severity, Reason: f.Reason, Message: f.Message, Remedy: f.Remedy,
		})
	}
	slices.SortStableFunc(findings, func(a, b simplyblockv1alpha2.PreflightFinding) int {
		return cmp.Compare(severityRank(a.Severity), severityRank(b.Severity))
	})
	if len(findings) > maxStatusFindings {
		findings = findings[:maxStatusFindings]
	}
	ops.Status.Findings = findings
	refreshWaivers(ops)
}

func severityRank(severity simplyblockv1alpha2.PreflightSeverity) int {
	if severity == simplyblockv1alpha2.PreflightSeverityFail {
		return 0
	}
	return 1
}

// refreshWaivers marks the findings spec.discover.ignoreFindings names as
// waived, recounts the Fail findings, and sets the Preflight condition,
// reporting whether anything changed.
func refreshWaivers(ops *simplyblockv1alpha2.OperatorOps) bool {
	var ignore []string
	if ops.Spec.Discover != nil {
		ignore = ops.Spec.Discover.IgnoreFindings
	}

	changed := false
	fails, waived := int32(0), 0
	for i := range ops.Status.Findings {
		f := &ops.Status.Findings[i]
		w := slices.Contains(ignore, f.Reason)
		if f.Waived != w {
			f.Waived, changed = w, true
		}
		if f.Severity != simplyblockv1alpha2.PreflightSeverityFail {
			continue
		}
		if w {
			waived++
		} else {
			fails++
		}
	}
	if ops.Status.FailCount != fails {
		ops.Status.FailCount, changed = fails, true
	}

	condition := metav1.Condition{
		Type:               simplyblockv1alpha2.OperatorOpsConditionPreflight,
		Status:             metav1.ConditionTrue,
		Reason:             "Passed",
		Message:            fmt.Sprintf("no Fail finding (%d waived)", waived),
		ObservedGeneration: ops.Generation,
	}
	if fails > 0 {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "Failed"
		condition.Message = fmt.Sprintf(
			"%d Fail finding(s) block approval of the draft. Fix them or name their reason in spec.discover.ignoreFindings",
			fails)
	}
	if meta.SetStatusCondition(&ops.Status.Conditions, condition) {
		changed = true
	}
	return changed
}

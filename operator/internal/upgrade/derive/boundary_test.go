// The boundary tests §30.8 asks for. Every row gets one input at the longest
// length that works, one a single byte longer, and one whose final character is
// the dash a label value may not end on.
//
// The expected numbers are the design's measured ones, so a test that disagrees
// with one has found either a formula change or an error in the audit. They are
// written as the fixed cost each formula spends, because that is what a reader
// can check against the literal in the row: the design's "a 37-character cluster
// name" is 63 less this table's 26.
//
// Validity is asserted with k8s.io/apimachinery/pkg/util/validation rather than
// with a regular expression written for the test, so the assertion tracks the
// API server rather than a second opinion about it.

package derive

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// boundary is one row's measured budget.
type boundary struct {
	// rule is the row.
	rule upgrade.ID

	// parts is how many names the formula joins, which decides how many
	// separators come out of the budget.
	parts int

	// fixed is what the prefix and the suffix spend.
	fixed int

	// longest is the total length of the inputs that still fits, and is what
	// the design's tables state per row.
	longest int
}

// The audit, as a table. Every entry is derived from the row's own literals and
// the limit its kind carries, and the arithmetic is written out so a change to
// either shows up here rather than in a passing test.
var boundaries = []boundary{
	// §19.2, against a label's 63 bytes.
	{rule: IDPoolNodeLabelKey, parts: 3, fixed: len("pool."), longest: 63 - 5 - 2},
	{rule: IDNodeTypeLabel, parts: 1, fixed: len("simplyblock-storage-plane-"), longest: 63 - 26},
	{rule: IDStorageClassCluster, parts: 1, fixed: 0, longest: 63},
	{rule: IDStorageClassPool, parts: 1, fixed: 0, longest: 63},
	{rule: IDNodeSetLabel, parts: 1, fixed: 0, longest: 63},
	{rule: IDWorkerLabel, parts: 1, fixed: 0, longest: 63},
	{rule: IDDrainNodeLabel, parts: 1, fixed: 0, longest: 63},
	{rule: IDStorageNodeUUIDKey, parts: 2, fixed: len("storage-node-uuid."), longest: 63 - 18 - 1},

	// §19.3, against an object name's 253.
	{rule: IDStorageClassName, parts: 3, fixed: len("simplyblock-"), longest: 253 - 12 - 2},
	{rule: IDPerNodeConfigMap, parts: 1, fixed: len("-per-node-config"), longest: 253 - 16},
	{rule: IDStorageNodeDaemonSet, parts: 1, fixed: len("simplyblock-storage-node-ds-"), longest: 253 - 28},
	{rule: IDAPIEndpointSlice, parts: 1, fixed: len("-storage-node-api-endpoints"), longest: 253 - 27},
	{rule: IDClusterSecret, parts: 1, fixed: len("simplyblock-cluster-"), longest: 253 - 20},
	{rule: IDUpgradeSecret, parts: 1, fixed: len("simplyblock--upgrade"), longest: 253 - 20},
	{rule: IDNodeRemoveOps, parts: 1, fixed: len("-remove"), longest: 253 - 7},
	{rule: IDRestoredBackup, parts: 1, fixed: len("-restored"), longest: 253 - 9},
	{rule: IDImportedBackup, parts: 1, fixed: len("-imported"), longest: 253 - 9},

	// The target-model rows share their formulas with the current-model ones,
	// so their budgets are the same. They are listed anyway, because a change
	// to one of the three that did not reach its counterpart is a divergence
	// this table catches.
	{rule: IDPerNodeConfigMapTarget, parts: 1, fixed: len("-per-node-config"), longest: 253 - 16},
	{rule: IDStorageNodeDaemonSetTarget, parts: 1, fixed: len("simplyblock-storage-node-ds-"), longest: 253 - 28},
	{rule: IDAPIEndpointSliceTarget, parts: 1, fixed: len("-storage-node-api-endpoints"), longest: 253 - 27},
}

// declared indexes every row by identity.
func declared(t *testing.T) map[upgrade.ID]upgrade.Derivation {
	t.Helper()

	out := make(map[upgrade.ID]upgrade.Derivation)
	for _, rule := range append(Labels(), Names()...) {
		out[rule.ID()] = rule
	}
	return out
}

// spread splits a total length across n parts, so a three-part formula is fed
// three names rather than one long one.
func spread(total, parts int) []string {
	out := make([]string, parts)
	for i := range out {
		size := total / parts
		if i == parts-1 {
			size = total - (total/parts)*(parts-1)
		}
		out[i] = strings.Repeat("a", size)
	}
	return out
}

func TestBoundary_TheLongestInputThatFitsFits(t *testing.T) {
	rules := declared(t)
	for _, want := range boundaries {
		rule, ok := rules[want.rule]
		if !ok {
			t.Errorf("%s is in the audit and in no catalog", want.rule)
			continue
		}

		got := rule.Formula().Derive(spread(want.longest, want.parts)...)
		if !got.Fits() {
			t.Errorf("%s: %d characters of input did not fit, and the audit says it is "+
				"the longest that does; the formula produced %d bytes against a limit of %d",
				want.rule, want.longest, len(got.Natural), got.Limit)
		}
	}
}

func TestBoundary_OneByteLongerDoesNot(t *testing.T) {
	rules := declared(t)
	for _, want := range boundaries {
		rule, ok := rules[want.rule]
		if !ok {
			continue
		}

		got := rule.Formula().Derive(spread(want.longest+1, want.parts)...)
		if got.Fits() {
			t.Errorf("%s: %d characters of input fit, and the audit says %d is the "+
				"longest that does", want.rule, want.longest+1, want.longest)
		}
	}
}

func TestBoundary_TheFixedCostIsWhatTheAuditSays(t *testing.T) {
	// The budget a row leaves its input is the limit less the formula's own
	// literals and its separators, and a row whose prefix somebody widened
	// silently takes characters away from a name a user already chose.
	rules := declared(t)
	for _, want := range boundaries {
		rule, ok := rules[want.rule]
		if !ok {
			continue
		}

		formula := rule.Formula()
		got := formula.Derive(spread(want.longest, want.parts)...)
		if fixed := len(got.Natural) - want.longest; fixed != want.fixed+want.parts-1 {
			t.Errorf("%s spends %d bytes on its own literals and separators, and the "+
				"audit says %d", want.rule, fixed, want.fixed+want.parts-1)
		}
	}
}

func TestBoundary_AnOverlongInputStillDerivesALegalIdentifier(t *testing.T) {
	// The row reports the violation, and the value it produces has to be one
	// the API server would accept: a migration that resolves a violation by
	// truncating cannot produce a second one.
	rules := declared(t)
	for _, want := range boundaries {
		rule, ok := rules[want.rule]
		if !ok {
			continue
		}

		got := rule.Formula().Derive(spread(want.longest*2, want.parts)...)
		if errs := got.Errors(); len(errs) != 0 {
			t.Errorf("%s truncated to %q, which the API server refuses: %v", want.rule, got.Value, errs)
		}
		if len(got.Value) > got.Limit {
			t.Errorf("%s truncated to %d bytes against a limit of %d", want.rule, len(got.Value), got.Limit)
		}
	}
}

func TestBoundary_AnInputEndingOnASeparatorIsStillLegal(t *testing.T) {
	// Truncation lands wherever the input happens to put a dash, and a label
	// value may not end on one. This is the case §19.2's drain-node row is
	// about, and it applies to every row.
	rules := declared(t)
	for _, want := range boundaries {
		rule, ok := rules[want.rule]
		if !ok {
			continue
		}

		parts := spread(want.longest*2, want.parts)
		for i := range parts {
			parts[i] = strings.Repeat("a", len(parts[i])/2) + strings.Repeat("-", len(parts[i])-len(parts[i])/2)
		}

		got := rule.Formula().Derive(parts...)
		if errs := got.Errors(); len(errs) != 0 {
			t.Errorf("%s derived %q from an input ending on a separator, which the "+
				"API server refuses: %v", want.rule, got.Value, errs)
		}
	}
}

func TestBoundary_EveryDeclaredRowIsInTheAudit(t *testing.T) {
	// A row somebody adds without a measured boundary is a row nothing checks,
	// which is how the next overflow reaches a cluster.
	audited := make(map[upgrade.ID]bool, len(boundaries))
	for _, want := range boundaries {
		audited[want.rule] = true
	}

	for id := range declared(t) {
		if !audited[id] {
			t.Errorf("%s is declared and has no measured boundary in this file", id)
		}
	}
}

func TestBoundary_TheLimitsAreTheAPIServersOwn(t *testing.T) {
	if atlaskube.MaxLabelValueLength != validation.LabelValueMaxLength {
		t.Fatalf("the label limit is %d, and apimachinery says %d",
			atlaskube.MaxLabelValueLength, validation.LabelValueMaxLength)
	}
	if atlaskube.MaxObjectNameLength != validation.DNS1123SubdomainMaxLength {
		t.Fatalf("the object-name limit is %d, and apimachinery says %d",
			atlaskube.MaxObjectNameLength, validation.DNS1123SubdomainMaxLength)
	}
}

func TestBoundary_EveryRowDeclaresWhatResolvesIt(t *testing.T) {
	// A finding with no remediation is a gap in the check rather than in the
	// cluster, and §19.11 requires that every violation names the change that
	// resolves it. Which of §19.5's three applies is a property of the row, so
	// a row added without one is a violation nobody can act on.
	for id, rule := range declared(t) {
		if rule.Fix() == "" {
			t.Errorf("%s declares no fix, so a violation of it would be reported "+
				"with nothing a user can do about it", id)
		}
	}
}

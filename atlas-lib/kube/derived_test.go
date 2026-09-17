// Tests for the derived-identifier formulas: the boundary at which a name stops
// fitting, the determinism two processes depend on, and the collisions the
// digest exists to prevent.

package kube

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestFormula_ShortInputIsUntouched(t *testing.T) {
	f := Formula{Kind: ObjectName, Prefix: "simplyblock-"}

	got := f.Derive("simplyblock", "cluster-a", "gold")
	if got.Value != "simplyblock-simplyblock-cluster-a-gold" {
		t.Fatalf("Value = %q, want the formula's own output unchanged", got.Value)
	}
	if !got.Fits() {
		t.Fatal("Fits() = false, want true: the name is far below the limit")
	}
	if got.Truncated {
		t.Fatal("Truncated = true, want false: nothing was cut")
	}
}

func TestFormula_LongInputIsTruncatedAndHashed(t *testing.T) {
	f := Formula{Kind: LabelValue, Prefix: "simplyblock-"}

	got := f.Derive(strings.Repeat("a", 200))
	if len(got.Value) != MaxLabelValueLength {
		t.Fatalf("len(Value) = %d, want the limit %d", len(got.Value), MaxLabelValueLength)
	}
	if got.Fits() {
		t.Fatal("Fits() = false expected: the natural form is over the limit")
	}
	if errs := validation.IsValidLabelValue(got.Value); len(errs) != 0 {
		t.Fatalf("the bounded value is not a legal label value: %v", errs)
	}
}

func TestFormula_TruncationDoesNotCollide(t *testing.T) {
	f := Formula{Kind: ObjectName, Prefix: "simplyblock-"}
	stem := strings.Repeat("x", 300)

	a := f.Derive(stem + "one")
	b := f.Derive(stem + "two")
	if a.Value == b.Value {
		t.Fatalf("two distinct inputs derived one name %q: the digest must cover "+
			"the whole input, not the truncated stem", a.Value)
	}
}

func TestFormula_SeparatorAmbiguityIsResolvedByTheDigest(t *testing.T) {
	// design-api-upgrade.md §19.8: cluster a-b with pool c and cluster a with
	// pool b-c join to one string, so the digest is taken over the parts rather
	// than over the joined form.
	f := Formula{Kind: ObjectName, Prefix: "simplyblock-"}

	if a, b := f.Derive("ns", "a-b", "c"), f.Derive("ns", "a", "b-c"); a.Digest == b.Digest {
		t.Fatalf("cluster a-b/pool c and cluster a/pool b-c share digest %q", a.Digest)
	}
}

func TestFormula_IsDeterministic(t *testing.T) {
	f := Formula{Kind: ObjectName, Prefix: "simplyblock-"}
	long := strings.Repeat("worker.eu-central-1.compute.internal.", 20)

	if first, second := f.Derive(long), f.Derive(long); first.Value != second.Value {
		t.Fatalf("Derive is not deterministic: %q then %q", first.Value, second.Value)
	}
}

func TestFormula_DoesNotEndOnASeparator(t *testing.T) {
	// A label value may not end on '-' or '.', and truncation lands wherever the
	// input happens to put one.
	f := Formula{Kind: LabelValue}

	got := f.Derive(strings.Repeat("a", 40) + strings.Repeat("-", 40))
	if errs := validation.IsValidLabelValue(got.Value); len(errs) != 0 {
		t.Fatalf("value %q is not a legal label value: %v", got.Value, errs)
	}
}

func TestFormula_SanitizesWhatAnObjectNameMayNotCarry(t *testing.T) {
	f := Formula{Kind: ObjectName}

	got := f.Derive("Worker_3")
	if errs := validation.IsDNS1123Subdomain(got.Value); len(errs) != 0 {
		t.Fatalf("value %q is not a DNS-1123 subdomain: %v", got.Value, errs)
	}
}

func TestFormula_NaturalReportsWhatTheFormulaWouldHaveProduced(t *testing.T) {
	f := Formula{Kind: LabelValue, Prefix: "prefix-"}
	cluster := "production-cluster-eu-central-1-primary"

	got := f.Derive(cluster)
	if got.Natural != "prefix-"+cluster {
		t.Fatalf("Natural = %q, want the unbounded form so a report can name it", got.Natural)
	}
	if got.Limit != MaxLabelValueLength {
		t.Fatalf("Limit = %d, want %d", got.Limit, MaxLabelValueLength)
	}
}

func TestFormula_LongestInputThatFits(t *testing.T) {
	// A prefix spends the limit before the input does, so the budget is the
	// limit less the prefix and the boundary is exact.
	f := Formula{Kind: LabelValue, Prefix: "simplyblock-storage-plane-"}
	budget := MaxLabelValueLength - len("simplyblock-storage-plane-")

	if got := f.Derive(strings.Repeat("c", budget)); !got.Fits() {
		t.Fatalf("a %d-character input must fit, got %d bytes", budget, len(got.Natural))
	}
	if got := f.Derive(strings.Repeat("c", budget+1)); got.Fits() {
		t.Fatalf("a %d-character input must not fit", budget+1)
	}
}

func TestFormula_SuffixAndSeparator(t *testing.T) {
	f := Formula{Kind: ObjectName, Suffix: "-per-node-config"}
	if got := f.Derive("nodeset-a"); got.Value != "nodeset-a-per-node-config" {
		t.Fatalf("Value = %q, want the suffix appended", got.Value)
	}

	key := Formula{Kind: LabelKeyName, Separator: ".", Prefix: "pool."}
	if got := key.Derive("simplyblock", "cluster-a", "gold"); got.Value != "pool.simplyblock.cluster-a.gold" {
		t.Fatalf("Value = %q, want the parts joined on the formula's separator", got.Value)
	}
}

// A formula whose parts join ambiguously needs the digest on every value it
// produces, not only on the ones that had to be cut. The test above proves the
// digest tells the two inputs apart; this one is about the name actually using
// it, which is the whole of the difference between a collision resolved and a
// collision merely detectable.
func TestFormula_AlwaysDigestSeparatesShortAmbiguousInputs(t *testing.T) {
	f := Formula{Kind: ObjectName, Prefix: "sb-", AlwaysDigest: true}

	a, b := f.Derive("a-b", "c"), f.Derive("a", "b-c")
	if a.Value == b.Value {
		t.Fatalf("a-b/c and a/b-c both derived %q, and neither was truncated", a.Value)
	}
	if a.Truncated || b.Truncated {
		t.Error("a name well inside the limit is reported as truncated")
	}
	if !a.Fits() {
		t.Error("Fits() is false for a name the limit never bound")
	}
}

// The digest is part of the formula's output, so the value it produces for an
// input that fits is the value, rather than something the bounding rewrote.
func TestFormula_AlwaysDigestIsPartOfTheNaturalName(t *testing.T) {
	f := Formula{Kind: ObjectName, Prefix: "sb-", AlwaysDigest: true}

	got := f.Derive("oops-1", "worker-3")
	if got.Value != got.Natural {
		t.Errorf("Value is %q and Natural is %q; nothing was cut, so they are one name",
			got.Value, got.Natural)
	}
	if !strings.HasSuffix(got.Value, "-"+got.Digest) {
		t.Errorf("%q does not carry the digest %q", got.Value, got.Digest)
	}
}

// And it still fits. The digest is spent out of the budget whether or not the
// stem needed cutting, which is what stops a formula from producing a legal name
// for a short input and an over-long one for a middling input.
func TestFormula_AlwaysDigestHoldsToTheLimit(t *testing.T) {
	f := Formula{Kind: ObjectName, Prefix: "sb-nodeprobe-", Limit: 63, AlwaysDigest: true}

	for _, length := range []int{1, 30, 40, 41, 42, 50, 200} {
		got := f.Derive("run", strings.Repeat("n", length))
		if len(got.Value) > 63 {
			t.Errorf("an input of %d derived %q, which is %d bytes",
				length, got.Value, len(got.Value))
		}
		if errs := validation.IsDNS1123Subdomain(got.Value); len(errs) != 0 {
			t.Errorf("an input of %d derived %q: %v", length, got.Value, errs)
		}
	}
}

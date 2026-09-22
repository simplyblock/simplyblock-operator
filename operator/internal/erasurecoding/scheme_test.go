// The schemes and the minimum-node table, checked against the two authorities
// they are mirrored from: simplyblock_core's SUPPORTED_ERASURE_CODING_SCHEMES
// and the product documentation's table.
//
// Both are written out here rather than derived, because a test that computes
// its expectation the way the code does would pass whatever the code said.

package erasurecoding

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The seven pairs simplyblock_core's SUPPORTED_ERASURE_CODING_SCHEMES holds, and
// nothing else. A scheme the control plane refuses is one a cluster is never
// built with, so admitting it here only moves the refusal to a place where the
// document is already immutable.
func TestTheSupportedSchemesAreTheControlPlanes(t *testing.T) {
	want := []Scheme{
		{DataChunks: 1, ParityChunks: 0},
		{DataChunks: 1, ParityChunks: 1},
		{DataChunks: 2, ParityChunks: 1},
		{DataChunks: 4, ParityChunks: 1},
		{DataChunks: 1, ParityChunks: 2},
		{DataChunks: 2, ParityChunks: 2},
		{DataChunks: 4, ParityChunks: 2},
	}

	got := Supported()
	if len(got) != len(want) {
		t.Fatalf("Supported() has %d schemes, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Supported()[%d] = %s, want %s", i, got[i], want[i])
		}
		if !want[i].IsSupported() {
			t.Errorf("%s reports itself unsupported", want[i])
		}
	}
}

// Everything else is refused, including the schemes that merely look plausible:
// a 3+1 is neither in the control plane's set nor buildable by it.
func TestAnUnlistedSchemeIsNotSupported(t *testing.T) {
	for _, scheme := range []Scheme{
		{DataChunks: 3, ParityChunks: 1},
		{DataChunks: 8, ParityChunks: 2},
		{DataChunks: 2, ParityChunks: 0},
		{DataChunks: 4, ParityChunks: 0},
		{DataChunks: 1, ParityChunks: 3},
		{DataChunks: 0, ParityChunks: 1},
	} {
		if scheme.IsSupported() {
			t.Errorf("%s is reported supported", scheme)
		}
	}
}

// The minimum-node table of the product documentation, verbatim. The formula
// behind it is ndcs+npcs nodes to place a stripe on plus one spare per tolerated
// failure, and the table is what the formula is checked against rather than the
// other way around.
func TestTheMinimumNodeCountIsTheDocumentedOne(t *testing.T) {
	for _, testCase := range []struct {
		scheme Scheme
		nodes  int
	}{
		{Scheme{DataChunks: 1, ParityChunks: 0}, 1},
		{Scheme{DataChunks: 1, ParityChunks: 1}, 3},
		{Scheme{DataChunks: 2, ParityChunks: 1}, 4},
		{Scheme{DataChunks: 4, ParityChunks: 1}, 6},
		{Scheme{DataChunks: 1, ParityChunks: 2}, 5},
		{Scheme{DataChunks: 2, ParityChunks: 2}, 6},
		{Scheme{DataChunks: 4, ParityChunks: 2}, 8},
	} {
		if got := testCase.scheme.MinimumNodes(); got != testCase.nodes {
			t.Errorf("%s needs %d nodes, want %d", testCase.scheme, got, testCase.nodes)
		}
	}
}

// A scheme renders as the notation the documentation, the control plane, and
// the cluster's own status all use, so a message names what a reviewer can look
// up.
func TestASchemeRendersAsTheDocumentedNotation(t *testing.T) {
	if got := (Scheme{DataChunks: 2, ParityChunks: 1}).String(); got != "2+1" {
		t.Errorf("String() = %q, want %q", got, "2+1")
	}
}

// An unstated stripe is 1+1, which is what the control plane defaults to and
// what the operator has always sent for a cluster that states none. A document
// that says nothing about erasure coding therefore describes a cluster needing
// three nodes, and that is the thing the minimum has to be checked against.
func TestAnUnstatedStripeIsOnePlusOne(t *testing.T) {
	for name, stripe := range map[string]*simplyblockv1alpha2.StripeSpec{
		"absent":            nil,
		"empty":             {},
		"parity alone":      {ParityChunks: ptr.To(int32(1))},
		"data chunks alone": {DataChunks: ptr.To(int32(1))},
	} {
		if got := SchemeOf(stripe); got != (Scheme{DataChunks: 1, ParityChunks: 1}) {
			t.Errorf("SchemeOf(%s) = %s, want 1+1", name, got)
		}
	}
}

// A stated stripe is read as it is written, including the 1+0 that is the only
// scheme a fleet of one or two workers can carry.
func TestAStatedStripeIsReadAsItIsWritten(t *testing.T) {
	stripe := &simplyblockv1alpha2.StripeSpec{
		DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(0)),
	}
	if got := SchemeOf(stripe); got != (Scheme{DataChunks: 1, ParityChunks: 0}) {
		t.Errorf("SchemeOf(1+0) = %s, want 1+0", got)
	}
}

// The supported set renders as one phrase, because a refusal that names the
// scheme it rejected without naming the alternatives leaves the reviewer to
// find them.
func TestTheSupportedSetRendersAsAPhrase(t *testing.T) {
	if got := SupportedNotation(); got != "1+0, 1+1, 2+1, 4+1, 1+2, 2+2, and 4+2" {
		t.Errorf("SupportedNotation() = %q", got)
	}
}

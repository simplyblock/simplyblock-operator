// What the draft proposes for erasure coding, against the fleet the run found.
//
// The proposal is the one number in the template that a reviewer cannot correct
// after approval, because the cluster's stripe is immutable, and it is also the
// one the document's validation refuses outright: a scheme needing more storage
// nodes than the fleet has is a draft nobody can approve. So the rule this file
// holds the generator to is that whatever it proposes, the fleet it was derived
// from can carry it.

package discovery

import (
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"

	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
)

// aFleetOf is a plan whose node sets name count workers, one group, which is
// what a discovery run produces for a uniform fleet.
func aFleetOf(count int) Plan {
	workers := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		workers = append(workers, "worker-"+string(rune('0'+i)))
	}
	return Plan{NodeSets: []simplyblockv1alpha2.NodeSet{{
		Name:   "discovered",
		Groups: []simplyblockv1alpha2.NodeGroup{{Name: "group-1", Workers: workers}},
	}}}
}

func proposedScheme(t *testing.T, workers int) erasurecoding.Scheme {
	t.Helper()
	template := ClusterTemplateFor("a-cluster", aFleetOf(workers))
	if template.Template.Stripe == nil {
		t.Fatalf("the draft for %d workers proposes no stripe at all", workers)
	}
	return erasurecoding.SchemeOf(template.Template.Stripe)
}

// The ladder the generator walks. A fleet that cannot carry a redundant scheme
// is proposed 1+0 rather than a scheme it fails validation against, and a fleet
// that can is proposed the widest stripe that leaves a spare per failure.
func TestTheDraftProposesASchemeTheFleetCanCarry(t *testing.T) {
	for _, testCase := range []struct {
		workers int
		scheme  string
	}{
		{1, "1+0"},
		{2, "1+0"},
		{3, "1+1"},
		{4, "2+1"},
		{6, "2+1"},
		{9, "2+1"},
	} {
		if got := proposedScheme(t, testCase.workers).String(); got != testCase.scheme {
			t.Errorf("a fleet of %d is proposed %s, want %s",
				testCase.workers, got, testCase.scheme)
		}
	}
}

// The invariant behind the ladder, held against every fleet size rather than
// the tabulated ones: the proposal is a scheme the control plane accepts, and
// the fleet meets its minimum. A draft failing either is one the reviewer can
// only rewrite.
func TestTheProposedSchemeIsOneTheFleetAndTheControlPlaneBothAccept(t *testing.T) {
	for workers := 0; workers <= 9; workers++ {
		scheme := proposedScheme(t, workers)
		if !scheme.IsSupported() {
			t.Errorf("a fleet of %d is proposed %s, which the control plane refuses",
				workers, scheme)
		}
		if workers > 0 && scheme.MinimumNodes() > workers {
			t.Errorf("a fleet of %d is proposed %s, which needs %d storage nodes",
				workers, scheme, scheme.MinimumNodes())
		}
	}
}

// A derived number a reviewer cannot account for is one they cannot correct
// with confidence, so the note says what was proposed and what the fleet it was
// derived from was.
func TestTheDraftAccountsForTheSchemeItProposed(t *testing.T) {
	template := ClusterTemplateFor("a-cluster", aFleetOf(4))

	joined := strings.Join(template.Notes, "\n")
	for _, want := range []string{"2+1", "4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the notes do not mention %q:\n%s", want, joined)
		}
	}
}

// A fleet of two carries nothing redundant, and the note says so rather than
// leaving a reviewer to discover that the draft they approved protects nothing.
func TestAFleetTooSmallForRedundancyIsSaidSo(t *testing.T) {
	template := ClusterTemplateFor("a-cluster", aFleetOf(2))

	joined := strings.ToLower(strings.Join(template.Notes, "\n"))
	if !strings.Contains(joined, "1+0") {
		t.Fatalf("the notes do not name the scheme proposed:\n%s", joined)
	}
	if !strings.Contains(joined, "1+1") || !strings.Contains(joined, "third") {
		t.Errorf("the notes do not say what a redundant scheme would need:\n%s", joined)
	}
}

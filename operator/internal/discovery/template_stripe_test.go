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
	template, err := ClusterTemplateFor("a-cluster", aFleetOf(workers), TemplateOptions{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}
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
		{1, "1+0"}, // a single node that names no devices carries nothing redundant
		{2, "1+1"}, // as a two-node cluster
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
		template, err := ClusterTemplateFor("a-cluster", aFleetOf(workers), TemplateOptions{})
		if err != nil {
			t.Fatalf("the fleet was refused: %v", err)
		}
		topology := erasurecoding.TopologyOf(template.Template.ClusterMode)
		if workers > 0 && scheme.MinimumNodesFor(topology) > workers {
			t.Errorf("a fleet of %d is proposed %s, which needs %d storage nodes",
				workers, scheme, scheme.MinimumNodesFor(topology))
		}
	}
}

// A derived number a reviewer cannot account for is one they cannot correct
// with confidence, so the note says what was proposed and what the fleet it was
// derived from was.
func TestTheDraftAccountsForTheSchemeItProposed(t *testing.T) {
	template, err := ClusterTemplateFor("a-cluster", aFleetOf(4), TemplateOptions{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}

	joined := strings.Join(template.Notes, "\n")
	for _, want := range []string{"2+1", "4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the notes do not mention %q:\n%s", want, joined)
		}
	}
}

// A fleet of two is proposed as a two-node cluster running 1+1, which survives
// the loss of either worker, rather than a 1+0 that protects nothing and could
// never be changed.
func TestAFleetOfTwoIsProposedAsATwoNodeCluster(t *testing.T) {
	template, err := ClusterTemplateFor("a-cluster", aFleetOf(2), TemplateOptions{})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}
	if template.Template.ClusterMode != simplyblockv1alpha2.ClusterModeDual {
		t.Fatalf("a fleet of two is proposed clusterMode %q, want Dual", template.Template.ClusterMode)
	}
	if got := erasurecoding.SchemeOf(template.Template.Stripe).String(); got != "1+1" {
		t.Errorf("a two-node fleet is proposed %s, want 1+1", got)
	}
	joined := strings.Join(template.Notes, "\n")
	if !strings.Contains(joined, "clusterMode Dual") || !strings.Contains(joined, "relaxed") {
		t.Errorf("the note does not account for the two-node proposal:\n%s", joined)
	}
}

// oneWorkerWith is a fleet of one worker handing over disks of these sizes.
func oneWorkerWith(sizes ...uint64) Plan {
	plan := fleetWithDisks(sizes)
	plan.NodeSets = aFleetOf(1).NodeSets
	return plan
}

// A single worker is proposed as a single-node cluster, and its scheme follows
// from the devices it hands over for data, since each chunk of a stripe lands
// on its own device (the control plane's device floor is ndcs+npcs at one
// node). A disk proposed for the journal is not one data lands on.
func TestASingleWorkerIsProposedBySchemeOfItsDevices(t *testing.T) {
	const tb = uint64(1) << 40
	for _, tc := range []struct {
		name    string
		plan    Plan
		scheme  string
		journal bool
	}{
		{"one disk", oneWorkerWith(tb), "1+0", false},
		{"two equal disks", oneWorkerWith(tb, tb), "1+1", false},
		{"three equal disks", oneWorkerWith(tb, tb, tb), "2+1", false},
		{"six equal disks", oneWorkerWith(tb, tb, tb, tb, tb, tb), "2+1", false},
		{"a small disk beside two", oneWorkerWith(tb/8, tb, tb), "1+1", true},
		{"a small disk beside three", oneWorkerWith(tb/8, tb, tb, tb), "2+1", true},
	} {
		template, err := ClusterTemplateFor("a-cluster", tc.plan, TemplateOptions{})
		if err != nil {
			t.Fatalf("%s: the fleet was refused: %v", tc.name, err)
		}
		if template.Template.ClusterMode != simplyblockv1alpha2.ClusterModeSingle {
			t.Errorf("%s: a single worker is proposed clusterMode %q, want Single",
				tc.name, template.Template.ClusterMode)
		}
		journal := template.Template.EnableJournalDevice != nil && *template.Template.EnableJournalDevice
		if journal != tc.journal {
			t.Errorf("%s: journal device proposed = %v, want %v", tc.name, journal, tc.journal)
		}
		scheme := erasurecoding.SchemeOf(template.Template.Stripe)
		if got := scheme.String(); got != tc.scheme {
			t.Errorf("%s: proposed %s, want %s", tc.name, got, tc.scheme)
		}
		if scheme.ParityChunks > 0 && !strings.Contains(strings.Join(template.Notes, "\n"), "device failure") {
			t.Errorf("%s: the note does not say what the parity protects against", tc.name)
		}
	}
}

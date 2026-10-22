// The member ranking of design-superclusters.md §7.2, steps 4 to 9: the
// filters, the topology preference, and the three strategies, against fixed
// members.

package placement

import (
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const gib = int64(1) << 30

// The regions the topology tests place members in.
const (
	westRegion  = "eu-west-1"
	eastRegion  = "eu-east-1"
	southRegion = "eu-south-1"
)

// member is an eligible member with a reading of total and used bytes.
func member(name string, total, used int64) candidate {
	return candidate{Name: name, UUID: "uuid-" + name, Eligible: true, HasReading: true, Total: total, Used: used}
}

// first picks the first of equals, so a test of the ranking is not a test of
// the draw.
func first(int) int { return 0 }

func choose(t *testing.T, strategy simplyblockv1alpha2.PlacementStrategy, size int64,
	topology requestTopology, members ...candidate,
) (string, map[string]string) {
	t.Helper()
	chosen, reasons := rank(strategy, members, size, topology, first)
	return chosen, reasons
}

// U-47: MostAvailableCapacity picks the member with the most free capacity.
func TestMostAvailableCapacityPicksTheMostFree(t *testing.T) {
	chosen, _ := choose(t, simplyblockv1alpha2.PlacementMostAvailableCapacity, gib, requestTopology{},
		member("a", 100*gib, 80*gib), member("b", 100*gib, 10*gib), member("c", 50*gib, 0))
	if chosen != "b" {
		t.Errorf("chosen = %q, want b with 90 GiB free", chosen)
	}
}

// U-48: LeastProvisioned picks the member with the lowest provisioned ratio.
func TestLeastProvisionedPicksTheLowestRatio(t *testing.T) {
	a, b := member("a", 100*gib, 0), member("b", 200*gib, 0)
	a.Provisioned, b.Provisioned = 50*gib, 60*gib
	chosen, _ := choose(t, simplyblockv1alpha2.PlacementLeastProvisioned, gib, requestTopology{}, a, b)
	if chosen != "b" {
		t.Errorf("chosen = %q, want b at 30%% provisioned", chosen)
	}
}

// U-170: LeastVolumes picks the member holding the fewest volumes.
func TestLeastVolumesPicksTheFewest(t *testing.T) {
	a, b := member("a", 100*gib, 0), member("b", 100*gib, 90*gib)
	a.Volumes, b.Volumes = 4, 1
	chosen, _ := choose(t, simplyblockv1alpha2.PlacementLeastVolumes, gib, requestTopology{}, a, b)
	if chosen != "b" {
		t.Errorf("chosen = %q, want b with one volume", chosen)
	}
}

// U-171: members that rank equal are chosen between at random.
func TestEqualMembersAreDrawnBetween(t *testing.T) {
	members := []candidate{member("a", 100*gib, 0), member("b", 100*gib, 0), member("c", 100*gib, 0)}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		pick := func(int) int { return i }
		chosen, _ := rank(simplyblockv1alpha2.PlacementMostAvailableCapacity, members, gib, requestTopology{}, pick)
		seen[chosen] = true
	}
	if len(seen) != 3 {
		t.Errorf("draws chose %v, want each of three equal members", seen)
	}
}

// The default strategy is MostAvailableCapacity.
func TestNoStrategyIsMostAvailableCapacity(t *testing.T) {
	chosen, _ := choose(t, "", gib, requestTopology{}, member("a", 100*gib, 80*gib), member("b", 100*gib, 10*gib))
	if chosen != "b" {
		t.Errorf("chosen = %q, want b", chosen)
	}
}

// U-50: a member not placement-eligible is never chosen, however empty.
func TestAnIneligibleMemberIsNeverChosen(t *testing.T) {
	cordoned := member("a", 1000*gib, 0)
	cordoned.Eligible = false
	chosen, reasons := choose(t, simplyblockv1alpha2.PlacementMostAvailableCapacity, gib, requestTopology{},
		cordoned, member("b", 100*gib, 90*gib))
	if chosen != "b" || !strings.Contains(reasons["a"], "eligible") {
		t.Errorf("chosen = %q, reasons = %v; want b, and a dropped as ineligible", chosen, reasons)
	}
}

// U-51: a member whose free capacity equals the requested size is eligible,
// and one byte less is not.
func TestFreeCapacityEqualToTheSizeIsEnough(t *testing.T) {
	exact := member("a", 100*gib, 90*gib)
	if chosen, _ := choose(t, "", 10*gib, requestTopology{}, exact); chosen != "a" {
		t.Errorf("a member with exactly the size free was not chosen")
	}
	short := member("a", 100*gib, 90*gib+1)
	if chosen, reasons := choose(t, "", 10*gib, requestTopology{}, short); chosen != "" || reasons["a"] == "" {
		t.Errorf("a member one byte short was chosen, reasons %v", reasons)
	}
}

// U-147, the ranking half: sizes placed since the reading count against free
// capacity.
func TestPlacedSizesCountAgainstFreeCapacity(t *testing.T) {
	a, b := member("a", 100*gib, 0), member("b", 100*gib, 10*gib)
	a.Placed = 20 * gib
	chosen, _ := choose(t, "", gib, requestTopology{}, a, b)
	if chosen != "b" {
		t.Errorf("chosen = %q, want b once a's 20 GiB placed since its reading count", chosen)
	}
}

// U-52: a member at or past its own critical threshold is dropped.
func TestAMemberPastItsCriticalThresholdIsDropped(t *testing.T) {
	critical := member("a", 100*gib, 91*gib)
	critical.CriticalPercent = 90
	chosen, reasons := choose(t, "", gib, requestTopology{}, critical, member("b", 100*gib, 95*gib))
	if chosen != "b" || !strings.Contains(reasons["a"], "critical") {
		t.Errorf("chosen = %q, reasons = %v; want b, and a dropped past its critical threshold", chosen, reasons)
	}
}

// U-53, the ranking half: a member without a reading is dropped, and the
// others are still ranked.
func TestAMemberWithoutAReadingIsDropped(t *testing.T) {
	unread := member("a", 0, 0)
	unread.HasReading = false
	chosen, reasons := choose(t, "", gib, requestTopology{}, unread, member("b", 100*gib, 50*gib))
	if chosen != "b" || reasons["a"] == "" {
		t.Errorf("chosen = %q, reasons = %v; want b", chosen, reasons)
	}
}

// U-54: no candidate is no choice, with every member's reason.
func TestNoCandidateNamesEveryReason(t *testing.T) {
	full, cordoned := member("a", 100*gib, 100*gib), member("b", 100*gib, 0)
	cordoned.Eligible = false
	chosen, reasons := choose(t, "", gib, requestTopology{}, full, cordoned)
	if chosen != "" || reasons["a"] == "" || reasons["b"] == "" {
		t.Errorf("chosen = %q, reasons = %v; want none, with a reason for each", chosen, reasons)
	}
}

// U-109: a preferred topology narrows candidates to members in its region.
func TestAPreferredRegionNarrowsTheCandidates(t *testing.T) {
	west, east := member("west", 100*gib, 90*gib), member("east", 100*gib, 0)
	west.Region, east.Region = westRegion, eastRegion
	chosen, _ := choose(t, "", gib, requestTopology{Preferred: []string{westRegion}}, west, east)
	if chosen != "west" {
		t.Errorf("chosen = %q, want west in the preferred region despite less room", chosen)
	}
}

// U-110: no member in the preferred region with room falls back to the
// requisite regions, then to every eligible member.
func TestTheRegionsFallBackInOrder(t *testing.T) {
	west, east, south := member("west", 100*gib, 100*gib), member("east", 100*gib, 50*gib), member("south", 100*gib, 0)
	west.Region, east.Region, south.Region = westRegion, eastRegion, southRegion

	topology := requestTopology{Preferred: []string{westRegion}, Requisite: []string{westRegion, eastRegion}}
	if chosen, _ := choose(t, "", gib, topology, west, east, south); chosen != "east" {
		t.Errorf("chosen = %q, want east from the requisite regions", chosen)
	}

	topology = requestTopology{Preferred: []string{westRegion}, Requisite: []string{westRegion}}
	if chosen, _ := choose(t, "", gib, topology, west, east, south); chosen != "south" {
		t.Errorf("chosen = %q, want south once no region of the request has room", chosen)
	}
}

// A member whose workers carry no region matches any region, as an unlabeled
// worker does at approval.
func TestAMemberWithoutARegionMatchesAny(t *testing.T) {
	unlabeled, east := member("unlabeled", 100*gib, 0), member("east", 100*gib, 90*gib)
	east.Region = eastRegion
	chosen, _ := choose(t, "", gib, requestTopology{Preferred: []string{westRegion}}, unlabeled, east)
	if chosen != "unlabeled" {
		t.Errorf("chosen = %q, want the unlabeled member, which matches eu-west-1", chosen)
	}
}

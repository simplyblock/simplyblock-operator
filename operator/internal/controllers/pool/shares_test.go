// The shares of a supercluster pool's ceiling that its members' backend pools
// are held to (design-superclusters.md §7.1): an even split over the members
// that accept new volumes, summing to the ceiling.

package pool

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// U-119: a ceiling of 100k over three members is 33,334, 33,333, and 33,333,
// and sums to the ceiling.
func TestAnEvenSplitSumsToTheCeiling(t *testing.T) {
	got := capacityShares(100000, []memberShare{
		{Name: "prod-a", Accepting: true},
		{Name: "prod-b", Accepting: true},
		{Name: "prod-c", Accepting: true},
	})
	want := map[string]int64{"prod-a": 33334, "prod-b": 33333, "prod-c": 33333}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("shares (-want +got):\n%s", diff)
	}
}

// U-120: a member leaving placement has its share divided among the rest. It
// keeps the share its backend pool already enforces, because its volumes stay
// and may still grow.
func TestAMemberLeavingPlacementHasItsShareDividedAmongTheRest(t *testing.T) {
	got := capacityShares(90000, []memberShare{
		{Name: "prod-a", Accepting: true, HasPool: true, Applied: 30000},
		{Name: "prod-b", Accepting: false, HasPool: true, Applied: 30000},
		{Name: "prod-c", Accepting: true, HasPool: true, Applied: 30000},
	})
	want := map[string]int64{"prod-a": 45000, "prod-b": 30000, "prod-c": 45000}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("shares (-want +got):\n%s", diff)
	}
}

// A member that does not accept volumes yet and has no backend pool is created
// with the share it would have once it does, so its pool is never unlimited.
func TestAMemberWithoutAPoolIsCreatedWithTheShareItWouldHave(t *testing.T) {
	got := capacityShares(90000, []memberShare{
		{Name: "prod-a", Accepting: true, HasPool: true, Applied: 45000},
		{Name: "prod-b", Accepting: true, HasPool: true, Applied: 45000},
		{Name: "prod-c"},
	})
	if got["prod-c"] != 30000 {
		t.Errorf("the new member's share = %d, want 30000", got["prod-c"])
	}
	if got["prod-a"] != 45000 || got["prod-b"] != 45000 {
		t.Errorf("the accepting members' shares = %v, want 45000 each", got)
	}
}

// No ceiling is no share: every member's backend pool is unlimited.
func TestAnUnlimitedPoolHasNoShares(t *testing.T) {
	got := capacityShares(0, []memberShare{{Name: "prod-a", Accepting: true}, {Name: "prod-b"}})
	for name, share := range got {
		if share != 0 {
			t.Errorf("%s's share = %d, want 0", name, share)
		}
	}
}

// No member accepts volumes: each member is held to the whole ceiling rather
// than to nothing, which the control plane would read as unlimited.
func TestNoAcceptingMemberHoldsEachToTheCeiling(t *testing.T) {
	got := capacityShares(90000, []memberShare{{Name: "prod-a"}, {Name: "prod-b"}})
	want := map[string]int64{"prod-a": 90000, "prod-b": 90000}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("shares (-want +got):\n%s", diff)
	}
}

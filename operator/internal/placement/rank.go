// The member ranking of design-superclusters.md §7.2, steps 4 to 9: which
// members may take a new volume, which of those the request's topology
// prefers, and which one the supercluster's strategy picks.
//
// It is a pure function of the members as the service read them, kept apart
// from the service that gathers the readings, because every filter and every
// tie is a row of the test plan and each is tested against fixed members.

package placement

import (
	"fmt"
	"math/big"
	"slices"
	"sort"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// candidate is one member as a placement sees it.
type candidate struct {
	Name, UUID string

	// Eligible is the supercluster's verdict: serving, placement not
	// disabled, and holding every pool.
	Eligible bool

	// HasReading says whether Total, Used, and Provisioned are a capacity
	// reading, and Placed is what the service placed in the member since it.
	HasReading  bool
	Total, Used int64
	Provisioned int64
	Placed      int64

	// CriticalPercent is the member's own critical threshold, zero for none.
	CriticalPercent int64

	// Volumes is the member's volume count, for LeastVolumes.
	Volumes int

	// Region is the region of the member's workers, empty when they carry
	// none, which matches any.
	Region string
}

// free is what the member can still take: its free physical capacity less
// what was placed in it since the reading.
func (c candidate) free() int64 { return c.Total - c.Used - c.Placed }

// requestTopology is the regions a request prefers and requires, in order.
type requestTopology struct {
	Preferred, Requisite []string
}

// rank picks the member for a volume of size bytes, and gives the reason each
// member it did not consider was dropped. An empty choice means no member can
// take the volume. draw picks among n members that rank equal.
func rank(
	strategy simplyblockv1alpha2.PlacementStrategy,
	members []candidate,
	size int64,
	topology requestTopology,
	draw func(n int) int,
) (string, map[string]string) {
	reasons := map[string]string{}
	room := make([]candidate, 0, len(members))
	for _, m := range members {
		if reason := dropped(m, size); reason != "" {
			reasons[m.Name] = reason
			continue
		}
		room = append(room, m)
	}

	candidates := inRegions(room, topology.Preferred)
	if len(candidates) == 0 {
		candidates = inRegions(room, topology.Requisite)
	}
	if len(candidates) == 0 {
		candidates = room
	}
	if len(candidates) == 0 {
		return "", reasons
	}

	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	best := make([]candidate, 0, len(candidates))
	best = append(best, candidates[0])
	for _, c := range candidates[1:] {
		switch compare(strategy, c, best[0]) {
		case -1:
			best = append(best[:0], c)
		case 0:
			best = append(best, c)
		}
	}
	return best[draw(len(best))].Name, reasons
}

// dropped says why a member cannot take a volume of size bytes, or nothing.
func dropped(m candidate, size int64) string {
	switch {
	case !m.Eligible:
		return "not placement-eligible"
	case !m.HasReading:
		return "no current capacity reading"
	case m.free() < size:
		return fmt.Sprintf("%d bytes free, %d requested", max(m.free(), 0), size)
	case m.CriticalPercent > 0 && m.Total > 0 && m.Used*100/m.Total >= m.CriticalPercent:
		return fmt.Sprintf("%d%% used, past its critical threshold of %d%%", m.Used*100/m.Total, m.CriticalPercent)
	}
	return ""
}

// inRegions is the members in any of the regions, a member without a region
// matching all of them. No regions is no narrowing to make, and nothing.
func inRegions(members []candidate, regions []string) []candidate {
	if len(regions) == 0 {
		return nil
	}
	var out []candidate
	for _, m := range members {
		if m.Region == "" || slices.Contains(regions, m.Region) {
			out = append(out, m)
		}
	}
	return out
}

// compare orders two members by the strategy: -1 when a ranks before b, 0
// when they rank equal.
func compare(strategy simplyblockv1alpha2.PlacementStrategy, a, b candidate) int {
	switch strategy {
	case simplyblockv1alpha2.PlacementLeastProvisioned:
		// Compared as cross products, so two ratios that are equal compare
		// equal rather than by rounding, in big integers, because the
		// product of two byte counts overflows 64 bits.
		left := new(big.Int).Mul(big.NewInt(a.Provisioned+a.Placed), big.NewInt(b.Total))
		right := new(big.Int).Mul(big.NewInt(b.Provisioned+b.Placed), big.NewInt(a.Total))
		return left.Cmp(right)
	case simplyblockv1alpha2.PlacementLeastVolumes:
		return sign(int64(a.Volumes - b.Volumes))
	default:
		return sign(b.free() - a.free())
	}
}

func sign(v int64) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	}
	return 0
}

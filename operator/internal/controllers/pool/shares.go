// The shares of a supercluster pool's ceiling that each member's backend pool
// is held to (design-superclusters.md §7.1).
//
// The control plane enforces a limit per backend pool, and a pool of a
// supercluster is one backend pool per member, so the pool's ceiling is divided
// among them. It is a pure function of the members, kept apart from the
// reconciler that writes the shares, because the split is the part of the
// fan-out with arithmetic in it. Only the capacity ceiling is divided: pool
// IOPS and throughput limits are refused on every pool until SPDK enforces them
// (P0-5), and §7.5's demand-driven shares replace the even split in Phase 5.

package pool

import "sort"

// memberShare is one member as the split sees it.
type memberShare struct {
	Name string

	// Accepting is whether the member accepts new volumes: serving, with
	// placement not disabled.
	Accepting bool

	// HasPool is whether the member's backend pool exists, and Applied the
	// capacity it enforces.
	HasPool bool
	Applied int64
}

// capacityShares is the capacity each member's backend pool is held to.
//
// The members accepting new volumes divide the ceiling evenly, the remainder
// going to the first in name order, so the shares sum to the ceiling. A member
// that accepts none keeps the share its backend pool enforces: its volumes stay
// and may grow, and new ones are not placed there. One without a backend pool
// is created with the share it would have once it accepts volumes, so its pool
// is never created unlimited, which is what a share of zero means to the
// control plane. A ceiling of zero is no ceiling, and every share is zero.
func capacityShares(ceiling int64, members []memberShare) map[string]int64 {
	shares := make(map[string]int64, len(members))
	if ceiling <= 0 {
		for _, m := range members {
			shares[m.Name] = 0
		}
		return shares
	}

	accepting := make([]string, 0, len(members))
	for _, m := range members {
		if m.Accepting {
			accepting = append(accepting, m.Name)
		}
	}
	for name, share := range evenShares(ceiling, accepting) {
		shares[name] = share
	}
	for _, m := range members {
		switch {
		case m.Accepting:
		case m.HasPool:
			shares[m.Name] = m.Applied
		default:
			shares[m.Name] = evenShares(ceiling, append(append([]string(nil), accepting...), m.Name))[m.Name]
		}
	}
	return shares
}

// evenShares divides a ceiling evenly among the named members, giving the
// remainder one unit at a time to the first members in name order.
func evenShares(ceiling int64, names []string) map[string]int64 {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	shares := make(map[string]int64, len(sorted))
	if len(sorted) == 0 {
		return shares
	}
	n := int64(len(sorted))
	base, remainder := ceiling/n, ceiling%n
	for i, name := range sorted {
		shares[name] = base
		if int64(i) < remainder {
			shares[name]++
		}
	}
	return shares
}

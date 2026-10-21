// Choosing the standby to promote (design-pnfs-mds-standby.md §7.3 Step 1).
//
// The choice depends only on the observed guests, so two passes deciding from
// the same observation choose the same pod and the record's conditioned write
// settles any that do not.

package mdsfailover

import (
	"slices"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// Candidate reports whether a guest can be promoted now: it has a session, is
// Ready, is not going away, and reports the standby role.
func Candidate(g Guest) bool {
	return g.HasSession && g.Ready && !g.Deleting && g.Role == nfsexportrpc.RoleStandby
}

// ChooseTarget picks the guest to promote. A preferred pod that is a candidate
// wins. Otherwise, a candidate in another zone than lostZone comes first, when
// both zones are known, and then the lowest ordinal.
func ChooseTarget(guests []Guest, lostZone, preferred string) (Guest, bool) {
	var candidates []Guest
	for _, g := range guests {
		if Candidate(g) {
			candidates = append(candidates, g)
		}
	}
	if len(candidates) == 0 {
		return Guest{}, false
	}
	elsewhere := func(g Guest) bool { return lostZone != "" && g.Zone != "" && g.Zone != lostZone }
	slices.SortFunc(candidates, func(a, b Guest) int {
		switch {
		case a.Pod == preferred && b.Pod != preferred:
			return -1
		case b.Pod == preferred && a.Pod != preferred:
			return 1
		case elsewhere(a) && !elsewhere(b):
			return -1
		case elsewhere(b) && !elsewhere(a):
			return 1
		}
		return a.Ordinal - b.Ordinal
	})
	return candidates[0], true
}

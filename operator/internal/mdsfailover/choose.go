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

// ChooseTarget picks the guest to promote: a preferred pod that is a
// candidate, otherwise the candidate with the lowest ordinal.
func ChooseTarget(guests []Guest, preferred string) (Guest, bool) {
	var candidates []Guest
	for _, g := range guests {
		if Candidate(g) {
			candidates = append(candidates, g)
		}
	}
	if len(candidates) == 0 {
		return Guest{}, false
	}
	slices.SortFunc(candidates, func(a, b Guest) int {
		switch {
		case a.Pod == preferred && b.Pod != preferred:
			return -1
		case b.Pod == preferred && a.Pod != preferred:
			return 1
		}
		return a.Ordinal - b.Ordinal
	})
	return candidates[0], true
}

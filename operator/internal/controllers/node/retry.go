// What a drain remembers about the moves that failed, and how it names and
// aims the next one with that memory.
//
// A failed move is kept until the step ends rather than deleted, because it is
// the record of where a subsystem could not go: the target it was headed for,
// and whether the control plane had accepted the migration before it failed.
// Reading the memory back from the objects is what makes it survive a restart
// without a status field of its own, and it counts each failure exactly once,
// because each failure is exactly one object.
//
// It lives apart from the steps in remove.go because it is a question about the
// fan-out's history rather than a step.
//
// design-storagenode.md §8.2 and §8.4 are the specification.

package node

import (
	"slices"
	"strconv"
	"strings"

	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// maxUnattributedFailures is how many failures that never involved a target
// rule it out. Such a failure is a migration the cluster did not accept in
// time, or a step that ran out before the control plane was asked, and it says
// little about the target; three of them on one target say enough.
const maxUnattributedFailures = 3

// moveHistory is what the failed moves of one subsystem say.
type moveHistory struct {
	// attempts is how many moves of the subsystem have failed, which numbers
	// the next one's name.
	attempts int

	// ruledOut are the targets the subsystem is not sent to again, sorted.
	ruledOut []string

	// unattributed counts, per target, the failures that never involved it.
	unattributed map[string]int

	// last is one failed move, for the event announcing the retry.
	last vmigration.Move
}

// subsystemKey is the identity a move is remembered under: the NVMe-oF
// subsystem it carries, or the volume alone when the control plane reports it
// under none.
func subsystemKey(nqn, pvName string) string {
	if nqn != "" {
		return nqn
	}
	return "pv:" + pvName
}

// failureHistories reads the failed moves into one history per subsystem.
//
// A failure rules its target out at once when the target took part in it: the
// control plane had accepted the migration, or its refusal named the target. A
// failure that involved no target is counted against the one it was headed for
// and rules it out only at maxUnattributedFailures.
func failureHistories(census volumeCensus, failed []vmigration.Move) map[string]*moveHistory {
	histories := map[string]*moveHistory{}
	for _, move := range failed {
		key := subsystemKey(census.subsystemOf(move.PVName), move.PVName)
		history, seen := histories[key]
		if !seen {
			history = &moveHistory{unattributed: map[string]int{}}
			histories[key] = history
		}
		history.attempts++
		history.last = move

		target := move.TargetNodeUUID
		if target == "" {
			continue
		}
		if involvedTarget(move) {
			history.ruleOut(target)
			continue
		}
		history.unattributed[target]++
		if history.unattributed[target] >= maxUnattributedFailures {
			history.ruleOut(target)
		}
	}
	return histories
}

// involvedTarget reports whether a failed move failed with its target taking
// part: the control plane accepted the migration, or refused it by naming the
// target.
func involvedTarget(move vmigration.Move) bool {
	return move.Engaged ||
		(move.TargetNodeUUID != "" && strings.Contains(move.Message, move.TargetNodeUUID))
}

func (h *moveHistory) ruleOut(target string) {
	if !slices.Contains(h.ruledOut, target) {
		h.ruledOut = append(h.ruledOut, target)
		slices.Sort(h.ruledOut)
	}
}

// retryName names a subsystem's next move: the first keeps the name a move of
// that volume always had, and each one after a failure carries the attempt
// number. Deriving it from the count of failed moves keeps it deterministic
// across passes, so starting it twice is one move. A name some other move
// already has is skipped, which covers a subsystem whose first volume changed
// between attempts.
func retryName(nodeID, pvName string, attempt int, taken map[string]struct{}) string {
	for {
		name := migrationName(nodeID, pvName)
		if attempt > 0 {
			name = migrationFormula.Derive(nodeID, pvName, "retry", strconv.Itoa(attempt)).Value
		}
		if _, used := taken[name]; !used {
			return name
		}
		attempt++
	}
}

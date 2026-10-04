package controller

import (
	"fmt"
	"strings"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// groupMemberSource is one clone slot of a group drill as the mapping sees it:
// the PVC it recovers and the source lvol it was resolved from.
type groupMemberSource struct {
	PVC    string
	LvolID string
}

// assignGenerationMembers pairs every clone slot with the generation member that
// holds THAT slot's data, returning the member index per slot. The pairing is by
// identity, never by position: the control plane lists a generation's members in
// no particular order, so a positional pairing can restore one member's disk onto
// another's PVC (the database disk onto the web volume).
//
// The key is the member's source lvol (source_lvol_id) when every member carries
// it. A control plane that does not report it yet returns only the replica volume
// on the target (lvol_id); then targetOf maps each slot's source lvol to its
// replica volume, read from the per-volume latest-snapshot, and the pairing is on
// the replica volume. Anything short of a one-to-one pairing is an error: a slot
// without a member, a member no slot claims, an empty or a duplicate key.
func assignGenerationMembers(
	slots []groupMemberSource,
	members []webapi.ReplicatedGroupSnapshot,
	targetOf map[string]string,
) ([]int, error) {
	bySource := true
	for i := range members {
		if members[i].SourceLvolID == "" {
			bySource = false
			break
		}
	}

	memberKey := func(m webapi.ReplicatedGroupSnapshot) string {
		if bySource {
			return m.SourceLvolID
		}
		return m.LvolID
	}
	index := make(map[string]int, len(members))
	for i := range members {
		k := memberKey(members[i])
		if k == "" {
			return nil, fmt.Errorf("generation member snapshot %s names no volume; cannot tell whose data it holds",
				members[i].SnapshotID)
		}
		if j, dup := index[k]; dup {
			return nil, fmt.Errorf("generation members %s and %s both belong to volume %s",
				members[j].SnapshotID, members[i].SnapshotID, k)
		}
		index[k] = i
	}

	assigned := make([]int, len(slots))
	used := make(map[int]bool, len(members))
	var missing []string
	for s, slot := range slots {
		k := slot.LvolID
		if !bySource {
			k = targetOf[slot.LvolID]
			if k == "" {
				missing = append(missing, fmt.Sprintf("%s (source volume %s has no replica volume on the target)",
					slot.PVC, slot.LvolID))
				continue
			}
		}
		i, ok := index[k]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (volume %s)", slot.PVC, k))
			continue
		}
		if used[i] {
			return nil, fmt.Errorf("generation member snapshot %s matches more than one PVC", members[i].SnapshotID)
		}
		used[i] = true
		assigned[s] = i
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the generation holds no snapshot for %s", strings.Join(missing, ", "))
	}
	var stray []string
	for i := range members {
		if !used[i] {
			stray = append(stray, fmt.Sprintf("%s (volume %s)", members[i].SnapshotID, memberKey(members[i])))
		}
	}
	if len(stray) > 0 {
		return nil, fmt.Errorf("generation snapshots %s match no PVC of the drill", strings.Join(stray, ", "))
	}
	return assigned, nil
}

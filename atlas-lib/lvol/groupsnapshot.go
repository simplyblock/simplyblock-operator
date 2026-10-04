// The group-snapshot handle: the identity of one consistency-group generation
// as it crosses the CSI boundary, {clusterID}:{poolRef}:{groupUUID}:{seq}. It
// embeds the generation identity the consistency-group design names
// ({group_uuid}:{group_seq}) and adds the cluster and pool so a Delete or Get
// can resolve the group without re-reading a source volume, which a
// delete-after-group-gone no longer has. It lives beside Handle because it is
// the same identity family: the volume handle's grammar plus a generation.
package lvol

import (
	"strconv"
	"strings"
)

// GroupSnapshotHandle is a group-snapshot id taken apart: the cluster, the
// pool reference (a UUID or a name, exactly as Handle.PoolRef), the group's
// UUID, and the generation number stamped on every member snapshot.
type GroupSnapshotHandle struct {
	ClusterID string
	PoolRef   string
	GroupID   string
	Seq       int
}

// NewGroupSnapshotHandle assembles the handle from its parts. The group id
// must be the bare group UUID, not the control plane's composite
// "{clusterID}/{uuid}" spelling.
func NewGroupSnapshotHandle(clusterID, poolRef, groupID string, seq int) GroupSnapshotHandle {
	return GroupSnapshotHandle{ClusterID: clusterID, PoolRef: poolRef, GroupID: groupID, Seq: seq}
}

// ParseGroupSnapshotHandle splits a group-snapshot id into its parts,
// reporting whether it was well formed. The cluster and group segments must
// be canonical UUIDs, the pool segment only non-empty (it may be a name), and
// the generation a non-negative integer. Surrounding whitespace is trimmed
// for the same reason ParseHandle trims it: ids are read back out of
// Kubernetes objects a human may have edited.
func ParseGroupSnapshotHandle(s string) (GroupSnapshotHandle, bool) {
	parts := strings.Split(strings.TrimSpace(s), handleSeparator)
	if len(parts) != 4 {
		return GroupSnapshotHandle{}, false
	}
	seq, err := strconv.Atoi(parts[3])
	if err != nil || seq < 0 {
		return GroupSnapshotHandle{}, false
	}
	if !IsCanonicalUUID(parts[0]) || parts[1] == "" || !IsCanonicalUUID(parts[2]) {
		return GroupSnapshotHandle{}, false
	}
	return GroupSnapshotHandle{ClusterID: parts[0], PoolRef: parts[1], GroupID: parts[2], Seq: seq}, true
}

// String renders the handle back into the form ParseGroupSnapshotHandle reads.
func (h GroupSnapshotHandle) String() string {
	return strings.Join(
		[]string{h.ClusterID, h.PoolRef, h.GroupID, strconv.Itoa(h.Seq)}, handleSeparator)
}

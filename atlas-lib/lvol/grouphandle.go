// Consistency-group volume handles: the identity a csi-addons VolumeGroup and
// its group-level replication verbs address, kept distinct from a per-volume
// handle so the driver's Replication verbs can route a whole group to the
// cluster-scoped group-replication endpoints instead of the per-volume ones
// (design-csi-addons-replication.md §14.4). It lives beside handle.go because it
// is the same handle grammar with a group sentinel.
package lvol

import "strings"

// groupHandlePrefix marks a handle as naming a consistency group rather than a
// single volume. A per-volume handle's first segment is a cluster UUID, so a
// non-UUID sentinel here keeps the two grammars unambiguous: ParseHandle rejects
// a group handle (its first segment is not a UUID), and ParseGroupHandle rejects
// a per-volume one (it lacks the sentinel).
const groupHandlePrefix = "cg"

// GroupHandle is a consistency-group handle taken apart: the cluster and the
// group it names. The csi-addons VolumeGroup service returns one of these as a
// group's replication handle. The cluster-scoped group-replication endpoints
// need only the cluster and the group id, so a handle carries no pool segment.
type GroupHandle struct {
	// ClusterID and GroupID are canonical UUIDs, spelled exactly as the handle
	// spells them (not normalized), for the same string-comparison reason
	// Handle keeps its segments verbatim.
	ClusterID string
	GroupID   string
}

// ParseGroupHandle splits a consistency-group handle (cg:{clusterID}:{groupID})
// into the cluster and group it names, reporting whether it was well formed.
// Both ids must be canonical UUIDs, and the leading "cg" sentinel is what
// distinguishes it from a per-volume handle. Surrounding whitespace is trimmed,
// as ParseHandle trims it, because a handle is read back out of a YAML object.
func ParseGroupHandle(h VolumeHandle) (GroupHandle, bool) {
	parts := strings.Split(strings.TrimSpace(string(h)), handleSeparator)
	if len(parts) != 3 || parts[0] != groupHandlePrefix {
		return GroupHandle{}, false
	}
	clusterID, groupID := parts[1], parts[2]
	if !IsCanonicalUUID(clusterID) || !IsCanonicalUUID(groupID) {
		return GroupHandle{}, false
	}
	return GroupHandle{ClusterID: clusterID, GroupID: groupID}, true
}

// String renders the group handle back into the form ParseGroupHandle reads.
func (h GroupHandle) String() string {
	return strings.Join([]string{groupHandlePrefix, h.ClusterID, h.GroupID}, handleSeparator)
}

// Handle renders the parts into a VolumeHandle.
func (h GroupHandle) Handle() VolumeHandle {
	return VolumeHandle(h.String())
}

// IsGroupHandle reports whether a handle names a consistency group rather than a
// single volume. The driver's Replication verbs branch on this to route a group
// handle to the group-replication endpoints (design §14.4).
func IsGroupHandle(h VolumeHandle) bool {
	_, ok := ParseGroupHandle(h)
	return ok
}

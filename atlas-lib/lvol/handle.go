package lvol

import (
	"strings"

	"github.com/google/uuid"
)

// handleSeparator divides a volume handle into its three segments.
const handleSeparator = ":"

// Handle is a volume handle taken apart: the cluster and volume it names, and
// however it refers to the pool.
type Handle struct {
	// ClusterID and VolumeID are canonical UUIDs, exactly as the handle spells
	// them. They are not normalized, because a handle is compared against
	// control-plane responses and sysfs attributes as a string, and rewriting
	// the spelling would silently stop those comparisons matching.
	ClusterID string
	VolumeID  string

	// PoolRef is a pool UUID or a pool name. Both occur: volumes provisioned
	// before the v2 API migration encode the pool's name, and a
	// PersistentVolume outlives every driver upgrade, so a cluster holds a
	// mixture indefinitely. Resolving a name to a UUID needs the control
	// plane, which is why it is not done here — see the package's Resolver.
	PoolRef string
}

// ParseHandle splits a volume handle into the cluster, pool, and volume it
// names, reporting whether the handle was well formed.
//
// The cluster and volume segments must be canonical UUIDs. The pool segment
// only has to be non-empty, since it may be a name. Surrounding whitespace is
// trimmed, for the same reason Split trims it: a handle is read back out of a
// PersistentVolume, which is a YAML document a human may have edited.
//
// It sits beside Split rather than inside it because the two answer different
// questions: Split asks which three UUIDs a handle encodes, ParseHandle asks
// what a handle says. Those differ for as long as the pool segment can be a
// name. Use Split when the caller genuinely needs typed UUIDs and can require
// that the pool is one.
func ParseHandle(h VolumeHandle) (Handle, bool) {
	parts := strings.Split(strings.TrimSpace(string(h)), handleSeparator)
	if len(parts) != 3 {
		return Handle{}, false
	}
	clusterID, poolRef, volumeID := parts[0], parts[1], parts[2]
	if !IsCanonicalUUID(clusterID) || poolRef == "" || !IsCanonicalUUID(volumeID) {
		return Handle{}, false
	}
	return Handle{ClusterID: clusterID, PoolRef: poolRef, VolumeID: volumeID}, true
}

// String renders the handle back into the form ParseHandle reads.
func (h Handle) String() string {
	return strings.Join([]string{h.ClusterID, h.PoolRef, h.VolumeID}, handleSeparator)
}

// Handle renders the parts into a VolumeHandle.
func (h Handle) Handle() VolumeHandle {
	return VolumeHandle(h.String())
}

// IsCanonicalUUID reports whether s is a UUID in the hyphenated 8-4-4-4-12
// form, which is the only form the control plane emits.
//
// The length check is what makes it canonical-only: uuid.Validate also accepts
// the braced (38), URN (45), and undashed (32) spellings, and an identifier
// carrying one of those would pass here and then match nothing, because every
// identifier it is compared against is spelled the canonical way.
//
// It is exported because the same question is asked outside handle parsing:
// whether a pool reference is an id or a name, and whether the `uuid`
// attribute a namespace publishes in sysfs is one at all.
func IsCanonicalUUID(s string) bool {
	return len(s) == 36 && uuid.Validate(s) == nil
}

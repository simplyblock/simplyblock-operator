package volumehandle

import (
	"fmt"
	"regexp"
	"strings"
)

type VolumeHandle struct {
	ClusterID string
	PoolID    string
	VolumeID  string
}

var NilVolumeHandle = VolumeHandle{}

// uuidRegex matches a standard UUID (8-4-4-4-12 hex, with hyphens). Compiled
// once at package scope so IsUUID stays allocation-free per call.
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s is a standard UUID (8-4-4-4-12 hex, with hyphens).
//
// Exported because the same question is asked outside handle parsing: a pool
// reference is either a UUID or a name, and a namespace's sysfs uuid attribute
// is only trusted when it is one.
func IsUUID(s string) bool {
	return uuidRegex.MatchString(s)
}

// Parse splits a CSI volume handle of the form "{clusterID}:{poolID}:{lvolID}"
// into its three components. clusterID and lvolID must be UUIDs; poolID may be
// either a pool UUID or a pool name, so it only has to be non-empty. The second
// return value is false (and the handle is the zero value) when the input is
// not exactly three colon-separated parts or any part fails its check.
func Parse(handle string) (VolumeHandle, bool) {
	ids := strings.Split(strings.TrimSpace(handle), ":")
	if len(ids) != 3 {
		return NilVolumeHandle, false
	}
	clusterID, poolID, lvolID := ids[0], ids[1], ids[2]
	if !IsUUID(clusterID) || poolID == "" || !IsUUID(lvolID) {
		return NilVolumeHandle, false
	}
	return VolumeHandle{clusterID, poolID, lvolID}, true
}

// MustParse is like Parse but panics if the handle is malformed. Use it only
// for handles already known to be valid (constructed internally or in tests),
// never for cluster- or user-supplied input.
func MustParse(handle string) VolumeHandle {
	parsed, ok := Parse(handle)
	if !ok {
		panic(
			fmt.Sprintf(
				"volumehandle: invalid handle %q (expected {clusterID}:{poolID}:{lvolID} with UUID cluster/lvol)",
				handle,
			),
		)
	}
	return parsed
}

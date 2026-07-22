package nqn

import (
	"fmt"
	"strings"
)

// DefaultPrefix is the simplyblock NQN naming-authority prefix, as observed
// on live subsystems (subsysnqn attribute).
const DefaultPrefix = "nqn.2023-02.io.simplyblock"

// lvolMarker is the literal segment separating the cluster id from the lvol
// id in a logical-volume subsystem NQN.
const lvolMarker = "lvol"

// Subsystem identifies a simplyblock logical-volume subsystem. The on-wire
// NQN is formed as:
//
//	<Prefix>:<ClusterID>:lvol:<LvolID>
//
// e.g. nqn.2023-02.io.simplyblock:c30a691a-...:lvol:792e184c-...
type Subsystem struct {
	Prefix    string
	ClusterID string
	LvolID    string
}

// BuildWithPrefix returns the Subsystem for a logical volume under a custom
// naming-authority prefix. Call String on the result for the on-wire NQN, or
// use MakeWithPrefix to get that string in one step. Use Build for the
// simplyblock DefaultPrefix.
func BuildWithPrefix(prefix, clusterID, logicalVolumeID string) Subsystem {
	return Subsystem{Prefix: prefix, ClusterID: clusterID, LvolID: logicalVolumeID}
}

// Build returns the Subsystem for a logical volume in a cluster using the
// simplyblock DefaultPrefix. Call String on the result for the on-wire NQN, or
// use Make to get that string in one step.
func Build(clusterID, logicalVolumeID string) Subsystem {
	return BuildWithPrefix(DefaultPrefix, clusterID, logicalVolumeID)
}

// String renders the subsystem NQN.
func (s Subsystem) String() string {
	return fmt.Sprintf("%s:%s:%s:%s", s.Prefix, s.ClusterID, lvolMarker, s.LvolID)
}

// MakeWithPrefix composes a logical-volume subsystem NQN string in a single
// call under a custom naming-authority prefix:
//
//	<prefix>:<clusterID>:lvol:<logicalVolumeID>
//
// It is the one-shot string equivalent of BuildWithPrefix(...).String(). Use
// Make for the simplyblock DefaultPrefix.
func MakeWithPrefix(prefix, clusterID, logicalVolumeID string) string {
	return prefix + ":" + clusterID + ":" + lvolMarker + ":" + logicalVolumeID
}

// Make composes the on-wire subsystem NQN string for a logical volume in a
// cluster using the simplyblock DefaultPrefix — the one-shot string equivalent
// of Build(...).String().
func Make(clusterID, logicalVolumeID string) string {
	return MakeWithPrefix(DefaultPrefix, clusterID, logicalVolumeID)
}

// Parse parses a logical-volume subsystem NQN of the form
// "<prefix>:<clusterID>:lvol:<logicalVolumeID>". ok is false if it does not match.
func Parse(nqn string) (s Subsystem, ok bool) {
	rest, logicalVolumeID, found := strings.Cut(nqn, ":"+lvolMarker+":")
	if !found {
		return Subsystem{}, false
	}
	prefix, clusterID, found := cutLast(rest, ":")
	if !found || prefix == "" || clusterID == "" || logicalVolumeID == "" {
		return Subsystem{}, false
	}
	return Subsystem{Prefix: prefix, ClusterID: clusterID, LvolID: logicalVolumeID}, true
}

// cutLast splits s around the last instance of sep.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

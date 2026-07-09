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

// BuildLvol composes the subsystem NQN for a logical volume in a cluster
// using DefaultPrefix.
func BuildLvol(clusterID, lvolID string) string {
	return Subsystem{Prefix: DefaultPrefix, ClusterID: clusterID, LvolID: lvolID}.String()
}

// String renders the subsystem NQN.
func (s Subsystem) String() string {
	return fmt.Sprintf("%s:%s:%s:%s", s.Prefix, s.ClusterID, lvolMarker, s.LvolID)
}

// ParseLvol parses a logical-volume subsystem NQN of the form
// "<prefix>:<clusterID>:lvol:<lvolID>". ok is false if it does not match.
func ParseLvol(nqn string) (s Subsystem, ok bool) {
	rest, lvolID, found := strings.Cut(nqn, ":"+lvolMarker+":")
	if !found {
		return Subsystem{}, false
	}
	prefix, clusterID, found := cutLast(rest, ":")
	if !found || prefix == "" || clusterID == "" || lvolID == "" {
		return Subsystem{}, false
	}
	return Subsystem{Prefix: prefix, ClusterID: clusterID, LvolID: lvolID}, true
}

// cutLast splits s around the last instance of sep.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

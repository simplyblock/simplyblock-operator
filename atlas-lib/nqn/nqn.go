package nqn

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// DefaultPrefix is the simplyblock NQN naming-authority prefix, as observed
// on live subsystems (subsysnqn attribute).
const DefaultPrefix = "nqn.2023-02.io.simplyblock"

// lvolMarker is the literal segment separating the cluster id from the lvol
// id in a logical-volume subsystem NQN.
const lvolMarker = "lvol"

// HostPrefix is the naming-authority prefix of a simplyblock *host* NQN — the
// initiator identity, not a subsystem. It differs from DefaultPrefix in its
// date segment because the two were coined at different times and both are
// on-wire values now; neither can be derived from the other.
const HostPrefix = "nqn.2014-08.io.simplyblock"

// uuidMarker is the literal segment introducing the UUID of a UUID-based host
// NQN, in both the simplyblock and the NVMe-spec (nqn.2014-08.org.nvmexpress)
// forms.
const uuidMarker = "uuid"

// Host composes the host NQN for a Kubernetes node from its UID:
//
//	nqn.2014-08.io.simplyblock:uuid:<nodeUID>
//
// This is the identity an access-controlled pool authorizes. The operator
// registers exactly this string in the pool's allowed_hosts for every allowed
// node, and the CSI driver on that node must present exactly this string on
// connect and when asking the control plane to resolve a connection
// (lvol.ForHost) — so the two derive it here rather than each spelling out the
// format, which is what makes them agree.
//
// The node's UID is the right seed and the node name is not: a node rebuilt
// under the same name is a different host, and its old authorization should
// not carry over to it.
func Host(nodeUID string) string {
	return HostPrefix + ":" + uuidMarker + ":" + nodeUID
}

// HostUUID returns the UUID a UUID-based host NQN names, for both the
// simplyblock form Host builds and the NVMe-spec nqn.2014-08.org.nvmexpress
// one, and reports whether the NQN carried one at all.
//
// It is prefix-agnostic on purpose: what a caller needs from a host NQN is the
// UUID identifying the host, and an NQN in either form identifies it the same
// way. The UUID is returned exactly as spelled, since it must match what the
// NQN says character for character — the kernel pairs hostid with hostnqn by
// comparison, not by parsing.
func HostUUID(hostNQN string) (string, bool) {
	_, id, found := cutLast(hostNQN, ":"+uuidMarker+":")
	if !found || uuid.Validate(id) != nil {
		return "", false
	}
	return id, true
}

// Subsystem identifies a simplyblock logical-volume subsystem. The on-wire
// NQN is formed as:
//
//	<Prefix>:<ClusterID>:lvol:<LvolID>
//
// e.g., nqn.2023-02.io.simplyblock:c30a691a-...:lvol:792e184c-...
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
// `<prefix>:<clusterID>:lvol:<logicalVolumeID>`. ok is false if it does not match.
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

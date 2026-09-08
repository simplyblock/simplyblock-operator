// Package nqn parses the identities carried in an NVMe Qualified Name: which
// cluster and logical volume a subsystem NQN names, and which host a host NQN
// names.
//
// It is a leaf because three layers read the same strings from three different
// sources — the control plane's connect response, the kernel's list-subsys
// output, and a Kubernetes node UID — and none of them should have to import
// another to agree on what an NQN means.
//
// atlas-lib's nqn package already builds and parses these. This package exists
// only until the CSI driver adopts it; see docs/internal-package-layout.md.
package nqn

import "strings"

// LvolIDFromNQN splits a simplyblock subsystem NQN of the form
// <prefix>:<clusterID>:lvol:<lvolID> into its cluster and logical volume IDs.
// Both are empty when the NQN does not carry an lvol segment.
func LvolIDFromNQN(nqn string) (clusterID, lvolID string) {
	parts := strings.Split(nqn, ":lvol:")
	if len(parts) > 1 {
		subparts := strings.Split(parts[0], ":")
		clusterID := subparts[len(subparts)-1]
		lvolID := parts[1]
		return clusterID, lvolID
	}
	return "", ""
}

// HostIDFromHostNQN derives a --hostid from an
// nqn.2014-08.io.simplyblock:uuid:<uuid>-style host NQN by taking its UUID
// suffix, or "" if hostNQN doesn't carry one (e.g. empty, or some other NQN
// format not built by this codebase).
func HostIDFromHostNQN(hostNQN string) string {
	i := strings.LastIndex(hostNQN, ":")
	if i < 0 || i == len(hostNQN)-1 {
		return ""
	}
	return hostNQN[i+1:]
}

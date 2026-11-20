// Reading the NFS client's own per-operation counters out of
// /proc/self/mountstats.
//
// It is the only place the kernel says how a pNFS client actually moved data.
// A SCSI layout is invisible from the outside: the mount looks the same, the
// file appears, and the bytes land either way, through the metadata server if
// the layout was refused and past it if it was not. The counters tell those
// apart, and nothing else a test can reach does.

package e2e

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/nfsclient"
)

// parseNFSOpCounts is the number of each NFS operation the client has issued
// against the mount serving mountPath, keyed by the operation name as
// mountstats spells it (WRITE, LAYOUTGET, LAYOUTCOMMIT).
//
// The mount is found by path first. A pod sees its volume as a bind of the
// staging mount, and the kernel names the bind's own target, so the path a test
// knows may not be the one mountstats prints: when it is not there, the single
// NFS mount in the namespace is the volume under test. Two of them is
// ambiguous, and guessing would attribute another mount's writes to this one.
func parseNFSOpCounts(mountstats, mountPath string) (map[string]int64, error) {
	mounts := nfsclient.ParseMountstats(mountstats)
	if len(mounts) == 0 {
		return nil, fmt.Errorf("mountstats names no NFS mount")
	}

	for _, m := range mounts {
		if m.MountPoint == mountPath {
			return m.Ops, nil
		}
	}
	if len(mounts) == 1 {
		return mounts[0].Ops, nil
	}

	paths := make([]string, 0, len(mounts))
	for _, m := range mounts {
		paths = append(paths, m.MountPoint)
	}
	return nil, fmt.Errorf(
		"mountstats has no NFS mount on %s, and names %d of them (%s), "+
			"so which one served the write cannot be told", mountPath, len(mounts), strings.Join(paths, ", "))
}

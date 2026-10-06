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
	"strconv"
	"strings"
)

// nfsFSTypes are the mount types these counters exist for.
var nfsFSTypes = map[string]bool{"nfs": true, "nfs4": true}

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
	blocks := nfsBlocks(mountstats)
	if len(blocks) == 0 {
		return nil, fmt.Errorf("mountstats names no NFS mount")
	}

	if counts, ok := blocks[mountPath]; ok {
		return counts, nil
	}
	if len(blocks) == 1 {
		for _, counts := range blocks {
			return counts, nil
		}
	}

	paths := make([]string, 0, len(blocks))
	for path := range blocks {
		paths = append(paths, path)
	}
	return nil, fmt.Errorf(
		"mountstats has no NFS mount on %s, and names %d of them (%s), "+
			"so which one served the write cannot be told", mountPath, len(blocks), strings.Join(paths, ", "))
}

// nfsBlocks is the per-operation counters of every NFS mount in the file, keyed
// by the path each is mounted on.
func nfsBlocks(mountstats string) map[string]map[string]int64 {
	blocks := make(map[string]map[string]int64)

	var path string
	var counting bool
	for _, line := range strings.Split(mountstats, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "device ") {
			// A new mount ends whatever was being counted, including one whose
			// per-op section never arrived.
			path, counting = mountedNFSPath(trimmed), false
			continue
		}
		if path == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "per-op statistics") {
			counting = true
			blocks[path] = make(map[string]int64)
			continue
		}
		if !counting {
			continue
		}
		if op, count, ok := opCount(trimmed); ok {
			blocks[path][op] = count
		}
	}
	return blocks
}

// mountedNFSPath is where an NFS mount's device line says it is mounted, and is
// empty for a line describing any other filesystem.
//
// A device line reads `device <source> mounted on <path> with fstype <type>`.
// The path is taken between those two markers rather than by field index,
// because a source or a mount point may contain spaces.
func mountedNFSPath(line string) string {
	const on, with = " mounted on ", " with fstype "

	start := strings.Index(line, on)
	end := strings.LastIndex(line, with)
	if start < 0 || end < 0 || end <= start {
		return ""
	}
	fields := strings.Fields(line[end+len(with):])
	if len(fields) == 0 || !nfsFSTypes[fields[0]] {
		return ""
	}
	return line[start+len(on) : end]
}

// opCount reads one "OPNAME: <issued> <transmitted> ..." row. The first number
// is how many of that operation the client issued, which is the one that says
// whether data took this path at all.
func opCount(line string) (op string, count int64, ok bool) {
	name, rest, found := strings.Cut(line, ":")
	if !found {
		return "", 0, false
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	issued, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name, issued, true
}

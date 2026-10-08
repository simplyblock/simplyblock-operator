// The mountstats parser: one Mount per NFS mount in the file.

package nfsclient

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// MountstatsPath is the calling process's own table. A process sees the mounts
// of its mount namespace, so a container reads its own and the host's only
// where they are shared with it.
const MountstatsPath = "/proc/self/mountstats"

// nfsFSTypes are the mount types mountstats writes NFS statistics for.
var nfsFSTypes = map[string]bool{"nfs": true, "nfs4": true}

// Mount is one NFS mount as mountstats describes it.
type Mount struct {
	// Device is the mount's source, `<server>:<path>`.
	Device     string
	MountPoint string
	FSType     string
	// LayoutTypes are the pNFS layout types the mount uses (LAYOUT_SCSI,
	// LAYOUT_BLOCK_VOLUME), empty when it uses none.
	LayoutTypes []string
	// Connects is how many times the client's transport to the server has
	// connected. A bind of a mount and every mount of the same server share
	// the transport, and with it the count.
	Connects uint64
	// Ops is the number of each operation the client issued, keyed by the
	// name mountstats spells it with (READ, WRITE, LAYOUTGET).
	Ops map[string]int64
}

// UsesLayout reports whether the mount uses the layout type.
func (m Mount) UsesLayout(layoutType string) bool {
	return slices.Contains(m.LayoutTypes, layoutType)
}

// ReadMountstats parses the calling process's mountstats.
func ReadMountstats() ([]Mount, error) {
	data, err := os.ReadFile(MountstatsPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", MountstatsPath, err)
	}
	return ParseMountstats(string(data)), nil
}

// ParseMountstats reads every NFS mount out of the file, in the file's order.
// Other filesystems have a device line and nothing under it, and are skipped.
func ParseMountstats(mountstats string) []Mount {
	var mounts []Mount
	var current *Mount
	var perOp bool
	for _, line := range strings.Split(mountstats, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "device ") {
			// A new mount ends the one being read, whether or not its per-op
			// section arrived.
			current, perOp = nil, false
			if m, ok := parseDeviceLine(trimmed); ok {
				mounts = append(mounts, m)
				current = &mounts[len(mounts)-1]
			}
			continue
		}
		if current == nil {
			continue
		}
		key, value, _ := strings.Cut(trimmed, ":")
		switch {
		case trimmed == "per-op statistics":
			perOp = true
		case perOp:
			if count, ok := issued(value); ok && key != "" && !strings.ContainsAny(key, " \t") {
				current.Ops[key] = count
			}
		case key == "nfsv4":
			current.LayoutTypes = layoutTypes(value)
		case key == "xprt":
			current.Connects = connects(value)
		}
	}
	return mounts
}

// parseDeviceLine reads `device <source> mounted on <path> with fstype <type>`,
// taking the path between the two markers rather than by field index because a
// source or a mount point may contain spaces.
func parseDeviceLine(line string) (Mount, bool) {
	const on, with = " mounted on ", " with fstype "
	start := strings.Index(line, on)
	end := strings.LastIndex(line, with)
	if start < 0 || end <= start {
		return Mount{}, false
	}
	fields := strings.Fields(line[end+len(with):])
	if len(fields) == 0 || !nfsFSTypes[fields[0]] {
		return Mount{}, false
	}
	return Mount{
		Device:     strings.TrimPrefix(line[:start], "device "),
		MountPoint: line[start+len(on) : end],
		FSType:     fields[0],
		Ops:        map[string]int64{},
	}, true
}

// layoutTypes reads `pnfs=` out of the nfsv4 line. The kernel writes the layout
// driver's name there, or `not configured` when the mount has none.
func layoutTypes(nfsv4 string) []string {
	for _, field := range strings.Split(nfsv4, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || name != "pnfs" || !strings.HasPrefix(value, "LAYOUT_") {
			continue
		}
		return strings.Fields(value)
	}
	return nil
}

// connects reads the connect count out of a TCP or RDMA xprt line:
// `<proto> <srcport> <bind count> <connect count> ...`. UDP has no connections
// and no such field.
func connects(xprt string) uint64 {
	fields := strings.Fields(xprt)
	if len(fields) < 4 || fields[0] == "udp" {
		return 0
	}
	n, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// issued is the first number of an operation's row, how many the client issued.
func issued(row string) (int64, bool) {
	fields := strings.Fields(row)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	return n, err == nil
}

// The pNFS client path: what NodeStageVolume does when the volume is an export
// rather than a block device.
//
// The shape is the ordinary one plus a device alias. The namespace is attached
// exactly as it is for a block volume, because a pNFS client is an NVMe-oF
// initiator for the same namespace the MDS made the filesystem on -- that is
// the whole point, and it is why the data path bypasses the metadata server.
// What is added is a name: the kernel builds a /dev/disk/by-id path from the
// designator the MDS advertises, and nothing creates that path on its own.
//
// Then an ordinary NFSv4.1 mount, and the kernel does the rest: LAYOUTGET on
// first I/O, then reads and writes straight to the namespace.

package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// aliasDir is where the kernel looks for the device the layout names.
const aliasDir = "/dev/disk/by-id"

// aliasPrefix is the one of the three prefixes bl_parse_scsi tries that applies
// to an NVMe namespace.
//
// It is `nvme-eui.`, not `nvme-eui64.`. The kernel builds the path itself as
// "/dev/disk/by-id/%s%*phN" and tries dm-uuid-mpath-0x, then wwn-0x, then this
// one; a name that matches none of them leaves the client unable to map the
// layout, and the failure is silent -- it returns the layout and falls back to
// routing every byte through the metadata server, which looks exactly like
// working.
const aliasPrefix = "nvme-eui."

// nfsMountOptions are the options every pNFS mount carries. 4.1 is the floor:
// layouts do not exist before it.
var nfsMountOptions = []string{"vers=4.1"}

// aliasPath is where the alias for a namespace goes.
//
// The NGUID is lowercased and stripped of punctuation because the two places it
// comes from disagree: sysfs reports it hyphenated, `nvme id-ns` reports it
// bare, and the kernel formats the designator as plain lowercase hex. A name
// built from the wrong spelling matches nothing.
func aliasPath(nguid string) string {
	return filepath.Join(aliasDir, aliasPrefix+normalizeNGUID(nguid))
}

// normalizeNGUID reduces an NGUID to the spelling the kernel builds its lookup
// path from: lowercase hex, no separators.
func normalizeNGUID(nguid string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(nguid) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ensureDeviceAlias creates the by-id name the client kernel resolves the
// layout through, pointing at the local block device for the namespace.
//
// It is idempotent, and it replaces an alias pointing somewhere else rather than
// leaving it: a device node is assigned in attach order, so the same namespace
// can be a different path after a reconnect, and a stale alias would send the
// kernel at whatever now holds the old path.
func ensureDeviceAlias(nguid, devicePath string) (string, error) {
	if normalizeNGUID(nguid) == "" {
		return "", fmt.Errorf("pnfs: namespace has no usable NGUID (%q)", nguid)
	}
	if devicePath == "" {
		return "", fmt.Errorf("pnfs: no device path for namespace %s", nguid)
	}
	alias := aliasPath(nguid)
	if err := os.MkdirAll(aliasDir, 0o755); err != nil {
		return "", fmt.Errorf("pnfs: creating %s: %w", aliasDir, err)
	}

	if existing, err := os.Readlink(alias); err == nil {
		if existing == devicePath {
			return alias, nil
		}
		// Pointing elsewhere: replace rather than leave, per above.
		if err := os.Remove(alias); err != nil {
			return "", fmt.Errorf("pnfs: replacing stale alias %s: %w", alias, err)
		}
	}
	if err := os.Symlink(devicePath, alias); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("pnfs: linking %s to %s: %w", alias, devicePath, err)
	}
	return alias, nil
}

// removeDeviceAlias drops the alias, and treats an absent one as done.
func removeDeviceAlias(nguid string) error {
	if normalizeNGUID(nguid) == "" {
		return nil
	}
	if err := os.Remove(aliasPath(nguid)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pnfs: removing alias for %s: %w", nguid, err)
	}
	return nil
}

// nfsSource is what gets mounted: the export's address and the path the MDS
// exports it at.
func nfsSource(serviceAddress, exportPath string) string {
	return serviceAddress + ":" + exportPath
}

// mountOptions merges the class's extra options after the ones every pNFS mount
// needs, so a class can add but not silently drop the version that makes
// layouts possible.
func mountOptions(extra string) []string {
	opts := append([]string{}, nfsMountOptions...)
	for _, o := range strings.Split(extra, ",") {
		if o = strings.TrimSpace(o); o != "" {
			opts = append(opts, o)
		}
	}
	return opts
}

// NFSMounter is the mounting a pNFS stage needs. It is separate from the block
// mounter because nothing here formats: the filesystem was made by the MDS, and
// a client that could format one would be a client that could destroy it.
type NFSMounter interface {
	Mount(source, target, fsType string, options []string) error
	Unmount(target string) error
	IsMounted(target string) (bool, error)
}

// stagePNFS attaches the export at the staging path. The namespace is expected
// to be connected already, by the same path a block volume uses.
func stagePNFS(
	_ context.Context,
	mounter NFSMounter,
	stagingPath, serviceAddress, exportPath, nguid, devicePath, extraOptions string,
) error {
	if _, err := ensureDeviceAlias(nguid, devicePath); err != nil {
		// Without the alias the mount still succeeds and every byte silently
		// routes through the metadata server, so this is a failure rather than
		// a warning: a working-looking mount with none of the throughput the
		// feature exists for is worse than a refused one.
		return err
	}
	mounted, err := mounter.IsMounted(stagingPath)
	if err != nil {
		return fmt.Errorf("pnfs: checking %s: %w", stagingPath, err)
	}
	if mounted {
		return nil
	}
	if err := os.MkdirAll(stagingPath, 0o750); err != nil {
		return fmt.Errorf("pnfs: creating %s: %w", stagingPath, err)
	}
	source := nfsSource(serviceAddress, exportPath)
	if err := mounter.Mount(source, stagingPath, "nfs", mountOptions(extraOptions)); err != nil {
		return fmt.Errorf("pnfs: mounting %s at %s: %w", source, stagingPath, err)
	}
	return nil
}

// unstagePNFS detaches the export and drops the alias.
func unstagePNFS(_ context.Context, mounter NFSMounter, stagingPath, nguid string) error {
	mounted, err := mounter.IsMounted(stagingPath)
	if err != nil {
		return fmt.Errorf("pnfs: checking %s: %w", stagingPath, err)
	}
	if mounted {
		if err := mounter.Unmount(stagingPath); err != nil {
			return fmt.Errorf("pnfs: unmounting %s: %w", stagingPath, err)
		}
	}
	return removeDeviceAlias(nguid)
}

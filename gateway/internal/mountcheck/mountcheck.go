// Package mountcheck finds bucket directories under the gateway's root that
// look present but are no longer actually mounted: the readiness-visible
// symptom of design-bucket-provisioning.md's Open Question 6 (a bucket's
// mount dying under it -- most commonly, its node rebooting -- without the
// operator's own bookkeeping ever noticing, since the gateway pod and its
// node placement stay exactly as recorded). This package does not repair
// anything; it only lets a readiness probe refuse traffic instead of the
// gateway silently serving reads and writes from the node's local disk
// where a bucket's volume used to be bind-mounted.
//
// A bucket directory still bind-mounted from its pNFS volume has its own
// device: stat(2)'s st_dev differs from the gateway root's. One whose mount
// died collapses back onto the root's own local filesystem, indistinguishable
// from an ordinary subdirectory by name, permissions, or a read/write test --
// only the device comparison catches it.
package mountcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Unmounted reports the names of rootDir's immediate subdirectories that
// share rootDir's own device, meaning whatever was mounted there is gone.
// An empty, nil-error result means every bucket directory found is still a
// distinct mount.
func Unmounted(rootDir string) ([]string, error) {
	rootDev, err := device(rootDir)
	if err != nil {
		return nil, fmt.Errorf("mountcheck: stat root %s: %w", rootDir, err)
	}

	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, fmt.Errorf("mountcheck: read root %s: %w", rootDir, err)
	}

	var unmounted []string
	for _, entry := range entries {
		// A hidden entry is never a bucket: no valid S3 bucket name starts
		// with a dot (buckets.go's CreateBucket enforces DNS-1123), while
		// versitygw's posix backend creates `.vgwlocks` directly under the
		// root at startup (backend/posix/objlock.go) before any bucket
		// exists. Counting it as an unmounted bucket failed readiness
		// forever on a gateway that had never been delivered one yet.
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dev, err := device(filepath.Join(rootDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("mountcheck: stat bucket %s: %w", entry.Name(), err)
		}
		if dev == rootDev {
			unmounted = append(unmounted, entry.Name())
		}
	}
	return unmounted, nil
}

// device is a path's stat(2) st_dev, Linux's own way of telling a mount
// boundary apart from an ordinary subdirectory.
func device(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("mountcheck: no Stat_t for %s", path)
	}
	return uint64(stat.Dev), nil
}

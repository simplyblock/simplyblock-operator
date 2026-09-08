package fabric

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The tests run atlas's own sysfs resolver, which means they need a sysfs to
// point it at — and the kernel that holds the state is the node's, while the
// test process is on the developer's machine, which on macOS is not even Linux.
//
// So the node's NVMe sysfs is copied out as text and rebuilt locally. sysfs
// cannot be tarred: it is full of symlinks pointing back up the tree and files
// whose stat size is a lie. Walking the two class directories and writing one
// "path<TAB>value" line per attribute is what survives the trip.
//
// The format is deliberately the one hack/nvmet/capture-sysfs.sh produces, so a
// state reached here can be sanitized and committed as a unit-test fixture under
// nvmeof/testdata/sysfs — the integration suite is where those fixtures come
// from.

// dumpScript is capture-sysfs.sh's dump function, inlined because the pod has no
// copy of the script.
const dumpScript = `find -L /sys/class/nvme-subsystem /sys/class/nvme -maxdepth 3 \
	\( -name subsystem -o -name power -o -name device -o -name firmware_node \) -prune \
	-o -type f -print 2>/dev/null | while read -r f; do
	v=$(cat "$f" 2>/dev/null | head -1 | tr -d '\n')
	printf '%s\t%s\n' "$f" "$v"
done`

// DumpSysfs reads the node's NVMe sysfs as a snapshot.
func (s *Shell) DumpSysfs(ctx context.Context) (string, error) {
	out, err := s.Run(ctx, dumpScript)
	if err != nil {
		return "", fmt.Errorf("dump sysfs on %s: %w\n%s", s.node, err, out)
	}
	return out, nil
}

// ReconstructSysfs writes a snapshot into a directory tree that
// nvme.SysfsConfig{SysRoot: root} can be pointed at, and reports how many
// attributes it wrote.
//
// Attributes only: the resolver reads values and walks directory names, and a
// tree of real files reproduces both. What it does not reproduce is sysfs's
// symlinks, which is why the dump prunes the ones that lead out of the subtree.
func ReconstructSysfs(snapshot, root string) (int, error) {
	n := 0
	for _, line := range strings.Split(snapshot, "\n") {
		path, value, found := strings.Cut(strings.TrimSuffix(line, "\r"), "\t")
		if !found || !strings.HasPrefix(path, "/sys/") {
			continue
		}
		dest := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/sys/")))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return n, fmt.Errorf("reconstruct %s: %w", dest, err)
		}
		if err := os.WriteFile(dest, []byte(value+"\n"), 0o600); err != nil {
			return n, fmt.Errorf("reconstruct %s: %w", dest, err)
		}
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("reconstruct: snapshot held no /sys attributes")
	}
	return n, nil
}

// CaptureSysfs dumps the node's sysfs and reconstructs it under root.
func (s *Shell) CaptureSysfs(ctx context.Context, root string) (string, error) {
	snapshot, err := s.DumpSysfs(ctx)
	if err != nil {
		return "", err
	}
	if _, err := ReconstructSysfs(snapshot, root); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

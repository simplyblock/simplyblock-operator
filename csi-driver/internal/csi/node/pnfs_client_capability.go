// Whether this node's NFS client can open the device a pNFS SCSI layout
// names: the probe that reads the client's layout module, and the node label
// that publishes the answer (design-pnfs-rwx.md §7.3).
//
// The client looks the device up under /dev/disk/by-id with name prefixes
// compiled into blocklayoutdriver, and the node plugin links the device under
// nvme-eui. and wwn-0x (pnfs.go). A client that knows neither prefix, or a
// kernel without the module, sends every byte of every pNFS volume through the
// metadata server, and nothing fails to say so. The prefixes are read from the
// module rather than inferred from the kernel version, because distributions
// backport them: RHEL 9's 5.14.0-503 lacks nvme-eui., and 5.14.0-570 has it.
//
// It follows the VDO and KVM probes: probe once at start, publish a label, and
// leave a hand-set label alone.

package node

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/kube"
)

// layoutModule is the module that carries the pNFS block and SCSI layout
// client.
const layoutModule = "blocklayoutdriver"

// AdvertisePNFSClientCapability probes this node's layout module and sets
// nodeName's pnfs-client-capable label to match. Runs once at process start,
// like the VDO probe.
func AdvertisePNFSClientCapability(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	return advertisePNFSClientCapability(ctx, kubeClient, nodeName, "/")
}

func advertisePNFSClientCapability(
	ctx context.Context, kubeClient kubernetes.Interface, nodeName, root string,
) error {
	return publishCapability(ctx, kubeClient, nodeName, capabilityLabel{
		probe:        "pnfs-client",
		label:        kube.LabelPNFSClientCapable,
		managedBy:    kube.AnnoPNFSClientCapableManagedBy,
		managedValue: kube.AnnoPNFSClientCapableManagedByAutoDetect,
	}, probePNFSClient(nodeName, root))
}

// probePNFSClient reports whether the layout module under root knows one of
// the prefixes the device is linked under, and logs what it found.
//
// A layout client built into the kernel counts as capable without being
// read: its strings are in the kernel image, which the plugin does not have,
// and every kernel with the SCSI layout tries wwn-0x.
func probePNFSClient(nodeName, root string) bool {
	release, err := os.ReadFile(filepath.Join(root, "proc/sys/kernel/osrelease"))
	if err != nil {
		klog.Infof("pnfs-client probe: node %s kernel release unavailable: %v", nodeName, err)
		return false
	}
	modules := filepath.Join(root, "lib/modules", strings.TrimSpace(string(release)))

	path, builtin, err := findModule(modules, layoutModule)
	switch {
	case err != nil:
		klog.Infof("pnfs-client probe: node %s (kernel %s) has no %s: %v",
			nodeName, strings.TrimSpace(string(release)), layoutModule, err)
		return false
	case builtin:
		klog.Infof("pnfs-client probe: node %s has %s built in", nodeName, layoutModule)
		return true
	}

	content, err := readModule(filepath.Join(modules, path))
	if err != nil {
		klog.Infof("pnfs-client probe: node %s cannot read %s: %v", nodeName, path, err)
		return false
	}
	var known []string
	for _, prefix := range aliasPrefixes {
		if bytes.Contains(content, []byte(prefix)) {
			known = append(known, prefix)
		}
	}
	klog.Infof("pnfs-client probe: node %s's %s knows the prefixes %q of %q",
		nodeName, path, known, aliasPrefixes)
	return len(known) > 0
}

// findModule finds name in the module lists of the kernel whose modules are in
// dir: its path relative to dir from modules.dep, or builtin when
// modules.builtin lists it.
func findModule(dir, name string) (path string, builtin bool, err error) {
	matches := func(entry string) bool {
		base := filepath.Base(entry)
		return base == name+".ko" || strings.HasPrefix(base, name+".ko.")
	}
	if found, err := scanModuleList(filepath.Join(dir, "modules.dep"), matches); err != nil {
		return "", false, err
	} else if found != "" {
		return found, false, nil
	}
	if found, err := scanModuleList(filepath.Join(dir, "modules.builtin"), matches); err == nil && found != "" {
		return found, true, nil
	}
	return "", false, fmt.Errorf("neither modules.dep nor modules.builtin lists it")
}

// scanModuleList returns the first module in list, one per line and before any
// colon, that matches.
func scanModuleList(list string, matches func(string) bool) (string, error) {
	f, err := os.Open(list)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		entry, _, _ := strings.Cut(scanner.Text(), ":")
		if entry = strings.TrimSpace(entry); matches(entry) {
			return entry, nil
		}
	}
	return "", scanner.Err()
}

// readModule reads a module file, decompressed by its suffix.
func readModule(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var r io.Reader = f
	switch filepath.Ext(path) {
	case ".xz":
		if r, err = xz.NewReader(f); err != nil {
			return nil, err
		}
	case ".zst":
		d, err := zstd.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer d.Close()
		r = d
	case ".gz":
		g, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = g.Close() }()
		r = g
	}
	return io.ReadAll(r)
}

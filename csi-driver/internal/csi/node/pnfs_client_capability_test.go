// Tests for the pNFS client probe. They run against a /proc and /lib/modules
// written to a temporary directory, with the layout module stored the ways
// distributions ship it.

package node

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

const (
	rhel95Release = "5.14.0-503.11.1.el9_5.x86_64"
	layoutModPath = "kernel/fs/nfs/blocklayout/blocklayoutdriver.ko"
)

// The strings the two RHEL 9 builds carry, as `strings` showed them on lab
// cluster 111.
var (
	rhel95Module = []byte("\x7fELF\x00/dev/disk/by-id/dm-uuid-mpath-0x\x00/dev/disk/by-id/wwn-0x\x00")
	rhel98Module = []byte("\x7fELF\x00dm-uuid-mpath-0x\x00wwn-0x\x00nvme-eui.\x00")
	noPrefixes   = []byte("\x7fELF\x00blocklayoutdriver\x00")
)

// fakeModules is a /proc and /lib/modules under a temporary root.
type fakeModules struct {
	t       *testing.T
	root    string
	release string
}

func newFakeModules(t *testing.T, release string) *fakeModules {
	t.Helper()
	k := &fakeModules{t: t, root: t.TempDir(), release: release}
	k.write("proc/sys/kernel/osrelease", []byte(release+"\n"))
	k.write(filepath.Join("lib/modules", release, "modules.dep"),
		[]byte("kernel/fs/nfs/nfsv4.ko.xz: kernel/fs/nfs/nfs.ko.xz\n"))
	k.write(filepath.Join("lib/modules", release, "modules.builtin"), []byte("kernel/fs/ext4/ext4.ko\n"))
	return k
}

func (k *fakeModules) write(rel string, data []byte) {
	k.t.Helper()
	path := filepath.Join(k.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		k.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		k.t.Fatal(err)
	}
}

func (k *fakeModules) appendTo(rel, line string) {
	k.t.Helper()
	f, err := os.OpenFile(filepath.Join(k.root, rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		k.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		k.t.Fatal(err)
	}
}

// module installs the layout module, compressed as suffix says.
func (k *fakeModules) module(suffix string, content []byte) {
	k.t.Helper()
	var buf bytes.Buffer
	switch suffix {
	case "":
		buf.Write(content)
	case ".xz":
		w, err := xz.NewWriter(&buf)
		if err != nil {
			k.t.Fatal(err)
		}
		_, _ = w.Write(content)
		_ = w.Close()
	case ".zst":
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			k.t.Fatal(err)
		}
		_, _ = w.Write(content)
		_ = w.Close()
	case ".gz":
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(content)
		_ = w.Close()
	}
	rel := layoutModPath + suffix
	k.write(filepath.Join("lib/modules", k.release, rel), buf.Bytes())
	k.appendTo(filepath.Join("lib/modules", k.release, "modules.dep"), rel+": kernel/fs/nfs/nfsv4.ko.xz")
}

// Regression: 2026-11-18-pnfs-rhel95-no-nvme-eui (lab cluster 111). The RHEL
// 9.5 module knows wwn-0x and not nvme-eui., and the node links the device
// under both, so the node can open it. RHEL 9.8's knows both.
func TestAModuleThatKnowsEitherLinkedPrefixIsCapable(t *testing.T) {
	for name, content := range map[string][]byte{"rhel 9.5": rhel95Module, "rhel 9.8": rhel98Module} {
		k := newFakeModules(t, rhel95Release)
		k.module(".xz", content)
		if !probePNFSClient("node-a", k.root) {
			t.Errorf("%s: probe = false, want true", name)
		}
	}
}

// The module is read however the distribution compressed it.
func TestTheModuleIsReadInEveryCompression(t *testing.T) {
	for _, suffix := range []string{"", ".xz", ".zst", ".gz"} {
		k := newFakeModules(t, rhel95Release)
		k.module(suffix, rhel98Module)
		if !probePNFSClient("node-a", k.root) {
			t.Errorf("module %q: probe = false, want true", layoutModPath+suffix)
		}
	}
}

// A module that knows none of the prefixes the device is linked under cannot
// open it, and a kernel without the module has no layout client at all.
func TestAModuleWithoutTheLinkedPrefixesOrNoModuleIsNotCapable(t *testing.T) {
	k := newFakeModules(t, rhel95Release)
	k.module(".xz", noPrefixes)
	if probePNFSClient("node-a", k.root) {
		t.Error("a module with no linked prefix: probe = true, want false")
	}
	if probePNFSClient("node-a", newFakeModules(t, rhel95Release).root) {
		t.Error("no module: probe = true, want false")
	}
	if probePNFSClient("node-a", t.TempDir()) {
		t.Error("no /proc or /lib/modules: probe = true, want false")
	}
}

// A layout client built into the kernel cannot be read, and every kernel with
// the SCSI layout knows wwn-0x, so it counts as capable.
func TestABuiltInLayoutClientIsCapable(t *testing.T) {
	k := newFakeModules(t, "6.18.5-talos")
	k.appendTo("lib/modules/6.18.5-talos/modules.builtin", layoutModPath)
	if !probePNFSClient("node-a", k.root) {
		t.Error("built-in layout client: probe = false, want true")
	}
}

func pnfsClientLabel(t *testing.T, client *kfake.Clientset) (string, bool) {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, managed := node.Annotations[kube.AnnoPNFSClientCapableManagedBy]
	return node.Labels[kube.LabelPNFSClientCapable], managed
}

// The answer is published as the probe's own label, true or false.
func TestAdvertisePNFSClientCapabilityLabelsTheNode(t *testing.T) {
	capable := newFakeModules(t, rhel95Release)
	capable.module(".xz", rhel95Module)
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	if err := advertisePNFSClientCapability(context.Background(), client, "node-a", capable.root); err != nil {
		t.Fatalf("advertisePNFSClientCapability: %v", err)
	}
	if label, managed := pnfsClientLabel(t, client); label != vdoCapableTrue || !managed {
		t.Errorf("label = %q, managed = %t, want true and managed", label, managed)
	}

	client = kfake.NewSimpleClientset(testNode(nil, nil))
	if err := advertisePNFSClientCapability(context.Background(), client, "node-a", t.TempDir()); err != nil {
		t.Fatalf("advertisePNFSClientCapability: %v", err)
	}
	if label, _ := pnfsClientLabel(t, client); label != vdoCapableFalse {
		t.Errorf("label = %q without a layout client, want false", label)
	}
}

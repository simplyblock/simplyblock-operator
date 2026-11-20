// What the kernel reading concludes about native NVMe multipath and the three
// optional kernel capabilities, from each of the places a host states them.
//
// Every source is tested alone, because hosts differ in which ones they have:
// Ubuntu keeps its kernel configuration only in /boot, which the probe does not
// mount, RHEL and Amazon Linux keep a copy beside the modules, and a kernel
// built with CONFIG_IKCONFIG_PROC publishes it in /proc.

package inventory

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kernelRelease is the release every fixture below is booted on.
const kernelRelease = "6.1.155-176.282.amzn2023.x86_64"

// modulesDir is where a fixture keeps that release's module index.
const modulesDir = "usr/lib/modules/" + kernelRelease + "/"

// kernelHost is a fixture booted on kernelRelease, with the files given.
func kernelHost(files map[string]string, dirs ...string) fixture {
	all := map[string]string{"sys/kernel/osrelease": kernelRelease}
	for path, content := range files {
		all[path] = content
	}
	return fixture{files: all, dirs: dirs}
}

// kernelOf reads the kernel facts of a fixture.
func kernelOf(t *testing.T, f fixture) KernelFacts {
	t.Helper()
	root := f.write(t)
	facts, err := ReadKernel(Config{SysfsRoot: root, ProcRoot: root, HostRoot: root})
	if err != nil {
		t.Fatalf("read the kernel: %v", err)
	}
	return facts
}

func TestReadKernelReadsTheRelease(t *testing.T) {
	facts := kernelOf(t, kernelHost(nil))
	if facts.Release != kernelRelease {
		t.Errorf("read the release as %q, want %q", facts.Release, kernelRelease)
	}
}

func TestParseKernelVersion(t *testing.T) {
	cases := []struct {
		release string
		want    KernelVersion
		ok      bool
	}{
		{"6.18.24-talos", KernelVersion{6, 18, 24}, true},
		{"6.1.155-176.282.amzn2023.x86_64", KernelVersion{6, 1, 155}, true},
		{"5.15.0-122-generic", KernelVersion{5, 15, 0}, true},
		{"4.18.0-553.el8_10.x86_64", KernelVersion{4, 18, 0}, true},
		{"6.6.58+rpt-rpi-v8", KernelVersion{6, 6, 58}, true},
		{"6.12.0", KernelVersion{6, 12, 0}, true},
		{"6.8-rc1", KernelVersion{6, 8, 0}, true},
		{"6.8", KernelVersion{6, 8, 0}, true},
		{"", KernelVersion{}, false},
		{"6", KernelVersion{}, false},
		{"talos", KernelVersion{}, false},
		{"6.x.1", KernelVersion{}, false},
	}
	for _, c := range cases {
		t.Run(c.release, func(t *testing.T) {
			got, ok := ParseKernelVersion(c.release)
			if got != c.want || ok != c.ok {
				t.Errorf("parsed %q as %+v, %v, want %+v, %v", c.release, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestReadKernelReadsTheVersion(t *testing.T) {
	facts := kernelOf(t, kernelHost(nil))
	if want := (KernelVersion{6, 1, 155}); facts.Version != want {
		t.Errorf("read the version as %+v, want %+v", facts.Version, want)
	}
}

func TestKernelVersionCompare(t *testing.T) {
	version := KernelVersion{6, 1, 155}
	cases := []struct {
		other KernelVersion
		want  int
	}{
		{KernelVersion{6, 1, 155}, 0},
		{KernelVersion{6, 1, 154}, 1},
		{KernelVersion{6, 0, 200}, 1},
		{KernelVersion{5, 19, 0}, 1},
		{KernelVersion{6, 1, 156}, -1},
		{KernelVersion{6, 2, 0}, -1},
		{KernelVersion{7, 0, 0}, -1},
	}
	for _, c := range cases {
		if got := version.Compare(c.other); got != c.want {
			t.Errorf("%+v.Compare(%+v) is %d, want %d", version, c.other, got, c.want)
		}
	}
	if (KernelVersion{}).Compare(KernelVersion{0, 0, 1}) >= 0 {
		t.Error("a version that was not read is not earlier than 0.0.1")
	}
}

func TestReadKernelReportsAnUnreadableRelease(t *testing.T) {
	root := fixture{}.write(t)
	_, err := ReadKernel(Config{SysfsRoot: root, ProcRoot: root, HostRoot: root})
	if err == nil {
		t.Fatal("a host with no osrelease read without an error")
	}
	// The host's path and not the reader's mount of it: the sentence lands in a
	// report a reviewer checks on the host, and a temporary root in it would
	// make two reports of one host differ.
	if !strings.Contains(err.Error(), "/proc/sys/kernel/osrelease") || strings.Contains(err.Error(), root) {
		t.Errorf("the error %q does not name the host's /proc/sys/kernel/osrelease alone", err)
	}
}

func TestReadKernelReadsNVMeMultipath(t *testing.T) {
	cases := []struct {
		name string
		host fixture
		want MultipathState
	}{
		{"enabled by the parameter", kernelHost(map[string]string{
			"module/nvme_core/parameters/multipath": "Y",
		}), MultipathEnabled},
		{"disabled by the parameter", kernelHost(map[string]string{
			"module/nvme_core/parameters/multipath": "N",
		}), MultipathDisabled},
		{"nvme_core loaded without the parameter", kernelHost(nil,
			"module/nvme_core/parameters"), MultipathNotBuilt},
		{"not built, from the module tree's configuration", kernelHost(map[string]string{
			modulesDir + "config": "CONFIG_NVME_CORE=m\n# CONFIG_NVME_MULTIPATH is not set",
		}), MultipathNotBuilt},
		{"built, and switched off on the command line", kernelHost(map[string]string{
			modulesDir + "config": "CONFIG_NVME_MULTIPATH=y",
			"cmdline":             "BOOT_IMAGE=/vmlinuz root=/dev/sda1 nvme_core.multipath=N",
		}), MultipathDisabled},
		{"built, and nvme_core not loaded", kernelHost(map[string]string{
			modulesDir + "config": "CONFIG_NVME_MULTIPATH=y",
			"cmdline":             "BOOT_IMAGE=/vmlinuz root=/dev/sda1",
		}), MultipathEnabled},
		{"built without the parameter, which the configuration settles", kernelHost(map[string]string{
			modulesDir + "config": "CONFIG_NVME_MULTIPATH=y\n# CONFIG_NVME_MULTIPATH_PARAM is not set",
		}, "module/nvme_core/parameters"), MultipathEnabled},
		{"nothing to read", kernelHost(nil), MultipathUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kernelOf(t, c.host).NVMeMultipath
			if got.State != c.want {
				t.Errorf("concluded %q (%s), want %q", got.State, got.Detail, c.want)
			}
			if got.Detail == "" {
				t.Error("the conclusion carries no evidence")
			}
		})
	}
}

func TestReadKernelReadsTheMultipathConfigurationFromProc(t *testing.T) {
	root := kernelHost(nil).write(t)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("# CONFIG_NVME_MULTIPATH is not set\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.gz"), compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	facts, err := ReadKernel(Config{SysfsRoot: root, ProcRoot: root, HostRoot: root})
	if err != nil {
		t.Fatalf("read the kernel: %v", err)
	}
	if facts.NVMeMultipath.State != MultipathNotBuilt {
		t.Errorf("concluded %q, want %q", facts.NVMeMultipath.State, MultipathNotBuilt)
	}
	// The host's path, for the reason the release error names the host's path.
	if detail := facts.NVMeMultipath.Detail; !strings.Contains(detail, "/proc/config.gz") || strings.Contains(detail, root) {
		t.Errorf("the evidence %q does not name the host's /proc/config.gz alone", detail)
	}
}

func TestReadKernelNamesTheModuleTreesConfigurationByTheHostsPath(t *testing.T) {
	root := kernelHost(map[string]string{modulesDir + "config": "# CONFIG_NVME_MULTIPATH is not set"}).write(t)
	facts, err := ReadKernel(Config{SysfsRoot: root, ProcRoot: root, HostRoot: root})
	if err != nil {
		t.Fatalf("read the kernel: %v", err)
	}
	detail := facts.NVMeMultipath.Detail
	if !strings.Contains(detail, "/"+modulesDir+"config") || strings.Contains(detail, root) {
		t.Errorf("the evidence %q does not name the host's /%sconfig alone", detail, modulesDir)
	}
}

func TestReadKernelReadsTUN(t *testing.T) {
	cases := []struct {
		name string
		host fixture
		want CapabilityState
	}{
		{"registered", kernelHost(nil, "class/misc/tun"), CapabilityAvailable},
		{"a module not yet loaded", kernelHost(map[string]string{
			modulesDir + "modules.dep":     "kernel/drivers/net/tun.ko.xz:",
			modulesDir + "modules.builtin": "kernel/drivers/nvme/host/nvme-core.ko",
		}), CapabilityLoadable},
		{"neither built in nor a module", kernelHost(map[string]string{
			modulesDir + "modules.dep":     "kernel/drivers/net/veth.ko.xz:",
			modulesDir + "modules.builtin": "kernel/drivers/nvme/host/nvme-core.ko",
		}), CapabilityAbsent},
		{"not configured", kernelHost(map[string]string{
			modulesDir + "config": "# CONFIG_TUN is not set",
		}), CapabilityAbsent},
		{"nothing to read", kernelHost(nil), CapabilityUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kernelOf(t, c.host).TUN
			if got.State != c.want {
				t.Errorf("concluded %q (%s), want %q", got.State, got.Detail, c.want)
			}
		})
	}
}

func TestReadKernelReadsPNFSBlock(t *testing.T) {
	cases := []struct {
		name string
		host fixture
		want CapabilityState
	}{
		{"loaded", kernelHost(nil, "module/blocklayoutdriver"), CapabilityAvailable},
		{"built in", kernelHost(map[string]string{
			modulesDir + "modules.builtin": "kernel/fs/nfs/blocklayout/blocklayoutdriver.ko",
			modulesDir + "modules.dep":     "kernel/drivers/net/tun.ko:",
		}), CapabilityAvailable},
		{"a module not yet loaded", kernelHost(map[string]string{
			modulesDir + "modules.dep": "kernel/fs/nfs/blocklayout/blocklayoutdriver.ko.zst: kernel/fs/nfs/nfsv4.ko.zst",
		}), CapabilityLoadable},
		{"not configured", kernelHost(map[string]string{
			modulesDir + "config": "CONFIG_NFS_V4_1=y\n# CONFIG_PNFS_BLOCK is not set",
		}), CapabilityAbsent},
		{"absent from a readable module index", kernelHost(map[string]string{
			modulesDir + "modules.dep": "kernel/fs/nfs/nfsv4.ko.zst:",
		}), CapabilityAbsent},
		{"nothing to read", kernelHost(nil), CapabilityUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kernelOf(t, c.host).PNFSBlock
			if got.State != c.want {
				t.Errorf("concluded %q (%s), want %q", got.State, got.Detail, c.want)
			}
		})
	}
}

func TestReadKernelReadsKVM(t *testing.T) {
	cases := []struct {
		name string
		host fixture
		want CapabilityState
	}{
		{"registered", kernelHost(nil, "class/misc/kvm"), CapabilityAvailable},
		{"loaded and registered no device", kernelHost(map[string]string{
			"cpuinfo": "processor\t: 0\nflags\t\t: fpu vme de pse",
		}, "module/kvm", "module/kvm_intel"), CapabilityAbsent},
		{"a CPU without virtualization", kernelHost(map[string]string{
			"cpuinfo":                  "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr",
			modulesDir + "modules.dep": "kernel/arch/x86/kvm/kvm-intel.ko.xz: kernel/arch/x86/kvm/kvm.ko.xz",
		}), CapabilityAbsent},
		{"a module not yet loaded", kernelHost(map[string]string{
			"cpuinfo":                  "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr vmx",
			modulesDir + "modules.dep": "kernel/arch/x86/kvm/kvm-intel.ko.xz: kernel/arch/x86/kvm/kvm.ko.xz",
		}), CapabilityLoadable},
		{"an AMD CPU and its module", kernelHost(map[string]string{
			"cpuinfo":                  "processor\t: 0\nflags\t\t: fpu vme svm",
			modulesDir + "modules.dep": "kernel/arch/x86/kvm/kvm-amd.ko.xz: kernel/arch/x86/kvm/kvm.ko.xz",
		}), CapabilityLoadable},
		{"nothing to read", kernelHost(nil), CapabilityUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kernelOf(t, c.host).KVM
			if got.State != c.want {
				t.Errorf("concluded %q (%s), want %q", got.State, got.Detail, c.want)
			}
		})
	}
}

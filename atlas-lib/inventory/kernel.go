// What the running kernel can do for a storage node and the workloads beside it:
// native NVMe multipath, and the three optional capabilities /dev/kvm,
// /dev/net/tun, and the pNFS block layout.
//
// It lives beside the host OS reading because these are facts about the kernel
// a host boots and not about its distribution. Two hosts of one distribution can
// boot different kernels, and the distribution says nothing reliable about any
// of them: Amazon Linux 2023's default kernel is built without native NVMe
// multipath, and the same host booted on another kernel has it.
//
// A kernel states each fact in several places and no host offers all of them.
// The readers ask the running kernel first (a registered device, a loaded
// module, a module parameter), then the module index of the release it booted,
// then its build configuration, and say which one answered. A fact none of them
// answers is Unknown, which is a reading that was not taken rather than a
// capability the host lacks.

package inventory

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MultipathState is whether the kernel joins the paths of an NVMe namespace into
// one block device.
type MultipathState string

const (
	// MultipathEnabled joins them: a volume reached over three paths is one
	// device, and the kernel moves I/O between the paths by their ANA state.
	MultipathEnabled MultipathState = "Enabled"

	// MultipathDisabled is a kernel built with native multipath and booted with
	// it switched off (`nvme_core.multipath=N`). A reboot without the
	// parameter enables it.
	MultipathDisabled MultipathState = "Disabled"

	// MultipathNotBuilt is a kernel built without CONFIG_NVME_MULTIPATH. Every
	// path of a volume is a block device of its own, and no setting changes
	// that: the host needs another kernel.
	MultipathNotBuilt MultipathState = "NotBuilt"

	// MultipathUnknown is a host on which nothing that says was readable.
	MultipathUnknown MultipathState = "Unknown"
)

// CapabilityState is whether the kernel offers an optional capability.
type CapabilityState string

const (
	// CapabilityAvailable is a capability the kernel offers now.
	CapabilityAvailable CapabilityState = "Available"

	// CapabilityLoadable is a capability in a module the kernel has and has
	// not loaded. Loading it needs nothing but modprobe, and the kernel loads
	// some on their own: opening /dev/net/tun loads tun.
	CapabilityLoadable CapabilityState = "Loadable"

	// CapabilityAbsent is a capability the kernel was built without, or one
	// the machine cannot provide, such as KVM on a CPU without hardware
	// virtualization.
	CapabilityAbsent CapabilityState = "Absent"

	// CapabilityUnknown is a host on which nothing that says was readable.
	CapabilityUnknown CapabilityState = "Unknown"
)

// Multipath is the native NVMe multipath state with the evidence for it.
type Multipath struct {
	State MultipathState

	// Detail names the source that answered and what it said, so a reviewer
	// can check the conclusion on the host.
	Detail string
}

// Capability is one optional capability with the evidence for it.
type Capability struct {
	State CapabilityState

	// Detail names the source that answered and what it said.
	Detail string
}

// KernelFacts is what the running kernel offers.
type KernelFacts struct {
	// Release is the kernel's release string, the one `uname -r` prints, such
	// as `6.1.155-176.282.amzn2023.x86_64`.
	Release string

	// NVMeMultipath decides whether the host can mount a volume served over
	// more than one path. Storage nodes do not depend on it: the storage
	// engine runs in user space and does not use the kernel's NVMe initiator.
	NVMeMultipath Multipath

	// KVM is /dev/kvm, which a workload running virtual machines needs.
	KVM Capability

	// TUN is /dev/net/tun, which a userspace network device needs.
	TUN Capability

	// PNFSBlock is the pNFS block layout driver (`blocklayoutdriver`, built
	// from CONFIG_PNFS_BLOCK), which a pNFS client needs to write to a
	// volume directly instead of through the metadata server.
	PNFSBlock Capability
}

// ReadKernel reads the running kernel's release and capabilities.
//
// The error reports only an unreadable release. Every capability reads as
// Unknown when nothing about it was readable, and the release is what places
// the module index, so a host whose release is unreadable has the running
// kernel's answers and no others.
func ReadKernel(cfg Config) (KernelFacts, error) {
	var facts KernelFacts
	var err error

	raw, readErr := os.ReadFile(cfg.procPath("sys", "kernel", "osrelease"))
	if readErr != nil {
		// The host's path rather than the resolved one, which is wherever the
		// reader mounted the host's procfs.
		if pathErr, ok := errors.AsType[*os.PathError](readErr); ok {
			readErr = pathErr.Err
		}
		err = fmt.Errorf("read the kernel release from /proc/sys/kernel/osrelease: %w", readErr)
	}
	facts.Release = strings.TrimSpace(string(raw))

	k := kernelSources{cfg: cfg, release: facts.Release}
	k.load()

	facts.NVMeMultipath = k.multipath()
	facts.KVM = k.readKVM()
	facts.TUN = k.tun()
	facts.PNFSBlock = k.pnfsBlock()
	return facts, err
}

// kernelSources is what a host offered to read about its kernel.
type kernelSources struct {
	cfg     Config
	release string

	// builtin and loadable are the module index of the release, keyed by
	// module name, and indexed reports whether either file was readable.
	builtin, loadable map[string]bool
	indexed           bool

	// config is the build configuration, keyed by option, and configFrom is
	// the file it came from. config is nil when none was readable.
	config     map[string]string
	configFrom string
}

// load reads the module index and the build configuration.
func (k *kernelSources) load() {
	if k.release == "" {
		return
	}
	for _, dir := range k.moduleDirs() {
		builtin, builtinErr := readModuleList(filepath.Join(dir, "modules.builtin"))
		loadable, loadableErr := readModuleList(filepath.Join(dir, "modules.dep"))
		if builtinErr == nil || loadableErr == nil {
			k.builtin, k.loadable, k.indexed = builtin, loadable, true
			break
		}
	}

	candidates := []string{k.cfg.procPath("config.gz")}
	for _, dir := range k.moduleDirs() {
		candidates = append(candidates, filepath.Join(dir, "config"))
	}
	for _, path := range candidates {
		if config, err := readKernelConfig(path); err == nil {
			k.config, k.configFrom = config, path
			return
		}
	}
}

// moduleDirs is where the release's modules may be: /usr/lib/modules on a host
// whose /lib is merged into /usr, which every distribution this product
// supports is, and /lib/modules on one that is not.
func (k *kernelSources) moduleDirs() []string {
	root := k.cfg.host()
	return []string{
		filepath.Join(root, "usr", "lib", "modules", k.release),
		filepath.Join(root, "lib", "modules", k.release),
	}
}

// moduleState is what the kernel has of one module.
type moduleState int

const (
	moduleUnknown  moduleState = iota // nothing readable says
	moduleMissing                     // the index is readable and does not list it
	moduleLoadable                    // a module file the kernel has not loaded
	modulePresent                     // loaded, or built into the kernel
)

// module looks a module up in the running kernel and then in the index.
func (k *kernelSources) module(name string) (moduleState, string) {
	if isDir(k.cfg.sysfsPath("module", name)) {
		return modulePresent, name + " is loaded"
	}
	switch {
	case k.builtin[name]:
		return modulePresent, name + " is built into the kernel"
	case k.loadable[name]:
		return moduleLoadable, name + " is a module of " + k.release + " and not loaded"
	case k.indexed:
		return moduleMissing, "the module index of " + k.release + " lists no " + name
	}
	return moduleUnknown, ""
}

// option is one build option: `y`, `m`, or `n` for an option the configuration
// does not set, and empty when no configuration was readable.
func (k *kernelSources) option(name string) string {
	if k.config == nil {
		return ""
	}
	if value, set := k.config[name]; set {
		return value
	}
	return "n"
}

// optionDetail is the evidence an option's value is.
func (k *kernelSources) optionDetail(name string) string {
	value := k.option(name)
	if value == "n" {
		return fmt.Sprintf("%s does not set %s", k.configFrom, name)
	}
	return fmt.Sprintf("%s sets %s=%s", k.configFrom, name, value)
}

// multipath reads the native NVMe multipath state.
//
// The module parameter answers first, since it is the running kernel's own
// state. The build configuration comes before the parameter's absence, because
// a kernel built with CONFIG_NVME_MULTIPATH_PARAM off has native multipath and
// no parameter. Only with no configuration to ask does a loaded nvme_core
// without the parameter conclude that multipath was not built.
func (k *kernelSources) multipath() Multipath {
	parameter := k.cfg.sysfsPath("module", "nvme_core", "parameters", "multipath")
	if raw, err := os.ReadFile(parameter); err == nil {
		switch strings.TrimSpace(string(raw)) {
		case "Y":
			return Multipath{MultipathEnabled, "nvme_core.multipath is Y"}
		case "N":
			return Multipath{MultipathDisabled, "nvme_core.multipath is N, so the kernel was booted with it switched off"}
		}
	}

	switch k.option("CONFIG_NVME_MULTIPATH") {
	case "y":
		if k.multipathOffOnCommandLine() {
			return Multipath{MultipathDisabled, k.optionDetail("CONFIG_NVME_MULTIPATH") +
				", and the kernel command line sets nvme_core.multipath off"}
		}
		return Multipath{MultipathEnabled, k.optionDetail("CONFIG_NVME_MULTIPATH")}
	case "n":
		return Multipath{MultipathNotBuilt, k.optionDetail("CONFIG_NVME_MULTIPATH")}
	}

	if isDir(k.cfg.sysfsPath("module", "nvme_core")) {
		return Multipath{MultipathNotBuilt,
			"nvme_core is loaded and has no multipath parameter, so the kernel was built without CONFIG_NVME_MULTIPATH"}
	}
	return Multipath{MultipathUnknown,
		"nvme_core is not loaded and the kernel configuration is not readable"}
}

// multipathOffOnCommandLine reports whether the kernel was booted with native
// multipath switched off.
func (k *kernelSources) multipathOffOnCommandLine() bool {
	raw, err := os.ReadFile(k.cfg.procPath("cmdline"))
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(raw)) {
		value, found := strings.CutPrefix(field, "nvme_core.multipath=")
		if found && (value == "N" || value == "n" || value == "0") {
			return true
		}
	}
	return false
}

// capability concludes an optional capability from its module and its build
// option, after the caller has asked the running kernel.
func (k *kernelSources) capability(module, option string) Capability {
	state, detail := k.module(module)
	switch state {
	case modulePresent:
		return Capability{CapabilityAvailable, detail}
	case moduleLoadable:
		return Capability{CapabilityLoadable, detail}
	}
	switch k.option(option) {
	case "y":
		return Capability{CapabilityAvailable, k.optionDetail(option)}
	case "m":
		return Capability{CapabilityLoadable, k.optionDetail(option)}
	case "n":
		return Capability{CapabilityAbsent, k.optionDetail(option)}
	}
	if state == moduleMissing {
		return Capability{CapabilityAbsent, detail}
	}
	return Capability{CapabilityUnknown, fmt.Sprintf(
		"%s is not loaded, and neither the module index nor the kernel configuration is readable", module)}
}

// tun reads /dev/net/tun.
func (k *kernelSources) tun() Capability {
	if isDir(k.cfg.sysfsPath("class", "misc", "tun")) {
		return Capability{CapabilityAvailable, "the kernel has registered /dev/net/tun"}
	}
	return k.capability("tun", "CONFIG_TUN")
}

// pnfsBlock reads the pNFS block layout driver.
func (k *kernelSources) pnfsBlock() Capability {
	return k.capability("blocklayoutdriver", "CONFIG_PNFS_BLOCK")
}

// readKVM reads /dev/kvm.
//
// KVM differs from the other two in that a kernel with the module can still be
// unable to offer it: the vendor module refuses to load on a CPU without
// hardware virtualization, which is every virtual machine whose hypervisor does
// not pass it through. A loaded KVM with no device, or an x86 CPU that reports
// neither vmx nor svm, is therefore Absent whatever the module index says.
func (k *kernelSources) readKVM() Capability {
	if isDir(k.cfg.sysfsPath("class", "misc", "kvm")) {
		return Capability{CapabilityAvailable, "the kernel has registered /dev/kvm"}
	}
	for _, module := range []string{"kvm_intel", "kvm_amd", "kvm"} {
		if isDir(k.cfg.sysfsPath("module", module)) {
			return Capability{CapabilityAbsent, module +
				" is loaded and registered no /dev/kvm, so the CPU offers this kernel no hardware virtualization"}
		}
	}

	flags, x86 := k.cpuFlags()
	vendor := ""
	switch {
	case flags["vmx"]:
		vendor = "kvm_intel"
	case flags["svm"]:
		vendor = "kvm_amd"
	case x86:
		return Capability{CapabilityAbsent,
			"the CPU reports neither vmx nor svm: it has no hardware virtualization, or it is a virtual machine without nested virtualization"}
	}
	if vendor != "" {
		if state, detail := k.module(vendor); state == moduleLoadable {
			return Capability{CapabilityLoadable, detail}
		}
	}
	return k.capability("kvm", "CONFIG_KVM")
}

// cpuFlags is the flags line of /proc/cpuinfo, and whether there was one. Only
// x86 has a flags line, so its absence says nothing about the CPU.
func (k *kernelSources) cpuFlags() (map[string]bool, bool) {
	file, err := os.Open(k.cfg.procPath("cpuinfo"))
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found || strings.TrimSpace(key) != "flags" {
			continue
		}
		flags := map[string]bool{}
		for _, flag := range strings.Fields(value) {
			flags[flag] = true
		}
		return flags, true
	}
	return nil, false
}

// readModuleList reads modules.builtin or modules.dep into a set of module
// names. A line names the module's file, such as
// `kernel/arch/x86/kvm/kvm-intel.ko.xz`, and modules.dep follows it with a colon
// and the module's dependencies.
func readModuleList(path string) (map[string]bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	modules := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		entry, _, _ := strings.Cut(scanner.Text(), ":")
		if entry = strings.TrimSpace(entry); entry != "" {
			modules[moduleName(entry)] = true
		}
	}
	return modules, scanner.Err()
}

// moduleName is the name the kernel knows a module file by: the file name
// without its extensions, with every dash an underscore, which is how
// kvm-intel.ko becomes the module kvm_intel.
func moduleName(path string) string {
	name, _, _ := strings.Cut(filepath.Base(path), ".ko")
	return strings.ReplaceAll(name, "-", "_")
}

// readKernelConfig reads a kernel build configuration into the options it sets.
// A gzip-compressed file, which is what /proc/config.gz is, is decompressed. An
// option the file only mentions in a "# CONFIG_X is not set" comment is absent
// from the result, which option reads as not set.
func readKernelConfig(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var reader io.Reader = file
	if strings.HasSuffix(path, ".gz") {
		unzipped, err := gzip.NewReader(file)
		if err != nil {
			return nil, fmt.Errorf("decompress %s: %w", path, err)
		}
		defer func() { _ = unzipped.Close() }()
		reader = unzipped
	}

	config := map[string]string{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, value, found := strings.Cut(line, "="); found {
			config[name] = strings.Trim(value, `"`)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return config, nil
}

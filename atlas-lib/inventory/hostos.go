// Which operating system a worker runs, read from the host's own os-release
// file, and which hardware the kernel says it runs on.
//
// It sits beside the Kubernetes distribution reading rather than replacing it,
// because the two answer different questions about the same machine. The
// distribution decides what Kubernetes does to the host. This decides what the
// host itself offers: whether a kernel module has to be installed before
// NVMe-oF works, which package manager would install it, and whether the binary
// that would do so is built for this architecture. A storage node on Ubuntu
// needs linux-modules-extra for its kernel, one on a Red Hat host does not, and
// one on Talos has no package manager to ask.
//
// The reading is a fact about the machine and not a decision about it. Nothing
// here says what to install or whether a host is supported: it reports the
// distribution, the family it belongs to, its version, and its architecture,
// and the caller holding the policy decides.
//
// # Reading a host from inside a container
//
// os-release is the reading with the sharpest version of this package's usual
// trap. Every container image carries one of its own, so a process that reads
// /etc/os-release from inside a pod gets an answer, the image's, and nothing
// about it looks wrong. [Config.HostRoot] is therefore where the host's root
// filesystem is mounted, and a caller in a pod has to set it.

package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHostRoot is the host's root filesystem for a process that is not in a
// container of its own.
const DefaultHostRoot = "/"

// osReleasePaths are the files a distribution states itself in, in the order
// systemd documents: /etc/os-release is the one a host may have edited, and
// /usr/lib/os-release is the one its packaging shipped. On most hosts the first
// is a relative symlink to the second.
var osReleasePaths = []string{
	filepath.Join("etc", "os-release"),
	filepath.Join("usr", "lib", "os-release"),
}

// Distro is a Linux distribution, as its own os-release ID names it.
//
// The values are the IDs verbatim, lowercase, so that a caller comparing
// against one of the constants below is comparing against what the host
// actually wrote. A distribution with no constant here is still reported. It
// simply has no name in this package.
type Distro string

const (
	DistroUbuntu             Distro = "ubuntu"
	DistroDebian             Distro = "debian"
	DistroRHEL               Distro = "rhel"
	DistroCentOS             Distro = "centos"
	DistroRocky              Distro = "rocky"
	DistroAlmaLinux          Distro = "almalinux"
	DistroFedora             Distro = "fedora"
	DistroAmazonLinux        Distro = "amzn"
	DistroOracleLinux        Distro = "ol"
	DistroSLES               Distro = "sles"
	DistroOpenSUSE           Distro = "opensuse"
	DistroOpenSUSELeap       Distro = "opensuse-leap"
	DistroOpenSUSETumbleweed Distro = "opensuse-tumbleweed"
	DistroAlpine             Distro = "alpine"
	DistroArch               Distro = "arch"
	DistroFlatcar            Distro = "flatcar"
	DistroTalos              Distro = "talos"
)

// OSFamily is the packaging tradition a distribution belongs to.
//
// It is the question most callers actually have, because what differs between
// Ubuntu and Debian is rarely what a storage node needs and what differs
// between Ubuntu and Rocky always is. A host whose family is empty is one this
// package cannot place, which includes the hosts that have no package manager
// at all: Talos and Flatcar are not a family with no name, they are machines
// where the question does not arise.
type OSFamily string

const (
	OSFamilyDebian OSFamily = "Debian"
	OSFamilyRedHat OSFamily = "RedHat"
	OSFamilySUSE   OSFamily = "SUSE"
	OSFamilyAlpine OSFamily = "Alpine"
	OSFamilyArch   OSFamily = "Arch"
)

// distroFamilies places every distribution this package names.
//
// It is keyed by os-release ID and not by the name lsb_release prints, which is
// the one difference worth stating: lsb_release calls Red Hat Enterprise Linux
// RedHatEnterprise and os-release calls it `rhel`, so a table written from the
// first reports no family for Red Hat itself.
var distroFamilies = map[Distro]OSFamily{
	DistroUbuntu:             OSFamilyDebian,
	DistroDebian:             OSFamilyDebian,
	DistroRHEL:               OSFamilyRedHat,
	DistroCentOS:             OSFamilyRedHat,
	DistroRocky:              OSFamilyRedHat,
	DistroAlmaLinux:          OSFamilyRedHat,
	DistroFedora:             OSFamilyRedHat,
	DistroAmazonLinux:        OSFamilyRedHat,
	DistroOracleLinux:        OSFamilyRedHat,
	DistroSLES:               OSFamilySUSE,
	DistroOpenSUSE:           OSFamilySUSE,
	DistroOpenSUSELeap:       OSFamilySUSE,
	DistroOpenSUSETumbleweed: OSFamilySUSE,
	DistroAlpine:             OSFamilyAlpine,
	DistroArch:               OSFamilyArch,
}

// HostOS is what a worker's operating system is.
type HostOS struct {
	// Distro is the distribution's os-release ID, such as `ubuntu` or `rocky`.
	// It is empty when no os-release could be read.
	Distro Distro

	// Family is the packaging tradition Distro belongs to, concluded from the
	// ID itself or, for a derivative this package does not name, from the
	// distributions its ID_LIKE says it is built on. It is empty when neither
	// says anything known.
	Family OSFamily

	// Version is the os-release VERSION_ID, such as 22.04 or 9.4. A rolling
	// distribution states none and leaves this empty.
	Version string

	// PrettyName is the distribution's own one-line description, such as
	// `Ubuntu 22.04.4 LTS`, which is what a report shows a reviewer: a version
	// they recognize reads faster than an ID and a number.
	PrettyName string

	// Architecture is the hardware the kernel says it is running on, which is
	// uname's machine field: `x86_64`, `aarch64`. It is the kernel's answer and not
	// the reading process's, so a 32-bit binary on a 64-bit host reports the
	// host.
	Architecture string
}

// MajorVersion is Version up to its first separator, which is the granularity a
// support matrix is usually written at: Rocky 9.4 and Rocky 9.6 are both
// Rocky 9.
func (h HostOS) MajorVersion() string {
	major, _, _ := strings.Cut(h.Version, ".")
	return major
}

// MachineReader answers what hardware the kernel is running on.
//
// It is a seam because the answer comes from a system call rather than from a
// tree of files, so a test reading a captured host has nothing to point it at.
type MachineReader func() (string, error)

// host resolves the root filesystem, so that the zero Config reads this
// machine.
func (c Config) host() string {
	if c.HostRoot == "" {
		return DefaultHostRoot
	}
	return c.HostRoot
}

// machine is the reader to use, defaulted.
func (c Config) machine() MachineReader {
	if c.Machine != nil {
		return c.Machine
	}
	return LocalMachine
}

// ReadHostOS reads the host's distribution and architecture.
//
// The two halves fail separately and are returned together, because they come
// from different places: a host whose os-release is missing still has an
// architecture, and a caller that cannot make the system call still learns
// which distribution the host runs. The error joins whichever failed, and the
// HostOS carries whichever did not.
//
// A missing os-release is a failure and not a host with no distribution. Every
// host this product supports ships one, so the realistic reason for its absence
// is a probe that was not given the host's root filesystem, and reporting an
// empty distribution would present that as a reading that was taken.
func ReadHostOS(cfg Config) (HostOS, error) {
	var errs []error

	osRelease, err := readOSRelease(cfg.host())
	if err != nil {
		errs = append(errs, err)
	}

	machine, err := cfg.machine()()
	if err != nil {
		errs = append(errs, fmt.Errorf("read the machine's architecture: %w", err))
	}
	osRelease.Architecture = machine

	return osRelease, errors.Join(errs...)
}

// readOSRelease reads the first os-release the host root carries.
//
// A file that exists but cannot be read ends the search rather than falling
// through to the next candidate: an unreadable /etc/os-release is a permission
// problem to report, not a reason to answer from the packaged copy that may say
// something else.
func readOSRelease(root string) (HostOS, error) {
	for _, rel := range osReleasePaths {
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if err == nil {
			return ParseOSRelease(string(raw)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return HostOS{}, fmt.Errorf("read %s: %w", path, err)
		}
	}
	return HostOS{}, fmt.Errorf("the host root %s carries no os-release, so its distribution is unknown", root)
}

// ParseOSRelease reads an os-release file's text.
//
// It is exported for the caller that already holds the file: an inventory
// gathered from somewhere other than a live host, or a report being read back.
// The architecture is not in the file and is left empty.
func ParseOSRelease(text string) HostOS {
	fields := osReleaseFields(text)

	host := HostOS{
		Distro:     Distro(strings.ToLower(fields["ID"])),
		Version:    fields["VERSION_ID"],
		PrettyName: fields["PRETTY_NAME"],
	}
	host.Family = familyOf(host.Distro, fields["ID_LIKE"])
	return host
}

// familyOf places a distribution, by its own ID where that is known and by what
// it says it is built on where it is not.
//
// ID_LIKE is a list in the distribution's own order of closeness, so the first
// entry that is placeable is the answer. A derivative nothing here names still
// gets a family this way, which is the point: the question a caller asks the
// family is which package manager the host has, and a derivative keeps its
// parent's.
func familyOf(distro Distro, like string) OSFamily {
	if family, ok := distroFamilies[distro]; ok {
		return family
	}
	for _, parent := range strings.Fields(like) {
		if family, ok := distroFamilies[Distro(strings.ToLower(parent))]; ok {
			return family
		}
	}
	return ""
}

// osReleaseFields parses the file into values by key.
//
// The format is a subset of shell assignments: one KEY=value per line, comments
// and blank lines ignored, and values optionally quoted. Anything that is not
// an assignment is skipped rather than failing the read, because a file with
// one unparsable line still states the distribution on the others.
func osReleaseFields(text string) map[string]string {
	fields := map[string]string{}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		fields[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return fields
}

// unquote strips the quoting a value may carry.
//
// Only a double-quoted value carries escapes, and only the four the format
// defines: the quote itself, the backslash, the dollar sign, and the backtick.
// A single-quoted value is literal, which is why the two are not unescaped the
// same way.
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	quote := value[0]
	if quote != '"' && quote != '\'' || value[len(value)-1] != quote {
		return value
	}

	value = value[1 : len(value)-1]
	if quote == '\'' {
		return value
	}
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`").Replace(value)
}

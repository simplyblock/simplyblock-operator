// What the host OS reading concludes from an os-release file, and what it does
// when that file is missing, is quoted, or names a distribution nothing here
// knows.
//
// The fixtures are real os-release files, trimmed to the keys the reading uses.
// A hand-written approximation would not exercise what actually breaks: the
// quoting, the keys a distribution omits, and the derivatives that state their
// family only in ID_LIKE.

package inventory

import (
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/inventory/transcript"
)

// ubuntuOSRelease is Ubuntu 22.04's file, which is the host the storage node
// runs on most often and the one whose kernel modules need a package installed.
const ubuntuOSRelease = `PRETTY_NAME="Ubuntu 22.04.4 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
VERSION="22.04.4 LTS (Jammy Jellyfish)"
VERSION_CODENAME=jammy
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
UBUNTU_CODENAME=jammy`

// osReleaseHost is a host whose only distinguishing file is its os-release.
func osReleaseHost(path, text string) fixture {
	return fixture{files: map[string]string{path: text}}
}

// hostOSFixturePath is where the whole-host fixture keeps its os-release, named
// so that a test can take it back out again.
const hostOSFixturePath = "etc/os-release"

// osReleaseFixture is the OS half of the whole-host fixture the collection
// tests walk.
func osReleaseFixture() fixture {
	return osReleaseHost(hostOSFixturePath, ubuntuOSRelease)
}

// staticMachine is the kernel's answer to what hardware this is, fixed, so that
// a test asserts the reading rather than the machine it runs on.
func staticMachine(machine string) MachineReader {
	return func() (string, error) { return machine, nil }
}

// hostOSOf reads the OS of a fixture, with the architecture named rather than
// asked of the running kernel.
func hostOSOf(t *testing.T, f fixture, machine string) (HostOS, error) {
	t.Helper()
	return ReadHostOS(Config{HostRoot: f.write(t), Machine: staticMachine(machine)})
}

func TestReadHostOSReadsTheDistroFamilyVersionAndArchitecture(t *testing.T) {
	os, err := hostOSOf(t, osReleaseHost("etc/os-release", ubuntuOSRelease), "x86_64")
	if err != nil {
		t.Fatalf("read the host OS: %v", err)
	}

	if os.Distro != DistroUbuntu {
		t.Errorf("read the distro as %q, want %q", os.Distro, DistroUbuntu)
	}
	if os.Family != OSFamilyDebian {
		t.Errorf("concluded the family %q, want %q", os.Family, OSFamilyDebian)
	}
	if os.Version != "22.04" {
		t.Errorf("read the version as %q, want %q", os.Version, "22.04")
	}
	if os.PrettyName != "Ubuntu 22.04.4 LTS" {
		t.Errorf("read the name as %q, want %q", os.PrettyName, "Ubuntu 22.04.4 LTS")
	}
	if os.Architecture != "x86_64" {
		t.Errorf("read the architecture as %q, want %q", os.Architecture, "x86_64")
	}
}

func TestReadHostOSConcludesTheFamilyOfEveryDistroItKnows(t *testing.T) {
	// `rhel` is here because it is the one a lookup table written from
	// lsb_release's output misses: lsb_release calls it RedHatEnterprise and
	// os-release calls it `rhel`, and a table carrying only the first reports
	// no family for Red Hat itself.
	for _, tc := range []struct {
		id     Distro
		family OSFamily
	}{
		{DistroUbuntu, OSFamilyDebian},
		{DistroDebian, OSFamilyDebian},
		{DistroRHEL, OSFamilyRedHat},
		{DistroCentOS, OSFamilyRedHat},
		{DistroRocky, OSFamilyRedHat},
		{DistroAlmaLinux, OSFamilyRedHat},
		{DistroFedora, OSFamilyRedHat},
		{DistroAmazonLinux, OSFamilyRedHat},
		{DistroOracleLinux, OSFamilyRedHat},
		{DistroSLES, OSFamilySUSE},
		{DistroOpenSUSELeap, OSFamilySUSE},
		{DistroAlpine, OSFamilyAlpine},
		{DistroArch, OSFamilyArch},
		// Talos and Flatcar have no family, because the families name a
		// packaging tradition and neither host has a package manager at all.
		{DistroTalos, ""},
		{DistroFlatcar, ""},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			os := ParseOSRelease("ID=" + string(tc.id))
			if os.Distro != tc.id {
				t.Errorf("read the distro as %q, want %q", os.Distro, tc.id)
			}
			if os.Family != tc.family {
				t.Errorf("concluded the family %q, want %q", os.Family, tc.family)
			}
		})
	}
}

func TestReadHostOSConcludesADerivativesFamilyFromIDLike(t *testing.T) {
	// A derivative nothing here has heard of still says what it is built on,
	// and that answer is worth more than an empty family: the question a caller
	// asks the family is which package manager the host has.
	for _, tc := range []struct {
		text   string
		distro Distro
		family OSFamily
	}{
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"", "linuxmint", OSFamilyDebian},
		{"ID=cachyos\nID_LIKE=arch", "cachyos", OSFamilyArch},
		{"ID=eurolinux\nID_LIKE=\"rhel centos fedora\"", "eurolinux", OSFamilyRedHat},
		// An ID_LIKE naming nothing known leaves the family unstated rather
		// than guessing at the first entry.
		{"ID=serenity\nID_LIKE=\"haiku plan9\"", "serenity", ""},
	} {
		t.Run(string(tc.distro), func(t *testing.T) {
			os := ParseOSRelease(tc.text)
			if os.Distro != tc.distro {
				t.Errorf("read the distro as %q, want %q", os.Distro, tc.distro)
			}
			if os.Family != tc.family {
				t.Errorf("concluded the family %q, want %q", os.Family, tc.family)
			}
		})
	}
}

func TestParseOSReleaseUnquotesAndSkipsWhatIsNotAnAssignment(t *testing.T) {
	os := ParseOSRelease(`# the header a generator writes

ID='rocky'
VERSION_ID="9.4"
PRETTY_NAME="Rocky Linux 9.4 (Blue \"Onyx\")"
NOT AN ASSIGNMENT
  ANSI_COLOR="0;32"
`)

	if os.Distro != DistroRocky {
		t.Errorf("read the distro as %q, want %q", os.Distro, DistroRocky)
	}
	if os.Version != "9.4" {
		t.Errorf("read the version as %q, want %q", os.Version, "9.4")
	}
	if os.PrettyName != `Rocky Linux 9.4 (Blue "Onyx")` {
		t.Errorf("read the name as %q, want %q", os.PrettyName, `Rocky Linux 9.4 (Blue "Onyx")`)
	}
}

func TestParseOSReleaseLowercasesAnIDThatWasNot(t *testing.T) {
	// The specification says the ID is lowercase and some distributions have
	// shipped one that is not. A caller comparing against DistroRHEL should not
	// have to know which.
	if os := ParseOSRelease("ID=RHEL"); os.Distro != DistroRHEL {
		t.Errorf("read the distro as %q, want %q", os.Distro, DistroRHEL)
	}
}

func TestReadHostOSFallsBackToTheUsrLibCopy(t *testing.T) {
	// /etc/os-release is the one a host may have edited and /usr/lib/os-release
	// is the one its packaging ships. A host carrying only the second is read
	// from it rather than reported unknown.
	os, err := hostOSOf(t, osReleaseHost("usr/lib/os-release", ubuntuOSRelease), "x86_64")
	if err != nil {
		t.Fatalf("read the host OS: %v", err)
	}
	if os.Distro != DistroUbuntu {
		t.Errorf("read the distro as %q, want %q", os.Distro, DistroUbuntu)
	}
}

func TestReadHostOSFollowsTheEtcSymlinkIntoUsrLib(t *testing.T) {
	// The ordinary arrangement: /etc/os-release is a relative symlink to the
	// packaged file. It resolves inside the host root, so a pod that mounts
	// both trees reads the host's file and not its own.
	host := fixture{
		files: map[string]string{"usr/lib/os-release": ubuntuOSRelease},
		links: map[string]string{"etc/os-release": "../usr/lib/os-release"},
	}

	os, err := hostOSOf(t, host, "aarch64")
	if err != nil {
		t.Fatalf("read the host OS: %v", err)
	}
	if os.Distro != DistroUbuntu || os.Architecture != "aarch64" {
		t.Errorf("read %q on %q, want %q on %q", os.Distro, os.Architecture, DistroUbuntu, "aarch64")
	}
}

func TestReadHostOSFailsWhenTheHostHasNoOSReleaseAndStillNamesTheArchitecture(t *testing.T) {
	// A probe that was not given the host's root filesystem reads no
	// os-release, and that is a failure rather than a host with no
	// distribution: an empty distro reported as a reading would be taken for
	// one that was taken.
	os, err := hostOSOf(t, fixture{}, "x86_64")
	if err == nil {
		t.Fatal("read no os-release and reported no error")
	}
	if os.Distro != "" {
		t.Errorf("read the distro as %q with no file to read it from", os.Distro)
	}
	// The architecture comes from the kernel rather than from the file, so it
	// survives the failure and travels beside it.
	if os.Architecture != "x86_64" {
		t.Errorf("read the architecture as %q, want %q", os.Architecture, "x86_64")
	}
}

func TestReadHostOSReportsTheDistroWhenTheArchitectureCannotBeRead(t *testing.T) {
	root := osReleaseHost("etc/os-release", ubuntuOSRelease).write(t)
	unreadable := errors.New("uname refused")

	os, err := ReadHostOS(Config{
		HostRoot: root,
		Machine:  func() (string, error) { return "", unreadable },
	})
	if !errors.Is(err, unreadable) {
		t.Fatalf("reported %v, want the failure the kernel gave", err)
	}
	if os.Distro != DistroUbuntu {
		t.Errorf("read the distro as %q, want %q", os.Distro, DistroUbuntu)
	}
}

func TestHostOSMajorVersionIsTheVersionUpToTheFirstDot(t *testing.T) {
	for version, want := range map[string]string{
		"22.04": "22",
		"9.4":   "9",
		"9":     "9",
		"v1.7":  "v1",
		"":      "",
	} {
		if got := (HostOS{Version: version}).MajorVersion(); got != want {
			t.Errorf("read %q as major version %q, want %q", version, got, want)
		}
	}
}

func TestConfigDefaultsToTheLiveHostsRootFilesystem(t *testing.T) {
	var cfg Config
	if cfg.host() != DefaultHostRoot {
		t.Errorf("an unset HostRoot resolves to %q, want %q", cfg.host(), DefaultHostRoot)
	}
}

func TestCollectReadsTheHostOS(t *testing.T) {
	inv, err := collect(t, wholeHost(), blankDisks())
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	if inv.HostOS.Distro != DistroUbuntu {
		t.Errorf("read the distro as %q, want %q", inv.HostOS.Distro, DistroUbuntu)
	}
	if inv.HostOS.Family != OSFamilyDebian {
		t.Errorf("concluded the family %q, want %q", inv.HostOS.Family, OSFamilyDebian)
	}
}

func TestCollectReportsAHostWhoseOSCouldNotBeRead(t *testing.T) {
	// The rest of the inventory is unaffected: a worker whose root filesystem
	// was not mounted into the probe still has CPUs, memory, and disks.
	host := wholeHost()
	delete(host.files, hostOSFixturePath)

	inv, err := collect(t, host, blankDisks())
	if err == nil {
		t.Fatal("read no os-release and reported no error")
	}
	if !strings.Contains(err.Error(), "os-release") {
		t.Errorf("reported %v, which does not say which file was missing", err)
	}
	if inv.CPU.OnlineCount != 16 {
		t.Errorf("read %d online CPUs beside the failure, want 16", inv.CPU.OnlineCount)
	}
}

func TestReadHostOSReadsACapturedHostsOSRelease(t *testing.T) {
	// A transcript carries the host's os-release in a section of its own, under
	// a root of its own, because a path relative to the host's filesystem and
	// one relative to sysfs are two namespaces and a capture must not merge
	// them.
	host := transcript.Host{
		Root:    map[string]string{"etc/os-release": "ID=rocky\nVERSION_ID=\"9.4\""},
		Machine: "aarch64",
	}

	root := t.TempDir()
	if err := host.Materialize(root); err != nil {
		t.Fatalf("materialize the transcript: %v", err)
	}

	read, err := ReadHostOS(Config{
		HostRoot: transcript.HostRootOf(root),
		Machine:  staticMachine(host.Machine),
	})
	if err != nil {
		t.Fatalf("read the captured host's OS: %v", err)
	}
	if read.Distro != DistroRocky || read.Version != "9.4" || read.Architecture != "aarch64" {
		t.Errorf("read %+v, want rocky 9.4 on aarch64", read)
	}
}

func TestFamilyOfPlacesADistroWithoutItsFile(t *testing.T) {
	// A caller holding a distro read somewhere else, off a report or a document
	// rather than off a host, asks the same question the reading answers and
	// must not have to carry a second table to answer it.
	if got := FamilyOf(DistroRocky); got != OSFamilyRedHat {
		t.Errorf("placed %q in %q, want %q", DistroRocky, got, OSFamilyRedHat)
	}
	if got := FamilyOf("gentoo"); got != "" {
		t.Errorf("placed an unknown distro in %q, want no family", got)
	}
}

// What the host's /dev/disk tree is read as, and which of a device's links is
// the one to write down.
//
// The subject is the preference order. Every link under /dev/disk/by-id and
// /dev/disk/by-partuuid names the same device, so a reading that returned any
// of them would pass these tests while recording a name that says less about
// the device than the one beside it: a serial-derived link is only as stable as
// the enclosure that reports the serial, and a partition's by-id link is only
// as stable as its parent disk's identity.

package blockdev

import (
	"path/filepath"
	"testing"
)

// devTree is a /dev with the udev link directories, built from device name to
// the links pointing at it. The links are relative, as udev writes them.
func devTree(t *testing.T, links map[string]string) string {
	t.Helper()
	tr := tree{files: map[string]string{}, links: map[string]string{}}
	// The device nodes themselves: a symlink resolves to nothing without them,
	// and a scan never opens one, so an ordinary file stands in.
	for _, target := range links {
		tr.files[target] = ""
	}
	for link, target := range links {
		tr.links[link] = "../../" + target
	}
	return tr.write(t)
}

func TestTheWWNLinkIsPreferredForADisk(t *testing.T) {
	dev := devTree(t, map[string]string{
		"disk/by-id/wwn-0x5000c500a1b2c3d4":               "sda",
		"disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0": "sda",
		"disk/by-id/ata-ST2000DM008-2FR102_ZFL1ABCD":      "sda",
	})

	set, err := ReadStableLinks(ScanConfig{DevRoot: dev})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}

	want := filepath.Join(dev, "disk/by-id/wwn-0x5000c500a1b2c3d4")
	if got := set.Preferred(filepath.Join(dev, "sda")); got != want {
		t.Errorf("preferred %q, want %q: a WWN is the device's own name, where a "+
			"model-and-serial link is the enclosure's account of it", got, want)
	}
}

func TestThePartUUIDLinkIsPreferredForAPartition(t *testing.T) {
	dev := devTree(t, map[string]string{
		"disk/by-partuuid/6f7c8d91-2a3b-4c5d-8e9f-0a1b2c3d4e5f": "sda4",
		"disk/by-id/wwn-0x5000c500a1b2c3d4-part4":               "sda4",
		"disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0-part4": "sda4",
	})

	set, err := ReadStableLinks(ScanConfig{DevRoot: dev})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}

	want := filepath.Join(dev, "disk/by-partuuid/6f7c8d91-2a3b-4c5d-8e9f-0a1b2c3d4e5f")
	if got := set.Preferred(filepath.Join(dev, "sda4")); got != want {
		t.Errorf("preferred %q, want %q: the partition table's own identifier "+
			"survives the parent disk being re-exported under another serial, "+
			"and a by-id link derived from that serial does not", got, want)
	}
}

// Every link a device answers to is reported, not only the preferred one: the
// two sides of a deployment pick their own, and a selection is matched against
// the whole set rather than against one side's preference.
func TestEveryLinkOfADeviceIsReported(t *testing.T) {
	dev := devTree(t, map[string]string{
		"disk/by-id/wwn-0x5000c500a1b2c3d4":               "sda",
		"disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0": "sda",
	})

	set, err := ReadStableLinks(ScanConfig{DevRoot: dev})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}

	if got := len(set[filepath.Join(dev, "sda")]); got != 2 {
		t.Errorf("reported %d links for sda, want 2", got)
	}
}

// The filesystem UUID and the label are not device identity: both are written
// by mkfs and both move to another device when the filesystem is restored onto
// one, so a deployment that recorded them would name whatever carries the
// filesystem rather than the disk it was given.
func TestTheFilesystemsOwnNamesAreNotStableLinks(t *testing.T) {
	dev := devTree(t, map[string]string{
		"disk/by-uuid/0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9": "sda",
		"disk/by-label/data":                  "sda",
		"disk/by-path/pci-0000:00:1f.2-ata-1": "sda",
	})

	set, err := ReadStableLinks(ScanConfig{DevRoot: dev})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}

	if got := set.Preferred(filepath.Join(dev, "sda")); got != "" {
		t.Errorf("preferred %q, want no link at all", got)
	}
}

// A device udev published no link for has no persistent name, which is an
// answer rather than a failure: the caller falls back to the kernel path and
// says so.
func TestADeviceWithNoLinkHasNoPreferredPath(t *testing.T) {
	dev := devTree(t, map[string]string{
		"disk/by-id/wwn-0x5000c500a1b2c3d4": "sda",
	})

	set, err := ReadStableLinks(ScanConfig{DevRoot: dev})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}

	if got := set.Preferred(filepath.Join(dev, "sdb")); got != "" {
		t.Errorf("preferred %q for a device with no link, want none", got)
	}
}

// A host whose /dev carries no disk directory is a host with no persistent
// names, which every caller handles already. Failing the whole reading over it
// would take a discovery run down on a machine whose devices are all readable.
func TestAHostWithNoLinkDirectoryIsNotAFailure(t *testing.T) {
	set, err := ReadStableLinks(ScanConfig{DevRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}
	if len(set) != 0 {
		t.Errorf("read %d devices, want none", len(set))
	}
}

// A link is read without the device it names being there.
//
// Two cases need it. A captured machine's tree carries the links the host had
// and, deliberately, none of its device nodes — creating those would need root
// and would make a capture unusable as a fixture. And udev leaves a link behind
// when a device goes while something still holds it, which costs nothing here:
// the index is keyed by the path a link names, and every lookup comes from a
// device the scan found, so a link to a device that is gone lands under a key
// nobody asks about.
func TestALinkIsReadWithoutTheDeviceBeingThere(t *testing.T) {
	tr := tree{files: map[string]string{}, links: map[string]string{}}
	tr.links["disk/by-id/wwn-0x5000c500a1b2c3d4"] = "../../sda"
	dev := tr.write(t)

	set, err := ReadStableLinks(ScanConfig{DevRoot: dev})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}

	want := filepath.Join(dev, "disk/by-id/wwn-0x5000c500a1b2c3d4")
	if got := set.Preferred(filepath.Join(dev, "sda")); got != want {
		t.Errorf("preferred %q, want %q", got, want)
	}
}

// A gendisk the kernel presents to nobody carries udev links of its own on a
// real host, and reading them costs nothing: they resolve to device paths no
// scan reports, so they sit in the index under keys nobody looks up.
func TestTheLinksOfAHiddenPathAreHarmless(t *testing.T) {
	dev := devTree(t, map[string]string{
		"disk/by-id/nvme-uuid.8a695f15-8227-4b90-a0d5-66b9e72b496f": "nvme8n1",
	})

	set, err := ReadStableLinks(ScanConfig{DevRoot: dev})
	if err != nil {
		t.Fatalf("read the stable links: %v", err)
	}
	if got := set.Preferred(filepath.Join(dev, "nvme8c3n1")); got != "" {
		t.Errorf("a hidden path reads under %q", got)
	}
}

// A candidate carries the persistent name of the device it is about, because
// that is the name whatever accepts the candidate has to write down. Reading
// the links separately and matching them up by path again is the same work at
// every call site, and a call site that skipped it would record the kernel name.
func TestACandidateCarriesItsPersistentName(t *testing.T) {
	host := storageHost()
	host.files["self/mountinfo"] = mountedBootDisk
	host.files["swaps"] = swapOnVirtio
	sysfs := host.write(t)

	dev := devTree(t, map[string]string{
		"disk/by-id/nvme-SAMSUNG_MZQL23T8HCLS-00A07_S6CVNE0T500123": "nvme0n1",
		"disk/by-id/wwn-0x5000c500a1b2c3d4":                         "sdb",
	})

	in := Inspector{
		Config:    ScanConfig{SysfsRoot: sysfs, ProcRoot: sysfs, DevRoot: dev},
		Prober:    NewProberWithOpener(contentOpener(nil), WithRegionSize(MinRegionSize)),
		Exclusive: free,
	}
	cands, err := in.Candidates(t.Context())
	if err != nil {
		t.Fatalf("read the candidates: %v", err)
	}

	for name, want := range map[string]string{
		"nvme0n1": filepath.Join(dev, "disk/by-id/nvme-SAMSUNG_MZQL23T8HCLS-00A07_S6CVNE0T500123"),
		"sdb":     filepath.Join(dev, "disk/by-id/wwn-0x5000c500a1b2c3d4"),
	} {
		if got := found(t, cands, name).StablePath; got != want {
			t.Errorf("%s carries the persistent name %q, want %q", name, got, want)
		}
	}

	// The device udev published nothing for says so, rather than reporting its
	// kernel path as though that were persistent.
	if got := found(t, cands, "vda").StablePath; got != "" {
		t.Errorf("vda carries the persistent name %q, and udev made it none", got)
	}
}

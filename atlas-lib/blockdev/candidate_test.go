// Which devices a candidacy pass hands over, and the reason it gives for each
// one it does not.
//
// The reasons are the point rather than the boolean. A discovery run reports
// candidates to a human who has to decide whether the run was right, and
// "rejected" tells that reader nothing; "the kernel holds it" and "it carries a
// partition table" are different findings with different remedies, and only one
// of the two has a flag that overrides it.

package blockdev

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// sparse serves a device whose bytes are zero except where a signature was
// placed, which is how a real disk reads: the interesting bytes are a handful
// of magic numbers in an otherwise empty megabyte.
type sparse struct {
	size  int64
	bytes map[int64][]byte
}

func (s sparse) Reader() Reader { return sparseReader{s} }

type sparseReader struct{ s sparse }

func (sparseReader) Close() error { return nil }

func (r sparseReader) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for i := range p {
		p[i] = 0
	}
	for at, b := range r.s.bytes {
		for i := range b {
			if idx := at + int64(i) - off; idx >= 0 && idx < int64(len(p)) {
				p[idx] = b[i]
			}
		}
	}
	return len(p), nil
}

// contentOpener serves the devices named in contents and refuses the rest, so a
// test states exactly which devices it expects the prober to reach.
func contentOpener(contents map[string]sparse) Opener {
	return func(_ context.Context, dev Device) (Reader, error) {
		s, ok := contents[dev.Name]
		if !ok {
			return nil, errors.New("the test served no content for " + dev.Name)
		}
		return s.Reader(), nil
	}
}

// inspectorOver builds an inspector over the fixture host, serving content for
// the devices it is given and handing over every device the kernel is asked
// about.
func inspectorOver(t *testing.T, contents map[string]sparse, exclusive ExclusiveOpener) Inspector {
	t.Helper()
	h := storageHost()
	h.files["self/mountinfo"] = mountedBootDisk
	h.files["swaps"] = swapOnVirtio
	root := h.write(t)

	if exclusive == nil {
		exclusive = free
	}
	return Inspector{
		Config:    ScanConfig{SysfsRoot: root, ProcRoot: root},
		Prober:    NewProberWithOpener(contentOpener(contents), WithRegionSize(MinRegionSize)),
		Exclusive: exclusive,
	}
}

// blank is a device of the given size whose every byte is zero.
func blank(size int64) sparse { return sparse{size: size, bytes: map[int64][]byte{}} }

// found finds one candidate by kernel name.
func found(t *testing.T, cands []Candidate, name string) Candidate {
	t.Helper()
	for _, c := range cands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s is missing from the candidates", name)
	return Candidate{}
}

func TestCandidatesHandsOverABlankUnclaimedDisk(t *testing.T) {
	in := inspectorOver(t, map[string]sparse{
		"nvme0n1": blank(6251233968 * 512),
		"sdb":     blank(3750748848 * 512),
	}, nil)

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	for _, name := range []string{"nvme0n1", "sdb"} {
		got := found(t, cands, name)
		if !got.Available() {
			t.Errorf("%s was rejected: %v", name, got.Rejections)
		}
		if got.Reading.Content != ContentBlank {
			t.Errorf("%s reads as %v, want %v", name, got.Reading.Content, ContentBlank)
		}
	}
}

func TestCandidatesRejectsEveryDeviceSomethingElseIsUsing(t *testing.T) {
	in := inspectorOver(t, map[string]sparse{
		"nvme0n1": blank(6251233968 * 512),
		"sdb":     blank(3750748848 * 512),
	}, nil)

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	for _, tc := range []struct {
		name string
		want Reason
	}{
		{"nvme1n1", ReasonMounted},   // its partitions carry the root filesystem
		{"nvme1n1p2", ReasonMounted}, // and so does it, in its own right
		{"sda", ReasonStacked},       // an LVM volume group holds it
		{"vda", ReasonSwapArea},      // it is swap
		{"loop0", ReasonNotAWholeDisk},
		{"dm-0", ReasonNotAWholeDisk},
	} {
		got := found(t, cands, tc.name)
		if got.Available() {
			t.Errorf("%s was handed over", tc.name)
			continue
		}
		if !got.RejectedFor(tc.want) {
			t.Errorf("%s was rejected for %v, want %s among them", tc.name, got.Rejections, tc.want)
		}
	}
}

func TestCandidatesDoesNotOpenADeviceItAlreadyRejected(t *testing.T) {
	// The content read is the one step that touches the device, and a device
	// something else is writing to is one nothing here should open at all. The
	// opener refuses every device, so any read that happens is a failure.
	in := inspectorOver(t, nil, nil)

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	for _, name := range []string{"nvme1n1", "nvme1n1p2", "sda", "vda", "loop0", "dm-0"} {
		got := found(t, cands, name)
		if got.RejectedFor(ReasonUnreadable) {
			t.Errorf("%s was read even though it was already rejected: %v", name, got.Rejections)
		}
		if got.Reading.Content != ContentUnknown {
			t.Errorf("%s carries the reading %v, and a device that was not read "+
				"must carry none", name, got.Reading.Content)
		}
	}
}

func TestCandidatesNamesAPartitionTableSeparatelyFromAFilesystem(t *testing.T) {
	// A partition table is the one condition an administrator can override, so
	// it is its own reason: a device whose only problem is a stale table can be
	// handed over deliberately, and one carrying a filesystem cannot.
	gpt := blank(6251233968 * 512)
	gpt.bytes[512] = []byte("EFI PART")

	xfs := blank(3750748848 * 512)
	xfs.bytes[0] = []byte("XFSB")

	in := inspectorOver(t, map[string]sparse{"nvme0n1": gpt, "sdb": xfs}, nil)
	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	partitioned := found(t, cands, "nvme0n1")
	if !partitioned.RejectedFor(ReasonPartitioned) {
		t.Errorf("a device carrying a GPT was rejected for %v, want %s",
			partitioned.Rejections, ReasonPartitioned)
	}
	if !partitioned.OnlyRejectedFor(ReasonPartitioned) {
		t.Errorf("a device whose only problem is its partition table was rejected "+
			"for %v; the override that accepts it needs that to be the only reason",
			partitioned.Rejections)
	}

	formatted := found(t, cands, "sdb")
	if !formatted.RejectedFor(ReasonNotBlank) {
		t.Errorf("a device carrying XFS was rejected for %v, want %s",
			formatted.Rejections, ReasonNotBlank)
	}
	if formatted.OnlyRejectedFor(ReasonPartitioned) {
		t.Error("a device carrying a filesystem must not be accepted by the " +
			"override that accepts a partition table")
	}
	if formatted.Reading.Type != "xfs" {
		t.Errorf("the reading names %q, and the candidate carries it so that a "+
			"reviewer sees what is on the disk", formatted.Reading.Type)
	}
}

func TestCandidatesNamesAnLVMPhysicalVolumeSeparatelyFromOtherContent(t *testing.T) {
	// A physical volume whose volume group has no active logical volume has no
	// holder and nothing mounted, so lsblk shows a bare disk. The label is the
	// only evidence, and NotBlank sends the administrator looking for a
	// filesystem that is not there; the fix is pvremove, so the reason says LVM.
	pv := blank(3750748848 * 512)
	pv.bytes[512] = []byte("LABELONE")
	pv.bytes[512+24] = []byte("LVM2 001")

	in := inspectorOver(t, map[string]sparse{"sdb": pv}, nil)
	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	got := found(t, cands, "sdb")
	if got.Available() {
		t.Fatal("handed over a disk carrying an LVM physical-volume label")
	}
	if !got.OnlyRejectedFor(ReasonLVMPhysicalVolume) {
		t.Errorf("rejected it for %v, want %s alone", got.Rejections, ReasonLVMPhysicalVolume)
	}
}

func TestCandidatesRejectsADiskWhoseContentItCouldNotRead(t *testing.T) {
	// A device that could not be read is not a device that was read and found
	// empty. This is the defect the content reading exists to remove, and it is
	// as much a defect here as it was in blkid.
	in := inspectorOver(t, map[string]sparse{"sdb": blank(3750748848 * 512)}, nil)

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	got := found(t, cands, "nvme0n1")
	if got.Available() {
		t.Error("handed over a disk whose content could not be read")
	}
	if !got.RejectedFor(ReasonUnreadable) {
		t.Errorf("rejected it for %v, want %s", got.Rejections, ReasonUnreadable)
	}
}

func TestCandidatesNeverHandsOverAFabricNamespace(t *testing.T) {
	// A namespace this node has attached is blank as far as the content reading
	// is concerned right up until a volume is written to it, and it is exactly
	// the shape of thing a discovery run is looking for. Handing one over would
	// give a volume's own bytes away as free space, so the refusal cannot rest
	// on the content: it rests on where the namespace comes from.
	root := multipathHost().write(t)
	in := Inspector{
		Config: ScanConfig{SysfsRoot: root, ProcRoot: root},
		Prober: NewProberWithOpener(contentOpener(map[string]sparse{
			"nvme3n1": blank(209715200 * 512),
			"nvme4n1": blank(3125627568 * 512),
		}), WithRegionSize(MinRegionSize)),
		Exclusive: free,
	}

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	fabric := found(t, cands, "nvme3n1")
	if fabric.Available() {
		t.Error("handed over a fabric namespace as backend storage")
	}
	if !fabric.RejectedFor(ReasonFabricNamespace) {
		t.Errorf("rejected it for %v, want %s", fabric.Rejections, ReasonFabricNamespace)
	}

	// The dual-ported local disk is the other half of the same evidence, and
	// refusing it would cost a deployment every disk in a machine that has any.
	local := found(t, cands, "nvme4n1")
	if !local.Available() {
		t.Errorf("rejected a dual-ported local NVMe disk: %v", local.Rejections)
	}
}

// A removable device is never backend storage, whatever is in it.
//
// The optical drive is the case that turned up on a real worker: a QEMU
// DVD-ROM with install media in it reports a whole disk of 924 MB on the SATA
// bus, reads as a blank device through a content probe that finds no signature
// it knows, and is therefore admitted on every other ground. Nothing else
// distinguishes it, and a cluster that took it would lose the device the moment
// the medium was ejected.
//
// The waiver rules out nothing here: this is what the device is rather than
// what is currently on it, so unlike a stale partition table there is no state
// an administrator could know better than the kernel does.
func TestCandidatesRejectsARemovableDevice(t *testing.T) {
	const (
		fixed = "devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1"
		opti  = "devices/pci0000:00/0000:00:1f.2/ata2/host2/target2:0:0/2:0:0:0/block/sr0"
		usb   = "devices/pci0000:00/0000:14:00.0/usb1/1-3/1-3:1.0/host3/target3:0:0/3:0:0:0/block/sdc"
	)
	h := tree{files: map[string]string{}, links: map[string]string{}}

	// A fixed NVMe SSD, which is what the run is for.
	h.blockAttrs(fixed, "259:0", 6251233968, false, false, false)
	h.links["class/block/nvme0n1"] = "../../" + fixed
	h.links[fixed+"/device"] = ".."

	// The optical drive, with a medium in it so that it reports a size.
	h.blockAttrs(opti, "11:0", 1805164, true, false, true)
	h.links["class/block/sr0"] = "../../" + opti
	h.links[opti+"/device"] = "../.."

	// A USB stick, which is the same statement on a different bus: removable is
	// what the kernel says about the device, not about the medium.
	h.blockAttrs(usb, "8:32", 60063744, false, false, true)
	h.links["class/block/sdc"] = "../../" + usb
	h.links[usb+"/device"] = "../.."

	// Nothing on this host is mounted or swapping: the refusal under test has
	// to rest on what the devices are, and a usage finding would supply a
	// second ground that hides whether the first one fired.
	h.files["self/mountinfo"] = ""
	h.files["swaps"] = ""

	root := h.write(t)
	in := Inspector{
		Config: ScanConfig{SysfsRoot: root, ProcRoot: root},
		// Only the fixed disk is served, which asserts the second half of the
		// rejection: a device refused on a structural ground is never opened,
		// so a removable drive's medium is not read to decide something the
		// kernel already answered. An opener reaching sr0 fails the test by
		// having no content to serve.
		Prober: NewProberWithOpener(contentOpener(map[string]sparse{
			"nvme0n1": blank(6251233968 * 512),
		}), WithRegionSize(MinRegionSize)),
		Exclusive: free,
	}

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	for _, name := range []string{"sr0", "sdc"} {
		device := found(t, cands, name)
		if device.Available() {
			t.Errorf("handed over %s, a removable device, as backend storage", name)
		}
		if !device.RejectedFor(ReasonRemovable) {
			t.Errorf("rejected %s for %v, want %s", name, device.Rejections, ReasonRemovable)
		}
		// The waiver an administrator has is for a stale partition table, and
		// it must not reach this: a device rejected for being removable and for
		// nothing else is still not a candidate.
		if device.OnlyRejectedFor(ReasonPartitioned) {
			t.Errorf("%s reads as refused only for its partition table", name)
		}
	}

	fixedDisk := found(t, cands, "nvme0n1")
	if !fixedDisk.Available() {
		t.Errorf("rejected the fixed disk beside them: %v", fixedDisk.Rejections)
	}
}

func TestCandidatesRejectsADiskTheKernelWillNotHandOver(t *testing.T) {
	held := func(path string) error {
		if path == "/dev/nvme0n1" {
			return ErrDeviceBusy
		}
		return nil
	}
	in := inspectorOver(t, map[string]sparse{"sdb": blank(3750748848 * 512)}, held)

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	got := found(t, cands, "nvme0n1")
	if !got.RejectedFor(ReasonBusy) {
		t.Errorf("rejected a device the kernel holds for %v, want %s", got.Rejections, ReasonBusy)
	}
	if got.OnlyRejectedFor(ReasonPartitioned) {
		t.Error("a busy device must not be accepted by any override")
	}
}

// Regression: 2026-10-22-block-mode-partitions-never-proposed. A partition was
// refused as NotAWholeDisk before its bytes were read, so a logical block
// device run, which may take a partition, had no reading to decide on and
// never proposed one. A partition nothing uses is now read like a disk: its
// only structural ground stays NotAWholeDisk, which the run waives, and what it
// carries decides the rest.
func TestCandidatesReadsAPartitionNothingUses(t *testing.T) {
	pv := blank(1048576 * 512)
	pv.bytes[512] = []byte("LABELONE")
	pv.bytes[512+24] = []byte("LVM2 001")

	for _, tc := range []struct {
		name    string
		content sparse
		want    []Reason
		reading Content
	}{
		{"blank", blank(1048576 * 512), []Reason{ReasonNotAWholeDisk}, ContentBlank},
		{"carrying a physical volume", pv, []Reason{ReasonNotAWholeDisk, ReasonLVMPhysicalVolume}, ContentStackLayer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Only the root partition is mounted here, so the EFI partition
			// beside it is one nothing uses.
			h := storageHost()
			h.files["self/mountinfo"] = "25 1 259:4 / / rw,relatime shared:1 - ext4 /dev/nvme1n1p2 rw"
			h.files["swaps"] = swapOnVirtio
			root := h.write(t)
			in := Inspector{
				Config: ScanConfig{SysfsRoot: root, ProcRoot: root},
				Prober: NewProberWithOpener(contentOpener(map[string]sparse{"nvme1n1p1": tc.content}),
					WithRegionSize(MinRegionSize)),
				Exclusive: free,
			}

			cands, err := in.Candidates(context.Background())
			if err != nil {
				t.Fatalf("collect the candidates: %v", err)
			}

			got := found(t, cands, "nvme1n1p1")
			if got.Reading.Content != tc.reading {
				t.Errorf("the partition reads as %v, want %v", got.Reading.Content, tc.reading)
			}
			if !got.OnlyRejectedFor(tc.want...) || len(got.Rejections) != len(tc.want) {
				t.Errorf("rejected it for %v, want exactly %v", got.Rejections, tc.want)
			}
		})
	}
}

// A partition that is mounted is still refused before anything is opened,
// exactly as a mounted disk is.
func TestCandidatesDoesNotReadAMountedPartition(t *testing.T) {
	in := inspectorOver(t, map[string]sparse{}, nil)
	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}
	got := found(t, cands, "nvme1n1p2")
	if got.Reading.Content != ContentUnknown {
		t.Errorf("a mounted partition was read: %v", got.Reading)
	}
	if got.RejectedFor(ReasonUnreadable) {
		t.Errorf("a mounted partition was opened: %v", got.Rejections)
	}
}

// rejection is the ground a candidate was refused on, or a failure when it was
// not refused on it.
func rejection(t *testing.T, c Candidate, reason Reason) Rejection {
	t.Helper()
	for _, r := range c.Rejections {
		if r.Reason == reason {
			return r
		}
	}
	t.Fatalf("%s was not rejected for %s: %v", c.Name, reason, c.Rejections)
	return Rejection{}
}

// heldBy is a kernel that refuses the named devices and hands over the rest.
func heldBy(paths ...string) ExclusiveOpener {
	return func(path string) error {
		for _, held := range paths {
			if path == held {
				return ErrDeviceBusy
			}
		}
		return nil
	}
}

func TestCandidatesNamesTheArrayARAIDMemberBelongsTo(t *testing.T) {
	// GKE assembles a node pool's local SSDs into an MD RAID array for ephemeral
	// storage, and a reviewer told the disk is "held by [md0]" and "busy for no
	// reason" is left to work out what md0 is. The reason names the array and
	// its level, and the kernel's refusal is explained by it.
	root := raidHost().write(t)
	in := Inspector{
		Config:    ScanConfig{SysfsRoot: root, ProcRoot: root},
		Prober:    NewProberWithOpener(contentOpener(nil), WithRegionSize(MinRegionSize)),
		Exclusive: heldBy("/dev/nvme0n1", "/dev/nvme0n2", "/dev/md0"),
	}

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	for _, name := range []string{"nvme0n1", "nvme0n2"} {
		member := found(t, cands, name)
		if member.Available() {
			t.Fatalf("%s, a member of md0, was handed over", name)
		}
		got := rejection(t, member, ReasonRAIDMember)
		for _, want := range []string{"md0", "raid0"} {
			if !strings.Contains(got.Detail, want) {
				t.Errorf("%s: the RAID member detail %q does not name %s", name, got.Detail, want)
			}
		}
		if member.RejectedFor(ReasonStacked) {
			t.Errorf("%s was also rejected as Stacked, which says the same thing without the array: %v",
				name, member.Rejections)
		}
		busy := rejection(t, member, ReasonBusy)
		if !strings.Contains(busy.Detail, "md0") {
			t.Errorf("%s: the Busy detail %q does not say that md0 is why", name, busy.Detail)
		}
	}
}

func TestCandidatesExplainsAKernelRefusalItCanAccountFor(t *testing.T) {
	// The kernel gives no reason for refusing an exclusive open, but when a
	// readable source already found who holds the device, that is the reason,
	// and "gives no reason" would be telling the reader to look for one.
	in := inspectorOver(t, nil, heldBy("/dev/sda", "/dev/nvme1n1", "/dev/vda"))

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	for _, tc := range []struct{ name, want string }{
		{"sda", "dm-0"},        // an LVM volume group holds it
		{"nvme1n1", "mounted"}, // its partitions carry the root filesystem
		{"vda", "swap"},        // it is swap
	} {
		got := rejection(t, found(t, cands, tc.name), ReasonBusy)
		if !strings.Contains(got.Detail, tc.want) {
			t.Errorf("%s: the Busy detail %q does not mention %s", tc.name, got.Detail, tc.want)
		}
		if strings.Contains(got.Detail, "gives no reason") {
			t.Errorf("%s: the Busy detail %q claims there is no reason when one was found", tc.name, got.Detail)
		}
	}

	// A device held by a device-mapper node is still Stacked: the array reason
	// is for arrays.
	if sda := found(t, cands, "sda"); sda.RejectedFor(ReasonRAIDMember) {
		t.Errorf("sda, held by a device-mapper node, was rejected as a RAID member: %v", sda.Rejections)
	}
}

func TestCandidatesHandsOverAnIdleMDArray(t *testing.T) {
	// An administrator who built a RAID array out of a worker's local disks
	// means the array to be the device. Nothing is mounted from it and nothing
	// holds it, so it is read like a disk and handed over; its members are
	// not, because they are the array's.
	root := idleRaidHost().write(t)
	in := Inspector{
		Config:    ScanConfig{SysfsRoot: root, ProcRoot: root},
		Prober:    NewProberWithOpener(contentOpener(map[string]sparse{"md0": blank(1572335616 * 512)}), WithRegionSize(MinRegionSize)),
		Exclusive: heldBy("/dev/nvme0n1", "/dev/nvme0n2"),
	}

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	array := found(t, cands, "md0")
	if !array.Available() {
		t.Fatalf("an idle RAID array was refused: %v", array.Rejections)
	}
	if array.Reading.Content != ContentBlank {
		t.Errorf("the array was handed over without being read: content %s", array.Reading.Content)
	}
	if got := array.Members; !slices.Equal(got, []string{"nvme0n1", "nvme0n2"}) {
		t.Errorf("the array's members are %v, want its two namespaces", got)
	}
	for _, name := range []string{"nvme0n1", "nvme0n2"} {
		if member := found(t, cands, name); member.Available() || !member.RejectedFor(ReasonRAIDMember) {
			t.Errorf("%s, a member of the array that was handed over, was not refused as its member: %v",
				name, member.Rejections)
		}
	}
}

func TestCandidatesHandsOverAnIdleMultipathMapAndRefusesItsPaths(t *testing.T) {
	// A LUN of a storage array arrives over several paths, and multipathd joins
	// them into one device-mapper map. The map is the device: the paths are
	// held by it, and the map itself is idle.
	root := lunHost().write(t)
	in := Inspector{
		Config: ScanConfig{SysfsRoot: root, ProcRoot: root},
		Prober: NewProberWithOpener(contentOpener(map[string]sparse{"dm-0": blank(4294967296 * 512)}),
			WithRegionSize(MinRegionSize)),
		Exclusive: heldBy("/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/dm-1"),
	}

	cands, err := in.Candidates(context.Background())
	if err != nil {
		t.Fatalf("collect the candidates: %v", err)
	}

	lun := found(t, cands, "dm-0")
	if !lun.Available() {
		t.Fatalf("an idle multipath map was refused: %v", lun.Rejections)
	}
	if lun.DMSubsystem != DMSubsystemMultipath {
		t.Errorf("the map's subsystem is %q, want %q", lun.DMSubsystem, DMSubsystemMultipath)
	}
	if got := lun.Members; !slices.Equal(got, []string{"sda", "sdb"}) {
		t.Errorf("the map's members are %v, want its two paths", got)
	}
	for _, name := range []string{"sda", "sdb"} {
		path := found(t, cands, name)
		if path.Available() {
			t.Errorf("%s, a path of the map, was handed over beside it", name)
		}
		if got := rejection(t, path, ReasonStacked); !strings.Contains(got.Detail, "dm-0") {
			t.Errorf("%s: the Stacked detail %q does not name the map", name, got.Detail)
		}
	}

	// The logical volume is a device-mapper node too, and is not a device
	// anybody hands to a cluster: the volume group is what owns the bytes.
	volume := found(t, cands, "dm-1")
	if volume.Available() {
		t.Fatal("an LVM logical volume was handed over")
	}
	if got := rejection(t, volume, ReasonNotAWholeDisk); !strings.Contains(got.Detail, "LVM") {
		t.Errorf("the logical volume's detail %q does not say it is LVM's", got.Detail)
	}
}

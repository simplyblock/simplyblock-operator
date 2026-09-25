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

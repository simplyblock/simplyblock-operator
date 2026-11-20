// Decoding the LBA formats from Identify Namespace buffers built to the layout
// the kernel's struct nvme_id_ns gives, and listing a controller's namespaces
// from a sysfs tree.

package nvme

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// lbaf is one descriptor as a test writes it.
type lbaf struct {
	ms    uint16
	lbads uint8
	rp    uint8
}

// identifyNamespaceBuf lays out an Identify Namespace structure with nlbaf (0's
// based), flbas, and the descriptors at their offsets.
func identifyNamespaceBuf(nlbaf, flbas uint8, formats ...lbaf) []byte {
	buf := make([]byte, identifyNamespaceLen)
	buf[nlbafOffset] = nlbaf
	buf[flbasOffset] = flbas
	for i, f := range formats {
		off := lbafOffset + i*lbafEntryLen
		binary.LittleEndian.PutUint16(buf[off:], f.ms)
		buf[off+2] = f.lbads
		buf[off+3] = f.rp
	}
	return buf
}

func TestLBAFormatsOfAnNVMe1Controller(t *testing.T) {
	// NVMe 1.x: at most 16 formats, FLBAS bits 6:5 reserved. The reserved
	// half of the table carries a stray value the decoder must not reach,
	// because NLBAF bounds it.
	buf := identifyNamespaceBuf(1, 0x01,
		lbaf{ms: 0, lbads: 9, rp: 2},
		lbaf{ms: 0, lbads: 12, rp: 0},
	)
	buf[lbafOffset+20*lbafEntryLen+2] = 12

	got := namespaceFormatsFromIdentify(buf, 1)

	want := []LBAFormat{
		{Index: 0, DataSize: 512, RelativePerformance: PerformanceGood},
		{Index: 1, DataSize: 4096, RelativePerformance: PerformanceBest},
	}
	if !slices.Equal(got.Formats, want) {
		t.Errorf("formats = %+v, want %+v", got.Formats, want)
	}
	if got.InUse != 1 {
		t.Errorf("in use = %d, want 1", got.InUse)
	}
	if current, ok := got.Current(); !ok || current.DataSize != 4096 {
		t.Errorf("current = %+v, %v, want the 4096-byte format", current, ok)
	}
}

func TestLBAFormatsBeyondSixteenTakeTheHighIndexBits(t *testing.T) {
	// NVMe 2.0: FLBAS bits 6:5 are the format index's bits 5:4, so 0x21 is
	// format 17, and bit 4 is the extended-metadata flag.
	formats := make([]lbaf, 20)
	for i := range formats {
		formats[i] = lbaf{ms: uint16(i), lbads: 9, rp: 1}
	}
	formats[17] = lbaf{ms: 64, lbads: 12, rp: 0}

	got := namespaceFormatsFromIdentify(identifyNamespaceBuf(19, 0x31, formats...), 3)

	if len(got.Formats) != 20 {
		t.Fatalf("decoded %d formats, want 20", len(got.Formats))
	}
	if got.InUse != 17 {
		t.Errorf("in use = %d, want 17", got.InUse)
	}
	if !got.ExtendedMetadata {
		t.Error("FLBAS bit 4 is set and extended metadata reads false")
	}
	current, ok := got.Current()
	if !ok || current != (LBAFormat{Index: 17, DataSize: 4096, MetadataSize: 64, RelativePerformance: PerformanceBest}) {
		t.Errorf("current = %+v, %v, want format 17: 4096+64, best", current, ok)
	}
}

func TestUnusedLBAFormatEntriesAreOmitted(t *testing.T) {
	got := namespaceFormatsFromIdentify(identifyNamespaceBuf(2, 0x00,
		lbaf{lbads: 9},
		lbaf{},                    // LBADS 0: the controller left it unused
		lbaf{lbads: 12, rp: 0xff}, // reserved bits above RP set
	), 1)

	indexes := make([]int, 0, len(got.Formats))
	for _, f := range got.Formats {
		indexes = append(indexes, f.Index)
	}
	if !slices.Equal(indexes, []int{0, 2}) {
		t.Errorf("format indexes = %v, want [0 2]", indexes)
	}
	if len(got.Formats) == 2 && got.Formats[1].RelativePerformance != PerformanceDegraded {
		t.Errorf("RP of 0xff = %v, want degraded (bits 1:0)", got.Formats[1].RelativePerformance)
	}
}

func TestTheControllersCommonFormatsHaveNoneInUse(t *testing.T) {
	// A broadcast Identify zero-fills FLBAS, which would otherwise read as
	// format 0 being in use.
	got := namespaceFormatsFromIdentify(identifyNamespaceBuf(1, 0x10,
		lbaf{lbads: 9}, lbaf{lbads: 12},
	), NSIDAll)

	if got.NSID != NSIDAll || got.InUse != -1 || got.ExtendedMetadata {
		t.Errorf("got NSID %#x, in use %d, extended %v; want NSIDAll, -1, false", got.NSID, got.InUse, got.ExtendedMetadata)
	}
	if len(got.Formats) != 2 {
		t.Errorf("decoded %d formats, want 2", len(got.Formats))
	}
	if _, ok := got.Current(); ok {
		t.Error("the common formats report a current one")
	}
}

func TestAShortIdentifyNamespaceBufferYieldsNoFormats(t *testing.T) {
	got := namespaceFormatsFromIdentify(make([]byte, 100), 1)
	if len(got.Formats) != 0 || got.InUse != -1 {
		t.Errorf("short buffer decoded to %+v", got)
	}
}

func TestReadNamespaceFormatsAsksTheControllerForTheNamespace(t *testing.T) {
	prev, prevCtrl := identifyNS, identifyCtrl
	t.Cleanup(func() { identifyNS, identifyCtrl = prev, prevCtrl })
	identifyCtrl = func(string) ([]byte, error) { return identifyControllerBuf(0), nil }

	var asked NamespaceID
	identifyNS = func(devicePath string, nsid NamespaceID) ([]byte, error) {
		asked = nsid
		if devicePath != "/dev/nvme4" {
			return nil, errors.New("wrong device " + devicePath)
		}
		return identifyNamespaceBuf(0, 0, lbaf{lbads: 12}), nil
	}

	got, err := ReadNamespaceFormats("/dev/nvme4", 7)
	if err != nil {
		t.Fatal(err)
	}
	if asked != 7 || got.NSID != 7 || len(got.Formats) != 1 {
		t.Errorf("asked for %d, got %+v", asked, got)
	}
}

func TestControllerNamespaceIDsReadsTheNSIDAttributeOfEveryNamespace(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A private namespace whose name says n1 but whose NSID is 5, a multipath
	// leg, the leg's twin on the same NSID, and the generic character device,
	// which is not a namespace of its own.
	write("nvme0n1/nsid", "5")
	write("nvme0c0n2/nsid", "2")
	write("nvme0c3n2/nsid", "2")
	write("ng0n1/nsid", "5")
	write("state", "live")

	got, err := ControllerNamespaceIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []NamespaceID{2, 5}) {
		t.Errorf("namespaces = %v, want [2 5]", got)
	}

	none, err := ControllerNamespaceIDs(filepath.Join(dir, "gone"))
	if err != nil || len(none) != 0 {
		t.Errorf("a controller that vanished = %v, %v; want none and no error", none, err)
	}
}

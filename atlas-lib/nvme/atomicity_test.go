// Decoding the power-fail atomicity fields from Identify buffers laid out the
// way the kernel's struct nvme_id_ns and struct nvme_id_ctrl give them, and
// what a decoded reading promises about one write.

package nvme

import (
	"encoding/binary"
	"os"
	"slices"
	"testing"
)

// withNamespaceAtomicity sets NSFEAT bit 1 and the three namespace fields, all
// 0's based as the controller writes them.
func withNamespaceAtomicity(buf []byte, nawupf, nabspf, nabo uint16) []byte {
	buf[nsfeatOffset] |= nsfeatAtomics
	binary.LittleEndian.PutUint16(buf[nawupfOffset:], nawupf)
	binary.LittleEndian.PutUint16(buf[nabspfOffset:], nabspf)
	binary.LittleEndian.PutUint16(buf[naboOffset:], nabo)
	return buf
}

func identifyControllerBuf(awupf uint16) []byte {
	buf := make([]byte, identifyControllerLen)
	binary.LittleEndian.PutUint16(buf[awupfOffset:], awupf)
	return buf
}

func TestTheNamespaceAtomicityFieldsAreDecoded(t *testing.T) {
	buf := withNamespaceAtomicity(identifyNamespaceBuf(0, 0, lbaf{lbads: 9}), 7, 15, 0)

	got := namespaceFormatsFromIdentify(buf, 1).PowerFailAtomicity

	want := PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 8, BoundaryBlocks: 16}
	if got == nil || *got != want {
		t.Errorf("atomicity = %+v, want %+v", got, want)
	}
}

// Without NSFEAT bit 1 the namespace states nothing of its own, and whatever
// sits in NAWUPF is not a value to read.
func TestANamespaceWithoutItsOwnAtomicityLeavesItToTheController(t *testing.T) {
	buf := identifyNamespaceBuf(0, 0, lbaf{lbads: 9})
	binary.LittleEndian.PutUint16(buf[nawupfOffset:], 7)

	got := namespaceFormatsFromIdentify(buf, 1).PowerFailAtomicity

	if got == nil || got.NamespaceSpecific || got.NamespaceBlocks != 0 {
		t.Errorf("atomicity = %+v, want nothing namespace-specific", got)
	}
}

// The broadcast namespace describes no formatted namespace, so it carries no
// atomicity either.
func TestTheCommonFormatsCarryNoAtomicity(t *testing.T) {
	buf := withNamespaceAtomicity(identifyNamespaceBuf(0, 0, lbaf{lbads: 9}), 7, 0, 0)

	if got := namespaceFormatsFromIdentify(buf, NSIDAll).PowerFailAtomicity; got != nil {
		t.Errorf("atomicity = %+v for NSIDAll, want none", got)
	}
}

func TestAWUPFIsDecodedOneBased(t *testing.T) {
	if got := awupfFromIdentify(identifyControllerBuf(7)); got != 8 {
		t.Errorf("AWUPF = %d blocks, want 8", got)
	}
	if got := awupfFromIdentify(make([]byte, 10)); got != 0 {
		t.Errorf("a short buffer decoded to %d blocks, want 0", got)
	}
}

// Real Identify dumps from one Samsung PM983 (MZQLB1T9HAJR-00007, firmware
// EDA5702Q) on a dev-ocp worker, read at each of its two formats, with the
// serial number and NGUID blanked. Kernel 6.12 reported
// atomic_write_unit_max_bytes 4096 at both, and a 4096-byte boundary at 512.
//
// The drive states every unit in blocks of the format in use, so the numbers
// change with the format and the size they describe does not: one 4K block at
// 4K, and eight 512-byte blocks with a boundary every eight at 512. NAWUN is
// the normal-operation unit and says nothing about a power failure. Which is
// also why a reading taken at one format says nothing about another.
func TestTheAtomicityOfARealSamsungPM983(t *testing.T) {
	formats := []LBAFormat{
		{Index: 0, DataSize: 512, RelativePerformance: PerformanceBest},
		{Index: 1, DataSize: 4096, RelativePerformance: PerformanceBest},
	}
	for _, tc := range []struct {
		format    string
		inUse     int
		blockSize uint32
		want      PowerFailAtomicity
	}{
		{"4k", 1, 4096, PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 1, ControllerBlocks: 1}},
		{"512", 0, 512, PowerFailAtomicity{
			NamespaceSpecific: true, NamespaceBlocks: 8, ControllerBlocks: 8, BoundaryBlocks: 8}},
	} {
		t.Run(tc.format, func(t *testing.T) {
			ns, err := os.ReadFile("testdata/identify_ns_samsung_pm983_" + tc.format + ".bin")
			if err != nil {
				t.Fatal(err)
			}
			ctrl, err := os.ReadFile("testdata/identify_ctrl_samsung_pm983_" + tc.format + ".bin")
			if err != nil {
				t.Fatal(err)
			}

			read := namespaceFormatsFromIdentify(ns, 1)
			if !slices.Equal(read.Formats, formats) || read.InUse != tc.inUse {
				t.Fatalf("formats = %+v in use %d, want %+v in use %d", read.Formats, read.InUse, formats, tc.inUse)
			}
			got := *read.PowerFailAtomicity
			got.ControllerBlocks = awupfFromIdentify(ctrl)
			if got != tc.want {
				t.Fatalf("atomicity = %+v, want %+v", got, tc.want)
			}
			if !got.AtomicAt(4096, tc.blockSize) {
				t.Errorf("an aligned 4K write at %s is not atomic, and the kernel says it is", tc.format)
			}
			if got.AtomicAt(8192, tc.blockSize) {
				t.Errorf("an aligned 8K write at %s is atomic, and the drive promises 4K", tc.format)
			}
		})
	}
}

func TestAtomicAt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		atomicity PowerFailAtomicity
		blockSize uint32
		want      bool
	}{
		{"the controller covers eight 512-byte blocks", PowerFailAtomicity{ControllerBlocks: 8}, 512, true},
		{"the controller covers seven", PowerFailAtomicity{ControllerBlocks: 7}, 512, false},
		{"the controller covers one", PowerFailAtomicity{ControllerBlocks: 1}, 512, false},
		{"one 4K block is always whole", PowerFailAtomicity{ControllerBlocks: 1}, 4096, true},
		{"the namespace overrides a larger controller value",
			PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 1, ControllerBlocks: 8}, 512, false},
		{"the namespace overrides a smaller controller value",
			PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 8, ControllerBlocks: 1}, 512, true},
		{"a boundary every 8 KiB",
			PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 16, BoundaryBlocks: 16}, 512, true},
		{"a boundary every 3 KiB splits an aligned 4K write",
			PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 16, BoundaryBlocks: 6}, 512, false},
		{"boundaries offset by one block split an aligned 4K write",
			PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 16, BoundaryBlocks: 16, BoundaryOffset: 1}, 512, false},
		{"no reading at all", PowerFailAtomicity{}, 512, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.atomicity.AtomicAt(4096, tc.blockSize); got != tc.want {
				t.Errorf("AtomicAt(4096, %d) = %v, want %v", tc.blockSize, got, tc.want)
			}
		})
	}
}

func TestReadNamespaceFormatsAddsTheControllersAtomicity(t *testing.T) {
	prevNS, prevCtrl := identifyNS, identifyCtrl
	t.Cleanup(func() { identifyNS, identifyCtrl = prevNS, prevCtrl })
	identifyNS = func(string, NamespaceID) ([]byte, error) {
		return identifyNamespaceBuf(0, 0, lbaf{lbads: 9}), nil
	}
	identifyCtrl = func(devicePath string) ([]byte, error) {
		return identifyControllerBuf(7), nil
	}

	got, err := ReadNamespaceFormats("/dev/nvme4", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.PowerFailAtomicity == nil || got.PowerFailAtomicity.ControllerBlocks != 8 {
		t.Errorf("atomicity = %+v, want the controller's 8 blocks", got.PowerFailAtomicity)
	}
}

// Real Identify dumps from an EKS node (i4i.large, 2026-10-27), serial numbers,
// NGUID, and EUI-64 blanked. All three AWS devices were formatted at 512 bytes.
//
// An EBS volume (gp3 and io2 answered alike) states its own unit, NAWUPF 31:
// 16 KiB atomic within 16 KiB boundaries, which is what AWS documents and what
// the 6.18 kernel reported. The i4i instance store states nothing of its own,
// and its controller's AWUPF is the one-block floor: 512 bytes, so a 4K write
// is not promised whole there, though AWS documents torn-write prevention for
// some other I-family instance stores.
func TestTheAtomicityOfRealAWSDevices(t *testing.T) {
	for _, tc := range []struct {
		device string
		want   PowerFailAtomicity
		atomic bool
	}{
		{"aws_ebs_gp3", PowerFailAtomicity{
			NamespaceSpecific: true, NamespaceBlocks: 32, ControllerBlocks: 1, BoundaryBlocks: 32}, true},
		{"aws_i4i_instance_store", PowerFailAtomicity{ControllerBlocks: 1}, false},
	} {
		t.Run(tc.device, func(t *testing.T) {
			ns, err := os.ReadFile("testdata/identify_ns_" + tc.device + ".bin")
			if err != nil {
				t.Fatal(err)
			}
			ctrl, err := os.ReadFile("testdata/identify_ctrl_" + tc.device + ".bin")
			if err != nil {
				t.Fatal(err)
			}

			read := namespaceFormatsFromIdentify(ns, 1)
			if len(read.Formats) != 1 || read.Formats[0].DataSize != 512 || read.InUse != 0 {
				t.Fatalf("formats = %+v in use %d, want one 512-byte format in use", read.Formats, read.InUse)
			}
			got := *read.PowerFailAtomicity
			got.ControllerBlocks = awupfFromIdentify(ctrl)
			if got != tc.want {
				t.Fatalf("atomicity = %+v, want %+v", got, tc.want)
			}
			if got.AtomicAt(4096, 512) != tc.atomic {
				t.Errorf("AtomicAt(4096, 512) = %v, want %v", !tc.atomic, tc.atomic)
			}
		})
	}
}

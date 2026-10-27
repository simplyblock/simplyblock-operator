// Decoding the power-fail atomicity fields from Identify buffers laid out the
// way the kernel's struct nvme_id_ns and struct nvme_id_ctrl give them, and
// what a decoded reading promises about one write.

package nvme

import (
	"encoding/binary"
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

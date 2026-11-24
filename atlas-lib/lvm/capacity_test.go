// Tests for the capacity a volume group's devices need.
package lvm

import "testing"

// The estimate covers what VDO was measured to keep (4.02GiB of a 12GiB pool
// on the runner kernel) with room, and grows with the logical size for the part
// of VDO's metadata that does.
func TestVDOOverheadBytesCoversWhatWasMeasured(t *testing.T) {
	const measured = int64(4315938816)
	if got := VDOOverheadBytes(6 << 30); got < measured {
		t.Errorf("VDOOverheadBytes(6GiB) = %d, below the %d measured", got, measured)
	}
	if small, large := VDOOverheadBytes(1<<30), VDOOverheadBytes(1<<40); large <= small {
		t.Errorf("VDOOverheadBytes does not grow with the logical size: %d at 1GiB, %d at 1TiB", small, large)
	}
}

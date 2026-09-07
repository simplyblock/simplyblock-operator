// The namespace-to-block-device mapping, and the unit conversion that is the
// reason it exists.

package nvme

import "testing"

func TestNamespaceBlockDevice(t *testing.T) {
	ns := Namespace{
		Name:             "nvme0n1",
		DevicePath:       "/dev/nvme0n1",
		Dev:              "259:1",
		LogicalBlockSize: 512,
		Capacity:         2048, // 512-byte sectors
		ReadOnly:         true,
	}

	got := ns.BlockDevice()
	if got.Path != "/dev/nvme0n1" || got.Name != "nvme0n1" {
		t.Errorf("path %q name %q", got.Path, got.Name)
	}
	if got.Major != 259 || got.Minor != 1 {
		t.Errorf("device numbers = %d:%d, want 259:1", got.Major, got.Minor)
	}
	if got.SizeBytes != 2048*512 {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, 2048*512)
	}
	if !got.ReadOnly {
		t.Error("ReadOnly was not carried across")
	}
}

// Capacity is in 512-byte sectors whatever the logical block size is, so a 4Kn
// namespace is the case that catches a caller multiplying by the wrong unit.
func TestNamespaceBlockDeviceSizeIsSectorsNotBlocks(t *testing.T) {
	ns := Namespace{
		Name:             "nvme0n1",
		LogicalBlockSize: 4096,
		Capacity:         2048, // still 512-byte sectors: one mebibyte
	}

	got := ns.BlockDevice()
	if want := uint64(1 << 20); got.SizeBytes != want {
		t.Fatalf("SizeBytes = %d, want %d: capacity is in 512-byte sectors, not in logical blocks",
			got.SizeBytes, want)
	}
	if got.LogicalBlockSize != 4096 {
		t.Errorf("LogicalBlockSize = %d, want 4096", got.LogicalBlockSize)
	}
}

func TestParseDevNumbers(t *testing.T) {
	cases := []struct {
		in               string
		wantMaj, wantMin uint32
	}{
		{"259:1", 259, 1},
		{"8:0", 8, 0},
		{"", 0, 0},
		{"nonsense", 0, 0},
		{"259:", 0, 0},
		{":1", 0, 0},
	}
	for _, tc := range cases {
		maj, min := parseDevNumbers(tc.in)
		if maj != tc.wantMaj || min != tc.wantMin {
			t.Errorf("parseDevNumbers(%q) = %d:%d, want %d:%d", tc.in, maj, min, tc.wantMaj, tc.wantMin)
		}
	}
}

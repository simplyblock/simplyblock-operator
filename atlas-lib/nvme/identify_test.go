package nvme

import (
	"os"
	"testing"
)

// Real Identify Controller dumps captured from simplyblock (SPDK NVMe-oF)
// controllers: a plain volume (MNAN 1) and a namespaced one (MNAN 32).
func TestMNANFromIdentify(t *testing.T) {
	for _, tc := range []struct {
		file string
		want uint32
	}{
		{"testdata/identify_ctrl_mnan1.bin", 1},
		{"testdata/identify_ctrl_mnan32.bin", 32},
	} {
		buf, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		if len(buf) != identifyControllerLen {
			t.Fatalf("%s: %d bytes, want %d", tc.file, len(buf), identifyControllerLen)
		}
		if got := mnanFromIdentify(buf); got != tc.want {
			t.Errorf("%s: mnanFromIdentify = %d, want %d", tc.file, got, tc.want)
		}
	}

	if got := mnanFromIdentify(make([]byte, 100)); got != 0 {
		t.Errorf("mnanFromIdentify(short) = %d, want 0", got)
	}
}

package nvme

import (
	"os"
	"strings"
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

// A controller that rejects an Identify, such as one asked for the broadcast
// namespace without namespace management, completes it with a status and no
// errno. That is a failure, not an answer of zeros.
func TestARejectedAdminCommandIsAnError(t *testing.T) {
	if err := adminStatus(0); err != nil {
		t.Errorf("adminStatus(0) = %v, want nil", err)
	}
	// SCT 0 (generic), SC 0x0B: Invalid Namespace or Format.
	err := adminStatus(0x000B)
	if err == nil {
		t.Fatal("adminStatus(0x0B) = nil, want the controller's rejection")
	}
	if !strings.Contains(err.Error(), "0xb") {
		t.Errorf("adminStatus(0x0B) = %q, want the status code in it", err)
	}
	// SCT 1 (command specific), SC 0x02.
	if err := adminStatus(0x0102); err == nil || !strings.Contains(err.Error(), "type 1") {
		t.Errorf("adminStatus(0x0102) = %v, want status code type 1 named", err)
	}
}

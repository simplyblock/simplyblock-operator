// Which controllers' power-fail atomicity statements are not to be believed,
// matched the way sysfs spells PCI identifiers.

package nvme

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestAtomicityDisclaimed(t *testing.T) {
	for _, tc := range []struct {
		name            string
		vendor, product string
		disclaimed      bool
	}{
		{"Google Cloud local SSD, as sysfs spells it", "0x1ae0", "0x001f", true},
		{"the same, without the prefix and in capitals", "1AE0", "001F", true},
		{"a Samsung PM983", "0x144d", "0xa808", false},
		{"another Google device", "0x1ae0", "0x0042", false},
		{"nothing read", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, disclaimed := AtomicityDisclaimed(tc.vendor, tc.product)
			if disclaimed != tc.disclaimed {
				t.Fatalf("AtomicityDisclaimed(%q, %q) = %v, want %v", tc.vendor, tc.product, disclaimed, tc.disclaimed)
			}
			if disclaimed && !strings.Contains(reason, "Google") {
				t.Errorf("the reason %q does not say whose device it is", reason)
			}
		})
	}
}

// Real Identify dumps from a GKE node's Local SSD (n2-standard-2, 2026-10-27),
// NGUID blanked. It offers one format, 4K, and reports the one-block floor for
// its power-fail unit, which decodes as a 4K atomic write: the statement the
// deny-list exists to overrule. Its Identify Controller VID is the PCI vendor
// the list matches on.
func TestARealGoogleLocalSSDReportsOnlyTheFloor(t *testing.T) {
	ns, err := os.ReadFile("testdata/identify_ns_gcp_local_ssd.bin")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := os.ReadFile("testdata/identify_ctrl_gcp_local_ssd.bin")
	if err != nil {
		t.Fatal(err)
	}

	read := namespaceFormatsFromIdentify(ns, 1)
	if len(read.Formats) != 1 || read.Formats[0].DataSize != 4096 || read.InUse != 0 {
		t.Fatalf("formats = %+v in use %d, want one 4K format in use", read.Formats, read.InUse)
	}
	got := *read.PowerFailAtomicity
	got.ControllerBlocks = awupfFromIdentify(ctrl)
	if want := (PowerFailAtomicity{NamespaceSpecific: true, NamespaceBlocks: 1, ControllerBlocks: 1}); got != want {
		t.Fatalf("atomicity = %+v, want the one-block floor %+v", got, want)
	}
	if !got.AtomicAt(4096, 4096) {
		t.Error("the floor does not decode as a 4K atomic write on the 4K format")
	}

	vendor := fmt.Sprintf("0x%04x", binary.LittleEndian.Uint16(ctrl[0:2]))
	if _, disclaimed := AtomicityDisclaimed(vendor, "0x001f"); !disclaimed {
		t.Errorf("the controller (VID %s) is not on the list", vendor)
	}
}

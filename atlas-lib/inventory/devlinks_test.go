// The transcript's compatibility with the captures already committed.
//
// devlinks was added to the format after okd-worker.json.gz and
// okd-worker-unbound-nvme.json.gz were taken, and those two are the reason the
// section is additive: a capture is expensive to retake -- it wants the machine
// it came from -- so a format change that invalidated one would cost the
// fixture rather than the format.

package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A transcript captured before the section existed decodes, materializes, and
// reads as a host whose udev made no links. It is not an error and not an empty
// host: every other reader still answers from it.
func TestATranscriptWithoutDevLinksStillMaterializes(t *testing.T) {
	for _, name := range []string{"okd-worker", "okd-worker-unbound-nvme"} {
		t.Run(name, func(t *testing.T) {
			root := hostFixture(t, name)

			// The readers that made the capture worth keeping still answer.
			if _, err := ReadCPU(syntheticHost(root)); err != nil {
				t.Fatalf("ReadCPU against a transcript with no devlinks: %v", err)
			}

			// And the device tree is absent rather than wrong.
			entries, err := os.ReadDir(devRootOf(root))
			if err == nil && len(entries) > 0 {
				t.Errorf("a transcript with no devlinks materialized %d entries under dev/",
					len(entries))
			} else if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read the device root: %v", err)
			}
		})
	}
}

// The section decodes when it is there, which is the other half of the
// contract: an old reader ignores it and a new reader uses it, so one capture
// serves both.
func TestATranscriptWithDevLinksDecodes(t *testing.T) {
	raw := []byte(`{
	  "dirs": ["block"],
	  "files": {"block/sda/size": "1024\n"},
	  "links": {"block/sda": "../devices/pci0000:00/block/sda"},
	  "devlinks": {
	    "disk/by-id/scsi-36000c29": "../../sda",
	    "disk/by-id/scsi-36000c29-part1": "../../sda1",
	    "disk/by-path/pci-0000:00:10.0-scsi-0:0:0:0": "../../sda"
	  }
	}`)

	var host hostTranscript
	if err := json.Unmarshal(raw, &host); err != nil {
		t.Fatalf("decode a transcript carrying devlinks: %v", err)
	}

	if got := len(host.DevLinks); got != 3 {
		t.Fatalf("decoded %d devlinks, want 3", got)
	}
	// A partition carries its own link beside the whole disk's, which is what
	// makes a stable name able to address one at all.
	if target := host.DevLinks[filepath.Join("disk", "by-id", "scsi-36000c29-part1")]; target != "../../sda1" {
		t.Errorf("the partition link resolves to %q, want ../../sda1", target)
	}
	// And the fields the older captures carry are untouched by the addition.
	if len(host.Dirs) != 1 || len(host.Files) != 1 || len(host.Links) != 1 {
		t.Error("adding devlinks changed how the original three sections decode")
	}
}

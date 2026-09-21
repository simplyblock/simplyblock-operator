package nfsexport

import (
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/export"
	csimount "github.com/simplyblock/csi-driver/internal/mount"
)

// Regression: an export was formatted from a feature list written out here, and
// the list was missing what nobody thought to add. mkfs.xfs 6.18 in this image
// defaults parent pointers on, the option lives under -n rather than -m, and a
// host on 5.14 answers:
//
//	XFS (nvme2n1): Superblock has unknown incompatible features (0x80) enabled.
//	XFS (nvme2n1): Filesystem cannot be safely mounted by this kernel.
//
// The block path had already solved this with mkfs.xfs's own config file, which
// pins the whole feature baseline rather than the bits somebody enumerated. An
// export takes the same one, so the two cannot drift and neither has to be
// updated when xfsprogs defaults another feature on.
func TestExportFormatOptionsArePinnedTheSameWayTheBlockPathPinsThem(t *testing.T) {
	options := exportFormatOptions()

	// Equal, not merely similar. Which features the baseline pins is the
	// config file's business, and whether this host has one is the image's;
	// what belongs here is that an export does not answer the question twice.
	if want := csimount.FormatOptions(export.FSType, nil); !slices.Equal(options, want) {
		t.Errorf("export format options = %v, want the block path's %v", options, want)
	}
}

// A feature named here is one somebody has to remember to add to. The config
// file is the whole baseline, so an export must not carry its own list beside
// it.
func TestExportCarriesNoHandWrittenFeatureList(t *testing.T) {
	for _, option := range exportFormatOptions() {
		for _, feature := range []string{"nrext64", "bigtime", "inobtcount", "reflink", "parent", "exchange"} {
			if strings.Contains(option, feature) {
				t.Errorf("export format options name %s by hand (%q); the pinned config covers it",
					feature, option)
			}
		}
	}
}

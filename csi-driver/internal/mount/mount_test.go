// Tests for the mkfs options a filesystem is created with, and for which
// filesystems this driver creates at all. They run without a kernel, a device,
// or a CSI request, which is the point of this package having its own
// boundary.

package mount

import (
	"strings"
	"testing"

	utilexec "k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
)

// scriptedResult is one external command's scripted outcome: its combined
// output and its error, in the order the code under test runs commands.
type scriptedResult struct {
	out string
	err error
}

// scriptedExec builds a FakeExec answering successive commands from script and
// recording every invocation's argv, so a test can assert which commands were
// chosen. Scripting more results than the code consumes is fine. Running more
// commands than scripted panics.
func scriptedExec(script []scriptedResult) (*testingexec.FakeExec, *[][]string) {
	fe := &testingexec.FakeExec{}
	calls := &[][]string{}
	for _, r := range script {
		fe.CommandScript = append(fe.CommandScript, func(cmd string, args ...string) utilexec.Cmd {
			*calls = append(*calls, append([]string{cmd}, args...))
			fc := &testingexec.FakeCmd{
				CombinedOutputScript: []testingexec.FakeAction{
					func() ([]byte, []byte, error) { return []byte(r.out), nil, r.err },
				},
			}
			return testingexec.InitFakeCmd(fc, cmd, args...)
		})
	}
	return fe, calls
}

func TestFormatOptions(t *testing.T) {
	if got := FormatOptions("ext4", nil, false); got != nil {
		t.Errorf("FormatOptions(ext4, vdo=false) = %v, want none", got)
	}

	got := FormatOptions("xfs", map[string]string{"xfs_su": "32k", "xfs_sw": "4"}, false)
	joined := strings.Join(got, " ")
	for _, want := range []string{"su=32k", "sw=4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("FormatOptions(xfs) = %v, missing %s", got, want)
		}
	}
}

// TestFormatOptionsDefaultsTheStripeGeometry. A class that says nothing about
// striping still gets a geometry, because mkfs.xfs otherwise infers one from the
// device and an NVMe-oF namespace reports nothing useful to infer from.
func TestFormatOptionsDefaultsTheStripeGeometry(t *testing.T) {
	joined := strings.Join(FormatOptions("xfs", nil, false), " ")
	for _, want := range []string{"su=" + defaultXFSStripeUnit, "sw=" + defaultXFSStripeWidth} {
		if !strings.Contains(joined, want) {
			t.Errorf("FormatOptions(xfs, nil) = %q, missing %s", joined, want)
		}
	}
}

// TestFormatOptionsSkipsStripeAlignmentWhenAsked. Once a layer such as VDO
// virtualizes and relocates blocks, the filesystem no longer sits directly on
// the erasure-coded device those hints describe, so skipStripeAlignment omits
// them regardless of what the volume context says — the feature options still
// apply, because on-disk feature compatibility has nothing to do with the
// backend's layout.
// mke2fs discards the whole device by default too, confirmed live to cost the
// same order of magnitude as mkfs.xfs's: 12.08s vs. 0.09s formatting a 20G
// VDO volume, "Discarding device blocks" being the whole difference.
func TestFormatOptionsForVDOSkipsDiscardOnExt4Too(t *testing.T) {
	got := FormatOptions("ext4", nil, true)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "nodiscard") {
		t.Errorf("FormatOptions(ext4, vdo=true) = %q, want -E nodiscard to skip mke2fs's discard", joined)
	}
}

func TestFormatOptionsForVDOSkipsStripeAlignmentAndDiscard(t *testing.T) {
	got := FormatOptions("xfs", map[string]string{"xfs_su": "32k", "xfs_sw": "4"}, true)
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "su=") || strings.Contains(joined, "sw=") {
		t.Errorf("FormatOptions(xfs, vdo=true) = %q, want no stripe options at all", joined)
	}
	// -K skips mkfs.xfs's default full-device discard, confirmed live to be the
	// difference between an 11.5s and a 0.13s format on the same 20G VDO
	// volume: VDO's block map processes a discard proportionally to the
	// volume's size, and a freshly created volume has nothing worth discarding.
	if !strings.Contains(joined, "-K") {
		t.Errorf("FormatOptions(xfs, vdo=true) = %q, want -K to skip mkfs.xfs's discard", joined)
	}
	// The feature options still apply unconditionally: whatever
	// xfsFeatureOptions() alone would produce in this environment (it depends
	// on a config file this sandbox does not carry) must still be there ahead
	// of -K, not replaced by it.
	if want := strings.Join(xfsFeatureOptions(), " "); !strings.HasPrefix(joined, want) {
		t.Errorf("FormatOptions(xfs, vdo=true) = %q, want it to start with the feature options %q", joined, want)
	}
}

func TestSupported(t *testing.T) {
	for _, fs := range []string{"ext4", "xfs"} {
		if !Supported(fs) {
			t.Errorf("Supported(%q) = false, want true", fs)
		}
	}
	for _, fs := range []string{"", "btrfs", "zfs", "ntfs"} {
		if Supported(fs) {
			t.Errorf("Supported(%q) = true, want false", fs)
		}
	}
}

// TestFormatOptionsNeedsBothStripeValues. su and sw describe one geometry, so a
// class setting only one of them has described nothing, and mkfs is given the
// defaults rather than half an alignment.
func TestFormatOptionsNeedsBothStripeValues(t *testing.T) {
	for _, half := range []map[string]string{{"xfs_su": "32k"}, {"xfs_sw": "4"}} {
		joined := strings.Join(FormatOptions("xfs", half, false), " ")
		if !strings.Contains(joined, "su="+defaultXFSStripeUnit) {
			t.Errorf("FormatOptions(xfs, %v) = %q, want the default geometry", half, joined)
		}
	}
}

// TestFormatOptionsRejectsAnUnusableStripeWidth. sw multiplies su into the full
// stripe, so a non-positive value would ask mkfs for a stripe of zero width, and
// alignment is dropped rather than guessed.
func TestFormatOptionsRejectsAnUnusableStripeWidth(t *testing.T) {
	for _, sw := range []string{"0", "-1", "many"} {
		joined := strings.Join(FormatOptions("xfs", map[string]string{"xfs_su": "32k", "xfs_sw": sw}, false), " ")
		if strings.Contains(joined, "su=32k") {
			t.Errorf("FormatOptions(xfs, sw=%q) = %q, want no stripe alignment", sw, joined)
		}
	}
}

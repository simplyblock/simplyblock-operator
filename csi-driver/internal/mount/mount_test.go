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
	if got := FormatOptions("ext4", nil); got != nil {
		t.Errorf("FormatOptions(ext4) = %v, want none — only XFS is tuned at mkfs time", got)
	}

	got := FormatOptions("xfs", map[string]string{"xfs_su": "32k", "xfs_sw": "4"})
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
	joined := strings.Join(FormatOptions("xfs", nil), " ")
	for _, want := range []string{"su=" + defaultXFSStripeUnit, "sw=" + defaultXFSStripeWidth} {
		if !strings.Contains(joined, want) {
			t.Errorf("FormatOptions(xfs, nil) = %q, missing %s", joined, want)
		}
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
		joined := strings.Join(FormatOptions("xfs", half), " ")
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
		joined := strings.Join(FormatOptions("xfs", map[string]string{"xfs_su": "32k", "xfs_sw": sw}), " ")
		if strings.Contains(joined, "su=32k") {
			t.Errorf("FormatOptions(xfs, sw=%q) = %q, want no stripe alignment", sw, joined)
		}
	}
}

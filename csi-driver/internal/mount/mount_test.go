// Tests for what a device is read to carry, and for the mkfs and mount options
// that follow from it. They run without a kernel, a device, or a CSI request:
// the point of this package having its own boundary is that the never-format
// contract can be asserted against scripted commands alone.

package mount

import (
	"context"
	"errors"
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

func proberFor(t *testing.T, result scriptedResult) *Mounter {
	t.Helper()
	fe, _ := scriptedExec([]scriptedResult{result})
	return NewWith(nil, fe)
}

func TestProbe(t *testing.T) {
	cases := []struct {
		name      string
		result    scriptedResult
		want      string
		wantError bool
	}{
		{name: "blank device", result: scriptedResult{err: &testingexec.FakeExitError{Status: 2}}, want: ""},
		{name: "already formatted", result: scriptedResult{out: "TYPE=ext4\n"}, want: "ext4"},
		{name: "unreadable device", result: scriptedResult{err: errors.New("blkid: broken pipe")}, wantError: true},
		{name: "partition table, no filesystem", result: scriptedResult{out: "PTTYPE=dos\n"}, wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := proberFor(t, tc.result).Probe(context.Background(), "/dev/nvme9n1")
			if tc.wantError {
				if err == nil {
					t.Fatalf("Probe(%s) = %q, nil, want an error", tc.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe(%s): unexpected error %v", tc.name, err)
			}
			if got != tc.want {
				t.Fatalf("Probe(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestProbeRefusalNamesThePartitionTable. A device carrying a partition table is
// refused rather than formatted, and which table it is decides what an operator
// does next: a GPT disk handed to the driver by mistake is a different problem
// from a stale DOS label on a volume that was reused. The prober knows, since
// blkid reports PTTYPE, so the refusal has to carry it rather than saying only
// that something was there.
func TestProbeRefusalNamesThePartitionTable(t *testing.T) {
	for _, table := range []string{"gpt", "dos"} {
		t.Run(table, func(t *testing.T) {
			_, err := proberFor(t, scriptedResult{out: "PTTYPE=" + table + "\n"}).
				Probe(context.Background(), "/dev/fake-lvol")
			if err == nil {
				t.Fatal("probed a device carrying a partition table without refusing it")
			}
			if !strings.Contains(err.Error(), table) {
				t.Errorf("the refusal does not say which table it found: %v", err)
			}
		})
	}
}

// TestFlagsForXFSCarriesNouuid. Two XFS filesystems with the same UUID cannot be
// mounted on one node, which is exactly what a volume and its clone are, so the
// option is a property of the filesystem rather than of what the volume asked
// for.
func TestFlagsForXFSCarriesNouuid(t *testing.T) {
	if got := FlagsFor("xfs"); len(got) != 1 || got[0] != "nouuid" {
		t.Errorf(`FlagsFor("xfs") = %v, want ["nouuid"]`, got)
	}
	if got := FlagsFor("ext4"); len(got) != 0 {
		t.Errorf(`FlagsFor("ext4") = %v, want none`, got)
	}
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

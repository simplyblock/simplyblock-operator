// Tests for the on-disk format probe: every reading blkid can produce, driven
// through a scripted Runner so no blkid, device, or kernel is needed. The
// classification cases were the CSI driver's before the probe moved here; the
// exit-code-2 case is the load-bearing one.
package blockdev

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// scripted returns a Runner answering every command with the given output,
// exit code, and error, and records the argv it was handed.
func scripted(out string, code int, err error) (Runner, *[]string) {
	var argv []string
	return func(_ context.Context, name string, args ...string) ([]byte, int, error) {
		argv = append([]string{name}, args...)
		return []byte(out), code, err
	}, &argv
}

func TestFormatReadings(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		code      int
		runErr    error
		want      string
		wantError bool
		wantIs    error
	}{
		{name: "formatted device", out: "DEVNAME=/dev/nvme9n1\nTYPE=ext4\n", want: "ext4"},
		{name: "blank device", code: 2, want: ""},
		// Regression: 2026-09-03-blkid-exit2-conflation — a device whose every
		// read fails (all NVMe-oF paths down) makes blkid exit 2 exactly like a
		// blank device does, and the driver formatted a data-bearing volume on
		// that reading. The probe cannot repair blkid's conflation; what it must
		// do is return the "" that forces callers to settle it elsewhere, rather
		// than inventing an error blkid never raised or a type it never named.
		{name: "unreadable device reads as blank", code: 2, out: "", want: ""},
		{name: "probe failed outright", runErr: errors.New("blkid: broken pipe"), wantError: true},
		{name: "blkid exited oddly", code: 4, out: "ambivalent result", wantError: true},
		{name: "partition table, no filesystem", out: "PTTYPE=dos\n", wantIs: ErrPartitionTable},
		{name: "partition table wins over a type", out: "TYPE=ext4\nPTTYPE=gpt\n", wantIs: ErrPartitionTable},
		{name: "malformed output", out: "not-an-export-line\n", wantError: true},
		{name: "no tags at all", out: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, _ := scripted(tc.out, tc.code, tc.runErr)
			got, err := NewBlkidProberWithRunner(run).Format(context.Background(), "/dev/nvme9n1")
			if tc.wantIs != nil {
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("Format = %q, %v, want errors.Is(err, %v)", got, err, tc.wantIs)
				}
				return
			}
			if tc.wantError {
				if err == nil {
					t.Fatalf("Format = %q, nil, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Format: unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("Format = %q, want %q", got, tc.want)
			}
		})
	}
}

// The argv is part of the contract: the driver's staging decision and any
// harness reproducing it must observe the same probe, so a drive-by "helpful"
// flag change is a behavior change.
func TestFormatProbeArgv(t *testing.T) {
	run, argv := scripted("TYPE=xfs\n", 0, nil)
	if _, err := NewBlkidProberWithRunner(run).Format(context.Background(), "/dev/loop7"); err != nil {
		t.Fatalf("Format: %v", err)
	}
	want := "blkid -p -s TYPE -s PTTYPE -o export /dev/loop7"
	if got := strings.Join(*argv, " "); got != want {
		t.Fatalf("probe ran %q, want %q", got, want)
	}
}

// A canceled context must not surface as a "blank device": the default runner
// reports it as an error, and Format passes that through.
func TestFormatContextError(t *testing.T) {
	run := Runner(func(_ context.Context, _ string, _ ...string) ([]byte, int, error) {
		return nil, 0, fmt.Errorf("timed out running blkid")
	})
	if got, err := NewBlkidProberWithRunner(run).Format(context.Background(), "/dev/nvme9n1"); err == nil {
		t.Fatalf("Format = %q, nil, want the runner's error", got)
	}
}

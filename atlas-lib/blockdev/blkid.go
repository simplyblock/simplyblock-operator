// The blkid-backed probe, which asks a tool what it recognized.
//
// It is retained as the shadow of the content reading in content.go, per the
// migration in operator/docs/designs/design-device-content-detection.md section
// 13: both run, the reading decides, and a disagreement is counted. It decides
// nothing on its own once that reading lands, and it stays afterward because an
// operator on a host reaches for the same tool.
package blockdev

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes one external command and hands back its combined output, the
// exit code it exited with, and an error only when the command did not run to
// an exit at all. The exit code is data rather than failure: blkid answers
// "nothing found" with exit code 2, so a runner that folded exit codes into its
// error would erase the one distinction Format is built on. A Runner also
// decides where the command executes — the local host by default, or a remote
// shell when a harness probes another machine's devices.
type Runner func(ctx context.Context, name string, args ...string) (output []byte, exitCode int, err error)

// execRunner is the default Runner: os/exec on the local host, respecting ctx's
// deadline rather than imposing one of its own.
func execRunner(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	//nolint:gosec // fixed probe binary, structured args
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, 0, fmt.Errorf("timed out running %s", name)
	}
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return out, exit.ExitCode(), nil
		}
		return out, 0, err
	}
	return out, 0, nil
}

// BlkidProber reads the on-disk format of block devices through a Runner.
type BlkidProber struct {
	run Runner
}

// NewBlkidProber returns a BlkidProber that runs blkid on the local host.
func NewBlkidProber() *BlkidProber { return NewBlkidProberWithRunner(execRunner) }

// NewBlkidProberWithRunner returns a BlkidProber that runs blkid through run, which is
// how a test supplies scripted answers and how a harness probes a device on
// another machine.
func NewBlkidProberWithRunner(run Runner) *BlkidProber { return &BlkidProber{run: run} }

// ErrPartitionTable reports a device carrying a partition table rather than a
// filesystem: something is on it, but not something mountable, and formatting
// it would destroy whatever it is.
var ErrPartitionTable = errors.New("device carries a partition table rather than a filesystem")

// Format reports the filesystem on device: the type blkid names, or "" when
// blkid positively reported nothing.
//
// The "" reading is weaker than it looks, and every caller must treat it so.
// blkid answers "this device carries no filesystem" and "I could not read this
// device" with the same exit code 2, no output, and nothing on stderr; its own
// manual folds "impossible to gather any information about the device content"
// into the not-found exit. A device whose every read fails is byte-identical
// to a blank one from here — which is how the 2026-09-03 incident formatted a
// volume whose NVMe-oF paths were all down. So "" means "no filesystem was
// seen," never "there is no filesystem," and a decision as irreversible as
// formatting has to settle "" against another source of truth.
//
// Every other reading is an error: a probe that failed outright, malformed
// blkid output, and a partition table (ErrPartitionTable) all leave open
// whether the device holds data.
func (p *BlkidProber) Format(ctx context.Context, device string) (string, error) {
	args := []string{"-p", "-s", "TYPE", "-s", "PTTYPE", "-o", "export", device}
	out, code, err := p.run(ctx, "blkid", args...)
	if err != nil {
		return "", fmt.Errorf("probe %s: %w", device, err)
	}
	switch code {
	case 0:
	case 2:
		// Nothing found — or nothing readable; see the doc comment above.
		return "", nil
	default:
		return "", fmt.Errorf("probe %s: blkid exited %d: %s", device, code, strings.TrimSpace(string(out)))
	}

	var fstype, pttype string
	for line := range strings.SplitSeq(string(out), "\n") {
		if len(line) == 0 {
			continue
		}
		cs := strings.Split(line, "=")
		if len(cs) != 2 {
			return "", fmt.Errorf("probe %s: unexpected blkid output: %s", device, string(out))
		}
		// TYPE is the filesystem type and PTTYPE the partition table type,
		// per libblkid's export format.
		switch cs[0] {
		case "TYPE":
			fstype = cs[1]
		case "PTTYPE":
			pttype = cs[1]
		}
	}

	if pttype != "" {
		return "", fmt.Errorf("probe %s: %w (%s)", device, ErrPartitionTable, pttype)
	}
	return fstype, nil
}

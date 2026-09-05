// Putting a file on a node, which is how the on-node tests get there.
//
// The layers under test call the kernel: an O_DIRECT read with its own
// alignment rules, an ioctl for the block size, a mount syscall, nvme-cli. None
// of that can be driven from the test host without replacing the very code that
// is supposed to be under test, so the test binary is compiled for the node and
// runs there. This is what carries it across.

package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// copyTimeout is generous because a Go test binary is tens of megabytes and the
// transfer is a base64 stream through the API server.
const copyTimeout = 5 * time.Minute

// CopyTo writes local's contents to remote inside pod, and makes it executable.
//
// It streams through `kubectl exec` rather than `kubectl cp`, which truncates
// large files silently: a test binary that arrives short still starts, and it
// fails in a way that reads as a bug in the code under test rather than as a
// broken transfer.
func (c *Cluster) CopyTo(ctx context.Context, namespace, pod, local, remote string) error {
	f, err := os.Open(local) //nolint:gosec // a path the test built itself
	if err != nil {
		return fmt.Errorf("open %s: %w", local, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", local, err)
	}

	ctx, cancel := context.WithTimeout(ctx, copyTimeout)
	defer cancel()

	//nolint:gosec // fixed binary, structured args
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", c.kubeconfig,
		"exec", "-i", "-n", namespace, pod, "--",
		"/bin/sh", "-c", "cat > "+shellWord(remote)+" && chmod 0755 "+shellWord(remote))
	cmd.Stdin = f
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("copy %s to %s/%s:%s: %w: %s", local, namespace, pod, remote, runErr, out)
	}

	// The transfer is only believable if the far end is the same size. A
	// truncated stream is the failure this method exists to avoid, and it is
	// silent unless something checks.
	out, err := c.Exec(ctx, namespace, pod, "wc -c < "+shellWord(remote))
	if err != nil {
		return fmt.Errorf("size %s on %s/%s: %w", remote, namespace, pod, err)
	}
	var landed int64
	if _, scanErr := fmt.Sscan(out, &landed); scanErr != nil {
		return fmt.Errorf("unreadable size for %s on %s/%s: %q", remote, namespace, pod, out)
	}
	if landed != info.Size() {
		return fmt.Errorf("copy %s to %s/%s:%s: %d of %d bytes arrived",
			local, namespace, pod, remote, landed, info.Size())
	}
	return nil
}

// shellWord makes one value safe as a single shell word.
func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

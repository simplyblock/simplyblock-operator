// Running a child process under a deadline that holds even when the process
// cannot be reaped.
//
// exec.CommandContext kills the process when its context is done, and the call
// then waits for the process to exit and for its output pipes to close. An
// nvme-cli blocked in the kernel exits only when the kernel lets it, and a
// process that left a child holding its output keeps the pipes open, so neither
// wait is bounded by the context. WaitDelay bounds the pipes, and running the
// call under Call bounds the rest.

package bounded

import (
	"context"
	"os/exec"
	"time"
)

// CommandTimeout is the budget for an nvme-cli query such as `nvme list` or
// `nvme list-subsys`, which read kernel state and return in milliseconds. It is
// longer than ReadTimeout only to cover starting the process.
const CommandTimeout = 2 * time.Second

// waitDelay is how long a killed process gets to release its output pipes
// before they are closed under it.
const waitDelay = 250 * time.Millisecond

// reapGrace is what the hard bound allows beyond the context deadline and
// waitDelay, for the kill and the pipe teardown themselves.
const reapGrace = 500 * time.Millisecond

// CombinedOutput runs name with args and returns its combined stdout and
// stderr, giving up after timeout or at ctx's deadline, whichever is earlier.
//
// key names the command for the stuck-call guard and for the error. Callers
// must keep secrets out of it, since an nvme-cli command line can carry DHCHAP
// keys, and should make it specific enough that one stuck target does not block
// commands for another.
func CombinedOutput(ctx context.Context, timeout time.Duration, key, name string, args ...string) ([]byte, error) {
	return run(ctx, timeout, key, true, name, args)
}

// Output is CombinedOutput with stdout only, for a command whose output is
// parsed and whose stderr would corrupt it.
func Output(ctx context.Context, timeout time.Duration, key, name string, args ...string) ([]byte, error) {
	return run(ctx, timeout, key, false, name, args)
}

func run(ctx context.Context, timeout time.Duration, key string, combined bool, name string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	limit := timeout
	if deadline, ok := ctx.Deadline(); ok {
		limit = time.Until(deadline)
	}
	return Call(key, limit+waitDelay+reapGrace, func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers pass fixed binaries and structured args
		cmd.WaitDelay = waitDelay
		if combined {
			return cmd.CombinedOutput()
		}
		return cmd.Output()
	})
}

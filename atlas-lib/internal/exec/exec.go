// Package exec is a thin, context-aware wrapper around os/exec used to
// invoke node tools such as nvme-cli. Internal so the public packages
// depend on behavior (the Connector/Resolver interfaces), not on the fact
// that a command is shelled out.
package exec

import (
	"context"
	"os/exec"
)

// Run executes name with args and returns combined stdout+stderr.
func Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

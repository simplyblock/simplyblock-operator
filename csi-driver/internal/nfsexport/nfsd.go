// Bringing the host's NFS server up, rather than requiring somebody to have.
//
// This starts the host's nfsd, not a copy: it is a kernel service, and the
// threads outlive this pod. That is not avoidable -- pNFS SCSI layouts are a
// kernel nfsd feature -- but who is responsible is. A prerequisite in a runbook
// is one a cluster silently fails to meet.
//
// Run before each assembly, not once at start-up, because a rebooted node comes
// back with none of it. Every step is skipped when already satisfied.

package nfsexport

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/simplyblock/atlas/export"
)

const (
	// The kernel default, and what the distributions ship.
	nfsdThreads = 8

	// Exists only once the nfsd filesystem is mounted, which is the test.
	nfsdControl = export.NFSDProcDir + "/threads"
)

// EnsureNFSD makes the host's NFS server ready to serve an export. Idempotent,
// and three reads when everything is already up. The steps report separately
// because they fail for different reasons: a missing module is a kernel with no
// NFS server, a failed mount or start is usually a lost privilege.
func EnsureNFSD(ctx context.Context, run runner) error {
	if err := ensureNFSDModule(ctx, run); err != nil {
		return err
	}
	if err := ensureNFSDFilesystem(ctx, run); err != nil {
		return err
	}
	return ensureNFSDThreads(ctx, run)
}

// runner is atlas/blockdev's command shape, named so a test can substitute.
type runner func(ctx context.Context, name string, args ...string) ([]byte, int, error)

// ensureNFSDModule loads nfsd. modprobe is a no-op when it is already in.
func ensureNFSDModule(ctx context.Context, run runner) error {
	out, code, err := run(ctx, "modprobe", "nfsd")
	if err != nil {
		return fmt.Errorf("nfsd: running modprobe: %w", err)
	}
	if code != 0 {
		return fmt.Errorf(
			"nfsd: modprobe exited %d, so this kernel has no NFS server to bring up: %s",
			code, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureNFSDFilesystem mounts the nfsd control filesystem if it is not there.
//
// In this container's own mount namespace, which is enough: nfsd is keyed by
// network namespace and the plugin runs with hostNetwork, so what this mount
// exposes is the host's nfsd. A hostPath would not be an option in any case,
// because runc refuses every bind mount whose target is inside /proc.
func ensureNFSDFilesystem(ctx context.Context, run runner) error {
	if _, err := os.Stat(nfsdControl); err == nil {
		return nil
	}
	out, code, err := run(ctx, "mount", "-t", "nfsd", "nfsd", export.NFSDProcDir)
	if err != nil {
		return fmt.Errorf("nfsd: mounting the control filesystem: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("nfsd: mounting %s exited %d: %s",
			export.NFSDProcDir, code, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureNFSDThreads starts the server if no threads are running. The count is
// read rather than assumed: rpc.nfsd on a live server resets it, taking threads
// from a host serving somebody else's exports.
func ensureNFSDThreads(ctx context.Context, run runner) error {
	if running, err := os.ReadFile(nfsdControl); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(string(running))); convErr == nil && n > 0 {
			return nil
		}
	}
	out, code, err := run(ctx, "rpc.nfsd", strconv.Itoa(nfsdThreads))
	if err != nil {
		return fmt.Errorf("nfsd: running rpc.nfsd: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("nfsd: rpc.nfsd exited %d: %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// WithNFSD brings the host's NFS server up before an export is built on it.
// Delete does not: tearing down on a host whose nfsd is gone is exactly the
// case that has to converge.
func WithNFSD(assembler exportAssembler) exportAssembler {
	return nfsdAssembler{inner: assembler, run: run}
}

// exportAssembler is exportrpc's Assembler.
type exportAssembler interface {
	Create(ctx context.Context, spec export.Spec) error
	Delete(ctx context.Context, spec export.Spec) error
}

type nfsdAssembler struct {
	inner exportAssembler
	run   runner
}

func (a nfsdAssembler) Create(ctx context.Context, spec export.Spec) error {
	if err := EnsureNFSD(ctx, a.run); err != nil {
		return err
	}
	return a.inner.Create(ctx, spec)
}

func (a nfsdAssembler) Delete(ctx context.Context, spec export.Spec) error {
	return a.inner.Delete(ctx, spec)
}

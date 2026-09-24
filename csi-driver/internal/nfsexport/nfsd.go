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
	"path/filepath"
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
// and five reads when everything is already up. The steps report separately
// because they fail for different reasons: a missing module is a kernel with no
// NFS server, a failed mount or start is usually a lost privilege.
//
// nfsdcld before the threads, deliberately: nfsd reads its persisted
// client-recovery database the moment its threads start, and that read is
// what the package comment on ensureNfsdcld explains hangs indefinitely
// without nfsdcld there yet to answer for it.
func EnsureNFSD(ctx context.Context, run runner) error {
	if err := ensureNFSDModule(ctx, run); err != nil {
		return err
	}
	if err := ensureNFSDFilesystem(ctx, run); err != nil {
		return err
	}
	if err := ensureNfsdcld(ctx, run); err != nil {
		return err
	}
	if err := ensureNFSDThreads(ctx, run); err != nil {
		return err
	}
	return ensureIdmapd(ctx, run)
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

// ensureNfsdcld starts nfsdcld if it is not already running, before nfsd's
// own threads (see EnsureNFSD).
//
// nfsdcld is the userspace half of nfsd's client-recovery tracking: nfsd
// keeps a small stable-storage record of which clients held state, so that a
// server restart can tell a genuinely reconnecting client from a network
// partition's stale one (RFC 3530 §8.6.3). That record lives in
// /var/lib/nfs/nfsdcld, which this package's caller mounts in from the host
// specifically so it survives a container restart -- and that persistence is
// exactly what makes the ordering here matter. nfsd reads it the moment its
// threads start, concludes (correctly, since the record is real) that it is
// recovering rather than starting clean, and opens a grace period against it.
// Found live: with nfsdcld not yet running to service that recovery over,
// the grace period a fresh nfsd entered on a host whose /var/lib/nfs held a
// record from an earlier instance never resolved -- every client's very
// first NFSv4.1 call (EXCHANGE_ID) hung indefinitely, well past the bounded
// lease time a grace period is supposed to take, rather than nfsd falling
// back or failing visibly. Starting nfsdcld first, before nfsd ever reads
// that record, does not hang.
//
// Checked before it is started for the same reason idmapd is: nfsdcld keeps
// no pidfile, daemonizes without refusing a second copy, and EnsureNFSD runs
// before every assembly.
func ensureNfsdcld(ctx context.Context, run runner) error {
	running, err := processRunning("nfsdcld")
	if err != nil {
		return fmt.Errorf("nfsd: checking for a running nfsdcld: %w", err)
	}
	if running {
		return nil
	}
	// No -F: nfsdcld's own default is to daemonize, which is what leaves it
	// running after this call returns.
	out, code, err := run(ctx, "nfsdcld")
	if err != nil {
		return fmt.Errorf("nfsd: running nfsdcld: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("nfsd: nfsdcld exited %d: %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureIdmapd starts rpc.idmapd if it is not already running.
//
// NFSv4 represents every owner and group as a string, never a raw id, so the
// kernel needs a translation for essentially every GETATTR -- which is most
// of them -- even under sec=sys, where nothing about the credential itself
// needs mapping. Without idmapd servicing that upcall the kernel is left
// waiting for an answer that will never come: found live, a client's mount
// hung in D state indefinitely rather than failing, on a host that had
// nfsd's own threads running and nothing else about the export wrong.
//
// It has to be checked before it is started. idmapd keeps no pidfile, daemonizes
// without refusing a second copy, and EnsureNFSD runs before every assembly (see
// the package comment) -- a start that assumed "not running" would leak one
// idmapd per reconcile, forever.
func ensureIdmapd(ctx context.Context, run runner) error {
	running, err := processRunning("rpc.idmapd")
	if err != nil {
		return fmt.Errorf("nfsd: checking for a running rpc.idmapd: %w", err)
	}
	if running {
		return nil
	}
	// No -f: idmapd's own default is to daemonize, which is what leaves it
	// running after this call returns. A foreground run would block here for
	// as long as idmapd stays up, which is indefinitely.
	out, code, err := run(ctx, "rpc.idmapd")
	if err != nil {
		return fmt.Errorf("nfsd: running rpc.idmapd: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("nfsd: rpc.idmapd exited %d: %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// processRunning reports whether a process with the given kernel comm is
// actually running.
//
// Reads /proc directly rather than shelling out to pgrep or ps: neither
// idmapd nor nfsdcld write a pidfile to check instead, and this way costs
// nothing this image might not carry.
//
// A zombie's comm survives until something reaps it, indistinguishable by
// name alone from a live process -- found live, on a host where idmapd had
// exited (its double-fork daemonizing left the original, exec'd process
// unreaped) and this check kept reporting it present from that entry alone,
// so ensureIdmapd never started a replacement. Excluded here rather than
// reaped: reaping is this process's own child's job, and does not belong to
// a query about whether a name is running.
func processRunning(comm string) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Errorf("listing /proc: %w", err)
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid directory
		}
		got, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue // the process exited between ReadDir and here
		}
		if strings.TrimSpace(string(got)) != comm {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue // exited between the comm and status reads
		}
		if processStatusIsZombie(status) {
			continue // a dead process, not a running one, whatever its comm still says
		}
		return true, nil
	}
	return false, nil
}

// processStatusIsZombie is processRunning's decision pulled out of the /proc
// scan, so it is testable directly against a "State:" line rather than a
// live zombie process.
func processStatusIsZombie(status []byte) bool {
	for _, line := range strings.Split(string(status), "\n") {
		if state, ok := strings.CutPrefix(line, "State:"); ok {
			return strings.Contains(state, "Z")
		}
	}
	return false
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
	Check(ctx context.Context, spec export.Spec) error
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

// Check reports nfsd itself unhealthy before asking inner about the export
// sitting on it: an export cannot be well served by a kernel NFS server with
// no threads running, whatever its own mount and export-table state says.
func (a nfsdAssembler) Check(ctx context.Context, spec export.Spec) error {
	if err := checkNFSDThreads(); err != nil {
		return err
	}
	return a.inner.Check(ctx, spec)
}

// checkNFSDThreads reads nfsd's own thread count rather than assuming it from
// having brought nfsd up once: the threads outlive this pod (see the package
// comment), but a kernel that dropped the module or another actor that reset
// them does not.
func checkNFSDThreads() error {
	data, err := os.ReadFile(nfsdControl)
	if err != nil {
		return fmt.Errorf("nfsd: reading its thread count: %w", err)
	}
	return validateThreadCount(data)
}

// validateThreadCount is checkNFSDThreads' parsing, pulled out so the actual
// decision -- is this a healthy count -- is testable without a real nfsd
// control filesystem, which a sandbox does not have.
func validateThreadCount(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return fmt.Errorf("nfsd: thread count %q is not a number: %w", trimmed, err)
	}
	if n <= 0 {
		return fmt.Errorf("nfsd: running with no threads")
	}
	return nil
}

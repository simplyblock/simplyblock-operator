// The guest's lifecycle inside the MDS pod: start QEMU, wait for the guest to
// turn healthy within the boot deadline, report readiness, and shut the guest
// down when the pod is told to stop.
//
// The pod is the unit of recovery. Whatever ends the guest (a panic, a power
// off from inside, a missed boot deadline) ends Run with an error, the runner
// exits, and kubelet restarts the pod into a cold boot that the operator then
// resyncs. The supervisor therefore never restarts QEMU itself. QEMU, the
// health probe, and the power button are injected, so the lifecycle is tested
// without KVM or a guest.

package runner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/simplyblock/atlas/errs/deferrers"
)

// ErrBootDeadline is returned when the guest did not turn healthy in time.
var ErrBootDeadline = errors.New("guest did not turn healthy within the boot deadline")

// ErrGuestExited is returned when QEMU ended without being asked to.
var ErrGuestExited = errors.New("guest exited")

// Process is a started QEMU.
type Process interface {
	// Wait blocks until the process has exited.
	Wait() error
	Kill() error
}

// Supervisor runs one guest.
type Supervisor struct {
	// Start starts QEMU.
	Start func(ctx context.Context) (Process, error)
	// Probe returns nil while the guest is healthy.
	Probe func(ctx context.Context) error
	// Powerdown asks the guest to shut down.
	Powerdown func(ctx context.Context) error

	BootDeadline  time.Duration
	ProbeInterval time.Duration
	// ShutdownGrace is how long a powered-down guest gets to exit before
	// QEMU is killed. It has to fit in the pod's termination grace period.
	ShutdownGrace time.Duration

	ready atomic.Bool
}

// Run starts the guest and supervises it until ctx is canceled, which shuts
// it down, or until it fails. It returns nil only for a shutdown that was
// asked for.
func (s *Supervisor) Run(ctx context.Context) error {
	proc, err := s.Start(ctx)
	if err != nil {
		return fmt.Errorf("starting QEMU: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- proc.Wait() }()
	defer s.ready.Store(false)

	boot := time.NewTimer(s.BootDeadline)
	defer boot.Stop()
	probe := time.NewTicker(s.ProbeInterval)
	defer probe.Stop()
	booted := false

	for {
		select {
		case <-ctx.Done():
			s.ready.Store(false)
			return s.shutdown(proc, exited)

		case err := <-exited:
			return fmt.Errorf("%w: %s", ErrGuestExited, describeExit(err))

		case <-boot.C:
			kill(proc, exited)
			return fmt.Errorf("%w (%s)", ErrBootDeadline, s.BootDeadline)

		case <-probe.C:
			healthy := s.probeOnce(ctx) == nil
			s.ready.Store(healthy)
			if healthy && !booted {
				booted = true
				boot.Stop()
			}
		}
	}
}

// Ready reports whether the last probe found the guest healthy.
func (s *Supervisor) Ready() bool {
	return s.ready.Load()
}

// probeOnce bounds one probe by the interval, so a hanging guest shows up as
// unhealthy on time instead of stalling the loop.
func (s *Supervisor) probeOnce(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.ProbeInterval)
	defer cancel()
	return s.Probe(ctx)
}

// shutdown presses the power button and gives the guest ShutdownGrace to
// exit, then kills QEMU. The guest unmounts its filesystems and stops nfsd on
// the way down, which a kill skips.
func (s *Supervisor) shutdown(proc Process, exited <-chan error) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.ShutdownGrace)
	defer cancel()

	if err := s.Powerdown(ctx); err != nil {
		kill(proc, exited)
		return fmt.Errorf("pressing the power button: %w, killed QEMU", err)
	}
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		kill(proc, exited)
		return fmt.Errorf("guest did not power down within %s, killed QEMU", s.ShutdownGrace)
	}
}

// kill kills QEMU and waits until it has exited, so nothing outlives Run.
func kill(proc Process, exited <-chan error) {
	deferrers.Run(proc.Kill)
	<-exited
}

func describeExit(err error) string {
	if err == nil {
		return "QEMU exited 0"
	}
	return "QEMU " + err.Error()
}

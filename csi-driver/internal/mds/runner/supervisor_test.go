package runner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeQEMU exits when told to or when killed.
type fakeQEMU struct {
	once   sync.Once
	exit   chan error
	killed atomic.Bool
}

func newFakeQEMU() *fakeQEMU { return &fakeQEMU{exit: make(chan error, 1)} }

func (f *fakeQEMU) Wait() error { return <-f.exit }

func (f *fakeQEMU) Kill() error {
	f.killed.Store(true)
	f.stop(errors.New("signal: killed"))
	return nil
}

func (f *fakeQEMU) stop(err error) { f.once.Do(func() { f.exit <- err }) }

// healthAfter fails the probe until n probes have run, then passes.
func healthAfter(n int32) func(context.Context) error {
	var probes atomic.Int32
	return func(context.Context) error {
		if probes.Add(1) > n {
			return nil
		}
		return errors.New("connection refused")
	}
}

func supervisor(q *fakeQEMU) *Supervisor {
	return &Supervisor{
		Start: func(context.Context) (Process, error) { return q, nil },
		Probe: healthAfter(2),
		// A guest that honors the power button: QEMU exits cleanly.
		Powerdown:     func(context.Context) error { q.stop(nil); return nil },
		BootDeadline:  2 * time.Second,
		ProbeInterval: 5 * time.Millisecond,
		ShutdownGrace: 2 * time.Second,
	}
}

func runAsync(ctx context.Context, s *Supervisor) <-chan error {
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	return done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestGuestTurnsReadyAndShutsDownCleanlyOnCancel(t *testing.T) {
	q := newFakeQEMU()
	s := supervisor(q)
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, s)

	waitFor(t, "readiness", s.Ready)
	cancel()

	if err := result(t, done); err != nil {
		t.Errorf("Run after a requested shutdown = %v, want nil", err)
	}
	if q.killed.Load() {
		t.Error("QEMU was killed although the guest powered down")
	}
	if s.Ready() {
		t.Error("still Ready after shutdown")
	}
}

// The pod restarts into a cold boot rather than serving nothing forever.
func TestMissedBootDeadlineKillsQEMU(t *testing.T) {
	q := newFakeQEMU()
	s := supervisor(q)
	s.Probe = func(context.Context) error { return errors.New("connection refused") }
	s.BootDeadline = 30 * time.Millisecond

	err := result(t, runAsync(context.Background(), s))
	if !errors.Is(err, ErrBootDeadline) {
		t.Errorf("Run = %v, want ErrBootDeadline", err)
	}
	if !q.killed.Load() {
		t.Error("QEMU was left running past the boot deadline")
	}
}

// A guest that powers off or panics on its own is a failure, even when QEMU
// exits 0: nobody asked for it, and the pod has to restart.
func TestGuestExitingOnItsOwnIsAnError(t *testing.T) {
	for name, exitErr := range map[string]error{"clean exit": nil, "crash": errors.New("exit status 1")} {
		t.Run(name, func(t *testing.T) {
			q := newFakeQEMU()
			s := supervisor(q)
			done := runAsync(context.Background(), s)

			waitFor(t, "readiness", s.Ready)
			q.stop(exitErr)

			if err := result(t, done); !errors.Is(err, ErrGuestExited) {
				t.Errorf("Run = %v, want ErrGuestExited", err)
			}
			if s.Ready() {
				t.Error("still Ready after the guest exited")
			}
		})
	}
}

func TestGuestIgnoringThePowerButtonIsKilledAfterTheGrace(t *testing.T) {
	q := newFakeQEMU()
	s := supervisor(q)
	var pressed atomic.Bool
	s.Powerdown = func(context.Context) error { pressed.Store(true); return nil }
	s.ShutdownGrace = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, s)

	waitFor(t, "readiness", s.Ready)
	cancel()
	if err := result(t, done); err == nil {
		t.Error("Run = nil for a guest that had to be killed")
	}

	if !pressed.Load() || !q.killed.Load() {
		t.Errorf("power button pressed = %v, QEMU killed = %v, want both", pressed.Load(), q.killed.Load())
	}
}

func TestFailedPowerdownKillsAtOnce(t *testing.T) {
	q := newFakeQEMU()
	s := supervisor(q)
	s.Powerdown = func(context.Context) error { return errors.New("QMP connection closed") }
	s.ShutdownGrace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, s)

	waitFor(t, "readiness", s.Ready)
	cancel()
	if err := result(t, done); err == nil {
		t.Error("Run = nil although the power button could not be pressed")
	}

	if !q.killed.Load() {
		t.Error("QEMU not killed after the power button could not be pressed")
	}
}

// Shutdown during boot is a shutdown, not a missed deadline.
func TestCancelDuringBootShutsDown(t *testing.T) {
	q := newFakeQEMU()
	s := supervisor(q)
	s.Probe = func(context.Context) error { return errors.New("connection refused") }
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, s)

	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := result(t, done); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

// Readiness follows the guest after boot: the pod drops out of the Service's
// endpoints while the guest is unhealthy, without being restarted.
func TestReadinessFollowsTheProbeAfterBoot(t *testing.T) {
	q := newFakeQEMU()
	s := supervisor(q)
	var healthy atomic.Bool
	healthy.Store(true)
	s.Probe = func(context.Context) error {
		if healthy.Load() {
			return nil
		}
		return errors.New("connection refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, s)

	waitFor(t, "readiness", s.Ready)
	healthy.Store(false)
	waitFor(t, "unreadiness", func() bool { return !s.Ready() })
	healthy.Store(true)
	waitFor(t, "readiness again", s.Ready)

	select {
	case err := <-done:
		t.Fatalf("Run returned while the guest was merely unhealthy: %v", err)
	default:
	}
}

func TestStartFailureIsReturned(t *testing.T) {
	s := supervisor(newFakeQEMU())
	s.Start = func(context.Context) (Process, error) { return nil, errors.New("exec: qemu-system-x86_64: not found") }
	if err := result(t, runAsync(context.Background(), s)); err == nil {
		t.Error("Run succeeded although QEMU did not start")
	}
}

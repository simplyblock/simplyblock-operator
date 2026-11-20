package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func healthy() Health {
	return Health{
		StateDir: "/var/lib/nfs",
		Mounted:  func(string) (bool, error) { return true, nil },
		NFSD:     func() error { return nil },
	}
}

func TestAGuestWithItsStateDiskAndNFSDIsHealthy(t *testing.T) {
	if err := healthy().Check(); err != nil {
		t.Errorf("Check = %v, want nil", err)
	}
}

// nfsd must not run without its client-recovery database: a server that comes
// back with an empty one grants no grace period, and clients lose their locks.
func TestAGuestWithoutItsStateDiskIsUnhealthy(t *testing.T) {
	h := healthy()
	var asked string
	h.Mounted = func(path string) (bool, error) { asked = path; return false, nil }
	if err := h.Check(); err == nil {
		t.Error("Check passed with the state disk not mounted")
	}
	if asked != "/var/lib/nfs" {
		t.Errorf("checked %q, want the state directory", asked)
	}

	h.Mounted = func(string) (bool, error) { return false, errors.New("stat: no such file") }
	if err := h.Check(); err == nil {
		t.Error("Check passed when the state directory could not be inspected")
	}
}

func TestAGuestWithoutNFSDIsUnhealthy(t *testing.T) {
	h := healthy()
	h.NFSD = func() error { return errors.New("nfsd has no threads") }
	if err := h.Check(); err == nil {
		t.Error("Check passed with nfsd down")
	}
}

func overall(t *testing.T, s *health.Server) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	resp, err := s.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		return healthpb.HealthCheckResponse_UNKNOWN
	}
	return resp.GetStatus()
}

func waitStatus(t *testing.T, s *health.Server, want healthpb.HealthCheckResponse_ServingStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for overall(t, s) != want {
		if time.Now().After(deadline) {
			t.Fatalf("status = %v, want %v", overall(t, s), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// The served status follows the check both ways, so a guest that recovers
// turns Ready again without a restart.
func TestReportFollowsTheCheck(t *testing.T) {
	s := health.NewServer()
	var failing atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Report(ctx, s, func() error {
		if failing.Load() {
			return errors.New("nfsd has no threads")
		}
		return nil
	}, time.Millisecond, time.Second)

	waitStatus(t, s, healthpb.HealthCheckResponse_SERVING)
	failing.Store(true)
	waitStatus(t, s, healthpb.HealthCheckResponse_NOT_SERVING)
	failing.Store(false)
	waitStatus(t, s, healthpb.HealthCheckResponse_SERVING)
}

// Regression: 2026-11-12-pnfs-mds-stays-dead-after-state-disk-journal-abort.
// The state disk lost its backing device, ext4 aborted its journal, and the
// guest still counted as healthy because the disk was still mounted.
func TestAGuestWhoseStateDiskTakesNoWriteIsUnhealthy(t *testing.T) {
	h := healthy()
	h.StateWritable = func() error { return errors.New("read-only file system") }
	if err := h.Check(); err == nil {
		t.Error("Check passed with a state disk that takes no write")
	}
}

// Regression: 2026-11-12-pnfs-mds-stays-dead-after-state-disk-journal-abort.
// Reproduced on a cluster: the export's XFS shut down on a log I/O error when
// its paths were removed, and reconnecting the paths never brought it back.
func TestAGuestWithAShutDownExportIsUnhealthy(t *testing.T) {
	h := healthy()
	h.Exports = func() error { return errors.New("statfs /exports/a: input/output error") }
	if err := h.Check(); err == nil {
		t.Error("Check passed with an export whose filesystem has shut down")
	}
}

// Regression: 2026-11-12-pnfs-mds-stays-dead-after-state-disk-journal-abort.
// The nfsd watchdog warned 2 h 40 m before anything else and nothing read it.
// A short episode is normal during a reconnect, and a long one is not.
func TestAGuestWhoseNFSDStaysStuckIsUnhealthy(t *testing.T) {
	h := healthy()
	h.StuckLimit = 3 * time.Minute
	h.Stuck = func() time.Duration { return 30 * time.Second }
	if err := h.Check(); err != nil {
		t.Errorf("Check = %v for a 30 s episode, want nil", err)
	}
	h.Stuck = func() time.Duration { return 4 * time.Minute }
	if err := h.Check(); err == nil {
		t.Error("Check passed with nfsd stuck for longer than the limit")
	}
}

// Regression: 2026-11-12-pnfs-mds-stays-dead-after-state-disk-journal-abort.
// A check that blocks on a dead disk must not freeze the last answer: the
// agent hung in the incident, and its last answer was healthy.
func TestAHangingCheckReportsNotServing(t *testing.T) {
	s := health.NewServer()
	var hang atomic.Bool
	var checks atomic.Int32
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	reported := make(chan struct{})
	defer func() {
		cancel()
		close(release)
		<-reported
	}()
	check := func() error {
		checks.Add(1)
		if hang.Load() {
			<-release
		}
		return ctx.Err()
	}
	go func() {
		defer close(reported)
		Report(ctx, s, check, time.Millisecond, 20*time.Millisecond)
	}()

	// The health server starts out SERVING, so wait for a check to have run.
	deadline := time.Now().Add(5 * time.Second)
	for checks.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the check never ran")
		}
		time.Sleep(time.Millisecond)
	}
	waitStatus(t, s, healthpb.HealthCheckResponse_SERVING)
	hang.Store(true)
	waitStatus(t, s, healthpb.HealthCheckResponse_NOT_SERVING)
}

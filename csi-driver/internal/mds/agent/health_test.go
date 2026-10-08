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
	}, time.Millisecond)

	waitStatus(t, s, healthpb.HealthCheckResponse_SERVING)
	failing.Store(true)
	waitStatus(t, s, healthpb.HealthCheckResponse_NOT_SERVING)
	failing.Store(false)
	waitStatus(t, s, healthpb.HealthCheckResponse_SERVING)
}

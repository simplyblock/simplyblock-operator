package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func healthy() Health {
	return Health{
		StateDir: stateDir,
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
	if asked != stateDir {
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

func roleOf(r nfsexportrpc.Role) func() nfsexportrpc.Role {
	return func() nfsexportrpc.Role { return r }
}

// U-21: a standby has no state volume and no nfsd, and is still ready.
func TestHealthU21ABootedStandbyServesWithoutTheStateVolumeOrNFSD(t *testing.T) {
	h := Health{
		StateDir: stateDir,
		Role:     roleOf(nfsexportrpc.RoleStandby),
		Mounted:  func(string) (bool, error) { return false, nil },
		NFSD:     func() error { return errors.New("nfsd has no threads") },
	}
	if err := h.Check(); err != nil {
		t.Errorf("Check = %v, want a standby to serve", err)
	}
}

func TestHealthU21AnActiveGuestNeedsTheStateVolumeAndNFSD(t *testing.T) {
	h := healthy()
	h.Role = roleOf(nfsexportrpc.RoleActive)
	if err := h.Check(); err != nil {
		t.Errorf("Check = %v, want an active guest with both to serve", err)
	}
	h.Mounted = func(string) (bool, error) { return false, nil }
	if err := h.Check(); err == nil {
		t.Error("an active guest without its state volume served")
	}
	h = healthy()
	h.Role = roleOf(nfsexportrpc.RoleActive)
	h.NFSD = func() error { return errors.New("nfsd has no threads") }
	if err := h.Check(); err == nil {
		t.Error("an active guest without nfsd threads served")
	}
}

func TestHealthAGuestInTheMiddleOfAPromotionIsNotServing(t *testing.T) {
	h := healthy()
	h.Role = roleOf(nfsexportrpc.RolePromoting)
	if err := h.Check(); err == nil {
		t.Error("a promoting guest served")
	}
}

// What the guest agent reports as its health, and how the runner reads it.
//
// The pod's readiness follows it, so it answers one question: can this guest
// assemble an export now. That needs the state disk mounted, since nfsd keeps
// its client-recovery database there and must not run without it, and nfsd
// running. The answer is served through gRPC's standard health service on
// the agent's port, which the runner probes across the pod's bridge.

package agent

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/klog"
)

// Health checks the guest.
type Health struct {
	// StateDir is where the state disk is mounted.
	StateDir string
	// Mounted reports whether a path is a mount point.
	Mounted func(path string) (bool, error)
	// NFSD returns nil while nfsd has threads running.
	NFSD func() error
}

// Check returns nil while the guest can serve, and why not otherwise.
func (h Health) Check() error {
	mounted, err := h.Mounted(h.StateDir)
	if err != nil {
		return fmt.Errorf("inspecting the state disk at %s: %w", h.StateDir, err)
	}
	if !mounted {
		return fmt.Errorf("the state disk is not mounted at %s", h.StateDir)
	}
	return h.NFSD()
}

// Report keeps the health server's overall status current with check, every
// interval, until ctx ends.
//
// The status starts as NOT_SERVING rather than the health server's default
// SERVING, so a guest that is still booting is never reported ready.
func Report(ctx context.Context, server *health.Server, check func() error, interval time.Duration) {
	server.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last error
	for {
		err := check()
		if err != nil {
			server.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
			if last == nil || last.Error() != err.Error() {
				klog.Warningf("guest unhealthy: %v", err)
			}
		} else {
			server.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		}
		last = err
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

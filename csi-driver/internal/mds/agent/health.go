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
	"os"
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
	// StateWritable returns nil while the state disk takes a write. Optional.
	StateWritable func() error
	// Exports returns nil while every assembled export's filesystem answers.
	// Optional.
	Exports func() error
	// Stuck reports how long nfsd has looked stuck, zero when it does not.
	// Optional.
	Stuck func() time.Duration
	// StuckLimit is how long nfsd may look stuck before the guest is
	// unhealthy.
	StuckLimit time.Duration
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
	if err := h.NFSD(); err != nil {
		return err
	}
	if h.StateWritable != nil {
		if err := h.StateWritable(); err != nil {
			return fmt.Errorf("the state disk at %s takes no write: %w", h.StateDir, err)
		}
	}
	if h.Exports != nil {
		if err := h.Exports(); err != nil {
			return err
		}
	}
	if h.Stuck != nil && h.StuckLimit > 0 {
		if stuck := h.Stuck(); stuck >= h.StuckLimit {
			return fmt.Errorf("nfsd has looked stuck for %s", stuck.Round(time.Second))
		}
	}
	return nil
}

// WriteProbe writes and syncs a small file in dir, so a state disk whose
// journal aborted or whose device is gone fails it.
func WriteProbe(dir string) func() error {
	return func() error {
		f, err := os.CreateTemp(dir, ".health-")
		if err != nil {
			return err
		}
		name := f.Name()
		defer os.Remove(name) //nolint:errcheck // best effort
		if _, err := f.WriteString("ok"); err != nil {
			f.Close() //nolint:errcheck // the write error is the answer
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close() //nolint:errcheck // the sync error is the answer
			return err
		}
		return f.Close()
	}
}

// Report keeps the health server's overall status current with check, every
// interval, until ctx ends.
//
// The status starts as NOT_SERVING rather than the health server's default
// SERVING, so a guest that is still booting is never reported ready.
func Report(ctx context.Context, server *health.Server, check func() error, interval, timeout time.Duration) {
	server.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last error
	for {
		err := checkWithin(check, timeout)
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

// checkWithin runs check and gives up after timeout. A check blocked in the
// kernel on a dead disk never returns, and the guest has to read as unhealthy
// meanwhile rather than keep its last answer. The blocked check is left behind.
func checkWithin(check func() error, timeout time.Duration) error {
	if timeout <= 0 {
		return check()
	}
	done := make(chan error, 1)
	go func() { done <- check() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("the health check did not answer within %s", timeout)
	}
}

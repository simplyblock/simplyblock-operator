// Package readiness serves the gateway's own readiness probe: whether every
// bucket directory under its root is still the mount DeliverBucket (the CSI
// node plugin) put there, or has silently fallen back to the node's local
// disk because that mount died under it -- most commonly, the node
// rebooting (design-bucket-provisioning.md Open Question 6). Separate from
// embedgw's own HealthPath (cmd/gateway's healthPath constant), which only
// answers that the process is up, never that anything mounted under it
// still is.
//
// This does not repair anything, and cannot: only the operator, re-asserting
// mount delivery (operator/internal/controllers/objectstore/
// s3bucket_delivery.go's periodic resync), can do that. What this buys is
// turning a silent wrong answer into a pod Kubernetes stops routing to.
package readiness

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/simplyblock/gateway/internal/mountcheck"
)

// shutdownTimeout bounds how long Serve waits for an in-flight probe to
// finish once ctx is done.
const shutdownTimeout = 5 * time.Second

// handler answers 200 when every bucket directory under rootDir is still a
// distinct mount (mountcheck.Unmounted), and 503 naming which ones are not
// otherwise. Split out of Serve so a test can exercise it without binding a
// real port.
func handler(rootDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		unmounted, err := mountcheck.Unmounted(rootDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(unmounted) > 0 {
			http.Error(w, fmt.Sprintf("bucket(s) not mounted: %v", unmounted), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// Serve runs handler(rootDir) at addr+path until ctx is done. Blocks;
// callers run it in its own goroutine.
func Serve(ctx context.Context, addr, path, rootDir string) error {
	mux := http.NewServeMux()
	mux.HandleFunc(path, handler(rootDir))

	srv := &http.Server{Addr: addr, Handler: mux}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("readiness: %w", err)
		}
		return nil
	}
}

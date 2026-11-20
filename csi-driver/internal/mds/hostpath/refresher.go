// Keeping the pod's redirects in step with the node's addresses: the runner
// asks the proxy periodically and rebuilds the redirects only when the answer
// changed.

package hostpath

import (
	"context"
	"net/netip"
	"slices"
	"time"
)

// RefreshInterval is how often the runner asks the proxy for the node's
// addresses. A proxy that was not up when the runner started is found within
// it.
const RefreshInterval = 30 * time.Second

// Refresher applies the proxy's addresses to the pod's redirects.
type Refresher struct {
	// Query asks the proxy for the node's addresses.
	Query func(ctx context.Context) ([]netip.Addr, error)
	// Apply makes the redirects match addrs.
	Apply func(ctx context.Context, addrs []netip.Addr) error
	// Log reports failed queries and applies.
	Log func(format string, args ...any)

	applied []netip.Addr
	ok      bool
}

// Tick queries once and applies the answer if it differs from what was last
// applied. A failed query leaves the redirects as they are: the proxy may be
// restarting, and the addresses it served before are still the node's.
func (r *Refresher) Tick(ctx context.Context) {
	addrs, err := r.Query(ctx)
	if err != nil {
		r.logf("asking the mds-proxy for the node's addresses: %v", err)
		return
	}
	if r.ok && slices.Equal(addrs, r.applied) {
		return
	}
	if err := r.Apply(ctx, addrs); err != nil {
		r.logf("redirecting the guest's connections to %v through the mds-proxy: %v", addrs, err)
		r.ok = false
		return
	}
	r.applied = slices.Clone(addrs)
	r.ok = true
}

// Run ticks at once and then every interval until ctx ends.
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Refresher) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

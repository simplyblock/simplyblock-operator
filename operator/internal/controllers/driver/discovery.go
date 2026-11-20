// Asking the API server whether this cluster has snapshot support.
//
// §4.1 reads the presence of a snapshot-controller from the API serving
// snapshot.storage.k8s.io/v1 rather than from any object, because that
// controller exists to reconcile those kinds and its Deployment is named
// differently by every distribution: kube-system/snapshot-controller on one,
// something else on the next, and nothing findable on a managed service that
// runs it outside the cluster.
//
// The answer is cached for a while rather than asked per reconcile. Discovery
// is a round trip against the API server's aggregated document, the driver
// resyncs on a timer, and the set of served API groups changes when somebody
// installs a CRD rather than continuously. The window is what bounds how long a
// cluster that has just gained snapshot support waits to be told.

package driver

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
)

// snapshotAPITTL is how long a discovery answer is trusted.
//
// It is shorter than the driver's own resync, so a cluster that gains the
// snapshot API is noticed on a reconcile rather than on the one after it, and
// long enough that a fleet of drivers does not turn one question into a poll.
const snapshotAPITTL = 2 * time.Minute

// DiscoveredSnapshotAPI answers [SnapshotAPI] from the API server's discovery
// document.
type DiscoveredSnapshotAPI struct {
	// Discovery is the client the question goes to.
	Discovery discovery.DiscoveryInterface

	mu       sync.Mutex
	served   bool
	asked    bool
	askedAt  time.Time
	nowFuncT func() time.Time
}

// SnapshotAPIServed reports whether the cluster serves
// snapshot.storage.k8s.io/v1.
//
// A group that is not served comes back as a NotFound rather than as an empty
// list, and that is the ordinary answer here rather than a failure: it is what
// a cluster without the snapshot CRDs says. Every other error is returned, so a
// reconcile refuses rather than deciding from an API server it could not reach.
func (d *DiscoveredSnapshotAPI) SnapshotAPIServed(_ context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.asked && d.now().Sub(d.askedAt) < snapshotAPITTL {
		return d.served, nil
	}
	if d.Discovery == nil {
		return false, fmt.Errorf("no discovery client is configured")
	}

	_, err := d.Discovery.ServerResourcesForGroupVersion(snapshotGroupVersion.String())
	switch {
	case apierrors.IsNotFound(err):
		d.served = false
	case err != nil:
		return false, fmt.Errorf("read the resources of %s: %w", snapshotGroupVersion, err)
	default:
		d.served = true
	}

	d.asked, d.askedAt = true, d.now()
	return d.served, nil
}

// now is the clock, which a test replaces to age the cache without waiting.
func (d *DiscoveredSnapshotAPI) now() time.Time {
	if d.nowFuncT != nil {
		return d.nowFuncT()
	}
	return time.Now()
}

// The bucket phase collector: what the operator publishes about S3Buckets on
// a timer rather than on an event.
//
// Separate from the reconciler for the reason the storage pool collector
// already is: a reconcile writes an object when something changed, and the
// phase gauge is a property of the bucket that stands still between
// reconciles. Rebuilding it from the full list on every tick, rather than
// updating it inline as each bucket reconciles, is what lets a deleted
// bucket's series be cleared rather than reported in its last phase
// forever.

package objectstore

import (
	"context"
	"fmt"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
)

// DefaultCollectInterval is how often the phase gauge is rebuilt. A bucket's
// phase is also published by the reconciler's own status writes (readable
// through kubectl); this is what a dashboard scrapes instead.
const DefaultCollectInterval = 30 * time.Second

// bucketPhases is every value the gauge carries a series for, so a bucket
// currently in one phase still reports 0 for the other four rather than
// those series being absent.
var bucketPhases = []objectstorev1alpha1.S3BucketPhase{
	phasePending, phaseProvisioning, phaseReady, phaseDegraded, phaseDeleting,
}

// Collector publishes the bucket phase gauge.
type Collector struct {
	client.Client

	// Interval is how often to collect. Zero selects DefaultCollectInterval.
	Interval time.Duration
}

// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=s3buckets,verbs=get;list;watch

// NeedLeaderElection implements manager.LeaderElectionRunnable. The gauge is
// a per-bucket fact rather than a per-replica one, so a follower publishing
// it too would make the series depend on which replica a scrape reached.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: collect once immediately, so a restart
// does not leave the gauge empty for an interval, then on every tick.
func (c *Collector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval == 0 {
		interval = DefaultCollectInterval
	}
	log := logf.FromContext(ctx).WithName("s3bucket-collector")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.collect(ctx); err != nil {
			// A failed pass is not a failed operator: the next tick tries
			// again, and the gauge keeps whatever the last successful pass
			// left.
			log.Error(err, "collecting S3Bucket metrics")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// collect rebuilds the phase gauge from the buckets that exist right now.
func (c *Collector) collect(ctx context.Context) error {
	var buckets objectstorev1alpha1.S3BucketList
	if err := c.List(ctx, &buckets); err != nil {
		return fmt.Errorf("list the S3Buckets: %w", err)
	}

	resetBucketPhaseMetrics()
	for i := range buckets.Items {
		b := &buckets.Items[i]
		for _, phase := range bucketPhases {
			value := 0.0
			if b.Status.Phase == phase {
				value = 1
			}
			bucketPhaseState.WithLabelValues(b.Spec.BucketName, b.Spec.ObjectStoreRef, string(phase)).Set(value)
		}
	}
	return nil
}

// The phase collectors: what the operator publishes about S3Buckets and
// ObjectStores on a timer rather than on an event.
//
// Separate from the reconcilers for the reason the storage pool collector
// already is: a reconcile writes an object when something changed, and a
// phase gauge is a property that stands still between reconciles.
// Rebuilding it from the full list on every tick, rather than updating it
// inline as each object reconciles, is what lets a deleted object's series
// be cleared rather than reported in its last phase forever. One Collector
// carries both passes rather than two runnables, since both are the same
// shape and both read from the same manager's cache.

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

// objectStorePhaseValues is the same, for ObjectStore's own four-value phase.
var objectStorePhaseValues = []objectstorev1alpha1.ObjectStorePhase{
	osPhasePending, osPhaseProvisioning, osPhaseReady, osPhaseDegraded,
}

// Collector publishes the bucket phase gauge.
type Collector struct {
	client.Client

	// Interval is how often to collect. Zero selects DefaultCollectInterval.
	Interval time.Duration
}

// +kubebuilder:rbac:groups=objectstore.simplyblock.io,resources=s3buckets;objectstores,verbs=get;list;watch

// NeedLeaderElection implements manager.LeaderElectionRunnable. Both gauges
// are per-object facts rather than per-replica ones, so a follower
// publishing them too would make the series depend on which replica a
// scrape reached.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: collect once immediately, so a restart
// does not leave the gauges empty for an interval, then on every tick.
func (c *Collector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval == 0 {
		interval = DefaultCollectInterval
	}
	log := logf.FromContext(ctx).WithName("objectstore-collector")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.collect(ctx); err != nil {
			// A failed pass is not a failed operator: the next tick tries
			// again, and the gauges keep whatever the last successful pass
			// left.
			log.Error(err, "collecting S3Bucket/ObjectStore metrics")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// collect rebuilds both phase gauges, and the ready-replicas gauge, from the
// buckets and ObjectStores that exist right now.
func (c *Collector) collect(ctx context.Context) error {
	if err := c.collectBuckets(ctx); err != nil {
		return err
	}
	return c.collectObjectStores(ctx)
}

func (c *Collector) collectBuckets(ctx context.Context) error {
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

func (c *Collector) collectObjectStores(ctx context.Context) error {
	var stores objectstorev1alpha1.ObjectStoreList
	if err := c.List(ctx, &stores); err != nil {
		return fmt.Errorf("list the ObjectStores: %w", err)
	}

	resetObjectStoreMetrics()
	for i := range stores.Items {
		s := &stores.Items[i]
		for _, phase := range objectStorePhaseValues {
			value := 0.0
			if s.Status.Phase == phase {
				value = 1
			}
			objectStorePhaseState.WithLabelValues(s.Name, string(phase)).Set(value)
		}
		objectStoreReadyReplicasCount.WithLabelValues(s.Name).Set(float64(s.Status.ReadyReplicas))
	}
	return nil
}

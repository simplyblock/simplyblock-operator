// The pool collector: what the operator publishes about pools on a timer rather
// than on an event.
//
// It is separate from the reconciler because the two answer to different clocks.
// The reconciler writes a Kubernetes object when a spec or the control plane
// says something changed, and everything it publishes is a property of the pool
// that stands still: its ceilings, its classes, its phase. What a pool holds
// moves continuously, arrives from Prometheus rather than from a reconcile, and
// is worth neither an etcd write nor a wake-up, so it is read on a tick,
// published as a gauge, and never stored.
//
// One pass rebuilds every series from the full list of pools. That is
// deliberately not incremental: a collector that updated one pool's gauges as
// its object changed would have to remember which series to delete when a pool
// went away, and a series nobody deletes reports a deleted pool as half full
// forever.
//
// design-storagepool.md §9.2 is the specification.

package pool

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/prometheus"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// DefaultCollectInterval is how often the gauges are rebuilt. It is the
// resolution of a capacity graph rather than of an alert: a pool fills over
// hours, and the phase it is in is published by the reconciler's own writes as
// well.
const DefaultCollectInterval = 30 * time.Second

// CapacitySource supplies what a pool holds. It is satisfied by atlas-lib's
// prometheus.Provider, and it is an interface here so that a test needs no
// Prometheus and a deployment without one can pass nil.
//
// The control plane's own pool response is not the source. It carries the
// ceilings the pool was created with and says nothing about how full it is,
// while the metrics the same service exports carry the occupancy.
type CapacitySource interface {
	// PoolCapacity returns the sample for every pool of a cluster, keyed by
	// control-plane pool id. A pool with no sample is absent.
	PoolCapacity(ctx context.Context, clusterUUID string) (map[string]prometheus.Capacity, error)
}

// VolumeSource is the read side of the control-plane volume cache, which is what
// says how many logical volumes a pool actually holds. It is an interface so the
// collector can be tested without a stream.
type VolumeSource interface {
	// PoolVolumeCounts returns the number of cached volumes per pool id.
	PoolVolumeCounts() map[string]int
}

// Collector publishes the pool gauges and the one pool event that needs a
// measurement to decide.
type Collector struct {
	client.Client

	// Recorder announces a pool reaching its capacity limit.
	Recorder events.EventRecorder

	// Capacity is where the used and provisioned sizes come from. A nil source
	// is a deployment with no reachable Prometheus: every gauge that does not
	// depend on one is still published, and the occupancy is absent rather than
	// zero, because a zero is the reading of an empty pool and not the absence
	// of a reading.
	Capacity CapacitySource

	// Volumes is the control-plane volume cache. Nil leaves the volume count
	// unpublished, for the same reason.
	Volumes VolumeSource

	// Interval is how often to collect. Zero selects DefaultCollectInterval.
	Interval time.Duration

	// exhausted remembers which pools have already been warned about, so a pool
	// that stays full is one event rather than one per tick. A pool that drops
	// back under its limit is forgotten, which is what makes the next filling a
	// second crossing and a second warning, and so is a pool that goes away: the
	// map is pruned to the pools each pass listed, so it cannot grow with every
	// pool the deployment has ever held, and a pool recreated under the same
	// name does not inherit a crossing it never made.
	exhausted map[string]bool
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools,verbs=get;list;watch

// NeedLeaderElection implements manager.LeaderElectionRunnable. The gauges are
// per-pool facts rather than per-replica ones, and the events are written to the
// API: a follower publishing both would double every event and make the gauges
// depend on which replica a scrape reached.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: collect once immediately, so a restart does
// not leave the gauges empty for an interval, then on every tick.
func (c *Collector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval == 0 {
		interval = DefaultCollectInterval
	}
	log := logf.FromContext(ctx).WithName("storagepool-collector")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.collect(ctx, log); err != nil {
			// A failed pass is not a failed operator: the next tick tries again,
			// and the gauges keep whatever the last successful pass left.
			log.Error(err, "collecting storage pool metrics")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// collect rebuilds every gauge from the pools that exist right now.
func (c *Collector) collect(ctx context.Context, log logr.Logger) error {
	var pools simplyblockv1alpha2.StoragePoolList
	if err := c.List(ctx, &pools); err != nil {
		return fmt.Errorf("list the storage pools: %w", err)
	}

	volumeCounts := map[string]int{}
	if c.Volumes != nil {
		volumeCounts = c.Volumes.PoolVolumeCounts()
	}
	bound, err := c.boundVolumeCounts(ctx)
	if err != nil {
		return err
	}
	samples := newCapacityLookup(c.Capacity, log)

	resetPoolMetrics()
	seen := make(map[string]bool, len(pools.Items))

	for i := range pools.Items {
		p := &pools.Items[i]
		cluster, name := p.Spec.ClusterRef, p.Name
		key := p.Namespace + "/" + name
		seen[key] = true

		poolCapacityBytes.WithLabelValues(cluster, name).Set(float64(parseSize(limitCapacity(p))))
		poolStorageClassesCount.WithLabelValues(cluster, name).
			Set(float64(len(p.Status.StorageClassNames)))
		poolBoundVolumesCount.WithLabelValues(cluster, name).
			Set(float64(bound[p.Status.UUID] + bound[p.Name]))
		for _, phase := range poolPhases {
			value := 0.0
			if p.Status.Phase == phase {
				value = 1
			}
			poolPhaseState.WithLabelValues(cluster, name, string(phase)).Set(value)
		}
		if count, ok := volumeCounts[p.Status.UUID]; ok {
			poolVolumesCount.WithLabelValues(cluster, name).Set(float64(count))
		}

		sample, ok := samples.forPool(ctx, c.clusterUUID(ctx, p), p.Status.UUID)
		if !ok {
			continue
		}
		poolUsedBytes.WithLabelValues(cluster, name).Set(float64(sample.Used))
		poolProvisionedBytes.WithLabelValues(cluster, name).Set(float64(sample.Provisioned))
		c.reportExhaustion(p, key, sample)
	}

	for key := range c.exhausted {
		if !seen[key] {
			delete(c.exhausted, key)
		}
	}
	return nil
}

// poolPhases is every phase the gauge publishes a series for, so that a pool
// leaving a phase sets it to zero rather than leaving the last value behind.
var poolPhases = []simplyblockv1alpha2.StoragePoolPhase{
	simplyblockv1alpha2.StoragePoolPhasePending,
	simplyblockv1alpha2.StoragePoolPhaseReady,
	simplyblockv1alpha2.StoragePoolPhaseDeleting,
}

// reportExhaustion announces a pool that has reached the capacity it was given,
// once per crossing.
func (c *Collector) reportExhaustion(
	p *simplyblockv1alpha2.StoragePool, key string, sample prometheus.Capacity,
) {
	limit := parseSize(limitCapacity(p))
	// A pool with no capacity limit cannot be exhausted, and comparing against a
	// limit of zero would announce every pool that holds anything at all.
	if limit <= 0 {
		return
	}
	full := sample.Provisioned >= limit
	if c.exhausted == nil {
		c.exhausted = map[string]bool{}
	}
	if full && !c.exhausted[key] {
		if c.Recorder != nil {
			c.Recorder.Eventf(p, nil, corev1.EventTypeWarning, CapacityExhausted, CapacityExhausted,
				"the pool has provisioned %d bytes against its limit of %d; "+
					"further claims will not be satisfied until the limit is raised",
				sample.Provisioned, limit)
		}
	}
	c.exhausted[key] = full
}

// clusterUUID resolves the pool's cluster to the id the exporter labels its
// series with. It is read per pool rather than cached across the pass because a
// deployment has few clusters and the manager's cache answers it without a round
// trip.
func (c *Collector) clusterUUID(ctx context.Context, p *simplyblockv1alpha2.StoragePool) string {
	var cluster simplyblockv1alpha1.StorageCluster
	key := client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.ClusterRef}
	if err := c.Get(ctx, key, &cluster); err != nil {
		return ""
	}
	return cluster.Status.UUID
}

// boundVolumeCounts counts the PersistentVolumes per pool segment of their
// volume handle. Both spellings of the segment are counted under their own key —
// a UUID for a handle provisioned after the v2 API migration and a pool name for
// one provisioned before it — and the caller adds the two, because one pool's
// volumes may be split across both generations.
func (c *Collector) boundVolumeCounts(ctx context.Context) (map[string]int, error) {
	var volumes corev1.PersistentVolumeList
	if err := c.List(ctx, &volumes); err != nil {
		return nil, fmt.Errorf("list the persistent volumes: %w", err)
	}
	counts := map[string]int{}
	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if !kube.IsManaged(pv) {
			continue
		}
		if segment := handlePoolSegment(pv.Spec.CSI.VolumeHandle); segment != "" {
			counts[segment]++
		}
	}
	return counts, nil
}

// capacityLookup fetches samples once per cluster for the duration of one pass,
// because a pass walks many pools of the same few clusters and each query is an
// HTTP round trip.
//
// A cluster whose query fails is recorded as having no samples and is not
// retried within the pass. Prometheus being down therefore costs the occupancy
// gauges and not the rest of them.
type capacityLookup struct {
	source    CapacitySource
	log       logr.Logger
	byCluster map[string]map[string]prometheus.Capacity
}

func newCapacityLookup(source CapacitySource, log logr.Logger) *capacityLookup {
	return &capacityLookup{
		source:    source,
		log:       log,
		byCluster: map[string]map[string]prometheus.Capacity{},
	}
}

// forPool returns the sample for one pool and whether there is one.
//
// Presence in the map is the test, not Capacity.Sampled: the control plane
// exports no pool_date, unlike the volume and device families, so every pool
// sample carries a zero SampledAt and asking whether it was sampled would reject
// all of them.
func (l *capacityLookup) forPool(
	ctx context.Context, clusterUUID, poolUUID string,
) (prometheus.Capacity, bool) {
	if l == nil || l.source == nil || clusterUUID == "" || poolUUID == "" {
		return prometheus.Capacity{}, false
	}
	samples, ok := l.byCluster[clusterUUID]
	if !ok {
		var err error
		samples, err = l.source.PoolCapacity(ctx, clusterUUID)
		if err != nil {
			l.log.V(1).Info("no pool capacity samples for this pass",
				"cluster", clusterUUID, "err", err.Error())
			samples = nil
		}
		l.byCluster[clusterUUID] = samples
	}
	sample, ok := samples[poolUUID]
	return sample, ok
}

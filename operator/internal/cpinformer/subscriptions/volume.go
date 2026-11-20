// The volume subscription: it streams one storage pool's logical volumes and
// caches them, and that is all it does.
//
// Every other subscription in this package feeds a reconciler that writes a
// Kubernetes object, and so carries a trigger channel and an object-naming rule.
// This one deliberately does not. Its reader is the aggregated metrics API,
// which computes a LogicalVolumeMetrics from the cache at the moment a client
// asks for one; nothing is ever written, so there is nothing to reconcile and no
// object to name. The cache is the whole product.
//
// That also decides where it runs. A subscription backing a mirror runs on the
// leader, because two writers would fight. This one backs a read served by
// whichever replica the aggregated API's Service routed the request to, so every
// replica needs its own cache and the manager that owns it is not leader-elected
// (see cpinformer.EveryReplica).

package subscriptions

import (
	"context"
	"fmt"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// VolumeCapacityDTO is the capacity block the control plane reports with a
// volume. The sizes are bytes; SizeUtil is a percentage from 0 to 100.
//
// SizeUsed against SizeProv is why the subscription exists: a volume is thin-
// provisioned, so what it was asked for and what it occupies are different
// numbers, and only the first appears anywhere in the Kubernetes API.
type VolumeCapacityDTO struct {
	// Date is the sample's Unix timestamp in seconds.
	Date      int64 `json:"date"`
	SizeTotal int64 `json:"size_total"`
	SizeProv  int64 `json:"size_prov"`
	SizeUsed  int64 `json:"size_used"`
	SizeFree  int64 `json:"size_free"`
	SizeUtil  int32 `json:"size_util"`
}

// VolumeDTO is the operator's view of a control-plane volume, matching the v2
// VolumeDTO wire schema. Unknown fields are ignored on decode, so the thirty-odd
// fields the metrics API does not publish (QoS limits, fabric addresses,
// replication state) cost nothing here, and a field added upstream does not
// break the decode.
type VolumeDTO struct {
	ID        string            `json:"id"`
	ClusterID string            `json:"cluster_id"`
	PoolID    string            `json:"pool_uuid"`
	PoolName  string            `json:"pool_name"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Size      int64             `json:"size"`
	Capacity  VolumeCapacityDTO `json:"capacity"`
}

// VolumeSubscription streams a storage pool's volumes (one stream per pool) and
// decodes them into an in-memory cache. It performs no Kubernetes writes and
// emits no reconcile triggers; readers pull from it with [VolumeSubscription.All]
// and [VolumeSubscription.Get].
type VolumeSubscription struct {
	*Cache[VolumeDTO]
}

// NewVolumeSubscription returns an empty volume subscription.
func NewVolumeSubscription() *VolumeSubscription {
	return &VolumeSubscription{Cache: NewCache(func(v VolumeDTO) string { return v.ID })}
}

// Name implements cpinformer.Subscription.
func (s *VolumeSubscription) Name() string { return "volume" }

// Path implements cpinformer.Subscription: volumes are scoped per (cluster,
// pool), so one stream is opened per storage pool.
func (s *VolumeSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/", scope[0], scope[1])
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache. It performs no API I/O, so it never stalls the stream loop.
func (s *VolumeSubscription) Ingest(_ context.Context, ev cpinformer.Event) error {
	// No onChange: nothing reconciles a volume. The aggregated metrics API
	// reads this cache when a client asks, so a change needs no notification.
	return s.Cache.Ingest(ev, nil)
}

// All returns every cached volume across every pool. The metrics API reads it
// this way because it serves a namespace's volumes and a namespace is not a
// pool: one namespace's claims can be provisioned from several.

// Get returns the cached volume with the given control-plane id.
func (s *VolumeSubscription) Get(volumeID string) (VolumeDTO, bool) {
	_, dto, ok := s.Find(volumeID)
	return dto, ok
}

// PoolVolumeCounts returns how many cached volumes each pool holds, keyed by
// control-plane pool id.
//
// It counts what the control plane reports rather than what Kubernetes accounts
// for, and that is the point: the gap between this number and the pool's bound
// PersistentVolumes is the unmanaged-volume condition, which blocks a node drain
// and is better noticed before somebody tries to drain.
//
// A pool whose stream has not been opened is absent from the result rather than
// present and zero, so an unopened subscription is not reported as an empty pool.
func (s *VolumeSubscription) PoolVolumeCounts() map[string]int {
	counts := map[string]int{}
	for _, v := range s.All() {
		if v.PoolID != "" {
			counts[v.PoolID]++
		}
	}
	return counts
}

var _ cpinformer.Subscription = (*VolumeSubscription)(nil)

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
	"encoding/json"
	"fmt"
	"sync"

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
	store *cpinformer.Store[VolumeDTO]

	mu     sync.Mutex
	synced map[string]bool // scopeKey -> first snapshot applied
}

// NewVolumeSubscription returns an empty volume subscription.
func NewVolumeSubscription() *VolumeSubscription {
	return &VolumeSubscription{
		store:  cpinformer.NewStore(func(v VolumeDTO) string { return v.ID }),
		synced: map[string]bool{},
	}
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
	switch ev.Kind {
	case cpinformer.EventSnapshot:
		var dtos []VolumeDTO
		if err := json.Unmarshal(ev.Data, &dtos); err != nil {
			return fmt.Errorf("decode snapshot: %w", err)
		}
		// Replace rather than merge: a snapshot arrives on every reconnect, and
		// a volume deleted while the stream was down is absent from it and from
		// nothing else.
		s.store.Replace(ev.Scope, dtos)
		s.markSynced(ev.Scope)

	case cpinformer.EventCreated, cpinformer.EventUpdated:
		var dto VolumeDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode %s: %w", ev.Kind, err)
		}
		s.store.Upsert(ev.Scope, dto)

	case cpinformer.EventDeleted:
		var dto VolumeDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode deleted: %w", err)
		}
		if dto.ID == "" {
			return nil // physical removal carries {}; the snapshot relist covers it
		}
		s.store.Remove(ev.Scope, dto.ID)
	}
	return nil
}

// All returns every cached volume across every pool. The metrics API reads it
// this way because it serves a namespace's volumes and a namespace is not a
// pool: one namespace's claims can be provisioned from several.
func (s *VolumeSubscription) All() []VolumeDTO { return s.store.All() }

// Get returns the cached volume with the given control-plane id.
func (s *VolumeSubscription) Get(volumeID string) (VolumeDTO, bool) {
	_, dto, ok := s.store.Find(volumeID)
	return dto, ok
}

// Synced reports whether a scope has received its initial snapshot. A metrics
// read is served from whatever is cached rather than waiting on this: a pool
// that has never connected is indistinguishable from a pool with no volumes, and
// a cluster with no pools would otherwise never become ready.
func (s *VolumeSubscription) Synced(scope cpinformer.Scope) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[scope.Key()]
}

func (s *VolumeSubscription) markSynced(scope cpinformer.Scope) {
	s.mu.Lock()
	s.synced[scope.Key()] = true
	s.mu.Unlock()
}

var _ cpinformer.Subscription = (*VolumeSubscription)(nil)

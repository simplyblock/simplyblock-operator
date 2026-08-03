/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package subscriptions holds the operator's cpinformer.Subscription
// implementations — one per control-plane resource type mirrored into
// Kubernetes. A Subscription owns everything about the event: retrieval (path +
// scopes), decoding, filtering, caching, and enqueuing a reconcile trigger for
// the affected Kubernetes object. It does NOT write Kubernetes objects — that is
// a separate concern handled by a reconciler that reads the subscription's cache
// (see controller.LogicalVolumeReconciler).
package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// VolumeDTO is the operator's view of a control-plane volume, matching the v2
// VolumeDTO wire schema (design doc §3.3). Unknown fields are ignored on decode.
type VolumeDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	PoolName string `json:"pool_name"`
	Size     int64  `json:"size"`
	NQN      string `json:"nqn"`
	Status   string `json:"status"`
}

// VolumeFilter decides whether a volume should be mirrored. A nil filter accepts
// everything. This is where PVC-backed-only filtering plugs in.
type VolumeFilter func(VolumeDTO) bool

// VolumeSubscription streams control-plane volumes (one stream per pool),
// decodes and filters them into an in-memory cache, and enqueues a reconcile
// trigger naming the affected LogicalVolume CR. It performs no Kubernetes writes;
// a reconciler consumes its cache ([VolumeSubscription.Lookup]) and trigger
// channel ([VolumeSubscription.Triggers]).
type VolumeSubscription struct {
	namespace string
	store     *cpinformer.Store[VolumeDTO]
	ch        chan event.GenericEvent
	filter    VolumeFilter

	mu     sync.Mutex
	synced map[string]bool // scopeKey -> first snapshot applied
}

// NewVolumeSubscription returns a volume subscription that mirrors into
// LogicalVolume CRs in namespace. filter may be nil to mirror every volume.
func NewVolumeSubscription(namespace string, filter VolumeFilter) *VolumeSubscription {
	return &VolumeSubscription{
		namespace: namespace,
		store:     cpinformer.NewStore(func(v VolumeDTO) string { return v.ID }),
		ch:        make(chan event.GenericEvent, 1024),
		filter:    filter,
		synced:    map[string]bool{},
	}
}

// Name implements cpinformer.Subscription.
func (s *VolumeSubscription) Name() string { return "volume" }

// Path implements cpinformer.Subscription: volumes are scoped per (cluster, pool).
func (s *VolumeSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/", scope[0], scope[1])
}

// Ingest implements cpinformer.Subscription: it decodes and filters the event
// into the cache, then enqueues a reconcile trigger for each affected volume's
// LogicalVolume CR. It performs no API I/O, so it never stalls the stream loop.
func (s *VolumeSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	switch ev.Kind {
	case cpinformer.EventSnapshot:
		var dtos []VolumeDTO
		if err := json.Unmarshal(ev.Data, &dtos); err != nil {
			return fmt.Errorf("decode snapshot: %w", err)
		}
		kept := dtos[:0]
		for _, dto := range dtos {
			if s.accept(dto) {
				kept = append(kept, dto)
			}
		}
		present, removed := s.store.Replace(ev.Scope, kept)
		s.markSynced(ev.Scope)
		// Re-sync every current volume, and delete any that vanished while
		// disconnected.
		for _, id := range present {
			s.enqueue(ctx, id)
		}
		for _, id := range removed {
			s.enqueue(ctx, id)
		}

	case cpinformer.EventCreated, cpinformer.EventUpdated:
		var dto VolumeDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode %s: %w", ev.Kind, err)
		}
		// A volume that no longer passes the filter is dropped so its CR is deleted.
		if s.accept(dto) {
			s.store.Upsert(ev.Scope, dto)
		} else {
			s.store.Remove(ev.Scope, dto.ID)
		}
		s.enqueue(ctx, dto.ID)

	case cpinformer.EventDeleted:
		var dto VolumeDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode deleted: %w", err)
		}
		if dto.ID == "" {
			return nil // physical removal carries {}; the snapshot relist covers it
		}
		s.store.Remove(ev.Scope, dto.ID)
		s.enqueue(ctx, dto.ID)
	}
	return nil
}

func (s *VolumeSubscription) accept(dto VolumeDTO) bool {
	return s.filter == nil || s.filter(dto)
}

// enqueue pushes a reconcile trigger naming the volume's LogicalVolume CR,
// dropping it only on shutdown.
func (s *VolumeSubscription) enqueue(ctx context.Context, volumeID string) {
	lv := &simplyblockv1alpha1.LogicalVolume{}
	lv.SetNamespace(s.namespace)
	lv.SetName(simplyblockv1alpha1.LogicalVolumeName(volumeID))
	select {
	case s.ch <- event.GenericEvent{Object: lv}:
	case <-ctx.Done():
	}
}

// Triggers is the reconcile-trigger channel; the reconciler attaches it via
// source.Channel. Each event names the LogicalVolume CR to reconcile.
func (s *VolumeSubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// Lookup returns the cached volume with the given id and the scope it belongs
// to, or ok=false if the control plane no longer has it.
func (s *VolumeSubscription) Lookup(volumeID string) (cpinformer.Scope, VolumeDTO, bool) {
	return s.store.Find(volumeID)
}

// Synced reports whether a scope has received its initial snapshot; the
// reconciler must not treat an unsynced scope's cache as authoritative (i.e.
// must not delete a CR just because the volume is absent from a cold cache).
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

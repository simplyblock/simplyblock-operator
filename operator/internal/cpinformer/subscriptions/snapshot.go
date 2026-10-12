// The snapshot subscription: it streams one pool's snapshots, caches them, and
// wakes the backup request that took each one. It lives here for the reason the
// backup subscription does: retrieval, decoding, and caching are the
// subscription's concerns, and writing Kubernetes objects is the reconciler's.
//
// Its trigger differs from the backup mirror's in what it names. The mirror's
// objects are named by the store's identifier, so a stream event can name its
// object outright. A request is a person's object with a name of their choosing,
// so the event carries the snapshot's name instead, and the request reconciler
// maps it back through an index.

package subscriptions

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// SnapshotDTO is the operator's view of a control-plane snapshot, matching the
// v2 snapshot wire schema. Unknown fields are ignored on decode.
type SnapshotDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// SnapshotSubscription streams a pool's snapshots (one stream per pool) into an
// in-memory cache and enqueues a trigger naming each changed snapshot. It
// performs no Kubernetes writes.
type SnapshotSubscription struct {
	*Cache[SnapshotDTO]

	ch chan event.GenericEvent
}

// NewSnapshotSubscription returns an empty snapshot subscription.
func NewSnapshotSubscription() *SnapshotSubscription {
	return &SnapshotSubscription{
		Cache: NewCache(func(s SnapshotDTO) string { return s.ID }),
		ch:    make(chan event.GenericEvent, 1024),
	}
}

// Name implements cpinformer.Subscription.
func (s *SnapshotSubscription) Name() string { return "snapshot" }

// Path implements cpinformer.Subscription: snapshots are scoped per (cluster,
// pool), so one stream is opened per storage pool.
func (s *SnapshotSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/snapshots/", scope[0], scope[1])
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache, then triggers on each snapshot the stream still reports. A snapshot that
// left gets no trigger, because nothing waits for a snapshot to disappear. It
// performs no API I/O, so it never stalls the stream loop.
func (s *SnapshotSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	return s.Cache.Ingest(ev, func(_ cpinformer.Scope, id string, present bool) {
		if !present {
			return
		}
		if _, dto, ok := s.Find(id); ok {
			enqueueByName(ctx, s.ch, dto.Name)
		}
	})
}

// FindByName returns the snapshot of that name in one pool's scope. A request
// derives its snapshot's name from its UID, so the name is the one stable handle
// it has before the control plane has issued an identifier.
func (s *SnapshotSubscription) FindByName(scope cpinformer.Scope, name string) (SnapshotDTO, bool) {
	for _, dto := range s.List(scope) {
		if dto.Name == name {
			return dto, true
		}
	}
	return SnapshotDTO{}, false
}

// Triggers is the trigger channel; each event carries a snapshot's name as the
// name of a metadata-only object, for the request reconciler to map back to its
// request.
func (s *SnapshotSubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// enqueueByName pushes a trigger that carries only a name, giving up on
// shutdown rather than dropping it when the channel is full.
func enqueueByName(ctx context.Context, ch chan<- event.GenericEvent, name string) {
	if name == "" {
		return
	}
	obj := &metav1.PartialObjectMetadata{}
	obj.SetName(name)
	select {
	case ch <- event.GenericEvent{Object: obj}:
	case <-ctx.Done():
	}
}

var _ cpinformer.Subscription = (*SnapshotSubscription)(nil)

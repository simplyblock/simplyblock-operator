// The half of a subscription that does not depend on which resource it streams:
// the cache, whether a scope has been snapshotted, and the decode of an event
// into that cache.
//
// Every subscription in this package needs exactly this and differs only in
// which DTO it decodes, which URL it streams from, and what it does about a
// change once the cache holds it. Keeping the common half here is what stops
// the next resource from arriving as a third copy of the same event switch, and
// what makes the reconnect and empty-delete rules hold identically for all of
// them rather than per file.

package subscriptions

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// Cache holds one resource type's control-plane state and tracks which scopes
// have been snapshotted. It is safe for concurrent use: the stream goroutine
// writes it through Ingest while reconcilers read it.
type Cache[DTO any] struct {
	store *cpinformer.Store[DTO]
	idOf  func(DTO) string

	mu     sync.Mutex
	synced map[string]bool // scopeKey -> first snapshot applied
}

// NewCache returns a cache keyed by the id that idOf reads off a DTO.
func NewCache[DTO any](idOf func(DTO) string) *Cache[DTO] {
	return &Cache[DTO]{
		store:  cpinformer.NewStore(idOf),
		idOf:   idOf,
		synced: map[string]bool{},
	}
}

// Changed is how Ingest reports what moved, once the cache already reflects it.
// present distinguishes a resource the control plane still reports from one it
// has stopped reporting, which is the difference between indexing a resource
// and forgetting it.
//
// It runs on the stream goroutine, so it must not block on I/O.
type Changed func(scope cpinformer.Scope, id string, present bool)

// Ingest decodes one event into the cache and reports each affected id through
// onChange, which may be nil for a subscription that only answers reads.
//
// Three rules are enforced here rather than per resource, because getting any
// of them wrong is silent:
//
//   - A snapshot replaces its scope rather than merging into it. One arrives on
//     every reconnect, and a resource deleted while the stream was down is
//     absent from the snapshot and from nothing else, so merging would keep it
//     forever.
//   - The cache is updated before onChange runs, so a reconcile triggered by a
//     change cannot read the state that preceded it.
//   - A delete carrying no id is ignored. The control plane sends an empty
//     object once a resource is gone entirely, and the relist is what says
//     which one that was, so removing anything here would be removing a guess.
func (c *Cache[DTO]) Ingest(ev cpinformer.Event, onChange Changed) error {
	report := func(scope cpinformer.Scope, id string, present bool) {
		if onChange != nil {
			onChange(scope, id, present)
		}
	}

	switch ev.Kind {
	case cpinformer.EventSnapshot:
		var dtos []DTO
		if err := json.Unmarshal(ev.Data, &dtos); err != nil {
			return fmt.Errorf("decode snapshot: %w", err)
		}
		present, removed := c.store.Replace(ev.Scope, dtos)
		c.markSynced(ev.Scope)
		for _, id := range present {
			report(ev.Scope, id, true)
		}
		for _, id := range removed {
			report(ev.Scope, id, false)
		}

	case cpinformer.EventCreated, cpinformer.EventUpdated:
		var dto DTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode %s: %w", ev.Kind, err)
		}
		c.store.Upsert(ev.Scope, dto)
		report(ev.Scope, c.idOf(dto), true)

	case cpinformer.EventDeleted:
		var dto DTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode deleted: %w", err)
		}
		id := c.idOf(dto)
		if id == "" {
			return nil
		}
		c.store.Remove(ev.Scope, id)
		report(ev.Scope, id, false)
	}
	return nil
}

// Find returns the cached resource with the given id and the scope it belongs
// to, or ok=false when the control plane no longer reports it.
func (c *Cache[DTO]) Find(id string) (cpinformer.Scope, DTO, bool) {
	if id == "" {
		var zero DTO
		return nil, zero, false
	}
	return c.store.Find(id)
}

// List returns every resource cached for one scope.
func (c *Cache[DTO]) List(scope cpinformer.Scope) []DTO {
	return c.store.List(scope)
}

// All returns every cached resource across every scope.
func (c *Cache[DTO]) All() []DTO {
	return c.store.All()
}

// Synced reports whether a scope has received its initial snapshot. Until it
// has, an absent resource is an absence of information rather than evidence
// that the control plane has forgotten it, so a reconciler must not delete
// anything on the strength of a miss.
func (c *Cache[DTO]) Synced(scope cpinformer.Scope) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.synced[scope.Key()]
}

func (c *Cache[DTO]) markSynced(scope cpinformer.Scope) {
	c.mu.Lock()
	c.synced[scope.Key()] = true
	c.mu.Unlock()
}

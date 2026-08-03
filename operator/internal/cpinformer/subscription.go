package cpinformer

import "context"

// Event kinds delivered to a [Subscription] (design doc §3.3). For a snapshot,
// Event.Data is a JSON array of DTOs; otherwise a single DTO object.
const (
	EventSnapshot = "snapshot"
	EventCreated  = "created"
	EventUpdated  = "updated"
	EventDeleted  = "deleted"
)

// Event is one decoded control-plane change handed to a [Subscription].
type Event struct {
	Kind  string // one of the Event* constants
	Scope Scope
	Data  []byte // JSON: an array for a snapshot, a single object otherwise
}

// Subscription is one resource type plugged into a [SubscriptionManager]. It
// declares where to stream (Path), which scopes to stream (Scopes), and how to
// ingest each change (Ingest).
//
// Ingest runs on the stream read goroutine, so it must be cheap and must not
// block on external I/O (no API calls): it decodes the event into the
// subscription's in-memory cache and enqueues a reconcile trigger. The actual
// Kubernetes writes happen off this path, in a reconciler the subscription
// registers with the manager — so a slow or failing API server never stalls or
// tears down the stream.
type Subscription interface {
	// Name identifies the subscription in logs.
	Name() string
	// Path builds the watch URL (appended to the endpoint) for a scope, e.g.
	// /api/v2/clusters/<c>/storage-pools/<p>/volumes/ — "?watch=true" is added
	// automatically.
	Path(scope Scope) string
	// Ingest decodes one event into the cache and enqueues a reconcile trigger.
	// It must not block on external I/O.
	Ingest(ctx context.Context, ev Event) error
}

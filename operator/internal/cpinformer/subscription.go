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
// declares where to stream a scope from (Path) and how to ingest each change
// (Ingest). Which scopes are streamed is not its to decide: the manager hands
// back a [ScopeSet] at registration, and the reconcilers that know which
// objects exist drive it.
//
// Ingest runs on the stream read goroutine, so it must be cheap and must not
// block on external I/O (no API calls): it decodes the event into the
// subscription's in-memory cache and enqueues a reconcile trigger. The actual
// Kubernetes writes happen off this path, in a reconciler the subscription
// registers with the manager — so a slow or failing API server never stalls or
// tears down the stream.
//
// The one thing it may wait on is room in its own trigger channel. That channel
// is drained by controller-runtime's source.Channel into a workqueue, which is
// unbounded and deduplicates by key, so the drain is an enqueue rather than a
// reconcile and does not move at the speed of the API server. Waiting there is
// deliberate: a dropped trigger is a mirror left stale until the next event or
// reconnect, because nothing else observes a control-plane-only change.
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

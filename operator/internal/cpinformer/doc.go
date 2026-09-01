// Package cpinformer turns the simplyblock control-plane's Server-Sent-Events
// push notifications into controller-runtime reconcile triggers, and keeps a
// local, in-memory cache of control-plane state that reconcilers read instead
// of polling the HTTP API.
//
// It is a re-implementation of client-go's SharedIndexInformer + Lister
// pattern, sourced from SSE (`GET .../<resource>/?watch=true`) rather than the
// Kubernetes API server. See operator/docs/designs/design-sse-push-notifications.md
// for the full design and the verified upstream wire contract.
//
// # Pieces
//
//   - [Store] / [Lister] — a thread-safe cache keyed by control-plane id,
//     scoped by the watch's path parameters (e.g., cluster and storage node for
//     devices).
//   - [Subscription] — one resource type: where to stream it from, and how to
//     ingest each change into a cache and enqueue a reconcile trigger. Each
//     implementation lives in the subscriptions subpackage.
//   - [SubscriptionManager] — a manager.Runnable that opens and closes one
//     stream per active scope as the operator's own objects come and go, and
//     hands each decoded event to its subscription. [Election] decides whether
//     it runs on the leader alone or on every replica.
//   - [ScopeSet] — the live set of scopes a subscription streams. Reconcilers
//     add and remove scopes; the manager opens and closes streams to match.
//
// # Division of labor
//
// A subscription owns retrieval, decoding, and caching. It never writes
// Kubernetes objects: that is a reconciler's, reading the subscription's cache
// and its trigger channel. The split is what keeps a slow or failing API server
// from stalling or tearing down a stream.
//
// # Consistency rules (see the design doc §6)
//
//   - The cache is updated *before* the reconcile trigger is enqueued
//     (store-then-enqueue ordering).
//   - Every SSE (re)connect begins with a full snapshot; a subscription diffs it
//     against its cache and enqueues the entities that vanished during the
//     disconnect (reconnect relist with delete-detection).
//   - A scope is not authoritative until its first snapshot has been applied,
//     so a reconciler must not delete an object merely because a cold cache
//     does not have its entity.
//   - The Lister is not linearizable: a briefly stale read causes at most one
//     extra, idempotent reconcile, never incorrectness.
package cpinformer

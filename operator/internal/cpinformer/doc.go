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
//     scoped by the watch's path parameters (e.g. cluster+pool for volumes).
//   - [Informer] — ties an SSE stream to a Store: it applies snapshot/created/
//     updated/deleted events and, for every applied change, pushes a
//     controller-runtime event onto a channel via [Informer.Channel].
//   - [SubscriptionManager] — a leader-election-gated manager.Runnable that
//     opens and closes one stream per scope as the operator's own CRs come and
//     go.
//
// # Consistency rules (see the design doc §6)
//
//   - The Store is updated *before* the reconcile trigger is enqueued
//     (store-then-enqueue ordering).
//   - Every SSE (re)connect begins with a full snapshot; the Informer diffs it
//     against the Store and synthesizes deletions for entities that vanished
//     during a disconnect (reconnect relist with delete-detection).
//   - The Lister is not linearizable: a briefly-stale read causes at most one
//     extra, idempotent reconcile, never incorrectness.
//
// This is an initial implementation of the reusable core plus a Volume pilot;
// wiring into a specific reconciler is intentionally left to the caller (see
// the example on [Informer.Source]).
package cpinformer

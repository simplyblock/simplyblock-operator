package cpinformer

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/simplyblock/atlas/errs/deferrers"
)

// StreamConfig is the shared connection config for every subscription: it
// locates and authenticates against the control plane but is resource-agnostic
// (the path comes from each [Subscription]).
type StreamConfig struct {
	// Endpoint is the control-plane base URL, e.g. http://simplyblock-webappapi:5000.
	Endpoint string
	// Token is the bearer token (cluster secret or service-account JWT).
	Token string
	// Client is the HTTP client; it must have no request timeout (streams are
	// long-lived). nil applies a plain client.
	Client *http.Client

	// RetryBase and RetryMax bound the reconnect backoff; the server hints 3s.
	RetryBase time.Duration
	RetryMax  time.Duration
	// Liveness is the maximum gap between frames (events or `: ping`) before a
	// half-open connection is dropped. The server pings every 15s.
	Liveness time.Duration
}

func (c *StreamConfig) withDefaults() {
	if c.RetryBase == 0 {
		c.RetryBase = 3 * time.Second
	}
	if c.RetryMax == 0 {
		c.RetryMax = 30 * time.Second
	}
	if c.Liveness == 0 {
		c.Liveness = 45 * time.Second
	}
}

var errLiveness = errors.New("cpinformer: liveness deadline exceeded")

// ScopeSet is a subscription's live set of scopes to stream. Callers add and
// remove scopes as their source CRs come and go (e.g. a Pool controller adding a
// pool's scope when it becomes ready); the manager opens/closes streams to match.
type ScopeSet struct {
	mu     sync.Mutex
	scopes map[string]Scope
	notify chan struct{}
}

func newScopeSet() *ScopeSet {
	return &ScopeSet{scopes: map[string]Scope{}, notify: make(chan struct{}, 1)}
}

// Add includes a scope in the set (idempotent).
func (s *ScopeSet) Add(scope Scope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.scopes[scope.Key()]; !ok {
		s.scopes[scope.Key()] = scope
		s.signal()
	}
}

// Remove drops a scope from the set (idempotent).
func (s *ScopeSet) Remove(scope Scope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.scopes[scope.Key()]; ok {
		delete(s.scopes, scope.Key())
		s.signal()
	}
}

func (s *ScopeSet) signal() { // caller holds s.mu
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *ScopeSet) list() []Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Scope, 0, len(s.scopes))
	for _, sc := range s.scopes {
		out = append(out, sc)
	}
	return out
}

// SubscriptionManager is the single, global owner of the operator's
// control-plane streams. Each resource type is added as a [Subscription] with
// its own [ScopeSet]; the manager keeps one stream per scope and dispatches every
// decoded event to the subscription's Ingest.
//
// It is one manager.Runnable and manager.LeaderElectionRunnable shared across
// all resource types, and runs on the leader only (design doc §8).
type SubscriptionManager struct {
	cfg  StreamConfig
	log  logr.Logger
	subs []registration
	wg   sync.WaitGroup
}

type registration struct {
	sub    Subscription
	scopes *ScopeSet
}

// NewSubscriptionManager returns a manager with the shared stream config.
func NewSubscriptionManager(cfg StreamConfig, log logr.Logger) *SubscriptionManager {
	cfg.withDefaults()
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	return &SubscriptionManager{cfg: cfg, log: log.WithName("cpinformer")}
}

// AddSubscription registers a resource type and returns the [ScopeSet] the caller
// drives to control which scopes are streamed. Call before Start.
func (m *SubscriptionManager) AddSubscription(sub Subscription) *ScopeSet {
	ss := newScopeSet()
	m.subs = append(m.subs, registration{sub: sub, scopes: ss})
	return ss
}

// NeedLeaderElection reports that this runnable runs on the leader only.
func (m *SubscriptionManager) NeedLeaderElection() bool { return true }

// Start runs every subscription until ctx is canceled, then tears the streams
// down. It implements manager.Runnable.
func (m *SubscriptionManager) Start(ctx context.Context) error {
	for _, reg := range m.subs {
		m.wg.Add(1)
		go func(reg registration) {
			defer m.wg.Done()
			m.runSubscription(ctx, reg)
		}(reg)
	}
	m.log.Info("subscription manager started", "subscriptions", len(m.subs))
	<-ctx.Done()
	m.wg.Wait()
	m.log.Info("subscription manager stopped")
	return nil
}

// runSubscription keeps one stream alive per scope in the subscription's
// ScopeSet, reconciling whenever the set changes, until ctx is canceled.
func (m *SubscriptionManager) runSubscription(ctx context.Context, reg registration) {
	log := m.log.WithValues("subscription", reg.sub.Name())
	streams := map[string]context.CancelFunc{} // scopeKey -> cancel
	defer func() {
		for _, cancel := range streams {
			cancel()
		}
	}()

	for {
		m.reconcileStreams(ctx, reg.sub, streams, reg.scopes.list(), log)
		select {
		case <-ctx.Done():
			return
		case <-reg.scopes.notify:
		}
	}
}

// reconcileStreams opens streams for newly-desired scopes and cancels those no
// longer desired.
func (m *SubscriptionManager) reconcileStreams(ctx context.Context, sub Subscription, streams map[string]context.CancelFunc, scopes []Scope, log logr.Logger) {
	want := make(map[string]Scope, len(scopes))
	for _, s := range scopes {
		want[s.Key()] = s
	}
	for key, cancel := range streams {
		if _, ok := want[key]; !ok {
			cancel()
			delete(streams, key)
		}
	}
	for key, scope := range want {
		if _, ok := streams[key]; ok {
			continue
		}
		streamCtx, cancel := context.WithCancel(ctx)
		streams[key] = cancel
		m.wg.Add(1)
		go func(scope Scope) {
			defer m.wg.Done()
			m.runStream(streamCtx, sub, scope, log)
		}(scope)
	}
}

// runStream keeps one scope's stream connected, reconnecting with jittered
// exponential backoff. Each (re)connect re-seeds via the server's snapshot.
func (m *SubscriptionManager) runStream(ctx context.Context, sub Subscription, scope Scope, log logr.Logger) {
	backoff := m.cfg.RetryBase
	for {
		if ctx.Err() != nil {
			return
		}
		delivered, err := m.streamOnce(ctx, sub, scope)
		if ctx.Err() != nil {
			return
		}
		if delivered {
			backoff = m.cfg.RetryBase
		}
		if err != nil {
			log.V(1).Info("stream disconnected, reconnecting", "scope", scope.Key(), "after", backoff.String(), "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, m.cfg.RetryMax)
	}
}

// streamOnce runs a single connection: connect, then dispatch frames to the
// subscription until the stream ends, errors, or the liveness watchdog fires.
func (m *SubscriptionManager) streamOnce(ctx context.Context, sub Subscription, scope Scope) (delivered bool, err error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := openStream(reqCtx, m.cfg, sub.Path(scope))
	if err != nil {
		return false, err
	}
	defer deferrers.Close(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("watch %s %s: unexpected status %s", sub.Name(), scope.Key(), resp.Status)
	}

	timer := time.AfterFunc(m.cfg.Liveness, cancel)
	defer timer.Stop()
	keepalive := func() { timer.Reset(m.cfg.Liveness) }

	derr := decodeSSE(resp.Body,
		func(ev sseEvent) error {
			delivered = true
			keepalive()
			return m.dispatch(ctx, sub, scope, ev)
		},
		keepalive,
	)
	if derr != nil && reqCtx.Err() != nil && ctx.Err() == nil {
		return delivered, errLiveness
	}
	return delivered, derr
}

// dispatch routes one decoded frame to the subscription (or surfaces a stream
// error to trigger a reconnect).
func (m *SubscriptionManager) dispatch(ctx context.Context, sub Subscription, scope Scope, ev sseEvent) error {
	switch ev.Name {
	case EventSnapshot, EventCreated, EventUpdated, EventDeleted:
		return sub.Ingest(ctx, Event{Kind: ev.Name, Scope: scope, Data: ev.Data})
	case eventError:
		return fmt.Errorf("control-plane reported stream error: %s", strings.TrimSpace(string(ev.Data)))
	default:
		return nil // unknown/ignored frame
	}
}

// jitter returns d scaled by a random factor in [0.5, 1.0) to spread reconnects.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int63n(int64(d)/2+1))
}

package cpinformer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// stubSub is a Subscription that records the events it ingests.
type stubSub struct {
	path string

	mu     sync.Mutex
	events []Event
}

func (s *stubSub) Name() string      { return "stub" }
func (s *stubSub) Path(Scope) string { return s.path }
func (s *stubSub) Ingest(_ context.Context, ev Event) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}
func (s *stubSub) handled() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func writeSSE(w http.ResponseWriter, name, data string) {
	_, _ = io.WriteString(w, "event: "+name+"\ndata: "+data+"\n\n")
	w.(http.Flusher).Flush()
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", msg)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSubscriptionManagerStreamsAndDispatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("watch"); got != "true" {
			t.Errorf("watch query = %q, want true", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, EventSnapshot, `[{"id":"v1"}]`)
		writeSSE(w, EventCreated, `{"id":"v2"}`)
		<-r.Context().Done() // hold the stream open (no reconnect storm)
	}))
	defer srv.Close()

	sub := &stubSub{path: "/vols"}
	m := NewSubscriptionManager(StreamConfig{Endpoint: srv.URL, Liveness: 2 * time.Second}, logr.Discard(), LeaderOnly)
	scopes := m.AddSubscription(sub)
	scopes.Add(Scope{"c", "p"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Start(ctx); close(done) }()

	waitFor(t, func() bool { return len(sub.handled()) >= 2 }, "two dispatched events")

	got := sub.handled()
	if got[0].Kind != EventSnapshot || got[1].Kind != EventCreated {
		t.Errorf("event kinds = %q,%q; want snapshot,created", got[0].Kind, got[1].Kind)
	}
	if got[0].Scope.Key() != "c/p" {
		t.Errorf("scope = %q, want c/p", got[0].Scope.Key())
	}

	cancel()
	<-done
}

func TestSubscriptionManagerAddScopeAfterStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, EventSnapshot, `[]`)
		<-r.Context().Done()
	}))
	defer srv.Close()

	sub := &stubSub{path: "/vols"}
	m := NewSubscriptionManager(StreamConfig{Endpoint: srv.URL}, logr.Discard(), LeaderOnly)
	scopes := m.AddSubscription(sub) // empty at start

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Start(ctx); close(done) }()

	// A scope added after Start opens a stream.
	scopes.Add(Scope{"c", "p"})
	waitFor(t, func() bool { return len(sub.handled()) >= 1 }, "the snapshot after Add")

	cancel()
	<-done
}

func TestSubscriptionManagerNeedsLeaderElection(t *testing.T) {
	m := NewSubscriptionManager(StreamConfig{}, logr.Discard(), LeaderOnly)
	if !m.NeedLeaderElection() {
		t.Error("NeedLeaderElection() = false, want true")
	}
}

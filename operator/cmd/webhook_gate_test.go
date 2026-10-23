// Tests for the ordering between the operator's admission webhooks and its
// controllers: a controller holds its first reconcile until the webhook Service
// can be reached, and the pod reports ready only once the webhook serves.

package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// recordingManager keeps what is added to it instead of running it.
type recordingManager struct {
	manager.Manager
	added []manager.Runnable
}

func (m *recordingManager) Add(r manager.Runnable) error {
	m.added = append(m.added, r)
	return nil
}

// startRecorder is a runnable that reports when it starts.
type startRecorder struct {
	started chan struct{}
	leader  bool
}

func (r *startRecorder) Start(ctx context.Context) error {
	close(r.started)
	<-ctx.Done()
	return nil
}

func (r *startRecorder) NeedLeaderElection() bool { return r.leader }

// Regression: 2026-10-23-controllers-before-webhook: the controllers started
// with the manager, while the webhook server came up only once its serving
// certificate had been provisioned, about 30s later on lab-talos. The
// OperatorOps reconciler's write of a ClusterDeploymentConfig was refused 17
// times with "connection refused" by the operator's own webhook, and the
// growing requeue backoff then held the first discovery run for about 11
// minutes.
func TestAControllerDoesNotStartBeforeTheWebhookServes(t *testing.T) {
	gate := make(chan struct{})
	rec := &recordingManager{}
	controller := &startRecorder{started: make(chan struct{}), leader: true}
	if err := (gatedManager{Manager: rec, gate: gate}).Add(controller); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(rec.added) != 1 {
		t.Fatalf("%d runnables reached the manager, want 1", len(rec.added))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rec.added[0].Start(ctx) }()

	select {
	case <-controller.started:
		t.Fatal("the controller started while the webhook was not serving")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	select {
	case <-controller.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the controller did not start once the webhook served")
	}
}

// A runnable held back by the gate goes to the same manager group it would
// have gone to without it: a controller that needs no leader election must not
// start waiting for one.
func TestAGatedRunnableKeepsItsLeaderElection(t *testing.T) {
	for _, leader := range []bool{true, false} {
		rec := &recordingManager{}
		_ = (gatedManager{Manager: rec, gate: make(chan struct{})}).Add(
			&startRecorder{started: make(chan struct{}), leader: leader})
		got, ok := rec.added[0].(manager.LeaderElectionRunnable)
		if !ok || got.NeedLeaderElection() != leader {
			t.Errorf("NeedLeaderElection through the gate = %v (ok=%v), want %v", got, ok, leader)
		}
	}
}

// A controller waiting on the gate stops waiting when the manager stops.
func TestAGatedRunnableReturnsWhenTheManagerStops(t *testing.T) {
	rec := &recordingManager{}
	_ = (gatedManager{Manager: rec, gate: make(chan struct{})}).Add(
		&startRecorder{started: make(chan struct{}), leader: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rec.added[0].Start(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the gated runnable kept waiting after its context ended")
	}
}

func webhookSlice(ready *bool) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-operator-webhook-service-abcde",
			Namespace: "simplyblock",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "simplyblock-operator-webhook-service"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	if ready != nil {
		s.Endpoints = []discoveryv1.Endpoint{{
			Addresses:  []string{"10.244.2.9"},
			Conditions: discoveryv1.EndpointConditions{Ready: ready},
		}}
	}
	return s
}

func gateOver(t *testing.T, slices ...*discoveryv1.EndpointSlice) *webhookGate {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, s := range slices {
		b = b.WithObjects(s)
	}
	g := newWebhookGate(b.Build(), "simplyblock", "simplyblock-operator-webhook-service", logr.Discard())
	g.interval = 10 * time.Millisecond
	return g
}

func opensWithin(g *webhookGate, d time.Duration) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = g.Start(ctx) }()
	select {
	case <-g.open:
		return true
	case <-time.After(d):
		return false
	}
}

// Until the Service has a ready endpoint the API server has nowhere to send an
// admission request: it fails as "connection refused" when the endpoint is a
// pod not yet listening, and as "no endpoints available" when there is none.
func TestTheGateStaysShutWithoutAReadyWebhookEndpoint(t *testing.T) {
	notReady := false
	cases := map[string]*webhookGate{
		"no EndpointSlice":          gateOver(t),
		"an EndpointSlice, empty":   gateOver(t, webhookSlice(nil)),
		"an endpoint not yet ready": gateOver(t, webhookSlice(&notReady)),
	}
	for name, g := range cases {
		if opensWithin(g, 200*time.Millisecond) {
			t.Errorf("%s: the gate opened", name)
		}
	}
}

func TestTheGateOpensOnAReadyWebhookEndpoint(t *testing.T) {
	ready := true
	if !opensWithin(gateOver(t, webhookSlice(&ready)), 2*time.Second) {
		t.Fatal("the gate stayed shut with a ready endpoint")
	}
}

// The pod's readiness is what puts it into the webhook Service's endpoints. A
// pod ready before its webhook server listens is sent admission requests it
// refuses, both at start and while a rollout runs an old replica beside it.
func TestThePodIsNotReadyBeforeTheWebhookServes(t *testing.T) {
	certReady := make(chan struct{})
	serving := errors.New("webhook server has not started yet")
	check := webhookReadyCheck(certReady, func() healthz.Checker {
		return func(*http.Request) error { return serving }
	})
	req, _ := http.NewRequest(http.MethodGet, "/readyz", nil)

	if check(req) == nil {
		t.Fatal("ready before the webhook serving certificate was there")
	}
	close(certReady)
	if check(req) == nil {
		t.Fatal("ready while the webhook server was not yet serving")
	}
	serving = nil
	if err := check(req); err != nil {
		t.Fatalf("not ready once the webhook served: %v", err)
	}
}

// The ordering between the operator's own admission webhooks and its
// controllers. Lives in cmd because it is wiring: which runnables start when,
// and what the pod's readiness reports.
//
// The webhook server starts only once its serving certificate has been
// provisioned (internal/webhook.SetupWebhookCertificate), which on a fresh
// install takes tens of seconds. A controller that started with the manager
// wrote objects in that window, the API server sent the admission request to a
// pod that was not listening, and every refusal grew the controller's requeue
// backoff: on lab-talos, 17 refusals left the first discovery run waiting about
// 11 minutes for its next attempt (2026-10-23).

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// webhookGate opens once the webhook Service has an endpoint ready, which is
// the condition under which the API server can deliver an admission request.
//
// It looks at the Service's endpoints rather than at this process's own
// listener. Serving locally is not enough: the pod enters the endpoints only
// once its readiness passes (webhookReadyCheck), a kubelet probe period later.
// And during a rollout another replica may already be serving, in which case
// there is nothing to wait for.
type webhookGate struct {
	reader    client.Reader
	namespace string
	service   string
	interval  time.Duration
	log       logr.Logger
	open      chan struct{}
}

func newWebhookGate(reader client.Reader, namespace, service string, log logr.Logger) *webhookGate {
	return &webhookGate{reader: reader, namespace: namespace, service: service,
		interval: time.Second, log: log, open: make(chan struct{})}
}

// Start implements manager.Runnable. It polls until an endpoint is ready, closes
// the gate, and returns.
func (g *webhookGate) Start(ctx context.Context) error {
	g.log.Info("controllers wait until the admission webhook can be reached",
		"service", g.service, "namespace", g.namespace)
	for {
		ready, err := g.endpointReady(ctx)
		if err != nil {
			g.log.Info("could not read the webhook Service's endpoints; trying again", "error", err.Error())
		} else if ready {
			g.log.Info("the admission webhook can be reached; starting the controllers")
			close(g.open)
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(g.interval):
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. Every replica
// runs the gate: a follower's controllers wait behind it as well, so that it
// is open by the time the follower is elected.
func (g *webhookGate) NeedLeaderElection() bool { return false }

// endpointReady reports whether any EndpointSlice of the Service has an
// endpoint ready. A nil Ready condition counts as ready, as the API defines it.
func (g *webhookGate) endpointReady(ctx context.Context) (bool, error) {
	var slices discoveryv1.EndpointSliceList
	if err := g.reader.List(ctx, &slices, client.InNamespace(g.namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: g.service}); err != nil {
		return false, err
	}
	for _, s := range slices.Items {
		for _, e := range s.Endpoints {
			if e.Conditions.Ready == nil || *e.Conditions.Ready {
				return true, nil
			}
		}
	}
	return false, nil
}

// gatedManager is the manager the controllers are registered with. Every
// runnable added through it waits for gate before it starts. Everything else
// goes to the manager it wraps, so field indexes and the shared cache are
// registered as before.
type gatedManager struct {
	manager.Manager
	gate <-chan struct{}
}

// Add implements manager.Manager.
func (m gatedManager) Add(r manager.Runnable) error {
	return m.Manager.Add(&gatedRunnable{runnable: r, gate: m.gate})
}

// gatedRunnable starts runnable once gate opens.
type gatedRunnable struct {
	runnable manager.Runnable
	gate     <-chan struct{}
}

// Start implements manager.Runnable.
func (r *gatedRunnable) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-r.gate:
	}
	return r.runnable.Start(ctx)
}

// NeedLeaderElection implements manager.LeaderElectionRunnable, keeping the
// group the manager puts the runnable in. A runnable that does not say is
// leader-elected, which is also what the manager assumes of it.
func (r *gatedRunnable) NeedLeaderElection() bool {
	if le, ok := r.runnable.(manager.LeaderElectionRunnable); ok {
		return le.NeedLeaderElection()
	}
	return true
}

// webhookReadyCheck reports the pod ready only while the webhook server
// serves. certReady closes once the serving certificate is on disk. Until
// then the server has not been added to the manager, and asking for it would
// start it with no certificate. started is the server's own StartedChecker,
// fetched only after that.
func webhookReadyCheck(certReady <-chan struct{}, started func() healthz.Checker) healthz.Checker {
	return func(req *http.Request) error {
		select {
		case <-certReady:
		default:
			return errors.New("the admission webhook's serving certificate is not there yet")
		}
		if err := started()(req); err != nil {
			return fmt.Errorf("the admission webhook is not serving yet: %w", err)
		}
		return nil
	}
}

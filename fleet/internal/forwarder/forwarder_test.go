package forwarder

// The agent's start-up steps around the link library: it publishes the CA its
// site's peers trust it by, and it waits for the hub to publish the relay's
// endpoint rather than failing while the hub has not yet.

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/fleet/internal/sitelink"
)

func caOf(t *testing.T, cs *fake.Clientset) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps("agent-ns").Get(context.Background(), sitelink.LocalCAConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the published CA: %v", err)
	}
	return cm.Data[sitelink.CAKey]
}

// The CA is published, and replaced when the agent restarts with a new one.
func TestTheAgentPublishesItsCAForItsSitesPeers(t *testing.T) {
	cs := fake.NewClientset()
	if err := publishLocalCA(context.Background(), cs, "agent-ns", []byte("first")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := caOf(t, cs); got != "first" {
		t.Errorf("published CA = %q, want first", got)
	}
	if err := publishLocalCA(context.Background(), cs, "agent-ns", []byte("second")); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if got := caOf(t, cs); got != "second" {
		t.Errorf("republished CA = %q, want second", got)
	}
}

// The agent waits for the hub's endpoint and returns it once published.
func TestTheAgentWaitsForTheRelayEndpoint(t *testing.T) {
	cs := fake.NewClientset()
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = cs.CoreV1().ConfigMaps("site-a").Create(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: sitelink.EndpointConfigMap, Namespace: "site-a"},
			Data:       map[string]string{sitelink.AddressKey: "hub:30443", sitelink.CAKey: "PEM"},
		}, metav1.CreateOptions{})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, err := awaitEndpoint(ctx, cs, "site-a", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("awaitEndpoint: %v", err)
	}
	if ep.Address != "hub:30443" {
		t.Errorf("endpoint address = %q", ep.Address)
	}
}

// An agent whose context ends while it waits returns instead of waiting on.
func TestTheAgentStopsWaitingWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := awaitEndpoint(ctx, fake.NewClientset(), "site-a", 20*time.Millisecond); err == nil {
		t.Error("awaitEndpoint returned an endpoint the hub never published")
	}
}

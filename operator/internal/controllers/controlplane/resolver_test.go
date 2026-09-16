// The endpoint resolver: what the rest of the operator reads the control
// plane's address from.
//
// The behavior that matters is the empty answer. A resolver that returned a
// wrong address on a control plane it could not read would point every caller at
// nothing; returning empty leaves each caller on the default it already had,
// which is what makes adopting this additive.

package controlplane

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A published endpoint is what callers get. This is the whole point of the
// field: a remote control plane is somewhere the environment variable does
// not name.
func TestTheResolverAnswersWithThePublishedEndpoint(t *testing.T) {
	const endpoint = "https://sb-control.example.com:5000"

	cp := managedControlPlane(endpoint)
	cp.Status.Endpoint = endpoint

	resolve := NewEndpointResolver(newClient(t, cp), testNamespace)

	if got := resolve(context.Background()); got != endpoint {
		t.Errorf("resolved %q, want %q", got, endpoint)
	}
}

// A control plane that has published nothing yet resolves to nothing, and the
// caller keeps its own default rather than being pointed at an empty address.
func TestAControlPlaneWithNoEndpointResolvesToNothing(t *testing.T) {
	cp := localControlPlane()

	resolve := NewEndpointResolver(newClient(t, cp), testNamespace)

	if got := resolve(context.Background()); got != "" {
		t.Errorf("resolved %q, want nothing published", got)
	}
}

// A singleton that cannot be read resolves to nothing rather than to an error.
// Every control-plane call in the operator goes through this, so failing here
// would fail all of them while the object was briefly unreadable.
func TestAnAbsentControlPlaneResolvesToNothing(t *testing.T) {
	resolve := NewEndpointResolver(newClient(t), testNamespace)

	if got := resolve(context.Background()); got != "" {
		t.Errorf("resolved %q against a cluster with no ControlPlane", got)
	}
}

// Only the singleton answers. A ControlPlane under another name is one the
// reconciler ignores, so reading an endpoint off it would send every caller
// somewhere nothing reconciles.
func TestOnlyTheSingletonAnswers(t *testing.T) {
	other := localControlPlane()
	other.Name = "a-second-one"
	other.Status.Endpoint = "https://not-the-singleton.example.com:5000"

	resolve := NewEndpointResolver(newClient(t, other), testNamespace)

	if got := resolve(context.Background()); got != "" {
		t.Errorf("resolved %q from an object the reconciler ignores", got)
	}
}

// A change to the published endpoint reaches callers, which is the property that
// makes this a resolver rather than a value injected at startup.
func TestAChangedEndpointReachesTheNextCaller(t *testing.T) {
	ctx := context.Background()

	cp := managedControlPlane("https://first.example.com:5000")
	cp.Status.Endpoint = "https://first.example.com:5000"
	c := newClient(t, cp)

	resolve := NewEndpointResolver(c, testNamespace)
	if got := resolve(ctx); got != "https://first.example.com:5000" {
		t.Fatalf("resolved %q before the change", got)
	}

	var current simplyblockv1alpha2.ControlPlane
	key := client.ObjectKey{Name: SingletonName, Namespace: testNamespace}
	if err := c.Get(ctx, key, &current); err != nil {
		t.Fatalf("read the control plane: %v", err)
	}
	current.Status.Endpoint = "https://second.example.com:5000"
	if err := c.Status().Update(ctx, &current); err != nil {
		t.Fatalf("publish the new endpoint: %v", err)
	}

	// The resolver caches for a short interval, so a fresh one stands in for the
	// interval passing. What is under test is that the answer comes from the
	// object rather than from anything captured at construction.
	resolve = NewEndpointResolver(c, testNamespace)
	if got := resolve(ctx); got != "https://second.example.com:5000" {
		t.Errorf("resolved %q, want the endpoint the object now publishes", got)
	}
}

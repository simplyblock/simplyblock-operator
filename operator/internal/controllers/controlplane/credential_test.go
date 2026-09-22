// The credential resolver: what the rest of the operator authenticates a call
// to a managed control plane with.
//
// It follows NewEndpointResolver's shape deliberately: a ControlPlane or
// Secret this reads cannot answer right now is the same, to every caller, as
// one that names no credential, and both mean "fall back to whatever this
// caller already had." Nothing here is required to reach a control plane the
// deployment itself installed, which is what makes it additive.

package controlplane

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func credentialSecret(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-token", Namespace: testNamespace},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

// A managed control plane naming a credential resolves to it, which is what
// lets a call carry the token the remote control plane's admin auth expects.
func TestTheCredentialResolverAnswersWithTheManagedToken(t *testing.T) {
	cp := managedControlPlane("https://sb-control.example.com:5000")
	secret := credentialSecret("a-bearer-token")

	resolve := NewCredentialResolver(newClient(t, cp, secret), testNamespace)

	token, ok := resolve(context.Background())
	if !ok || token != "a-bearer-token" {
		t.Errorf("resolved (%q, %v), want (%q, true)", token, ok, "a-bearer-token")
	}
}

// A local control plane names no managed credential at all, so the resolver
// says so rather than inventing one.
func TestALocalControlPlaneResolvesToNoCredential(t *testing.T) {
	cp := localControlPlane()

	resolve := NewCredentialResolver(newClient(t, cp), testNamespace)

	if token, ok := resolve(context.Background()); ok {
		t.Errorf("resolved (%q, true) from a local control plane, want no credential", token)
	}
}

// A managed control plane that names no credentialsSecretRef is the in-cluster
// case the chart writes when it installs the control plane itself -- no token
// to carry.
func TestAManagedControlPlaneWithNoCredentialsRefResolvesToNoCredential(t *testing.T) {
	cp := managedControlPlane("https://sb-control.example.com:5000")
	cp.Spec.Source.Managed.CredentialsSecretRef = nil

	resolve := NewCredentialResolver(newClient(t, cp), testNamespace)

	if token, ok := resolve(context.Background()); ok {
		t.Errorf("resolved (%q, true) with no ref set, want no credential", token)
	}
}

// A singleton that cannot be read resolves to no credential rather than to an
// error, for the same reason the endpoint resolver does: an unreadable object
// is a transient miss, not a failed call.
func TestAnAbsentControlPlaneResolvesToNoCredential(t *testing.T) {
	resolve := NewCredentialResolver(newClient(t), testNamespace)

	if token, ok := resolve(context.Background()); ok {
		t.Errorf("resolved (%q, true) against a cluster with no ControlPlane", token)
	}
}

// A Secret the ref names but that does not exist is the same as no credential
// to the caller -- there is nothing to authenticate with either way.
func TestAMissingCredentialsSecretResolvesToNoCredential(t *testing.T) {
	cp := managedControlPlane("https://sb-control.example.com:5000")

	resolve := NewCredentialResolver(newClient(t, cp), testNamespace)

	if token, ok := resolve(context.Background()); ok {
		t.Errorf("resolved (%q, true) with the named Secret absent", token)
	}
}

// A change to the credential reaches the next caller, which is the property
// that makes this a resolver rather than a value captured once at startup.
func TestAChangedCredentialReachesTheNextCaller(t *testing.T) {
	ctx := context.Background()

	cp := managedControlPlane("https://sb-control.example.com:5000")
	secret := credentialSecret("first-token")
	c := newClient(t, cp, secret)

	resolve := NewCredentialResolver(c, testNamespace)
	if token, ok := resolve(ctx); !ok || token != "first-token" {
		t.Fatalf("resolved (%q, %v) before the change", token, ok)
	}

	var current corev1.Secret
	key := client.ObjectKey{Name: "cp-token", Namespace: testNamespace}
	if err := c.Get(ctx, key, &current); err != nil {
		t.Fatalf("read the secret: %v", err)
	}
	current.Data["token"] = []byte("second-token")
	if err := c.Update(ctx, &current); err != nil {
		t.Fatalf("publish the new token: %v", err)
	}

	// The resolver caches for a short interval, so a fresh one stands in for the
	// interval passing, exactly as TestAChangedEndpointReachesTheNextCaller does.
	resolve = NewCredentialResolver(c, testNamespace)
	if token, ok := resolve(ctx); !ok || token != "second-token" {
		t.Errorf("resolved (%q, %v), want the token the Secret now carries", token, ok)
	}
}

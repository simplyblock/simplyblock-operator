package sitelink

// The pieces the relay and a site's agent share: the hub token a site's agent
// links with, the relay endpoint it reads from its namespace on the hub, and
// the self-signed serving certificate either end can present.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// tokenClient answers TokenRequests for the site link's ServiceAccount with a
// new token each time, valid for lifetime, and counts them.
func tokenClient(lifetime time.Duration, requests *int) *fake.Clientset {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok || action.GetSubresource() != "token" {
			return false, nil, nil
		}
		*requests++
		req := create.GetObject().(*authenticationv1.TokenRequest).DeepCopy()
		req.Status = authenticationv1.TokenRequestStatus{
			Token:               "token-" + string(rune('0'+*requests)),
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(lifetime)),
		}
		return true, req, nil
	})
	return cs
}

// A token is requested once and reused while most of its lifetime remains.
func TestTheHubTokenIsReusedWhileItIsFresh(t *testing.T) {
	requests := 0
	source := HubToken(tokenClient(time.Hour, &requests), "site-a")
	first, err := source(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	second, err := source(context.Background())
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if first != second || requests != 1 {
		t.Errorf("tokens %q then %q after %d requests; want one token reused", first, second, requests)
	}
}

// A token near its expiry is replaced before it lapses.
func TestTheHubTokenIsRenewedBeforeItExpires(t *testing.T) {
	requests := 0
	source := HubToken(tokenClient(time.Second, &requests), "site-a")
	first, _ := source(context.Background())
	second, _ := source(context.Background())
	if first == second || requests != 2 {
		t.Errorf("tokens %q then %q after %d requests; want a new token for one about to expire", first, second, requests)
	}
}

// The relay endpoint is the address and CA the hub publishes in the site's
// namespace; either missing is an error rather than a guess.
func TestTheRelayEndpointIsReadFromTheSitesNamespace(t *testing.T) {
	cs := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: EndpointConfigMap, Namespace: "site-a"},
		Data:       map[string]string{"address": "hub.example:30443", "ca.crt": "PEM"},
	})
	ep, err := ReadEndpoint(context.Background(), cs, "site-a")
	if err != nil {
		t.Fatalf("ReadEndpoint: %v", err)
	}
	if ep.Address != "hub.example:30443" || string(ep.CA) != "PEM" {
		t.Errorf("endpoint = %+v", ep)
	}
	if _, err := ReadEndpoint(context.Background(), fake.NewClientset(), "site-b"); err == nil {
		t.Error("a site with no published endpoint read as one")
	}
}

// A self-signed serving certificate verifies against the CA it publishes, for
// the names it was made for.
func TestASelfSignedCertificateVerifiesForItsNames(t *testing.T) {
	cert, caPEM, err := SelfSigned([]string{"relay.example"}, []string{"192.0.2.10"})
	if err != nil {
		t.Fatalf("SelfSigned: %v", err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("the published CA is not PEM")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	for _, name := range []string{"relay.example", "192.0.2.10"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("verify for %s: %v", name, err)
		}
	}
	_ = cert.PrivateKey
}

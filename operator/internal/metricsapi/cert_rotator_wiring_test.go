// Tests that the aggregated metrics API's certificate rotator can coexist with
// the webhook's, which is a property of the two packages together rather than of
// either one, and so has no home inside either implementation's own tests.
//
// It lives here, on the metrics API's side, because the metrics API is the
// newcomer: the webhook's rotator was registered first and its name is the one
// already in use, so this side is the one that has to stay out of the way.
//
// The case runs in the external test package because it imports
// internal/webhook, which the metrics API itself does not.

package metricsapi_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/simplyblock/simplyblock-operator/internal/metricsapi"
	internalwebhook "github.com/simplyblock/simplyblock-operator/internal/webhook"
)

const (
	wiringNamespace = "simplyblock-operator-system"
	// Any provider that is not cert-manager selects the self-signed rotator,
	// which is the mode that registers a controller. It is also the default the
	// operator ships with, so it is the mode the collision happens in.
	selfSignedProvider = "openshift"
)

// newRotatorTestManager builds a manager that can accept runnables and
// controllers without an API server behind it.
//
// Registering a rotator performs no I/O: it constructs a cache and a controller,
// both of which stay idle until the manager starts, and the manager is never
// started here. The one call that would reach the network is the operator's own
// pre-create of the certificate Secret, so the client is a fake one.
func newRotatorTestManager(t *testing.T) ctrl.Manager {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	mgr, err := manager.New(&rest.Config{Host: "127.0.0.1:1"}, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		NewClient: func(*rest.Config, client.Options) (client.Client, error) {
			return fakeClient, nil
		},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	return mgr
}

// Regression: 2026-09-02-metrics-apiserver-cert-rotator-name — the aggregated
// metrics API registered its certificate rotator under cert-controller's default
// controller name, which the webhook's rotator already held. controller-runtime
// rejects a duplicate controller name, so installing the metrics API failed at
// startup, the manager exited, and the operator crash-looped: no reconcilers ran
// at all, and the e2e suite saw a controller pod that never became ready.
//
// The two are registered in the order cmd/main.go registers them, because the
// second one is the one that fails.
func TestBothOfTheOperatorsCertRotatorsCanBeRegistered(t *testing.T) {
	mgr := newRotatorTestManager(t)

	if _, err := internalwebhook.SetupWebhookCertificate(mgr, wiringNamespace, selfSignedProvider); err != nil {
		t.Fatalf("set up the webhook certificate: %v", err)
	}

	if _, err := metricsapi.SetupCertificate(mgr, wiringNamespace, metricsapi.CertDir); err != nil {
		t.Fatalf("set up the metrics apiserver certificate alongside the webhook's: %v", err)
	}
}

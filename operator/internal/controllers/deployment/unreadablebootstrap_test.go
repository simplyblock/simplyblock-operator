// What the two readers of the installation's configuration do when they cannot
// read it.
//
// Not what they do when it is absent, which is settled elsewhere and means the
// operator's own behavior. This is the case where the answer exists and the
// operator did not get it, and both readers used to treat the two alike.
//
// The consequences differ and neither is recoverable by waiting. A startup check
// that assumes the defaults raises the run a managed installation wrote
// `enabled: false` to suppress, and that run creates an OperatorOps and a probe
// Job on every worker in the fleet. A draft written without the installation's
// seed keeps the numbers this run found, and the next reconcile finds the
// document already there and leaves it alone, so the API recovering changes
// nothing.

package deployment

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/bootstrap"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// unreadableConfig is the API answering the bootstrap ConfigMap read the way it
// does when the caller may not look at it. Every other read succeeds, because
// the failure under test is this one.
func unreadableConfig(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, simplyblockv1alpha2.AddToScheme)
	built := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if key.Name == bootstrap.ConfigMapName {
					return apierrors.NewForbidden(
						schema.GroupResource{Resource: "configmaps"},
						key.Name, errors.New("no permission"))
				}
				return built.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
}

// The startup check raises nothing when it could not read the configuration.
//
// Raising the operator's own run instead is the failure this replaces: on a
// managed installation the answer it could not read says to raise none, and the
// run it would raise reaches every worker.
func TestTheStartupCheckRaisesNoRunItCouldNotAskAbout(t *testing.T) {
	discovery := &InitialDiscovery{
		Client:    unreadableConfig(t, labeledWorker("worker-1", nil)),
		Namespace: theNamespace,
	}

	if err := discovery.Start(context.Background()); err == nil {
		t.Error("the check reported success without knowing whether it should have run")
	}
	if raised(t, discovery) {
		t.Error("the check raised the run it could not establish was wanted, which is " +
			"an OperatorOps and a probe Job per worker on an installation that declined both")
	}
}

// An absent configuration is still the operator's own behavior. The startup
// check has to keep telling the two apart, or this fix trades one silent wrong
// answer for another.
func TestTheStartupCheckStillRaisesTheRunWhenNothingWasStated(t *testing.T) {
	discovery := discoveryFor(t, labeledWorker("worker-1", nil))

	if err := discovery.Start(context.Background()); err != nil {
		t.Fatalf("an installation that stated nothing reported %v", err)
	}
	if !raised(t, discovery) {
		t.Error("an installation that stated nothing raised no run, and absent means " +
			"the operator's own behavior")
	}
}

// The Degraded → Provisioning edge (design-objectstore.md §6), proved against
// a fake client: a Degraded ObjectStore returns to Provisioning once the
// object it was waiting on appears, and stays Degraded while nothing changed.

package objectstore

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// degradedStore is an ObjectStore Degraded at degradedAt for conditionType,
// with spec.envFrom naming the Secret gateway-env.
func degradedStore(degradedAt time.Time, conditionType, reason string, mutate ...func(*simplyblockv1alpha2.ObjectStore)) *simplyblockv1alpha2.ObjectStore {
	return testStore(append([]func(*simplyblockv1alpha2.ObjectStore){func(o *simplyblockv1alpha2.ObjectStore) {
		o.Spec.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "gateway-env"}},
		}}
		o.Status.Phase = osPhaseDegraded
		o.Status.Conditions = []metav1.Condition{{
			Type: conditionType, Status: metav1.ConditionFalse, Reason: reason,
			LastTransitionTime: metav1.NewTime(degradedAt),
		}}
	}}, mutate...)...)
}

// envSecret is the Secret degradedStore's envFrom names, created at createdAt.
func envSecret(createdAt time.Time) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "gateway-env", Namespace: testObjectStoreNamespace, CreationTimestamp: metav1.NewTime(createdAt),
	}}
}

// U-20: a Degraded ObjectStore whose envFrom Secret was created after it
// degraded returns to Provisioning with a fresh deadline.
//
// Regression: 2026-11-23-objectstore-degraded-never-recovers. Degraded had no
// exit, so creating the Secret an ObjectStore's envFrom named left it
// Degraded until it was deleted and recreated (CI run 45100042373,
// S3MissingEnvFromSecretK8s).
func TestDegradedObjectStoreRecoversWhenItsEnvFromSecretAppears(t *testing.T) {
	degradedAt := time.Now().Add(-time.Minute)
	store := degradedStore(degradedAt, conditionDeploymentAvailable, "DeploymentNotAvailable")
	cli := newTestClient(t, store, availableDeployment(store, false, 0), envSecret(degradedAt.Add(30*time.Second)))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseProvisioning {
		t.Fatalf("phase = %q, want Provisioning", got.Status.Phase)
	}
	if got.Status.PhaseDeadline == nil || !got.Status.PhaseDeadline.After(time.Now()) {
		t.Errorf("phaseDeadline = %v, want a fresh deadline in the future", got.Status.PhaseDeadline)
	}
}

// U-21: a Degraded ObjectStore whose referenced objects have not changed
// since it degraded stays Degraded, so a refusal is not retried on every
// event that happens to touch the ObjectStore.
func TestDegradedObjectStoreStaysDegradedWhileNothingChanged(t *testing.T) {
	degradedAt := time.Now().Add(-time.Minute)
	store := degradedStore(degradedAt, conditionDeploymentAvailable, "DeploymentNotAvailable")
	cli := newTestClient(t, store, availableDeployment(store, false, 0), envSecret(degradedAt.Add(-time.Hour)))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
}

// U-22: an ObjectStore Degraded on its certificate returns to Provisioning
// once the Certificate reports Ready, for example, after the missing Issuer
// was created.
func TestDegradedObjectStoreRecoversWhenItsCertificateIsReady(t *testing.T) {
	degradedAt := time.Now().Add(-time.Minute)
	store := degradedStore(degradedAt, conditionCertificateReady, "CertificateNotReady", func(o *simplyblockv1alpha2.ObjectStore) {
		o.Spec.TLSProvider = utils.TLSProviderCertManager
	})
	cli := newTestClient(t, store, availableDeployment(store, true, 1), readyCertificate(store, true), envSecret(degradedAt.Add(-time.Hour)))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
}

// A Degraded ObjectStore whose spec changed since it degraded returns to
// Provisioning: the edit may be the fix, such as a corrected image, and
// before this only deleting and recreating the object let it take effect.
func TestDegradedObjectStoreRecoversWhenItsSpecChanges(t *testing.T) {
	degradedAt := time.Now().Add(-time.Minute)
	store := degradedStore(degradedAt, conditionDeploymentAvailable, "DeploymentNotAvailable",
		func(o *simplyblockv1alpha2.ObjectStore) {
			o.Generation = 3
			o.Status.ObservedGeneration = 2
		})
	cli := newTestClient(t, store, availableDeployment(store, false, 0), envSecret(degradedAt.Add(-time.Hour)))

	got := reconcileStore(t, cli, store)

	if got.Status.Phase != osPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
}

// U-23: an event on a Secret or ConfigMap enqueues every ObjectStore in its
// namespace whose envFrom names it, and no other.
func TestEnvFromReferenceEnqueuesTheObjectStoresNamingIt(t *testing.T) {
	naming := degradedStore(time.Now(), conditionDeploymentAvailable, "DeploymentNotAvailable")
	other := testStore(func(o *simplyblockv1alpha2.ObjectStore) { o.Name = "other-store" })
	cli := newTestClient(t, naming, other)
	r := &ObjectStoreReconciler{Client: cli, Scheme: cli.Scheme()}

	requests := r.storesReferencingEnvFrom(context.Background(), envSecret(time.Now()))

	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(naming) {
		t.Errorf("enqueued %v, want only %s", requests, client.ObjectKeyFromObject(naming))
	}
}

// Switching spec.tlsProvider on a running ObjectStore (design-objectstore.md
// §5): the previous issuer's state is removed, so only one issuer writes the
// serving Secret, and the store waits in Provisioning for the new issuer's
// certificate. Proved against a fake client; the issuers' own writes are
// fixture data.

package objectstore

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// servingSecret is the serving Secret as an issuer writes it, carrying that
// issuer's own annotations: none when issuer is empty.
func servingSecret(store *simplyblockv1alpha2.ObjectStore, issuer string) *corev1.Secret {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: tlsSecretName(store.Name), Namespace: store.Namespace,
	}}
	switch issuer {
	case utils.TLSProviderCertManager:
		secret.Annotations = map[string]string{certManagerCertificateNameAnnotation: store.Name}
	case utils.TLSProviderOpenShift:
		secret.Annotations = map[string]string{serviceCAOriginatingServiceAnnotation: store.Name}
	}
	return secret
}

func objectGone(t *testing.T, cli client.Client, obj client.Object) bool {
	t.Helper()
	err := cli.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("reading %s: %v", obj.GetName(), err)
	}
	return apierrors.IsNotFound(err)
}

// issuerObjects is a Ready store with its Deployment and, for cert-manager,
// its Ready Certificate: what the store holds while provider stays selected.
func issuerObjects(store *simplyblockv1alpha2.ObjectStore, provider string) []client.Object {
	objects := []client.Object{store, availableDeployment(store, true, 1)}
	if provider == utils.TLSProviderCertManager {
		objects = append(objects, readyCertificate(store, true))
	}
	return objects
}

func readyStore(provider string) *simplyblockv1alpha2.ObjectStore {
	return testStore(func(o *simplyblockv1alpha2.ObjectStore) {
		o.Status.Phase = osPhaseReady
		o.Spec.TLSProvider = provider
	})
}

// U-24: a Ready store switched from cert-manager to OpenShift deletes its
// Certificate and the Secret cert-manager wrote, and waits in Provisioning
// for the service CA's certificate.
func TestSwitchingToOpenShiftRetiresTheCertificate(t *testing.T) {
	store := readyStore(utils.TLSProviderOpenShift)
	cert := readyCertificate(store, true)
	secret := servingSecret(store, utils.TLSProviderCertManager)
	cli := newTestClient(t, store, availableDeployment(store, true, 1), cert, secret)

	got := reconcileStore(t, cli, store)

	if !objectGone(t, cli, cert) {
		t.Error("the cert-manager Certificate is kept, so cert-manager keeps writing the Secret")
	}
	if !objectGone(t, cli, &corev1.Secret{ObjectMeta: secret.ObjectMeta}) {
		t.Error("the Secret cert-manager wrote is kept, so the service CA refuses to issue into it")
	}
	if got.Status.Phase != osPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning until the new certificate is issued", got.Status.Phase)
	}
}

// U-25: a Ready store switched from OpenShift to cert-manager deletes the
// Secret the service CA wrote, creates the Certificate, and waits in
// Provisioning for it.
func TestSwitchingToCertManagerRetiresTheServiceCASecret(t *testing.T) {
	store := readyStore(utils.TLSProviderCertManager)
	secret := servingSecret(store, utils.TLSProviderOpenShift)
	cli := newTestClient(t, store, availableDeployment(store, true, 1), secret)

	got := reconcileStore(t, cli, store)

	if !objectGone(t, cli, &corev1.Secret{ObjectMeta: secret.ObjectMeta}) {
		t.Error("the Secret the service CA wrote is kept")
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(store.Name)
	cert.SetNamespace(store.Namespace)
	if objectGone(t, cli, cert) {
		t.Error("no Certificate was created")
	}
	if got.Status.Phase != osPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning until the new certificate is issued", got.Status.Phase)
	}
}

// U-26: a Ready store whose Secret its own provider wrote stays Ready and
// keeps it: nothing is retired without a switch.
func TestAnUnchangedProviderRetiresNothing(t *testing.T) {
	for _, provider := range []string{utils.TLSProviderOpenShift, utils.TLSProviderCertManager} {
		t.Run(provider, func(t *testing.T) {
			store := readyStore(provider)
			secret := servingSecret(store, provider)
			cli := newTestClient(t, append(issuerObjects(store, provider), secret)...)

			got := reconcileStore(t, cli, store)

			if objectGone(t, cli, &corev1.Secret{ObjectMeta: secret.ObjectMeta}) {
				t.Error("the current provider's Secret was deleted")
			}
			if got.Status.Phase != osPhaseReady {
				t.Errorf("phase = %q, want Ready", got.Status.Phase)
			}
		})
	}
}

// U-27: a Secret neither issuer wrote is never deleted, whichever provider
// is selected.
func TestASecretNoIssuerWroteIsKept(t *testing.T) {
	for _, provider := range []string{utils.TLSProviderOpenShift, utils.TLSProviderCertManager} {
		t.Run(provider, func(t *testing.T) {
			store := readyStore(provider)
			secret := servingSecret(store, "")
			cli := newTestClient(t, append(issuerObjects(store, provider), secret)...)

			reconcileStore(t, cli, store)

			if objectGone(t, cli, &corev1.Secret{ObjectMeta: secret.ObjectMeta}) {
				t.Error("a Secret no issuer wrote was deleted")
			}
		})
	}
}

// U-28: with the OpenShift provider, Provisioning waits for the service CA
// to write the Secret, and reaches Ready once it has.
func TestOpenShiftProvisioningWaitsForTheServiceCASecret(t *testing.T) {
	store := testStore(func(o *simplyblockv1alpha2.ObjectStore) {
		o.Status.Phase = osPhaseProvisioning
		o.Spec.TLSProvider = utils.TLSProviderOpenShift
	})
	cli := newTestClient(t, store, availableDeployment(store, true, 1))

	if got := reconcileStore(t, cli, store); got.Status.Phase != osPhaseProvisioning {
		t.Fatalf("phase = %q with no Secret yet, want Provisioning", got.Status.Phase)
	}

	if err := cli.Create(context.Background(), servingSecret(store, utils.TLSProviderOpenShift)); err != nil {
		t.Fatalf("writing the service CA's Secret: %v", err)
	}
	if got := reconcileStore(t, cli, store); got.Status.Phase != osPhaseReady {
		t.Errorf("phase = %q once the Secret exists, want Ready", got.Status.Phase)
	}
}

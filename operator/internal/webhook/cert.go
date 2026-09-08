package webhook

import (
	"context"
	"fmt"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// RBAC needed to provision the webhook serving certificate at runtime.
//
// Self-signed mode (cert-controller): manage the webhook-server-cert Secret and
// inject the CA bundle into the MutatingWebhookConfiguration.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;update;patch
//
// A conversion webhook's CA bundle lives in the CRD rather than in a webhook
// configuration, so the converted kinds' CRDs are written too. The grant is split
// in two, because the write half is the dangerous one and the read half cannot be
// narrowed.
//
// Writing is restricted to the CRDs that actually declare a converted kind.
// Patching an arbitrary CRD is close to cluster-admin in effect — a rewritten
// schema or conversion stanza reaches every object of that kind — so the blast
// radius of a compromised operator is worth bounding here even though the
// resourceNames list has to be kept beside the one in config/crd/converted-kinds.txt.
// TestManagerRoleNamesOnlyTheConvertedCRDs is what keeps the two in step. Neither
// create nor delete appears: the operator does not install its own CRDs and must
// not be able to.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;update;patch,resourceNames=controlplanes.storage.simplyblock.io;storagebackups.storage.simplyblock.io;storageclusterops.storage.simplyblock.io;storagenodeops.storage.simplyblock.io
//
// Reading stays cluster-wide because it cannot be otherwise: cert-controller's
// rotator establishes an informer on CustomResourceDefinition to re-inject the CA
// when one changes, and Kubernetes RBAC ignores resourceNames for list and watch.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=list;watch
//
// cert-manager mode: create a Certificate whose Secret cert-manager fills in.
// (Also declared on the StorageNodeSet reconciler for storage-node TLS.)
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// SetupWebhookCertificate provisions the mutating-webhook serving certificate at
// runtime and returns a channel that is closed once the certificate is present on
// disk (utils.WebhookCertDir) and the CA bundle has been injected into the webhook
// configuration. Callers must defer registering webhook handlers until the channel
// is closed, so controller-runtime's webhook server only starts its certwatcher
// after the cert files exist (certwatcher.New fails on a missing file).
//
// The source of the certificate is selected by SB_TLS_PROVIDER (via tlsProvider):
//   - cert-manager: the operator creates a cert-manager Certificate and consumes
//     the Secret it issues (see certManagerProvisioner).
//   - anything else (default OpenShift): a self-signed cert is generated and
//     rotated by open-policy-agent/cert-controller.
func SetupWebhookCertificate(mgr ctrl.Manager, namespace, tlsProvider string) (chan struct{}, error) {
	ready := make(chan struct{})

	// All three kinds of configuration are served by the same webhook server (same
	// DNS name), so they share one serving certificate and CA — only the CA-bundle
	// injection targets differ. A conversion webhook's bundle goes into the CRD
	// rather than into a webhook configuration, and an untrusted conversion webhook
	// makes its kind unreadable rather than merely unvalidated, so every converted
	// kind has to be listed here.
	convertedCRDs := ConvertedKindCRDNames()
	webhooks := make([]rotator.WebhookInfo, 0, 2+len(convertedCRDs))
	webhooks = append(webhooks,
		rotator.WebhookInfo{Name: utils.WebhookConfigurationName, Type: rotator.Mutating},
		rotator.WebhookInfo{Name: utils.WebhookValidatingConfigurationName, Type: rotator.Validating},
	)
	for _, crdName := range convertedCRDs {
		webhooks = append(webhooks, rotator.WebhookInfo{Name: crdName, Type: rotator.CRDConversion})
	}

	// Neither provider's CA injection touches the service reference, and a CRD
	// shipped for the default install names the wrong namespace anywhere else.
	if err := SetupConversionServiceReference(mgr, namespace); err != nil {
		return nil, fmt.Errorf("add conversion service reference correction: %w", err)
	}
	dnsName := fmt.Sprintf("%s.%s.svc", utils.WebhookServiceName, namespace)

	if utils.IsCertManagerTLSProvider(tlsProvider) {
		if err := mgr.Add(&certManagerProvisioner{
			client:    mgr.GetClient(),
			apiReader: mgr.GetAPIReader(),
			namespace: namespace,
			dnsName:   dnsName,
			certDir:   utils.WebhookCertDir,
			ready:     ready,
		}); err != nil {
			return nil, fmt.Errorf("add cert-manager webhook provisioner: %w", err)
		}
		return ready, nil
	}

	// The Cert-Controller Rotator requires an empty secret to exist
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      utils.WebhookServerCertSecret,
		},
	}
	if err := mgr.GetClient().Create(context.Background(), sec); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("pre-create webhook cert secret: %w", err)
	}

	if err := rotator.AddRotator(mgr, &rotator.CertRotator{
		SecretKey:              types.NamespacedName{Namespace: namespace, Name: utils.WebhookServerCertSecret},
		CertDir:                utils.WebhookCertDir,
		CAName:                 "simplyblock-operator-webhook-ca",
		CAOrganization:         "simplyblock.io",
		DNSName:                dnsName,
		ExtraDNSNames:          []string{dnsName + ".cluster.local"},
		IsReady:                ready,
		Webhooks:               webhooks,
		RestartOnSecretRefresh: true,
		RequireLeaderElection:  true,
	}); err != nil {
		return nil, fmt.Errorf("add webhook cert rotator: %w", err)
	}
	return ready, nil
}

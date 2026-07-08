package webhook

import (
	"fmt"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
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

	webhooks := []rotator.WebhookInfo{
		{Name: utils.WebhookConfigurationName, Type: rotator.Mutating},
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
	}); err != nil {
		return nil, fmt.Errorf("add webhook cert rotator: %w", err)
	}
	return ready, nil
}

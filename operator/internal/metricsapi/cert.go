// The aggregated API server's serving certificate, and the CA bundle the
// kube-apiserver needs in order to trust it.
//
// These are one concern rather than two. The kube-apiserver will not proxy to an
// extension API server whose certificate it cannot verify, and the only place it
// looks for that trust is the APIService object's spec.caBundle. So issuing the
// certificate and publishing its CA are the same operation, and doing one
// without the other leaves the APIService permanently `Available=False` with a
// TLS error, which is the single most common way an aggregated API fails to
// come up.
//
// open-policy-agent/cert-controller does both, and the operator already depends
// on it for the webhook (see internal/webhook/cert.go). Its WebhookInfo carries
// an APIService type for exactly this, so the mechanism, the rotation schedule,
// and the failure modes are the ones already running in this process.
//
// It differs from the webhook's rotator in one setting, and the difference is
// deliberate: this one does not require leader election. The webhook's rotator
// runs on the leader because one writer of the Secret is enough. Here every
// replica must have the certificate on disk before it can serve, and a follower
// that never ran the rotator would have no certificate at all, so every replica
// runs it and they converge on the same Secret rather than each minting its own.

package metricsapi

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

// RBAC needed to provision the aggregated API server's serving certificate and
// to publish its CA bundle.
//
// The secrets grant is already held for the webhook certificate; apiservices is
// new, and is the narrowest form of the grant: the CA bundle is written by
// patching the one APIService object this operator owns.
// +kubebuilder:rbac:groups=apiregistration.k8s.io,resources=apiservices,verbs=get;list;watch;update;patch

// SetupCertificate provisions the aggregated API server's serving certificate
// and returns a channel that is closed once it is on disk in certDir and the CA
// bundle has been injected into the APIService.
//
// Callers must wait on that channel before calling NewServer: the listener reads
// the key pair when it is constructed, and a missing file there is a startup
// failure rather than something it retries.
func SetupCertificate(mgr ctrl.Manager, namespace, certDir string) (chan struct{}, error) {
	ready := make(chan struct{})

	// The rotator adopts an existing Secret and fills in an empty one, but it
	// does not create the object, so an empty one is placed first.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      utils.MetricsAPIServerCertName,
		},
	}
	if err := mgr.GetClient().Create(context.Background(), secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("pre-create the metrics apiserver cert secret: %w", err)
	}

	// The certificate is presented to the kube-apiserver, which reaches the
	// server through the Service, so the Service's cluster DNS name is the name
	// that has to be on it.
	dnsName := fmt.Sprintf("%s.%s.svc", utils.MetricsAPIServiceName, namespace)

	if err := rotator.AddRotator(mgr, &rotator.CertRotator{
		SecretKey:      types.NamespacedName{Namespace: namespace, Name: utils.MetricsAPIServerCertName},
		CertDir:        certDir,
		CAName:         "simplyblock-operator-metrics-apiserver-ca",
		CAOrganization: "simplyblock.io",
		DNSName:        dnsName,
		ExtraDNSNames:  []string{dnsName + ".cluster.local"},
		IsReady:        ready,
		Webhooks: []rotator.WebhookInfo{
			{Name: utils.MetricsAPIServiceObject, Type: rotator.APIService},
		},
		RestartOnSecretRefresh: true,
		RequireLeaderElection:  false,
	}); err != nil {
		return nil, fmt.Errorf("add the metrics apiserver cert rotator: %w", err)
	}
	return ready, nil
}

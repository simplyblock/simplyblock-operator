package webhook

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// tlsCertFileName / tlsKeyFileName match controller-runtime's default
	// CertName / KeyName, which the webhook server reads from utils.WebhookCertDir.
	tlsCertFileName = "tls.crt"
	tlsKeyFileName  = "tls.key"
	// caCrtKey is the CA published by cert-manager alongside the serving cert.
	caCrtKey = "ca.crt"

	certSyncInterval = 30 * time.Second
)

// certManagerProvisioner runs when SB_TLS_PROVIDER=cert-manager. It creates a
// cert-manager Certificate for the webhook Service, then materialises the issued
// Secret onto disk (utils.WebhookCertDir) and injects the CA bundle into the
// MutatingWebhookConfiguration — the same responsibilities cert-controller's
// rotator handles in self-signed mode, but sourced from cert-manager. It keeps
// running to pick up cert-manager's periodic rotation of the Secret.
//
// It implements manager.Runnable (started by the manager after caches sync) and
// manager.LeaderElectionRunnable (must run on every replica so each pod has a
// serving cert, regardless of leadership).
type certManagerProvisioner struct {
	client    client.Client
	apiReader client.Reader
	namespace string
	dnsName   string
	// certDir is where the serving cert is written; defaults to utils.WebhookCertDir.
	certDir string

	ready     chan struct{}
	readyOnce sync.Once

	lastCert []byte
	lastKey  []byte
	lastCA   []byte
}

func (p *certManagerProvisioner) NeedLeaderElection() bool { return false }

func (p *certManagerProvisioner) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("webhook-cert-manager-provisioner")

	if err := p.ensureCertificate(ctx); err != nil {
		// Log and continue; the sync loop retries and cert-manager may catch up.
		log.Error(err, "failed to ensure webhook Certificate; will retry")
	}

	ticker := time.NewTicker(certSyncInterval)
	defer ticker.Stop()

	// Run an immediate sync so the webhook can come up as soon as the Secret exists.
	p.syncOnce(ctx, log)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.syncOnce(ctx, log)
		}
	}
}

func (p *certManagerProvisioner) syncOnce(ctx context.Context, log logr.Logger) {
	if err := p.ensureCertificate(ctx); err != nil {
		log.Error(err, "ensuring webhook Certificate")
		return
	}
	if err := p.reconcileCert(ctx); err != nil {
		log.V(1).Info("webhook serving certificate not ready yet", "reason", err.Error())
	}
}

// ensureCertificate creates the cert-manager Certificate for the webhook Service
// if it does not already exist. cert-manager then issues the Secret referenced by
// utils.WebhookServerCertSecret.
func (p *certManagerProvisioner) ensureCertificate(ctx context.Context) error {
	cert := utils.BuildServiceServingCertificate(p.namespace, utils.WebhookServiceName, utils.WebhookServerCertSecret)
	if err := p.client.Create(ctx, cert); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create webhook Certificate: %w", err)
	}
	return nil
}

// reconcileCert loads the issued Secret, writes the serving cert to disk, and
// injects the CA bundle into the webhook configuration. It signals readiness the
// first time both steps succeed.
func (p *certManagerProvisioner) reconcileCert(ctx context.Context) error {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: p.namespace, Name: utils.WebhookServerCertSecret}
	if err := p.apiReader.Get(ctx, key, &secret); err != nil {
		return fmt.Errorf("get serving cert secret: %w", err)
	}

	crt := secret.Data[tlsCertFileName]
	tlsKey := secret.Data[tlsKeyFileName]
	if len(crt) == 0 || len(tlsKey) == 0 {
		return fmt.Errorf("secret %s missing %s/%s", key.Name, tlsCertFileName, tlsKeyFileName)
	}
	// cert-manager publishes the issuing CA in ca.crt; fall back to the leaf cert
	// for self-signed issuers that omit it.
	ca := secret.Data[caCrtKey]
	if len(ca) == 0 {
		ca = crt
	}

	if err := p.writeCert(crt, tlsKey); err != nil {
		return err
	}
	if err := p.injectCABundle(ctx, ca); err != nil {
		return err
	}

	p.readyOnce.Do(func() { close(p.ready) })
	return nil
}

// writeCert writes tls.crt/tls.key into utils.WebhookCertDir when they change.
// controller-runtime's certwatcher picks up the change and reloads automatically.
func (p *certManagerProvisioner) writeCert(crt, key []byte) error {
	if bytes.Equal(crt, p.lastCert) && bytes.Equal(key, p.lastKey) {
		return nil
	}
	if err := os.MkdirAll(p.certDir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(p.certDir, tlsCertFileName), crt, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tlsCertFileName, err)
	}
	if err := os.WriteFile(filepath.Join(p.certDir, tlsKeyFileName), key, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tlsKeyFileName, err)
	}
	p.lastCert, p.lastKey = crt, key
	return nil
}

// injectCABundle sets clientConfig.caBundle on every webhook of both the mutating
// and validating configurations when it changes, so the API server trusts the
// serving cert. Both configurations are served by the same webhook server and
// therefore share one CA.
func (p *certManagerProvisioner) injectCABundle(ctx context.Context, ca []byte) error {
	if bytes.Equal(ca, p.lastCA) {
		return nil
	}

	var mwc admissionregistrationv1.MutatingWebhookConfiguration
	if err := p.apiReader.Get(ctx, types.NamespacedName{Name: utils.WebhookConfigurationName}, &mwc); err != nil {
		return fmt.Errorf("get mutating webhook configuration: %w", err)
	}
	mPatch := client.MergeFrom(mwc.DeepCopy())
	mChanged := false
	for i := range mwc.Webhooks {
		if !bytes.Equal(mwc.Webhooks[i].ClientConfig.CABundle, ca) {
			mwc.Webhooks[i].ClientConfig.CABundle = ca
			mChanged = true
		}
	}
	if mChanged {
		if err := p.client.Patch(ctx, &mwc, mPatch); err != nil {
			return fmt.Errorf("patch mutating webhook caBundle: %w", err)
		}
	}

	var vwc admissionregistrationv1.ValidatingWebhookConfiguration
	if err := p.apiReader.Get(ctx, types.NamespacedName{Name: utils.WebhookValidatingConfigurationName}, &vwc); err != nil {
		return fmt.Errorf("get validating webhook configuration: %w", err)
	}
	vPatch := client.MergeFrom(vwc.DeepCopy())
	vChanged := false
	for i := range vwc.Webhooks {
		if !bytes.Equal(vwc.Webhooks[i].ClientConfig.CABundle, ca) {
			vwc.Webhooks[i].ClientConfig.CABundle = ca
			vChanged = true
		}
	}
	if vChanged {
		if err := p.client.Patch(ctx, &vwc, vPatch); err != nil {
			return fmt.Errorf("patch validating webhook caBundle: %w", err)
		}
	}

	p.lastCA = ca
	return nil
}

// The trust the conversion webhook needs before the manager starts.
//
// This exists because of an ordering the manager cannot be asked to change.
// controller-runtime starts its HTTP servers, then its webhook servers, then
// syncs its caches, and only then runs the Runnables that are not one of those
// (pkg/manager/internal.go). Syncing a cache over a converted kind lists it at
// v1alpha2, which makes the API server convert every stored v1alpha1 object,
// which calls this operator's conversion webhook.
//
// So a CA injected by a Runnable arrives too late by construction: the list fails,
// the cache never syncs, the manager exits, and the injection that would have
// fixed it never runs. The operator crash-loops, and no amount of retrying inside
// it helps, because the retry is on the far side of the sync that is failing.
//
// The fix is to be finished before the manager begins. Nothing here uses the
// manager or its client: it reads and writes through a direct client, and the two
// kinds it touches — Secret and CustomResourceDefinition — are core and
// apiextensions kinds that no conversion webhook stands in front of, so the
// bootstrap can always make progress.
//
// Rotation stays where it was. cert-controller's rotator and the cert-manager
// provisioner keep running under the manager and re-inject whenever the material
// changes; this only guarantees the first pass has already happened.

package webhook

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// caKeyKey is the CA private key in the serving Secret. cert-controller's
	// rotator stores its CA key under this name and refuses a Secret without it,
	// so material written here has to carry it to be adopted rather than replaced.
	caKeyKey = "ca.key"

	// bootstrapCADuration and bootstrapCertDuration are deliberately long. This
	// material exists to get the operator past its first cache sync; the rotator
	// takes ownership afterward and rotates on its own schedule, and an
	// expiry-driven handover during startup is not something to design in.
	bootstrapCADuration   = 10 * 365 * 24 * time.Hour
	bootstrapCertDuration = 365 * 24 * time.Hour

	// certManagerWaitTimeout bounds how long the bootstrap waits for cert-manager
	// to issue. Failing is better than hanging: the pod restarts and tries again,
	// which is visible, whereas a hang looks like a healthy operator doing nothing.
	certManagerWaitTimeout  = 2 * time.Minute
	certManagerPollInterval = 2 * time.Second
)

// trustBootstrapper puts the serving material on disk and the trust on the CRDs,
// synchronously, before any manager exists.
type trustBootstrapper struct {
	client    client.Client
	apiReader client.Reader
	namespace string
	dnsName   string
	certDir   string
	// tlsProvider selects where the serving certificate comes from. Anything other
	// than cert-manager is self-signed, matching utils.IsCertManagerTLSProvider.
	tlsProvider string
}

// BootstrapConversionTrust provisions the webhook's serving certificate and tells
// the API server to trust it, before the manager starts. See this file's opening
// comment for why it cannot be a Runnable.
func BootstrapConversionTrust(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme,
	namespace, tlsProvider string,
) error {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build bootstrap client: %w", err)
	}

	b := &trustBootstrapper{
		client:      c,
		apiReader:   c,
		namespace:   namespace,
		dnsName:     fmt.Sprintf("%s.%s.svc", utils.WebhookServiceName, namespace),
		certDir:     utils.WebhookCertDir,
		tlsProvider: tlsProvider,
	}
	return b.run(ctx)
}

// run provisions the serving material and injects the trust that depends on it.
func (b *trustBootstrapper) run(ctx context.Context) error {
	var (
		ca  []byte
		err error
	)

	if utils.IsCertManagerTLSProvider(b.tlsProvider) {
		ca, err = b.awaitCertManagerMaterial(ctx)
	} else {
		ca, err = b.ensureSelfSignedMaterial(ctx)
	}
	if err != nil {
		return err
	}

	return injectConversionTrust(ctx, b.client, b.apiReader, ca, b.namespace)
}

// awaitCertManagerMaterial waits for the Secret cert-manager issues, then writes
// the serving pair to disk and returns the CA to trust.
//
// The Certificate itself is created by certManagerProvisioner once the manager is
// up. Creating it here as well would duplicate that ownership, so this waits for
// what a previous run left behind and fails if there is nothing yet — a first
// install under cert-manager therefore restarts once, which is visible and
// bounded, rather than silently proceeding without trust.
func (b *trustBootstrapper) awaitCertManagerMaterial(ctx context.Context) ([]byte, error) {
	cert := utils.BuildServiceServingCertificate(b.namespace, utils.WebhookServiceName, utils.WebhookServerCertSecret)
	if err := b.client.Create(ctx, cert); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create webhook Certificate: %w", err)
	}

	deadline := time.Now().Add(certManagerWaitTimeout)
	for {
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: b.namespace, Name: utils.WebhookServerCertSecret}
		err := b.apiReader.Get(ctx, key, &secret)
		if err == nil {
			crt := secret.Data[tlsCertFileName]
			tlsKey := secret.Data[tlsKeyFileName]
			if len(crt) > 0 && len(tlsKey) > 0 {
				if err := b.writeServingPair(crt, tlsKey); err != nil {
					return nil, err
				}
				// cert-manager publishes the issuing CA in ca.crt; a self-signed
				// issuer may omit it, in which case the leaf is its own trust root.
				ca := secret.Data[caCrtKey]
				if len(ca) == 0 {
					ca = crt
				}
				return ca, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get serving cert secret: %w", err)
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("cert-manager did not issue %s within %s",
				utils.WebhookServerCertSecret, certManagerWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(certManagerPollInterval):
		}
	}
}

// ensureSelfSignedMaterial reuses the serving material already in the Secret when
// it is usable, and issues a CA and a leaf when it is not.
//
// Reuse is the important half. The operator restarts, and issuing a fresh CA on
// every start would invalidate the bundle the CRDs already carry, leaving a
// window in which the API server rejects the webhook it was just told to trust.
func (b *trustBootstrapper) ensureSelfSignedMaterial(ctx context.Context) ([]byte, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: b.namespace, Name: utils.WebhookServerCertSecret}

	err := b.apiReader.Get(ctx, key, &secret)
	switch {
	case err == nil:
		if ca, crt, tlsKey, ok := usableServingMaterial(&secret, b.dnsName); ok {
			if err := b.writeServingPair(crt, tlsKey); err != nil {
				return nil, err
			}
			return ca, nil
		}
	case apierrors.IsNotFound(err):
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: b.namespace, Name: utils.WebhookServerCertSecret},
		}
	default:
		return nil, fmt.Errorf("get serving cert secret: %w", err)
	}

	material, err := issueSelfSignedMaterial(b.dnsName)
	if err != nil {
		return nil, err
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	// The key names are cert-controller's, so that its rotator adopts this
	// material on its first pass instead of issuing a second CA beside it.
	secret.Data[caCrtKey] = material.caCert
	secret.Data[caKeyKey] = material.caKey
	secret.Data[tlsCertFileName] = material.cert
	secret.Data[tlsKeyFileName] = material.key

	if secret.ResourceVersion == "" {
		if err := b.client.Create(ctx, &secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create serving cert secret: %w", err)
		}
	} else if err := b.client.Update(ctx, &secret); err != nil {
		return nil, fmt.Errorf("update serving cert secret: %w", err)
	}

	if err := b.writeServingPair(material.cert, material.key); err != nil {
		return nil, err
	}
	return material.caCert, nil
}

// writeServingPair puts the serving certificate and key where the webhook
// server's certwatcher reads them. It is written before the manager starts
// because certwatcher.New fails outright on a missing file.
func (b *trustBootstrapper) writeServingPair(crt, key []byte) error {
	if err := os.MkdirAll(b.certDir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(b.certDir, tlsCertFileName), crt, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tlsCertFileName, err)
	}
	if err := os.WriteFile(filepath.Join(b.certDir, tlsKeyFileName), key, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tlsKeyFileName, err)
	}
	return nil
}

// usableServingMaterial reports whether a Secret already holds a complete set
// that is valid for dnsName and not close to expiring.
func usableServingMaterial(secret *corev1.Secret, dnsName string) (ca, crt, key []byte, ok bool) {
	ca = secret.Data[caCrtKey]
	crt = secret.Data[tlsCertFileName]
	key = secret.Data[tlsKeyFileName]
	if len(ca) == 0 || len(secret.Data[caKeyKey]) == 0 || len(crt) == 0 || len(key) == 0 {
		return nil, nil, nil, false
	}

	block, _ := pem.Decode(crt)
	if block == nil {
		return nil, nil, nil, false
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, nil, false
	}
	if parsed.VerifyHostname(dnsName) != nil {
		return nil, nil, nil, false
	}
	// A certificate about to expire is treated as unusable, so the handover
	// happens here rather than minutes later inside a running operator.
	if time.Now().Add(24 * time.Hour).After(parsed.NotAfter) {
		return nil, nil, nil, false
	}

	return ca, crt, key, true
}

// servingMaterial is a self-signed CA and the serving pair it signed.
type servingMaterial struct {
	caCert, caKey []byte
	cert, key     []byte
}

// issueSelfSignedMaterial mints a CA and a serving certificate for dnsName.
//
// The certificate carries the in-cluster service name and its .cluster.local
// form, which are the names the API server dials the conversion webhook by.
func issueSelfSignedMaterial(dnsName string) (*servingMaterial, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "simplyblock-operator-webhook-ca", Organization: []string{"simplyblock.io"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(bootstrapCADuration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	servingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate serving key: %w", err)
	}
	servingTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName, dnsName + ".cluster.local"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(bootstrapCertDuration),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	servingDER, err := x509.CreateCertificate(rand.Reader, servingTemplate, caCert, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create serving certificate: %w", err)
	}

	return &servingMaterial{
		caCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		caKey:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}),
		cert:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: servingDER}),
		key:    pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(servingKey)}),
	}, nil
}

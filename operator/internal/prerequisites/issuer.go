// The cert-manager issuer chain the control plane's certificates are signed
// from: a self-signed ClusterIssuer, a CA Certificate, the CA ClusterIssuer,
// the CA bundle the driver mounts, and the operator's own client certificate. The Helm chart creates the same chain;
// this is the copy an OLM install has none of.

package prerequisites

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// SelfSignedIssuerName is the root the CA Certificate is minted from.
	SelfSignedIssuerName = "simplyblock-selfsigned-issuer"
	// CAIssuerName signs every certificate the operator requests.
	CAIssuerName = utils.CertManagerClusterIssuerName

	caCertificateName       = "simplyblock-certificate-authority"
	caSecretName            = "simplyblock-certificate-authority-tls"
	caBundleCertificateName = "simplyblock-ca-bundle"
	caBundleSecretName      = "simplyblock-ca-bundle-tls"

	operatorClientCertificateName = "simplyblock-operator-client"
	// OperatorClientSecretName holds the CA, certificate, and key the operator
	// presents to the management API; the bundle's Deployment mounts it.
	OperatorClientSecretName = "simplyblock-operator-client-tls"

	defaultCertManagerNamespace = "cert-manager"
)

// EnsureCertManagerIssuerChain creates the issuer chain unless a CA
// ClusterIssuer of that name already exists, which means a chart or a customer
// owns the chain. The CA bundle Certificate in namespace is ensured either way.
func EnsureCertManagerIssuerChain(ctx context.Context, c client.Client, namespace string) error {
	var objs []*unstructured.Unstructured

	existing := certManagerObject("ClusterIssuer", "", CAIssuerName, nil)
	err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	switch {
	case apierrors.IsNotFound(err):
		cmNamespace, err := certManagerNamespace(ctx, c)
		if err != nil {
			return err
		}
		objs = append(objs,
			certManagerObject("ClusterIssuer", "", SelfSignedIssuerName, map[string]any{"selfSigned": map[string]any{}}),
			// A ClusterIssuer reads its CA Secret from cert-manager's own
			// namespace, so the CA Certificate is issued there.
			certManagerObject("Certificate", cmNamespace, caCertificateName, map[string]any{
				"isCA":       true,
				"commonName": caCertificateName,
				"secretName": caSecretName,
				"privateKey": map[string]any{"algorithm": "ECDSA", "size": int64(256)},
				"issuerRef":  map[string]any{"kind": "ClusterIssuer", "name": SelfSignedIssuerName},
			}),
			certManagerObject("ClusterIssuer", "", CAIssuerName, map[string]any{"ca": map[string]any{"secretName": caSecretName}}),
		)
	case err != nil:
		return fmt.Errorf("reading ClusterIssuer %s: %w", CAIssuerName, err)
	}

	// Pods cannot mount a Secret from another namespace, so the CA is
	// re-issued here as a bundle the driver mounts.
	objs = append(objs, certManagerObject("Certificate", namespace, caBundleCertificateName, map[string]any{
		"secretName": caBundleSecretName,
		"commonName": caBundleCertificateName,
		"issuerRef":  map[string]any{"kind": "ClusterIssuer", "name": CAIssuerName},
		"usages":     []any{"digital signature", "key encipherment"},
	}))

	// The operator's own client certificate for mutual TLS. cert-manager puts
	// the CA beside it, so this one Secret is all the operator mounts.
	objs = append(objs, certManagerObject("Certificate", namespace, operatorClientCertificateName, map[string]any{
		"secretName": OperatorClientSecretName,
		"commonName": "simplyblock-operator",
		"issuerRef":  map[string]any{"kind": "ClusterIssuer", "name": CAIssuerName},
		"usages":     []any{"digital signature", "key encipherment", "client auth"},
	}))

	for _, o := range objs {
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), fieldOwner, client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

// certManagerNamespace is where cert-manager's controller runs, found by its
// standard labels, or cert-manager's default namespace when none is found.
func certManagerNamespace(ctx context.Context, c client.Reader) (string, error) {
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.MatchingLabels{
		"app.kubernetes.io/name":      "cert-manager",
		"app.kubernetes.io/component": "controller",
	}); err != nil {
		return "", fmt.Errorf("finding cert-manager's controller: %w", err)
	}
	if len(deployments.Items) > 0 {
		return deployments.Items[0].Namespace, nil
	}
	return defaultCertManagerNamespace, nil
}

func certManagerObject(kind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cert-manager.io/v1", "kind": kind}}
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(map[string]string{LabelInstalledBy: CertManager.Name})
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

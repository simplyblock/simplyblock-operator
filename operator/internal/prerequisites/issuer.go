// The cert-manager issuer chain the control plane's certificates are signed
// from: a root ClusterIssuer, a CA Certificate, the CA ClusterIssuer, the CA
// bundle the driver mounts, and the operator's own client certificate. The
// operator owns the cluster-scoped part under every install, because only it
// can find cert-manager's cluster resource namespace the CA must live in.

package prerequisites

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

	// The CSI link's Service and the serving certificate the chart mounts for it.
	csiLinkServiceName = "simplyblock-csi-link"
	csiLinkSecretName  = "simplyblock-csi-link-tls"

	defaultCertManagerNamespace  = "cert-manager"
	clusterResourceNamespaceFlag = "--cluster-resource-namespace="
)

// CAChain names the root the CA Certificate is minted from, and whether the
// operator creates that root as a self-signed ClusterIssuer.
type CAChain struct {
	RootIssuer string
	CreateRoot bool
}

// DefaultCAChain is the chain of an install that configured none.
var DefaultCAChain = CAChain{RootIssuer: SelfSignedIssuerName, CreateRoot: true}

// EnsureCAChain creates the cluster-scoped chain unless a CA ClusterIssuer of
// that name already exists, which means someone else owns it.
func EnsureCAChain(ctx context.Context, c client.Client, chain CAChain) error {
	existing := certManagerObject("ClusterIssuer", "", CAIssuerName, nil)
	err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading ClusterIssuer %s: %w", CAIssuerName, err)
	}
	cmNamespace, err := certManagerNamespace(ctx, c)
	if err != nil {
		return err
	}
	var objs []*unstructured.Unstructured
	if chain.CreateRoot {
		objs = append(objs, certManagerObject("ClusterIssuer", "", chain.RootIssuer,
			map[string]any{"selfSigned": map[string]any{}}))
	}
	objs = append(objs,
		// A ClusterIssuer reads its CA Secret from cert-manager's cluster
		// resource namespace, so the CA Certificate is issued there.
		certManagerObject("Certificate", cmNamespace, caCertificateName, map[string]any{
			"isCA":       true,
			"commonName": caCertificateName,
			"secretName": caSecretName,
			"privateKey": map[string]any{"algorithm": "ECDSA", "size": int64(256)},
			"issuerRef":  map[string]any{"kind": "ClusterIssuer", "name": chain.RootIssuer},
		}),
		certManagerObject("ClusterIssuer", "", CAIssuerName, map[string]any{"ca": map[string]any{"secretName": caSecretName}}),
	)
	return apply(ctx, c, objs)
}

// EnsureCertManagerIssuerChain ensures the default chain, then the CA bundle
// and the operator's client certificate in namespace.
func EnsureCertManagerIssuerChain(ctx context.Context, c client.Client, namespace string) error {
	if err := EnsureCAChain(ctx, c, DefaultCAChain); err != nil {
		return err
	}
	return apply(ctx, c, []*unstructured.Unstructured{
		// Pods cannot mount a Secret from another namespace, so the CA is
		// re-issued here as a bundle the driver mounts.
		certManagerObject("Certificate", namespace, caBundleCertificateName, map[string]any{
			"secretName": caBundleSecretName,
			"commonName": caBundleCertificateName,
			"issuerRef":  map[string]any{"kind": "ClusterIssuer", "name": CAIssuerName},
			"usages":     []any{"digital signature", "key encipherment"},
		}),
		// The operator's own client certificate for mutual TLS. cert-manager
		// puts the CA beside it, so this one Secret is all the operator mounts.
		certManagerObject("Certificate", namespace, operatorClientCertificateName, map[string]any{
			"secretName": OperatorClientSecretName,
			"commonName": "simplyblock-operator",
			"issuerRef":  map[string]any{"kind": "ClusterIssuer", "name": CAIssuerName},
			"usages":     []any{"digital signature", "key encipherment", "client auth"},
		}),
		// The CSI link's serving certificate. A chart install on a cluster
		// without cert-manager cannot render it until this operator installs
		// cert-manager, so the operator issues it, with the chart's spec.
		certManagerObject("Certificate", namespace, csiLinkServiceName, map[string]any{
			"secretName": csiLinkSecretName,
			"commonName": csiLinkServiceName,
			"dnsNames": []any{
				csiLinkServiceName,
				csiLinkServiceName + "." + namespace,
				csiLinkServiceName + "." + namespace + ".svc",
			},
			"issuerRef": map[string]any{"kind": "ClusterIssuer", "name": CAIssuerName},
			"usages":    []any{"digital signature", "key encipherment", "server auth"},
		}),
	})
}

func apply(ctx context.Context, c client.Client, objs []*unstructured.Unstructured) error {
	for _, o := range objs {
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), fieldOwner, client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

// CAChainKeeper ensures the CA chain at start and every Interval after, so a
// chain that cert-manager's webhook refused at first, or that a Helm upgrade
// removed, comes back. It runs on every replica: the apply is idempotent.
type CAChainKeeper struct {
	Client   client.Client
	Chain    CAChain
	Interval time.Duration
}

// Start runs until ctx ends; a failed ensure is logged and retried.
func (k *CAChainKeeper) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("ca-chain")
	t := time.NewTicker(k.Interval)
	defer t.Stop()
	for {
		if err := EnsureCAChain(ctx, k.Client, k.Chain); err != nil {
			log.Info("the simplyblock CA chain is not in place yet", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// NeedLeaderElection is false: the chain must exist before any replica's
// client certificate can be issued, leader or not.
func (k *CAChainKeeper) NeedLeaderElection() bool { return false }

// certManagerNamespace is cert-manager's cluster resource namespace, where a
// ClusterIssuer reads its CA Secret: the controller's explicit
// --cluster-resource-namespace, else the namespace it runs in, else the default.
func certManagerNamespace(ctx context.Context, c client.Reader) (string, error) {
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.MatchingLabels{
		"app.kubernetes.io/name":      "cert-manager",
		"app.kubernetes.io/component": "controller",
	}); err != nil {
		return "", fmt.Errorf("finding cert-manager's controller: %w", err)
	}
	if len(deployments.Items) == 0 {
		return defaultCertManagerNamespace, nil
	}
	d := deployments.Items[0]
	for _, ctr := range d.Spec.Template.Spec.Containers {
		for _, arg := range ctr.Args {
			ns, ok := strings.CutPrefix(arg, clusterResourceNamespaceFlag)
			// $(POD_NAMESPACE), the chart's default, is the controller's own.
			if ok && ns != "" && !strings.HasPrefix(ns, "$(") {
				return ns, nil
			}
		}
	}
	return d.Namespace, nil
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

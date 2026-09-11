// What the API server needs before it will call the conversion webhook, and the
// one function that puts it there.
//
// Two things have to be right on a converted kind's CRD: the CA bundle the API
// server verifies the webhook's serving certificate against, and the service
// reference it dials. Neither is a degradation when wrong — the API server cannot
// reach or cannot trust the webhook, so every read of that kind fails.
//
// Three callers need this and would otherwise each have their own copy: the
// pre-start bootstrap (bootstrap.go), the cert-manager provisioner on every
// rotation (certmanager.go), and the standing correction that survives a CRD
// re-apply (conversion_service.go).

package webhook

import (
	"bytes"
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// RBAC for the conversion webhook, which is a different workload from the
// operator and carries a role of its own (design-api-upgrade.md §6.1). These
// markers generate into the same manager role today because controller-gen scans
// one tree; the conversion webhook's Deployment binds only what it needs, and
// config/conversion-webhook holds that binding.
//
// Writing is restricted to the CRDs that declare a converted kind. Patching an
// arbitrary CRD is close to cluster-admin in effect — a rewritten schema or
// conversion stanza reaches every object of that kind — so the blast radius of a
// compromised webhook is worth bounding, even though the resourceNames list has
// to be kept beside the one in config/crd/converted-kinds.txt.
// TestManagerRoleNamesOnlyTheConvertedCRDs is what keeps the two in step.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;update;patch,resourceNames=controlplanes.storage.simplyblock.io;storagebackups.storage.simplyblock.io;storageclusterops.storage.simplyblock.io;storagenodeops.storage.simplyblock.io
//
// Reading stays cluster-wide because it cannot be otherwise: cert-controller's
// rotator establishes an informer on CustomResourceDefinition to re-inject the CA
// when one changes, and Kubernetes RBAC ignores resourceNames for list and watch.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=list;watch

// injectConversionTrust points every converted kind's CRD at the conversion
// webhook: the serving CA it should trust, and where its service lives.
//
// A nil ca leaves the bundle as it is, which is what the self-signed provider
// needs — cert-controller's rotator owns the bundle there, and only the namespace
// is this package's to correct.
//
// A CRD that is absent, or that carries no webhook conversion strategy, is
// skipped rather than treated as an error. The operator and its CRDs are applied
// by separate steps and in either order, so starting before they exist is an
// ordering to tolerate rather than a failure.
func injectConversionTrust(
	ctx context.Context,
	c client.Client,
	reader client.Reader,
	ca []byte,
	namespace string,
) error {
	return injectConversionTrustFor(ctx, c, reader, ca, namespace, utils.ConversionWebhookServiceName)
}

// injectConversionTrustFor is injectConversionTrust with the service name given
// rather than assumed, so a test can name a service of its own.
func injectConversionTrustFor(
	ctx context.Context,
	c client.Client,
	reader client.Reader,
	ca []byte,
	namespace, serviceName string,
) error {
	for _, name := range ConvertedKindCRDNames() {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := reader.Get(ctx, types.NamespacedName{Name: name}, &crd); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get crd %s: %w", name, err)
		}

		if crd.Spec.Conversion == nil || crd.Spec.Conversion.Webhook == nil ||
			crd.Spec.Conversion.Webhook.ClientConfig == nil {
			continue
		}
		cc := crd.Spec.Conversion.Webhook.ClientConfig

		// The shipped manifest names the namespace and service of the default
		// install. Both are corrected here because the conversion webhook is the
		// only party that knows where it is actually running, and a reference the
		// API server cannot resolve makes every read of the kind fail rather than
		// merely skipping conversion.
		bundleStale := ca != nil && !bytes.Equal(cc.CABundle, ca)
		serviceStale := cc.Service != nil &&
			(cc.Service.Namespace != namespace || cc.Service.Name != serviceName)
		if !bundleStale && !serviceStale {
			continue
		}

		patch := client.MergeFrom(crd.DeepCopy())
		if bundleStale {
			cc.CABundle = ca
		}
		if serviceStale {
			cc.Service.Namespace = namespace
			cc.Service.Name = serviceName
		}
		if err := c.Patch(ctx, &crd, patch); err != nil {
			return fmt.Errorf("patch crd %s conversion webhook: %w", name, err)
		}
	}

	return nil
}

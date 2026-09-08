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
)

// injectConversionTrust points every converted kind's CRD at this operator: the
// serving CA it should trust, and the namespace its webhook service is in.
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

		bundleStale := ca != nil && !bytes.Equal(cc.CABundle, ca)
		serviceStale := cc.Service != nil && cc.Service.Namespace != namespace
		if !bundleStale && !serviceStale {
			continue
		}

		patch := client.MergeFrom(crd.DeepCopy())
		if bundleStale {
			cc.CABundle = ca
		}
		if serviceStale {
			cc.Service.Namespace = namespace
		}
		if err := c.Patch(ctx, &crd, patch); err != nil {
			return fmt.Errorf("patch crd %s conversion webhook: %w", name, err)
		}
	}

	return nil
}

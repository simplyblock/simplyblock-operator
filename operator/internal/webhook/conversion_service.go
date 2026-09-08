// Correction of the conversion webhook's service reference on the converted CRDs.
//
// The CRD manifests ship a service reference naming the namespace of the default
// install, because a CRD is cluster-scoped and lands in the chart's `crds/`
// directory, which Helm does not template. An install into any other namespace
// therefore ships a reference to a service that is not there.
//
// That is not a degradation. The API server cannot reach a conversion webhook it
// cannot resolve, so every read of the kind fails until the reference is right,
// and the operator is the only party that knows which namespace it is in.
//
// The CA bundle is injected separately and differently by each TLS provider —
// cert-controller's rotator writes it for the self-signed provider, and
// certManagerProvisioner writes it for cert-manager — but the namespace is the
// same correction under both, so it lives here and runs once under either.

package webhook

import (
	"context"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// conversionServiceInterval is how often the correction is re-applied.
const conversionServiceInterval = 30 * time.Second

// conversionServiceReconciler points every converted CRD's conversion webhook at
// the namespace this operator is running in.
type conversionServiceReconciler struct {
	client    client.Client
	apiReader client.Reader
	namespace string
	// interval is how often the correction is re-applied. Zero means
	// conversionServiceInterval.
	interval time.Duration
}

// NeedLeaderElection reports that only the leader corrects the CRDs. They are
// cluster-scoped and shared, so several replicas writing the same field would
// contend for nothing.
func (r *conversionServiceReconciler) NeedLeaderElection() bool { return true }

// Start re-applies the correction until the manager shuts down.
//
// One pass is not enough, for two reasons that pull the same way. The operator
// and its CRDs are applied by separate steps and in either order, so a CRD may
// not exist yet when the operator starts; and re-applying the CRDs, as an
// upgrade does, puts the shipped namespace back over a correction already made.
// In both cases a one-shot pass leaves the conversion webhook pointing at a
// service that is not there, which does not degrade the kind — it makes every
// read of it fail, for as long as the operator keeps running.
//
// The pass is cheap when there is nothing to do: an uncached read per converted
// CRD, and a write only when the namespace actually differs.
func (r *conversionServiceReconciler) Start(ctx context.Context) error {
	interval := r.interval
	if interval == 0 {
		interval = conversionServiceInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := r.reconcileOnce(ctx); err != nil {
			// A failure here is not fatal to the manager: the next tick retries,
			// and returning would stop the correction for the process's lifetime.
			logf.FromContext(ctx).WithName("conversion-service").
				Error(err, "correcting the conversion service reference, will retry")
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// reconcileOnce points every converted CRD's conversion webhook at this
// operator's namespace, skipping the ones it cannot or need not touch.
func (r *conversionServiceReconciler) reconcileOnce(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("conversion-service")

	for _, name := range ConvertedKindCRDNames() {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := r.apiReader.Get(ctx, types.NamespacedName{Name: name}, &crd); err != nil {
			// The operator and its CRDs are applied by separate steps, so
			// starting before the CRDs exist is an ordering the operator has to
			// tolerate rather than an error.
			if apierrors.IsNotFound(err) {
				// Debug level: the pass repeats, so an absent CRD would otherwise
				// say so on every tick for as long as it is absent.
				log.V(1).Info("converted CRD not present yet, leaving its conversion service alone", "crd", name)
				continue
			}
			return fmt.Errorf("get crd %s: %w", name, err)
		}

		if crd.Spec.Conversion == nil || crd.Spec.Conversion.Webhook == nil ||
			crd.Spec.Conversion.Webhook.ClientConfig == nil ||
			crd.Spec.Conversion.Webhook.ClientConfig.Service == nil {
			continue
		}
		svc := crd.Spec.Conversion.Webhook.ClientConfig.Service
		if svc.Namespace == r.namespace {
			continue
		}

		patch := client.MergeFrom(crd.DeepCopy())
		svc.Namespace = r.namespace
		if err := r.client.Patch(ctx, &crd, patch); err != nil {
			return fmt.Errorf("patch crd %s conversion service namespace: %w", name, err)
		}
		log.Info("corrected the conversion webhook's service namespace",
			"crd", name, "namespace", r.namespace)
	}

	return nil
}

// SetupConversionServiceReference adds the correction to the manager. It runs
// under both TLS providers, because the namespace in the shipped manifest is
// wrong for the same reason under either.
func SetupConversionServiceReference(mgr ctrl.Manager, namespace string) error {
	return mgr.Add(&conversionServiceReconciler{
		client:    mgr.GetClient(),
		apiReader: mgr.GetAPIReader(),
		namespace: namespace,
	})
}

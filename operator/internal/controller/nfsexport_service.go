// The Service and EndpointSlice that give an export a mount address that
// outlives any one MDS host. Specified by design-pnfs-rwx.md §13.3.
//
// Built ahead of failover (§13.4/§13.5, not yet implemented) rather than
// alongside it: the address only has to exist once, at binding, for a client
// never to need to remount, and standing it up now is what a health probe and
// a future failover need to be pointed at.

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// reconcileExportService ensures the export's Service and EndpointSlice exist
// and point at mdsNodeIP, and returns the Service's ClusterIP: the address a
// client mounts.
func (r *NFSExportReconciler) reconcileExportService(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	mdsNodeIP string,
) (string, error) {
	clusterIP, err := r.reconcileExportServiceObject(ctx, export)
	if err != nil {
		return "", err
	}
	if err := r.reconcileExportEndpointSlice(ctx, export, mdsNodeIP); err != nil {
		return "", err
	}
	return clusterIP, nil
}

// reconcileExportServiceObject creates the Service on first bind, and
// otherwise leaves it alone: a ClusterIP is allocated once and must not
// change underneath an already-mounted client.
func (r *NFSExportReconciler) reconcileExportServiceObject(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) (string, error) {
	svc := utils.BuildNFSExportService(export.Namespace, export.Name)
	if err := controllerutil.SetControllerReference(export, svc, r.Scheme); err != nil {
		return "", fmt.Errorf("setting the Service owner reference: %w", err)
	}

	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, svc); err != nil {
			return "", fmt.Errorf("creating the Service: %w", err)
		}
		return svc.Spec.ClusterIP, nil
	case err != nil:
		return "", fmt.Errorf("reading the Service: %w", err)
	default:
		return existing.Spec.ClusterIP, nil
	}
}

// reconcileExportEndpointSlice publishes the bound host as the export's one
// endpoint, creating the slice on first bind and rewriting it on every pass
// after: this is the one thing a future failover repoints, and it does so by
// calling this again with a different mdsNodeIP.
func (r *NFSExportReconciler) reconcileExportEndpointSlice(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
	mdsNodeIP string,
) error {
	eps := utils.BuildNFSExportEndpointSlice(export.Namespace, export.Name, mdsNodeIP)
	if err := controllerutil.SetControllerReference(export, eps, r.Scheme); err != nil {
		return fmt.Errorf("setting the EndpointSlice owner reference: %w", err)
	}

	var existing discoveryv1.EndpointSlice
	err := r.Get(ctx, client.ObjectKeyFromObject(eps), &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, eps); err != nil {
			return fmt.Errorf("creating the EndpointSlice: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading the EndpointSlice: %w", err)
	default:
		eps.ResourceVersion = existing.ResourceVersion
		if err := r.Update(ctx, eps); err != nil {
			return fmt.Errorf("updating the EndpointSlice: %w", err)
		}
		return nil
	}
}

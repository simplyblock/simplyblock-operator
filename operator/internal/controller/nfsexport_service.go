// The Service and EndpointSlice that give a metadata server's exports one mount
// address that outlives any one pod of it. Specified by design-pnfs-rwx.md
// §13.3.
//
// One address per metadata server, not per export: the Linux client keeps one
// NFS client per server and trunks a mount at another address of the same
// server onto it, so with an address per export every mount on a node rode on
// whichever export it mounted first, and stalled once that export was deleted.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// reconcileExportService ensures the metadata server's Service and
// EndpointSlice exist and point at address, and returns the Service's
// ClusterIP: the address every export of that metadata server is mounted at.
// mdsPod is the metadata server pod the export is bound to.
func (r *NFSExportReconciler) reconcileExportService(
	ctx context.Context, mdsPod, address string,
) (string, error) {
	clusterIP, err := r.reconcileMDSServiceObject(ctx, mdsPod)
	if err != nil {
		return "", err
	}
	if err := r.reconcileExportEndpointSlice(ctx, mdsPod, address); err != nil {
		return "", err
	}
	return clusterIP, nil
}

// reconcileMDSServiceObject creates the Service on the first binding to the
// metadata server, and otherwise leaves it alone: a ClusterIP is allocated
// once and must not change underneath an already-mounted client.
func (r *NFSExportReconciler) reconcileMDSServiceObject(ctx context.Context, mdsPod string) (string, error) {
	svc := utils.BuildMDSService(r.OperatorNamespace, utils.MDSServiceName(mdsPod))
	svc.OwnerReferences = r.mdsOwner(ctx, mdsPod)

	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the Service: %w", err)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing); err != nil {
			return svc.Spec.ClusterIP, nil
		}
		return existing.Spec.ClusterIP, nil
	case err != nil:
		return "", fmt.Errorf("reading the Service: %w", err)
	default:
		return existing.Spec.ClusterIP, nil
	}
}

// reconcileExportEndpointSlice publishes address as the metadata server's one
// endpoint, creating the slice on the first binding and rewriting it on every
// pass after: this is what follows the metadata server to another pod, and an
// empty address withdraws it while the pod goes away.
func (r *NFSExportReconciler) reconcileExportEndpointSlice(ctx context.Context, mdsPod, address string) error {
	eps := utils.BuildMDSEndpointSlice(r.OperatorNamespace, utils.MDSServiceName(mdsPod), address)
	eps.OwnerReferences = r.mdsOwner(ctx, mdsPod)

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

// mdsOwner is the metadata server pod's controller, its StatefulSet, as the
// owner of its Service and EndpointSlice: they go when the metadata server
// goes, and never with one of its exports. A pod that cannot be read, or has
// no controller, leaves them unowned.
func (r *NFSExportReconciler) mdsOwner(ctx context.Context, mdsPod string) []metav1.OwnerReference {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: mdsPod}, &pod); err != nil {
		return nil
	}
	if ref := metav1.GetControllerOf(&pod); ref != nil {
		return []metav1.OwnerReference{*ref}
	}
	return nil
}

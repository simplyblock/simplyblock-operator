// The two headless Services a cluster's storage nodes are reached through, and
// their serving certificates, applied under the names the owning cluster
// recorded. A standalone cluster's workload reconciler applies them for itself,
// and a supercluster's reconciler applies them for its members (§6.2 of
// design-superclusters.md), so the apply lives here rather than in either.

package utils

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ApplyWorkloadServices applies the storage-node API and SPDK proxy Services
// the owner's workload names name, controlled by the owner.
//
// A Service another object controls is another cluster's, and it is never
// taken over: that would hand the other cluster's nodes to this one, and
// deleting this one would then delete the other's Service.
func ApplyWorkloadServices(
	ctx context.Context, c client.Client, scheme *runtime.Scheme,
	owner *simplyblockv1alpha2.StorageCluster, tlsEnabled bool, tlsProvider string,
) error {
	for _, desired := range []*corev1.Service{
		BuildStorageNodeService(owner, tlsEnabled, tlsProvider),
		BuildSpdkProxyService(owner, tlsEnabled, tlsProvider),
	} {
		if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
			return err
		}
		var existing corev1.Service
		err := c.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
		if apierrors.IsNotFound(err) {
			if err := c.Create(ctx, desired); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if controller := metav1.GetControllerOf(&existing); controller != nil && controller.UID != owner.UID {
			return fmt.Errorf("the Service %s belongs to %s %s, not to %s",
				existing.Name, controller.Kind, controller.Name, owner.Name)
		}
		// The cluster IP is assigned by Kubernetes and may not be rewritten, so
		// it is carried forward rather than re-stated.
		desired.ResourceVersion = existing.ResourceVersion
		desired.Spec.ClusterIP = existing.Spec.ClusterIP
		if err := c.Update(ctx, desired); err != nil {
			return err
		}
	}
	return nil
}

// ApplyServingCertificates applies the cert-manager Certificates for the two
// Services, where the deployment uses cert-manager at all. OpenShift's
// service-ca issues its own from an annotation on the Service, so there is
// nothing to apply there.
func ApplyServingCertificates(
	ctx context.Context, c client.Client, scheme *runtime.Scheme,
	owner *simplyblockv1alpha2.StorageCluster, tlsEnabled bool, tlsProvider string,
) error {
	if !tlsEnabled || !IsCertManagerTLSProvider(tlsProvider) {
		return nil
	}
	names, _ := WorkloadNamesOf(owner)
	for _, certificate := range []struct{ service, secret string }{
		{names.NodeAPIService, names.NodeAPITLSSecret},
		{names.SpdkProxyService, names.SpdkProxyTLSSecret},
	} {
		object := BuildServiceServingCertificate(owner.Namespace, certificate.service, certificate.secret)
		_, err := controllerutil.CreateOrUpdate(ctx, c, object, func() error {
			desired := BuildServiceServingCertificate(owner.Namespace, certificate.service, certificate.secret)
			object.Object["spec"] = desired.Object["spec"]
			return controllerutil.SetControllerReference(owner, object, scheme)
		})
		if err != nil {
			return fmt.Errorf("apply the serving certificate for %s: %w", certificate.service, err)
		}
	}
	return nil
}

// OwnServingSecrets makes the owner an owner of its two serving-certificate
// Secrets once they exist. The certificate issuer writes them, OpenShift's
// service CA from the Service's annotation and cert-manager from the
// Certificate, and neither gives them an owner, so without this a deleted
// cluster's Secrets would outlive it under names nothing uses. The reference
// is a plain one, beside any controller reference an issuer sets. A Secret not
// issued yet is taken on a later pass.
func OwnServingSecrets(
	ctx context.Context, c client.Client, scheme *runtime.Scheme,
	owner *simplyblockv1alpha2.StorageCluster, tlsEnabled bool,
) error {
	if !tlsEnabled {
		return nil
	}
	names, _ := WorkloadNamesOf(owner)
	for _, name := range []string{names.NodeAPITLSSecret, names.SpdkProxyTLSSecret} {
		var secret corev1.Secret
		err := c.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: name}, &secret)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		before := len(secret.OwnerReferences)
		if err := controllerutil.SetOwnerReference(owner, &secret, scheme); err != nil {
			return err
		}
		if len(secret.OwnerReferences) == before {
			continue
		}
		if err := c.Update(ctx, &secret); err != nil {
			return fmt.Errorf("own Secret %s: %w", name, err)
		}
	}
	return nil
}

// WorkloadServicesReady reports whether the Services a cluster's workload names
// name exist, and their serving-certificate Secrets where TLS is on, and names
// what is missing when they do not. A supercluster's member, whose names are
// its supercluster's, waits on it before it builds anything that would be
// reached through them.
func WorkloadServicesReady(
	ctx context.Context, c client.Reader, cluster *simplyblockv1alpha2.StorageCluster, tlsEnabled bool,
) (bool, string, error) {
	names, _ := WorkloadNamesOf(cluster)
	wanted := []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: names.NodeAPIService}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: names.SpdkProxyService}},
	}
	if tlsEnabled {
		wanted = append(wanted,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.NodeAPITLSSecret}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.SpdkProxyTLSSecret}})
	}
	for _, object := range wanted {
		err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: object.GetName()}, object)
		if apierrors.IsNotFound(err) {
			kind := "Service"
			if _, secret := object.(*corev1.Secret); secret {
				kind = "Secret"
			}
			return false, fmt.Sprintf("waiting for %s %s", kind, object.GetName()), nil
		}
		if err != nil {
			return false, "", err
		}
	}
	return true, "", nil
}

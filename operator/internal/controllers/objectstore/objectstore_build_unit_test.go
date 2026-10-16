// buildDeployment, buildService, and the TLS provider dispatch between them
// (design-objectstore.md §4, §5), proved as pure functions: no cluster, no
// client.Client.

package objectstore

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/bucketdelivery"
	objectstorev1alpha1 "github.com/simplyblock/simplyblock-operator/api/objectstore/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	testObjectStoreNamespace = "default"
	testObjectStoreOnlyName  = "team-a-store"
)

func testObjectStoreSpec(mutate ...func(*objectstorev1alpha1.ObjectStore)) *objectstorev1alpha1.ObjectStore {
	store := &objectstorev1alpha1.ObjectStore{}
	store.Name = testObjectStoreOnlyName
	store.Namespace = testObjectStoreNamespace
	store.Spec = objectstorev1alpha1.ObjectStoreSpec{
		Image: "docker.io/simplyblock/gateway:main",
		Port:  9000,
	}
	for _, m := range mutate {
		m(store)
	}
	return store
}

// Every gateway pod carries the shared hostPath volume DeliverBucket's own
// bind mounts need to reach it (design-bucket-provisioning.md §5.1, §5.4):
// without this, a bucket delivered after the pod started would never
// become visible inside it, since propagation has nothing to propagate
// into.
func TestBuildDeploymentMountsTheSharedBucketHostPathWithPropagation(t *testing.T) {
	store := testObjectStoreSpec()
	deployment := buildDeployment(store)

	container := deployment.Spec.Template.Spec.Containers[0]
	var mount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == bucketMountVolumeName {
			mount = &container.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("no volume mount named %q, want the gateway container to carry it", bucketMountVolumeName)
	}
	if mount.MountPath != bucketdelivery.GatewayMountRoot {
		t.Errorf("mount path = %q, want %q (the same path DeliverBucket bind-mounts under)",
			mount.MountPath, bucketdelivery.GatewayMountRoot)
	}
	if mount.MountPropagation == nil || *mount.MountPropagation != corev1.MountPropagationHostToContainer {
		t.Errorf("mountPropagation = %v, want HostToContainer", mount.MountPropagation)
	}

	var volume *corev1.Volume
	for i := range deployment.Spec.Template.Spec.Volumes {
		if deployment.Spec.Template.Spec.Volumes[i].Name == bucketMountVolumeName {
			volume = &deployment.Spec.Template.Spec.Volumes[i]
		}
	}
	if volume == nil || volume.HostPath == nil || volume.HostPath.Path != bucketdelivery.GatewayMountRoot {
		t.Fatalf("volume %q = %+v, want a hostPath volume at %q", bucketMountVolumeName, volume, bucketdelivery.GatewayMountRoot)
	}

	var rootDir string
	for _, env := range container.Env {
		if env.Name == gatewayRootDirEnv {
			rootDir = env.Value
		}
	}
	if rootDir != bucketdelivery.GatewayMountRoot {
		t.Errorf("%s = %q, want %q (the gateway's own backend root has to be the same path the volume is mounted at)",
			gatewayRootDirEnv, rootDir, bucketdelivery.GatewayMountRoot)
	}
}

// U-09: the pod template carries the label
// design-bucket-provisioning.md's S3BucketReconciler finds gateway pods by.
func TestBuildDeploymentCarriesTheGatewayInstanceLabel(t *testing.T) {
	store := testObjectStoreSpec()
	deployment := buildDeployment(store)

	got := deployment.Spec.Template.Labels[gatewayInstanceLabel]
	if got != store.Name {
		t.Errorf("pod template label %s = %q, want %q", gatewayInstanceLabel, got, store.Name)
	}
	if deployment.Spec.Selector.MatchLabels[gatewayInstanceLabel] != store.Name {
		t.Errorf("selector label %s = %q, want %q",
			gatewayInstanceLabel, deployment.Spec.Selector.MatchLabels[gatewayInstanceLabel], store.Name)
	}
}

// U-10: spec.envFrom is copied onto the gateway container's own envFrom
// unmodified.
func TestBuildDeploymentCopiesEnvFromUnmodified(t *testing.T) {
	envFrom := []corev1.EnvFromSource{
		{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "team-a-store-vault-token"}}},
	}
	store := testObjectStoreSpec(func(o *objectstorev1alpha1.ObjectStore) { o.Spec.EnvFrom = envFrom })

	deployment := buildDeployment(store)

	got := deployment.Spec.Template.Spec.Containers[0].EnvFrom
	if len(got) != 1 || got[0].SecretRef == nil || got[0].SecretRef.Name != "team-a-store-vault-token" {
		t.Errorf("gateway container envFrom = %+v, want %+v copied unmodified", got, envFrom)
	}
}

// U-11: spec.replicas unset requests the default of 1.
func TestBuildDeploymentDefaultsReplicasToOne(t *testing.T) {
	store := testObjectStoreSpec()
	deployment := buildDeployment(store)

	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want a pointer to 1", deployment.Spec.Replicas)
	}
}

// U-12: the Service is ClusterIP, carries exactly spec.port, and selects the
// same label the Deployment's pod template carries.
func TestBuildServiceIsClusterIPOnSpecPortSelectingTheDeployment(t *testing.T) {
	store := testObjectStoreSpec()
	service := buildService(store)

	if service.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %q, want ClusterIP", service.Spec.Type)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != store.Spec.Port {
		t.Errorf("Service ports = %+v, want exactly one port %d", service.Spec.Ports, store.Spec.Port)
	}
	if service.Spec.Selector[gatewayInstanceLabel] != store.Name {
		t.Errorf("Service selector %s = %q, want %q",
			gatewayInstanceLabel, service.Spec.Selector[gatewayInstanceLabel], store.Name)
	}
}

// U-13: spec.port at its minimum (1) and maximum (65535) are both carried
// through unchanged.
func TestBuildServicePortAtItsLimits(t *testing.T) {
	for _, port := range []int32{1, 65535} {
		store := testObjectStoreSpec(func(o *objectstorev1alpha1.ObjectStore) { o.Spec.Port = port })
		service := buildService(store)
		if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != port {
			t.Errorf("port %d: Service ports = %+v, want exactly one port %d", port, service.Spec.Ports, port)
		}
	}
}

// U-14: tlsProvider OpenShift sets the serving-cert annotation on the
// Service and creates no Certificate object (applyCertificate is never
// called for this provider; see applyOwnedObjects).
func TestOpenShiftTLSProviderAnnotatesTheServiceOnly(t *testing.T) {
	store := testObjectStoreSpec(func(o *objectstorev1alpha1.ObjectStore) {
		o.Spec.TLSProvider = utils.TLSProviderOpenShift
	})
	service := buildService(store)

	secretName := service.Annotations[utils.OpenShiftServingCertAnnotation]
	if secretName != store.Name {
		t.Errorf("Service annotation %s = %q, want %q",
			utils.OpenShiftServingCertAnnotation, secretName, store.Name)
	}
	if utils.IsCertManagerTLSProvider(store.Spec.TLSProvider) {
		t.Error("OpenShift provider reads as cert-manager, which would also create a Certificate")
	}
}

// U-15: tlsProvider cert-manager carries no OpenShift annotation, and the
// Certificate utils.BuildServiceServingCertificate builds names this
// ObjectStore's Service and Secret.
func TestCertManagerTLSProviderBuildsACertificateAndNoAnnotation(t *testing.T) {
	store := testObjectStoreSpec(func(o *objectstorev1alpha1.ObjectStore) {
		o.Spec.TLSProvider = utils.TLSProviderCertManager
	})
	service := buildService(store)

	if _, set := service.Annotations[utils.OpenShiftServingCertAnnotation]; set {
		t.Error("cert-manager provider carries the OpenShift serving-cert annotation")
	}

	cert := utils.BuildServiceServingCertificate(store.Namespace, store.Name, tlsSecretName(store.Name))
	if cert.GetName() != store.Name || cert.GetNamespace() != store.Namespace {
		t.Errorf("Certificate = %s/%s, want %s/%s", cert.GetNamespace(), cert.GetName(), store.Namespace, store.Name)
	}
}

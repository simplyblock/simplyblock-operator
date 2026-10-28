// buildDeployment, buildService, and the TLS provider dispatch between them
// (design-objectstore.md §4, §5), proved as pure functions: no cluster, no
// client.Client.

package objectstore

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/bucketdelivery"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	testObjectStoreNamespace = "default"
	testObjectStoreOnlyName  = "team-a-store"
)

var testObjectStoreBuckets = simplyblockv1alpha2.ObjectStoreBuckets{
	StorageClassName: "simplyblock-pnfs",
	CapacityBytes:    10 << 30,
}

func testObjectStoreSpec(mutate ...func(*simplyblockv1alpha2.ObjectStore)) *simplyblockv1alpha2.ObjectStore {
	store := &simplyblockv1alpha2.ObjectStore{}
	store.Name = testObjectStoreOnlyName
	store.Namespace = testObjectStoreNamespace
	store.Spec = simplyblockv1alpha2.ObjectStoreSpec{
		Image:   "docker.io/simplyblock/gateway:main",
		Port:    9000,
		Buckets: testObjectStoreBuckets,
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
	store := testObjectStoreSpec(func(o *simplyblockv1alpha2.ObjectStore) { o.Spec.EnvFrom = envFrom })

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
		store := testObjectStoreSpec(func(o *simplyblockv1alpha2.ObjectStore) { o.Spec.Port = port })
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
	store := testObjectStoreSpec(func(o *simplyblockv1alpha2.ObjectStore) {
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
	store := testObjectStoreSpec(func(o *simplyblockv1alpha2.ObjectStore) {
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

// The gateway creates and deletes S3Buckets for CreateBucket and DeleteBucket
// under its own identity, so every gateway pod runs as the ServiceAccount
// buildServiceAccount names.
func TestBuildDeploymentRunsAsTheGatewayServiceAccount(t *testing.T) {
	store := testObjectStoreSpec()
	deployment := buildDeployment(store)

	if got, want := deployment.Spec.Template.Spec.ServiceAccountName, buildServiceAccount(store).Name; got != want {
		t.Errorf("serviceAccountName = %q, want %q", got, want)
	}
}

// The gateway fills every S3Bucket it creates from these: which ObjectStore
// it serves, the namespace to create in, and spec.buckets.
func TestBuildDeploymentPassesTheBucketSettingsToTheGateway(t *testing.T) {
	store := testObjectStoreSpec()
	container := buildDeployment(store).Spec.Template.Spec.Containers[0]

	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		gatewayObjectStoreEnv:         store.Name,
		gatewayNamespaceEnv:           store.Namespace,
		gatewayBucketStorageClassEnv:  "simplyblock-pnfs",
		gatewayBucketCapacityBytesEnv: "10737418240",
	} {
		if got, ok := env[name]; !ok || got != want {
			t.Errorf("%s = %q (set: %v), want %q", name, got, ok, want)
		}
	}
}

// The gateway serves the certificate mounted from the TLS Secret, for either
// provider: cert-manager and OpenShift's service CA both write tls.crt and
// tls.key, so the paths are the same.
func TestBuildDeploymentPointsTheGatewayAtTheMountedCertificate(t *testing.T) {
	for _, provider := range []string{"cert-manager", "OpenShift"} {
		t.Run(provider, func(t *testing.T) {
			store := testObjectStoreSpec(func(s *simplyblockv1alpha2.ObjectStore) {
				s.Spec.TLSProvider = provider
			})
			container := buildDeployment(store).Spec.Template.Spec.Containers[0]

			env := map[string]string{}
			for _, e := range container.Env {
				env[e.Name] = e.Value
			}
			for name, want := range map[string]string{
				gatewayTLSCertFileEnv: tlsMountPath + "/tls.crt",
				gatewayTLSKeyFileEnv:  tlsMountPath + "/tls.key",
			} {
				if got, ok := env[name]; !ok || got != want {
					t.Errorf("%s = %q (set: %v), want %q", name, got, ok, want)
				}
			}
		})
	}
}

// The gateway's Role grants exactly what CreateBucket and DeleteBucket use,
// on S3Buckets only, and its RoleBinding binds it to the gateway's
// ServiceAccount and nothing else.
func TestBuildGatewayRBACGrantsOnlyTheBucketLifecycle(t *testing.T) {
	store := testObjectStoreSpec()
	sa := buildServiceAccount(store)
	role := buildRole(store)
	binding := buildRoleBinding(store)

	if sa.Namespace != store.Namespace || role.Namespace != store.Namespace || binding.Namespace != store.Namespace {
		t.Errorf("namespaces = %q/%q/%q, want all %q", sa.Namespace, role.Namespace, binding.Namespace, store.Namespace)
	}

	if len(role.Rules) != 1 {
		t.Fatalf("rules = %+v, want exactly one", role.Rules)
	}
	rule := role.Rules[0]
	if !slices.Equal(rule.APIGroups, []string{simplyblockv1alpha2.GroupVersion.Group}) ||
		!slices.Equal(rule.Resources, []string{"s3buckets"}) ||
		!slices.Equal(rule.Verbs, []string{"get", "create", "delete"}) {
		t.Errorf("rule = %+v, want get/create/delete on %s s3buckets", rule, simplyblockv1alpha2.GroupVersion.Group)
	}

	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name {
		t.Errorf("roleRef = %+v, want Role/%s", binding.RoleRef, role.Name)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Name != sa.Name || binding.Subjects[0].Namespace != sa.Namespace {
		t.Errorf("subjects = %+v, want only ServiceAccount %s/%s", binding.Subjects, sa.Namespace, sa.Name)
	}
}

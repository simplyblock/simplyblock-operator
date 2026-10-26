// Pure construction of an ObjectStore's owned objects: the gateway
// Deployment, its ClusterIP Service, the TLS volume that wires the Service's
// serving certificate into the gateway container (design-objectstore.md §4,
// §5), and the ServiceAccount, Role, and RoleBinding the gateway creates and
// deletes S3Buckets under. No client.Client here: every function
// takes the ObjectStore and returns the object, so the controller tests
// (objectstore_build_unit_test.go) assert what a pass would create without a
// cluster.

package objectstore

import (
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/simplyblock/atlas/bucketdelivery"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// gatewayContainerName is the one container every gateway pod runs.
	gatewayContainerName = "gateway"

	// tlsVolumeName and tlsMountPath match buildStorageNodeSetTLSVolume's own
	// convention (internal/utils/storage_node_workload.go): every TLS-serving
	// workload this operator builds mounts its certificate at the same path,
	// so an operator who knows one knows all of them.
	tlsVolumeName = "tls"
	tlsMountPath  = "/etc/simplyblock/tls"

	// certificateAuthorityConfigMap is the OpenShift CA bundle
	// buildStorageNodeSetTLSVolume projects alongside the serving-cert
	// Secret. Named here rather than imported: it is internal to that
	// function's package-private constant, so this file states the string
	// its own doc comment already promises.
	certificateAuthorityConfigMap = "simplyblock-certificate-authority"

	// bucketMountVolumeName and bucketMountPath are the shared hostPath
	// every gateway pod mounts HostToContainer
	// (design-bucket-provisioning.md §5.1, §5.4): the CSI node plugin's own
	// DeliverBucket bind-mounts a bucket's pNFS volume under
	// bucketdelivery.GatewayMountRoot on the host, and that mount only
	// reaches an already-running gateway pod if the pod already carries this
	// volume, set once at Deployment creation and never touched again. The
	// in-container path is the same as the host path on purpose: the
	// gateway's own GATEWAY_ROOT_DIR (gatewayRootDirEnv below) then names
	// one path that means the same thing on both sides, rather than this
	// reconciler and the gateway binary each needing their own opinion about
	// a translation between them.
	bucketMountVolumeName = "gateway-buckets"
	bucketMountPath       = bucketdelivery.GatewayMountRoot

	// gatewayRootDirEnv is the env var the gateway binary reads its backend
	// root directory from (kubernetes/gateway/internal/config). Set here
	// rather than left to spec.envFrom: it names an operator-determined
	// mount path, not a credential or anything else the gateway repository's
	// own Vault integration would supply (design §4.1's own boundary for
	// what envFrom carries).
	gatewayRootDirEnv = "GATEWAY_ROOT_DIR"

	// The env vars the gateway binary fills every S3Bucket it creates from
	// (kubernetes/gateway/internal/config): which ObjectStore it is, the
	// namespace its S3Buckets live in, and spec.buckets.
	gatewayObjectStoreEnv         = "GATEWAY_OBJECTSTORE"
	gatewayNamespaceEnv           = "GATEWAY_NAMESPACE"
	gatewayBucketStorageClassEnv  = "GATEWAY_BUCKET_STORAGE_CLASS"
	gatewayBucketCapacityBytesEnv = "GATEWAY_BUCKET_CAPACITY_BYTES"
)

// defaultReplicas is spec.replicas' own +kubebuilder:default, read here too:
// a build function has to pick a concrete number, and the default lives on
// the field so the OpenAPI schema (and kubectl explain) state it once.
const defaultReplicas = 1

// tlsSecretName is the Secret an ObjectStore's serving certificate is issued
// into or annotated for, and that its gateway pods mount. Named after the
// object itself, like the Deployment and the Service: design §5 states this
// pairing without a separate field to carry it.
func tlsSecretName(name string) string {
	return name
}

// buildDeployment is the gateway workload: one container, the image and port
// design §4.1 require, spec's passthrough fields applied verbatim, and the
// label (U-09) design-bucket-provisioning.md's S3BucketReconciler finds
// gateway pods by.
func buildDeployment(store *simplyblockv1alpha2.ObjectStore) *appsv1.Deployment {
	labels := map[string]string{gatewayInstanceLabel: store.Name}

	replicas := int32(defaultReplicas)
	if store.Spec.Replicas != nil {
		replicas = *store.Spec.Replicas
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: store.Name, Namespace: store.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: gatewayServiceAccountName(store.Name),
					NodeSelector:       store.Spec.NodeSelector,
					Tolerations:        store.Spec.Tolerations,
					Containers: []corev1.Container{
						{
							Name:            gatewayContainerName,
							Image:           string(store.Spec.Image),
							ImagePullPolicy: store.Spec.ImagePullPolicy,
							Ports: []corev1.ContainerPort{
								{Name: "s3", ContainerPort: store.Spec.Port},
							},
							Env: []corev1.EnvVar{
								{Name: gatewayRootDirEnv, Value: bucketMountPath},
								{Name: gatewayObjectStoreEnv, Value: store.Name},
								{Name: gatewayNamespaceEnv, Value: store.Namespace},
								{Name: gatewayBucketStorageClassEnv, Value: store.Spec.Buckets.StorageClassName},
								{
									Name:  gatewayBucketCapacityBytesEnv,
									Value: strconv.FormatInt(store.Spec.Buckets.CapacityBytes, 10),
								},
							},
							EnvFrom:   store.Spec.EnvFrom,
							Resources: store.Spec.Resources,
							VolumeMounts: []corev1.VolumeMount{
								{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true},
								{
									Name:             bucketMountVolumeName,
									MountPath:        bucketMountPath,
									MountPropagation: ptr.To(corev1.MountPropagationHostToContainer),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						buildGatewayTLSVolume(store),
						buildGatewayBucketMountVolume(),
					},
				},
			},
		},
	}
}

// buildGatewayTLSVolume mirrors buildStorageNodeSetTLSVolume's own shape
// (internal/utils/storage_node_workload.go): a plain Secret volume for
// cert-manager, since cert-manager already bundles the CA into the Secret it
// writes, and a projected volume combining the Secret with OpenShift's
// separate CA ConfigMap otherwise.
func buildGatewayTLSVolume(store *simplyblockv1alpha2.ObjectStore) corev1.Volume {
	secretName := tlsSecretName(store.Name)
	if utils.IsCertManagerTLSProvider(store.Spec.TLSProvider) {
		return corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: secretName},
			},
		}
	}

	return corev1.Volume{
		Name: tlsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}}},
					{
						ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: certificateAuthorityConfigMap},
							Items:                []corev1.KeyToPath{{Key: "service-ca.crt", Path: "ca.crt"}},
						},
					},
				},
			},
		},
	}
}

// buildGatewayBucketMountVolume is the shared hostPath every gateway pod
// carries so a bucket DeliverBucket bind-mounts after the pod is already
// running still reaches it (design-bucket-provisioning.md §5.1, §5.4): set
// once here, at Deployment creation, and never touched again by anything
// that reacts to a bucket's own lifecycle. DirectoryOrCreate rather than
// Directory: the first ObjectStore on a node precedes every bucket that
// will ever be delivered to it, so the path need not exist yet.
func buildGatewayBucketMountVolume() corev1.Volume {
	return corev1.Volume{
		Name: bucketMountVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: bucketMountPath,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	}
}

// buildService is the ClusterIP Service design §5 describes: one port,
// spec.port, selecting the Deployment's own pod label, carrying whichever of
// the two TLS annotations/objects spec.tlsProvider selects.
func buildService(store *simplyblockv1alpha2.ObjectStore) *corev1.Service {
	secretName := tlsSecretName(store.Name)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      store.Name,
			Namespace: store.Namespace,
			Annotations: utils.ServingCertServiceAnnotations(
				true, store.Spec.TLSProvider, secretName),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{gatewayInstanceLabel: store.Name},
			Ports: []corev1.ServicePort{
				{Name: "s3", Port: store.Spec.Port, TargetPort: intstr.FromInt32(store.Spec.Port)},
			},
		},
	}
}

// gatewayServiceAccountName names the gateway's ServiceAccount, Role, and
// RoleBinding after the ObjectStore, like the Deployment and the Service.
func gatewayServiceAccountName(name string) string {
	return name
}

// buildServiceAccount is the identity every gateway pod runs as.
func buildServiceAccount(store *simplyblockv1alpha2.ObjectStore) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayServiceAccountName(store.Name), Namespace: store.Namespace},
	}
}

// buildRole grants what the gateway's CreateBucket and DeleteBucket do with
// S3Buckets in its own namespace: create one, read its status until it is
// Ready or gone, and delete it. Nothing else.
func buildRole(store *simplyblockv1alpha2.ObjectStore) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayServiceAccountName(store.Name), Namespace: store.Namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{simplyblockv1alpha2.GroupVersion.Group},
			Resources: []string{"s3buckets"},
			Verbs:     []string{"get", "create", "delete"},
		}},
	}
}

// buildRoleBinding binds buildRole to the gateway's ServiceAccount.
func buildRoleBinding(store *simplyblockv1alpha2.ObjectStore) *rbacv1.RoleBinding {
	name := gatewayServiceAccountName(store.Name)
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: store.Namespace}},
	}
}

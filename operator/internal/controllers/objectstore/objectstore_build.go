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
	"path"
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
	// DeliverBucket bind-mounts a bucket's pNFS volume under a subdirectory
	// of bucketdelivery.GatewayMountRoot on the host, and that mount only
	// reaches an already-running gateway pod if the pod already carries this
	// volume, set once at Deployment creation and never touched again. The
	// in-container path is the same as the host path on purpose, so this
	// reconciler and the gateway binary never need their own opinion about a
	// translation between them: what differs per ObjectStore is not this
	// mount, but GATEWAY_ROOT_DIR (gatewayRootDirEnv below), which points
	// each gateway at its own subdirectory of this same mounted root
	// (gatewayMountPrefix) rather than at the root itself.
	bucketMountVolumeName = "gateway-buckets"
	bucketMountPath       = bucketdelivery.GatewayMountRoot

	// gatewayRootDirEnv is the env var the gateway binary reads its backend
	// root directory from (kubernetes/gateway/internal/config), set to this
	// ObjectStore's own subdirectory of bucketMountPath
	// (gatewayMountPrefix), not to bucketMountPath itself. Set here rather
	// than left to spec.envFrom: it names an operator-determined mount path,
	// not a credential or anything else the gateway repository's own Vault
	// integration would supply (design §4.1's own boundary for what envFrom
	// carries).
	gatewayRootDirEnv = "GATEWAY_ROOT_DIR"

	// The env vars the gateway binary fills every S3Bucket it creates from
	// (kubernetes/gateway/internal/config): which ObjectStore it is, the
	// namespace its S3Buckets live in, and spec.buckets.
	gatewayObjectStoreEnv         = "GATEWAY_OBJECTSTORE"
	gatewayNamespaceEnv           = "GATEWAY_NAMESPACE"
	gatewayBucketStorageClassEnv  = "GATEWAY_BUCKET_STORAGE_CLASS"
	gatewayBucketCapacityBytesEnv = "GATEWAY_BUCKET_CAPACITY_BYTES"
	gatewayRegionEnv              = "GATEWAY_REGION"

	// The env vars naming the serving certificate and key the gateway
	// serves, and re-reads when the TLS Secret is rotated.
	gatewayTLSCertFileEnv = "GATEWAY_TLS_CERT_FILE"
	gatewayTLSKeyFileEnv  = "GATEWAY_TLS_KEY_FILE"
)

// defaultReplicas is spec.replicas' own +kubebuilder:default, read here too:
// a build function has to pick a concrete number, and the default lives on
// the field so the OpenAPI schema (and kubectl explain) state it once.
const defaultReplicas = 1

// gatewayMountPrefix scopes GatewayMountRoot to one ObjectStore alone, so a
// bucket delivered for it is invisible to any other ObjectStore's gateway
// sharing the same node (tasks.md D4,
// issues/19-mc-smoke-test-bucket-not-found.md: BucketLinks mode lists every
// subdirectory it finds under its root as its own, with nothing before this
// to stop it finding another ObjectStore's). Namespace before name, so two
// ObjectStores named alike in different namespaces still get distinct
// prefixes. Takes the two strings rather than an *ObjectStore so a caller
// that only has an S3Bucket's own namespace and spec.objectStoreRef (every
// caller in this package, by design: see s3bucket_delivery.go's own package
// comment) need not fetch the ObjectStore object just for this.
func gatewayMountPrefix(namespace, objectStoreName string) string {
	return path.Join(namespace, objectStoreName)
}

// gatewayRootDir is this ObjectStore's own subdirectory of the shared
// GatewayMountRoot: what GATEWAY_ROOT_DIR is set to, what the gateway
// container's VolumeMount.MountPath is, and what the hostPath Volume's own
// Path is. All three have to be this identical value, not merely the same
// prefix in spirit: the hostPath volume (DirectoryOrCreate) is what makes
// this directory exist on the node in the first place, and if its Path
// stopped at the shared root instead, only the root would be created, not
// the scoped subdirectory the gateway is told to chdir into
// (2026-11-09-gateway-root-dir-not-created).
func gatewayRootDir(store *simplyblockv1alpha2.ObjectStore) string {
	return path.Join(bucketMountPath, gatewayMountPrefix(store.Namespace, store.Name))
}

// tlsSecretName is the Secret an ObjectStore's serving certificate is issued
// into or annotated for, and that its gateway pods mount. Named after the
// object itself, like the Deployment and the Service: design §5 states this
// pairing without a separate field to carry it.
func tlsSecretName(name string) string {
	return name
}

// gatewayAntiAffinity keeps an ObjectStore's gateway replicas off the same
// node as each other: required rather than preferred, matching
// spreadAcrossHosts's own reasoning (internal/controllers/controlplane/
// podspec.go) — two instances on one machine survive a process crash and
// not the machine, which is the whole point of running more than one
// (tasks.md D1).
func gatewayAntiAffinity(store *simplyblockv1alpha2.ObjectStore) *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{gatewayInstanceLabel: store.Name},
				},
				TopologyKey: "kubernetes.io/hostname",
			}},
		},
	}
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
					Affinity:           gatewayAntiAffinity(store),
					Containers: []corev1.Container{
						{
							Name:            gatewayContainerName,
							Image:           string(store.Spec.Image),
							ImagePullPolicy: store.Spec.ImagePullPolicy,
							Ports: []corev1.ContainerPort{
								{Name: "s3", ContainerPort: store.Spec.Port},
							},
							Env: []corev1.EnvVar{
								{Name: gatewayRootDirEnv, Value: gatewayRootDir(store)},
								{Name: gatewayObjectStoreEnv, Value: store.Name},
								{Name: gatewayNamespaceEnv, Value: store.Namespace},
								{Name: gatewayBucketStorageClassEnv, Value: store.Spec.Buckets.StorageClassName},
								{
									Name:  gatewayBucketCapacityBytesEnv,
									Value: strconv.FormatInt(store.Spec.Buckets.CapacityBytes, 10),
								},
								{Name: gatewayRegionEnv, Value: store.Spec.Region},
								{Name: gatewayTLSCertFileEnv, Value: tlsMountPath + "/tls.crt"},
								{Name: gatewayTLSKeyFileEnv, Value: tlsMountPath + "/tls.key"},
							},
							EnvFrom:   store.Spec.EnvFrom,
							Resources: store.Spec.Resources,
							VolumeMounts: []corev1.VolumeMount{
								{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true},
								{
									Name:             bucketMountVolumeName,
									MountPath:        gatewayRootDir(store),
									MountPropagation: ptr.To(corev1.MountPropagationHostToContainer),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						buildGatewayTLSVolume(store),
						buildGatewayBucketMountVolume(store),
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

// buildGatewayBucketMountVolume is the hostPath every gateway pod of this
// ObjectStore carries, at this ObjectStore's own scoped subdirectory
// (gatewayRootDir) of the shared GatewayMountRoot, so a bucket DeliverBucket
// bind-mounts after the pod is already running still reaches it
// (design-bucket-provisioning.md §5.1, §5.4): set once here, at Deployment
// creation, and never touched again by anything that reacts to a bucket's
// own lifecycle. DirectoryOrCreate rather than Directory: this is what
// brings the scoped subdirectory into existence on the node at all — the
// first ObjectStore here precedes every bucket that will ever be delivered
// to it, so the path need not exist yet, but it does need this volume to be
// the one thing that creates it, since nothing else will.
func buildGatewayBucketMountVolume(store *simplyblockv1alpha2.ObjectStore) corev1.Volume {
	return corev1.Volume{
		Name: bucketMountVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: gatewayRootDir(store),
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

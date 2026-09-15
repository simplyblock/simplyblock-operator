// The store the ApplyingDatastore step installs, and the bucket configuration
// beside it.
//
// design-controlplane.md §4.2 names this step "the document store the management
// API needs" and §5.1 identifies that as a MongoDBCommunity, which §12 Q6 then
// asks what reconciles. A base deployment settles the question by not having
// one: the chart renders the MongoDBCommunity only when observability is
// enabled, because what keeps documents in it is Graylog rather than the
// management API, and the reference deployment runs no MongoDB at all with the
// management API Available.
//
// What a base deployment does run is an object store, so that is what the step
// applies. The store holds what outlives a process: the metric history the
// control plane keeps and the backups it writes. Nothing here creates the bucket
// — a sidecar beside the server does, because the store has to be answering
// before a bucket can be made in it, and a Job would have to be retried against
// exactly that.

package controlplane

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The object store's images and its fixed configuration.
//
// The credentials are the chart's, and they are constants rather than a
// generated Secret for one reason worth stating plainly: the store listens only
// on a ClusterIP Service inside the namespace, and every consumer of it is a
// workload this same install creates. Making them a Secret the operator
// generated would be a real improvement and is not one this change makes,
// because the value is also written into the objstore configuration the chart
// still renders for the observability half, and the two have to agree.
const (
	minioImage       = "quay.io/simplyblock-io/minio:RELEASE.2024-01-16T16-07-38Z"
	minioClientImage = "quay.io/simplyblock-io/minio-client:RELEASE.2024-01-16T16-06-34Z"

	minioAccessKey = "minioadmin"
	minioSecretKey = "minioadmin"
	minioBucket    = "thanos"

	// minioVolumeSize is what the store claims. It holds metric history and
	// backup objects, so it grows with how long a deployment keeps them rather
	// than with how much data the fleet stores.
	minioVolumeSize = "50Gi"

	// minioDataVolume is the claim template's name, and therefore part of every
	// PersistentVolumeClaim name the StatefulSet creates. It is immutable in
	// practice: renaming it orphans the volume holding the store's data.
	minioDataVolume = "minio-data"
)

// datastoreObjects is everything the ApplyingDatastore step writes: the bucket
// configuration its consumers read, the store itself, and the Service they reach
// it on.
func datastoreObjects(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	return []client.Object{
		objectStoreConfig(cp.Namespace),
		minioStatefulSet(cp),
		minioService(cp.Namespace),
	}
}

// objectStoreConfig is the bucket, the endpoint, and the credentials in the form
// the store's consumers parse. It is applied here rather than with the
// management API because it describes the store rather than the reader.
func objectStoreConfig(namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectStoreConfigName,
			Namespace: namespace,
			Labels:    map[string]string{appLabel: ComponentMinio},
		},
		Data: map[string]string{
			"objstore.yml": "type: S3\n" +
				"config:\n" +
				"  bucket: " + minioBucket + "\n" +
				"  endpoint: " + ComponentMinio + ":9000\n" +
				"  access_key: " + minioAccessKey + "\n" +
				"  secret_key: " + minioSecretKey + "\n" +
				"  insecure: true\n",
		},
	}
}

// minioStatefulSet is the store: one server with a volume, and a sidecar that
// makes the bucket once the server answers.
//
// It is a StatefulSet rather than a Deployment because the data is on a volume
// the pod has to come back to. One replica is what a base deployment gets, and
// it is a single point of failure for the metric history rather than for the
// control plane: nothing in the install path or the data path reads it.
func minioStatefulSet(cp *simplyblockv1alpha2.ControlPlane) *appsv1.StatefulSet {
	labels := map[string]string{appLabel: ComponentMinio}
	credentials := []corev1.EnvVar{
		{Name: "MINIO_ROOT_USER", Value: minioAccessKey},
		{Name: "MINIO_ROOT_PASSWORD", Value: minioSecretKey},
	}

	spec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:            "minio",
				Image:           minioImage,
				ImagePullPolicy: pullPolicyOf(cp.Spec.Source.Managed),
				Args:            []string{"server", "/data", "--console-address=:9001"},
				Env:             credentials,
				Ports: []corev1.ContainerPort{
					{Name: "api", ContainerPort: minioAPIPort},
					{Name: "console", ContainerPort: minioConsolePort},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: minioDataVolume, MountPath: "/data"}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
			{
				// The sidecar waits for the server in the same pod, makes the
				// bucket, and then sleeps. Sleeping rather than exiting is what
				// keeps the pod out of a restart loop: a container that exits
				// zero in a StatefulSet pod is restarted, and the bucket would
				// be re-made on every restart forever.
				Name:            "bucket-init",
				Image:           minioClientImage,
				ImagePullPolicy: pullPolicyOf(cp.Spec.Source.Managed),
				Command: []string{"sh", "-c", `until mc alias set local http://localhost:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD"; do
  echo "Waiting for the object store..."; sleep 3;
done
mc mb --ignore-existing local/` + minioBucket + `
sleep infinity
`},
				Env: credentials,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			},
		},
	}
	scheduling(cp.Spec.Source.Managed, &spec)

	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: minioDataVolume},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(minioVolumeSize),
				},
			},
		},
	}
	// The store's volume comes from the same class as FoundationDB's, for the
	// same reason: it cannot be a class this operator provides, because the
	// control plane has to exist before any simplyblock volume can.
	if fdb := foundationDBSpecOf(cp); fdb != nil && fdb.StorageClassName != "" {
		claim.Spec.StorageClassName = ptr.To(fdb.StorageClassName)
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ComponentMinio,
			Namespace: cp.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: ComponentMinio,
			Replicas:    ptr.To(int32(1)),
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim},
		},
	}
}

// minioService is the address the store's consumers use. The console port is
// published beside the API port because the two are the same process, and
// reaching the console is how somebody looks at what is in the bucket.
func minioService(namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: ComponentMinio, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{appLabel: ComponentMinio},
			Ports: []corev1.ServicePort{
				{Name: "api", Port: minioAPIPort, TargetPort: intstrFromInt(minioAPIPort)},
				{Name: "console", Port: minioConsolePort, TargetPort: intstrFromInt(minioConsolePort)},
			},
		},
	}
}

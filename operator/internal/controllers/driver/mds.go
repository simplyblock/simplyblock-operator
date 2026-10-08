// The pNFS metadata server's pod: one StatefulSet per storage cluster, whose
// container runs mds-runner, which runs the guest serving that cluster's
// exports (design-pnfs-mds-vm.md §5).
//
// It is rendered here, beside the two plugins, because what it mounts is the
// driver's: the cluster configuration and secret, the TLS client certificate,
// and the csi-link token, and because its image defaults the way the driver's
// does. It is applied by the NFSExport reconciler when the first export of a
// storage cluster binds, not by this package's controller, since an idle guest
// per storage cluster is not wanted before any export needs one.

package driver

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// KVMCapableLabel marks a node whose /dev/kvm the metadata server pod can
// open, published by the CSI node plugin's probe. The pod is only scheduled
// onto nodes carrying it.
const KVMCapableLabel = kube.LabelKVMCapable

// KVMCapableValue is the value KVMCapableLabel carries on a capable node.
const KVMCapableValue = "true"

// MDSClusterLabel carries the storage cluster's full ID on the metadata server
// pod and its StatefulSet. The name only carries a prefix of it.
const MDSClusterLabel = "storage.simplyblock.io/cluster-id"

const (
	mdsContainerName = "mds-runner"
	// mdsImageTagPrefix sets the metadata server apart from the driver in the
	// repository both ship in.
	mdsImageTagPrefix = "pnfs-mds-"

	// The runner's own defaults, which the pod spec has to match: the state
	// disk's device path and the address its probes are served on.
	mdsStateClaim      = "state"
	mdsStateDevicePath = "/dev/mds-state"
	mdsProbePort       = 8080
	mdsNFSPort         = 2049

	// The runner gives a powered-down guest 20s to exit. The pod's grace
	// period has to leave it that, or kubelet kills QEMU first and the guest
	// never unmounts.
	mdsTerminationGraceSeconds = 30

	// The CRD's default for spec.pnfs.mds.stateSize, for an object admission
	// did not default.
	mdsDefaultStateSize = "1Gi"

	// The limits a metadata server pod gets when the spec states none. The
	// guest is sized from the pod's limits, and without one the downward API
	// reports the node's whole allocatable capacity, so the guest would take
	// the node.
	mdsDefaultCPULimit    = "2"
	mdsDefaultMemoryLimit = "2Gi"
)

// MDSStatefulSetName is the StatefulSet serving a storage cluster's exports.
// It carries the first group of the cluster's UUID: the whole UUID would push
// it past the 52 characters a StatefulSet name may have, and the label
// carries the rest.
func MDSStatefulSetName(d *simplyblockv1alpha2.SimplyblockDriver, clusterID string) string {
	short, _, _ := strings.Cut(clusterID, "-")
	return d.Name + "-pnfs-mds-" + short
}

// MDSPodName is the StatefulSet's only pod, the peer exports are bound to.
func MDSPodName(d *simplyblockv1alpha2.SimplyblockDriver, clusterID string) string {
	return MDSStatefulSetName(d, clusterID) + "-0"
}

// MDSObjects renders the metadata server for one storage cluster: its
// ServiceAccount, which carries no permissions and exists for the token
// csi-link authenticates, and its StatefulSet. The caller sets ownership, and
// chooses stateClass, the StorageClass of the state disk: choosing one takes a
// read of the cluster, which this package does not do.
func MDSObjects(
	d *simplyblockv1alpha2.SimplyblockDriver, clusterID, stateClass string,
) (*corev1.ServiceAccount, *appsv1.StatefulSet, error) {
	spec := d.Spec.PNFS.MDS
	if spec == nil {
		return nil, nil, errors.New("spec.pnfs.mds is unset, so the metadata server runs on nodes, not in a pod")
	}
	image, err := mdsImage(d)
	if err != nil {
		return nil, nil, err
	}

	n := names(d)
	name := MDSStatefulSetName(d, clusterID)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: n.mdsServiceAccount, Namespace: d.Namespace},
	}

	selector := map[string]string{"app": name}
	podLabels := map[string]string{"app": name, MDSClusterLabel: clusterID}
	nodeSelector := maps.Clone(spec.NodeSelector)
	if nodeSelector == nil {
		nodeSelector = map[string]string{}
	}
	nodeSelector[KVMCapableLabel] = KVMCapableValue

	volumes := slices.Concat(
		[]corev1.Volume{
			hostPathVolume("kvm", "/dev/kvm", ptr.To(corev1.HostPathCharDev)),
			hostPathVolume("tun", "/dev/net/tun", ptr.To(corev1.HostPathCharDev)),
			configMapVolume("csi-config", n.configMap, false),
			secretVolume("csi-secret", n.secretV2),
		},
		linkVolumes(),
	)
	// The control plane is called with the driver's own client certificate
	// (design-pnfs-mds-vm.md §6.4), the one the node plugin mounts.
	if v := tlsVolume(d, n.nodeClientSecret); v != nil {
		volumes = append(volumes, *v)
	}

	stateSize := spec.StateSize
	if stateSize.IsZero() {
		stateSize = resource.MustParse(mdsDefaultStateSize)
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.Namespace,
			Labels:    map[string]string{MDSClusterLabel: clusterID},
		},
		Spec: appsv1.StatefulSetSpec{
			// One, and never more: two guests mounting one filesystem lose
			// data, and a StatefulSet does not start a second pod while the
			// first is terminating.
			Replicas:    ptr.To(int32(1)),
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: map[string]string{utils.AnnotationLogCollector: "true"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            n.mdsServiceAccount,
					NodeSelector:                  nodeSelector,
					Tolerations:                   spec.Tolerations,
					TerminationGracePeriodSeconds: ptr.To(int64(mdsTerminationGraceSeconds)),
					Containers:                    []corev1.Container{mdsRunnerContainer(d, image)},
					Volumes:                       volumes,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: mdsStateClaim},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					VolumeMode:       ptr.To(corev1.PersistentVolumeBlock),
					StorageClassName: ptr.To(stateClass),
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: stateSize},
					},
				},
			}},
		},
	}
	return sa, sts, nil
}

// mdsRunnerContainer runs mds-runner. It is privileged because it opens
// /dev/kvm and /dev/net/tun, which an unprivileged container cannot without a
// device plugin, and it builds the guest's bridge and NAT rules in the pod's
// network namespace.
func mdsRunnerContainer(d *simplyblockv1alpha2.SimplyblockDriver, image string) corev1.Container {
	spec := d.Spec.PNFS.MDS
	return corev1.Container{
		Name:            mdsContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicy(d),
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		Args:            linkArgs(d),
		Env: slices.Concat(linkEnv(), []corev1.EnvVar{
			resourceEnv("MDS_CPU_LIMIT_MILLI", "limits.cpu", "1m"),
			resourceEnv("MDS_MEMORY_LIMIT_MIB", "limits.memory", "1Mi"),
		}, tlsEnv(d)),
		Resources: mdsResources(spec.Resources),
		Ports: []corev1.ContainerPort{
			{Name: "nfs", ContainerPort: mdsNFSPort, Protocol: corev1.ProtocolTCP},
			{Name: "probes", ContainerPort: mdsProbePort, Protocol: corev1.ProtocolTCP},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: named("probes")},
			},
			PeriodSeconds:    5,
			FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: named("probes")},
			},
			PeriodSeconds:    10,
			FailureThreshold: 3,
		},
		VolumeMounts: slices.Concat([]corev1.VolumeMount{
			{Name: "kvm", MountPath: "/dev/kvm"},
			{Name: "tun", MountPath: "/dev/net/tun"},
			{Name: "csi-config", MountPath: "/etc/spdkcsi-config/", ReadOnly: true},
			{Name: "csi-secret", MountPath: "/etc/spdkcsi-secret/", ReadOnly: true},
		}, tlsVolumeMount(d), linkVolumeMounts()),
		VolumeDevices: []corev1.VolumeDevice{{Name: mdsStateClaim, DevicePath: mdsStateDevicePath}},
	}
}

// mdsResources is the spec's requirements with a CPU and a memory limit
// defaulted where the spec states none.
func mdsResources(r corev1.ResourceRequirements) corev1.ResourceRequirements {
	out := *r.DeepCopy()
	if out.Limits == nil {
		out.Limits = corev1.ResourceList{}
	}
	if _, ok := out.Limits[corev1.ResourceCPU]; !ok {
		out.Limits[corev1.ResourceCPU] = resource.MustParse(mdsDefaultCPULimit)
	}
	if _, ok := out.Limits[corev1.ResourceMemory]; !ok {
		out.Limits[corev1.ResourceMemory] = resource.MustParse(mdsDefaultMemoryLimit)
	}
	return out
}

// resourceEnv hands the container one of its own resource values, in the unit
// divisor names.
func resourceEnv(name, resourceName, divisor string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			ResourceFieldRef: &corev1.ResourceFieldSelector{
				ContainerName: mdsContainerName,
				Resource:      resourceName,
				Divisor:       resource.MustParse(divisor),
			},
		},
	}
}

// mdsImage is spec.pnfs.mds.image, or the operator's own registry in the
// driver's repository, tagged with the operator's tag prefixed pnfs-mds-.
func mdsImage(d *simplyblockv1alpha2.SimplyblockDriver) (string, error) {
	if image := d.Spec.PNFS.MDS.Image; image != "" {
		return image, nil
	}
	operator := os.Getenv(OperatorImageEnv)
	if operator == "" {
		return "", fmt.Errorf(
			"spec.pnfs.mds.image is unset and nothing says which image to default to: "+
				"set it on the object, or set %s on the operator's deployment to the operator's own image",
			OperatorImageEnv)
	}
	driver, err := siblingImage(operator, csiDriverRepository)
	if err != nil {
		return "", fmt.Errorf(
			"spec.pnfs.mds.image is unset and the operator's own image %q cannot be turned into a "+
				"metadata server image: %w; set spec.pnfs.mds.image on the object instead",
			operator, err)
	}
	// siblingImage leaves exactly one tag and no digest, so the last colon
	// separates it.
	colon := strings.LastIndex(driver, ":")
	return driver[:colon+1] + mdsImageTagPrefix + driver[colon+1:], nil
}

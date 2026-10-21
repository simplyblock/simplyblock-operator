// The pNFS metadata server's pods: one StatefulSet per storage cluster, one
// pod per guest (the active and each standby), whose container runs mds-runner,
// which runs the guest serving that cluster's exports (design-pnfs-mds-vm.md §5,
// design-pnfs-mds-standby.md §5.1).
//
// It is rendered here, beside the two plugins, because what it mounts is the
// driver's: the cluster configuration and secret, the TLS client certificate,
// and the csi-link token, and because its image defaults the way the driver's
// does. It is applied by the NFSExport reconciler when the first export of a
// storage cluster binds, not by this package's controller, since an idle guest
// per storage cluster is not wanted before any export needs one.

package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// MDSTemplateHashAnnotation records MDSTemplateHash on the metadata server
// StatefulSet, so an operator release can tell whether the pod template is the
// one it would build.
const MDSTemplateHashAnnotation = "storage.simplyblock.io/mds-template-hash"

const (
	// MDSContainerName is the runner container. Its ID changes every time
	// kubelet restarts it, which is how a guest that crashed and came back in the
	// same pod is told apart from the one that was running.
	MDSContainerName = "mds-runner"
	// mdsImageTagPrefix sets the metadata server apart from the driver in the
	// repository both ship in.
	mdsImageTagPrefix = "pnfs-mds-"

	// The address the runner's probes are served on, which the pod spec has to
	// match.
	mdsProbePort = 8080
	mdsNFSPort   = 2049

	// The runner first drains the guest's requests (3s, and up to 5s to
	// install the drain) and then gives the powered-down guest 20s to exit.
	// The pod's grace period has to leave it all of that and a margin, or
	// kubelet kills QEMU first and the guest never unmounts.
	mdsTerminationGraceSeconds = 45

	// mdsDefaultStandbys is the CRD's default for spec.pnfs.mds.standbys, for
	// an object admission did not default.
	mdsDefaultStandbys = 1

	// The limits a metadata server pod gets when the spec states none. The
	// guest is sized from the pod's limits, and without one the downward API
	// reports the node's whole allocatable capacity, so the guest would take
	// the node.
	mdsDefaultCPULimit    = "2"
	mdsDefaultMemoryLimit = "2Gi"

	// The memory a metadata server pod reserves when the spec states no
	// request. The guest hands the memory it frees back to the node through its
	// balloon's free page reporting, so the pod uses what the guest does and
	// may grow to its limit. A pnfs-fio run peaked at 315Mi.
	mdsDefaultMemoryRequest = "512Mi"
)

// MDSStatefulSetName is the StatefulSet serving a storage cluster's exports.
//
// Built by atlas-lib's kube.Formula rather than by hand, held to the 52
// characters a StatefulSet name may have: its pods carry a
// controller-revision-hash label of the name plus eleven characters, and a
// label is 63. The whole cluster UUID does not fit beside the prefix, so the
// formula keeps what does and appends a digest of the full ID. Cutting the ID
// to its first group alone, as this once did, gave two clusters sharing that
// group one StatefulSet, and the second cluster's exports bound to the first
// one's pod. The label carries the full ID.
func MDSStatefulSetName(d *simplyblockv1alpha2.SimplyblockDriver, clusterID string) string {
	return kube.Formula{Prefix: d.Name + "-pnfs-mds-", Limit: mdsStatefulSetNameLimit}.
		Derive(clusterID).Value
}

// MDSMinMemoryLimit is the smallest memory limit the metadata server pod may
// have. The runner gives the guest the limit less 256Mi for QEMU, and a guest
// with less than 256Mi does not boot.
const MDSMinMemoryLimit = "512Mi"

// MDSResourcesProblem says why spec.pnfs.mds's resources cannot run the guest,
// or returns "" when they can. Only an explicit memory limit is checked: an
// unset one is defaulted to mdsDefaultMemoryLimit.
//
// It is not a CEL rule on the CRD because it cannot be one: limits is an
// unbounded map of quantities, and the apiserver refuses to install a schema
// whose rule over it has no bounded cost.
func MDSResourcesProblem(spec *simplyblockv1alpha2.DriverPNFSMDS) string {
	if spec == nil {
		return ""
	}
	mem, ok := spec.Resources.Limits[corev1.ResourceMemory]
	if !ok || mem.Cmp(resource.MustParse(MDSMinMemoryLimit)) >= 0 {
		return ""
	}
	return fmt.Sprintf("spec.pnfs.mds.resources.limits.memory is %s, under the %s the metadata "+
		"server needs: its guest gets the limit less 256Mi for QEMU", mem.String(), MDSMinMemoryLimit)
}

// UpdateMDS gives an existing metadata server StatefulSet the pod template this
// release builds, when its recorded hash says it has another one, and reports
// whether it wrote. Only the template and the hash are written, as a merge
// patch of those two and nothing else: the takeover writes the active record
// and the finalizer on the same object, and an update of the whole object would
// overwrite a record moved since this read or fail on the conflict. The replica
// count is the scale step's. A template edited by hand keeps its hash, and is
// left as it is.
func UpdateMDS(
	ctx context.Context, c client.Client, d *simplyblockv1alpha2.SimplyblockDriver,
	existing *appsv1.StatefulSet, data *MDSDataAttachment,
) (bool, error) {
	_, desired, err := MDSObjects(d, existing.Labels[MDSClusterLabel], data)
	if err != nil {
		return false, err
	}
	want := desired.Annotations[MDSTemplateHashAnnotation]
	if existing.Annotations[MDSTemplateHashAnnotation] == want {
		return false, nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Template = desired.Spec.Template
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[MDSTemplateHashAnnotation] = want
	if err := c.Patch(ctx, existing, patch); err != nil {
		return false, fmt.Errorf("updating the metadata server StatefulSet %s: %w", existing.Name, err)
	}
	return true, nil
}

// mdsStatefulSetNameLimit is the longest a StatefulSet name may be; see
// MDSStatefulSetName.
const mdsStatefulSetNameLimit = 52

// MDSPodName is the StatefulSet's pod of one ordinal.
func MDSPodName(d *simplyblockv1alpha2.SimplyblockDriver, clusterID string, ordinal int32) string {
	return fmt.Sprintf("%s-%d", MDSStatefulSetName(d, clusterID), ordinal)
}

// MDSStandbys is spec.pnfs.mds.standbys with the CRD's default applied.
func MDSStandbys(spec *simplyblockv1alpha2.DriverPNFSMDS) int32 {
	if spec == nil || spec.Standbys == nil {
		return mdsDefaultStandbys
	}
	return *spec.Standbys
}

// MDSObjects renders the metadata server for one storage cluster: its
// ServiceAccount, which carries no permissions and exists for the token
// csi-link authenticates, and its StatefulSet of one pod for the active and one
// for each standby. The caller sets ownership. data attaches the pods to the
// storage data network, and is nil for pods on the pod network alone; see
// EnsureMDSDataNetwork.
func MDSObjects(
	d *simplyblockv1alpha2.SimplyblockDriver, clusterID string, data *MDSDataAttachment,
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
	nodeSelector := mdsNodeSelector(spec)

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

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.Namespace,
			Labels:    map[string]string{MDSClusterLabel: clusterID},
		},
		Spec: appsv1.StatefulSetSpec{
			// The active and each standby. Two guests mounting one filesystem lose
			// data, which the takeover's fencing prevents, not this count.
			Replicas:    ptr.To(1 + MDSStandbys(spec)),
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: selector},
			// Parallel, so a pod stuck on a lost node does not stop the other
			// ordinal from starting. OnDelete, because the operator rolls the
			// pods itself, and the StatefulSet controller's order ignores which
			// one is active.
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: map[string]string{utils.AnnotationLogCollector: "true"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            n.mdsServiceAccount,
					NodeSelector:                  nodeSelector,
					Affinity:                      mdsPodAntiAffinity(selector),
					Tolerations:                   spec.Tolerations,
					TerminationGracePeriodSeconds: ptr.To(int64(mdsTerminationGraceSeconds)),
					Containers:                    []corev1.Container{mdsRunnerContainer(d, clusterID, image, data)},
					Volumes:                       volumes,
				},
			},
		},
	}
	if data != nil && spec.DataNetwork != nil {
		subnet, err := netip.ParsePrefix(spec.DataNetwork.Subnet)
		if err != nil {
			return nil, nil, fmt.Errorf("spec.pnfs.mds.dataNetwork.subnet %q: %w", spec.DataNetwork.Subnet, err)
		}
		if err := dataNetworkPodSpec(&sts.Spec.Template, subnet, spec.DataNetwork, data); err != nil {
			return nil, nil, err
		}
	}
	hash, err := MDSTemplateHash(sts)
	if err != nil {
		return nil, nil, err
	}
	sts.Annotations = map[string]string{MDSTemplateHashAnnotation: hash}
	return sa, sts, nil
}

// mdsPodAntiAffinity requires every pod of the StatefulSet to run on a node of
// its own: two guests on one node are lost together with it.
func mdsPodAntiAffinity(selector map[string]string) *corev1.Affinity {
	return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: selector},
			TopologyKey:   corev1.LabelHostname,
		}},
	}}
}

// MDSTemplateHash is the hash of a metadata server StatefulSet's pod template,
// which is what tells a StatefulSet built by another operator release from a
// current one. It is taken over the template as built, not as read back:
// Kubernetes fills in defaults on the way in, so a comparison of templates
// would always differ.
func MDSTemplateHash(sts *appsv1.StatefulSet) (string, error) {
	raw, err := json.Marshal(sts.Spec.Template)
	if err != nil {
		return "", fmt.Errorf("encoding the metadata server pod template: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16], nil
}

// mdsRunnerContainer runs mds-runner. It is privileged because it opens
// /dev/kvm and /dev/net/tun, which an unprivileged container cannot without a
// device plugin, and it builds the guest's bridge and NAT rules in the pod's
// network namespace.
func mdsRunnerContainer(
	d *simplyblockv1alpha2.SimplyblockDriver, clusterID, image string, data *MDSDataAttachment,
) corev1.Container {
	spec := d.Spec.PNFS.MDS
	// The guest reaches its own node's storage node through the mds-proxy
	// there (design-pnfs-mds-vm.md §8.5).
	// Every guest boots with the StatefulSet's name as its hostname, so a client
	// of one ordinal reclaims on another under the same server owner. The pod's
	// own name is what the runner registers as.
	args := append(linkArgs(d), "--host-proxy="+mdsProxyAddress,
		"--hostname="+MDSStatefulSetName(d, clusterID), "--pod-name=$(POD_NAME)")
	if data != nil {
		// The guest's traffic to the data network leaves through it.
		args = append(args, "--data-interface="+MDSDataInterface)
	}
	return corev1.Container{
		Name:            MDSContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicy(d),
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		Args:            args,
		Env: slices.Concat(linkEnv(), []corev1.EnvVar{
			resourceEnv("MDS_CPU_LIMIT_MILLI", "limits.cpu", "1m"),
			resourceEnv("MDS_MEMORY_LIMIT_MIB", "limits.memory", "1Mi"),
			hostIPEnvVar(),
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
	}
}

// mdsResources is the spec's requirements with a CPU and a memory limit, and a
// memory request, defaulted where the spec states none.
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
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	if _, ok := out.Requests[corev1.ResourceMemory]; !ok {
		request := resource.MustParse(mdsDefaultMemoryRequest)
		if limit := out.Limits[corev1.ResourceMemory]; limit.Cmp(request) < 0 {
			request = limit
		}
		out.Requests[corev1.ResourceMemory] = request
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
				ContainerName: MDSContainerName,
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

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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
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

// mdsStatefulSetNameLimit is the longest a StatefulSet name may be; see
// MDSStatefulSetName.
const mdsStatefulSetNameLimit = 52

// MDSStateClassSuffix ends the name of every storage cluster's state disk
// class, and is what the admission policy reserving those classes matches on.
const MDSStateClassSuffix = "-pnfs-mds-state"

// MDSStateClassManagedBy is the managed-by value on a state disk class: its
// own, so that the pool controller, which acts on classes carrying its value,
// leaves it alone.
const MDSStateClassManagedBy = "pnfs-mds"

// MDSStateClassName is the StorageClass of one storage cluster's state disk,
// ending in MDSStateClassSuffix. A StorageClass name may be 253 characters, so
// the whole cluster ID fits and the name is unique without a digest.
func MDSStateClassName(d *simplyblockv1alpha2.SimplyblockDriver, clusterID string) string {
	return kube.Formula{Prefix: d.Name + "-", Suffix: MDSStateClassSuffix}.Derive(clusterID).Value
}

// MDSStatePolicyName names the admission policy reserving the driver's state
// disk classes, and its binding.
func MDSStatePolicyName(d *simplyblockv1alpha2.SimplyblockDriver) string {
	return d.Name + MDSStateClassSuffix
}

// mdsStateParams are the parameters a state disk class takes from the class it
// is derived from: where the volume lives and how it is reached and stored.
// An allowlist rather than a blocklist, because a parameter added to user
// classes later, a cap or a placement hint, would otherwise reach the state
// disk without anyone deciding it should. Left out on purpose: the QoS caps,
// which belong to the user volumes the class was written for, and the
// filesystem, since the state disk is a raw block device.
var mdsStateParams = []string{kube.ParamClusterID, kube.ParamPool, kube.ParamFabric, kube.ParamEncryption}

// MDSStateClass is the StorageClass of one storage cluster's state disk,
// derived from source, a simplyblock class of the same cluster.
//
// It is labeled with a managed-by value of its own and with no pool, on
// purpose. A pool label would assign it to the pool, whose deletion waits on
// its classes, and nothing removes this one. A StorageClass is cluster-scoped,
// so it cannot be owned by the driver either.
func MDSStateClass(
	d *simplyblockv1alpha2.SimplyblockDriver, clusterID string, source *storagev1.StorageClass,
) *storagev1.StorageClass {
	params := map[string]string{}
	for _, key := range mdsStateParams {
		if v, ok := source.Parameters[key]; ok {
			params[key] = v
		}
	}
	params[kube.ParamClusterID] = clusterID
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   MDSStateClassName(d, clusterID),
			Labels: map[string]string{managedByLabel: MDSStateClassManagedBy},
		},
		Provisioner:          driverName(d),
		Parameters:           params,
		ReclaimPolicy:        ptr.To(corev1.PersistentVolumeReclaimDelete),
		VolumeBindingMode:    ptr.To(storagev1.VolumeBindingImmediate),
		AllowVolumeExpansion: ptr.To(true),
	}
}

// MDSStatePolicy is the admission policy reserving the state disk classes,
// and its binding.
//
// Kubernetes has no permission for using a StorageClass: anyone who may create
// a claim in any namespace may name any class. The policy refuses a new claim
// whose class name ends in MDSStateClassSuffix, and the binding applies it to
// every namespace but the operator's, where the StatefulSet controller creates
// the state disk's claim. Whoever may create claims there can do far more
// than this already. An in-process policy rather than a webhook: it costs
// nothing to serve and cannot make every claim in the cluster wait on the
// operator.
func MDSStatePolicy(
	d *simplyblockv1alpha2.SimplyblockDriver, operatorNamespace string,
) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	name := MDSStatePolicyName(d)
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: ptr.To(admissionregistrationv1.Fail),
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						// CREATE only: a claim's class cannot change afterward.
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{""},
							APIVersions: []string{"v1"},
							Resources:   []string{"persistentvolumeclaims"},
						},
					},
				}},
			},
			Validations: []admissionregistrationv1.Validation{{
				Expression: fmt.Sprintf("!has(object.spec.storageClassName) || "+
					"!object.spec.storageClassName.endsWith('%s')", MDSStateClassSuffix),
				Message: "this StorageClass is reserved for the pNFS metadata server's state disk",
			}},
		},
	}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			MatchResources: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      corev1.LabelMetadataName,
					Operator: metav1.LabelSelectorOpNotIn,
					Values:   []string{operatorNamespace},
				}}},
			},
		},
	}
	return policy, binding
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
	hash, err := MDSTemplateHash(sts)
	if err != nil {
		return nil, nil, err
	}
	sts.Annotations = map[string]string{MDSTemplateHashAnnotation: hash}
	return sa, sts, nil
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
func mdsRunnerContainer(d *simplyblockv1alpha2.SimplyblockDriver, image string) corev1.Container {
	spec := d.Spec.PNFS.MDS
	return corev1.Container{
		Name:            MDSContainerName,
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

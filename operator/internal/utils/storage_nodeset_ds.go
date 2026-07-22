package utils

import (
	"fmt"
	"strings"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// defaultInitContainerResources are applied when the user has not set
// InitContainerResources on the StorageNodeSet CR.
var defaultInitContainerResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("200m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
}

// defaultContainerResources are applied when the user has not set
// ContainerResources on the StorageNodeSet CR. No CPU limit is set because
// SPDK uses busy-polling and a hard CPU ceiling would degrade storage
// performance. Memory limits are enforced to allow kubelet eviction.
var defaultContainerResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("200m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	},
}

func BuildStorageNodeSetDaemonSet(sn *simplyblockv1alpha1.StorageNodeSet, tlsEnabled bool, tlsMutualEnabled bool, tlsProvider, tlsSecretResourceVersion string) *appsv1.DaemonSet {

	labels := map[string]string{
		"app":                 "storage-node",
		"simplyblock-cluster": sn.Spec.ClusterName,
	}

	image := sn.Spec.ClusterImage

	// Build the fleet-level (non-overridable) args that are always appended.
	// Per-node args (max-lvol, cores-percentage, pci-*, device-*, size-range)
	// are read at runtime from the per-node ConfigMap via the init script.
	fleetArgs := ""
	if len(sn.Spec.SocketsToUse) > 0 {
		fleetArgs += " --sockets-to-use=" + JoinList(sn.Spec.SocketsToUse)
	}
	if sn.Spec.NodesPerSocket != nil {
		fleetArgs += " --nodes-per-socket=" + Int32PtrToString(sn.Spec.NodesPerSocket)
	}

	// The init container sources the per-node env file (written by node-env-writer)
	// so that node_configure.py receives per-node values for each pod.
	initScript := `set -e
[ -f /etc/node-env/env.sh ] && . /etc/node-env/env.sh
ARGS="--max-lvol=${MAX_LVOL:-0} --max-size=\"${MAX_SIZE:-}\""
[ -n "${CORES_PERCENTAGE}" ] && ARGS="${ARGS} --cores-percentage=\"${CORES_PERCENTAGE}\""
[ -n "${PCI_ALLOWED}" ] && ARGS="${ARGS} --pci-allowed=\"${PCI_ALLOWED}\""
[ -n "${PCI_BLOCKED}" ] && ARGS="${ARGS} --pci-blocked=\"${PCI_BLOCKED}\""
[ -n "${NVME_DEVICES}" ] && ARGS="${ARGS} --nvme-devices=\"${NVME_DEVICES}\""
[ -n "${DEVICE_MODEL}" ] && ARGS="${ARGS} --device-model=\"${DEVICE_MODEL}\""
[ -n "${SIZE_RANGE}" ] && ARGS="${ARGS} --size-range=\"${SIZE_RANGE}\""
ARGS="${ARGS}` + fleetArgs + `"
eval sudo -E python3 simplyblock_web/node_configure.py ${ARGS}
`
	initCmd := []string{"sh", "-c", initScript}

	// nodeEnvWriterScript copies the per-node env file from the mounted ConfigMap
	// (keyed by hostname) into the shared node-env emptyDir volume.
	nodeEnvWriterScript := `mkdir -p /etc/node-env
if [ -f /etc/per-node-config/${HOSTNAME} ]; then
  cp /etc/per-node-config/${HOSTNAME} /etc/node-env/env.sh
else
  touch /etc/node-env/env.sh
fi`
	nodeEnvWriterCmd := []string{"sh", "-c", nodeEnvWriterScript}

	imagePullPolicy := sn.Spec.ImagePullPolicy
	if imagePullPolicy == "" {
		imagePullPolicy = corev1.PullAlways
	}

	mainEnv := []corev1.EnvVar{
		{Name: "UBUNTU_HOST", Value: BoolPtrToString(sn.Spec.UbuntuHost)},
		{Name: "OPENSHIFT_CLUSTER", Value: BoolPtrToString(sn.Spec.OpenShiftCluster)},
		{Name: "SKIP_KUBELET_CONFIGURATION", Value: BoolPtrToString(sn.Spec.SkipKubeletConfiguration)},
		{Name: "SIMPLY_BLOCK_DOCKER_IMAGE", Value: image},
		{Name: "HOSTNAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
		{Name: "CPU_TOPOLOGY_ENABLED", Value: BoolPtrToString(sn.Spec.EnableCpuTopology)},
	}
	if sn.Spec.MaxParallelNodeAdds != nil {
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "MAX_PARALLEL_NODE_ADDS", Value: fmt.Sprintf("%d", *sn.Spec.MaxParallelNodeAdds)})
	}
	if sn.Spec.OpenShiftMachineConfigPool != "" {
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "OPENSHIFT_MCP", Value: sn.Spec.OpenShiftMachineConfigPool})
	}
	if sn.Spec.ReservedSystemCPU != "" {
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "RESERVED_SYSTEM_CPUS", Value: sn.Spec.ReservedSystemCPU})
	}
	if tlsMutualEnabled {
		mainEnv = append(mainEnv,
			corev1.EnvVar{Name: "SB_TLS_SERVE", Value: "true"},
			corev1.EnvVar{Name: "SB_TLS_CONNECT", Value: "authenticated"},
			corev1.EnvVar{Name: "SB_TLS_CLIENT_AUTH", Value: "required"},
			corev1.EnvVar{Name: "SB_TLS_PROVIDER", Value: NormalizeTLSProvider(tlsProvider)},
		)
	} else if tlsEnabled {
		mainEnv = append(mainEnv,
			corev1.EnvVar{Name: "SB_TLS_SERVE", Value: "true"},
			corev1.EnvVar{Name: "SB_TLS_CONNECT", Value: "anonymous"},
			corev1.EnvVar{Name: "SB_TLS_CLIENT_AUTH", Value: "optional"},
			corev1.EnvVar{Name: "SB_TLS_PROVIDER", Value: NormalizeTLSProvider(tlsProvider)},
		)
	}

	perNodeConfigMapName := sn.Name + "-per-node-config"

	volumes := []corev1.Volume{
		// per-node-config: ConfigMap with one key per worker hostname.
		// Written by reconcilePerNodeConfigMap; read by node-env-writer init container.
		{
			Name: "per-node-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: perNodeConfigMapName},
					Optional:             BoolPtr(true),
				},
			},
		},
		// node-env: emptyDir shared between init containers and the main container.
		// node-env-writer writes /etc/node-env/env.sh; others source it.
		{
			Name:         "node-env",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: "dev-vol",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev",
				},
			},
		},
		{
			Name: "etc-simplyblock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/simplyblock",
				},
			},
		},
		{
			Name: "host-sys",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/sys",
				},
			},
		},
		{
			Name: "host-mnt",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/mnt",
				},
			},
		},
		{
			Name: "host-modules",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/lib/modules",
				},
			},
		},
		{
			Name: "var-run-simplyblock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/run/simplyblock",
					Type: func() *corev1.HostPathType { t := corev1.HostPathDirectoryOrCreate; return &t }(),
				},
			},
		},
	}

	nodeEnvMount := corev1.VolumeMount{Name: "node-env", MountPath: "/etc/node-env"}

	initMounts := []corev1.VolumeMount{
		{Name: "etc-simplyblock", MountPath: "/etc/simplyblock"},
		{Name: "host-modules", MountPath: "/lib/modules", ReadOnly: true},
		{Name: "host-mnt", MountPath: "/mnt"},
		nodeEnvMount,
	}

	mainMounts := []corev1.VolumeMount{
		{Name: "dev-vol", MountPath: "/dev"},
		{Name: "etc-simplyblock", MountPath: "/etc/simplyblock"},
		{Name: "host-sys", MountPath: "/sys"},
		{Name: "var-run-simplyblock", MountPath: "/var/run/simplyblock"},
		nodeEnvMount,
	}

	readinessProbe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/snode/check",
				Port: intstr.FromInt(5000),
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       5,
	}

	if tlsMutualEnabled {
		// kubelet does not present a client certificate, so an HTTPS probe
		// would be rejected by the snode API. Fall back to a TCP check.
		readinessProbe.ProbeHandler = corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt(5000),
			},
		}
	} else if tlsEnabled {
		readinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
	}

	if tlsEnabled {
		volumes = append(volumes, buildStorageNodeSetTLSVolume(tlsProvider))

		tlsMounts := []corev1.VolumeMount{
			{Name: "tls", MountPath: "/etc/simplyblock/tls", ReadOnly: true},
		}
		initMounts = append(initMounts, tlsMounts...)
		mainMounts = append(mainMounts, tlsMounts...)
	}

	var podAnnotations map[string]string
	if tlsEnabled && tlsSecretResourceVersion != "" {
		podAnnotations = map[string]string{
			AnnotationTLSSecretRevision: tlsSecretResourceVersion,
		}
	}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-storage-node-ds-" + sn.Spec.ClusterName,
			Namespace: sn.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 1,
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "simplyblock-storage-node-sa",
					HostNetwork:        true,
					Tolerations:        sn.Spec.Tolerations,
					NodeSelector: map[string]string{
						"io.simplyblock.node-type": "simplyblock-storage-plane-" + sn.Spec.ClusterName,
					},

					Volumes: volumes,

					InitContainers: []corev1.Container{
						// node-env-writer: copies the per-node env file from the
						// ConfigMap (keyed by hostname) into the shared node-env volume
						// so both the config-generator and the main container can source it.
						{
							Name:            "node-env-writer",
							Image:           image,
							ImagePullPolicy: imagePullPolicy,
							Command:         nodeEnvWriterCmd,
							Env: []corev1.EnvVar{
								{Name: "HOSTNAME", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
								}},
							},
							Resources: effectiveResources(sn.Spec.InitContainerResources, defaultInitContainerResources),
							VolumeMounts: []corev1.VolumeMount{
								{Name: "per-node-config", MountPath: "/etc/per-node-config", ReadOnly: true},
								nodeEnvMount,
							},
						},
						// s-node-api-config-generator: runs node_configure.py with
						// per-node values sourced from /etc/node-env/env.sh.
						{
							Name:            "s-node-api-config-generator",
							Image:           image,
							ImagePullPolicy: imagePullPolicy,
							Command:         initCmd,
							SecurityContext: &corev1.SecurityContext{Privileged: BoolPtr(true)},
							VolumeMounts:    initMounts,
							Resources:       effectiveResources(sn.Spec.InitContainerResources, defaultInitContainerResources),
							Env: []corev1.EnvVar{
								{Name: "HOSTNAME", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
								}},
							},
						},
					},

					Containers: []corev1.Container{
						{
							Name:            "s-node-api-container",
							Image:           image,
							ImagePullPolicy: imagePullPolicy,
							Command: []string{"sh", "-c",
								`[ -f /etc/node-env/env.sh ] && . /etc/node-env/env.sh
exec sudo -E python3 simplyblock_web/node_webapp.py storage_node_k8s`,
							},
							SecurityContext: &corev1.SecurityContext{Privileged: BoolPtr(true)},
							Resources:       effectiveResources(sn.Spec.ContainerResources, defaultContainerResources),
							ReadinessProbe:  readinessProbe,
							Env:             mainEnv,
							VolumeMounts:    mainMounts,
						},
					},
				},
			},
		},
	}
}

// buildStorageNodeSetTLSVolume returns a Volume that exposes tls.crt, tls.key,
// and ca.crt at /etc/simplyblock/tls. cert-manager already bundles the CA in
// the Secret, so a plain Secret volume is enough; OpenShift's serving-cert
// controller emits the CA via a separate ConfigMap, so the two sources are
// combined through a projected volume.
func buildStorageNodeSetTLSVolume(tlsProvider string) corev1.Volume {
	if IsCertManagerTLSProvider(tlsProvider) {
		return corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: SecretNameStorageNodeSetAPITLS,
				},
			},
		}
	}

	return corev1.Volume{
		Name: "tls",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: SecretNameStorageNodeSetAPITLS,
							},
						},
					},
					{
						ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "simplyblock-certificate-authority",
							},
							Items: []corev1.KeyToPath{
								{Key: "service-ca.crt", Path: "ca.crt"},
							},
						},
					},
				},
			},
		},
	}
}

func BuildStorageNodeSetServiceAccount(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ServiceAccount",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-storage-node-sa",
			Namespace: namespace,
		},
	}
}

func BuildStorageNodeSetClusterRole(isOpenShift bool) *rbacv1.ClusterRole {
	baseRules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"pods", "pods/exec"},
			Verbs:     []string{"list", "get", "create", "delete", "watch"},
		},
		{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments"},
			Verbs:     []string{"create", "delete"},
		},
		{
			APIGroups: []string{"batch"},
			Resources: []string{"jobs"},
			Verbs:     []string{"create", "delete", "get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"list", "get", "update", "patch", "watch"},
		},
	}

	if isOpenShift {
		baseRules = append(baseRules,
			rbacv1.PolicyRule{
				APIGroups: []string{"machineconfiguration.openshift.io"},
				Resources: []string{"machineconfigs", "machineconfigpools", "kubeletconfigs"},
				Verbs:     []string{"list", "get", "create", "update", "patch", "watch"},
			})
	}

	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRole",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "simplyblock-storage-node-role",
		},
		Rules: baseRules,
	}
}

// StorageNodeSetAPIAddress returns the per-pod headless-service DNS address that
// the cluster control-plane uses to reach a storage-node-api pod backing the
// given worker node.
func StorageNodeSetAPIAddress(workerNode, namespace string) string {
	return fmt.Sprintf("%s.simplyblock-storage-node-api.%s.svc.cluster.local:5000", nodeHostnameLabel(workerNode), namespace)
}

func BuildStorageNodeSetService(sn *simplyblockv1alpha1.StorageNodeSet, tlsEnabled bool, tlsProvider string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "simplyblock-storage-node-api",
			Namespace:   sn.Namespace,
			Annotations: ServingCertServiceAnnotations(tlsEnabled, tlsProvider, SecretNameStorageNodeSetAPITLS),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{
					Name:     "api",
					Port:     5000,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}
}

func BuildStorageNodeSetEndpointSlice(sn *simplyblockv1alpha1.StorageNodeSet, nodeIPs map[string]string) *discoveryv1.EndpointSlice {
	protocol := corev1.ProtocolTCP
	port := int32(5000)
	portName := "api"

	endpoints := make([]discoveryv1.Endpoint, 0, len(nodeIPs))
	for nodeName, ip := range nodeIPs {
		hostname := nodeHostnameLabel(nodeName)
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses: []string{ip},
			Hostname:  &hostname,
		})
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-storage-node-api-endpoints",
			Namespace: sn.Namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name": "simplyblock-storage-node-api",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     &portName,
				Protocol: &protocol,
				Port:     &port,
			},
		},
	}
}

// SpdkProxyEndpoint describes a single spdk-proxy pod instance that backs the
// headless spdk-proxy Service.
type SpdkProxyEndpoint struct {
	NodeName string
	PodIP    string
	RpcPort  int32
}

func BuildSpdkProxyService(sn *simplyblockv1alpha1.StorageNodeSet, tlsEnabled bool, tlsProvider string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "simplyblock-spdk-proxy",
			Namespace:   sn.Namespace,
			Annotations: ServingCertServiceAnnotations(tlsEnabled, tlsProvider, SecretNameSpdkProxyTLS),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
		},
	}
}

// BuildSpdkProxyEndpointSlice builds one EndpointSlice for a single RPC_PORT.
// Each endpoint resolves <nodeLabel>.spdk-proxy.<ns>.svc to the node's IP via
// the headless Service's per-endpoint hostname DNS. The hostname is the node
// name truncated at the first dot so FQDN-style node names stay within the
// 63-char DNS label limit.
func BuildSpdkProxyEndpointSlice(
	sn *simplyblockv1alpha1.StorageNodeSet,
	rpcPort int32,
	endpoints []SpdkProxyEndpoint,
) (*discoveryv1.EndpointSlice, error) {
	protocol := corev1.ProtocolTCP
	port := rpcPort
	portName := "proxy"
	ready := true

	eps := make([]discoveryv1.Endpoint, 0, len(endpoints))
	seen := make(map[string]string, len(endpoints))
	for _, e := range endpoints {
		label := nodeHostnameLabel(e.NodeName)
		if prev, ok := seen[label]; ok && prev != e.NodeName {
			return nil, fmt.Errorf("spdk-proxy endpoint hostname collision on label %q (nodes %q and %q): node names share the same first DNS segment", label, prev, e.NodeName)
		}
		seen[label] = e.NodeName

		hostname := label
		eps = append(eps, discoveryv1.Endpoint{
			Addresses:  []string{e.PodIP},
			Hostname:   &hostname,
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		})
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("spdk-proxy-endpoints-%d", rpcPort),
			Namespace: sn.Namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name": "simplyblock-spdk-proxy",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   eps,
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     &portName,
				Protocol: &protocol,
				Port:     &port,
			},
		},
	}, nil
}

func nodeHostnameLabel(nodeName string) string {
	label, _, _ := strings.Cut(nodeName, ".")
	return label
}

func BuildStorageNodeSetClusterRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRoleBinding",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("simplyblock-storage-node-binding-%s", namespace),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "simplyblock-storage-node-sa",
				Namespace: namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "simplyblock-storage-node-role",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
}

// effectiveResources returns user if the user has set any requests or limits,
// otherwise falls back to def. This ensures defaults are always applied while
// still allowing full user override.
func effectiveResources(user, def corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(user.Requests) > 0 || len(user.Limits) > 0 {
		return user
	}
	return def
}

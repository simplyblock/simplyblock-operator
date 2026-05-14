package utils

import (
	"fmt"
	"strings"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func BuildStorageNodeDaemonSet(sn *simplyblockv1alpha1.StorageNode, tlsEnabled bool, tlsMutualEnabled bool, tlsProvider, tlsSecretResourceVersion string) *appsv1.DaemonSet {

	labels := map[string]string{
		"app":                 "storage-node",
		"simplyblock-cluster": sn.Spec.ClusterName,
	}

	image := sn.Spec.ClusterImage
	initCmd := []string{
		"sudo", "-E",
		"python3",
		"simplyblock_web/node_configure.py",
		"--max-lvol=" + Int32PtrToString(sn.Spec.MaxLogicalVolumeCount),
		"--max-size=" + sn.Spec.MaxSize,
	}

	if len(sn.Spec.PcieAllowList) > 0 {
		initCmd = append(initCmd, "--pci-allowed="+JoinList(sn.Spec.PcieAllowList))
	}
	if len(sn.Spec.PcieDenyList) > 0 {
		initCmd = append(initCmd, "--pci-blocked="+JoinList(sn.Spec.PcieDenyList))
	}
	if len(sn.Spec.DeviceNames) > 0 {
		initCmd = append(initCmd, "--nvme-devices="+JoinList(sn.Spec.DeviceNames))
	}
	if len(sn.Spec.SocketsToUse) > 0 {
		initCmd = append(initCmd, "--sockets-to-use="+JoinList(sn.Spec.SocketsToUse))
	}
	if sn.Spec.NodesPerSocket != nil {
		initCmd = append(initCmd, "--nodes-per-socket="+Int32PtrToString(sn.Spec.NodesPerSocket))
	}
	if sn.Spec.PcieModel != "" {
		initCmd = append(initCmd, "--device-model="+sn.Spec.PcieModel)
	}
	if sn.Spec.DriveSizeRange != "" {
		initCmd = append(initCmd, "--size-range="+sn.Spec.DriveSizeRange)
	}
	if sn.Spec.CorePercentage != nil {
		initCmd = append(initCmd, "--cores-percentage="+Int32PtrToString(sn.Spec.CorePercentage))
	}

	mainEnv := []corev1.EnvVar{
		{Name: "UBUNTU_HOST", Value: BoolPtrToString(sn.Spec.UbuntuHost)},
		{Name: "OPENSHIFT_CLUSTER", Value: BoolPtrToString(sn.Spec.OpenShiftCluster)},
		{Name: "CPU_TOPOLOGY_ENABLED", Value: BoolPtrToString(sn.Spec.EnableCpuTopology)},
		{Name: "SKIP_KUBELET_CONFIGURATION", Value: BoolPtrToString(sn.Spec.SkipKubeletConfiguration)},
		{Name: "SIMPLY_BLOCK_DOCKER_IMAGE", Value: image},
		{Name: "HOSTNAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
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

	volumes := []corev1.Volume{
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
	}

	initMounts := []corev1.VolumeMount{
		{Name: "etc-simplyblock", MountPath: "/etc/simplyblock"},
		{Name: "host-modules", MountPath: "/lib/modules", ReadOnly: true},
		{Name: "host-mnt", MountPath: "/mnt"},
	}

	mainMounts := []corev1.VolumeMount{
		{Name: "dev-vol", MountPath: "/dev"},
		{Name: "etc-simplyblock", MountPath: "/etc/simplyblock"},
		{Name: "host-sys", MountPath: "/sys"},
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
		volumes = append(volumes, buildStorageNodeTLSVolume(tlsProvider))

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
						{
							Name:            "s-node-api-config-generator",
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         initCmd,
							SecurityContext: &corev1.SecurityContext{Privileged: BoolPtr(true)},
							VolumeMounts:    initMounts,
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
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"sudo", "-E", "python3", "simplyblock_web/node_webapp.py", "storage_node_k8s",
							},
							SecurityContext: &corev1.SecurityContext{Privileged: BoolPtr(true)},

							ReadinessProbe: readinessProbe,
							Env:            mainEnv,
							VolumeMounts:   mainMounts,
						},
					},
				},
			},
		},
	}
}

// buildStorageNodeTLSVolume returns a Volume that exposes tls.crt, tls.key,
// and ca.crt at /etc/simplyblock/tls. cert-manager already bundles the CA in
// the Secret, so a plain Secret volume is enough; OpenShift's serving-cert
// controller emits the CA via a separate ConfigMap, so the two sources are
// combined through a projected volume.
func buildStorageNodeTLSVolume(tlsProvider string) corev1.Volume {
	if IsCertManagerTLSProvider(tlsProvider) {
		return corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: SecretNameStorageNodeAPITLS,
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
								Name: SecretNameStorageNodeAPITLS,
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

func BuildStorageNodeServiceAccount(namespace string) *corev1.ServiceAccount {
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

func BuildStorageNodeClusterRole(isOpenShift bool) *rbacv1.ClusterRole {
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

// StorageNodeAPIAddress returns the per-pod headless-service DNS address that
// the cluster control-plane uses to reach a storage-node-api pod backing the
// given worker node.
func StorageNodeAPIAddress(workerNode, namespace string) string {
	return fmt.Sprintf("%s.simplyblock-storage-node-api.%s.svc.cluster.local:5000", nodeHostnameLabel(workerNode), namespace)
}

func BuildStorageNodeService(sn *simplyblockv1alpha1.StorageNode, tlsEnabled bool, tlsProvider string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "simplyblock-storage-node-api",
			Namespace:   sn.Namespace,
			Annotations: ServingCertServiceAnnotations(tlsEnabled, tlsProvider, SecretNameStorageNodeAPITLS),
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

func BuildStorageNodeEndpointSlice(sn *simplyblockv1alpha1.StorageNode, nodeIPs map[string]string) *discoveryv1.EndpointSlice {
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

func BuildSpdkProxyService(sn *simplyblockv1alpha1.StorageNode, tlsEnabled bool, tlsProvider string) *corev1.Service {
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
	sn *simplyblockv1alpha1.StorageNode,
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

func BuildStorageNodeClusterRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
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

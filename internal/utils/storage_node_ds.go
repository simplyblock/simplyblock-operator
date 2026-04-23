package utils

import (
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func BuildStorageNodeDaemonSet(sn *simplyblockv1alpha1.StorageNode) *appsv1.DaemonSet {

	labels := map[string]string{
		"app":                 "storage-node",
		"simplyblock-cluster": sn.Spec.ClusterName,
	}

	image := sn.Spec.ClusterImage
	initCmd := []string{
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
		{Name: "CORE_ISOLATION", Value: BoolPtrToString(sn.Spec.CoreIsolation)},
		{Name: "UBUNTU_HOST", Value: BoolPtrToString(sn.Spec.UbuntuHost)},
		{Name: "OPENSHIFT_CLUSTER", Value: BoolPtrToString(sn.Spec.OpenShiftCluster)},
		{Name: "CPU_TOPOLOGY_ENABLED", Value: BoolPtrToString(sn.Spec.EnableCpuTopology)},
		{Name: "SKIP_KUBELET_CONFIGURATION", Value: BoolPtrToString(sn.Spec.SkipKubeletConfiguration)},
		{Name: "HOSTNAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
	}
	if sn.Spec.ReservedSystemCPU != "" {
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "RESERVED_SYSTEM_CPUS", Value: sn.Spec.ReservedSystemCPU})
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
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "simplyblock-storage-node-sa",
					HostNetwork:        true,
					Tolerations:        sn.Spec.Tolerations,
					NodeSelector: map[string]string{
						"io.simplyblock.node-type": "simplyblock-storage-plane-" + sn.Spec.ClusterName,
					},

					Volumes: []corev1.Volume{
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
					},

					InitContainers: []corev1.Container{
						{
							Name:            "s-node-api-config-generator",
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         initCmd,
							SecurityContext: &corev1.SecurityContext{Privileged: BoolPtr(true)},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "etc-simplyblock", MountPath: "/etc/simplyblock"},
								{Name: "host-modules", MountPath: "/lib/modules", ReadOnly: true},
								{Name: "host-mnt", MountPath: "/mnt"},
							},
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

							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/snode/check",
										Port: intstr.FromInt(5000),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       5,
							},
							Env: mainEnv,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "dev-vol", MountPath: "/dev"},
								{Name: "etc-simplyblock", MountPath: "/etc/simplyblock"},
								{Name: "host-sys", MountPath: "/sys"},
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
	}

	if isOpenShift {
		baseRules = append(baseRules,
			rbacv1.PolicyRule{
				APIGroups: []string{"machineconfiguration.openshift.io"},
				Resources: []string{"machineconfigs", "machineconfigpools", "kubeletconfigs"},
				Verbs:     []string{"list", "get", "create", "update", "patch", "watch"},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"list", "get", "update", "patch", "watch"},
			},
		)
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

func BuildStorageNodeClusterRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRoleBinding",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "simplyblock-storage-node-binding",
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

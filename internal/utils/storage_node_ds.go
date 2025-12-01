package utils

import (
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func BuildStorageNodeDaemonSet(sn *simplyblockv1alpha1.StorageNode) *appsv1.DaemonSet {

	labels := map[string]string{
		"app":                 "storage-node",
		"simplyblock-cluster": sn.Spec.ClusterName,
	}

	image := sn.Spec.ClusterImage
	initCmd := []string{
		"python",
		"simplyblock_web/node_configure.py",
		"--max-lvol=" + Int32PtrToString(sn.Spec.MaxLVol),
		"--max-size=" + sn.Spec.MaxSize,
	}

	if len(sn.Spec.PcieAllowList) > 0 {
		initCmd = append(initCmd, "--pci-allowed="+JoinList(sn.Spec.PcieAllowList))
	}
	if len(sn.Spec.PcieDenyList) > 0 {
		initCmd = append(initCmd, "--pci-blocked="+JoinList(sn.Spec.PcieDenyList))
	}
	if sn.Spec.SocketsToUse != nil {
		initCmd = append(initCmd, "--sockets-to-use="+Int32PtrToString(sn.Spec.SocketsToUse))
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

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-storage-node-ds",
			Namespace: sn.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "simplyblock-storage-node-sa",
					HostNetwork:        true,
					NodeSelector: map[string]string{
						"io.simplyblock.node-type": "simplyblock-storage-plane",
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
								"python", "simplyblock_web/node_webapp.py", "storage_node_k8s",
							},
							SecurityContext: &corev1.SecurityContext{Privileged: BoolPtr(true)},
							Env: []corev1.EnvVar{
								{Name: "CORE_ISOLATION", Value: BoolPtrToString(sn.Spec.CoreIsolation)},
								{Name: "UBUNTU_HOST", Value: "false"},
								{Name: "OPENSHIFT_CLUSTER", Value: "false"},
								{Name: "HOSTNAME", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
								}},
							},
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

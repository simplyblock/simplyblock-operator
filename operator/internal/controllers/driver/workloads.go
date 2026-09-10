// The two plugins: a node DaemonSet on every schedulable worker, and a
// controller StatefulSet that provisions.
//
// Both pod specs reproduce what the chart renders, because adoption reconciles
// toward the state that is running and a field invented here is a rolling
// restart of every node plugin in the cluster on the reconcile that adopts. The
// csi-link volumes and arguments are deliberately absent: they are gated on a
// chart value that is off, and a deployment that has them is one the translation
// has to grow a field for rather than one this file guesses at.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1.

package driver

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// socketDir is where both plugins put their Unix socket. The node plugin's
	// is a hostPath the kubelet also opens; the controller plugin's is an
	// in-memory emptyDir shared only with its sidecars.
	socketDir = "/csi"

	// nodeSocketPath and controllerSocketPath differ, and the sidecars are
	// addressed to the controller's.
	nodeSocketPath       = "unix:///csi/csi.sock"
	controllerSocketPath = "unix:///csi/csi-provisioner.sock"

	// kubeletPluginsDir is where the kubelet looks for a driver's socket. The
	// registration path under it is per driver name, which is what makes two
	// drivers on one worker contend below the level any object name reaches.
	kubeletPluginsDir = "/var/lib/kubelet/plugins"

	verbosity = "--v=5"
)

// nodeRegistrationPath is the socket the kubelet is told to open, which is under
// a directory named for the driver rather than for the deployment.
func nodeRegistrationPath(driver string) string {
	return kubeletPluginsDir + "/" + driver + "/csi.sock"
}

// nodePluginHostDir is the same directory, as the node plugin mounts it.
func nodePluginHostDir(driver string) string {
	return kubeletPluginsDir + "/" + driver
}

// TODO(simplyblockdriver): give TLS and csi-link a spec surface. The chart
// could express both and this kind cannot, so adoption refuses a deployment
// carrying either rather than reconciling it away
// (adoption.go, unsupportedConfiguration).
//
// TLS was the unconditional `simplyblock.tlsEnv`, `tlsVolumeMount`, and
// `clientTlsVolume` on both plugins, gated on tls.enabled: SB_TLS_SERVE,
// SB_TLS_PROVIDER, SB_TLS_CLIENT_AUTH, SB_TLS_CONNECT, the FDB_TLS_* set, a
// serving bundle, and a client certificate per plugin. It is the more urgent of
// the two, because a deployment that had it is one whose data path is
// encrypted.
//
// csiLink is the second. The chart
// gated it on csiLink.enabled and gave each plugin three arguments, a
// service-account token projected for the operator's audience, and a CA bundle
// from a ConfigMap. None of that is expressible on the CRD, so a deployment
// that had it enabled loses it here, and the values that configured it now
// configure only the Service the operator itself serves.
//
// It is off in every deployment measured, which is why the driver could move
// without it. Turning it on again needs a spec surface first, and that decision
// belongs with the csi-link design rather than being guessed at from the
// template it used to be rendered from.

func nodeDaemonSet(d *simplyblockv1alpha2.SimplyblockDriver, image string) *appsv1.DaemonSet {
	n := names(d)
	s := sidecars(d)
	driver := n.csiDriver
	labels := map[string]string{"app": n.nodeDaemonSet}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: n.nodeDaemonSet, Namespace: d.Namespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: n.nodeServiceAccount,
					NodeSelector:       d.Spec.NodeSelector,
					Tolerations:        d.Spec.Tolerations,
					HostNetwork:        true,
					DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
					Containers: []corev1.Container{
						nodeRegistrarContainer(d, s, driver),
						nodePluginContainer(d, image),
					},
					Volumes: nodeVolumes(n, driver),
				},
			},
		},
	}
}

func nodeRegistrarContainer(
	d *simplyblockv1alpha2.SimplyblockDriver, s resolvedSidecars, driver string,
) corev1.Container {
	return corev1.Container{
		Name:            "csi-registrar",
		Image:           s.nodeDriverRegistrar,
		ImagePullPolicy: pullPolicy(d),
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		Args: []string{
			verbosity,
			"--csi-address=" + nodeSocketPath,
			"--kubelet-registration-path=" + nodeRegistrationPath(driver),
			"--health-port=9809",
		},
		Ports: []corev1.ContainerPort{{ContainerPort: 9809, Name: "healthz"}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: named("healthz")},
			},
			InitialDelaySeconds: 20,
			TimeoutSeconds:      10,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "socket-dir", MountPath: socketDir},
			{Name: "registration-dir", MountPath: "/registration"},
		},
	}
}

func nodePluginContainer(d *simplyblockv1alpha2.SimplyblockDriver, image string) corev1.Container {
	bidirectional := corev1.MountPropagationBidirectional

	return corev1.Container{
		Name:            "csi-node",
		Image:           image,
		ImagePullPolicy: pullPolicy(d),
		SecurityContext: &corev1.SecurityContext{
			Privileged:               ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(true),
			Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN", "SYS_MODULE"}},
		},
		Args: []string{
			verbosity,
			"--endpoint=" + nodeSocketPath,
			"--nodeid=$(NODE_ID)",
			"--node",
		},
		Env: append([]corev1.EnvVar{
			fieldRefEnv("NODE_ID", "spec.nodeName"),
			{Name: "GUARDIAN_MIN_BROKEN_FOR", Value: "30s"},
		}, serviceAccountAuthEnv(d)...),
		Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", nodePostStartScript}},
		}},
		Resources: d.Spec.NodeResources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "socket-dir", MountPath: socketDir},
			{Name: "plugin-dir", MountPath: kubeletPluginsDir, MountPropagation: &bidirectional},
			{Name: "pod-dir", MountPath: "/var/lib/kubelet/pods", MountPropagation: &bidirectional},
			{Name: "nvme-hostid-dir", MountPath: "/var/lib/nvme", MountPropagation: &bidirectional},
			{Name: "host-dev", MountPath: "/dev"},
			{Name: "host-sys", MountPath: "/sys"},
			{Name: "csi-nodeserver-config", MountPath: "/etc/spdkcsi-nodeserver-config/", ReadOnly: true},
			{Name: "csi-config", MountPath: "/etc/spdkcsi-config/", ReadOnly: true},
			{Name: "csi-secret", MountPath: "/etc/spdkcsi-secret/", ReadOnly: true},
			{Name: "host-modules", MountPath: "/lib/modules", ReadOnly: true},
			{Name: "guardian-state", MountPath: "/var/run/simplyblock/guardian"},
		},
	}
}

// nodePostStartScript loads the NVMe-oF transports and gives the host a stable
// NVMe host identity. The hostid is generated once and kept on the host, because
// a host that comes back with a new NQN is a host the control plane does not
// recognize as the one holding its connections.
const nodePostStartScript = `modprobe nvme-tcp || echo failed to modprobe nvme-tcp && ` +
	`modprobe nvme-rdma || echo failed to modprobe nvme-rdma && ` +
	`if [ ! -f /var/lib/nvme/hostid ]; then uuidgen > /var/lib/nvme/hostid; fi && ` +
	`cp /var/lib/nvme/hostid /etc/nvme/hostid && ` +
	`echo "nqn.2014-08.org.nvmexpress:uuid:$(cat /etc/nvme/hostid)" > /etc/nvme/hostnqn`

func nodeVolumes(n objectNames, driver string) []corev1.Volume {
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	dir := corev1.HostPathDirectory

	return []corev1.Volume{
		hostPathVolume("socket-dir", nodePluginHostDir(driver), &dirOrCreate),
		hostPathVolume("registration-dir", kubeletPluginsDir+"_registry/", &dir),
		hostPathVolume("plugin-dir", kubeletPluginsDir, &dir),
		hostPathVolume("pod-dir", "/var/lib/kubelet/pods", &dir),
		hostPathVolume("nvme-hostid-dir", "/var/lib/nvme", &dirOrCreate),
		hostPathVolume("host-dev", "/dev", nil),
		hostPathVolume("host-sys", "/sys", nil),
		hostPathVolume("host-modules", "/lib/modules", nil),
		hostPathVolume("guardian-state", "/var/lib/simplyblock/guardian", &dirOrCreate),
		configMapVolume("csi-nodeserver-config", n.nodeServerConfigMap, true),
		configMapVolume("csi-config", n.configMap, false),
		secretVolume("csi-secret", n.secretV2),
	}
}

func controllerStatefulSet(d *simplyblockv1alpha2.SimplyblockDriver, image string) *appsv1.StatefulSet {
	n := names(d)
	s := sidecars(d)
	labels := map[string]string{"app": "csi-controller"}

	sidecarMount := []corev1.VolumeMount{{Name: "socket-dir", MountPath: socketDir}}
	containers := []corev1.Container{
		controllerSidecar(d, "csi-provisioner", s.provisioner, sidecarMount, []string{
			verbosity,
			"--csi-address=" + controllerSocketPath,
			"--timeout=180s",
			"--kube-api-qps=50",
			"--kube-api-burst=100",
			"--worker-threads=1",
			"--retry-interval-start=2s",
			"--leader-election=false",
			"--extra-create-metadata=true",
			"--feature-gates=Topology=true",
		}),
		controllerSidecar(d, "csi-snapshotter", s.snapshotter, sidecarMount, []string{
			"--csi-address=" + controllerSocketPath,
			verbosity,
			"--timeout=150s",
			"--leader-election=false",
		}),
		controllerSidecar(d, "csi-attacher", s.attacher, sidecarMount, []string{
			verbosity,
			"--csi-address=" + controllerSocketPath,
			"--leader-election=false",
		}),
		controllerSidecar(d, "csi-resizer", s.resizer, sidecarMount, []string{
			verbosity,
			"--csi-address=" + controllerSocketPath,
			"--leader-election=false",
		}),
		controllerSidecar(d, "csi-health-monitor", s.healthMonitor, sidecarMount, []string{
			verbosity,
			"--csi-address=" + controllerSocketPath,
			"--leader-election=false",
		}),
		controllerPluginContainer(d, image),
	}
	// The snapshotter runs privileged in the chart, and the health monitor
	// exposes a port. Both are properties of the container rather than of the
	// loop above.
	containers[1].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	containers[4].Ports = []corev1.ContainerPort{
		{ContainerPort: 8080, Name: "http-endpoint", Protocol: corev1.ProtocolTCP},
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: n.controllerStatefulSet, Namespace: d.Namespace},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: n.controllerStatefulSet,
			Replicas:    d.Spec.ControllerReplicas,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: n.controllerServiceAccount,
					NodeSelector:       d.Spec.ControllerNodeSelector,
					Tolerations:        d.Spec.ControllerTolerations,
					HostNetwork:        true,
					DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
					Containers:         containers,
					Volumes: []corev1.Volume{
						{
							Name: "socket-dir",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
							},
						},
						configMapVolume("csi-config", n.configMap, false),
						secretVolume("csi-secret", n.secretV2),
					},
				},
			},
		},
	}
}

func controllerSidecar(
	d *simplyblockv1alpha2.SimplyblockDriver, name, image string,
	mounts []corev1.VolumeMount, args []string,
) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: pullPolicy(d),
		Args:            args,
		VolumeMounts:    mounts,
		Resources:       d.Spec.ControllerResources,
	}
}

func controllerPluginContainer(d *simplyblockv1alpha2.SimplyblockDriver, image string) corev1.Container {
	return corev1.Container{
		Name:            "csi-controller",
		Image:           image,
		ImagePullPolicy: pullPolicy(d),
		Args: []string{
			verbosity,
			"--endpoint=" + controllerSocketPath,
			"--nodeid=$(NODE_ID)",
			"--controller",
		},
		Env: append([]corev1.EnvVar{
			fieldRefEnv("NODE_ID", "spec.nodeName"),
		}, serviceAccountAuthEnv(d)...),
		Resources: d.Spec.ControllerResources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "socket-dir", MountPath: socketDir},
			{Name: "csi-config", MountPath: "/etc/spdkcsi-config/", ReadOnly: true},
			{Name: "csi-secret", MountPath: "/etc/spdkcsi-secret/", ReadOnly: true},
		},
	}
}

// serviceAccountAuthEnv points the plugin at its projected service-account
// token. The token is mounted by the kubelet at a fixed path in every pod, so
// enabling this adds an environment variable and no volume.
func serviceAccountAuthEnv(d *simplyblockv1alpha2.SimplyblockDriver) []corev1.EnvVar {
	if d.Spec.EnableServiceAccountAuth == nil || !*d.Spec.EnableServiceAccountAuth {
		return nil
	}
	return []corev1.EnvVar{{
		Name:  "SPDKCSI_API_TOKEN_PATH",
		Value: "/var/run/secrets/kubernetes.io/serviceaccount/token",
	}}
}

// pullPolicy applies the CRD's default, so that an object written before
// admission defaulted the field does not produce an empty policy the API server
// then fills in differently from the spec a reader sees.
func pullPolicy(d *simplyblockv1alpha2.SimplyblockDriver) corev1.PullPolicy {
	if d.Spec.ImagePullPolicy != "" {
		return d.Spec.ImagePullPolicy
	}
	return corev1.PullAlways
}

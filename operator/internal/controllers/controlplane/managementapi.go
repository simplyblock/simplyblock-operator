// The management API and the workloads beside it: what the ApplyingAPI step
// applies, and the Service status.endpoint resolves to.
//
// Four workloads share one image, one account, and one configuration, and they
// differ in what they run. The management API serves the REST surface every
// controller in this operator calls. The monitoring pool watches nodes, devices,
// volumes, and snapshots and writes what it sees. The task runner executes the
// queued work those two produce. The admin surface runs nothing and exists to be
// exec'd into, which is why its command is a sleep.
//
// The exporter is the fifth object here and is not built from that image: it
// reads FoundationDB's status JSON through the client library and publishes it
// as metrics. It sits in this step rather than the FoundationDB one because what
// it needs is the cluster file the database's own step produces, so it can only
// run once that step has finished.

package controlplane

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// fdbExporterImage turns FoundationDB's machine-readable status into Prometheus
// metrics. It is upstream's rather than simplyblock's, and pinned for the same
// reason the database's images are.
const fdbExporterImage = "aikoven/foundationdb-exporter:3.1.0"

// restartOnClusterFileChange is what makes a coordinator change reach the pods
// that hold a connection to the old one. The reloader watches the ConfigMap the
// FoundationDB operator writes and rolls whatever carries this annotation.
//
// The reloader itself is a chart dependency and stays one. A control plane
// installed without it keeps working and takes a coordinator change on the next
// restart instead of at once, which is a degradation rather than a break.
var restartOnClusterFileChange = map[string]string{
	"reloader.stakater.com/auto":      "true",
	"reloader.stakater.com/configmap": clusterFileConfigMapName,
}

// managementAPIObjects is everything the ApplyingAPI step writes, in the order it
// depends on itself: the account and the configuration, then the roles that name
// the account, then the workloads that run under it, then the Service in front
// of the one that serves.
func managementAPIObjects(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	ns := cp.Namespace
	//nolint:prealloc // the literal is the declaration; the append below is the conditional set
	objects := []client.Object{
		serviceAccount(ns, serviceAccountName),
		sharedConfigMap(ns),
		controlPlaneClusterRole(),
		controlPlaneClusterRoleBinding(ns),
		serviceReaderClusterRole(),
		serviceReaderClusterRoleBinding(ns),
		webAPIDeployment(cp),
		webAPIService(cp),
		tasksDeployment(cp),
		monitoringDeployment(cp),
		adminControlDeployment(cp),
		fdbExporterDeployment(cp),
		fdbExporterService(ns),
	}
	return append(objects, servingCertificateObjects(cp)...)
}

// managementAPIClusterScoped is the half of that set the garbage collector will
// not remove with the ControlPlane, and which the finalizer therefore deletes.
func managementAPIClusterScoped() []client.Object {
	return []client.Object{
		controlPlaneClusterRole(),
		controlPlaneClusterRoleBinding(""),
		serviceReaderClusterRole(),
		serviceReaderClusterRoleBinding(""),
	}
}

// sharedConfigMap holds the one setting every workload reads at run time. It is
// a ConfigMap rather than a field on each Deployment so that changing the log
// level of a whole control plane is one edit, and so that the reloader can roll
// the pods that read it.
func sharedConfigMap(namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: namespace},
		Data: map[string]string{
			logLevelKey:             "DEBUG",
			"LOG_DELETION_INTERVAL": "3d",
		},
	}
}

// controlPlaneClusterRole is what the control plane does to Kubernetes.
//
// It is wide, and the width is the control plane's own architecture rather than
// this install's choice: it reads and patches the storage-node workloads it
// manages, execs into their pods to drive SPDK, labels nodes, and reviews the
// tokens presented to its API. Narrowing it is worthwhile and is a change to the
// control plane rather than to the object that grants it.
func controlPlaneClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch", "patch", "update"},
			},
			{
				APIGroups: []string{"", "apps"},
				Resources: []string{"pods", "deployments", "statefulsets", "daemonsets"},
				Verbs:     []string{"get", "list", "watch", "patch", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/log"},
				Verbs:     []string{"get", "list"},
			},
			{
				// Exec is how the control plane drives the storage nodes'
				// processes, and it is the strongest grant in this role: exec
				// into a privileged pod is that pod's privilege.
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"create", "get", "list", "watch", "patch", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "list", "watch", "patch", "update"},
			},
			{
				APIGroups: []string{"storage.simplyblock.io"},
				Resources: []string{
					"pools", "lvols", "storageclusters", "storagenodesets", "devices",
					"tasks", "storagebackups",
				},
				Verbs: []string{"get", "list", "patch", "update", "watch"},
			},
			{
				APIGroups: []string{"storage.simplyblock.io"},
				Resources: []string{
					"pools/status", "lvols/status", "storageclusters/status",
					"storagenodesets/status", "devices/status", "tasks/status",
					"storagebackups/status",
				},
				Verbs: []string{"get", "patch", "update"},
			},
			{
				// A TokenReview is how the control plane authenticates a caller
				// presenting a service-account token instead of the static
				// cluster secret.
				APIGroups: []string{"authentication.k8s.io"},
				Resources: []string{"tokenreviews"},
				Verbs:     []string{"create"},
			},
		},
	}
}

func controlPlaneClusterRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRoleBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: serviceAccountName, Namespace: namespace,
		}},
	}
}

// serviceReaderClusterRole lets the namespace's default account resolve
// services, endpoints, and nodes. It is bound to `default` rather than to the
// control plane's own account because what needs it is the tooling run inside
// pods that did not name an account.
func serviceReaderClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: serviceReaderRoleName},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"services", "pods", "endpoints", "nodes"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
}

func serviceReaderClusterRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: serviceReaderBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: serviceReaderRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: "default", Namespace: namespace,
		}},
	}
}

// webAPIDeployment is the management API: the endpoint every controller in this
// operator calls, and the one workload whose absence is an outage.
//
// Two replicas by default, spread across machines, rolled one at a time with no
// surge. That combination is what lets a pod be replaced while the control plane
// keeps answering, and three things in the design rest on it: Degraded is a
// component below its desired count while the probe still passes, a Restart
// recycles the workload after draining rather than instead of serving, and an
// Upgrade rolls this Deployment.
func webAPIDeployment(cp *simplyblockv1alpha2.ControlPlane) *appsv1.Deployment {
	managed := cp.Spec.Source.Local
	labels := map[string]string{appLabel: ComponentWebAPI}

	//nolint:prealloc // the literal is the declaration; the append below is the shared set
	env := []corev1.EnvVar{
		logLevelEnv(),
		{Name: "LVOL_NVMF_PORT_START", Value: lvolNVMfPortStart},
		{Name: "ENABLE_MONITORING", Value: "false"},
		namespaceEnv(),
		{Name: "FLASK_DEBUG", Value: "False"},
		{Name: "FLASK_ENV", Value: "production"},
		// The operator's own account is the one caller the control plane has to
		// trust before anything else works: every controller in this process
		// authenticates with its token.
		{Name: "SB_K8S_ADMIN_SERVICE_ACCOUNTS",
			Value: "system:serviceaccount:" + cp.Namespace + ":simplyblock-operator"},
		{Name: "SB_K8S_METRICS_SERVICE_ACCOUNTS",
			Value: "system:serviceaccount:" + cp.Namespace + ":simplyblock-prometheus"},
	}
	env = append(env, prometheusEnv()...)
	env = append(env, tlsEnv(managed)...)

	spec := corev1.PodSpec{
		ServiceAccountName: serviceAccountName,
		Affinity:           spreadAcrossHosts(ComponentWebAPI),
		Containers: []corev1.Container{{
			Name:            "webappapi",
			Image:           localImage(cp),
			ImagePullPolicy: pullPolicyOf(managed),
			Command:         []string{"python3", "simplyblock_web/app.py"},
			Ports:           []corev1.ContainerPort{{ContainerPort: webAPIPort}},
			Env:             env,
			VolumeMounts:    append([]corev1.VolumeMount{clusterFileMount()}, tlsMount(managed)...),
			Resources:       webAPIResources(managed),
		}},
		Volumes: append([]corev1.Volume{clusterFileVolumeSource()},
			tlsVolume(managed, ServingCertSecret)...),
	}
	scheduling(managed, &spec)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ComponentWebAPI,
			Namespace:   cp.Namespace,
			Annotations: restartOnClusterFileChange,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(apiReplicas(managed)),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       ptr.To(intstr.FromInt32(0)),
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"log-collector/enabled": "true"},
				},
				Spec: spec,
			},
		},
	}
}

// webAPIResources is what the management API asks for, or what the spec states.
// The default is the chart's: enough headroom for the request volume a fleet
// produces, and a limit that contains a leak rather than one anybody has
// measured against.
func webAPIResources(managed *simplyblockv1alpha2.LocalControlPlane) corev1.ResourceRequirements {
	if managed != nil && (len(managed.Resources.Requests) > 0 || len(managed.Resources.Limits) > 0) {
		return managed.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

// webAPIService is what status.endpoint resolves to, and the name the CSI
// driver's configuration and the metrics scrape both carry.
func webAPIService(cp *simplyblockv1alpha2.ControlPlane) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ComponentWebAPI,
			Namespace:   cp.Namespace,
			Annotations: servingCertAnnotations(cp),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{appLabel: ComponentWebAPI},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: webAPIPort, TargetPort: intstrFromInt(webAPIPort)},
			},
		},
	}
}

// monitoringServices are the watchers the control plane runs: one per subject it
// keeps state about. They are one pod rather than ten because each is a small
// polling loop and the pod is what shares the cluster file and the log level.
//
// The pod runs on the host network, which is how the node and device monitors
// reach the storage nodes' management addresses directly.
func monitoringServices() []service {
	return []service{
		{name: "storage-node-monitor", module: "simplyblock_core/services/storage_node_monitor.py"},
		{
			name:     "mgmt-node-monitor",
			module:   "simplyblock_core/services/mgmt_node_monitor.py",
			extraEnv: []corev1.EnvVar{{Name: "BACKEND_TYPE", Value: "k8s"}},
		},
		{name: "lvol-stats-collector", module: "simplyblock_core/services/lvol_stat_collector.py"},
		{name: "main-distr-event-collector", module: "simplyblock_core/services/main_distr_event_collector.py"},
		{name: "capacity-and-stats-collector", module: "simplyblock_core/services/capacity_and_stats_collector.py"},
		{name: "capacity-monitor", module: "simplyblock_core/services/cap_monitor.py"},
		{name: "health-check", module: "simplyblock_core/services/health_check_service.py"},
		{name: "device-monitor", module: "simplyblock_core/services/device_monitor.py"},
		{name: "lvol-monitor", module: "simplyblock_core/services/lvol_monitor.py"},
		{name: "snapshot-monitor", module: "simplyblock_core/services/snapshot_monitor.py"},
	}
}

// taskServices are the runners that execute queued work. Their work is queued,
// which is what makes this component non-essential in the phase table: a runner
// at zero defers what is waiting rather than dropping it, and the queue is still
// there when it comes back.
func taskServices() []service {
	portStart := []corev1.EnvVar{{Name: "LVOL_NVMF_PORT_START", Value: lvolNVMfPortStart}}
	return []service{
		{name: "tasks-node-add-runner", module: "simplyblock_core/services/tasks_runner_node_add.py", extraEnv: portStart},
		{name: "tasks-runner-restart", module: "simplyblock_core/services/tasks_runner_restart.py"},
		{name: "tasks-runner-migration", module: "simplyblock_core/services/tasks_runner_migration.py"},
		{name: "tasks-runner-lvol-migration", module: "simplyblock_core/services/tasks_runner_lvol_migration.py"},
		{name: "tasks-runner-batch-migration", module: "simplyblock_core/services/tasks_runner_batch_migration.py"},
		{name: "tasks-runner-failed-migration", module: "simplyblock_core/services/tasks_runner_failed_migration.py"},
		{name: "tasks-runner-cluster-status", module: "simplyblock_core/services/tasks_cluster_status.py"},
		{name: "tasks-runner-new-device-migration", module: "simplyblock_core/services/tasks_runner_new_dev_migration.py"},
		{name: "tasks-runner-port-allow", module: "simplyblock_core/services/tasks_runner_port_allow.py"},
		{name: "tasks-runner-jc-comp-resume", module: "simplyblock_core/services/tasks_runner_jc_comp.py"},
		{name: "tasks-runner-sync-lvol-del", module: "simplyblock_core/services/tasks_runner_sync_lvol_del.py"},
		{name: "tasks-runner-cluster-expand", module: "simplyblock_core/services/tasks_runner_cluster_expand.py"},
		{name: "tasks-runner-node-removal", module: "simplyblock_core/services/tasks_runner_node_removal.py"},
		{name: "tasks-runner-snapshot-replication", module: "simplyblock_core/services/snapshot_replication.py"},
		{name: "tasks-runner-backup", module: "simplyblock_core/services/tasks_runner_backup.py"},
		{name: "tasks-runner-backup-merge", module: "simplyblock_core/services/tasks_runner_backup_merge.py"},
		{name: "tasks-runner-replication-final", module: "simplyblock_core/services/tasks_runner_replication_final.py"},
	}
}

// monitoringDeployment and tasksDeployment are the same pod with a different
// service list. One replica each: both are pollers over shared state, and a
// second instance of either would do the same work twice.
func monitoringDeployment(cp *simplyblockv1alpha2.ControlPlane) *appsv1.Deployment {
	return servicePoolDeployment(cp, ComponentMonitoring, monitoringServices())
}

func tasksDeployment(cp *simplyblockv1alpha2.ControlPlane) *appsv1.Deployment {
	return servicePoolDeployment(cp, ComponentTasks, taskServices())
}

// servicePoolDeployment builds one pod holding a list of the control plane's
// long-running processes.
func servicePoolDeployment(
	cp *simplyblockv1alpha2.ControlPlane, name string, services []service,
) *appsv1.Deployment {
	managed := cp.Spec.Source.Local
	labels := map[string]string{appLabel: name}

	spec := corev1.PodSpec{
		ServiceAccountName: serviceAccountName,
		// The processes here reach the storage nodes' management addresses
		// directly, which are host addresses rather than Service names.
		HostNetwork: true,
		DNSPolicy:   corev1.DNSClusterFirstWithHostNet,
		Containers:  containers(services, managed, localImage(cp), pullPolicyOf(managed)),
		Volumes: append([]corev1.Volume{clusterFileVolumeSource()},
			tlsVolume(managed, ServingCertSecret)...),
	}
	scheduling(managed, &spec)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   cp.Namespace,
			Annotations: restartOnClusterFileChange,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"log-collector/enabled": "true"},
				},
				Spec: spec,
			},
		},
	}
}

// adminControlDeployment runs nothing. It is a pod with the control plane's
// tooling, its cluster file, and its account, kept alive so that an
// administrator can exec into it, which is how the command-line surface is
// reached on a deployment that has no shell access to the control plane's hosts.
func adminControlDeployment(cp *simplyblockv1alpha2.ControlPlane) *appsv1.Deployment {
	managed := cp.Spec.Source.Local
	labels := map[string]string{appLabel: ComponentAdminControl}

	//nolint:prealloc // the literal is the declaration; the append below is the shared set
	env := []corev1.EnvVar{
		{Name: "LVOL_NVMF_PORT_START", Value: lvolNVMfPortStart},
		namespaceEnv(),
		logLevelEnv(),
	}
	env = append(env, prometheusEnv()...)
	env = append(env, tlsEnv(managed)...)

	spec := corev1.PodSpec{
		ServiceAccountName: serviceAccountName,
		HostNetwork:        true,
		DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
		Affinity:           spreadAcrossHosts(ComponentAdminControl),
		Containers: []corev1.Container{{
			Name:            "simplyblock-control",
			Image:           localImage(cp),
			ImagePullPolicy: pullPolicyOf(managed),
			// Trapping the two signals is what makes the pod terminate promptly
			// on a delete instead of waiting out its grace period: bash does not
			// act on a signal while a foreground sleep is running.
			Command:      []string{"/bin/bash", "-c", "trap : TERM INT; sleep infinity & wait"},
			Env:          env,
			VolumeMounts: append([]corev1.VolumeMount{clusterFileMount()}, tlsMount(managed)...),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("600m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		}},
		Volumes: append([]corev1.Volume{clusterFileVolumeSource()},
			tlsVolume(managed, ServingCertSecret)...),
	}
	scheduling(managed, &spec)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ComponentAdminControl,
			Namespace:   cp.Namespace,
			Annotations: restartOnClusterFileChange,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(apiReplicas(managed)),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       ptr.To(intstr.FromInt32(0)),
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"log-collector/enabled": "true"},
				},
				Spec: spec,
			},
		},
	}
}

// fdbExporterDeployment publishes the database's own view of itself as metrics,
// which is the half of a FoundationDB problem that pod counts cannot show: write
// latency, storage lag, and coordinator health.
func fdbExporterDeployment(cp *simplyblockv1alpha2.ControlPlane) *appsv1.Deployment {
	const tmpVolume = "tmp"
	labels := map[string]string{appLabel: ComponentFDBExporter}
	// The exporter opens the database like any other client, so peer TLS is its
	// decision too: the cluster file it is given names :tls coordinators, and a
	// client with no certificate hangs in the handshake rather than failing.
	peerTLS := fdbPeerTLS(cp)

	spec := corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true),
			RunAsUser:    ptr.To(int64(4059)),
			RunAsGroup:   ptr.To(int64(4059)),
			FSGroup:      ptr.To(int64(4059)),
		},
		Volumes: append([]corev1.Volume{
			{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			clusterFileVolumeSource(),
		}, peerVolumeIf(peerTLS)...),
		Containers: []corev1.Container{{
			Name:  "exporter",
			Image: fdbExporterImage,
			Env: append([]corev1.EnvVar{
				{Name: "FDB_CLUSTER_FILE", Value: clusterFilePath},
			}, peerEnvIf(peerTLS)...),
			Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: fdbExporterPort}},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromString("metrics")},
				},
				InitialDelaySeconds: 15,
				PeriodSeconds:       30,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromString("metrics")},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       10,
			},
			SecurityContext: &corev1.SecurityContext{
				ReadOnlyRootFilesystem:   ptr.To(true),
				AllowPrivilegeEscalation: ptr.To(false),
				Privileged:               ptr.To(false),
			},
			VolumeMounts: append([]corev1.VolumeMount{
				{Name: tmpVolume, MountPath: "/tmp"},
				{
					Name:      clusterFileVolume,
					MountPath: clusterFilePath,
					SubPath:   clusterFileSubURL,
					ReadOnly:  true,
				},
			}, peerMountIf(peerTLS)...),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		}},
		TerminationGracePeriodSeconds: ptr.To(int64(10)),
	}
	scheduling(cp.Spec.Source.Local, &spec)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ComponentFDBExporter,
			Namespace:   cp.Namespace,
			Labels:      labels,
			Annotations: restartOnClusterFileChange,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
		},
	}
}

func fdbExporterService(namespace string) *corev1.Service {
	labels := map[string]string{appLabel: ComponentFDBExporter}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ComponentFDBExporter,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "metrics", Port: fdbExporterPort, TargetPort: intstr.FromString("metrics")},
			},
		},
	}
}

// intstrFromInt names a numeric target port, which is what every Service here
// takes except the exporter's. That one names its container port, because the
// port is declared with a name and matching on it survives a renumber.
func intstrFromInt(port int32) intstr.IntOrString {
	return intstr.FromInt32(port)
}

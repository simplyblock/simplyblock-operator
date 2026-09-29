// The FoundationDB half of the install: the operator that reconciles
// FoundationDBClusters, the accounts and roles it and the database pods need,
// and the FoundationDBCluster itself.
//
// Nothing here is a Go type, because this repository has no dependency on the
// FoundationDB operator's API module and taking one would pull its whole
// controller in for two status fields. The cluster is built and read as
// unstructured, which is also what lets the apply work against whatever
// v1beta2's schema is on the cluster rather than against a pinned copy of it.
//
// # Detection, and where it differs from the design
//
// design-controlplane.md §5.1 detects the FoundationDB operator by whether
// apps.foundationdb.org/v1beta2 is served, and installs the CRDs and the
// controller where it is not. That test does not separate the two things on this
// chart: the CRDs ship in the chart's CRD directory, which Helm applies on
// install and never removes, so the group is served on every deployment whether
// or not a controller is reconciling it.
//
// So the two halves are split by who can answer for them. The CRDs stay with the
// chart, and the group being served is treated as a prerequisite: an install
// that cannot see v1beta2 holds and names it, because creating a
// FoundationDBCluster against a group the API server does not know is not a wait
// but an error. The controller is applied, under the name the chart gave it, in
// the namespace the ControlPlane is in. That is what ships today, so a cluster
// that also runs a FoundationDB operator of its own is in the same position it
// was in before the install moved.

package controlplane

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The FoundationDB group, and the version of it this install writes.
const (
	fdbGroup   = "apps.foundationdb.org"
	fdbVersion = "v1beta2"
)

// fdbClusterGVK is the kind the install applies and reads back.
var fdbClusterGVK = schema.GroupVersionKind{
	Group:   fdbGroup,
	Version: fdbVersion,
	Kind:    "FoundationDBCluster",
}

// fdbBackupGVK is what a Backup operation creates or triggers. It is here rather
// than beside that operation because it is the same group, and the two would
// otherwise disagree about the version.
var fdbBackupGVK = schema.GroupVersionKind{
	Group:   fdbGroup,
	Version: fdbVersion,
	Kind:    "FoundationDBBackup",
}

// The images the FoundationDB half runs. They are pinned constants rather than
// spec fields for the reason design-controlplane.md gives for not putting the
// component table in the API: a user cannot add a component to a managed control
// plane, and the version of the database is the control plane's business rather
// than the deployment's. Moving FoundationDB forward is a release of this
// operator.
const (
	fdbOperatorImage = "quay.io/simplyblock-io/fdb-kubernetes-operator:v2.13.0"
	fdbMonitorImage  = "quay.io/simplyblock-io/fdb-kubernetes-monitor:7.3.63"
	fdbVersionString = "7.3.63"
)

// fdbCoordinatorDisk is what each FoundationDB process claims. It is the chart's
// value, and it sizes the metadata of a fleet rather than its data: what lives in
// FoundationDB is cluster definitions, node registrations, and lvol records.
const fdbCoordinatorDisk = "10G"

// foundationDBObjects is everything the ApplyingFoundationDB step writes, in the
// order it depends on itself: the accounts, then the roles that name them, then
// the controller, then the cluster the controller reconciles.
func foundationDBObjects(cp *simplyblockv1alpha2.ControlPlane) []client.Object {
	ns := cp.Namespace
	//nolint:prealloc // the literal is the declaration; the append below is the conditional set
	objects := []client.Object{
		serviceAccount(ns, fdbOperatorServiceAccount),
		serviceAccount(ns, fdbPodServiceAccount),
		fdbManagerRoleObject(),
		fdbManagerClusterRoleObject(),
		fdbManagerRoleBindingObject(ns),
		fdbManagerClusterRoleBindingObject(ns),
		fdbPodRoleObject(ns),
		fdbPodRoleBindingObject(ns),
		fdbOperatorDeployment(cp),
		foundationDBCluster(cp),
	}
	return append(objects, fdbPeerCertificate(cp)...)
}

// foundationDBClusterScoped is the half of that set the garbage collector will
// not remove with the ControlPlane, and which the finalizer therefore deletes.
func foundationDBClusterScoped() []client.Object {
	return []client.Object{
		fdbManagerRoleObject(),
		fdbManagerClusterRoleObject(),
		fdbManagerClusterRoleBindingObject(""),
	}
}

// serviceAccount is the account an object set runs under. It carries nothing but
// its name: what it may do is in the bindings that name it.
func serviceAccount(namespace, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// fdbManagerRoleObject is what the FoundationDB operator does inside the namespace:
// the pods, volumes, and services that make up a database, and the three
// FoundationDB kinds themselves. It is a ClusterRole bound by a RoleBinding, so
// the grant is cluster-shaped and its effect is one namespace.
func fdbManagerRoleObject() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: fdbOperatorRoleName},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{
					"configmaps", "events", "persistentvolumeclaims", "pods", "secrets", "services",
				},
				Verbs: []string{"create", "delete", "get", "list", "patch", "update", "watch"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				Verbs:     []string{"create", "delete", "get", "list", "patch", "update", "watch"},
			},
			{
				APIGroups: []string{fdbGroup},
				Resources: []string{
					"foundationdbbackups", "foundationdbclusters", "foundationdbrestores",
				},
				Verbs: []string{"create", "delete", "get", "list", "patch", "update", "watch"},
			},
			{
				APIGroups: []string{fdbGroup},
				Resources: []string{
					"foundationdbbackups/status", "foundationdbclusters/status",
					"foundationdbrestores/status",
				},
				Verbs: []string{"get", "patch", "update"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{"create", "delete", "get", "list", "patch", "update", "watch"},
			},
		},
	}
}

// fdbManagerClusterRoleObject is the one thing the operator needs outside the
// namespace: the nodes it reads to place a database across fault domains.
func fdbManagerClusterRoleObject() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: fdbOperatorClusterRole},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
}

func fdbManagerRoleBindingObject(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: fdbOperatorRoleBinding, Namespace: namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: fdbOperatorRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: fdbOperatorServiceAccount, Namespace: namespace,
		}},
	}
}

func fdbManagerClusterRoleBindingObject(namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: fdbOperatorClusterBinding},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: fdbOperatorClusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: fdbOperatorServiceAccount, Namespace: namespace,
		}},
	}
}

// fdbPodRoleObject is what a FoundationDB pod does to itself. The unified monitor in
// each pod writes locality and launcher-environment annotations onto its own pod
// to signal the operator, which the namespace's default account cannot do.
func fdbPodRoleObject(namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: fdbPodRoleName, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list", "watch", "update", "patch"},
		}},
	}
}

func fdbPodRoleBindingObject(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: fdbPodRoleBinding, Namespace: namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: fdbPodRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: fdbPodServiceAccount, Namespace: namespace,
		}},
	}
}

// fdbOperatorDeployment is the controller that turns a FoundationDBCluster into
// running pods. Its init container copies the FoundationDB client library and
// the three command-line tools out of the monitor image, because the controller
// shells out to fdbcli to read status and to change configuration.
func fdbOperatorDeployment(cp *simplyblockv1alpha2.ControlPlane) *appsv1.Deployment {
	const (
		binariesVolume = "fdb-binaries"
		tmpVolume      = "tmp"
		logsVolume     = "logs"
	)
	labels := map[string]string{appLabel: ComponentFDBOperator}
	peerTLS := fdbPeerTLS(cp)

	spec := corev1.PodSpec{
		ServiceAccountName: fdbOperatorServiceAccount,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:  ptr.To(int64(4059)),
			RunAsGroup: ptr.To(int64(4059)),
			FSGroup:    ptr.To(int64(4059)),
		},
		Volumes: append([]corev1.Volume{
			{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: logsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: binariesVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}, peerVolumeIf(peerTLS)...),
		InitContainers: []corev1.Container{{
			Name:  "foundationdb-kubernetes-init-7-3",
			Image: fdbMonitorImage,
			Args: []string{
				"--copy-library", "7.3",
				"--copy-binary", "fdbcli",
				"--copy-binary", "fdbbackup",
				"--copy-binary", "fdbrestore",
				"--output-dir", "/var/output-files",
				"--mode", "init",
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: binariesVolume, MountPath: "/var/output-files"},
			},
		}},
		Containers: []corev1.Container{{
			Name:    "manager",
			Image:   fdbOperatorImage,
			Command: []string{"/manager"},
			Args:    []string{"--health-probe-bind-address=:9443"},
			// The operator reconciles the database, so it reaches it the way its
			// processes reach each other and needs the same material.
			Env: append([]corev1.EnvVar{{
				Name: "WATCH_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				},
			}}, peerEnvIf(peerTLS)...),
			Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			SecurityContext: &corev1.SecurityContext{
				ReadOnlyRootFilesystem:   ptr.To(true),
				AllowPrivilegeEscalation: ptr.To(false),
				Privileged:               ptr.To(false),
			},
			VolumeMounts: append([]corev1.VolumeMount{
				{Name: tmpVolume, MountPath: "/tmp"},
				{Name: logsVolume, MountPath: "/var/log/fdb"},
				{Name: binariesVolume, MountPath: "/usr/bin/fdb"},
			}, peerMountIf(peerTLS)...),
		}},
		TerminationGracePeriodSeconds: ptr.To(int64(10)),
	}
	scheduling(cp.Spec.Source.Local, &spec)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ComponentFDBOperator,
			Namespace: cp.Namespace,
			Labels:    labels,
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

// foundationDBCluster is the database itself.
//
// It is built as unstructured for the reason this file's opening comment gives,
// and the shape is the chart's: the unified image type, explicit listen
// addresses, DNS locality, and a per-class pod template that names the pods'
// account. What varies with the spec is the coordinator count, the storage
// class, and the resources.
func foundationDBCluster(cp *simplyblockv1alpha2.ControlPlane) *unstructured.Unstructured {
	fdb := foundationDBSpecOf(cp)
	processes := coordinatorCount(fdb)

	// The class templates are identical apart from their anti-affinity, and the
	// FoundationDB operator replaces general.podTemplate wholesale when a class
	// override exists, so each one repeats what general already said.
	peerTLS := fdbPeerTLS(cp)
	classTemplate := func(class string) map[string]any {
		template := map[string]any{
			"spec": map[string]any{
				"serviceAccountName": fdbPodServiceAccount,
				"containers": []any{
					fdbContainer(fdb, peerTLS),
				},
				"affinity": map[string]any{
					"podAntiAffinity": map[string]any{
						"preferredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
							"weight": int64(100),
							"podAffinityTerm": map[string]any{
								"labelSelector": map[string]any{
									"matchLabels": map[string]any{
										"foundationdb.org/fdb-process-class": class,
									},
								},
								"topologyKey": "kubernetes.io/hostname",
							},
						}},
					},
				},
			},
		}
		if local := cp.Spec.Source.Local; local != nil && len(local.NodeSelector) > 0 {
			template["spec"].(map[string]any)["nodeSelector"] = toAnyMap(local.NodeSelector)
		}
		if peerTLS {
			// The operator replaces general.podTemplate wholesale for a class
			// that overrides it, so the volume is repeated here rather than
			// inherited.
			template["spec"].(map[string]any)["volumes"] = fdbPeerVolume()
		}
		return map[string]any{"podTemplate": template}
	}

	general := map[string]any{
		"customParameters": []any{"knob_disable_posix_kernel_aio=1"},
		"podTemplate": map[string]any{
			"spec": map[string]any{
				"serviceAccountName": fdbPodServiceAccount,
				"containers":         []any{fdbContainer(fdb, peerTLS)},
				"initContainers": []any{map[string]any{
					"name": "foundationdb-kubernetes-init",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
						"limits":   map[string]any{"cpu": "100m", "memory": "128Mi"},
					},
					"securityContext": map[string]any{"runAsUser": int64(0)},
				}},
			},
		},
		"volumeClaimTemplate": map[string]any{
			"spec": volumeClaimSpec(fdb),
		},
	}
	if local := cp.Spec.Source.Local; local != nil && len(local.NodeSelector) > 0 {
		general["podTemplate"].(map[string]any)["spec"].(map[string]any)["nodeSelector"] =
			toAnyMap(local.NodeSelector)
	}
	if peerTLS {
		general["podTemplate"].(map[string]any)["spec"].(map[string]any)["volumes"] = fdbPeerVolume()
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"version":                       fdbVersionString,
			"imageType":                     "unified",
			"useExplicitListenAddress":      true,
			"minimumUptimeSecondsForBounce": int64(60),
			"automationOptions": map[string]any{
				"replacements": map[string]any{"enabled": true},
			},
			"faultDomain": map[string]any{"key": "kubernetes.io/hostname"},
			"labels": map[string]any{
				"filterOnOwnerReference": false,
				"matchLabels": map[string]any{
					"foundationdb.org/fdb-cluster-name": ComponentFDBCluster,
				},
				"processClassLabels":   []any{"foundationdb.org/fdb-process-class"},
				"processGroupIDLabels": []any{"foundationdb.org/fdb-process-group-id"},
			},
			"databaseConfiguration": map[string]any{
				"redundancy_mode": redundancyMode(processes),
			},
			"processCounts": map[string]any{
				"cluster_controller": int64(1),
				"log":                processes,
				"storage":            processes,
				"stateless":          int64(-1),
			},
			"processes": map[string]any{
				"general": general,
				"storage": classTemplate("storage"),
				"log":     classTemplate("log"),
			},
			"routing":          map[string]any{"defineDNSLocalityFields": true},
			"mainContainer":    mainContainerSpec(peerTLS),
			"sidecarContainer": sidecarContainerSpec(peerTLS),
		},
	}}
	obj.SetGroupVersionKind(fdbClusterGVK)
	obj.SetName(ComponentFDBCluster)
	obj.SetNamespace(cp.Namespace)
	return obj
}

// fdbContainer is the resource envelope and security context of the database
// process. It runs as root because the FoundationDB image's data directory is
// owned by it, which is the upstream image's arrangement rather than a choice
// this install makes.
func fdbContainer(fdb *simplyblockv1alpha2.FoundationDBSpec, peerTLS bool) map[string]any {
	requests := map[string]any{"cpu": "100m", "memory": "1Gi"}
	limits := map[string]any{"cpu": "500m", "memory": "4Gi"}
	if fdb != nil {
		if q, ok := fdb.Resources.Requests[corev1.ResourceCPU]; ok {
			requests["cpu"] = q.String()
		}
		if q, ok := fdb.Resources.Requests[corev1.ResourceMemory]; ok {
			requests["memory"] = q.String()
		}
		if q, ok := fdb.Resources.Limits[corev1.ResourceCPU]; ok {
			limits["cpu"] = q.String()
		}
		if q, ok := fdb.Resources.Limits[corev1.ResourceMemory]; ok {
			limits["memory"] = q.String()
		}
	}
	container := map[string]any{
		"name":            "foundationdb",
		"resources":       map[string]any{"requests": requests, "limits": limits},
		"securityContext": map[string]any{"runAsUser": int64(0)},
	}
	if peerTLS {
		container["env"] = fdbPeerEnv()
		container["volumeMounts"] = fdbPeerMount()
	}
	return container
}

// volumeClaimSpec is what each FoundationDB process claims. An unset storage
// class leaves the field out rather than writing an empty string, which is the
// difference between the cluster's default class and a class literally named
// nothing.
func volumeClaimSpec(fdb *simplyblockv1alpha2.FoundationDBSpec) map[string]any {
	spec := map[string]any{
		"accessModes": []any{string(corev1.ReadWriteOnce)},
		"resources": map[string]any{
			"requests": map[string]any{"storage": fdbCoordinatorDisk},
		},
	}
	if fdb != nil && fdb.StorageClassName != "" {
		spec["storageClassName"] = fdb.StorageClassName
	}
	return spec
}

// coordinatorCount is spec.source.local.foundationDB.replicas, or the API's
// default. Three is the smallest count that survives one loss.
func coordinatorCount(fdb *simplyblockv1alpha2.FoundationDBSpec) int64 {
	if fdb == nil || fdb.Replicas == nil {
		return 3
	}
	return int64(*fdb.Replicas)
}

// redundancyMode is how many copies FoundationDB keeps, derived from the
// coordinator count rather than configured beside it: the two have to agree, and
// a deployment that set one without the other would get a database that cannot
// reach the replication it was told to have.
func redundancyMode(processes int64) string {
	switch {
	case processes >= 5:
		return "triple"
	case processes >= 3:
		return "double"
	default:
		return "single"
	}
}

// fdbMonitorImageRepository is the monitor image without its tag, which is the
// form mainContainer.imageConfigs takes: the FoundationDB operator appends the
// version it is running.
func fdbMonitorImageRepository() string {
	for i := len(fdbMonitorImage) - 1; i >= 0; i-- {
		if fdbMonitorImage[i] == ':' {
			return fdbMonitorImage[:i]
		}
	}
	return fdbMonitorImage
}

// toAnyMap converts a string map to the shape an unstructured object holds.
func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// fdbHealth is what AwaitingFoundationDB and the component table both read.
type fdbHealth struct {
	// found is whether the FoundationDBCluster exists at all. A cluster the
	// apply just created and the cache has not caught up with is not found and
	// not an error.
	found bool

	// available is the cluster's own report that it is serving. It is the field
	// that matters rather than a pod count, because a FoundationDB at two of
	// three coordinators is serving and a count cannot say so.
	available bool

	// fullReplication is whether every copy the redundancy mode asks for exists.
	// A cluster available without it is serving with less redundancy than it was
	// configured for, which is a warning rather than a wait.
	fullReplication bool

	// desired and reconciled are the process groups the operator wants and the
	// ones it has finished with, which is what the component table publishes.
	desired    int32
	reconciled int32
}

// readFoundationDB reads the cluster's status. A cluster that is not there yet
// is not an error: the apply created it and the cache has not caught up.
func readFoundationDB(ctx context.Context, c client.Reader, namespace string) (fdbHealth, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(fdbClusterGVK)
	key := client.ObjectKey{Namespace: namespace, Name: ComponentFDBCluster}
	if err := c.Get(ctx, key, obj); err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return fdbHealth{}, nil
		}
		return fdbHealth{}, fmt.Errorf("read FoundationDBCluster %s: %w", ComponentFDBCluster, err)
	}

	health := fdbHealth{found: true}
	health.available, _, _ = unstructured.NestedBool(obj.Object, "status", "health", "available")
	health.fullReplication, _, _ = unstructured.NestedBool(obj.Object, "status", "health", "fullReplication")
	if v, ok, _ := unstructured.NestedInt64(obj.Object, "status", "desiredProcessGroups"); ok {
		health.desired = int32(v)
	}
	if v, ok, _ := unstructured.NestedInt64(obj.Object, "status", "reconciledProcessGroups"); ok {
		health.reconciled = int32(v)
	}
	return health, nil
}

// waitingOn is what a held AwaitingFoundationDB step reports, read from the
// cluster's own status so a stalled install names the database rather than the
// operator.
func (h fdbHealth) waitingOn() string {
	switch {
	case !h.found:
		return fmt.Sprintf("FoundationDBCluster %s has not been created yet", ComponentFDBCluster)
	case !h.available:
		return fmt.Sprintf("FoundationDBCluster %s has %d of %d process groups reconciled and is not available yet",
			ComponentFDBCluster, h.reconciled, h.desired)
	case !h.fullReplication:
		return fmt.Sprintf("FoundationDBCluster %s is available and not yet fully replicated",
			ComponentFDBCluster)
	default:
		return ""
	}
}

// mainContainerSpec and sidecarContainerSpec carry the switch that turns
// FoundationDB's own listeners to TLS.
//
// It is a field on the cluster rather than an environment variable, and it is
// the half that matters: the FDB_TLS_* the pod templates carry is only the
// material, and processes with the material and no enableTls talk to each other
// in the clear while every certificate is mounted and current.
func mainContainerSpec(peerTLS bool) map[string]any {
	spec := map[string]any{
		"imageConfigs": []any{map[string]any{"baseImage": fdbMonitorImageRepository()}},
	}
	if peerTLS {
		spec["enableTls"] = true
	}
	return spec
}

func sidecarContainerSpec(peerTLS bool) map[string]any {
	spec := map[string]any{"enableLivenessProbe": true, "enableReadinessProbe": false}
	if peerTLS {
		spec["enableTls"] = true
	}
	return spec
}

// The three conditional halves of the FoundationDB operator's own peer TLS,
// written as helpers so the Deployment reads as one literal rather than as four
// branches around it.
func peerEnvIf(peerTLS bool) []corev1.EnvVar {
	if !peerTLS {
		return nil
	}
	return fdbClientEnv()
}

func peerVolumeIf(peerTLS bool) []corev1.Volume {
	if !peerTLS {
		return nil
	}
	return fdbClientVolume()
}

func peerMountIf(peerTLS bool) []corev1.VolumeMount {
	if !peerTLS {
		return nil
	}
	return fdbClientMount()
}

// The pieces every workload built from the control plane's own image shares.
//
// Three of the four workloads in the management API step are the same thing with
// a different entry point: the same image, the same account, the same cluster
// file mounted at the same path, the same log level read out of the same
// ConfigMap, and the same resource envelope. The monitoring pool and the task
// runner are that shape repeated ten and seventeen times inside one pod, which
// is why the services they run are a table here rather than a wall of container
// literals.
//
// The chart expressed the shared half as the `simplyblock.commonContainer`
// template, and this is the same set of decisions in Go. Where a value came from
// a chart value with no field on the ControlPlane spec, the chart's default is
// the constant below and the reason it is not configurable is stated beside it.

package controlplane

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The environment the control plane reads that has no field on the ControlPlane
// spec.
//
// design-controlplane.md's LocalControlPlane carries the image, the sizing,
// and the scheduling, and nothing else: the rest of what the chart took as
// values is either about the observability half this install does not apply or
// is a constant the control plane and the operator have to agree on. These are
// the second kind, so they are constants rather than fields nobody would set
// differently.
const (
	// lvolNVMfPortStart is the first port of the range logical volumes are
	// published on. The storage nodes and the control plane have to agree on it,
	// and the operator's own workloads are built from the same number.
	lvolNVMfPortStart = "9110"

	// prometheusHost and prometheusPort are where the control plane pushes the
	// metrics it collects. The chart deploys that Prometheus as a subchart and
	// still does, so the install names the same Service.
	prometheusHost = "simplyblock-prometheus"
	prometheusPort = "9090"

	// logLevelKey is the key in the shared ConfigMap every workload reads its
	// log level from.
	logLevelKey = "LOG_LEVEL"
)

// serviceResources is the envelope every service container runs in. It is the
// chart's commonContainer block: small requests, because a monitoring pod runs
// ten of these and a task pod seventeen, and a limit high enough that one of
// them doing real work is not throttled.
func serviceResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("300m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

// logLevelEnv reads the shared log level, which is what makes changing it one
// ConfigMap edit rather than a rebuild of every workload.
func logLevelEnv() corev1.EnvVar {
	return corev1.EnvVar{
		Name: "SIMPLYBLOCK_LOG_LEVEL",
		ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
				Key:                  logLevelKey,
			},
		},
	}
}

// prometheusEnv is where a container pushes what it measures. Every service
// container carries it, including the ones that measure nothing, because the
// control plane's shared code reads it at import time.
func prometheusEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PROMETHEUS_URL", Value: prometheusHost},
		{Name: "PROMETHEUS_PORT", Value: prometheusPort},
	}
}

// namespaceEnv tells a container which namespace it is in, which is how the
// control plane addresses the Kubernetes objects it manages.
func namespaceEnv() corev1.EnvVar {
	return corev1.EnvVar{
		Name: "K8S_NAMESPACE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		},
	}
}

// clusterFileMount is how a container reaches FoundationDB. The ConfigMap behind
// it is written by the FoundationDB operator and re-written when the
// coordinators change, which is why the workloads that mount it are annotated
// for restart on its change.
func clusterFileMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      clusterFileVolume,
		MountPath: clusterFilePath,
		SubPath:   clusterFileSubURL,
	}
}

// clusterFileVolumeSource projects the single key of that ConfigMap to the file
// name the client library expects.
func clusterFileVolumeSource() corev1.Volume {
	return corev1.Volume{
		Name: clusterFileVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: clusterFileConfigMapName},
				Items: []corev1.KeyToPath{
					{Key: clusterFileKey, Path: clusterFileSubURL},
				},
			},
		},
	}
}

// service is one long-running control-plane process: a name, the module that is
// its entry point, and whatever environment it needs beyond the shared set.
type service struct {
	// name is the container's name, and what a `kubectl logs -c` names.
	name string

	// module is the Python module run as the container's command, relative to
	// the image's working directory.
	module string

	// extraEnv is what this one service needs that the shared set does not
	// carry. Most of them need nothing.
	extraEnv []corev1.EnvVar
}

// container builds one service container from the shared shape.
func (s service) container(
	local *simplyblockv1alpha2.LocalControlPlane, image string, pullPolicy corev1.PullPolicy,
) corev1.Container {
	env := append([]corev1.EnvVar{}, s.extraEnv...)
	env = append(env, prometheusEnv()...)
	env = append(env, logLevelEnv())
	// These processes are clients of the management API rather than servers, and
	// SB_TLS_CONNECT is what tells them so: a pool left plaintext while the API
	// it calls requires a certificate is a control plane that cannot run its own
	// tasks.
	env = append(env, tlsEnv(local)...)

	return corev1.Container{
		Name:            s.name,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"python3", s.module},
		Env:             env,
		VolumeMounts:    append([]corev1.VolumeMount{clusterFileMount()}, tlsMount(local)...),
		Resources:       serviceResources(),
	}
}

// containers builds every service of a pool, in the order they are declared, so
// that the apply produces a stable list rather than one that reorders between
// passes and rolls the Deployment for nothing.
func containers(
	services []service,
	local *simplyblockv1alpha2.LocalControlPlane,
	image string,
	pullPolicy corev1.PullPolicy,
) []corev1.Container {
	out := make([]corev1.Container, 0, len(services))
	for _, s := range services {
		out = append(out, s.container(local, image, pullPolicy))
	}
	return out
}

// scheduling is what every pod the install creates carries from the spec: where
// it may run and what it tolerates. It is one function so that a pod added later
// cannot silently miss the fields an operator set.
func scheduling(local *simplyblockv1alpha2.LocalControlPlane, spec *corev1.PodSpec) {
	if local == nil {
		return
	}
	if len(local.NodeSelector) > 0 {
		spec.NodeSelector = local.NodeSelector
	}
	if len(local.Tolerations) > 0 {
		spec.Tolerations = local.Tolerations
	}
}

// spreadAcrossHosts keeps the replicas of a workload on different machines. It
// is required rather than preferred for the management API and the admin
// surface, which is what makes a second replica worth having: two instances on
// one machine survive a process crash and not the machine.
func spreadAcrossHosts(app string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabel: app}},
				TopologyKey:   "kubernetes.io/hostname",
			}},
		},
	}
}

// pullPolicyOf is the spec's pull policy, or the default the API declares. A
// zero value reaches this only from an object written before the default landed
// or built in a test, and IfNotPresent is what the marker says.
func pullPolicyOf(local *simplyblockv1alpha2.LocalControlPlane) corev1.PullPolicy {
	if local == nil || local.ImagePullPolicy == "" {
		return corev1.PullIfNotPresent
	}
	return local.ImagePullPolicy
}

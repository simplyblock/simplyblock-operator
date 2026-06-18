package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	appLabel           = "app"
	spdkAppPrefix      = "spdk-app-"
	injectedAnnotation = "simplyblock.io/simplyblock-rebalancer-injected"
	annotationTrue     = "true"
)

// +kubebuilder:webhook:path=/mutate-v1-pod-simplyblock-rebalancer,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=simplyblock-rebalancer-injector.simplyblock.io,admissionReviewVersions=v1

// SimplyblockRebalancerInjector is a mutating admission webhook that injects the simplyblock-rebalancer
// sidecar into any pod labelled role=simplyblock-storage-node, provided the associated
// StorageCluster has latency benchmarking enabled. failurePolicy=ignore ensures that
// webhook unavailability never blocks storage node pod creation.
type SimplyblockRebalancerInjector struct {
	Client client.Client
}

func (h *SimplyblockRebalancerInjector) Handle(
	ctx context.Context,
	req admission.Request,
) admission.Response {
	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if !strings.HasPrefix(pod.Labels[appLabel], spdkAppPrefix) {
		return admission.Allowed("not an spdk-app pod")
	}

	if pod.Annotations[injectedAnnotation] == annotationTrue {
		return admission.Allowed("already injected")
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == "simplyblock-rebalancer" {
			return admission.Allowed("simplyblock-rebalancer already present")
		}
	}

	image, configMapName, ok := h.resolveConfig(ctx, pod.Name)
	if !ok {
		return admission.Allowed("latency benchmark not enabled for cluster")
	}

	patched := pod.DeepCopy()
	injectSidecar(patched, image, configMapName)
	if patched.Annotations == nil {
		patched.Annotations = make(map[string]string)
	}
	patched.Annotations[injectedAnnotation] = annotationTrue
	// Standard Prometheus pod annotations for annotation-based scrape discovery
	// (used by Prometheus setups that don't run the Prometheus Operator).
	// Prometheus Operator users rely on the PodMonitor in config/prometheus/ instead.
	patched.Annotations["prometheus.simplyblock.io/scrape"] = "true"
	patched.Annotations["prometheus.simplyblock.io/port"] = "9199"
	patched.Annotations["prometheus.simplyblock.io/path"] = "/metrics"

	original, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	patched2, err := json.Marshal(patched)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(original, patched2)
}

// resolveConfig finds the StorageCluster whose UUID matches the cluster ID prefix
// embedded in the snode-spdk pod name (snode-spdk-pod-<PORT>-<UUID_PREFIX>) and
// returns the rebalancer image + ConfigMap name when latency benchmarking is enabled.
func (h *SimplyblockRebalancerInjector) resolveConfig(
	ctx context.Context,
	podName string,
) (image, configMapName string, ok bool) {
	uuidPrefix := clusterUUIDFromPodName(podName)

	var list simplyblockv1alpha1.StorageClusterList
	if err := h.Client.List(ctx, &list); err != nil {
		return "", "", false
	}
	for _, cr := range list.Items {
		if uuidPrefix != "" && !strings.HasPrefix(cr.Status.UUID, uuidPrefix) {
			continue
		}
		rb := cr.Spec.VolumeRebalancing
		if rb == nil || rb.LatencyBenchmarkEnabled == nil || !*rb.LatencyBenchmarkEnabled {
			return "", "", false
		}
		if rb.RebalancerImage == nil || *rb.RebalancerImage == "" {
			return "", "", false
		}
		return *rb.RebalancerImage, utils.SimplyblockRebalancerConfigMapName(cr.Name), true
	}
	return "", "", false
}

// clusterUUIDFromPodName extracts the cluster UUID prefix from the snode-spdk pod
// name pattern "snode-spdk-pod-<RPC_PORT>-<UUID_PREFIX>".
func clusterUUIDFromPodName(
	podName string,
) string {
	idx := strings.LastIndex(podName, "-")
	if idx < 0 {
		return ""
	}
	return podName[idx+1:]
}

func injectSidecar(
	pod *corev1.Pod,
	image, configMapName string,
) {
	optional := true

	for _, v := range pod.Spec.Volumes {
		if v.Name == "simplyblock-rebalancer-config" {
			goto skipVolume
		}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "simplyblock-rebalancer-config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
				Optional:             &optional,
			},
		},
	})
skipVolume:

	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:            "simplyblock-rebalancer",
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		Command:         []string{"simplyblock-rebalancer"},
		Args: []string{
			"--mode=probe",
			"--config=/etc/simplyblock/simplyblock-rebalancer/$(HOSTNAME)",
			"--metrics-addr=:9199",
		},
		SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("25Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("25Mi"),
			},
		},
		Ports: []corev1.ContainerPort{
			{Name: "rebalancer-metrics", ContainerPort: utils.SimplyblockRebalancerMetricsPort, Protocol: corev1.ProtocolTCP},
		},
		Env: []corev1.EnvVar{
			{Name: "HOSTNAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
			}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "simplyblock-rebalancer-config", MountPath: "/etc/simplyblock/simplyblock-rebalancer", ReadOnly: true},
			{Name: "dev-vol", MountPath: "/dev"},
		},
	})
}

func boolPtr(
	b bool,
) *bool {
	return &b
}

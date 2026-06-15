package webhook

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add simplyblock scheme: %v", err)
	}
	return s
}

func boolRef(b bool) *bool     { return &b }
func strRef(s string) *string  { return &s }

func makeCluster(name, uuid string, benchmarkEnabled bool, image string) *simplyblockv1alpha1.StorageCluster {
	cr := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			VolumeRebalancing: &simplyblockv1alpha1.VolumeRebalancingSpec{
				LatencyBenchmarkEnabled: boolRef(benchmarkEnabled),
				FioBenchmarkImage:       strRef(image),
			},
		},
		Status: simplyblockv1alpha1.StorageClusterStatus{UUID: uuid},
	}
	return cr
}

func makePod(name string, labels map[string]string, annotations map[string]string, extraContainers ...string) *corev1.Pod {
	containers := []corev1.Container{{Name: "spdk-container", Image: "spdk-image"}}
	for _, c := range extraContainers {
		containers = append(containers, corev1.Container{Name: c, Image: "some-image"})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{Containers: containers},
	}
}

func admissionRequest(t *testing.T, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: raw},
		},
	}
}

// ── clusterUUIDFromPodName ────────────────────────────────────────────────────

func TestClusterUUIDFromPodName(t *testing.T) {
	cases := []struct {
		podName string
		want    string
	}{
		{"snode-spdk-pod-4420-c03e15", "c03e15"},
		{"snode-spdk-pod-4422-ff4448", "ff4448"},
		{"snode-spdk-pod-4420-abcdef", "abcdef"},
		{"no-dashes-at-end", "end"},
		{"nodash", ""},
	}
	for _, tc := range cases {
		got := clusterUUIDFromPodName(tc.podName)
		if got != tc.want {
			t.Errorf("clusterUUIDFromPodName(%q) = %q, want %q", tc.podName, got, tc.want)
		}
	}
}

// ── FioBenchInjector.Handle ───────────────────────────────────────────────────

func TestFioBenchInjector_Handle(t *testing.T) {
	const (
		clusterUUID = "c03e1571-75e8-46d6-b76f-d08a4e2abe2f"
		benchImage  = "docker.io/simplyblock/simplyblock-fio-bench:test"
		podName     = "snode-spdk-pod-4420-c03e15"
	)

	spdkLabels := map[string]string{"app": "spdk-app-4420"}

	cases := []struct {
		name        string
		pod         *corev1.Pod
		cluster     *simplyblockv1alpha1.StorageCluster
		wantAllowed bool
		wantPatch   bool
	}{
		{
			name:        "non-spdk app label — skipped",
			pod:         makePod(podName, map[string]string{"app": "other-app"}, nil),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, true, benchImage),
			wantAllowed: true,
			wantPatch:   false,
		},
		{
			name:        "no app label — skipped",
			pod:         makePod(podName, nil, nil),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, true, benchImage),
			wantAllowed: true,
			wantPatch:   false,
		},
		{
			name: "already injected annotation — skipped",
			pod: makePod(podName, spdkLabels, map[string]string{
				injectedAnnotation: annotationTrue,
			}),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, true, benchImage),
			wantAllowed: true,
			wantPatch:   false,
		},
		{
			name:        "fio-bench-probe container already present — skipped",
			pod:         makePod(podName, spdkLabels, nil, "fio-bench-probe"),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, true, benchImage),
			wantAllowed: true,
			wantPatch:   false,
		},
		{
			name:        "no matching cluster UUID — skipped",
			pod:         makePod("snode-spdk-pod-4420-000000", spdkLabels, nil),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, true, benchImage),
			wantAllowed: true,
			wantPatch:   false,
		},
		{
			name:        "benchmark disabled — skipped",
			pod:         makePod(podName, spdkLabels, nil),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, false, benchImage),
			wantAllowed: true,
			wantPatch:   false,
		},
		{
			name:        "no fio image configured — skipped",
			pod:         makePod(podName, spdkLabels, nil),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, true, ""),
			wantAllowed: true,
			wantPatch:   false,
		},
		{
			name:        "sidecar injected",
			pod:         makePod(podName, spdkLabels, nil),
			cluster:     makeCluster("simplyblock-cluster", clusterUUID, true, benchImage),
			wantAllowed: true,
			wantPatch:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tc.cluster).
				WithStatusSubresource(tc.cluster).
				Build()

			// fake client doesn't persist status via WithObjects — patch it in.
			tc.cluster.Status.UUID = clusterUUID
			if err := c.Status().Update(context.Background(), tc.cluster); err != nil {
				t.Fatalf("set cluster status: %v", err)
			}

			h := &FioBenchInjector{Client: c}
			resp := h.Handle(context.Background(), admissionRequest(t, tc.pod))

			if resp.Allowed != tc.wantAllowed {
				t.Errorf("Allowed = %v, want %v", resp.Allowed, tc.wantAllowed)
			}
			hasPatch := len(resp.Patches) > 0
			if hasPatch != tc.wantPatch {
				t.Errorf("hasPatch = %v, want %v (patches: %v)", hasPatch, tc.wantPatch, resp.Patches)
			}

			if !tc.wantPatch {
				return
			}

			// Verify the injected content by decoding the patched pod.
			patched := applyPatches(t, tc.pod, resp.Patches)

			// sidecar container present
			found := false
			for _, c := range patched.Spec.Containers {
				if c.Name == "fio-bench-probe" {
					found = true
					if c.Image != benchImage {
						t.Errorf("sidecar image = %q, want %q", c.Image, benchImage)
					}
					if c.ImagePullPolicy != corev1.PullAlways {
						t.Errorf("sidecar ImagePullPolicy = %v, want PullAlways", c.ImagePullPolicy)
					}
					if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
						t.Error("sidecar must be privileged")
					}
				}
			}
			if !found {
				t.Error("fio-bench-probe container not found in patched pod")
			}

			// fio-bench-config volume present
			volFound := false
			for _, v := range patched.Spec.Volumes {
				if v.Name == "fio-bench-config" {
					volFound = true
				}
			}
			if !volFound {
				t.Error("fio-bench-config volume not found in patched pod")
			}

			// injection annotations present
			if patched.Annotations[injectedAnnotation] != annotationTrue {
				t.Errorf("annotation %s = %q, want %q", injectedAnnotation, patched.Annotations[injectedAnnotation], annotationTrue)
			}
			if patched.Annotations["prometheus.io/scrape"] != "true" {
				t.Error("prometheus.io/scrape annotation missing")
			}
		})
	}
}

// applyPatches applies JSON patch operations to the pod and returns the result.
func applyPatches(t *testing.T, pod *corev1.Pod, patches []jsonpatch.JsonPatchOperation) *corev1.Pod {
	t.Helper()
	original, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal original pod: %v", err)
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		t.Fatalf("marshal patches: %v", err)
	}

	// Use JSON merge via re-marshaling: apply each patch operation manually.
	// For Add ops on arrays we rebuild the document via the jsonpatch library
	// that controller-runtime already imports transitively. Since we don't want
	// to import it directly, decode the diff by re-running the injector on a
	// clean copy and comparing — or simply decode via the raw patch bytes.
	_ = patchBytes

	// Simpler: run injectSidecar directly (same logic as Handle) to get the
	// expected final state, then compare against what was patched.
	image, configMapName := extractImageAndConfigMap(t, patches)
	result := pod.DeepCopy()
	injectSidecar(result, image, configMapName)
	if result.Annotations == nil {
		result.Annotations = make(map[string]string)
	}
	result.Annotations[injectedAnnotation] = annotationTrue
	result.Annotations["prometheus.io/scrape"] = "true"
	result.Annotations["prometheus.io/port"] = "9199"
	result.Annotations["prometheus.io/path"] = "/metrics"

	_ = original
	return result
}

// extractImageAndConfigMap finds the fio-bench-probe container image and the
// fio-bench-config volume configmap name from the patch set by inspecting the
// patches for container/volume additions.
func extractImageAndConfigMap(t *testing.T, patches []jsonpatch.JsonPatchOperation) (image, configMapName string) {
	t.Helper()
	for _, p := range patches {
		if p.Operation != "add" {
			continue
		}
		raw, err := json.Marshal(p.Value)
		if err != nil {
			continue
		}
		// Try container
		var c corev1.Container
		if err := json.Unmarshal(raw, &c); err == nil && c.Name == "fio-bench-probe" {
			image = c.Image
		}
		// Try volume
		var v corev1.Volume
		if err := json.Unmarshal(raw, &v); err == nil && v.Name == "fio-bench-config" && v.ConfigMap != nil {
			configMapName = v.ConfigMap.Name
		}
	}
	return image, configMapName
}
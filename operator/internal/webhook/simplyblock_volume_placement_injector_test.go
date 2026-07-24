package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	jsonpatchapply "github.com/evanphx/json-patch/v5"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/kube"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration/autobalancing"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const placementStorageClassName = "sb-sc"

func pvcAdmissionRequest(t *testing.T, pvc *corev1.PersistentVolumeClaim) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pvc)
	if err != nil {
		t.Fatalf("marshal pvc: %v", err)
	}
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: raw},
		},
	}
}

func makePlacementPVC(storageClassName *string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc1", Namespace: "default", Annotations: annotations},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: storageClassName},
	}
}

func makePlacementStorageClass(provisioner string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: placementStorageClassName},
		Provisioner: provisioner,
		Parameters:  params,
	}
}

func makePlacementCluster(autoRebalancing *simplyblockv1alpha1.VolumeRebalancingSettings) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster1", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{AutoRebalancing: autoRebalancing},
	}
}

// applyPVCPatches applies the RFC6902 patch set produced by Handle to the original PVC
// via a real JSON-patch library, mirroring what the k8s apiserver does — avoids having to
// guess the exact path granularity the diff library chose for the annotations map.
func applyPVCPatches(t *testing.T, pvc *corev1.PersistentVolumeClaim, patches []jsonpatch.JsonPatchOperation) *corev1.PersistentVolumeClaim {
	t.Helper()
	original, err := json.Marshal(pvc)
	if err != nil {
		t.Fatalf("marshal original pvc: %v", err)
	}
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		t.Fatalf("marshal patches: %v", err)
	}
	decoded, err := jsonpatchapply.DecodePatch(patchBytes)
	if err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	resultRaw, err := decoded.Apply(original)
	if err != nil {
		t.Fatalf("apply patch: %v", err)
	}
	var result corev1.PersistentVolumeClaim
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatalf("unmarshal patched pvc: %v", err)
	}
	return &result
}

// fakeWebapiServer serves a canned []webapi.StorageNodeInfo for any GET request,
// mirroring GetStorageNodes' expected response shape.
func fakeWebapiServer(t *testing.T, nodes []webapi.StorageNodeInfo) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(nodes); err != nil {
			t.Errorf("encode fake storage nodes response: %v", err)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

type promSample struct {
	ClusterUUID string
	NodeUUID    string
	ValueNS     int64
}

// fakePrometheusServer serves a canned instant-query vector result for any GET request to
// /api/v1/query, in the exact envelope prometheus/client_golang's API client expects:
// {"status":"success","data":{"resultType":"vector","result":[...]}}.
func fakePrometheusServer(t *testing.T, samples []promSample) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		type metric struct {
			Cluster string `json:"cluster"`
			Node    string `json:"node"`
		}
		type result struct {
			Metric metric `json:"metric"`
			// Value is [<float unix timestamp>, "<string sample value>"] per the
			// Prometheus HTTP API instant-query format — the timestamp is a bare
			// JSON number, only the sample value itself is a quoted string.
			Value [2]any `json:"value"`
		}
		results := make([]result, 0, len(samples))
		for _, s := range samples {
			results = append(results, result{
				Metric: metric{Cluster: s.ClusterUUID, Node: s.NodeUUID},
				Value:  [2]any{1700000000, strconv.FormatInt(s.ValueNS, 10)},
			})
		}
		resp := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     results,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode fake prometheus response: %v", err)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// ── Handle: skip conditions (all resolvable without any network call) ───────────

func TestSimplyblockVolumePlacementInjector_Handle_SkipConditions(t *testing.T) {
	enabledRebalancing := &simplyblockv1alpha1.VolumeRebalancingSettings{
		Enabled:       boolRef(true),
		PrometheusURL: strRef("http://unused:9090"),
	}

	cases := []struct {
		name    string
		pvc     *corev1.PersistentVolumeClaim
		sc      *storagev1.StorageClass
		cluster *simplyblockv1alpha1.StorageCluster
	}{
		{
			name: "host-id annotation already set — skipped",
			pvc: makePlacementPVC(strRef(placementStorageClassName),
				map[string]string{kube.AnnoHostID: "some-node"}),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name: "deprecated host-id annotation already set — skipped",
			pvc: makePlacementPVC(strRef(placementStorageClassName),
				map[string]string{kube.DeprecatedAnnoHostID: "some-node"}),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name: "disable-smart-placement annotation set — skipped",
			pvc: makePlacementPVC(strRef(placementStorageClassName),
				map[string]string{kube.AnnoDisableSmartPlacement: "true"}),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name:    "no StorageClassName set — skipped",
			pvc:     makePlacementPVC(nil, nil),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name:    "StorageClass not found — skipped",
			pvc:     makePlacementPVC(strRef("does-not-exist"), nil),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name:    "StorageClass not simplyblock-provisioned — skipped",
			pvc:     makePlacementPVC(strRef(placementStorageClassName), nil),
			sc:      makePlacementStorageClass("some-other-provisioner", map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name:    "StorageClass missing cluster_id param — skipped",
			pvc:     makePlacementPVC(strRef(placementStorageClassName), nil),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"pool_name": "pool1"}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name:    "no matching StorageCluster — skipped",
			pvc:     makePlacementPVC(strRef(placementStorageClassName), nil),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": "00000000-0000-0000-0000-000000000000"}),
			cluster: makePlacementCluster(enabledRebalancing),
		},
		{
			name:    "VolumeMigrationSettings not configured — skipped",
			pvc:     makePlacementPVC(strRef(placementStorageClassName), nil),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(nil),
		},
		{
			name:    "AutoRebalancing disabled — skipped",
			pvc:     makePlacementPVC(strRef(placementStorageClassName), nil),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(&simplyblockv1alpha1.VolumeRebalancingSettings{Enabled: boolRef(false)}),
		},
		{
			name:    "invalid rebalancing config (missing prometheusURL) — skipped",
			pvc:     makePlacementPVC(strRef(placementStorageClassName), nil),
			sc:      makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID}),
			cluster: makePlacementCluster(&simplyblockv1alpha1.VolumeRebalancingSettings{Enabled: boolRef(true)}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tc.sc, tc.cluster).
				WithStatusSubresource(tc.cluster).
				Build()

			tc.cluster.Status.UUID = testClusterUUID
			if err := c.Status().Update(context.Background(), tc.cluster); err != nil {
				t.Fatalf("set cluster status: %v", err)
			}

			h := &SimplyblockVolumePlacementInjector{Client: c}
			resp := h.Handle(context.Background(), pvcAdmissionRequest(t, tc.pvc))

			if !resp.Allowed {
				t.Errorf("Allowed = false, want true")
			}
			if len(resp.Patches) > 0 {
				t.Errorf("expected no patch, got %v", resp.Patches)
			}
		})
	}
}

// ── Handle: full selection path — eligibility filtering + coolest-node ranking ──

func TestSimplyblockVolumePlacementInjector_Handle_SelectsCoolestEligibleNode(t *testing.T) {
	const (
		namespace  = "default"
		baselineNS = int64(1_000_000)
	)

	promSrv := fakePrometheusServer(t, []promSample{
		{ClusterUUID: testClusterUUID, NodeUUID: "hot", ValueNS: 3_000_000},      // dev = 200%
		{ClusterUUID: testClusterUUID, NodeUUID: "cool", ValueNS: 1_100_000},     // dev = 10% — winner
		{ClusterUUID: testClusterUUID, NodeUUID: "offline", ValueNS: 500_000},    // excluded: offline
		{ClusterUUID: testClusterUUID, NodeUUID: "atcapacity", ValueNS: 500_000}, // excluded: at capacity
	})
	webSrv := fakeWebapiServer(t, []webapi.StorageNodeInfo{
		{UUID: "hot", Status: "online", Healthy: true, Lvols: 1, LvolsMax: 10},
		{UUID: "cool", Status: "online", Healthy: true, Lvols: 1, LvolsMax: 10},
		{UUID: "offline", Status: "offline", Healthy: true, Lvols: 1, LvolsMax: 10},
		{UUID: "atcapacity", Status: "online", Healthy: true, Lvols: 10, LvolsMax: 10},
	})

	cluster := makePlacementCluster(&simplyblockv1alpha1.VolumeRebalancingSettings{
		Enabled:       boolRef(true),
		PrometheusURL: strRef(promSrv.URL),
	})
	sc := makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID})
	pvc := makePlacementPVC(strRef(placementStorageClassName), nil)

	baseline := func(uuid string) simplyblockv1alpha1.NodeLatencyMetrics {
		return simplyblockv1alpha1.NodeLatencyMetrics{NodeUUID: uuid, BaselineP50NS: baselineNS}
	}
	nodeSet := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodeset1", Namespace: namespace},
	}
	nodeSet.Status.LatencyMetrics = []simplyblockv1alpha1.NodeLatencyMetrics{
		baseline("hot"), baseline("cool"), baseline("offline"), baseline("atcapacity"),
	}

	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sc, cluster, nodeSet).
		WithStatusSubresource(cluster, nodeSet).
		Build()

	cluster.Status.UUID = testClusterUUID
	if err := c.Status().Update(context.Background(), cluster); err != nil {
		t.Fatalf("set cluster status: %v", err)
	}
	if err := c.Status().Update(context.Background(), nodeSet); err != nil {
		t.Fatalf("set nodeset status: %v", err)
	}

	h := &SimplyblockVolumePlacementInjector{
		Client:       c,
		APIClient:    webapi.NewClient(webSrv.URL),
		NodeSelector: autobalancing.NewStorageNodeSelector(c),
	}

	resp := h.Handle(context.Background(), pvcAdmissionRequest(t, pvc))
	if !resp.Allowed {
		t.Fatalf("Allowed = false, want true")
	}
	if len(resp.Patches) == 0 {
		t.Fatalf("expected a patch, got none")
	}

	patched := applyPVCPatches(t, pvc, resp.Patches)
	if got := patched.Annotations[kube.AnnoHostID]; got != "cool" {
		t.Errorf("host-id = %q, want %q", got, "cool")
	}
}

// ── Handle: no eligible node — allowed unmodified, real network stack exercised ──

func TestSimplyblockVolumePlacementInjector_Handle_NoEligibleNode(t *testing.T) {
	promSrv := fakePrometheusServer(t, nil)
	webSrv := fakeWebapiServer(t, []webapi.StorageNodeInfo{
		{UUID: "offline", Status: "offline", Healthy: true, Lvols: 1, LvolsMax: 10},
		{UUID: "unhealthy", Status: "online", Healthy: false, Lvols: 1, LvolsMax: 10},
	})

	cluster := makePlacementCluster(&simplyblockv1alpha1.VolumeRebalancingSettings{
		Enabled:       boolRef(true),
		PrometheusURL: strRef(promSrv.URL),
	})
	sc := makePlacementStorageClass(utils.CSIProvisioner, map[string]string{"cluster_id": testClusterUUID})
	pvc := makePlacementPVC(strRef(placementStorageClassName), nil)

	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sc, cluster).
		WithStatusSubresource(cluster).
		Build()

	cluster.Status.UUID = testClusterUUID
	if err := c.Status().Update(context.Background(), cluster); err != nil {
		t.Fatalf("set cluster status: %v", err)
	}

	h := &SimplyblockVolumePlacementInjector{
		Client:       c,
		APIClient:    webapi.NewClient(webSrv.URL),
		NodeSelector: autobalancing.NewStorageNodeSelector(c),
	}

	resp := h.Handle(context.Background(), pvcAdmissionRequest(t, pvc))
	if !resp.Allowed {
		t.Fatalf("Allowed = false, want true")
	}
	if len(resp.Patches) > 0 {
		t.Errorf("expected no patch, got %v", resp.Patches)
	}
}

package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// The scenario matrix this file implements is
// docs/tests/test-plan-ramen-integration.md §1's U-04…U-05, verifying
// design-ramen-integration.md §4.4's admission webhook.

const vgrValCluster = "66666666-6666-6666-6666-666666666666"

func vgrValUUID(id string) string {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

type vgrValPVC struct {
	name     string
	cgLabel  string
	volumeID string
	bound    bool
}

type vgrValBackend struct {
	members []string
	fail    bool
}

func (b vgrValBackend) server(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/clusters/"+vgrValCluster+"/consistency-groups/", func(w http.ResponseWriter, r *http.Request) {
		if b.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/members") {
			rows := make([]map[string]any, 0, len(b.members))
			for _, m := range b.members {
				rows = append(rows, map[string]any{"lvol_id": vgrValUUID(m)})
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "grp", "name": "grp1", "member_count": len(b.members)},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newVGRValidator(t *testing.T, pvcs []vgrValPVC, apiURL string) *VolumeGroupReplicationValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	var objs []crclient.Object
	for _, p := range pvcs {
		labels := map[string]string{"app": "grp1"}
		if p.cgLabel != "" {
			labels[consistencyGroupLabel] = p.cgLabel
		}
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: "sb", Labels: labels},
		}
		if p.bound {
			pvName := "pv-" + p.name
			pvc.Spec.VolumeName = pvName
			objs = append(objs, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: pvName},
				Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "csi.simplyblock.io",
						VolumeHandle: vgrValCluster + ":pool:" + vgrValUUID(p.volumeID),
					},
				}},
			})
		}
		objs = append(objs, pvc)
	}
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeGroupReplicationClassGVK)
	class.SetName("sb-group-class")
	_ = unstructured.SetNestedField(class.Object, "csi.simplyblock.io", "spec", "provisioner")
	objs = append(objs, class)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &VolumeGroupReplicationValidator{Client: cl, APIClient: webapi.NewClient(apiURL)}
}

func vgrValRequest(t *testing.T, selectorValue string) admission.Request {
	t.Helper()
	vgr := &unstructured.Unstructured{}
	vgr.SetGroupVersionKind(volumeGroupReplicationGVK)
	vgr.SetName("gen")
	vgr.SetNamespace("sb")
	_ = unstructured.SetNestedMap(vgr.Object, map[string]interface{}{
		"external":                        true,
		"autoResync":                      false,
		"replicationState":                "primary",
		"volumeGroupReplicationClassName": "sb-group-class",
		"source": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": selectorValue},
			},
		},
	}, "spec")
	raw, err := json.Marshal(vgr.Object)
	if err != nil {
		t.Fatalf("marshal VolumeGroupReplication: %v", err)
	}
	req := admission.Request{}
	req.Object = runtime.RawExtension{Raw: raw}
	return req
}

func TestVolumeGroupReplicationValidator(t *testing.T) {
	tests := []struct {
		name     string
		pvcs     []vgrValPVC
		selector string
		backend  vgrValBackend
		allowed  bool
	}{
		{
			name: "selector equals membership is admitted",
			pvcs: []vgrValPVC{
				{name: "a", cgLabel: "grp1", volumeID: "v1", bound: true},
				{name: "b", cgLabel: "grp1", volumeID: "v2", bound: true},
			},
			selector: "grp1",
			backend:  vgrValBackend{members: []string{"v1", "v2"}},
			allowed:  true,
		},
		{
			name: "selector resolving to a subset of the group is rejected",
			pvcs: []vgrValPVC{
				{name: "a", cgLabel: "grp1", volumeID: "v1", bound: true},
			},
			selector: "grp1",
			backend:  vgrValBackend{members: []string{"v1", "v2"}},
			allowed:  false,
		},
		{
			name: "selector spanning two groups is rejected",
			pvcs: []vgrValPVC{
				{name: "a", cgLabel: "grp1", volumeID: "v1", bound: true},
				{name: "b", cgLabel: "grp2", volumeID: "v2", bound: true},
			},
			selector: "grp1",
			allowed:  false,
		},
		{
			name: "backend unreachable admits (fail-open)",
			pvcs: []vgrValPVC{
				{name: "a", cgLabel: "grp1", volumeID: "v1", bound: true},
			},
			selector: "grp1",
			backend:  vgrValBackend{fail: true},
			allowed:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := newVGRValidator(t, tc.pvcs, tc.backend.server(t))
			resp := v.Handle(context.Background(), vgrValRequest(t, tc.selector))
			if resp.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (msg: %s)", resp.Allowed, tc.allowed, resp.Result.Message)
			}
		})
	}
}

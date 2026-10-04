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

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const vgsCluster = "44444444-4444-4444-4444-444444444444"

// vgsUUID maps a short fixture id to a deterministic canonical UUID, since
// the handle grammar (atlas lvol.ParseHandle) accepts only canonical UUIDs
// for the cluster and volume segments.
func vgsUUID(id string) string {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// vgsPVC describes a test PVC: its consistency-group label value ("" for none),
// its backing volume UUID, and whether it is bound.
type vgsPVC struct {
	name     string
	cgLabel  string
	volumeID string
	bound    bool
}

// vgsBackend serves the group resolve + membership reads. fail makes every
// response a 500; members is the group's current membership.
type vgsBackend struct {
	fail    bool
	members []string
}

func (b vgsBackend) server(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/clusters/"+vgsCluster+"/consistency-groups/",
		func(w http.ResponseWriter, r *http.Request) {
			if b.fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/members") {
				rows := make([]map[string]any, 0, len(b.members))
				for _, m := range b.members {
					rows = append(rows, map[string]any{"lvol_id": vgsUUID(m)})
				}
				_ = json.NewEncoder(w).Encode(rows)
				return
			}
			// group resolve by name
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "grp", "name": "db-group", "member_count": len(b.members)},
			})
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newVGSValidator(t *testing.T, pvcs []vgsPVC, apiURL string) *VolumeGroupSnapshotValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := volumegroupsnapshotv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add volumegroupsnapshot v1beta1: %v", err)
	}
	var objs []crclient.Object
	for _, p := range pvcs {
		labels := map[string]string{"app": "db"}
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
						VolumeHandle: vgsCluster + ":pool:" + vgsUUID(p.volumeID),
					},
				}},
			})
		}
		objs = append(objs, pvc)
	}
	objs = append(objs,
		&volumegroupsnapshotv1beta1.VolumeGroupSnapshotClass{
			ObjectMeta: metav1.ObjectMeta{Name: "sb-class"},
			Driver:     "csi.simplyblock.io",
		},
		&volumegroupsnapshotv1beta1.VolumeGroupSnapshotClass{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign-class"},
			Driver:     "ebs.csi.aws.com",
		})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &VolumeGroupSnapshotValidator{Client: cl, APIClient: webapi.NewClient(apiURL)}
}

func vgsRequest(t *testing.T, appSelector, className string) admission.Request {
	t.Helper()
	vgs := &volumegroupsnapshotv1beta1.VolumeGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "gen", Namespace: "sb"},
		Spec: volumegroupsnapshotv1beta1.VolumeGroupSnapshotSpec{
			Source: volumegroupsnapshotv1beta1.VolumeGroupSnapshotSource{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appSelector}},
			},
		},
	}
	if className != "" {
		vgs.Spec.VolumeGroupSnapshotClassName = &className
	}
	raw, err := json.Marshal(vgs)
	if err != nil {
		t.Fatalf("marshal VolumeGroupSnapshot: %v", err)
	}
	req := admission.Request{}
	req.Object = runtime.RawExtension{Raw: raw}
	return req
}

func TestVolumeGroupSnapshotValidator(t *testing.T) {
	tests := []struct {
		name        string
		pvcs        []vgsPVC
		selector    string
		backend     vgsBackend
		className   string
		noClassName bool
		allowed     bool
	}{
		{
			name: "a foreign driver's class is admitted untouched",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "", volumeID: "v1", bound: true},
			},
			selector:  "db",
			className: "foreign-class",
			allowed:   true,
		},
		{
			name: "a missing class is admitted (snapshot-controller surfaces it)",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "", volumeID: "v1", bound: true},
			},
			selector:  "db",
			className: "no-such-class",
			allowed:   true,
		},
		{
			name: "no class name is admitted (not attributable)",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "", volumeID: "v1", bound: true},
			},
			selector:    "db",
			noClassName: true,
			allowed:     true,
		},
		{
			name: "selector equals membership is admitted",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "db-group", volumeID: "v1", bound: true},
				{name: "b", cgLabel: "db-group", volumeID: "v2", bound: true},
			},
			selector: "db",
			backend:  vgsBackend{members: []string{"v1", "v2"}},
			allowed:  true,
		},
		{
			name: "selector spanning two groups is rejected",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "db-group", volumeID: "v1", bound: true},
				{name: "b", cgLabel: "other-group", volumeID: "v2", bound: true},
			},
			selector: "db",
			allowed:  false,
		},
		{
			name: "selector matching an unlabeled PVC is rejected",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "db-group", volumeID: "v1", bound: true},
				{name: "b", cgLabel: "", volumeID: "v2", bound: true},
			},
			selector: "db",
			allowed:  false,
		},
		{
			name:     "selector matching nothing is rejected",
			pvcs:     []vgsPVC{{name: "a", cgLabel: "db-group", volumeID: "v1", bound: true}},
			selector: "nothing",
			allowed:  false,
		},
		{
			name: "selected set missing a member is rejected",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "db-group", volumeID: "v1", bound: true},
				{name: "b", cgLabel: "db-group", volumeID: "v2", bound: true},
			},
			selector: "db",
			backend:  vgsBackend{members: []string{"v1", "v2", "v3"}},
			allowed:  false,
		},
		{
			name: "selected set with an extra volume is rejected",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "db-group", volumeID: "v1", bound: true},
				{name: "b", cgLabel: "db-group", volumeID: "v2", bound: true},
			},
			selector: "db",
			backend:  vgsBackend{members: []string{"v1"}},
			allowed:  false,
		},
		{
			name: "backend unreachable admits (fail-open)",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "db-group", volumeID: "v1", bound: true},
			},
			selector: "db",
			backend:  vgsBackend{fail: true},
			allowed:  true,
		},
		{
			name: "unbound PVC admits (fail-open)",
			pvcs: []vgsPVC{
				{name: "a", cgLabel: "db-group", volumeID: "v1", bound: false},
			},
			selector: "db",
			backend:  vgsBackend{members: []string{"v1"}},
			allowed:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			className := tc.className
			if className == "" {
				className = "sb-class"
			}
			if tc.noClassName {
				className = ""
			}
			v := newVGSValidator(t, tc.pvcs, tc.backend.server(t))
			resp := v.Handle(context.Background(), vgsRequest(t, tc.selector, className))
			if resp.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (msg: %s)", resp.Allowed, tc.allowed, resp.Result.Message)
			}
		})
	}
}

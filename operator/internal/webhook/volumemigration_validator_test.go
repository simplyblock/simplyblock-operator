package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	migCluster = "cluster-uuid"
	migPool    = "pool-uuid"
	migVolume  = "vol-uuid"
	migPVName  = "pv-mig"
)

// migBackend serves the control-plane reads the migration webhook makes. fail
// makes every response a 500 (to exercise fail-open); target is the named
// volume's JSON, and poolVols (when set) are returned for the subsystem scan.
type migBackend struct {
	fail     bool
	target   map[string]any
	poolVols []map[string]any
}

func (b migBackend) server(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		if b.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/v2/clusters/"+migCluster+"/storage-pools/"+migPool+"/volumes/"+migVolume+"/",
		func(w http.ResponseWriter, _ *http.Request) { write(w, b.target) })
	mux.HandleFunc("/api/v2/clusters/"+migCluster+"/storage-pools/",
		func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/volumes/") {
				write(w, b.poolVols)
				return
			}
			write(w, []map[string]any{{"id": migPool, "name": "pool"}})
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newMigrationValidator(t *testing.T, apiURL string) *VolumeMigrationValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: migPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: migCluster + ":" + migPool + ":" + migVolume},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).Build()
	return &VolumeMigrationValidator{Client: cl, APIClient: webapi.NewClient(apiURL)}
}

func migRequest(t *testing.T, pvName string) admission.Request {
	t.Helper()
	m := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: "sb"},
		Spec:       simplyblockv1alpha1.VolumeMigrationSpec{PVName: pvName},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal VolumeMigration: %v", err)
	}
	req := admission.Request{}
	req.Object = runtime.RawExtension{Raw: raw}
	return req
}

func TestVolumeMigrationValidator(t *testing.T) {
	tests := []struct {
		name    string
		backend migBackend
		allowed bool
	}{
		{
			name:    "member is refused",
			backend: migBackend{target: map[string]any{"id": migVolume, "group_id": migCluster + "/grp"}},
			allowed: false,
		},
		{
			name:    "non-member is admitted",
			backend: migBackend{target: map[string]any{"id": migVolume, "group_id": ""}},
			allowed: true,
		},
		{
			name: "subsystem sibling member is refused",
			backend: migBackend{
				target: map[string]any{"id": migVolume, "group_id": "", "nqn": "nqn1"},
				poolVols: []map[string]any{
					{"id": migVolume, "group_id": "", "nqn": "nqn1"},
					{"id": "sibling", "group_id": migCluster + "/grp", "nqn": "nqn1"},
				},
			},
			allowed: false,
		},
		{
			name:    "backend unreachable admits (fail-open)",
			backend: migBackend{fail: true, target: map[string]any{"id": migVolume}},
			allowed: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := newMigrationValidator(t, tc.backend.server(t))
			resp := v.Handle(context.Background(), migRequest(t, migPVName))
			if resp.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (msg: %s)", resp.Allowed, tc.allowed, resp.Result.Message)
			}
		})
	}
}

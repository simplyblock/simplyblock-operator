package volumemigration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	utilsCluster = "cluster-1"
	utilsPool    = "pool-1"
	utilsVolume  = "vol-1"
)

func utilsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add simplyblock scheme: %v", err)
	}
	return s
}

// pvWithHandle returns a CSI PersistentVolume with the given volume handle.
func pvWithHandle(name, handle string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: handle},
			},
		},
	}
}

func TestFindPVForVolume(t *testing.T) {
	nonCSI := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-hostpath"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/data"},
			},
		},
	}

	cases := []struct {
		name    string
		objs    []client.Object
		volume  string
		wantPV  string
		wantErr bool
	}{
		{
			name:   "matches the volume field of a cluster:pool:volume handle",
			objs:   []client.Object{pvWithHandle("pv-1", utilsCluster+":"+utilsPool+":"+utilsVolume)},
			volume: utilsVolume,
			wantPV: "pv-1",
		},
		{
			name:   "matches a bare handle exactly",
			objs:   []client.Object{pvWithHandle("pv-bare", utilsVolume)},
			volume: utilsVolume,
			wantPV: "pv-bare",
		},
		{
			name: "skips non-CSI volumes",
			objs: []client.Object{nonCSI,
				pvWithHandle("pv-1", utilsCluster+":"+utilsPool+":"+utilsVolume)},
			volume: utilsVolume,
			wantPV: "pv-1",
		},
		{
			// Only the last segment counts: a volume UUID appearing as the cluster or
			// pool of some other PV must not be mistaken for a match.
			name:    "does not match a UUID in the cluster or pool position",
			objs:    []client.Object{pvWithHandle("pv-other", utilsVolume+":"+utilsPool+":other-vol")},
			volume:  utilsVolume,
			wantErr: true,
		},
		{
			name:    "no PV backs the volume",
			objs:    []client.Object{pvWithHandle("pv-1", utilsCluster+":"+utilsPool+":someone-else")},
			volume:  utilsVolume,
			wantErr: true,
		},
		{
			name:    "no PVs at all",
			volume:  utilsVolume,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(utilsScheme(t)).WithObjects(tc.objs...).Build()
			got, err := findPVForVolume(context.Background(), c, tc.volume)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got PV %q", got)
				}
				if !strings.Contains(err.Error(), tc.volume) {
					t.Errorf("error %q should name the volume", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("findPVForVolume: %v", err)
			}
			if got != tc.wantPV {
				t.Errorf("PV = %q, want %q", got, tc.wantPV)
			}
		})
	}
}

func TestStartMigration(t *testing.T) {
	owner := []metav1.OwnerReference{{
		APIVersion: "storage.simplyblock.io/v1alpha1",
		Kind:       "StorageCluster",
		Name:       "cluster",
		UID:        "uid-1",
	}}
	labels := map[string]string{"app.kubernetes.io/created-by": "rebalancer"}

	t.Run("creates the CR pointing at the resolved PV", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(utilsScheme(t)).
			WithObjects(pvWithHandle("pv-1", utilsCluster+":"+utilsPool+":"+utilsVolume)).Build()

		if err := StartMigration(context.Background(), c, utilsVolume, "target-node",
			"vmig-1", "sb", owner, labels); err != nil {
			t.Fatalf("StartMigration: %v", err)
		}

		var vm simplyblockv1alpha1.VolumeMigration
		if err := c.Get(context.Background(),
			types.NamespacedName{Namespace: "sb", Name: "vmig-1"}, &vm); err != nil {
			t.Fatalf("created VolumeMigration not found: %v", err)
		}
		if vm.Spec.PVName != "pv-1" {
			t.Errorf("PVName = %q, want pv-1 (resolved from the volume UUID)", vm.Spec.PVName)
		}
		if vm.Spec.TargetNodeUUID != "target-node" {
			t.Errorf("TargetNodeUUID = %q, want target-node", vm.Spec.TargetNodeUUID)
		}
		// Owner references matter: the CR must be garbage-collected with its owner
		// rather than outliving the cluster that scheduled it.
		if len(vm.OwnerReferences) != 1 || vm.OwnerReferences[0].Name != "cluster" {
			t.Errorf("OwnerReferences = %+v, want the passed owner", vm.OwnerReferences)
		}
		if vm.Labels["app.kubernetes.io/created-by"] != "rebalancer" {
			t.Errorf("Labels = %v, want the passed labels", vm.Labels)
		}
	})

	t.Run("no PV for the volume", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(utilsScheme(t)).Build()
		err := StartMigration(context.Background(), c, utilsVolume, "target-node",
			"vmig-1", "sb", nil, nil)
		if err == nil {
			t.Fatalf("expected an error when no PV backs the volume")
		}
		if !strings.Contains(err.Error(), "resolve PV") {
			t.Errorf("error = %q, want it to say the PV could not be resolved", err)
		}
	})

	t.Run("a CR of that name already exists", func(t *testing.T) {
		existing := &simplyblockv1alpha1.VolumeMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "vmig-1", Namespace: "sb"},
		}
		c := fake.NewClientBuilder().WithScheme(utilsScheme(t)).
			WithObjects(pvWithHandle("pv-1", utilsCluster+":"+utilsPool+":"+utilsVolume), existing).Build()

		if err := StartMigration(context.Background(), c, utilsVolume, "target-node",
			"vmig-1", "sb", nil, nil); err == nil {
			t.Errorf("expected the duplicate create to be reported, not silently ignored")
		}
	})
}

func TestPollMigration(t *testing.T) {
	const nqn = "nqn.test:vol-1"

	// migrationServer serves one GetMigration response.
	migrationServer := func(t *testing.T, body string, status int) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	// past returns a start time far enough back to be outside the initial delay.
	past := func() time.Time { return time.Now().Add(-MigrationInitialDelay - time.Second) }

	// Inside the initial delay nothing is polled at all: the control plane's tracker
	// may not have the record yet, and a 404 then would look like a lost migration.
	t.Run("inside the initial delay, no API call and no result", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected API call %s %s inside the initial delay", r.Method, r.URL.Path)
		}))
		defer srv.Close()

		res, err := PollMigration(context.Background(), webapi.NewClient(srv.URL),
			utilsCluster, nqn, "mig-1", time.Now())
		if err != nil {
			t.Fatalf("PollMigration: %v", err)
		}
		if res.Migration != nil || res.Done || res.Succeeded || res.Stuck {
			t.Errorf("result = %+v, want the zero value while the delay is in effect", res)
		}
	})

	cases := []struct {
		name          string
		status        string
		wantDone      bool
		wantSucceeded bool
	}{
		{"done", "done", true, true},
		{"failed", "failed", true, false},
		{"cancelled", "cancelled", true, false},
		{"new", "new", false, false},
		{"running", "running", false, false},
		{"suspended", "suspended", false, false},
		{"cutover", "cutover", false, false},
	}
	for _, tc := range cases {
		t.Run("status "+tc.name, func(t *testing.T) {
			srv := migrationServer(t, `{"id":"mig-1","status":"`+tc.status+`"}`, 0)
			res, err := PollMigration(context.Background(), webapi.NewClient(srv.URL),
				utilsCluster, nqn, "mig-1", past())
			if err != nil {
				t.Fatalf("PollMigration: %v", err)
			}
			if res.Done != tc.wantDone || res.Succeeded != tc.wantSucceeded {
				t.Errorf("done/succeeded = %v/%v, want %v/%v",
					res.Done, res.Succeeded, tc.wantDone, tc.wantSucceeded)
			}
			if res.Migration == nil {
				t.Errorf("Migration must carry the polled DTO")
			}
		})
	}

	// A terminal status wins over a lingering error_message from a retried step.
	t.Run("done with a lingering error message still succeeds", func(t *testing.T) {
		srv := migrationServer(t, `{"id":"mig-1","status":"done","error_message":"transient blip"}`, 0)
		res, err := PollMigration(context.Background(), webapi.NewClient(srv.URL),
			utilsCluster, nqn, "mig-1", past())
		if err != nil {
			t.Fatalf("PollMigration: %v", err)
		}
		if !res.Done || !res.Succeeded {
			t.Errorf("done/succeeded = %v/%v, want true/true", res.Done, res.Succeeded)
		}
	})

	t.Run("in flight past the stuck timeout is flagged", func(t *testing.T) {
		srv := migrationServer(t, `{"id":"mig-1","status":"running"}`, 0)
		res, err := PollMigration(context.Background(), webapi.NewClient(srv.URL),
			utilsCluster, nqn, "mig-1", time.Now().Add(-MigrationStuckWarningTimeout-time.Minute))
		if err != nil {
			t.Fatalf("PollMigration: %v", err)
		}
		if !res.Stuck || res.Done {
			t.Errorf("stuck/done = %v/%v, want true/false", res.Stuck, res.Done)
		}
	})

	t.Run("a terminal migration is never reported stuck", func(t *testing.T) {
		srv := migrationServer(t, `{"id":"mig-1","status":"done"}`, 0)
		res, err := PollMigration(context.Background(), webapi.NewClient(srv.URL),
			utilsCluster, nqn, "mig-1", time.Now().Add(-MigrationStuckWarningTimeout-time.Minute))
		if err != nil {
			t.Fatalf("PollMigration: %v", err)
		}
		if res.Stuck {
			t.Errorf("stuck = true for a completed migration")
		}
	})

	t.Run("API error is propagated", func(t *testing.T) {
		srv := migrationServer(t, `{"detail":"boom"}`, http.StatusInternalServerError)
		if _, err := PollMigration(context.Background(), webapi.NewClient(srv.URL),
			utilsCluster, nqn, "mig-1", past()); err == nil {
			t.Errorf("expected the API error to surface")
		}
	})
}

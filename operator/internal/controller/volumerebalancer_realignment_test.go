package controller

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	realignNamespace   = "sb"
	realignClusterName = "cluster-a"
	realignClusterUUID = "cluster-uuid-a"
)

func boolPtr(b bool) *bool { return &b }

// ---------------------------------------------------------------------------
// resolveDataRealignmentConfig — pure decision table.
// ---------------------------------------------------------------------------

func TestResolveDataRealignmentConfig(t *testing.T) {
	dur := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

	cases := []struct {
		name         string
		vms          *simplyblockv1alpha1.VolumeMigrationSettings
		wantEnabled  bool
		wantInterval time.Duration
	}{
		{
			name:         "nil settings → enabled with default interval",
			vms:          nil,
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
		},
		{
			name:         "settings present but DataRealignment nil → enabled default",
			vms:          &simplyblockv1alpha1.VolumeMigrationSettings{},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
		},
		{
			name:        "volume migration disabled → realignment disabled",
			vms:         &simplyblockv1alpha1.VolumeMigrationSettings{Enabled: boolPtr(false)},
			wantEnabled: false,
		},
		{
			name: "DataRealignment explicitly disabled",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Enabled: boolPtr(false)},
			},
			wantEnabled: false,
		},
		{
			name: "custom interval honored",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Interval: dur(3 * time.Minute)},
			},
			wantEnabled:  true,
			wantInterval: 3 * time.Minute,
		},
		{
			name: "zero interval falls back to default",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Interval: dur(0)},
			},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
		},
		{
			name: "negative interval falls back to default",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Interval: dur(-5 * time.Minute)},
			},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
		},
		{
			name: "DataRealignment Enabled nil defaults to on",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Interval: dur(time.Minute)},
			},
			wantEnabled:  true,
			wantInterval: time.Minute,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &simplyblockv1alpha1.StorageCluster{
				Spec: simplyblockv1alpha1.StorageClusterSpec{VolumeMigrationSettings: tc.vms},
			}
			gotEnabled, gotInterval := resolveDataRealignmentConfig(cr)
			if gotEnabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if tc.wantEnabled && gotInterval != tc.wantInterval {
				t.Fatalf("interval = %v, want %v", gotInterval, tc.wantInterval)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// nextRequeue — realignment interval caps the auto-rebalancing requeue.
// ---------------------------------------------------------------------------

func TestNextRequeue(t *testing.T) {
	now := time.Now()

	// realignRequeue disabled (0) → falls back to eval-based requeue.
	if got := nextRequeue(now, 5*time.Minute, 0); got <= 0 || got > 5*time.Minute {
		t.Fatalf("disabled realign: got %v, want (0, 5m]", got)
	}
	// realignRequeue shorter than eval remaining → wins.
	if got := nextRequeue(now, time.Hour, 2*time.Minute); got != 2*time.Minute {
		t.Fatalf("short realign: got %v, want 2m", got)
	}
	// realignRequeue longer than eval remaining → eval remaining wins.
	if got := nextRequeue(now, 30*time.Second, time.Hour); got > 30*time.Second {
		t.Fatalf("long realign: got %v, want <= 30s", got)
	}
}

// ---------------------------------------------------------------------------
// reconcileDataRealignment — behavior + negative cases.
// ---------------------------------------------------------------------------

// realignFixture wires a reconciler to a counting HTTP stub and a fake k8s client.
type realignFixture struct {
	r        *VolumeRebalancerReconciler
	cl       client.Client
	recorder *events.FakeRecorder
	calls    *int32
}

func newRealignFixture(t *testing.T, status int, cr *simplyblockv1alpha1.StorageCluster) *realignFixture {
	t.Helper()

	var calls int32
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(status)
	})

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha1.StorageCluster{}}, cr)

	rec := events.NewFakeRecorder(64)
	r := &VolumeRebalancerReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  rec,
		apiClient: webapi.NewClient(srv.URL),
	}
	return &realignFixture{r: r, cl: cl, recorder: rec, calls: &calls}
}

// getCluster reloads the cluster from the fake client.
func (f *realignFixture) getCluster(t *testing.T) *simplyblockv1alpha1.StorageCluster {
	t.Helper()
	out := &simplyblockv1alpha1.StorageCluster{}
	if err := f.cl.Get(context.Background(),
		types.NamespacedName{Namespace: realignNamespace, Name: realignClusterName}, out); err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	return out
}

// realignTestCluster builds a StorageCluster with the given pending flag / annotation.
func realignTestCluster(pending *bool, lastAt *metav1.Time, annotate bool, vms *simplyblockv1alpha1.VolumeMigrationSettings) *simplyblockv1alpha1.StorageCluster {
	cr := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: realignClusterName, Namespace: realignNamespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{VolumeMigrationSettings: vms},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID:                   realignClusterUUID,
			PendingDataRealignment: pending,
			LastDataRealignmentAt:  lastAt,
		},
	}
	if annotate {
		cr.Annotations = map[string]string{TriggerRealignmentAnnotation: "1"}
	}
	return cr
}

func TestReconcileDataRealignment_DisabledSkips(t *testing.T) {
	vms := &simplyblockv1alpha1.VolumeMigrationSettings{
		DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Enabled: boolPtr(false)},
	}
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(boolPtr(true), nil, true, vms))

	if got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID); got != 0 {
		t.Fatalf("requeue = %v, want 0 (disabled)", got)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 when disabled", n)
	}
}

func TestReconcileDataRealignment_NothingPendingSkips(t *testing.T) {
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(nil, nil, false, nil))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got != defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want %v (nothing pending)", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 when nothing pending", n)
	}
}

func TestReconcileDataRealignment_PendingWithinIntervalWaits(t *testing.T) {
	recent := metav1.NewTime(time.Now().Add(-time.Minute))
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(boolPtr(true), &recent, false, nil))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got <= 0 || got > defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want remaining interval in (0, %v]", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 within interval", n)
	}
	// Flag must still be pending — no realignment happened.
	if cr := f.getCluster(t); cr.Status.PendingDataRealignment == nil || !*cr.Status.PendingDataRealignment {
		t.Fatalf("pending flag cleared without a realignment")
	}
}

func TestReconcileDataRealignment_PendingNeverRealignedTriggers(t *testing.T) {
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(boolPtr(true), nil, false, nil))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got != defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want %v after success", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1", n)
	}
	cr := f.getCluster(t)
	if cr.Status.PendingDataRealignment == nil || *cr.Status.PendingDataRealignment {
		t.Fatalf("pending flag not reset after successful realignment")
	}
	if cr.Status.LastDataRealignmentAt == nil {
		t.Fatalf("LastDataRealignmentAt not stamped after success")
	}
}

func TestReconcileDataRealignment_PendingIntervalElapsedTriggers(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaultDataRealignmentInterval))
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(boolPtr(true), &old, false, nil))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1 after interval elapsed", n)
	}
}

func TestReconcileDataRealignment_ForcedBypassesInterval(t *testing.T) {
	// Recently realigned AND not pending, but the trigger annotation forces it now.
	recent := metav1.NewTime(time.Now().Add(-time.Second))
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(boolPtr(false), &recent, true, nil))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1 (forced)", n)
	}
	// The one-shot trigger annotation must be consumed.
	if cr := f.getCluster(t); cr.Annotations[TriggerRealignmentAnnotation] != "" {
		t.Fatalf("trigger annotation not removed after forced realignment")
	}
}

func TestReconcileDataRealignment_EmptyAnnotationDoesNotForce(t *testing.T) {
	// An empty-string annotation value is not a trigger: with nothing pending it
	// must behave like no annotation at all (no realignment).
	cr := realignTestCluster(nil, nil, false, nil)
	cr.Annotations = map[string]string{TriggerRealignmentAnnotation: ""}
	f := newRealignFixture(t, http.StatusOK, cr)

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got != defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want %v (empty annotation is not a trigger)", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 for empty annotation value", n)
	}
}

func TestReconcileDataRealignment_APIFailureRetainsFlag(t *testing.T) {
	f := newRealignFixture(t, http.StatusInternalServerError, realignTestCluster(boolPtr(true), nil, false, nil))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got != realignmentRetryDelay {
		t.Fatalf("requeue = %v, want retry delay %v on failure", got, realignmentRetryDelay)
	}
	// The pending flag must NOT be cleared when the realignment call failed.
	cr := f.getCluster(t)
	if cr.Status.PendingDataRealignment == nil || !*cr.Status.PendingDataRealignment {
		t.Fatalf("pending flag cleared despite failed realignment")
	}
	if cr.Status.LastDataRealignmentAt != nil {
		t.Fatalf("LastDataRealignmentAt stamped despite failed realignment")
	}
	assertEvent(t, f.recorder, "DataRealignmentFailed")
}

func TestReconcileDataRealignment_ForcedFailureKeepsAnnotation(t *testing.T) {
	f := newRealignFixture(t, http.StatusBadGateway, realignTestCluster(boolPtr(false), nil, true, nil))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	// A failed forced run must keep the annotation so the trigger is retried.
	if cr := f.getCluster(t); cr.Annotations[TriggerRealignmentAnnotation] == "" {
		t.Fatalf("trigger annotation removed despite failed forced realignment")
	}
}

func assertEvent(t *testing.T, rec *events.FakeRecorder, reason string) {
	t.Helper()
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				return
			}
		default:
			t.Fatalf("expected event containing %q", reason)
		}
	}
}

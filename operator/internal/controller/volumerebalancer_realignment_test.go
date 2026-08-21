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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	realignNamespace   = "sb"
	realignClusterName = "cluster-a"
	realignClusterUUID = "cluster-uuid-a"
)

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
		wantMinMoves int64
	}{
		{
			name:         "nil settings → enabled with default interval",
			vms:          nil,
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
			wantMinMoves: defaultDataRealignmentMinMoves,
		},
		{
			name:         "settings present but DataRealignment nil → enabled default",
			vms:          &simplyblockv1alpha1.VolumeMigrationSettings{},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
			wantMinMoves: defaultDataRealignmentMinMoves,
		},
		{
			name:        "volume migration disabled → realignment disabled",
			vms:         &simplyblockv1alpha1.VolumeMigrationSettings{Enabled: ptr.To(false)},
			wantEnabled: false,
		},
		{
			name: "DataRealignment explicitly disabled",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Enabled: ptr.To(false)},
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
			wantMinMoves: defaultDataRealignmentMinMoves,
		},
		{
			name: "zero interval falls back to default",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Interval: dur(0)},
			},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
			wantMinMoves: defaultDataRealignmentMinMoves,
		},
		{
			name: "negative interval falls back to default",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Interval: dur(-5 * time.Minute)},
			},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
			wantMinMoves: defaultDataRealignmentMinMoves,
		},
		{
			name: "DataRealignment Enabled nil defaults to on",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Interval: dur(time.Minute)},
			},
			wantEnabled:  true,
			wantInterval: time.Minute,
			wantMinMoves: defaultDataRealignmentMinMoves,
		},
		{
			name: "custom minMoves honored",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{MinMoves: ptr.To(int32(10))},
			},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
			wantMinMoves: 10,
		},
		{
			// Zero would mean "realign when nothing has moved", which is not a
			// meaningful request; fall back rather than spin.
			name: "zero minMoves falls back to default",
			vms: &simplyblockv1alpha1.VolumeMigrationSettings{
				DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{MinMoves: ptr.To(int32(0))},
			},
			wantEnabled:  true,
			wantInterval: defaultDataRealignmentInterval,
			wantMinMoves: defaultDataRealignmentMinMoves,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &simplyblockv1alpha1.StorageCluster{
				Spec: simplyblockv1alpha1.StorageClusterSpec{VolumeMigrationSettings: tc.vms},
			}
			gotEnabled, gotInterval, gotMinMoves := resolveDataRealignmentConfig(cr)
			if gotEnabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if tc.wantEnabled && gotInterval != tc.wantInterval {
				t.Fatalf("interval = %v, want %v", gotInterval, tc.wantInterval)
			}
			if tc.wantEnabled && gotMinMoves != tc.wantMinMoves {
				t.Fatalf("minMoves = %d, want %d", gotMinMoves, tc.wantMinMoves)
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

// realignTestCluster builds a StorageCluster with `generation` volume moves recorded and
// `realigned` of them already covered — so generation-realigned is what is outstanding.
func realignTestCluster(
	generation, realigned int64,
	lastAt *metav1.Time,
	annotate bool,
	vms *simplyblockv1alpha1.VolumeMigrationSettings,
) *simplyblockv1alpha1.StorageCluster {
	cr := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: realignClusterName, Namespace: realignNamespace},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{VolumeMigrationSettings: vms},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID:                  realignClusterUUID,
			VolumeMoveGeneration:  ptr.To(generation),
			RealignedGeneration:   ptr.To(realigned),
			LastDataRealignmentAt: lastAt,
		},
	}
	if annotate {
		cr.Annotations = map[string]string{simplyblockv1alpha1.TriggerRealignmentAnnotation: "1"}
	}
	return cr
}

func TestReconcileDataRealignment_DisabledSkips(t *testing.T) {
	vms := &simplyblockv1alpha1.VolumeMigrationSettings{
		DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{Enabled: ptr.To(false)},
	}
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(1, 0, nil, true, vms))

	if got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID); got != 0 {
		t.Fatalf("requeue = %v, want 0 (disabled)", got)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 when disabled", n)
	}
}

// TestReconcile_RealignmentRunsWhenAutoRebalancingDisabled locks in that data
// realignment is driven by the full Reconcile independently of auto-rebalancing.
// Auto-rebalancing is configured separately via Spec.VolumeAutoPlacement; whether
// that is unset or explicitly disabled, a due (here: forced) realignment still
// fires and the returned requeue keeps it on schedule. Realignment itself is
// enabled by default (nil VolumeMigrationSettings).
func TestReconcile_RealignmentRunsWhenAutoRebalancingDisabled(t *testing.T) {
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: realignNamespace, Name: realignClusterName}}

	cases := []struct {
		name          string
		autoPlacement *simplyblockv1alpha1.VolumeAutoPlacementSettings
	}{
		{
			name:          "auto-placement unset",
			autoPlacement: nil,
		},
		{
			name:          "auto-rebalancing explicitly disabled",
			autoPlacement: &simplyblockv1alpha1.VolumeAutoPlacementSettings{Enabled: ptr.To(false)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// annotate=true forces an immediate realignment regardless of interval;
			// nil VolumeMigrationSettings leaves realignment enabled by default.
			cr := realignTestCluster(1, 0, nil, true, nil)
			cr.Spec.VolumeAutoPlacement = tc.autoPlacement
			f := newRealignFixture(t, http.StatusOK, cr)

			res, err := f.r.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if n := atomic.LoadInt32(f.calls); n != 1 {
				t.Fatalf("realignment API calls = %d, want 1 (realignment must run with auto-rebalancing off)", n)
			}
			if res.RequeueAfter != defaultDataRealignmentInterval {
				t.Fatalf("RequeueAfter = %v, want %v (realignment cadence preserved)", res.RequeueAfter, defaultDataRealignmentInterval)
			}
		})
	}
}

func TestReconcileDataRealignment_NothingPendingSkips(t *testing.T) {
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(0, 0, nil, false, nil))

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
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(1, 0, &recent, false, nil))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got <= 0 || got > defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want remaining interval in (0, %v]", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 within interval", n)
	}
	// The move must still be outstanding — no realignment happened.
	if cr := f.getCluster(t); ptr.Int64FromOrZero(cr.Status.RealignedGeneration) != 0 {
		t.Fatalf("realignedGeneration advanced without a realignment")
	}
}

func TestReconcileDataRealignment_PendingNeverRealignedTriggers(t *testing.T) {
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(1, 0, nil, false, nil))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got != defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want %v after success", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1", n)
	}
	cr := f.getCluster(t)
	if got := ptr.Int64FromOrZero(cr.Status.RealignedGeneration); got != 1 {
		t.Fatalf("realignedGeneration = %d, want 1 after a successful realignment", got)
	}
	if cr.Status.LastDataRealignmentAt == nil {
		t.Fatalf("LastDataRealignmentAt not stamped after success")
	}
}

func TestReconcileDataRealignment_PendingIntervalElapsedTriggers(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaultDataRealignmentInterval))
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(1, 0, &old, false, nil))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1 after interval elapsed", n)
	}
}

func TestReconcileDataRealignment_ForcedBypassesInterval(t *testing.T) {
	// Recently realigned AND not pending, but the trigger annotation forces it now.
	recent := metav1.NewTime(time.Now().Add(-time.Second))
	f := newRealignFixture(t, http.StatusOK, realignTestCluster(0, 0, &recent, true, nil))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1 (forced)", n)
	}
	// The one-shot trigger annotation must be consumed.
	if cr := f.getCluster(t); cr.Annotations[simplyblockv1alpha1.TriggerRealignmentAnnotation] != "" {
		t.Fatalf("trigger annotation not removed after forced realignment")
	}
}

func TestReconcileDataRealignment_EmptyAnnotationDoesNotForce(t *testing.T) {
	// An empty-string annotation value is not a trigger: with nothing pending it
	// must behave like no annotation at all (no realignment).
	cr := realignTestCluster(0, 0, nil, false, nil)
	cr.Annotations = map[string]string{simplyblockv1alpha1.TriggerRealignmentAnnotation: ""}
	f := newRealignFixture(t, http.StatusOK, cr)

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got != defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want %v (empty annotation is not a trigger)", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 for empty annotation value", n)
	}
}

func TestReconcileDataRealignment_APIFailureLeavesTheMoveOutstanding(t *testing.T) {
	f := newRealignFixture(t, http.StatusInternalServerError, realignTestCluster(1, 0, nil, false, nil))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if got != realignmentRetryDelay {
		t.Fatalf("requeue = %v, want retry delay %v on failure", got, realignmentRetryDelay)
	}
	// The move must NOT be recorded as covered when the realignment call failed.
	cr := f.getCluster(t)
	if got := ptr.Int64FromOrZero(cr.Status.RealignedGeneration); got != 0 {
		t.Fatalf("realignedGeneration = %d, want 0 despite a failed realignment", got)
	}
	if cr.Status.LastDataRealignmentAt != nil {
		t.Fatalf("LastDataRealignmentAt stamped despite failed realignment")
	}
	assertEvent(t, f.recorder, "DataRealignmentFailed")
}

func TestReconcileDataRealignment_ForcedFailureKeepsAnnotation(t *testing.T) {
	f := newRealignFixture(t, http.StatusBadGateway, realignTestCluster(0, 0, nil, true, nil))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	// A failed forced run must keep the annotation so the trigger is retried.
	if cr := f.getCluster(t); cr.Annotations[simplyblockv1alpha1.TriggerRealignmentAnnotation] == "" {
		t.Fatalf("trigger annotation removed despite failed forced realignment")
	}
}

func assertEvent(t *testing.T, rec *events.FakeRecorder, reason string) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				return
			}
		case <-timeout:
			t.Fatalf("expected event containing %q", reason)
		}
	}
}

// ---------------------------------------------------------------------------
// Generation accounting, the in-flight gate, and MinMoves batching.
//
// These three exist because of one observed sequence (run fio-mig-1787257731). A
// realignment was requested at 02:13:31 while mig-67 was still moving data; mig-67
// completed at 02:13:41, ten seconds later, re-arming the pending flag; and at 02:23:32
// — the next interval tick — the operator sent a second realignment request to a cluster
// that was still rebalancing from the first. The cases below pin each link.
// ---------------------------------------------------------------------------

// movingMigration is a VolumeMigration the control plane has accepted and not finished.
func movingMigration(name string, phase simplyblockv1alpha1.VolumeMigrationPhase) *simplyblockv1alpha1.VolumeMigration {
	return &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: realignNamespace},
		Status: simplyblockv1alpha1.VolumeMigrationStatus{
			Phase:         phase,
			MigrationUUID: "migration-uuid-" + name,
			ClusterUUID:   realignClusterUUID,
		},
	}
}

// newRealignFixtureWith wires the fixture with extra objects (VolumeMigrations) in the
// fake client, so the in-flight lookup has something to find.
// The realignment call always succeeds here; the failure paths are covered through
// newRealignFixture, which takes a status.
func newRealignFixtureWith(
	t *testing.T,
	cr *simplyblockv1alpha1.StorageCluster,
	extra ...client.Object,
) *realignFixture {
	t.Helper()

	var calls int32
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	})

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	objs := append([]client.Object{cr}, extra...)
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha1.StorageCluster{}}, objs...)

	rec := events.NewFakeRecorder(64)
	r := &VolumeRebalancerReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  rec,
		apiClient: webapi.NewClient(srv.URL),
	}
	return &realignFixture{r: r, cl: cl, recorder: rec, calls: &calls}
}

// The 02:13:31 link: a realignment that is otherwise due must not be requested while a
// volume is still moving.
func TestReconcileDataRealignment_DefersWhileVolumeIsMoving(t *testing.T) {
	for _, phase := range []simplyblockv1alpha1.VolumeMigrationPhase{
		simplyblockv1alpha1.VolumeMigrationPhaseValidating,
		simplyblockv1alpha1.VolumeMigrationPhaseRunning,
	} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRealignFixtureWith(t,
				realignTestCluster(1, 0, nil, false, nil), movingMigration("mig-67", phase))

			got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
			if got != realignmentBusyRetryDelay {
				t.Fatalf("requeue = %v, want busy retry %v", got, realignmentBusyRetryDelay)
			}
			if n := atomic.LoadInt32(f.calls); n != 0 {
				t.Fatalf("API called %d times, want 0 while a volume is moving", n)
			}
			// Nothing recorded, so the realignment stays owed.
			if cr := f.getCluster(t); ptr.Int64FromOrZero(cr.Status.RealignedGeneration) != 0 {
				t.Fatalf("realignedGeneration advanced without a realignment")
			}
		})
	}
}

// A migration the control plane refused — which is exactly what happens while a
// realignment runs, since it rejects migrations then — is not moving data and must not
// hold realignment off, or the two would deadlock each other.
func TestReconcileDataRealignment_UnacceptedMigrationDoesNotDefer(t *testing.T) {
	deferring := movingMigration("mig-68", simplyblockv1alpha1.VolumeMigrationPhasePending)
	deferring.Status.MigrationUUID = "" // never accepted

	f := newRealignFixtureWith(t, realignTestCluster(1, 0, nil, false, nil), deferring)

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1 (a refused migration is not moving data)", n)
	}
}

func TestReconcileDataRealignment_TerminalAndForeignMigrationsDoNotDefer(t *testing.T) {
	done := movingMigration("mig-66", simplyblockv1alpha1.VolumeMigrationPhaseCompleted)
	failed := movingMigration("mig-36", simplyblockv1alpha1.VolumeMigrationPhaseFailed)
	aborted := movingMigration("mig-26", simplyblockv1alpha1.VolumeMigrationPhaseAborted)
	foreign := movingMigration("other-cluster", simplyblockv1alpha1.VolumeMigrationPhaseRunning)
	foreign.Status.ClusterUUID = "some-other-cluster-uuid"

	f := newRealignFixtureWith(t,
		realignTestCluster(1, 0, nil, false, nil), done, failed, aborted, foreign)

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1", n)
	}
}

// An explicit trigger (drain, node removal) must still run immediately: deferring it
// could leave a removed node's data unaligned.
func TestReconcileDataRealignment_ForcedIgnoresMovingVolumes(t *testing.T) {
	cr := realignTestCluster(0, 0, nil, true, nil)
	f := newRealignFixtureWith(t, cr,
		movingMigration("mig-67", simplyblockv1alpha1.VolumeMigrationPhaseRunning))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1 (forced bypasses the in-flight gate)", n)
	}
}

// The 02:23:32 link. A move that lands after the request went out must leave a
// realignment owed rather than being absorbed by the one already running — the request
// covered the generation as read *before* the call.
func TestReconcileDataRealignment_RecordsOnlyTheGenerationItCovered(t *testing.T) {
	f := newRealignFixtureWith(t, realignTestCluster(3, 0, nil, false, nil))

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	cr := f.getCluster(t)
	if got := ptr.Int64FromOrZero(cr.Status.RealignedGeneration); got != 3 {
		t.Fatalf("realignedGeneration = %d, want 3 (the value read before the call)", got)
	}

	// mig-67 finishes ten seconds later.
	vmr := &VolumeMigrationReconciler{Client: f.cl, Scheme: f.r.Scheme, Recorder: events.NewFakeRecorder(8)}
	vmr.markClusterVolumeMoved(context.Background(), realignNamespace, realignClusterUUID)

	cr = f.getCluster(t)
	gen := ptr.Int64FromOrZero(cr.Status.VolumeMoveGeneration)
	realigned := ptr.Int64FromOrZero(cr.Status.RealignedGeneration)
	if gen != 4 || realigned != 3 {
		t.Fatalf("generation/realigned = %d/%d, want 4/3", gen, realigned)
	}
	if gen <= realigned {
		t.Fatalf("the late move was swallowed; a realignment must still be owed")
	}
}

// ...and that owed realignment must wait for the cluster to go quiet rather than being
// sent on top of the one still running. This is the whole bug in one test.
func TestReconcileDataRealignment_LateMoveDoesNotStackOnRunningRealignment(t *testing.T) {
	// Interval already elapsed, one move owed, and a volume still moving — the exact
	// 02:23:32 state.
	old := metav1.NewTime(time.Now().Add(-2 * defaultDataRealignmentInterval))
	f := newRealignFixtureWith(t, realignTestCluster(4, 3, &old, false, nil),
		movingMigration("mig-69", simplyblockv1alpha1.VolumeMigrationPhaseRunning))

	got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times, want 0 — this is the duplicate request", n)
	}
	if got != realignmentBusyRetryDelay {
		t.Fatalf("requeue = %v, want busy retry %v", got, realignmentBusyRetryDelay)
	}
}

func TestReconcileDataRealignment_MinMovesBatches(t *testing.T) {
	vms := &simplyblockv1alpha1.VolumeMigrationSettings{
		DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{MinMoves: ptr.To(int32(5))},
	}

	// Four moves owed: below the threshold, so no realignment and no blocked migrations.
	f := newRealignFixtureWith(t, realignTestCluster(4, 0, nil, false, vms))
	if got := f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID); got != defaultDataRealignmentInterval {
		t.Fatalf("requeue = %v, want %v below threshold", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("API called %d times with 4 of 5 moves owed, want 0", n)
	}

	// The fifth reaches it.
	f = newRealignFixtureWith(t, realignTestCluster(5, 0, nil, false, vms))
	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times at the threshold, want 1", n)
	}
	if got := ptr.Int64FromOrZero(f.getCluster(t).Status.RealignedGeneration); got != 5 {
		t.Fatalf("realignedGeneration = %d, want 5 (all five moves accounted for)", got)
	}
}

// MinMoves must not hold off an explicit trigger: a drain still realigns on one move.
func TestReconcileDataRealignment_ForcedIgnoresMinMoves(t *testing.T) {
	vms := &simplyblockv1alpha1.VolumeMigrationSettings{
		DataRealignment: &simplyblockv1alpha1.DataRealignmentSettings{MinMoves: ptr.To(int32(50))},
	}
	cr := realignTestCluster(1, 0, nil, true, vms)
	cr.Status.VolumeMoveGeneration = ptr.To(int64(1))
	f := newRealignFixtureWith(t, cr)

	f.r.reconcileDataRealignment(context.Background(), f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("API called %d times, want 1 (forced bypasses MinMoves)", n)
	}
}

// A replay of the observed sequence, end to end, as one test: a realignment goes out
// while a volume is moving, the move lands just after, and the next interval tick must
// not stack a second request on the one still running — but must send it once the
// cluster is quiet. Exactly two requests, in the right places.
func TestReconcileDataRealignment_ReplayOfStackedRealignment(t *testing.T) {
	ctx := context.Background()
	// mig-67 is moving; one earlier move is already owed.
	f := newRealignFixtureWith(t, realignTestCluster(1, 0, nil, false, nil),
		movingMigration("mig-67", simplyblockv1alpha1.VolumeMigrationPhaseRunning))
	vmr := &VolumeMigrationReconciler{Client: f.cl, Scheme: f.r.Scheme, Recorder: events.NewFakeRecorder(8)}

	// 02:13:31 — due, but a volume is moving: deferred instead of sent.
	f.r.reconcileDataRealignment(ctx, f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 0 {
		t.Fatalf("after tick 1: %d call(s), want 0 while mig-67 moves", n)
	}

	// 02:13:41 — mig-67 completes: counter goes to 2, migration reaches a terminal phase.
	vmr.markClusterVolumeMoved(ctx, realignNamespace, realignClusterUUID)
	done := &simplyblockv1alpha1.VolumeMigration{}
	if err := f.cl.Get(ctx, types.NamespacedName{Namespace: realignNamespace, Name: "mig-67"}, done); err != nil {
		t.Fatalf("get mig-67: %v", err)
	}
	done.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseCompleted
	// Plain Update: the fixture registers the status subresource for StorageCluster
	// only, so VolumeMigration status travels with the object.
	if err := f.cl.Update(ctx, done); err != nil {
		t.Fatalf("complete mig-67: %v", err)
	}

	// Next tick: quiet now, so the realignment goes out covering both moves.
	f.r.reconcileDataRealignment(ctx, f.getCluster(t), realignClusterUUID)
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("after tick 2: %d call(s), want 1", n)
	}
	cr := f.getCluster(t)
	if got := ptr.Int64FromOrZero(cr.Status.RealignedGeneration); got != 2 {
		t.Fatalf("realignedGeneration = %d, want 2 (both moves covered)", got)
	}

	// 02:23:32 — the interval has elapsed and nothing further moved. Under the old
	// boolean this is where the duplicate request went out.
	cr.Status.LastDataRealignmentAt = &metav1.Time{Time: time.Now().Add(-2 * defaultDataRealignmentInterval)}
	if err := f.cl.Status().Update(ctx, cr); err != nil {
		t.Fatalf("age the realignment stamp: %v", err)
	}
	if got := f.r.reconcileDataRealignment(ctx, f.getCluster(t), realignClusterUUID); got != defaultDataRealignmentInterval {
		t.Fatalf("tick 3 requeue = %v, want %v", got, defaultDataRealignmentInterval)
	}
	if n := atomic.LoadInt32(f.calls); n != 1 {
		t.Fatalf("after tick 3: %d call(s), want 1 — nothing moved, so nothing to realign", n)
	}
}

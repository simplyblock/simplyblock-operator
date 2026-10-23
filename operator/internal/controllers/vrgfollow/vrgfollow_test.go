package vrgfollow

// The operator, not a pod's stage, moves a stretched cluster's volumes, and it
// moves them when Ramen makes a VolumeReplicationGroup primary on this site
// (design-stretched-sync-replication-ramen.md §9).

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/link/sitev1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const (
	testNamespace = "simplyblock"
	appNamespace  = "app"
	thisSite      = "site-b"
	clusterUUID   = "7416b334-72c7-42cf-9630-41686c8d83d3"
	otherUUID     = "11111111-1111-1111-1111-111111111111"
	pool          = "9137ade3-4e5d-492a-9fd8-a7261ba5116c"
	vrgName       = "app"
)

func handle(cluster, volume string) string { return cluster + ":" + pool + ":" + volume }

// fakeMover answers every Move with state, or with refused for the volumes in it.
type fakeMover struct {
	state  sitev1.MoveResponse_State
	refuse map[string]*sitev1.MoveResponse
	asked  []*sitev1.MoveRequest
}

func (f *fakeMover) Move(_ context.Context, req *sitev1.MoveRequest) (*sitev1.MoveResponse, error) {
	f.asked = append(f.asked, req)
	if r, ok := f.refuse[req.GetVolume()]; ok {
		return r, nil
	}
	return &sitev1.MoveResponse{State: f.state, LedFrom: req.GetToSite()}, nil
}

func (f *fakeMover) movedVolumes() []string {
	out := make([]string, 0, len(f.asked))
	for _, r := range f.asked {
		out = append(out, r.GetVolume())
	}
	return out
}

func stretchedCluster() *simplyblockv1alpha2.StorageCluster {
	c := testsupport.Cluster(testNamespace, "stretched", clusterUUID)
	c.Spec.Stretched = &simplyblockv1alpha2.StretchedSpec{Site: thisSite, PeerSite: "site-a"}
	return c
}

// vrg is a Ramen VolumeReplicationGroup in mode sync or async.
func vrg(mode, state, action string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VRGKind)
	u.SetNamespace(appNamespace)
	u.SetName(vrgName)
	spec := map[string]any{
		"replicationState": state,
		"pvcSelector":      map[string]any{"matchLabels": map[string]any{"app": "app"}},
		mode:               map[string]any{},
	}
	if action != "" {
		spec["action"] = action
	}
	u.Object["spec"] = spec
	return u
}

// claim is a PVC labeled for the VRG, bound to a PV of driver carrying handle h.
func claim(name, driver, h string, bound bool) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNamespace, Labels: map[string]string{"app": "app"}},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-" + name},
	}
	if bound {
		pvc.Status.Phase = corev1.ClaimBound
	} else {
		pvc.Status.Phase = corev1.ClaimPending
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: h},
		}},
	}
	return pvc, pv
}

func newReconciler(t *testing.T, mover *fakeMover, objs ...client.Object) (*Reconciler, *events.FakeRecorder) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, func(s *runtime.Scheme) error {
		s.AddKnownTypeWithName(VRGKind, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(VRGKind.GroupVersion().WithKind(VRGKind.Kind+"List"), &unstructured.UnstructuredList{})
		return nil
	})
	recorder := events.NewFakeRecorder(16)
	return &Reconciler{
		Client:    testsupport.NewClient(t, scheme, nil, objs...),
		Recorder:  recorder,
		Mover:     mover,
		Namespace: testNamespace,
	}, recorder
}

func reconcileOnce(t *testing.T, r *Reconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: appNamespace, Name: vrgName,
	}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		case <-time.After(10 * time.Millisecond):
			return out
		}
	}
}

// A sync VRG Ramen made primary on this site moves every volume it protects
// here, planned, and settles once all are moved.
func TestAPrimaryVRGMovesItsVolumesToThisSite(t *testing.T) {
	pvc1, pv1 := claim("one", csiDriver, handle(clusterUUID, "v1"), true)
	pvc2, pv2 := claim("two", csiDriver, handle(clusterUUID, "v2"), true)
	mover := &fakeMover{state: sitev1.MoveResponse_STATE_MOVED}
	r, _ := newReconciler(t, mover, stretchedCluster(), vrg("sync", "primary", "Relocate"), pvc1, pv1, pvc2, pv2)

	res := reconcileOnce(t, r)
	if res.RequeueAfter != 0 {
		t.Errorf("requeue %v after every volume moved", res.RequeueAfter)
	}
	got := mover.movedVolumes()
	if len(got) != 2 || !testsupport.Contains(got, handle(clusterUUID, "v1")) || !testsupport.Contains(got, handle(clusterUUID, "v2")) {
		t.Fatalf("moved %v, want both volumes", got)
	}
	for _, req := range mover.asked {
		if req.GetToSite() != thisSite || req.GetAllowForced() {
			t.Errorf("move %+v; want to %s, planned", req, thisSite)
		}
	}
}

// A failover is the one move allowed to escalate to a forced promote.
func TestAFailoverVRGAllowsAForcedPromote(t *testing.T) {
	pvc, pv := claim("one", csiDriver, handle(clusterUUID, "v1"), true)
	mover := &fakeMover{state: sitev1.MoveResponse_STATE_MOVED}
	r, _ := newReconciler(t, mover, stretchedCluster(), vrg("sync", "primary", "Failover"), pvc, pv)

	reconcileOnce(t, r)
	if len(mover.asked) != 1 || !mover.asked[0].GetAllowForced() {
		t.Errorf("moves %v; a failover must allow a forced promote", mover.asked)
	}
}

// A move still running is asked again later, not waited on.
func TestAMoveInProgressIsAskedAgain(t *testing.T) {
	pvc, pv := claim("one", csiDriver, handle(clusterUUID, "v1"), true)
	mover := &fakeMover{state: sitev1.MoveResponse_STATE_IN_PROGRESS}
	r, _ := newReconciler(t, mover, stretchedCluster(), vrg("sync", "primary", "Relocate"), pvc, pv)

	if res := reconcileOnce(t, r); res.RequeueAfter <= 0 {
		t.Error("a move in progress was not requeued")
	}
}

// A refused move says why on the VRG and is asked again later.
func TestARefusedMoveIsReportedOnTheVRG(t *testing.T) {
	pvc, pv := claim("one", csiDriver, handle(clusterUUID, "v1"), true)
	mover := &fakeMover{refuse: map[string]*sitev1.MoveResponse{handle(clusterUUID, "v1"): {
		State: sitev1.MoveResponse_STATE_REFUSED, Reason: "volumes still served", BlockingVolumes: []string{"v9"},
	}}}
	r, rec := newReconciler(t, mover, stretchedCluster(), vrg("sync", "primary", "Relocate"), pvc, pv)

	if res := reconcileOnce(t, r); res.RequeueAfter <= 0 {
		t.Error("a refused move was not requeued")
	}
	evs := drainEvents(rec)
	if len(evs) != 1 || !strings.Contains(evs[0], "SyncMoveRefused") || !strings.Contains(evs[0], "v9") {
		t.Errorf("events %v; want one SyncMoveRefused naming the blocking volume", evs)
	}
}

// Nothing moves for a VRG that is not primary, an async VRG, or a cluster that
// is not stretched.
func TestOnlyASyncPrimaryVRGOnAStretchedClusterMovesAnything(t *testing.T) {
	pvc, pv := claim("one", csiDriver, handle(clusterUUID, "v1"), true)
	for name, objs := range map[string][]client.Object{
		"secondary":     {stretchedCluster(), vrg("sync", "secondary", "Relocate"), pvc, pv},
		"async":         {stretchedCluster(), vrg("async", "primary", ""), pvc, pv},
		"not stretched": {testsupport.Cluster(testNamespace, "plain", clusterUUID), vrg("sync", "primary", ""), pvc, pv},
	} {
		t.Run(name, func(t *testing.T) {
			mover := &fakeMover{state: sitev1.MoveResponse_STATE_MOVED}
			r, _ := newReconciler(t, mover, objs...)
			if res := reconcileOnce(t, r); res.RequeueAfter != 0 {
				t.Errorf("requeued %v", res.RequeueAfter)
			}
			if len(mover.asked) != 0 {
				t.Errorf("moved %v", mover.movedVolumes())
			}
		})
	}
}

// Only bound claims of this driver and this cluster are moved. A claim Ramen
// has not restored and bound yet is waited for.
func TestOnlyThisClustersBoundVolumesAreMoved(t *testing.T) {
	ours, oursPV := claim("ours", csiDriver, handle(clusterUUID, "v1"), true)
	other, otherPV := claim("other-driver", "rbd.csi.ceph.com", "0001-abc", true)
	foreign, foreignPV := claim("other-cluster", csiDriver, handle(otherUUID, "v2"), true)
	pending, pendingPV := claim("pending", csiDriver, handle(clusterUUID, "v3"), false)
	mover := &fakeMover{state: sitev1.MoveResponse_STATE_MOVED}
	r, _ := newReconciler(t, mover, stretchedCluster(), vrg("sync", "primary", "Relocate"),
		ours, oursPV, other, otherPV, foreign, foreignPV, pending, pendingPV)

	res := reconcileOnce(t, r)
	if got := mover.movedVolumes(); len(got) != 1 || got[0] != handle(clusterUUID, "v1") {
		t.Errorf("moved %v, want only this cluster's bound volume", got)
	}
	if res.RequeueAfter <= 0 {
		t.Error("a claim not yet bound was not waited for")
	}
}

// A primary VRG whose claims Ramen has not restored yet is waited for.
func TestAPrimaryVRGWithNoClaimsYetIsWaitedFor(t *testing.T) {
	mover := &fakeMover{state: sitev1.MoveResponse_STATE_MOVED}
	r, _ := newReconciler(t, mover, stretchedCluster(), vrg("sync", "primary", "Relocate"))

	if res := reconcileOnce(t, r); res.RequeueAfter <= 0 {
		t.Error("a VRG with no claims restored yet was not requeued")
	}
}

// A VRG that is gone is not an error.
func TestAGoneVRGIsNotAnError(t *testing.T) {
	mover := &fakeMover{state: sitev1.MoveResponse_STATE_MOVED}
	r, _ := newReconciler(t, mover, stretchedCluster())
	if res := reconcileOnce(t, r); res.RequeueAfter != 0 || len(mover.asked) != 0 {
		t.Errorf("res %v, moves %v", res, mover.asked)
	}
}

// Regression: 2026-10-23-vrg-own-volumes-refusal-warning: the volumes of one VRG
// share a store, and they are moved one at a time, so the first promote is
// refused while the VRG's other volumes are still active on the old site. That
// is ordering, not a problem: it resolves on the next pass, and it was reported
// as a SyncMoveRefused warning on every relocate (live 2026-10-23, 09-relocate.sh).
func TestARefusalBlockedOnlyByTheVRGsOwnVolumesIsNotReported(t *testing.T) {
	pvc1, pv1 := claim("one", csiDriver, handle(clusterUUID, "v1"), true)
	pvc2, pv2 := claim("two", csiDriver, handle(clusterUUID, "v2"), true)
	mover := &fakeMover{state: sitev1.MoveResponse_STATE_MOVED, refuse: map[string]*sitev1.MoveResponse{
		handle(clusterUUID, "v1"): {State: sitev1.MoveResponse_STATE_REFUSED, LedFrom: "site-a",
			Reason: "other volumes of the LVS are still active on site site-a", BlockingVolumes: []string{"v2"}},
	}}
	r, rec := newReconciler(t, mover, stretchedCluster(), vrg("sync", "primary", "Relocate"), pvc1, pv1, pvc2, pv2)

	if res := reconcileOnce(t, r); res.RequeueAfter <= 0 {
		t.Error("a move refused for ordering was not asked again")
	}
	if evs := drainEvents(rec); len(evs) != 0 {
		t.Errorf("events %v; a refusal blocked only by the VRG's own volumes is not a problem to report", evs)
	}
}

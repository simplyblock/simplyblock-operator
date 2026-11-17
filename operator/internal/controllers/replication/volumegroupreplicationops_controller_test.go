// Unit tests for the VolumeGroupReplicationOps Fence action
// (design-volumegroupreplicationops.md §5, §6; test plan U-05 … U-22): resolving
// the group from the VolumeGroupReplication, ordering the fence, the retryable and
// permanent answers, the step deadline, and the member report, against a fake
// client and a fake control plane.
package replication

import (
	"context"
	"errors"
	"net/http"
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

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const (
	appNamespace = "gitea"
	vgrName      = "gitea"
	contentName  = "vgrcontent-gitea"
	clusterID    = "aaaaaaaa-0000-0000-0000-000000000001"
	groupID      = "cccccccc-0000-0000-0000-000000000003"
	groupHandle  = "cg:" + clusterID + ":" + groupID
)

// fakeControlPlane records the fences it was asked for and answers with err.
type fakeControlPlane struct {
	fenced     []lvol.GroupHandle
	err        error
	members    []string
	membersErr error
}

func (f *fakeControlPlane) FenceGroup(_ context.Context, gh lvol.GroupHandle) error {
	f.fenced = append(f.fenced, gh)
	return f.err
}

func (f *fakeControlPlane) ConsistencyGroupMembers(_ context.Context, _ lvol.GroupHandle) ([]string, error) {
	return f.members, f.membersErr
}

func vgr(content string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VolumeGroupReplicationKind)
	u.SetNamespace(appNamespace)
	u.SetName(vgrName)
	spec := map[string]any{"volumeGroupReplicationClassName": "simplyblock"}
	if content != "" {
		spec["volumeGroupReplicationContentName"] = content
	}
	u.Object["spec"] = spec
	return u
}

func vgrContent(handle string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VolumeGroupReplicationContentKind)
	u.SetName(contentName)
	u.Object["spec"] = map[string]any{"volumeGroupReplicationHandle": handle}
	return u
}

func fenceOps() *simplyblockv1alpha2.VolumeGroupReplicationOps {
	return &simplyblockv1alpha2.VolumeGroupReplicationOps{
		ObjectMeta: metav1.ObjectMeta{Name: "fence-gitea", Namespace: appNamespace, Generation: 1},
		Spec: simplyblockv1alpha2.VolumeGroupReplicationOpsSpec{
			VolumeGroupReplicationRef: vgrName,
			Action:                    simplyblockv1alpha2.VolumeGroupReplicationOpsActionFence,
		},
	}
}

// csiAddonsServed registers the two csi-addons kinds, as a site with the DR stack serves them.
func csiAddonsServed(s *runtime.Scheme) error {
	s.AddKnownTypeWithName(VolumeGroupReplicationKind, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(VolumeGroupReplicationContentKind, &unstructured.Unstructured{})
	return nil
}

type fixture struct {
	r     *VolumeGroupReplicationOpsReconciler
	cl    client.Client
	cp    *fakeControlPlane
	rec   *events.FakeRecorder
	clock time.Time
}

func newFixture(t *testing.T, served bool, objs ...client.Object) *fixture {
	t.Helper()
	adds := []func(*runtime.Scheme) error{simplyblockv1alpha2.AddToScheme, corev1.AddToScheme}
	if served {
		adds = append(adds, csiAddonsServed)
	}
	scheme := testsupport.NewScheme(t, adds...)
	cl := testsupport.NewClient(t, scheme, []client.Object{&simplyblockv1alpha2.VolumeGroupReplicationOps{}}, objs...)
	f := &fixture{
		cl:    cl,
		cp:    &fakeControlPlane{members: []string{"vol-1", "vol-2"}},
		rec:   events.NewFakeRecorder(32),
		clock: time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC),
	}
	f.r = &VolumeGroupReplicationOpsReconciler{
		Client:       cl,
		APIReader:    cl,
		Recorder:     f.rec,
		ControlPlane: func(context.Context) (GroupFencer, error) { return f.cp, nil },
		Now:          func() time.Time { return f.clock },
	}
	return f
}

func (f *fixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := f.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "fence-gitea", Namespace: appNamespace}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// settle reconciles until the operation stops moving.
func (f *fixture) settle(t *testing.T) *simplyblockv1alpha2.VolumeGroupReplicationOps {
	t.Helper()
	for range 10 {
		f.reconcile(t)
	}
	return f.ops(t)
}

func (f *fixture) ops(t *testing.T) *simplyblockv1alpha2.VolumeGroupReplicationOps {
	t.Helper()
	ops := &simplyblockv1alpha2.VolumeGroupReplicationOps{}
	if err := f.cl.Get(context.Background(), types.NamespacedName{Name: "fence-gitea", Namespace: appNamespace}, ops); err != nil {
		t.Fatalf("get ops: %v", err)
	}
	return ops
}

func (f *fixture) events() string {
	var out []string
	for {
		select {
		case e := <-f.rec.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// U-05, U-11, U-20: the group resolves, the fence is ordered once, the members are reported.
func TestFence_ResolvesTheGroupAndFencesIt(t *testing.T) {
	f := newFixture(t, true, fenceOps(), vgr(contentName), vgrContent(groupHandle))

	ops := f.settle(t)

	if ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded ||
		ops.Status.Step.State != simplyblockv1alpha2.VolumeGroupReplicationOpsStepCompleted {
		t.Fatalf("phase/step = %s/%s (%s), want Succeeded/Completed", ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if ops.Status.GroupHandle != groupHandle {
		t.Errorf("groupHandle = %q, want %q", ops.Status.GroupHandle, groupHandle)
	}
	want := lvol.GroupHandle{ClusterID: clusterID, GroupID: groupID}
	if len(f.cp.fenced) != 1 || f.cp.fenced[0] != want {
		t.Errorf("fences ordered = %v, want exactly one for %v", f.cp.fenced, want)
	}
	if len(ops.Status.Members) != 2 || !ops.Status.Members[0].Fenced || ops.Status.Members[1].VolumeID != "vol-2" {
		t.Errorf("members = %+v, want vol-1 and vol-2, fenced", ops.Status.Members)
	}
	if ops.Status.StartedAt == nil || ops.Status.CompletedAt == nil {
		t.Error("startedAt and completedAt must both be set")
	}
	if ev := f.events(); !strings.Contains(ev, "GroupFenced") {
		t.Errorf("events = %q, want GroupFenced", ev)
	}
}

// U-06, U-07, U-08, U-09: nothing to resolve fails the operation and orders no fence.
func TestFence_UnresolvableTargetFails(t *testing.T) {
	cases := []struct {
		name   string
		served bool
		objs   []client.Object
		want   string
	}{
		{"no VolumeGroupReplication", true, nil, vgrName},
		{"no content name", true, []client.Object{vgr("")}, "content"},
		{"content missing", true, []client.Object{vgr(contentName)}, contentName},
		{"volume handle, not a group", true, []client.Object{vgr(contentName),
			vgrContent(clusterID + ":pool:" + groupID)}, "pool"},
		{"empty handle", true, []client.Object{vgr(contentName), vgrContent("")}, "handle"},
		{"csi-addons not served", false, nil, "VolumeGroupReplication"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.served, append([]client.Object{fenceOps()}, tc.objs...)...)

			ops := f.settle(t)

			if ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseFailed {
				t.Fatalf("phase = %s (%s), want Failed", ops.Status.Phase, ops.Status.Message)
			}
			if !strings.Contains(ops.Status.Message, tc.want) {
				t.Errorf("message = %q, want it to name %q", ops.Status.Message, tc.want)
			}
			if len(f.cp.fenced) != 0 {
				t.Errorf("fences ordered = %v, want none", f.cp.fenced)
			}
			if ev := f.events(); !strings.Contains(ev, "FenceFailed") {
				t.Errorf("events = %q, want FenceFailed", ev)
			}
		})
	}
}

// U-10, U-17: a recorded handle is acted on without reading the VolumeGroupReplication
// again, which Ramen may have deleted, and a restart in Fencing orders the fence once more.
func TestFence_ActsOnTheRecordedHandle(t *testing.T) {
	ops := fenceOps()
	ops.Status = simplyblockv1alpha2.VolumeGroupReplicationOpsStatus{
		Phase:       simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseRunning,
		Step:        simplyblockv1alpha2.VolumeGroupReplicationOpsStepSnapshot{State: simplyblockv1alpha2.VolumeGroupReplicationOpsStepFencing},
		GroupHandle: groupHandle,
	}
	f := newFixture(t, true, ops)

	got := f.settle(t)

	if got.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded {
		t.Fatalf("phase = %s (%s), want Succeeded", got.Status.Phase, got.Status.Message)
	}
	if len(f.cp.fenced) != 1 {
		t.Errorf("fences ordered = %d, want 1", len(f.cp.fenced))
	}
}

// U-12, U-13: an unreachable or busy control plane keeps the operation in Fencing, requeued.
func TestFence_RetryableAnswersKeepFencing(t *testing.T) {
	for _, err := range []error{
		errors.New("dial tcp 10.70.3.21:443: connect: connection refused"),
		&controlplane.StatusError{Op: "fence group", StatusCode: http.StatusServiceUnavailable},
		&controlplane.StatusError{Op: "fence group", StatusCode: http.StatusInternalServerError},
		&controlplane.StatusError{Op: "fence group", StatusCode: http.StatusConflict},
	} {
		t.Run(err.Error(), func(t *testing.T) {
			f := newFixture(t, true, fenceOps(), vgr(contentName), vgrContent(groupHandle))
			f.cp.err = err

			var res ctrl.Result
			for range 5 {
				res = f.reconcile(t)
			}
			ops := f.ops(t)
			if ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseRunning ||
				ops.Status.Step.State != simplyblockv1alpha2.VolumeGroupReplicationOpsStepFencing {
				t.Fatalf("phase/step = %s/%s (%s), want Running/Fencing", ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
			}
			if res.RequeueAfter <= 0 {
				t.Errorf("result = %+v, want a requeue", res)
			}
			if ev := f.events(); !strings.Contains(ev, "FenceRetrying") {
				t.Errorf("events = %q, want FenceRetrying", ev)
			}

			f.cp.err = nil
			if got := f.settle(t); got.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded {
				t.Errorf("after the control plane answers: phase = %s, want Succeeded", got.Status.Phase)
			}
		})
	}
}

// U-14: a refusal that will not change fails the operation with the control plane's detail.
func TestFence_PermanentRefusalFails(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound} {
		f := newFixture(t, true, fenceOps(), vgr(contentName), vgrContent(groupHandle))
		f.cp.err = &controlplane.StatusError{Op: "fence group", StatusCode: code, Body: "superseded applies to asynchronous replication only"}

		ops := f.settle(t)

		if ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseFailed {
			t.Fatalf("HTTP %d: phase = %s, want Failed", code, ops.Status.Phase)
		}
		if !strings.Contains(ops.Status.Message, "asynchronous replication only") {
			t.Errorf("HTTP %d: message = %q, want the control plane's detail", code, ops.Status.Message)
		}
		if len(f.cp.fenced) != 1 {
			t.Errorf("HTTP %d: fences ordered = %d, want 1 (no retry)", code, len(f.cp.fenced))
		}
	}
}

// U-15, U-16: the Fencing step fails at its deadline and not a moment before.
func TestFence_StepDeadline(t *testing.T) {
	f := newFixture(t, true, fenceOps(), vgr(contentName), vgrContent(groupHandle))
	f.cp.err = errors.New("connection refused")
	for range 3 {
		f.reconcile(t)
	}
	entered := f.clock

	f.clock = entered.Add(FencingTimeout - time.Second)
	f.reconcile(t)
	if ops := f.ops(t); ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseRunning {
		t.Fatalf("one second before the deadline: phase = %s, want Running", ops.Status.Phase)
	}

	f.clock = entered.Add(FencingTimeout + time.Second)
	f.reconcile(t)
	ops := f.ops(t)
	if ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseFailed {
		t.Fatalf("past the deadline: phase = %s, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "connection refused") {
		t.Errorf("message = %q, want the last error", ops.Status.Message)
	}
}

// U-18: a terminal operation is inert.
func TestFence_TerminalOperationIsInert(t *testing.T) {
	ops := fenceOps()
	ops.Status.Phase = simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded
	f := newFixture(t, true, ops, vgr(contentName), vgrContent(groupHandle))

	if res := f.reconcile(t); res.RequeueAfter != 0 {
		t.Errorf("result = %+v, want no requeue", res)
	}
	if len(f.cp.fenced) != 0 {
		t.Errorf("fences ordered = %d, want 0", len(f.cp.fenced))
	}
}

// U-19: every status write records the generation it was computed from.
func TestFence_StatusCarriesObservedGeneration(t *testing.T) {
	ops := fenceOps()
	ops.Generation = 3
	f := newFixture(t, true, ops, vgr(contentName), vgrContent(groupHandle))

	f.reconcile(t)

	if got := f.ops(t).Status.ObservedGeneration; got != 3 {
		t.Errorf("observedGeneration = %d, want 3", got)
	}
}

// U-21, U-22: the member report never fails a fence that happened.
func TestFence_MemberReport(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		f := newFixture(t, true, fenceOps(), vgr(contentName), vgrContent(groupHandle))
		f.cp.membersErr = errors.New("connection reset")

		ops := f.settle(t)

		if ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded {
			t.Fatalf("phase = %s, want Succeeded", ops.Status.Phase)
		}
		if len(ops.Status.Members) != 0 || !strings.Contains(ops.Status.Message, "members") {
			t.Errorf("members = %+v, message = %q, want none and a message saying so", ops.Status.Members, ops.Status.Message)
		}
	})
	t.Run("empty group", func(t *testing.T) {
		f := newFixture(t, true, fenceOps(), vgr(contentName), vgrContent(groupHandle))
		f.cp.members = nil

		ops := f.settle(t)

		if ops.Status.Phase != simplyblockv1alpha2.VolumeGroupReplicationOpsPhaseSucceeded || len(ops.Status.Members) != 0 {
			t.Errorf("phase = %s, members = %+v, want Succeeded with none", ops.Status.Phase, ops.Status.Members)
		}
	})
}

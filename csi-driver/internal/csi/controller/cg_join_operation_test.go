// The JoinConsistencyGroup operation a late label turns into
// (design-cg-live-subsystem-flip.md §8.1, test plan U-47 to U-57): what the
// requester writes, that it never rewrites, and which label states make the
// watcher ask for one.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

func newFakeJoinOperations(objects ...runtime.Object) (*joinOperations, *dynamicfake.FakeDynamicClient) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{persistentVolumeOpsGVR: "PersistentVolumeOpsList"}, objects...)
	return &joinOperations{client: client}, client
}

// U-47: the requester writes one cluster-scoped JoinConsistencyGroup operation
// naming the volume and the label's group, and tells its caller it did.
func TestTheJoinRequestIsOneClusterScopedOperationNamingTheVolumeAndTheGroup(t *testing.T) {
	j, client := newFakeJoinOperations()
	phase, created, err := j.Ensure(context.Background(), "cg-join-v1", "pv-1", "orders-db")
	if err != nil || phase != "" || !created {
		t.Fatalf("first Ensure: phase %q created %v err %v, want \"\" true nil", phase, created, err)
	}
	got, err := client.Resource(persistentVolumeOpsGVR).Get(context.Background(), "cg-join-v1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the PersistentVolumeOps was not created: %v", err)
	}
	if got.GetNamespace() != "" {
		t.Fatalf("the operation is cluster-scoped, got namespace %q", got.GetNamespace())
	}
	pv, _, _ := unstructured.NestedString(got.Object, "spec", "persistentVolumeName")
	action, _, _ := unstructured.NestedString(got.Object, "spec", "action")
	group, _, _ := unstructured.NestedString(got.Object, "spec", "joinConsistencyGroup", "groupName")
	if pv != "pv-1" || action != "JoinConsistencyGroup" || group != "orders-db" {
		t.Fatalf("spec = %s/%s/%s, want pv-1/JoinConsistencyGroup/orders-db", pv, action, group)
	}
	labels := got.GetLabels()
	if labels[preJoinPurposeLabel] != "consistency-group-join" || labels["app.kubernetes.io/managed-by"] != "spdkcsi" {
		t.Fatalf("labels = %v, want the purpose and managed-by labels", labels)
	}
}

// U-50: an existing operation is read, never rewritten or recreated, and a
// failed one stays failed.
func TestAnExistingJoinRequestIsNeverRewritten(t *testing.T) {
	j, client := newFakeJoinOperations()
	if _, _, err := j.Ensure(context.Background(), "cg-join-v1", "pv-1", "orders-db"); err != nil {
		t.Fatal(err)
	}
	got, _ := client.Resource(persistentVolumeOpsGVR).Get(context.Background(), "cg-join-v1", metav1.GetOptions{})
	if got == nil {
		t.Fatal("the first Ensure created nothing")
	}
	if err := unstructured.SetNestedField(got.Object, "Failed", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	ops := client.Resource(persistentVolumeOpsGVR)
	if _, err := ops.Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	phase, created, err := j.Ensure(context.Background(), "cg-join-v1", "pv-1", "another-group")
	if err != nil || phase != "Failed" || created {
		t.Fatalf("second Ensure: phase %q created %v err %v, want Failed false nil", phase, created, err)
	}
	again, _ := client.Resource(persistentVolumeOpsGVR).Get(context.Background(), "cg-join-v1", metav1.GetOptions{})
	if g, _, _ := unstructured.NestedString(again.Object, "spec", "joinConsistencyGroup", "groupName"); g != "orders-db" {
		t.Fatalf("an existing request must never be rewritten, group is now %s", g)
	}
}

func TestTheJoinOperationIsOnUnlessTurnedOff(t *testing.T) {
	t.Setenv("SPDKCSI_CG_JOIN_OPERATION", "")
	if !joinOperationEnabled() {
		t.Fatal("SPDKCSI_CG_JOIN_OPERATION unset must mean on")
	}
	t.Setenv("SPDKCSI_CG_JOIN_OPERATION", "false")
	if joinOperationEnabled() {
		t.Fatal("SPDKCSI_CG_JOIN_OPERATION=false must turn it off")
	}
}

// fakeJoinOps records the operations the watcher requests and reports a
// scripted phase for them.
type fakeJoinOps struct {
	phase    string
	existing map[string]bool
	requests [][3]string // {name, pvName, group}
}

func (f *fakeJoinOps) Ensure(_ context.Context, name, pvName, group string) (string, bool, error) {
	f.requests = append(f.requests, [3]string{name, pvName, group})
	if f.existing == nil {
		f.existing = map[string]bool{}
	}
	created := !f.existing[name]
	f.existing[name] = true
	if created {
		return "", true, nil
	}
	return f.phase, false, nil
}

// joinWatcher is the watcher with the join operation on, for one bound PVC of
// the given namespace and volume.
func joinWatcher(
	t *testing.T, namespace, lvol string, membership *fakeMembership, ops *fakeJoinOps,
) (*cgMembershipWatcher, *corev1.PersistentVolumeClaim, *record.FakeRecorder) {
	t.Helper()
	pvName := "pv-" + namespace
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data", Namespace: namespace,
			Labels: map[string]string{consistencyGroupLabel: "orders-db"},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       testDriver,
					VolumeHandle: testCluster + ":" + testPool + ":" + lvol,
				},
			},
		},
	}
	recorder := record.NewFakeRecorder(16)
	return &cgMembershipWatcher{
		kube:       fake.NewSimpleClientset(pvc, pv),
		driverName: testDriver,
		clientFor: func(context.Context, string, string) (membershipClient, error) {
			return membership, nil
		},
		recorder:   recorder,
		migrations: &fakeMigrations{},
		colocate:   true,
		joinOps:    ops,
	}, pvc, recorder
}

func groupOrdersDB() *fakeMembership {
	g := &controlplane.ConsistencyGroupSummary{ID: "g1", Name: "orders-db"}
	return &fakeMembership{
		groupsByName: map[string]*controlplane.ConsistencyGroupSummary{"orders-db": g},
		groupsByID:   map[string]*controlplane.ConsistencyGroupSummary{"g1": g},
	}
}

func drain(recorder *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(events []string, prefix string) bool {
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// U-47: a label on a volume in no group asks for the operation, announces it,
// and joins nothing directly.
func TestALateLabelOnAVolumeInNoGroupRequestsTheJoinOperation(t *testing.T) {
	membership := groupOrdersDB()
	ops := &fakeJoinOps{}
	w, pvc, recorder := joinWatcher(t, "app", testLvol, membership, ops)

	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	want := [3]string{wantOpName("orders-db"), "pv-app", "orders-db"}
	if len(ops.requests) != 1 || ops.requests[0] != want {
		t.Fatalf("requests = %v, want one %v", ops.requests, want)
	}
	if len(membership.joined) != 0 || len(membership.colocated) != 0 {
		t.Fatalf("the operator joins and colocates, the watcher must not: joined %v colocated %v",
			membership.joined, membership.colocated)
	}
	if events := drain(recorder); !hasEvent(events, "Normal ConsistencyGroupJoinStarted") {
		t.Fatalf("events = %v, want ConsistencyGroupJoinStarted", events)
	}
}

// U-49: a volume that already belongs to a group is never handed to the
// operation, whether or not it sits in the group's subsystem.
func TestAVolumeAlreadyInAGroupGetsNoJoinOperation(t *testing.T) {
	membership := groupOrdersDB()
	membership.groupIDByLvol = map[string]string{testLvol: "g1"}
	ops := &fakeJoinOps{}
	w, pvc, _ := joinWatcher(t, "app", testLvol, membership, ops)
	w.colocate = false

	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	if len(ops.requests) != 0 {
		t.Fatalf("a member must get no operation, got %v", ops.requests)
	}
}

// U-50, U-51: later passes report the operation's phase as an event, and a
// failed one is reported, not recreated.
func TestEachPassReportsTheJoinOperationsPhase(t *testing.T) {
	cases := []struct {
		phase string
		event string
	}{
		{"", "Normal ConsistencyGroupJoining"},
		{"Pending", "Normal ConsistencyGroupJoining"},
		{"Running", "Normal ConsistencyGroupJoining"},
		{"Succeeded", "Normal ConsistencyGroupColocated"},
		{"Failed", "Warning ConsistencyGroupJoinFailed"},
		{"Aborted", "Warning ConsistencyGroupJoinFailed"},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			ops := &fakeJoinOps{phase: tc.phase, existing: map[string]bool{wantOpName("orders-db"): true}}
			w, pvc, recorder := joinWatcher(t, "app", testLvol, groupOrdersDB(), ops)
			if err := w.reconcile(context.Background(), pvc); err != nil {
				t.Fatal(err)
			}
			events := drain(recorder)
			if !hasEvent(events, tc.event) {
				t.Fatalf("events = %v, want %s", events, tc.event)
			}
			if hasEvent(events, "Normal ConsistencyGroupJoinStarted") {
				t.Fatalf("an existing operation must not be announced as started again: %v", events)
			}
			if tc.event == "Warning ConsistencyGroupJoinFailed" && !strings.Contains(strings.Join(events, " "), "delete") {
				t.Fatalf("a failed operation must say that deleting it retries: %v", events)
			}
		})
	}
}

// U-52: two claims in two namespaces get two operations with distinct names.
func TestTwoClaimsInTwoNamespacesGetTwoOperations(t *testing.T) {
	ops := &fakeJoinOps{}
	other := "33333333-3333-3333-3333-333333333333"
	for _, c := range []struct{ ns, lvol string }{{"app", testLvol}, {"billing", other}} {
		w, pvc, _ := joinWatcher(t, c.ns, c.lvol, groupOrdersDB(), ops)
		if err := w.reconcile(context.Background(), pvc); err != nil {
			t.Fatal(err)
		}
	}
	if len(ops.requests) != 2 || ops.requests[0][0] == ops.requests[1][0] {
		t.Fatalf("requests = %v, want two with distinct names", ops.requests)
	}
}

// U-57: a claim that is not bound yet asks for nothing.
func TestAnUnboundClaimGetsNoJoinOperation(t *testing.T) {
	ops := &fakeJoinOps{}
	w, pvc, _ := joinWatcher(t, "app", testLvol, groupOrdersDB(), ops)
	pvc.Status.Phase = corev1.ClaimPending

	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	if len(ops.requests) != 0 {
		t.Fatalf("an unbound claim must get no operation, got %v", ops.requests)
	}
}

// wantOpName spells, for testLvol, the join operation's name independently of
// the code under test: the volume, then the first eight hex characters of the
// group name's SHA-256.
func wantOpName(group string) string {
	sum := sha256.Sum256([]byte(group))
	return "cg-join-" + testLvol + "-" + hex.EncodeToString(sum[:])[:8]
}

// twoGroups is a membership with the volume in orders-db (g1) and a second
// group, billing-db (g2), the label can be moved to.
func twoGroups() *fakeMembership {
	g1 := &controlplane.ConsistencyGroupSummary{ID: "g1", Name: "orders-db"}
	g2 := &controlplane.ConsistencyGroupSummary{ID: "g2", Name: "billing-db"}
	return &fakeMembership{
		groupIDByLvol: map[string]string{testLvol: "g1"},
		groupsByName:  map[string]*controlplane.ConsistencyGroupSummary{"orders-db": g1, "billing-db": g2},
		groupsByID:    map[string]*controlplane.ConsistencyGroupSummary{"g1": g1, "g2": g2},
	}
}

// Regression: 2026-11-03-cg-relabel-move. A member relabeled to another group
// was only warned about, so a volume could never move from one consistency
// group to another. The relabel now asks for one join operation into the new
// group, named for the volume and that group, and the operator leaves the old
// group as part of it.
func TestRelabelingAMemberRequestsOneJoinOperationIntoTheNewGroup(t *testing.T) {
	membership := twoGroups()
	ops := &fakeJoinOps{}
	w, pvc, recorder := joinWatcher(t, "app", testLvol, membership, ops)
	w.colocate = false
	pvc.Labels[consistencyGroupLabel] = "billing-db"

	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	want := [3]string{wantOpName("billing-db"), "pv-app", "billing-db"}
	if len(ops.requests) != 1 || ops.requests[0] != want {
		t.Fatalf("requests = %v, want one %v", ops.requests, want)
	}
	if len(membership.joined)+len(membership.detached) != 0 {
		t.Fatalf("the operator leaves and joins, the watcher must not: joined %v detached %v",
			membership.joined, membership.detached)
	}
	events := drain(recorder)
	if !hasEvent(events, "Normal ConsistencyGroupMoveStarted") || hasEvent(events, "Warning ConsistencyGroupConflict") {
		t.Fatalf("events = %v, want ConsistencyGroupMoveStarted and no conflict", events)
	}

	// The next pass finds the same operation and creates nothing new.
	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	if len(ops.requests) != 2 || ops.requests[1] != want || len(ops.existing) != 1 {
		t.Fatalf("a second pass must ensure the same operation: requests %v existing %v", ops.requests, ops.existing)
	}
	if events := drain(recorder); hasEvent(events, "Normal ConsistencyGroupMoveStarted") {
		t.Fatalf("an existing operation must not be announced as started again: %v", events)
	}
}

// Regression: 2026-11-03-cg-relabel-move. The first join's operation is named
// for its group too, so a later move to another group gets an operation of its
// own instead of finding the first one Succeeded.
func TestTheFirstJoinsOperationIsNamedForTheVolumeAndTheGroup(t *testing.T) {
	ops := &fakeJoinOps{}
	w, pvc, _ := joinWatcher(t, "app", testLvol, groupOrdersDB(), ops)
	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	if len(ops.requests) != 1 || ops.requests[0][0] != wantOpName("orders-db") {
		t.Fatalf("requests = %v, want the name %s", ops.requests, wantOpName("orders-db"))
	}
	if wantOpName("orders-db") == wantOpName("billing-db") {
		t.Fatal("two groups must give two operation names")
	}
}

// Regression: 2026-11-03-cg-relabel-move. Without the join operation the
// watcher has no way to move a volume, so a relabel stays a warning and asks
// for nothing.
func TestWithoutTheJoinOperationARelabelStaysAConflict(t *testing.T) {
	membership := twoGroups()
	w, pvc, recorder := joinWatcher(t, "app", testLvol, membership, nil)
	w.joinOps = nil
	w.colocate = false
	pvc.Labels[consistencyGroupLabel] = "billing-db"

	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	if len(membership.joined)+len(membership.detached) != 0 {
		t.Fatalf("no membership call expected: joined %v detached %v", membership.joined, membership.detached)
	}
	if events := drain(recorder); !hasEvent(events, "Warning ConsistencyGroupConflict") {
		t.Fatalf("events = %v, want ConsistencyGroupConflict", events)
	}
}

// Regression: 2026-11-03-cg-relabel-move. A label pointing back at a group
// whose operation already Succeeded, while the volume is in another group, is
// a rejoin the one-way rule forbids: it is a conflict, never reported as
// colocated.
func TestARelabelBackToAGroupTheVolumeLeftIsAConflict(t *testing.T) {
	membership := twoGroups()
	membership.groupIDByLvol[testLvol] = "g2"
	ops := &fakeJoinOps{phase: "Succeeded", existing: map[string]bool{wantOpName("orders-db"): true}}
	w, pvc, recorder := joinWatcher(t, "app", testLvol, membership, ops)
	w.colocate = false

	if err := w.reconcile(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	events := drain(recorder)
	if !hasEvent(events, "Warning ConsistencyGroupConflict") {
		t.Fatalf("events = %v, want ConsistencyGroupConflict", events)
	}
	if hasEvent(events, "Normal ConsistencyGroupColocated") {
		t.Fatalf("a volume in another group must not be reported colocated: %v", events)
	}
	if len(ops.existing) != 1 {
		t.Fatalf("no new operation expected, got %v", ops.existing)
	}
}

// ordersGroup is the group the member tests below label their volume into.
const ordersGroup = "orders-db"

// memberOutsideItsSubsystem is a volume that joined orders-db but is not in
// the group's subsystem: the state a rolled-back swap leaves behind. The
// control plane's plan for such a member is the one step that remains.
func memberOutsideItsSubsystem(lvol string) *fakeMembership {
	m := groupOrdersDB()
	m.groupIDByLvol = map[string]string{lvol: "g1"}
	m.plan = &controlplane.JoinPlan{Steps: []string{controlplane.JoinStepColocate}, TargetNQN: "nqn.group"}
	return m
}

// Live 2026-11-10 (regression_test/26 edge-*-window): after a join's swap
// rolled back, the volume was a member in its own subsystem, and nothing ever
// moved it: the watcher only requested the operation for a non-member, so
// deleting the failed operation, which its own event asked for, created
// nothing. A member the control plane says is outside its subsystem gets the
// same operation a late join does.
func TestAMemberOutsideItsSubsystemRequestsTheJoinOperation(t *testing.T) {
	ops := &fakeJoinOps{}
	watcher, pvc, recorder := joinWatcher(t, "orders", testLvol, memberOutsideItsSubsystem(testLvol), ops)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(ops.requests) != 1 || ops.requests[0][0] != wantOpName(ordersGroup) || ops.requests[0][2] != ordersGroup {
		t.Fatalf("requests = %v, want one %s for %s", ops.requests, wantOpName(ordersGroup), ordersGroup)
	}
	if !hasEvent(drain(recorder), "Normal ConsistencyGroupJoinStarted") {
		t.Fatal("the request was not announced on the PVC")
	}
}

// A member in its group's subsystem is converged: no operation, no event, and
// the control plane's old co-location call is not made either.
func TestAMemberInItsSubsystemRequestsNothing(t *testing.T) {
	ops := &fakeJoinOps{}
	membership := groupOrdersDB()
	membership.groupIDByLvol = map[string]string{testLvol: "g1"}
	membership.plan = &controlplane.JoinPlan{}
	watcher, pvc, recorder := joinWatcher(t, "orders", testLvol, membership, ops)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(ops.requests) != 0 || len(membership.colocated) != 0 {
		t.Fatalf("a converged member produced requests %v, co-locations %v", ops.requests, membership.colocated)
	}
	if events := drain(recorder); len(events) != 0 {
		t.Fatalf("a converged member produced events %v", events)
	}
}

// The operation's phase is reported the way a late join's is, and a Failed one
// says that deleting it retries, which is now true: the next pass creates a
// new one for a member still outside the subsystem.
func TestAMemberOutsideItsSubsystemReportsItsOperationsPhase(t *testing.T) {
	for _, tc := range []struct {
		phase string
		want  string
	}{
		{opsFailed, "Warning ConsistencyGroupJoinFailed"},
		{opsAborted, "Warning ConsistencyGroupJoinFailed"},
		{"Running", "Normal ConsistencyGroupJoining"},
		{opsSucceeded, "Warning ConsistencyGroupNotColocated"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			ops := &fakeJoinOps{phase: tc.phase, existing: map[string]bool{wantOpName(ordersGroup): true}}
			watcher, pvc, recorder := joinWatcher(t, "orders", testLvol, memberOutsideItsSubsystem(testLvol), ops)
			if err := watcher.reconcile(context.Background(), pvc); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			events := drain(recorder)
			if !hasEvent(events, tc.want) {
				t.Fatalf("events = %v, want one starting %q", events, tc.want)
			}
			if tc.phase == opsFailed && !strings.Contains(strings.Join(events, "\n"), "delete it to retry") {
				t.Fatalf("events = %v, want the retry instruction", events)
			}
		})
	}
}

// A plan the control plane refuses (the group's node at its subsystem limit,
// for one) is a hold, reported as before, with no operation requested.
func TestAMemberWhoseColocationIsRefusedIsHeldWithAnEvent(t *testing.T) {
	ops := &fakeJoinOps{}
	membership := memberOutsideItsSubsystem(testLvol)
	membership.plan = nil
	membership.planErr = fmt.Errorf("%w: node holds its maximum of subsystems", controlplane.ErrMembershipRefused)
	watcher, pvc, recorder := joinWatcher(t, "orders", testLvol, membership, ops)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("a refused plan is not an error: %v", err)
	}
	if len(ops.requests) != 0 {
		t.Fatalf("a refused co-location requested %v", ops.requests)
	}
	if !hasEvent(drain(recorder), "Normal ConsistencyGroupColocationDeferred") {
		t.Fatal("the hold was not reported")
	}
}

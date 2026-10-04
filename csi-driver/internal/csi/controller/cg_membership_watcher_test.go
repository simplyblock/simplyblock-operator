// Reconcile decisions of the consistency-group membership watcher (design
// §4.5): which label states join, which detach, which hold with an event, and
// which must never touch the backend at all.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

const (
	testDriver  = "csi.simplyblock.io"
	testCluster = "11111111-1111-1111-1111-111111111111"
	testPool    = "pool-1"
	testLvol    = "22222222-2222-2222-2222-222222222222"
)

type fakeMembership struct {
	groupIDByLvol map[string]string
	groupsByName  map[string]*controlplane.ConsistencyGroupSummary
	groupsByID    map[string]*controlplane.ConsistencyGroupSummary
	joinErr       error

	joined   [][2]string // {groupID, lvolID}
	detached [][2]string

	plan        *controlplane.JoinPlan
	planErr     error
	colocateErr error
	colocated   [][2]string
	swapReady   []bool
}

func (f *fakeMembership) PlanConsistencyGroupJoin(
	_ context.Context, _, _ string,
) (*controlplane.JoinPlan, error) {
	if f.planErr != nil {
		return nil, f.planErr
	}
	if f.plan == nil {
		return &controlplane.JoinPlan{Steps: []string{controlplane.JoinStepJoin}}, nil
	}
	return f.plan, nil
}

func (f *fakeMembership) ColocateConsistencyGroupMember(
	_ context.Context, groupID, lvolID string, clientSwapReady bool,
) error {
	f.colocated = append(f.colocated, [2]string{groupID, lvolID})
	f.swapReady = append(f.swapReady, clientSwapReady)
	return f.colocateErr
}

// fakeMigrations records the VolumeMigrations the watcher requests and
// reports a scripted phase for them.
type fakeMigrations struct {
	phase    string
	err      error
	requests [][3]string // {name, pvName, target}
}

func (f *fakeMigrations) Ensure(_ context.Context, name, pvName, target string) (string, error) {
	f.requests = append(f.requests, [3]string{name, pvName, target})
	return f.phase, f.err
}

func (f *fakeMembership) GetVolumeGroupID(_ context.Context, lvolID string) (string, error) {
	return f.groupIDByLvol[lvolID], nil
}

func (f *fakeMembership) ResolveConsistencyGroupByName(
	_ context.Context, name string,
) (*controlplane.ConsistencyGroupSummary, error) {
	return f.groupsByName[name], nil
}

func (f *fakeMembership) GetConsistencyGroup(
	_ context.Context, groupID string,
) (*controlplane.ConsistencyGroupSummary, error) {
	if g := f.groupsByID[groupID]; g != nil {
		return g, nil
	}
	return nil, errors.New("group not found")
}

func (f *fakeMembership) JoinConsistencyGroupMember(_ context.Context, groupID, lvolID string) error {
	if f.joinErr != nil {
		return f.joinErr
	}
	f.joined = append(f.joined, [2]string{groupID, lvolID})
	return nil
}

func (f *fakeMembership) DetachConsistencyGroupMember(_ context.Context, groupID, lvolID string) error {
	f.detached = append(f.detached, [2]string{groupID, lvolID})
	return nil
}

// boundPVC builds a Bound PVC and its CSI PV in a fake clientset, returning
// the watcher wired to the given membership fake and a 16-event recorder.
func watcherFixture(
	t *testing.T, labels map[string]string, driver string, membership *fakeMembership,
) (*cgMembershipWatcher, *corev1.PersistentVolumeClaim, *record.FakeRecorder) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app", Labels: labels},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-data"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       driver,
					VolumeHandle: testCluster + ":" + testPool + ":" + testLvol,
				},
			},
		},
	}
	recorder := record.NewFakeRecorder(16)
	watcher := &cgMembershipWatcher{
		kube:       fake.NewSimpleClientset(pvc, pv),
		driverName: testDriver,
		clientFor: func(context.Context, string, string) (membershipClient, error) {
			return membership, nil
		},
		recorder: recorder,
	}
	return watcher, pvc, recorder
}

func requireEvent(t *testing.T, recorder *record.FakeRecorder, substring string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, substring) {
			t.Fatalf("event %q does not contain %q", event, substring)
		}
	default:
		t.Fatalf("expected an event containing %q, got none", substring)
	}
}

func TestLabelAddJoinsTheNamedGroup(t *testing.T) {
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{},
		groupsByName: map[string]*controlplane.ConsistencyGroupSummary{
			"db-group": {ID: "gid-1", Name: "db-group"},
		},
	}
	watcher, pvc, recorder := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "db-group"}, testDriver, membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.joined) != 1 || membership.joined[0] != [2]string{"gid-1", testLvol} {
		t.Fatalf("expected one join of %s to gid-1, got %v", testLvol, membership.joined)
	}
	requireEvent(t, recorder, "ConsistencyGroupJoined")
}

func TestLabelNamingNoGroupHoldsWithAnEvent(t *testing.T) {
	// A group is born from its first provisioned labeled volume, never by the
	// watcher (design §4.1): the mismatch holds visibly until then.
	membership := &fakeMembership{groupIDByLvol: map[string]string{}}
	watcher, pvc, recorder := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "nope"}, testDriver, membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.joined) != 0 {
		t.Fatalf("no join expected, got %v", membership.joined)
	}
	requireEvent(t, recorder, "ConsistencyGroupPending")
}

func TestLabelRemovalDetachesFromALabelManagedGroup(t *testing.T) {
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{testLvol: "gid-1"},
		groupsByID: map[string]*controlplane.ConsistencyGroupSummary{
			"gid-1": {ID: "gid-1", Name: "db-group"},
		},
	}
	watcher, pvc, recorder := watcherFixture(t, nil, testDriver, membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.detached) != 1 || membership.detached[0] != [2]string{"gid-1", testLvol} {
		t.Fatalf("expected one detach of %s from gid-1, got %v", testLvol, membership.detached)
	}
	requireEvent(t, recorder, "ConsistencyGroupDetached")
}

func TestLabelRemovalNeverDetachesFromAPolicyOwnedGroup(t *testing.T) {
	// A legacy policy-owned group has no name: its membership belongs to the
	// policy, and a PVC that never carried the label says nothing about it.
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{testLvol: "gid-legacy"},
		groupsByID: map[string]*controlplane.ConsistencyGroupSummary{
			"gid-legacy": {ID: "gid-legacy", Name: ""},
		},
	}
	watcher, pvc, _ := watcherFixture(t, nil, testDriver, membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.detached) != 0 {
		t.Fatalf("policy-owned group must not be touched, got detach %v", membership.detached)
	}
}

func TestRefusedJoinIsAnEventNotAnError(t *testing.T) {
	// A precondition refusal (placement, pool, cap, one-way) is user-fixable
	// state: surfaced as a Warning and retried on resync, never hot-looped.
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{},
		groupsByName: map[string]*controlplane.ConsistencyGroupSummary{
			"db-group": {ID: "gid-1", Name: "db-group"},
		},
		joinErr: controlplane.ErrMembershipRefused,
	}
	watcher, pvc, recorder := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "db-group"}, testDriver, membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("a refusal must not surface as a transient error: %v", err)
	}
	requireEvent(t, recorder, "ConsistencyGroupJoinRefused")
}

func TestForeignDriverVolumesAreIgnored(t *testing.T) {
	membership := &fakeMembership{groupIDByLvol: map[string]string{testLvol: "gid-1"}}
	watcher, pvc, _ := watcherFixture(t, nil, "ebs.csi.aws.com", membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.joined)+len(membership.detached) != 0 {
		t.Fatal("a foreign driver's volume must never be reconciled")
	}
}

func TestConvergedMembershipIsANoOp(t *testing.T) {
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{testLvol: "gid-1"},
		groupsByID: map[string]*controlplane.ConsistencyGroupSummary{
			"gid-1": {ID: "gid-1", Name: "db-group"},
		},
	}
	watcher, pvc, _ := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "db-group"}, testDriver, membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.joined)+len(membership.detached) != 0 {
		t.Fatal("a converged PVC must not produce membership calls")
	}
}

func TestConflictingLabelIsSurfacedNotActedOn(t *testing.T) {
	// Moving a volume between groups would be a detach plus a rejoin the
	// one-way rule forbids (design §4.3): the watcher only reports it.
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{testLvol: "gid-1"},
		groupsByID: map[string]*controlplane.ConsistencyGroupSummary{
			"gid-1": {ID: "gid-1", Name: "db-group"},
		},
	}
	watcher, pvc, recorder := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "other-group"}, testDriver, membership)

	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.joined)+len(membership.detached) != 0 {
		t.Fatal("a group conflict must not produce membership calls")
	}
	requireEvent(t, recorder, "ConsistencyGroupConflict")
}

func offPinFixture(t *testing.T, phase string) (
	*cgMembershipWatcher, *corev1.PersistentVolumeClaim, *record.FakeRecorder,
	*fakeMembership, *fakeMigrations,
) {
	t.Helper()
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{},
		groupsByName: map[string]*controlplane.ConsistencyGroupSummary{
			"db-group": {ID: "gid-1", Name: "db-group"},
		},
		joinErr: controlplane.ErrMembershipRefused,
		plan: &controlplane.JoinPlan{
			Steps:          []string{controlplane.JoinStepMigrate, controlplane.JoinStepJoin},
			TargetNodeID:   "node-pin",
			MigrateLvolIDs: []string{testLvol, "sibling"},
		},
	}
	migrations := &fakeMigrations{phase: phase}
	watcher, pvc, recorder := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "db-group"}, testDriver, membership)
	watcher.migrations = migrations
	return watcher, pvc, recorder, membership, migrations
}

func TestOffPinJoinRequestsThePreJoinMigration(t *testing.T) {
	watcher, pvc, recorder, membership, migrations := offPinFixture(t, "")
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(migrations.requests) != 1 ||
		migrations.requests[0] != [3]string{"cg-join-" + testLvol, "pv-data", "node-pin"} {
		t.Fatalf("expected one VolumeMigration of pv-data to node-pin, got %v", migrations.requests)
	}
	if len(membership.joined) != 0 {
		t.Fatalf("no join may happen before the volume is on the pin, got %v", membership.joined)
	}
	requireEvent(t, recorder, "ConsistencyGroupMigrating")
}

func TestACompletedPreJoinMigrationIsReportedAndTheJoinFollowsOnTheNextPass(t *testing.T) {
	watcher, pvc, recorder, _, migrations := offPinFixture(t, migrationCompleted)
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireEvent(t, recorder, "ConsistencyGroupMigrated")
	if len(migrations.requests) != 1 {
		t.Fatalf("the existing request is read, not duplicated: %v", migrations.requests)
	}
}

func TestAFailedPreJoinMigrationIsAWarning(t *testing.T) {
	watcher, pvc, recorder, _, _ := offPinFixture(t, migrationFailed)
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireEvent(t, recorder, "ConsistencyGroupMigrationFailed")
}

func TestAJoinThatCanNeverSucceedIsNotMigrated(t *testing.T) {
	watcher, pvc, recorder, membership, migrations := offPinFixture(t, "")
	membership.planErr = controlplane.ErrMembershipRefused
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(migrations.requests) != 0 {
		t.Fatalf("a refused plan must not request a migration: %v", migrations.requests)
	}
	requireEvent(t, recorder, "ConsistencyGroupJoinRefused")
}

func TestWithoutTheMigrationRequesterAnOffPinJoinStaysRefused(t *testing.T) {
	watcher, pvc, recorder, _, _ := offPinFixture(t, "")
	watcher.migrations = nil
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireEvent(t, recorder, "ConsistencyGroupJoinRefused")
}

func TestColocationRunsAfterAJoinOnlyWhenEnabled(t *testing.T) {
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{},
		groupsByName: map[string]*controlplane.ConsistencyGroupSummary{
			"db-group": {ID: "gid-1", Name: "db-group"},
		},
	}
	watcher, pvc, recorder := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "db-group"}, testDriver, membership)
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.colocated) != 0 {
		t.Fatalf("co-location is off by default, got %v", membership.colocated)
	}
	requireEvent(t, recorder, "ConsistencyGroupJoined")

	membership.joined = nil
	watcher.colocate, watcher.clientSwapReady = true, true
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(membership.colocated) != 1 || !membership.swapReady[0] {
		t.Fatalf("expected one co-location with the client swap asserted, got %v %v",
			membership.colocated, membership.swapReady)
	}
}

func TestARefusedColocationIsANormalEvent(t *testing.T) {
	membership := &fakeMembership{
		groupIDByLvol: map[string]string{testLvol: "gid-1"},
		groupsByID: map[string]*controlplane.ConsistencyGroupSummary{
			"gid-1": {ID: "gid-1", Name: "db-group"},
		},
		colocateErr: controlplane.ErrColocationRefused,
	}
	watcher, pvc, recorder := watcherFixture(t,
		map[string]string{consistencyGroupLabel: "db-group"}, testDriver, membership)
	watcher.colocate = true
	if err := watcher.reconcile(context.Background(), pvc); err != nil {
		t.Fatalf("a refused co-location is not an error: %v", err)
	}
	requireEvent(t, recorder, "ConsistencyGroupColocationDeferred")
}

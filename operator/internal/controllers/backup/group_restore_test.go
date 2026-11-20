// The group path of the Restore action, driven end to end against the fake
// control plane and a fake API server: a group restore produces one claim per
// member, named <claimName>-<member> and labeled into the restored consistency
// group (design-consistency-group-backup.md §6, §7.3).

package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/utils"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	ptr "github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const testGroupBackupID = testClusterID + "/gb-1"

func groupRestoreOps(name string) *simplyblockv1alpha2.StorageBackupOps {
	return &simplyblockv1alpha2.StorageBackupOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNamespace,
			UID:        types.UID("uid-" + name),
			Finalizers: []string{opsFinalizer},
		},
		Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
			ClusterRef:    simplyblockv1alpha2.NamespacedDnsReference{Name: testClusterCR},
			GroupBackupID: testGroupBackupID,
			Action:        simplyblockv1alpha2.StorageBackupOpsActionRestore,
			Restore: &simplyblockv1alpha2.RestoreSpec{
				ClaimName:  "restored-claim",
				TargetPool: testPoolCR,
			},
		},
	}
}

func groupMemberBackup(name, memberName string) *simplyblockv1alpha2.StorageBackup {
	return &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{simplyblockv1alpha2.BackupLabelGroupBackupID: simplyblockv1alpha2.GroupBackupLabelValue(testGroupBackupID)},
		},
		Spec: simplyblockv1alpha2.StorageBackupSpec{ClusterRef: testClusterCR},
		Status: simplyblockv1alpha2.StorageBackupStatus{
			Phase:     simplyblockv1alpha2.StorageBackupPhaseAvailable,
			ClusterID: testClusterID,
			Backup:    &simplyblockv1alpha2.BackupCopy{BackupID: name, Size: ptr.To(int64(1 << 30))},
			Source: &simplyblockv1alpha2.BackupSource{
				FSType:   "xfs",
				LvolName: memberName,
				Group:    &simplyblockv1alpha2.BackupGroup{GroupBackupID: testGroupBackupID},
			},
		},
	}
}

func TestGroupRestoreRunsTheWholeGraphAndProducesClaims(t *testing.T) {
	const lvolDB = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const lvolData = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	handleDB := lvol.NewVolumeHandle(testClusterID, testPoolID, lvolDB)
	handleData := lvol.NewVolumeHandle(testClusterID, testPoolID, lvolData)
	api := &fakeControlPlane{
		restoredGroupID: testClusterID + "/restored-cg",
		restoredGroupMembers: []controlplane.RestoredGroupMember{
			{LvolID: lvolDB, MemberName: "db"},
			{LvolID: lvolData, MemberName: "data"},
		},
		volumes: map[string]lvol.Volume{
			string(handleDB):   {Status: cpVolumeOnline},
			string(handleData): {Status: cpVolumeOnline},
		},
		connection: lvol.Connection{
			NQN:       "nqn.2023-01.io.simplyblock:lvol",
			Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.0.0.1", Port: 4420}},
		},
	}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(),
		groupMemberBackup("member-db", "db"),
		groupMemberBackup("member-data", "data"),
		groupRestoreOps(testOpsName))

	claimNames := []string{"restored-claim-db", "restored-claim-data"}
	for range 10 {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		// Bind each member's claim as it appears, standing in for the volume
		// controller that binds it in a real cluster.
		bound := 0
		for _, name := range claimNames {
			var claim corev1.PersistentVolumeClaim
			if err := r.Get(context.Background(),
				client.ObjectKey{Name: name, Namespace: testNamespace}, &claim); err != nil {
				continue
			}
			if claim.Status.Phase != corev1.ClaimBound {
				claim.Status.Phase = corev1.ClaimBound
				if err := r.Status().Update(context.Background(), &claim); err != nil {
					t.Fatalf("bind the claim %s: %v", name, err)
				}
			}
			bound++
		}
		if bound == len(claimNames) {
			break
		}
	}

	ops := reconcileUntilSettled(t, r)

	if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	// Every restored PV must name the driver as its provisioner, or the
	// external provisioner never deletes it and a deleted claim leaves the
	// backend volume alive in the restored group (live 2026-10-20).
	var pvs corev1.PersistentVolumeList
	if err := r.List(context.Background(), &pvs, client.MatchingLabels{RestoredByLabel: ops.Name}); err != nil {
		t.Fatal(err)
	}
	if len(pvs.Items) != len(claimNames) {
		t.Fatalf("%d restored PVs, want %d", len(pvs.Items), len(claimNames))
	}
	for _, pv := range pvs.Items {
		if got := pv.Annotations[ProvisionedByAnnotation]; got != utils.CSIProvisioner {
			t.Errorf("PV %s %s = %q, want %q", pv.Name, ProvisionedByAnnotation, got, utils.CSIProvisioner)
		}
	}
	if api.groupRestores != 1 {
		t.Errorf("the control plane was asked for %d group restores, want exactly 1", api.groupRestores)
	}
	if ops.Status.RestoredGroupID != testClusterID+"/restored-cg" {
		t.Errorf("status.restoredGroupID = %q, want the restored group", ops.Status.RestoredGroupID)
	}

	// One claim per member, named <claimName>-<member> and labeled into the
	// restored consistency group so the restored claims are a group.
	for _, name := range []string{"restored-claim-db", "restored-claim-data"} {
		var claim corev1.PersistentVolumeClaim
		if err := r.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: testNamespace}, &claim); err != nil {
			t.Fatalf("the group restore produced no claim %s: %v", name, err)
		}
		if got := claim.Labels[RestoredByLabel]; got != testOpsName {
			t.Errorf("claim %s restored-by label = %q, want %s", name, got, testOpsName)
		}
		if got := claim.Labels[consistencyGroupLabel]; got != restoredGroupName(ops) {
			t.Errorf("claim %s consistency-group label = %q, want %s", name, got, restoredGroupName(ops))
		}
		if len(claim.OwnerReferences) != 0 {
			t.Errorf("claim %s carries owner references, and must carry none", name)
		}
	}
}

// Two group restores of one generation run side by side. Both read the same
// immutable copies from the store and produce different volumes and claims, so
// neither waits for the other, and neither writes a lock onto the member
// StorageBackups the mirror also writes.
func TestTwoGroupRestoresOfOneGenerationRunTogether(t *testing.T) {
	api := &fakeControlPlane{
		restoredGroupID:      testClusterID + "/restored-cg",
		restoredGroupMembers: []controlplane.RestoredGroupMember{{LvolID: "lvol-db", MemberName: "db"}},
		volumes:              map[string]lvol.Volume{},
	}
	first := groupRestoreOps("restore-first")
	second := groupRestoreOps("restore-second")
	second.Spec.Restore.ClaimName = "second-claim"
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(),
		groupMemberBackup("member-db", "db"),
		first, second)

	for range 4 {
		for _, name := range []string{"restore-first", "restore-second"} {
			if _, err := r.Reconcile(context.Background(), opsRequest(name)); err != nil {
				t.Fatalf("Reconcile %s: %v", name, err)
			}
		}
	}
	for _, name := range []string{"restore-first", "restore-second"} {
		var ops simplyblockv1alpha2.StorageBackupOps
		if err := r.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: testNamespace}, &ops); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if ops.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseRunning {
			t.Errorf("%s phase = %q (%s), want Running", name, ops.Status.Phase, ops.Status.Message)
		}
	}
	if api.groupRestores != 2 {
		t.Errorf("the control plane was asked for %d group restores, want 2", api.groupRestores)
	}
	var member simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "member-db", Namespace: testNamespace}, &member); err != nil {
		t.Fatal(err)
	}
	if member.Status.ActiveOpsRef != "" {
		t.Errorf("member activeOpsRef = %q, want no lock written", member.Status.ActiveOpsRef)
	}
}

// The restored consistency group's name includes the operation, so two
// restores that pick the same claimName, in one namespace after another or in
// two namespaces, never ask the control plane for one group name twice.
func TestRestoredGroupNameIsUniquePerOperation(t *testing.T) {
	first := groupRestoreOps("restore-first")
	second := groupRestoreOps("restore-second")
	if restoredGroupName(first) == restoredGroupName(second) {
		t.Fatalf("both restores name the restored group %q", restoredGroupName(first))
	}
	if !strings.HasPrefix(restoredGroupName(first), first.Spec.Restore.ClaimName+"-") {
		t.Errorf("restored group = %q, want it to start with the claim name", restoredGroupName(first))
	}
	// The name is also the claims' consistency-group label value.
	first.Spec.Restore.ClaimName = strings.Repeat("a", 200)
	if errs := atlaskube.Validate(atlaskube.LabelValue, restoredGroupName(first)); len(errs) != 0 {
		t.Errorf("restored group %q is not a label value: %v", restoredGroupName(first), errs)
	}
}

// A group restore reports its own start and success as events on the operation,
// so a group drive is legible as one unit rather than a generic operation.
func TestGroupRestoreEmitsItsOwnEvents(t *testing.T) {
	const lvolDB = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	handleDB := lvol.NewVolumeHandle(testClusterID, testPoolID, lvolDB)
	api := &fakeControlPlane{
		restoredGroupID:      testClusterID + "/restored-cg",
		restoredGroupMembers: []controlplane.RestoredGroupMember{{LvolID: lvolDB, MemberName: "db"}},
		volumes:              map[string]lvol.Volume{string(handleDB): {Status: cpVolumeOnline}},
		connection: lvol.Connection{
			NQN:       "nqn.2023-01.io.simplyblock:lvol",
			Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.0.0.1", Port: 4420}},
		},
	}
	rec := events.NewFakeRecorder(128)
	r := &StorageBackupOpsReconciler{
		Client: testClient(t, testClusterObject(), testPoolObject(),
			groupMemberBackup("member-db", "db"), groupRestoreOps(testOpsName)),
		Scheme:   testScheme(t),
		Recorder: rec,
		API:      api,
	}

	for range 10 {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		var claim corev1.PersistentVolumeClaim
		if err := r.Get(context.Background(),
			client.ObjectKey{Name: "restored-claim-db", Namespace: testNamespace}, &claim); err == nil &&
			claim.Status.Phase != corev1.ClaimBound {
			claim.Status.Phase = corev1.ClaimBound
			if err := r.Status().Update(context.Background(), &claim); err != nil {
				t.Fatalf("bind the claim: %v", err)
			}
		}
	}
	reconcileUntilSettled(t, r)

	seen := drainEvents(rec)
	for _, reason := range []string{ReasonGroupRestoreStarted, ReasonGroupRestored} {
		if !containsEvent(seen, reason) {
			t.Errorf("events %v, want one carrying %s", seen, reason)
		}
	}
}

func drainEvents(rec events.EventRecorder) []string {
	f := rec.(*events.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-f.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func containsEvent(seen []string, reason string) bool {
	for _, e := range seen {
		if strings.Contains(e, reason) {
			return true
		}
	}
	return false
}

// groupBackupNamespaceA is the application namespace the restore tests'
// StorageGroupBackup is in, another than the operation's.
const groupBackupNamespaceA = "app-a"

// groupBackupIn is a StorageGroupBackup in groupBackupNamespaceA. Available, it
// names testGroupBackupID with one member.
func groupBackupIn(phase simplyblockv1alpha2.StorageGroupBackupPhase) *simplyblockv1alpha2.StorageGroupBackup {
	sgb := &simplyblockv1alpha2.StorageGroupBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-now", Namespace: groupBackupNamespaceA},
		Spec: simplyblockv1alpha2.StorageGroupBackupSpec{
			Source: simplyblockv1alpha2.GroupBackupSource{ConsistencyGroupName: "orders"},
		},
		Status: simplyblockv1alpha2.StorageGroupBackupStatus{Phase: phase},
	}
	if phase == simplyblockv1alpha2.StorageGroupBackupPhaseAvailable {
		sgb.Status.GroupBackupID = testGroupBackupID
		sgb.Status.MemberCount = 1
	}
	return sgb
}

// memberBesideGroupBackup is a member record placed beside the
// StorageGroupBackup, as the mirror places it.
func memberBesideGroupBackup(namespace string) *simplyblockv1alpha2.StorageBackup {
	sb := groupMemberBackup("member-db", "db")
	sb.Namespace = namespace
	sb.Labels[simplyblockv1alpha2.BackupLabelGroupBackup] = "orders-now"
	return sb
}

func byReference(ops *simplyblockv1alpha2.StorageBackupOps) *simplyblockv1alpha2.StorageBackupOps {
	ops.Spec.GroupBackupID = ""
	ops.Spec.GroupBackupRef = simplyblockv1alpha2.NamespacedReference{Name: "orders-now", Namespace: groupBackupNamespaceA}
	return ops
}

func reconcileOps(t *testing.T, r *StorageBackupOpsReconciler, passes int) *simplyblockv1alpha2.StorageBackupOps {
	t.Helper()
	for range passes {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	var got simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(), client.ObjectKey{Name: testOpsName, Namespace: testNamespace}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

// A restore that names a StorageGroupBackup in another namespace restores the
// group backup it produced, with the members read beside it.
func TestGroupRestoreByReferenceRestoresTheGroupBackup(t *testing.T) {
	api := &fakeControlPlane{
		restoredGroupID:      testClusterID + "/restored-cg",
		restoredGroupMembers: []controlplane.RestoredGroupMember{{LvolID: "lvol-db", MemberName: "db"}},
		volumes:              map[string]lvol.Volume{},
	}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(),
		memberBesideGroupBackup(groupBackupNamespaceA),
		groupBackupIn(simplyblockv1alpha2.StorageGroupBackupPhaseAvailable),
		byReference(groupRestoreOps(testOpsName)))

	got := reconcileOps(t, r, 6)
	if len(api.groupRestored) != 1 || api.groupRestored[0].GroupBackupID != testGroupBackupID {
		t.Fatalf("group restores asked = %+v, want one of %s", api.groupRestored, testGroupBackupID)
	}
	if got.Status.BackupID != testGroupBackupID {
		t.Errorf("status.backupID = %q, want the group backup the StorageGroupBackup produced", got.Status.BackupID)
	}
}

// A StorageGroupBackup still being taken, or one whose members the mirror has
// not placed beside it yet, holds the restore at Validating rather than failing
// it: both resolve on their own.
func TestGroupRestoreByReferenceWaitsForTheGroupBackup(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"in progress": {groupBackupIn(simplyblockv1alpha2.StorageGroupBackupPhaseCreating)},
		// Available, but its one member is still recorded beside the cluster.
		"members not placed yet": {
			groupBackupIn(simplyblockv1alpha2.StorageGroupBackupPhaseAvailable),
			groupMemberBackup("member-db", "db"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeControlPlane{volumes: map[string]lvol.Volume{}}
			objs = append(objs, testClusterObject(), testPoolObject(), byReference(groupRestoreOps(testOpsName)))
			r := opsReconciler(t, api, objs...)

			got := reconcileOps(t, r, 6)
			if got.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseRunning || got.Status.Step.State != string(stepValidating) {
				t.Errorf("phase = %q at %q (%s), want Running at Validating", got.Status.Phase, got.Status.Step.State, got.Status.Message)
			}
			if api.groupRestores != 0 {
				t.Errorf("the control plane was asked for %d group restores, want none yet", api.groupRestores)
			}
		})
	}
}

// A change to a StorageGroupBackup wakes the restores that name it, so a
// restore waiting for it moves on when it completes rather than on a timer.
func TestAGroupBackupWakesTheRestoresNamingIt(t *testing.T) {
	sgb := groupBackupIn(simplyblockv1alpha2.StorageGroupBackupPhaseAvailable)
	r := opsReconciler(t, &fakeControlPlane{}, byReference(groupRestoreOps(testOpsName)), sgb)

	got := r.operationsOnGroupBackup(context.Background(), sgb)
	if len(got) != 1 || got[0].Name != testOpsName {
		t.Errorf("woken = %v, want %s", got, testOpsName)
	}
}

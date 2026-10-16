// The group path of the Restore action, driven end to end against the fake
// control plane and a fake API server: a group restore produces one claim per
// member, named <claimName>-<member> and labeled into the restored consistency
// group (design-consistency-group-backup.md §6, §7.3).

package backup

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
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
			Labels:    map[string]string{simplyblockv1alpha2.BackupLabelGroupBackupID: testGroupBackupID},
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
		if got := claim.Labels[consistencyGroupLabel]; got != "restored-claim" {
			t.Errorf("claim %s consistency-group label = %q, want restored-claim", name, got)
		}
		if len(claim.OwnerReferences) != 0 {
			t.Errorf("claim %s carries owner references, and must carry none", name)
		}
	}
}

// Two group restores of one generation serialize on the per-member lock: the
// second acquires nothing and waits while the first holds the members.
func TestSecondGroupRestoreWaitsForTheMemberLock(t *testing.T) {
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

	// Drive the first until it holds the lock and is running.
	for range 4 {
		if _, err := r.Reconcile(context.Background(), opsRequest("restore-first")); err != nil {
			t.Fatalf("Reconcile first: %v", err)
		}
	}
	// The second cannot acquire the lock and stays Pending.
	if _, err := r.Reconcile(context.Background(), opsRequest("restore-second")); err != nil {
		t.Fatalf("Reconcile second: %v", err)
	}
	var secondOps simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: "restore-second", Namespace: testNamespace}, &secondOps); err != nil {
		t.Fatalf("get second: %v", err)
	}
	if secondOps.Status.Phase == simplyblockv1alpha2.StorageBackupOpsPhaseRunning {
		t.Errorf("the second group restore is Running, want it waiting for the member lock")
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

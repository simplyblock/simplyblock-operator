// What the StorageBackupOps guard admits and refuses for a group restore
// (spec.groupBackupID). The members of a group backup are the StorageBackups
// carrying its label, so admission resolves the group to that set and refuses a
// group that names no member or holds a failed one, the same reference check the
// single-backup path makes applied across the members.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	backupOpsNamespace = "simplyblock"
	testGroupBackupID  = "cluster-1/gb-1"
)

func groupMember(name, groupID string, phase simplyblockv1alpha2.StorageBackupPhase) *simplyblockv1alpha2.StorageBackup {
	return &simplyblockv1alpha2.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: backupOpsNamespace,
			Labels:    map[string]string{simplyblockv1alpha2.BackupLabelGroupBackupID: groupID},
		},
		Status: simplyblockv1alpha2.StorageBackupStatus{Phase: phase},
	}
}

func groupRestoreOps() *simplyblockv1alpha2.StorageBackupOps {
	return &simplyblockv1alpha2.StorageBackupOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-group-restore", Namespace: backupOpsNamespace},
		Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
			ClusterRef:    simplyblockv1alpha2.NamespacedDnsReference{Name: backupOpsNamespace},
			GroupBackupID: testGroupBackupID,
			Action:        simplyblockv1alpha2.StorageBackupOpsActionRestore,
			Restore:       &simplyblockv1alpha2.RestoreSpec{ClaimName: "restored-claim", TargetPool: "pool-1"},
		},
	}
}

func testCluster() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: backupOpsNamespace, Namespace: backupOpsNamespace},
	}
}

func admitGroupRestore(t *testing.T, ops *simplyblockv1alpha2.StorageBackupOps, existing ...client.Object) admission.Response {
	t.Helper()
	scheme := newScheme(t)
	v := &StorageBackupOpsValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing...).Build(),
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal ops: %v", err)
	}
	return v.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: backupOpsNamespace,
		Object:    runtime.RawExtension{Raw: raw},
	}})
}

func TestGroupRestoreWithNoMembersIsRefused(t *testing.T) {
	resp := admitGroupRestore(t, groupRestoreOps(), testCluster())
	if resp.Allowed {
		t.Fatal("a group restore naming a group backup with no members was admitted")
	}
	if !strings.Contains(resp.Result.Message, testGroupBackupID) {
		t.Errorf("the refusal is %q, want it to name the group backup id", resp.Result.Message)
	}
}

func TestGroupRestoreWithAFailedMemberIsRefused(t *testing.T) {
	resp := admitGroupRestore(t, groupRestoreOps(),
		testCluster(),
		groupMember("member-db", testGroupBackupID, simplyblockv1alpha2.StorageBackupPhaseAvailable),
		groupMember("member-data", testGroupBackupID, simplyblockv1alpha2.StorageBackupPhaseFailed),
	)
	if resp.Allowed {
		t.Fatal("a group restore with a failed member was admitted")
	}
	if !strings.Contains(resp.Result.Message, "member-data") {
		t.Errorf("the refusal is %q, want it to name the failed member", resp.Result.Message)
	}
}

func TestGroupRestoreWithHealthyMembersIsAdmitted(t *testing.T) {
	pool := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: backupOpsNamespace},
	}
	resp := admitGroupRestore(t, groupRestoreOps(),
		testCluster(),
		pool,
		groupMember("member-db", testGroupBackupID, simplyblockv1alpha2.StorageBackupPhaseAvailable),
		groupMember("member-data", testGroupBackupID, simplyblockv1alpha2.StorageBackupPhaseAvailable),
	)
	if !resp.Allowed {
		t.Fatalf("a group restore of a complete, healthy group backup was refused: %q", resp.Result.Message)
	}
}

// A members-only object graph still only resolves members labeled for THIS group
// backup. A member of a different group does not count.
func TestGroupRestoreIgnoresMembersOfOtherGroups(t *testing.T) {
	resp := admitGroupRestore(t, groupRestoreOps(),
		testCluster(),
		groupMember("other-group-member", "cluster-1/gb-2", simplyblockv1alpha2.StorageBackupPhaseAvailable),
	)
	if resp.Allowed {
		t.Fatal("a group restore was admitted on the strength of another group's member")
	}
}

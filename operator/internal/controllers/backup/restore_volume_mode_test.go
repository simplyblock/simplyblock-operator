// Tests that a restore reproduces the source volume's mode. A Block source
// restored as Filesystem reaches the CSI driver as a device with no filesystem
// signature and no on-disk-filesystem record, which the driver formats, so the
// mode is recorded at backup time and carried onto the restored volume and
// claim of both the single and the group restore paths.

package backup

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	ptr "github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Regression: 2026-10-28-restore-volume-mode — the mirror recorded the source
// volume's filesystem but not its mode, so nothing downstream could restore a
// Block volume as Block.
func TestMirrorRecordsTheSourceVolumeMode(t *testing.T) {
	volume := sourceVolume()
	volume.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
	volume.Spec.CSI.FSType = ""
	r := backupMirror(t, syncedCache(reportedBackup()), testClusterObject(), volume)

	if _, err := r.Reconcile(context.Background(), backupRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var backup simplyblockv1alpha2.StorageBackup
	if err := r.Get(context.Background(), backupRequest().NamespacedName, &backup); err != nil {
		t.Fatalf("the mirror created no object: %v", err)
	}
	if got := backup.Source().VolumeMode; got != corev1.PersistentVolumeBlock {
		t.Errorf("status.source.volumeMode = %q, want the source volume's Block", got)
	}
}

// singleRestoreAPI is a control plane whose restored volume is online and
// reachable, so a single restore runs to Binding.
func singleRestoreAPI() *fakeControlPlane {
	handle := lvol.NewVolumeHandle(testClusterID, testPoolID, testRestoreID)
	return &fakeControlPlane{
		restoredID: testRestoreID,
		volumes:    map[string]lvol.Volume{string(handle): {Status: cpVolumeOnline}},
		connection: lvol.Connection{
			NQN:       "nqn.2023-01.io.simplyblock:lvol",
			Endpoints: []lvol.Endpoint{{Transport: "tcp", Address: "10.0.0.1", Port: 4420}},
		},
	}
}

// restoreUntilClaimed drives the operation until the named claims exist and
// returns them. It stops there because the volume mode is fixed at creation.
func restoreUntilClaimed(
	t *testing.T, r *StorageBackupOpsReconciler, claimNames ...string,
) []corev1.PersistentVolumeClaim {
	t.Helper()
	for range 10 {
		if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		claims := make([]corev1.PersistentVolumeClaim, 0, len(claimNames))
		for _, name := range claimNames {
			var claim corev1.PersistentVolumeClaim
			if err := r.Get(context.Background(),
				client.ObjectKey{Name: name, Namespace: testNamespace}, &claim); err != nil {
				break
			}
			claims = append(claims, claim)
		}
		if len(claims) == len(claimNames) {
			return claims
		}
	}
	t.Fatalf("the restore produced none of the claims %v", claimNames)
	return nil
}

// expectVolumeMode checks a claim and the volume it names against want.
func expectVolumeMode(
	t *testing.T, r *StorageBackupOpsReconciler, claim corev1.PersistentVolumeClaim,
	want corev1.PersistentVolumeMode,
) {
	t.Helper()
	if got := claim.Spec.VolumeMode; got == nil || *got != want {
		t.Errorf("claim %s volumeMode = %v, want %s", claim.Name, ptrString(got), want)
	}
	var volume corev1.PersistentVolume
	if err := r.Get(context.Background(), client.ObjectKey{Name: claim.Spec.VolumeName}, &volume); err != nil {
		t.Fatalf("the restore produced no volume for claim %s: %v", claim.Name, err)
	}
	if got := volume.Spec.VolumeMode; got == nil || *got != want {
		t.Errorf("volume %s volumeMode = %v, want %s", volume.Name, ptrString(got), want)
	}
}

func ptrString(mode *corev1.PersistentVolumeMode) string {
	if mode == nil {
		return "<unset>"
	}
	return string(*mode)
}

// Regression: 2026-10-28-restore-volume-mode — a Block backup was restored as
// a Filesystem claim, and the CSI driver formats such a volume on first mount,
// destroying the restored data.
func TestRestoreOfABlockBackupProducesABlockClaim(t *testing.T) {
	backup := availableBackup()
	backup.Status.Source.FSType = ""
	backup.Status.Source.VolumeMode = corev1.PersistentVolumeBlock
	r := opsReconciler(t, singleRestoreAPI(),
		testClusterObject(), testPoolObject(), backup, restoreOps(testOpsName))

	claims := restoreUntilClaimed(t, r, "restored-claim")

	expectVolumeMode(t, r, claims[0], corev1.PersistentVolumeBlock)
}

// Regression: 2026-10-28-restore-volume-mode — a backup whose source volume was
// gone records no mode, so the request has to be able to name one.
func TestRestoreVolumeModeFromTheRequestOverridesTheBackup(t *testing.T) {
	backup := availableBackup()
	backup.Status.Source.FSType = ""
	ops := restoreOps(testOpsName)
	ops.Spec.Restore.VolumeMode = corev1.PersistentVolumeBlock
	r := opsReconciler(t, singleRestoreAPI(),
		testClusterObject(), testPoolObject(), backup, ops)

	claims := restoreUntilClaimed(t, r, "restored-claim")

	expectVolumeMode(t, r, claims[0], corev1.PersistentVolumeBlock)
}

// Regression: 2026-10-28-restore-volume-mode — the group path had the same
// omission: every member of a Block group was restored as Filesystem.
func TestGroupRestoreOfBlockMembersProducesBlockClaims(t *testing.T) {
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
	members := []*simplyblockv1alpha2.StorageBackup{
		groupMemberBackup("member-db", "db"),
		groupMemberBackup("member-data", "data"),
	}
	for _, member := range members {
		member.Status.Source.FSType = ""
		member.Status.Source.VolumeMode = corev1.PersistentVolumeBlock
	}
	r := opsReconciler(t, api,
		testClusterObject(), testPoolObject(), members[0], members[1], groupRestoreOps(testOpsName))

	for _, claim := range restoreUntilClaimed(t, r, "restored-claim-db", "restored-claim-data") {
		expectVolumeMode(t, r, claim, corev1.PersistentVolumeBlock)
	}
}

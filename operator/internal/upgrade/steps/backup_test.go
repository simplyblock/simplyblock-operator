// Tests for §16.2's two backup conversions.
//
// The one that matters is the policy copy's membership. A registered
// BackupPolicy covers the claims that carry an annotation naming it, and a
// StorageBackupPolicy covers the ones a label selector matches, so a copy that
// carried the schedule and lost the membership would leave an installation with
// a policy that looks right and backs up nothing. That failure is silent, which
// is exactly why it is asserted here rather than left to a review.

package steps

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// runMigrate drives §16's steps the way the migrate phase does.
func runMigrate(t *testing.T, scope *upgrade.Scope) error {
	t.Helper()

	catalog := upgrade.NewCatalog()
	catalog.Steps.MustRegister(copyBackupPolicies{}, absorbBackupRestores{})
	return upgrade.NewRunner(catalog, scope).ApplyAll(t.Context(), upgrade.StageMigrate)
}

func nightlyPolicy() *simplyblockv1alpha1.BackupPolicy {
	return &simplyblockv1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: testPolicyName, Namespace: "simplyblock"},
		Spec: simplyblockv1alpha1.BackupPolicySpec{
			ClusterName: "production",
			Schedule:    "24h,7",
			MaxAge:      "30d",
			MaxVersions: 7,
		},
	}
}

// The copy carries the schedule and the membership, and the membership moves
// from an annotation to the label a selector can match.
func TestCopyBackupPoliciesTurnsTheAnnotationIntoASelectorAndALabel(t *testing.T) {
	covered := claim("simplyblock", "data", map[string]string{
		"storage.simplyblock.io/backup-policy": testPolicyName,
	})
	other := claim("simplyblock", "unrelated", nil)

	scope := migration(t, nightlyPolicy(), covered, other)
	if err := runMigrate(t, scope); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}

	var copied simplyblockv1alpha2.StorageBackupPolicy
	if err := scope.Client.Get(t.Context(),
		client.ObjectKey{Name: testPolicyName, Namespace: "simplyblock"}, &copied); err != nil {
		t.Fatalf("the policy was not copied: %v", err)
	}
	if copied.Spec.ClusterRef != "production" || copied.Spec.Schedule != "24h,7" {
		t.Errorf("spec = %+v, want the original's cluster and schedule", copied.Spec)
	}
	if copied.Spec.MaxVersions == nil || *copied.Spec.MaxVersions != 7 {
		t.Errorf("maxVersions = %v, want 7", copied.Spec.MaxVersions)
	}
	if copied.Spec.ClaimSelector == nil ||
		copied.Spec.ClaimSelector.MatchLabels["storage.simplyblock.io/backup-policy"] != testPolicyName {
		t.Fatalf("claimSelector = %+v, want it to match the policy's own label", copied.Spec.ClaimSelector)
	}

	var labeled corev1.PersistentVolumeClaim
	if err := scope.Client.Get(t.Context(),
		client.ObjectKey{Name: "data", Namespace: "simplyblock"}, &labeled); err != nil {
		t.Fatal(err)
	}
	if got := labeled.Labels["storage.simplyblock.io/backup-policy"]; got != testPolicyName {
		t.Errorf("the covered claim's label = %q, want nightly", got)
	}

	var untouched corev1.PersistentVolumeClaim
	if err := scope.Client.Get(t.Context(),
		client.ObjectKey{Name: "unrelated", Namespace: "simplyblock"}, &untouched); err != nil {
		t.Fatal(err)
	}
	if _, labeledToo := untouched.Labels["storage.simplyblock.io/backup-policy"]; labeledToo {
		t.Error("a claim the policy never covered was labeled into it")
	}
}

// A claim annotated before the key rewrite ran still carries the old spelling,
// and it is covered today. Reading only the new key would drop it from the
// policy and stop its backups.
func TestCopyBackupPoliciesReadsEverySpellingOfTheMembershipKey(t *testing.T) {
	for _, spelling := range []string{
		"storage.simplyblock.io/backup-policy",
		"simplyblock.io/backup-policy",
		"simplybk/backup-policy",
	} {
		t.Run(spelling, func(t *testing.T) {
			covered := claim("simplyblock", "data", map[string]string{spelling: testPolicyName})
			scope := migration(t, nightlyPolicy(), covered)
			if err := runMigrate(t, scope); err != nil {
				t.Fatalf("ApplyAll: %v", err)
			}

			var labeled corev1.PersistentVolumeClaim
			if err := scope.Client.Get(t.Context(),
				client.ObjectKey{Name: "data", Namespace: "simplyblock"}, &labeled); err != nil {
				t.Fatal(err)
			}
			if got := labeled.Labels["storage.simplyblock.io/backup-policy"]; got != testPolicyName {
				t.Errorf("a claim annotated %s was not labeled: %q", spelling, got)
			}
		})
	}
}

// A rerun describes nothing for a policy it already copied, which is what makes
// a plan the outstanding work rather than what was originally intended.
func TestCopyBackupPoliciesIsIdempotent(t *testing.T) {
	scope := migration(t, nightlyPolicy(),
		claim("simplyblock", "data", map[string]string{"storage.simplyblock.io/backup-policy": testPolicyName}))

	if err := runMigrate(t, scope); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	if err := runMigrate(t, scope); err != nil {
		t.Fatalf("the second run: %v", err)
	}

	subject := upgrade.Subject{Ref: scope.Ref(nightlyPolicy()), Object: nightlyPolicy()}
	action, err := (copyBackupPolicies{}).Describe(t.Context(), scope, subject)
	if err != nil {
		t.Fatal(err)
	}
	if action != nil {
		t.Errorf("a policy that was already copied still describes work: %v", action)
	}
}

func finishedRestore(phase string) *simplyblockv1alpha1.BackupRestore {
	return &simplyblockv1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-1", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha1.BackupRestoreSpec{
			ClusterName: "production",
			BackupRef:   simplyblockv1alpha1.BackupRef{Name: "backup-1"},
			TargetPool:  "pool-a",
		},
		Status: simplyblockv1alpha1.BackupRestoreStatus{
			Phase:          phase,
			ClusterUUID:    "cluster-uuid",
			BackupID:       "backup-uuid",
			RestoredLvolID: "lvol-uuid",
			PoolName:       "pool-a",
			PVCName:        "restored-claim",
		},
	}
}

// The absorbed operation is a record of work that has already happened, so it
// is written terminal. Creating it in Pending would hand a finished restore to
// a controller that would run it again, which is a second volume and a claim
// that already exists.
func TestAbsorbBackupRestoresWritesTheOperationTerminal(t *testing.T) {
	scope := migration(t, finishedRestore(simplyblockv1alpha1.RestorePhaseDone))
	if err := runMigrate(t, scope); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}

	var absorbed simplyblockv1alpha2.StorageBackupOps
	if err := scope.Client.Get(t.Context(),
		client.ObjectKey{Name: "restore-1", Namespace: "simplyblock"}, &absorbed); err != nil {
		t.Fatalf("the restore was not absorbed: %v", err)
	}

	if absorbed.Spec.Action != simplyblockv1alpha2.StorageBackupOpsActionRestore {
		t.Errorf("action = %q, want Restore", absorbed.Spec.Action)
	}
	if absorbed.Spec.BackupRef != "backup-1" || absorbed.Spec.Restore.TargetPool != "pool-a" {
		t.Errorf("spec = %+v, want the backup and pool the restore named", absorbed.Spec)
	}
	if absorbed.Spec.Restore.ClaimName != "restored-claim" {
		t.Errorf("claimName = %q, want the claim the restore produced", absorbed.Spec.Restore.ClaimName)
	}
	if absorbed.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded", absorbed.Status.Phase)
	}
	if absorbed.Status.RestoredLvolID != "lvol-uuid" {
		t.Errorf("restoredLvolID = %q, want what the restore produced", absorbed.Status.RestoredLvolID)
	}
}

func TestAbsorbBackupRestoresCarriesAFailureAsAFailure(t *testing.T) {
	scope := migration(t, finishedRestore(simplyblockv1alpha1.RestorePhaseFailed))
	if err := runMigrate(t, scope); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}

	var absorbed simplyblockv1alpha2.StorageBackupOps
	if err := scope.Client.Get(t.Context(),
		client.ObjectKey{Name: "restore-1", Namespace: "simplyblock"}, &absorbed); err != nil {
		t.Fatal(err)
	}
	if absorbed.Status.Phase != simplyblockv1alpha2.StorageBackupOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", absorbed.Status.Phase)
	}
}

// A running restore has no step to be put on in the new kind, because the step
// machine did not exist when it started. Refusing here is what stops a skipped
// preflight check turning into a half-converted operation.
func TestAbsorbBackupRestoresRefusesOneStillRunning(t *testing.T) {
	running := finishedRestore(simplyblockv1alpha1.RestorePhaseInProgress)
	scope := migration(t, running)

	subject := upgrade.Subject{Ref: scope.Ref(running), Object: running}
	if err := (absorbBackupRestores{}).Validate(t.Context(), scope, subject); err == nil {
		t.Error("a running restore was admitted for absorption")
	}
}

var _ client.Object = (*simplyblockv1alpha2.StorageBackupOps)(nil)

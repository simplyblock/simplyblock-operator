// §16.2's two backup conversions: BackupPolicy becomes StorageBackupPolicy, and
// BackupRestore is absorbed into StorageBackupOps as action Restore.
//
// Both are a create and a delete rather than a conversion, because no conversion
// webhook is invoked across kinds: the target is a different kind under a
// different name and is born at v1alpha2 (design-api-upgrade.md §7.1). The old
// object is left in place, and the CRD that serves it is retired later rather
// than here, so a run that is rolled back has not destroyed what it read.
//
// The policy copy carries one change neither kind's schema shows. A registered
// BackupPolicy covers a claim that carries an annotation naming it, while a
// StorageBackupPolicy covers the claims a label selector matches
// (design-storagebackup.md §4.1). The two are the same fact under two
// mechanisms, and a selector cannot match an annotation, so the copy writes the
// key as a label on each covered claim and points the new policy's selector at
// it. The key is the same one throughout, which is what makes the membership
// legible before and after: a claim that was annotated
// storage.simplyblock.io/backup-policy: nightly ends up labeled with it.
//
// Doing otherwise would be worse than awkward. A policy copied with no selector
// covers nothing, which is the safe reading of an absent selector and the wrong
// answer here: every backup the installation was taking would silently stop.

package steps

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/keys"
)

// backupPolicyKey is the one key a policy's membership travels under, in the
// spelling §16.3 moves it to. rewriteKeys has already written it beside the old
// one by the time this runs, which is why this step requires that one.
const backupPolicyKey = simplyblockv1alpha2.BackupLabelPolicy

// copyBackupPolicies creates a StorageBackupPolicy for each BackupPolicy, and
// converts the membership annotation its claims carry into the label the new
// policy's selector matches.
type copyBackupPolicies struct{}

func (copyBackupPolicies) ID() upgrade.ID       { return IDCopyBackupPolicies }
func (copyBackupPolicies) Stage() upgrade.Stage { return upgrade.StageMigrate }
func (copyBackupPolicies) Phase() upgrade.Phase { return upgrade.PhaseTransforming }

// Requires the key rewrite, because the claims this reads carry their
// membership under the old spelling until that step has run.
func (copyBackupPolicies) Requires() []upgrade.ID { return []upgrade.ID{IDRewriteKeys} }

func (copyBackupPolicies) Description() string {
	return "copies each BackupPolicy to a StorageBackupPolicy of the same name, " +
		"and labels the claims it covers so the new policy's selector matches them"
}

// Describe names the policy that would be created, and declines one that is
// already there.
func (c copyBackupPolicies) Describe(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) (*upgrade.Action, error) {
	policy, ok := backupPolicyOf(subject)
	if !ok {
		return nil, nil
	}
	copied, err := c.copied(ctx, s, policy)
	if err != nil {
		return nil, err
	}
	if copied {
		return nil, nil
	}

	covered, err := c.coveredClaims(ctx, s, policy)
	if err != nil {
		return nil, err
	}
	return &upgrade.Action{
		Rule:   IDCopyBackupPolicies,
		Verb:   upgrade.VerbCreate,
		Object: subject.Ref,
		Detail: fmt.Sprintf("→ StorageBackupPolicy/%s, selecting %d labeled claim(s)",
			policy.Name, len(covered)),
	}, nil
}

// Done claims a policy whose copy exists.
func (c copyBackupPolicies) Done(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) (bool, error) {
	policy, ok := backupPolicyOf(subject)
	if !ok {
		return false, nil
	}
	return c.copied(ctx, s, policy)
}

// Validate has nothing to refuse. A policy is a declaration rather than an
// operation, so there is no state it can be in that makes copying it unsafe, and
// the claims it covers are labeled additively beside what they already carry.
func (copyBackupPolicies) Validate(context.Context, *upgrade.Scope, upgrade.Subject) error {
	return nil
}

// Apply labels the covered claims and writes the new policy.
//
// The labels go first. A policy created before its claims are labeled selects
// nothing for as long as the gap lasts, and the operator reconciling in that gap
// would detach every volume from the control-plane policy; the other order has
// the claims carrying a label no policy selects on yet, which does nothing at
// all.
func (c copyBackupPolicies) Apply(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) error {
	policy, ok := backupPolicyOf(subject)
	if !ok {
		return nil
	}

	covered, err := c.coveredClaims(ctx, s, policy)
	if err != nil {
		return err
	}
	for _, claim := range covered {
		labeled := claim.DeepCopy()
		if labeled.Labels == nil {
			labeled.Labels = map[string]string{}
		}
		labeled.Labels[backupPolicyKey] = policy.Name
		if err := s.Client.Update(ctx, labeled); err != nil {
			return fmt.Errorf("labeling claim %s/%s for policy %s: %w",
				claim.Namespace, claim.Name, policy.Name, err)
		}
		s.Adopt(labeled)
	}

	copied := &simplyblockv1alpha2.StorageBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policy.Name, Namespace: policy.Namespace},
		Spec: simplyblockv1alpha2.StorageBackupPolicySpec{
			ClusterRef: policy.Spec.ClusterName,
			ClaimSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{backupPolicyKey: policy.Name},
			},
			Schedule: policy.Spec.Schedule,
			MaxAge:   policy.Spec.MaxAge,
		},
	}
	if policy.Spec.MaxVersions > 0 {
		copied.Spec.MaxVersions = ptr.To(int32(policy.Spec.MaxVersions))
	}
	if err := s.Client.Create(ctx, copied); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating StorageBackupPolicy %s: %w", policy.Name, err)
	}
	s.Adopt(copied)
	return nil
}

// Verify re-reads the copy and checks it covers what the original did.
func (c copyBackupPolicies) Verify(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) error {
	policy, ok := backupPolicyOf(subject)
	if !ok {
		return nil
	}

	var copied simplyblockv1alpha2.StorageBackupPolicy
	if err := s.Client.Get(ctx,
		client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, &copied); err != nil {
		return fmt.Errorf("re-reading StorageBackupPolicy %s: %w", policy.Name, err)
	}
	// A selector that selects nothing is the failure this verification exists
	// for: it is indistinguishable from a working policy until somebody notices
	// no backups are being taken.
	if copied.Spec.ClaimSelector == nil ||
		copied.Spec.ClaimSelector.MatchLabels[backupPolicyKey] != policy.Name {
		return fmt.Errorf("StorageBackupPolicy %s has no selector for the claims it covers", policy.Name)
	}

	covered, err := c.coveredClaims(ctx, s, policy)
	if err != nil {
		return err
	}
	for _, claim := range covered {
		var fresh corev1.PersistentVolumeClaim
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(claim), &fresh); err != nil {
			return fmt.Errorf("re-reading claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		if fresh.Labels[backupPolicyKey] != policy.Name {
			return fmt.Errorf("claim %s/%s was not labeled for policy %s",
				claim.Namespace, claim.Name, policy.Name)
		}
	}
	return nil
}

// copied reports whether the StorageBackupPolicy this policy becomes is there.
func (copyBackupPolicies) copied(
	ctx context.Context, s *upgrade.Scope, policy *simplyblockv1alpha1.BackupPolicy,
) (bool, error) {
	var existing simplyblockv1alpha2.StorageBackupPolicy
	err := s.Client.Get(ctx,
		client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading StorageBackupPolicy %s: %w", policy.Name, err)
	}
	return true, nil
}

// coveredClaims are the claims in the policy's namespace whose membership
// annotation names it, under any of the three spellings the key has had.
//
// All three are read rather than only the current one, because a claim annotated
// before §16.3 ran carries the old spelling and is covered today: reading only
// the new key would drop it from the policy and stop its backups, which is the
// failure this whole step exists to avoid.
func (copyBackupPolicies) coveredClaims(
	ctx context.Context, s *upgrade.Scope, policy *simplyblockv1alpha1.BackupPolicy,
) ([]*corev1.PersistentVolumeClaim, error) {
	var claims corev1.PersistentVolumeClaimList
	if err := s.Client.List(ctx, &claims, client.InNamespace(policy.Namespace)); err != nil {
		return nil, fmt.Errorf("listing the claims of namespace %s: %w", policy.Namespace, err)
	}

	var covered []*corev1.PersistentVolumeClaim
	for i := range claims.Items {
		claim := &claims.Items[i]
		if namedPolicy(claim.Annotations) == policy.Name {
			covered = append(covered, claim)
		}
	}
	return covered, nil
}

// namedPolicy is the policy a claim's annotations name, reading the key's three
// spellings in the order the operator itself resolves them: the current one
// wins, then the bare-prefix one, then the legacy one.
func namedPolicy(annotations map[string]string) string {
	key := keys.BackupPolicy()
	for _, spelling := range []string{key.New(), key.Old(), key.LegacyOld()} {
		if named := annotations[spelling]; named != "" {
			return named
		}
	}
	return ""
}

// backupPolicyOf narrows a subject to a BackupPolicy.
func backupPolicyOf(subject upgrade.Subject) (*simplyblockv1alpha1.BackupPolicy, bool) {
	if subject.Ref.GVK.Kind != "BackupPolicy" {
		return nil, false
	}
	policy, ok := subject.Object.(*simplyblockv1alpha1.BackupPolicy)
	return policy, ok
}

// absorbBackupRestores creates a StorageBackupOps for each BackupRestore.
//
// Only a terminal one is absorbed, and that is the design's own disposition: a
// terminal restore is an audit record, and a running one cannot be handed to a
// kind whose step machine did not exist when it started
// (design-storagebackup.md §13). The preflight's in-flight check refuses a
// running restore before the migration reaches this, so what arrives here has
// finished.
type absorbBackupRestores struct{}

func (absorbBackupRestores) ID() upgrade.ID         { return IDAbsorbRestores }
func (absorbBackupRestores) Stage() upgrade.Stage   { return upgrade.StageMigrate }
func (absorbBackupRestores) Phase() upgrade.Phase   { return upgrade.PhaseTransforming }
func (absorbBackupRestores) Requires() []upgrade.ID { return nil }

func (absorbBackupRestores) Description() string {
	return "absorbs each finished BackupRestore into a StorageBackupOps carrying action Restore"
}

func (a absorbBackupRestores) Describe(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) (*upgrade.Action, error) {
	restore, ok := backupRestoreOf(subject)
	if !ok {
		return nil, nil
	}
	absorbed, err := a.absorbed(ctx, s, restore)
	if err != nil || absorbed {
		return nil, err
	}
	return &upgrade.Action{
		Rule:   IDAbsorbRestores,
		Verb:   upgrade.VerbCreate,
		Object: subject.Ref,
		Detail: fmt.Sprintf("→ StorageBackupOps/%s, action Restore", restore.Name),
	}, nil
}

func (a absorbBackupRestores) Done(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) (bool, error) {
	restore, ok := backupRestoreOf(subject)
	if !ok {
		return false, nil
	}
	return a.absorbed(ctx, s, restore)
}

// Validate refuses a restore that has not finished.
//
// It duplicates the preflight's in-flight check deliberately. That check is
// skippable, and this one is what stops a skip turning into a half-converted
// operation: the new kind's step machine did not exist when this restore
// started, so there is no step to put it on and no way to resume it.
func (absorbBackupRestores) Validate(
	_ context.Context, _ *upgrade.Scope, subject upgrade.Subject,
) error {
	restore, ok := backupRestoreOf(subject)
	if !ok {
		return nil
	}
	switch restore.Status.Phase {
	case simplyblockv1alpha1.RestorePhaseDone, simplyblockv1alpha1.RestorePhaseFailed:
		return nil
	default:
		return fmt.Errorf("it is %s rather than finished, and a running restore cannot be absorbed "+
			"into a kind whose step machine did not exist when it started; let it finish first",
			restore.Status.Phase)
	}
}

// Apply writes the operation the restore becomes.
//
// The new object is an audit record of work that has already happened, and two
// things follow from that. It carries the historical-record marker, which is what
// gets it past an admission check that would otherwise read its existing claim as
// the adoption it refuses, and what stops the controller running a restore that
// already ran. And its status is written terminal straight afterward, so the
// record reads as what it is rather than as an operation nobody started.
func (absorbBackupRestores) Apply(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) error {
	restore, ok := backupRestoreOf(subject)
	if !ok {
		return nil
	}

	absorbed := &simplyblockv1alpha2.StorageBackupOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restore.Name,
			Namespace: restore.Namespace,
			// Without this the object is rejected and, if it were admitted, run.
			// A finished restore's claim already exists, which the admission
			// check reads as the adoption it refuses, and the controller would
			// read the object's empty status as an operation to perform.
			Annotations: map[string]string{
				simplyblockv1alpha2.HistoricalRecordAnnotation: "true",
			},
		},
		Spec: simplyblockv1alpha2.StorageBackupOpsSpec{
			ClusterRef: restore.Spec.ClusterName,
			BackupRef:  restore.Spec.BackupRef.Name,
			Action:     simplyblockv1alpha2.StorageBackupOpsActionRestore,
			Restore: &simplyblockv1alpha2.RestoreSpec{
				ClaimName:  restoredClaimName(restore),
				TargetPool: restoredPoolName(restore),
			},
		},
	}
	if err := s.Client.Create(ctx, absorbed); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating StorageBackupOps %s: %w", restore.Name, err)
	}
	s.Adopt(absorbed)

	// The status is a second write because it is a subresource, and it is what
	// makes the new object the record the old one was rather than a request the
	// operator would act on.
	absorbed.Status = simplyblockv1alpha2.StorageBackupOpsStatus{
		Phase:                absorbedPhase(restore),
		ClusterID:            restore.Status.ClusterUUID,
		BackupID:             restore.Status.BackupID,
		RestoredLvolID:       restore.Status.RestoredLvolID,
		PoolUUID:             restore.Status.PoolUUID,
		PersistentVolumeName: restore.Status.PVName,
		ClaimName:            restore.Status.PVCName,
		Message:              restore.Status.Message,
		StartedAt:            restore.Status.StartedAt,
		CompletedAt:          restore.Status.CompletedAt,
	}
	absorbed.Status.Step.State = string(simplyblockv1alpha2.StorageBackupOpsStepBinding)
	if err := s.Client.Status().Update(ctx, absorbed); err != nil {
		return fmt.Errorf("recording what StorageBackupOps %s restored: %w", restore.Name, err)
	}
	return nil
}

func (a absorbBackupRestores) Verify(
	ctx context.Context, s *upgrade.Scope, subject upgrade.Subject,
) error {
	restore, ok := backupRestoreOf(subject)
	if !ok {
		return nil
	}

	var absorbed simplyblockv1alpha2.StorageBackupOps
	if err := s.Client.Get(ctx,
		client.ObjectKey{Name: restore.Name, Namespace: restore.Namespace}, &absorbed); err != nil {
		return fmt.Errorf("re-reading StorageBackupOps %s: %w", restore.Name, err)
	}
	// A terminal phase is what stops the operator running the restore a second
	// time, so it is the one thing worth failing the verification over.
	if absorbed.Status.Phase != absorbedPhase(restore) {
		return fmt.Errorf("StorageBackupOps %s is %s rather than the %s the restore ended in",
			restore.Name, absorbed.Status.Phase, absorbedPhase(restore))
	}
	return nil
}

func (absorbBackupRestores) absorbed(
	ctx context.Context, s *upgrade.Scope, restore *simplyblockv1alpha1.BackupRestore,
) (bool, error) {
	var existing simplyblockv1alpha2.StorageBackupOps
	err := s.Client.Get(ctx,
		client.ObjectKey{Name: restore.Name, Namespace: restore.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading StorageBackupOps %s: %w", restore.Name, err)
	}
	return true, nil
}

// absorbedPhase maps the restore's terminal phase onto the operation's.
func absorbedPhase(
	restore *simplyblockv1alpha1.BackupRestore,
) simplyblockv1alpha2.StorageBackupOpsPhase {
	if restore.Status.Phase == simplyblockv1alpha1.RestorePhaseDone {
		return simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded
	}
	return simplyblockv1alpha2.StorageBackupOpsPhaseFailed
}

// restoredClaimName is the claim the restore produced, or the one it would have.
// spec.restore.claimName is required on the new kind, so a restore that never
// got as far as naming one still needs a value, and the name the old controller
// would have derived is the honest answer.
func restoredClaimName(restore *simplyblockv1alpha1.BackupRestore) string {
	for _, candidate := range []string{
		restore.Status.PVCName,
		restore.Spec.PVCTemplate.Metadata.Name,
	} {
		if candidate != "" {
			return candidate
		}
	}
	return restore.Name + "-restored"
}

// restoredPoolName is the pool the restore ran against. It is required on the
// new kind, and a restore that resolved none has its own name recorded as the
// pool it asked for, which is what the object said at the time.
func restoredPoolName(restore *simplyblockv1alpha1.BackupRestore) string {
	for _, candidate := range []string{restore.Status.PoolName, restore.Spec.TargetPool} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// backupRestoreOf narrows a subject to a BackupRestore.
func backupRestoreOf(subject upgrade.Subject) (*simplyblockv1alpha1.BackupRestore, bool) {
	if subject.Ref.GVK.Kind != "BackupRestore" {
		return nil, false
	}
	restore, ok := subject.Object.(*simplyblockv1alpha1.BackupRestore)
	return restore, ok
}

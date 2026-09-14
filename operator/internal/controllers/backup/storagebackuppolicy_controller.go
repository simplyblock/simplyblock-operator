// The StorageBackupPolicy reconciler: it puts a schedule and a retention into
// the control plane, and keeps the set of volumes under that policy equal to the
// set of claims its selector matches.
//
// It does not run the schedule. A backup schedule that stopped when the operator
// was down would be a backup schedule nobody could rely on, so the control plane
// owns the timing and the pruning, and this reconciles a declaration into it and
// reports what came back (design-storagebackup.md §4.2 and §9).
//
// The one rule worth reading before the code is the selector's default. An
// absent selector selects nothing, which is the opposite of what an empty
// metav1.LabelSelector means in most Kubernetes APIs, and it is deliberate: the
// cost of backing up too much is silent and recurring, and the cost of backing
// up too little is an error somebody sees. A policy that covers nothing is
// reported with a SelectorEmpty event rather than refused, because it is
// otherwise indistinguishable from a policy that is working.

package backup

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// policyFinalizer holds the object open long enough to detach every volume
	// and remove the control-plane policy. Without it, deleting the object would
	// leave a policy in the control plane still taking copies nothing in
	// Kubernetes accounts for.
	policyFinalizer = "storage.simplyblock.io/storagebackuppolicy-finalizer"

	// policyRetry is how long to wait before looking again at something that is
	// not wrong, only not ready: a cluster with no backend id yet, or a control
	// plane that could not be reached.
	policyRetry = 15 * time.Second
)

// BackupClient is the control-plane surface this band writes through. It is an
// interface so that a test can drive a reconciler without an HTTP server, and it
// is declared here rather than in atlas-lib because what the operator needs is
// the operator's question.
type BackupClient interface {
	ListBackupPolicies(ctx context.Context, clusterID string) ([]controlplane.BackupPolicy, error)
	BackupPolicyByName(ctx context.Context, clusterID, name string) (controlplane.BackupPolicy, error)
	CreateBackupPolicy(ctx context.Context, clusterID string, params controlplane.CreateBackupPolicyParams) (string, error)
	DeleteBackupPolicy(ctx context.Context, clusterID, policyID string) error
	AttachBackupPolicy(ctx context.Context, clusterID, policyID, volumeID string) error
	DetachBackupPolicy(ctx context.Context, clusterID, policyID, volumeID string) error
}

// StorageBackupPolicyReconciler reconciles a StorageBackupPolicy.
type StorageBackupPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      BackupClient
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackuppolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackuppolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackuppolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager registers the controller and watches claims, so that a claim
// gaining or losing the labels a policy selects on reconciles that policy
// without waiting out an interval.
func (r *StorageBackupPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageBackupPolicy{}).
		Named("storagebackuppolicy").
		Watches(&corev1.PersistentVolumeClaim{},
			handler.EnqueueRequestsFromMapFunc(r.policiesInNamespace)).
		Complete(r)
}

// policiesInNamespace enqueues every policy in the claim's namespace.
//
// It is deliberately not narrowed to the policies whose selector matches. A
// claim's labels are what decides membership, and the event that matters most is
// the one where a label was just removed, so the claim no longer matches the
// policy that has to detach it. Narrowing by the current labels would drop
// exactly that event. Policies are few, and reconciling one whose membership did
// not change costs one list of the namespace's claims.
func (r *StorageBackupPolicyReconciler) policiesInNamespace(
	ctx context.Context, claim client.Object,
) []reconcile.Request {
	var policies simplyblockv1alpha2.StorageBackupPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(claim.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(policies.Items))
	for i := range policies.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&policies.Items[i]),
		})
	}
	return requests
}

func (r *StorageBackupPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy simplyblockv1alpha2.StorageBackupPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !policy.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &policy)
	}

	if !controllerutil.ContainsFinalizer(&policy, policyFinalizer) {
		controllerutil.AddFinalizer(&policy, policyFinalizer)
		return ctrl.Result{}, r.Update(ctx, &policy)
	}

	clusterID, err := utils.ResolveClusterUUID(ctx, r.Client, policy.Namespace, policy.Spec.ClusterRef)
	if err != nil {
		return r.hold(ctx, &policy, fmt.Sprintf("waiting for cluster %s: %v", policy.Spec.ClusterRef, err))
	}

	policyID, err := r.ensurePolicy(ctx, clusterID, &policy)
	if err != nil {
		return r.fail(ctx, &policy, clusterID, fmt.Sprintf("the control plane refused the policy: %v", err))
	}

	attached, err := r.reconcileMembership(ctx, clusterID, policyID, &policy)
	if err != nil {
		return r.hold(ctx, &policy, fmt.Sprintf("the membership could not be reconciled: %v", err))
	}

	if policy.Spec.ClaimSelector == nil {
		r.Recorder.Eventf(&policy, nil, corev1.EventTypeWarning, ReasonSelectorEmpty, ReasonSelectorEmpty,
			"The policy has no claimSelector, so it covers nothing and no backup will be taken under it")
	}

	r.publishCoverage(ctx, &policy, attached)

	return ctrl.Result{RequeueAfter: policyRetry}, r.writeStatus(ctx, &policy,
		func(status *simplyblockv1alpha2.StorageBackupPolicyStatus) {
			status.Phase = simplyblockv1alpha2.StorageBackupPolicyPhaseActive
			status.ClusterID = clusterID
			status.PolicyID = policyID
			status.AttachedClaims = attached
			status.Message = fmt.Sprintf("The policy is active and covers %d claim(s)", len(attached))
			status.LastBackupAt = r.lastBackupAt(ctx, &policy)
		})
}

// ensurePolicy returns the control plane's identifier for this policy, creating
// it when it is not there.
//
// The lookup is by identifier first and by name second. The name lookup is what
// makes a delete-and-recreate of the object converge onto the policy that is
// already there rather than leaving an orphan behind, and it works because the
// control-plane policy is named after the object that asked for it.
func (r *StorageBackupPolicyReconciler) ensurePolicy(
	ctx context.Context, clusterID string, policy *simplyblockv1alpha2.StorageBackupPolicy,
) (string, error) {
	if recorded := policy.Status.PolicyID; recorded != "" {
		policies, err := r.API.ListBackupPolicies(ctx, clusterID)
		if err != nil {
			return "", err
		}
		if slices.ContainsFunc(policies, func(p controlplane.BackupPolicy) bool { return p.ID == recorded }) {
			return recorded, nil
		}
		// The policy was removed outside the operator. Falling through recreates
		// it, which is what a declaration means.
	}

	existing, err := r.API.BackupPolicyByName(ctx, clusterID, policy.Name)
	switch {
	case err == nil:
		return existing.ID, nil
	case !errors.Is(err, errs.ErrNotFound):
		return "", err
	}

	return r.API.CreateBackupPolicy(ctx, clusterID, controlplane.CreateBackupPolicyParams{
		Name:        policy.Name,
		Schedule:    policy.Spec.Schedule,
		MaxAge:      policy.Spec.MaxAge,
		MaxVersions: ptr.FromOrZero(policy.Spec.MaxVersions),
	})
}

// reconcileMembership attaches the volumes behind newly matching claims and
// detaches the ones behind claims that stopped matching, and returns what is
// covered afterward.
//
// A failure to attach one claim does not stop the others: a policy covering
// nine of ten claims is better than a policy covering none, and the one that
// failed is retried on the next pass. What it does do is leave the returned set
// without that claim, so the status says what is actually covered.
func (r *StorageBackupPolicyReconciler) reconcileMembership(
	ctx context.Context, clusterID, policyID string, policy *simplyblockv1alpha2.StorageBackupPolicy,
) ([]simplyblockv1alpha2.AttachedClaim, error) {
	log := logf.FromContext(ctx)

	desired, err := r.selectedClaims(ctx, clusterID, policy)
	if err != nil {
		return nil, err
	}

	current := policy.Status.AttachedClaims
	covered := make([]simplyblockv1alpha2.AttachedClaim, 0, len(desired))
	var failures []string

	// A claim is matched on name and volume together, so that a claim rebound
	// onto a new volume is detached from the old one and attached to the new
	// rather than silently left pointing at what it used to be.
	for _, claim := range desired {
		if containsAttachment(current, claim) {
			covered = append(covered, claim)
			continue
		}
		if err := r.API.AttachBackupPolicy(ctx, clusterID, policyID, claim.LvolID); err != nil {
			log.Error(err, "could not attach a claim to the backup policy",
				"policy", policy.Name, "claim", claim.Name)
			failures = append(failures, claim.Name)
			continue
		}
		claim.AttachedAt = ptr.To(metav1.Now())
		covered = append(covered, claim)
		r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, ReasonClaimAttached, ReasonClaimAttached,
			"Claim %s started matching and is now backed up under this policy", claim.Name)
	}

	for _, claim := range current {
		if containsAttachment(desired, claim) {
			continue
		}
		if err := r.API.DetachBackupPolicy(ctx, clusterID, policyID, claim.LvolID); err != nil {
			log.Error(err, "could not detach a claim from the backup policy",
				"policy", policy.Name, "claim", claim.Name)
			// The claim is still attached, so it is still covered and the next
			// pass tries again. Dropping it from the status would report a
			// detach that did not happen.
			covered = append(covered, claim)
			failures = append(failures, claim.Name)
			continue
		}
		r.Recorder.Eventf(policy, nil, corev1.EventTypeNormal, ReasonClaimDetached, ReasonClaimDetached,
			"Claim %s stopped matching and is no longer backed up under this policy; the backups already taken are kept",
			claim.Name)
	}

	if len(failures) > 0 {
		return covered, fmt.Errorf("%d claim(s) could not be reconciled: %v", len(failures), failures)
	}
	return covered, nil
}

// selectedClaims are the claims the policy's selector matches whose volumes this
// cluster actually backs.
//
// An absent selector selects nothing, and that is the whole argument for the
// default: a policy that silently covered every claim in the namespace would
// back up more than its author intended, and the failure would be a bill rather
// than an error.
func (r *StorageBackupPolicyReconciler) selectedClaims(
	ctx context.Context, clusterID string, policy *simplyblockv1alpha2.StorageBackupPolicy,
) ([]simplyblockv1alpha2.AttachedClaim, error) {
	if policy.Spec.ClaimSelector == nil {
		return nil, nil
	}
	// An explicitly empty selector means every claim in the namespace, which is
	// what it means in every other Kubernetes API and what somebody who wrote two
	// empty braces asked for. The default this kind guards is the absent selector
	// handled above: omitting a field and writing it empty are different
	// statements, and only the first is the one whose cost is silent.
	selector, err := metav1.LabelSelectorAsSelector(policy.Spec.ClaimSelector)
	if err != nil {
		return nil, fmt.Errorf("the claimSelector is not a valid selector: %w", err)
	}

	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims,
		client.InNamespace(policy.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}

	selected := make([]simplyblockv1alpha2.AttachedClaim, 0, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.DeletionTimestamp != nil {
			continue
		}
		volume, lvolID, err := r.volumeBehind(ctx, claim, clusterID)
		if err != nil {
			r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning,
				ReasonClaimNotEligible, ReasonClaimNotEligible,
				"Claim %s matches the selector but cannot be backed up by cluster %s: %v",
				claim.Name, policy.Spec.ClusterRef, err)
			continue
		}
		selected = append(selected, simplyblockv1alpha2.AttachedClaim{
			Name:                 claim.Name,
			PersistentVolumeName: volume,
			LvolID:               lvolID,
		})
	}
	return selected, nil
}

// volumeBehind returns the PersistentVolume name and the logical volume of a
// claim, refusing a claim this cluster does not back. A claim provisioned by
// another cluster would otherwise be attached to a policy whose control plane
// has never heard of its volume.
func (r *StorageBackupPolicyReconciler) volumeBehind(
	ctx context.Context, claim *corev1.PersistentVolumeClaim, clusterID string,
) (volumeName, lvolID string, err error) {
	if claim.Spec.VolumeName == "" {
		return "", "", errors.New("the claim is not bound yet")
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, &pv); err != nil {
		return "", "", fmt.Errorf("read the volume %s: %w", claim.Spec.VolumeName, err)
	}
	if pv.Spec.CSI == nil {
		return "", "", fmt.Errorf("the volume %s is not a CSI volume", pv.Name)
	}
	handle, wellFormed := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !wellFormed {
		return "", "", fmt.Errorf("the volume %s carries an unreadable handle %q", pv.Name, pv.Spec.CSI.VolumeHandle)
	}
	if handle.ClusterID != clusterID {
		return "", "", fmt.Errorf("the volume %s belongs to cluster %s", pv.Name, handle.ClusterID)
	}
	return pv.Name, handle.VolumeID, nil
}

// publishCoverage records how much protection the policy's claims actually have.
//
// The age gauge is the one to alert on, and it is the only measurement in this
// band that reports a promise rather than an activity: a policy that stopped
// running produces no failure and no event, and the only thing that notices is
// the age of the newest copy climbing past the schedule.
func (r *StorageBackupPolicyReconciler) publishCoverage(
	ctx context.Context,
	policy *simplyblockv1alpha2.StorageBackupPolicy,
	attached []simplyblockv1alpha2.AttachedClaim,
) {
	policyAttachedClaimsCount.WithLabelValues(policy.Spec.ClusterRef, policy.Name).Set(float64(len(attached)))

	for _, claim := range attached {
		var backups simplyblockv1alpha2.StorageBackupList
		if err := r.List(ctx, &backups,
			client.InNamespace(policy.Namespace),
			client.MatchingLabels{simplyblockv1alpha2.BackupLabelClaim: claim.Name},
		); err != nil {
			continue
		}

		available := 0
		var newest *metav1.Time
		for i := range backups.Items {
			backup := &backups.Items[i]
			if backup.Status.Phase != simplyblockv1alpha2.StorageBackupPhaseAvailable {
				continue
			}
			available++
			if done := backup.Copy().CompletedAt; done != nil && (newest == nil || done.After(newest.Time)) {
				newest = done
			}
		}

		backupAvailableCount.WithLabelValues(policy.Spec.ClusterRef, claim.Name).Set(float64(available))
		if newest != nil {
			backupAgeSeconds.WithLabelValues(policy.Spec.ClusterRef, claim.Name).
				Set(time.Since(newest.Time).Seconds())
		}
	}
}

// lastBackupAt is when the newest copy under this policy completed, read from
// the backups the mirror has recorded rather than from the control plane. It is
// what an age alert is computed from, and it is nil while the policy has
// produced nothing.
func (r *StorageBackupPolicyReconciler) lastBackupAt(
	ctx context.Context, policy *simplyblockv1alpha2.StorageBackupPolicy,
) *metav1.Time {
	claimed := make([]string, 0, len(policy.Status.AttachedClaims))
	for _, claim := range policy.Status.AttachedClaims {
		claimed = append(claimed, claim.Name)
	}

	var backups simplyblockv1alpha2.StorageBackupList
	if err := r.List(ctx, &backups, client.InNamespace(policy.Namespace)); err != nil {
		return policy.Status.LastBackupAt
	}

	var newest *metav1.Time
	for i := range backups.Items {
		backup := &backups.Items[i]
		if !slices.Contains(claimed, backup.Source().ClaimName) {
			continue
		}
		if done := backup.Copy().CompletedAt; done != nil && (newest == nil || done.After(newest.Time)) {
			newest = done
		}
	}
	return newest
}

// teardown detaches every covered claim and removes the control-plane policy,
// then releases the finalizer.
//
// Detaching before deleting is not redundant. A delete that left attachments
// behind would leave the control plane taking copies for a policy nothing in
// Kubernetes accounts for, and the attachments are the only record of which
// volumes those were.
func (r *StorageBackupPolicyReconciler) teardown(
	ctx context.Context, policy *simplyblockv1alpha2.StorageBackupPolicy,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(policy, policyFinalizer) {
		return ctrl.Result{}, nil
	}

	clusterID, policyID := policy.Status.ClusterID, policy.Status.PolicyID
	if clusterID != "" && policyID != "" {
		for _, claim := range policy.Status.AttachedClaims {
			if err := r.API.DetachBackupPolicy(ctx, clusterID, policyID, claim.LvolID); err != nil {
				logf.FromContext(ctx).Error(err, "could not detach a claim while deleting the policy",
					"policy", policy.Name, "claim", claim.Name)
				return ctrl.Result{RequeueAfter: policyRetry}, nil
			}
		}
		if err := r.API.DeleteBackupPolicy(ctx, clusterID, policyID); err != nil {
			logf.FromContext(ctx).Error(err, "could not delete the backup policy", "policy", policy.Name)
			return ctrl.Result{RequeueAfter: policyRetry}, nil
		}
	}

	policyAttachedClaimsCount.DeleteLabelValues(policy.Spec.ClusterRef, policy.Name)

	controllerutil.RemoveFinalizer(policy, policyFinalizer)
	return ctrl.Result{}, r.Update(ctx, policy)
}

// hold reports a policy that cannot proceed yet and is not wrong, and asks to be
// looked at again. Pending rather than Failed: nothing about the declaration is
// bad, and a cluster that has not finished creating is a normal state to be in.
func (r *StorageBackupPolicyReconciler) hold(
	ctx context.Context, policy *simplyblockv1alpha2.StorageBackupPolicy, message string,
) (ctrl.Result, error) {
	err := r.writeStatus(ctx, policy, func(status *simplyblockv1alpha2.StorageBackupPolicyStatus) {
		status.Phase = simplyblockv1alpha2.StorageBackupPolicyPhasePending
		status.Message = message
	})
	return ctrl.Result{RequeueAfter: policyRetry}, err
}

// fail reports a policy the control plane refused. It still requeues, because
// the refusal may be a control plane that is briefly unhappy rather than a
// declaration that can never work, and the phase is what says which somebody is
// looking at.
func (r *StorageBackupPolicyReconciler) fail(
	ctx context.Context, policy *simplyblockv1alpha2.StorageBackupPolicy, clusterID, message string,
) (ctrl.Result, error) {
	err := r.writeStatus(ctx, policy, func(status *simplyblockv1alpha2.StorageBackupPolicyStatus) {
		status.Phase = simplyblockv1alpha2.StorageBackupPolicyPhaseFailed
		status.ClusterID = clusterID
		status.Message = message
	})
	return ctrl.Result{RequeueAfter: policyRetry}, err
}

// writeStatus applies the mutation and patches only when something changed,
// which is what stops a no-op write waking every watcher of the kind.
//
// observedGeneration is set here rather than by each caller, because every path
// that writes status has by definition just looked at the spec, and a field
// declared and not written is worse than an absent one.
func (r *StorageBackupPolicyReconciler) writeStatus(
	ctx context.Context,
	policy *simplyblockv1alpha2.StorageBackupPolicy,
	mutate func(*simplyblockv1alpha2.StorageBackupPolicyStatus),
) error {
	// Retried rather than swallowed. A caller that reads nil takes the write for
	// done, and the attachment set this records is what the next pass diffs
	// against: a dropped status would make it detach and reattach every claim.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageBackupPolicy
		if err := r.Get(ctx, client.ObjectKeyFromObject(policy), &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if reflect.DeepEqual(fresh.Status, desired) {
			policy.Status = desired
			policy.ResourceVersion = fresh.ResourceVersion
			return nil
		}

		patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
		fresh.Status = desired
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		policy.Status = fresh.Status
		policy.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// containsAttachment reports whether the set covers this claim, matched on the
// claim and the volume behind it together.
func containsAttachment(
	set []simplyblockv1alpha2.AttachedClaim, claim simplyblockv1alpha2.AttachedClaim,
) bool {
	return slices.ContainsFunc(set, func(a simplyblockv1alpha2.AttachedClaim) bool {
		return a.Name == claim.Name && a.LvolID == claim.LvolID
	})
}

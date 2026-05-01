/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	backupPolicyFinalizer        = "storage.simplyblock.io/backuppolicy-finalizer"
	backupPolicyReconcileRequeue = 15 * time.Second

	// pvcBackupPolicyAnnotation is the PVC annotation that names the BackupPolicy
	// to apply. The value must match the BackupPolicy CR name in the same namespace.
	//   simplybk/backup-policy: <BackupPolicy-name>
	pvcBackupPolicyAnnotation = "simplybk/backup-policy"
)

// Event reason constants for BackupPolicy reconciliation.
const (
	eventReasonPolicyClusterLookupError = "PolicyClusterLookupError"
	eventReasonPolicyClusterAuthError   = "PolicyClusterAuthError"
	eventReasonPolicyCreateFailed       = "PolicyCreateFailed"
	eventReasonPolicyDeleteFailed       = "PolicyDeleteFailed"
	eventReasonPolicyAttachFailed       = "PolicyAttachFailed"
	eventReasonPolicyDetachFailed       = "PolicyDetachFailed"
)

// BackupPolicyReconciler reconciles a BackupPolicy object.
type BackupPolicyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	APIClient *webapi.Client
}

// backupPolicyAPIResponse is the shape of a single policy returned by the
// GET /api/v2/clusters/{id}/backups/backup-policies/ endpoint.
type backupPolicyAPIResponse struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	MaxVersions    int    `json:"max_versions"`
	MaxAge         string `json:"max_age"`
	BackupSchedule string `json:"backup_schedule"`
	Status         string `json:"status"`
}

// backupPolicyCreateRequest is the body sent to create a policy.
type backupPolicyCreateRequest struct {
	Name     string `json:"name"`
	Versions int    `json:"versions,omitempty"`
	Age      string `json:"age,omitempty"`
	Schedule string `json:"schedule,omitempty"`
}

// backupPolicyAttachRequest is the body sent to attach or detach a policy.
type backupPolicyAttachRequest struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backuppolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backuppolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backuppolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *BackupPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	policyCR := &simplyblockv1alpha1.BackupPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policyCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !policyCR.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, policyCR)
	}

	if !controllerutil.ContainsFinalizer(policyCR, backupPolicyFinalizer) {
		controllerutil.AddFinalizer(policyCR, backupPolicyFinalizer)
		if err := r.Update(ctx, policyCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ── 1. Resolve cluster credentials ───────────────────────────────────────

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, policyCR.Namespace, policyCR.Spec.ClusterName)
	if err != nil {
		r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyClusterLookupError,
			"Failed to resolve cluster UUID for %s: %v", policyCR.Spec.ClusterName, err)
		if patchErr := r.patchStatus(ctx, policyCR, func(s *simplyblockv1alpha1.BackupPolicyStatus) {
			s.Phase = simplyblockv1alpha1.BackupPolicyPhasePending
			s.Message = err.Error()
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, policyCR.Namespace, policyCR.Spec.ClusterName)
	if err != nil {
		r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyClusterAuthError,
			"Failed to get cluster auth for %s: %v", policyCR.Spec.ClusterName, err)
		if patchErr := r.patchStatus(ctx, policyCR, func(s *simplyblockv1alpha1.BackupPolicyStatus) {
			s.Phase = simplyblockv1alpha1.BackupPolicyPhasePending
			s.ClusterUUID = clusterUUID
			s.Message = err.Error()
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
	}

	apiClient := r.apiClient()

	// ── 2. Ensure the policy exists in the backend ───────────────────────────

	policyID, err := r.ensurePolicy(ctx, apiClient, clusterSecret, clusterUUID, policyCR)
	if err != nil {
		log.Error(err, "Failed to ensure backup policy in backend")
		r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyCreateFailed,
			"Failed to create backup policy: %v", err)
		if patchErr := r.patchStatus(ctx, policyCR, func(s *simplyblockv1alpha1.BackupPolicyStatus) {
			s.Phase = simplyblockv1alpha1.BackupPolicyPhaseFailed
			s.ClusterUUID = clusterUUID
			s.Message = err.Error()
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
	}

	// Persist policyID early so subsequent reconciles can find it.
	if patchErr := r.patchStatus(ctx, policyCR, func(s *simplyblockv1alpha1.BackupPolicyStatus) {
		s.ClusterUUID = clusterUUID
		s.PolicyID = policyID
		if s.Phase == "" {
			s.Phase = simplyblockv1alpha1.BackupPolicyPhaseActive
		}
	}); patchErr != nil {
		return ctrl.Result{}, patchErr
	}

	// ── 3. Reconcile PVC attachments ─────────────────────────────────────────

	desired, err := r.computeDesiredAttachments(ctx, policyCR, clusterUUID)
	if err != nil {
		log.Error(err, "Failed to compute desired attachments")
		// Not fatal — retry; individual PVC errors are logged but don't block others.
		if patchErr := r.patchStatus(ctx, policyCR, func(s *simplyblockv1alpha1.BackupPolicyStatus) {
			s.Message = fmt.Sprintf("Attachment resolution error: %v", err)
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
	}

	current := policyCR.Status.AttachedLvols

	toAttach := diffAttachments(desired, current)
	toDetach := diffAttachments(current, desired)

	var attachErrors []string

	for _, a := range toAttach {
		if attachErr := r.attachPolicy(ctx, apiClient, clusterSecret, clusterUUID, policyID, a.LvolID); attachErr != nil {
			log.Error(attachErr, "Failed to attach policy to lvol", "lvolID", a.LvolID, "pvc", a.PVCName)
			r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyAttachFailed,
				"Failed to attach policy to lvol %s (PVC %s/%s): %v", a.LvolID, a.PVCNamespace, a.PVCName, attachErr)
			attachErrors = append(attachErrors, fmt.Sprintf("attach lvol %s: %v", a.LvolID, attachErr))
		} else {
			current = append(current, a)
			log.Info("Attached backup policy to lvol", "pvc", a.PVCName, "lvolID", a.LvolID)
		}
	}

	for _, d := range toDetach {
		if detachErr := r.detachPolicy(ctx, apiClient, clusterSecret, clusterUUID, policyID, d.LvolID); detachErr != nil {
			log.Error(detachErr, "Failed to detach policy from lvol", "lvolID", d.LvolID, "pvc", d.PVCName)
			r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyDetachFailed,
				"Failed to detach policy from lvol %s (PVC %s/%s): %v", d.LvolID, d.PVCNamespace, d.PVCName, detachErr)
			attachErrors = append(attachErrors, fmt.Sprintf("detach lvol %s: %v", d.LvolID, detachErr))
		} else {
			current = removeAttachment(current, d)
			log.Info("Detached backup policy from lvol", "pvc", d.PVCName, "lvolID", d.LvolID)
		}
	}

	msg := fmt.Sprintf("Policy active; %d PVC(s) attached", len(current))
	if len(attachErrors) > 0 {
		msg = fmt.Sprintf("Partial sync — errors: %v", attachErrors)
	}

	if patchErr := r.patchStatus(ctx, policyCR, func(s *simplyblockv1alpha1.BackupPolicyStatus) {
		s.Phase = simplyblockv1alpha1.BackupPolicyPhaseActive
		s.Message = msg
		s.AttachedLvols = current
	}); patchErr != nil {
		return ctrl.Result{}, patchErr
	}

	if len(attachErrors) > 0 {
		return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the BackupPolicy controller and sets up a watch
// on PVCs so that annotation additions/removals trigger a reconcile of the
// referenced BackupPolicy CR.
func (r *BackupPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.BackupPolicy{}).
		Watches(
			&corev1.PersistentVolumeClaim{},
			handler.TypedFuncs[client.Object, reconcile.Request]{
				CreateFunc: func(_ context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					enqueuePolicy(e.Object, "", q)
				},
				UpdateFunc: func(_ context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					oldPolicy := e.ObjectOld.GetAnnotations()[pvcBackupPolicyAnnotation]
					enqueuePolicy(e.ObjectNew, oldPolicy, q)
				},
				DeleteFunc: func(_ context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					enqueuePolicy(e.Object, "", q)
				},
			},
		).
		Named("backuppolicy").
		Complete(r)
}

// enqueuePolicy enqueues reconcile requests for the policy named in the PVC's
// current annotation, and also for oldPolicyName if it differs (so a policy
// whose annotation was just removed or changed also reconciles and detaches).
func enqueuePolicy(pvc client.Object, oldPolicyName string, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	ns := pvc.GetNamespace()
	current := pvc.GetAnnotations()[pvcBackupPolicyAnnotation]
	seen := map[string]struct{}{}
	for _, name := range []string{current, oldPolicyName} {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	}
}

// handleDeletion detaches all attached lvols and removes the policy from the
// backend before releasing the finalizer.
func (r *BackupPolicyReconciler) handleDeletion(
	ctx context.Context,
	policyCR *simplyblockv1alpha1.BackupPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(policyCR, backupPolicyFinalizer) {
		return ctrl.Result{}, nil
	}

	clusterUUID := policyCR.Status.ClusterUUID
	clusterSecret := ""

	if clusterUUID == "" {
		if resolved, err := utils.ResolveClusterUUID(ctx, r.Client, policyCR.Namespace, policyCR.Spec.ClusterName); err == nil {
			clusterUUID = resolved
		}
	}
	if clusterUUID != "" {
		if _, secret, err := utils.GetClusterAuth(ctx, r.Client, policyCR.Namespace, policyCR.Spec.ClusterName); err == nil {
			clusterSecret = secret
		}
	}

	policyID := policyCR.Status.PolicyID

	if clusterUUID != "" && clusterSecret != "" && policyID != "" {
		apiClient := r.apiClient()

		// Detach from every currently-attached lvol.
		for _, a := range policyCR.Status.AttachedLvols {
			if err := r.detachPolicy(ctx, apiClient, clusterSecret, clusterUUID, policyID, a.LvolID); err != nil {
				log.Error(err, "Failed to detach policy from lvol during deletion",
					"lvolID", a.LvolID, "pvc", a.PVCName)
				r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyDetachFailed,
					"Failed to detach policy from lvol %s during deletion: %v", a.LvolID, err)
				return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
			}
		}

		// Delete the policy itself.
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/backup-policies/%s", clusterUUID, policyID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
		if err != nil {
			log.Error(err, "Failed to delete backup policy from backend")
			r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyDeleteFailed,
				"Failed to delete backup policy %s: %v", policyID, err)
			return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
		}
		if status >= 300 && status != http.StatusNotFound {
			log.Info("Backup policy delete returned non-success status", "status", status, "body", string(body))
			r.Recorder.Eventf(policyCR, corev1.EventTypeWarning, eventReasonPolicyDeleteFailed,
				"Backup policy delete returned status %d: %s", status, string(body))
			return ctrl.Result{RequeueAfter: backupPolicyReconcileRequeue}, nil
		}
	}

	controllerutil.RemoveFinalizer(policyCR, backupPolicyFinalizer)
	if err := r.Update(ctx, policyCR); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ensurePolicy verifies that a backend policy exists for this CR and creates
// it if it doesn't. Returns the backend policy UUID.
//
// Lookup is by name so that a policy survives a CR delete-and-recreate
// without leaving a stale orphan in the backend.
func (r *BackupPolicyReconciler) ensurePolicy(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	policyCR *simplyblockv1alpha1.BackupPolicy,
) (string, error) {
	// If we already have an ID stored, verify it still exists.
	if policyCR.Status.PolicyID != "" {
		existing, err := r.listPolicies(ctx, apiClient, clusterSecret, clusterUUID)
		if err != nil {
			return "", err
		}
		for _, p := range existing {
			if p.ID == policyCR.Status.PolicyID {
				return p.ID, nil
			}
		}
		// Policy was deleted externally — fall through to recreate.
	}

	// Search by name in case a previous CR left a policy behind.
	policyName := policyBackendName(policyCR)
	existing, err := r.listPolicies(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		return "", err
	}
	for _, p := range existing {
		if p.Name == policyName {
			return p.ID, nil
		}
	}

	// Create new policy.
	return r.createPolicy(ctx, apiClient, clusterSecret, clusterUUID, policyCR)
}

// listPolicies fetches all backup policies for the given cluster.
func (r *BackupPolicyReconciler) listPolicies(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
) ([]backupPolicyAPIResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/backup-policies/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("list backup policies failed: status=%d body=%s", status, string(body))
	}

	var policies []backupPolicyAPIResponse
	if err := json.Unmarshal(body, &policies); err != nil {
		return nil, fmt.Errorf("unmarshal backup policies: %w", err)
	}
	return policies, nil
}

// createPolicy calls the backend API to create a new backup policy.
func (r *BackupPolicyReconciler) createPolicy(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	policyCR *simplyblockv1alpha1.BackupPolicy,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/backup-policies/", clusterUUID)
	req := backupPolicyCreateRequest{
		Name:     policyBackendName(policyCR),
		Versions: policyCR.Spec.MaxVersions,
		Age:      policyCR.Spec.MaxAge,
		Schedule: policyCR.Spec.Schedule,
	}

	body, headers, status, err := apiClient.DoWithHeaders(ctx, clusterSecret, http.MethodPost, endpoint, req)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("create backup policy failed: status=%d body=%s", status, string(body))
	}

	policyID := headers.Get("X-Policy-Id")
	if policyID == "" {
		return "", fmt.Errorf("create backup policy response missing X-Policy-Id header")
	}
	return policyID, nil
}

// attachPolicy calls the backend to attach the policy to a single lvol.
func (r *BackupPolicyReconciler) attachPolicy(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	policyID string,
	lvolID string,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/backup-policies/%s/attach", clusterUUID, policyID)
	body, _, status, err := apiClient.DoWithHeaders(ctx, clusterSecret, http.MethodPost, endpoint, backupPolicyAttachRequest{
		TargetType: "lvol",
		TargetID:   lvolID,
	})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("attach policy failed: status=%d body=%s", status, string(body))
	}
	return nil
}

// detachPolicy calls the backend to detach the policy from a single lvol.
//
// The sbcli detach endpoint returns HTTP 400 (not 404) when the attachment
// does not exist, with the body containing "Attachment not found". We treat
// this as success to make the operation idempotent — if the attachment is
// already gone the desired state is already achieved.
func (r *BackupPolicyReconciler) detachPolicy(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	policyID string,
	lvolID string,
) error {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/backup-policies/%s/detach", clusterUUID, policyID)
	body, _, status, err := apiClient.DoWithHeaders(ctx, clusterSecret, http.MethodPost, endpoint, backupPolicyAttachRequest{
		TargetType: "lvol",
		TargetID:   lvolID,
	})
	if err != nil {
		return err
	}
	if status >= 300 {
		// sbcli returns 400 with "Attachment not found" when the attachment
		// no longer exists. Treat as success — we want idempotent detach.
		if isAttachmentNotFound(status, body) {
			return nil
		}
		return fmt.Errorf("detach policy failed: status=%d body=%s", status, string(body))
	}
	return nil
}

// isAttachmentNotFound returns true when the sbcli detach endpoint signals
// that the attachment was already absent. sbcli returns HTTP 400 rather than
// 404 in this case, so we match on both the status code and the body text.
func isAttachmentNotFound(status int, body []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	if status == http.StatusBadRequest {
		// Case-insensitive substring match — avoids tight coupling to the
		// exact error string, which may be wrapped in JSON by sbcli.
		lower := strings.ToLower(string(body))
		return strings.Contains(lower, "attachment not found")
	}
	return false
}

// computeDesiredAttachments lists all PVCs in the same namespace as the
// BackupPolicy that carry the annotation pointing to this policy, resolves
// their lvol IDs, and returns the desired set of attachments.
//
// PVCs that are unbound or belong to a different cluster are skipped with a
// warning log rather than hard-failing the whole reconcile.
func (r *BackupPolicyReconciler) computeDesiredAttachments(
	ctx context.Context,
	policyCR *simplyblockv1alpha1.BackupPolicy,
	clusterUUID string,
) ([]simplyblockv1alpha1.AttachedLvol, error) {
	log := logf.FromContext(ctx)

	var pvcList corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList, client.InNamespace(policyCR.Namespace)); err != nil {
		return nil, fmt.Errorf("list PVCs in namespace %s: %w", policyCR.Namespace, err)
	}

	desired := make([]simplyblockv1alpha1.AttachedLvol, 0, len(pvcList.Items))

	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]

		if pvc.Annotations[pvcBackupPolicyAnnotation] != policyCR.Name {
			continue
		}
		if pvc.DeletionTimestamp != nil {
			continue
		}

		lvolID, err := resolvePVCLvolID(ctx, r.Client, pvc, clusterUUID)
		if err != nil {
			log.Info("Skipping PVC — cannot resolve lvol ID",
				"pvc", pvc.Name, "namespace", pvc.Namespace, "reason", err.Error())
			continue
		}

		desired = append(desired, simplyblockv1alpha1.AttachedLvol{
			PVCName:      pvc.Name,
			PVCNamespace: pvc.Namespace,
			LvolID:       lvolID,
		})
	}

	return desired, nil
}

// resolvePVCLvolID extracts the Simplyblock lvol UUID from a PVC.
// It reads the PV volume handle and validates that the PVC belongs to the
// expected cluster. The simplybk/lvol-id annotation may be used in place of
// the handle, but only when it agrees with the handle — a mismatch is rejected
// to prevent acting on a stale or mis-set annotation.
func resolvePVCLvolID(ctx context.Context, k8sClient client.Client, pvc *corev1.PersistentVolumeClaim, expectedClusterUUID string) (string, error) {
	if pvc.Spec.VolumeName == "" {
		return "", fmt.Errorf("PVC %s/%s is not bound", pvc.Namespace, pvc.Name)
	}

	pv := &corev1.PersistentVolume{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return "", fmt.Errorf("get PV %s: %w", pvc.Spec.VolumeName, err)
	}

	if pv.Spec.CSI == nil {
		return "", fmt.Errorf("PV %s is not a CSI volume", pv.Name)
	}

	handleClusterUUID, _, handleLvolID, err := parseSimplyblockVolumeHandle(pv.Spec.CSI.VolumeHandle)
	if err != nil {
		return "", err
	}

	if handleClusterUUID != "" && expectedClusterUUID != "" && handleClusterUUID != expectedClusterUUID {
		return "", fmt.Errorf("PVC %s/%s belongs to cluster %s, not %s",
			pvc.Namespace, pvc.Name, handleClusterUUID, expectedClusterUUID)
	}

	lvolID := pvc.Annotations[pvcLvolIDAnnotation]
	if lvolID == "" {
		lvolID = handleLvolID
	}
	if lvolID == "" {
		return "", fmt.Errorf("PVC %s/%s has no Simplyblock lvol ID", pvc.Namespace, pvc.Name)
	}
	if handleLvolID != "" && handleLvolID != lvolID {
		return "", fmt.Errorf(
			"PVC %s/%s lvol annotation %s does not match PV volume handle %s",
			pvc.Namespace, pvc.Name, lvolID, handleLvolID,
		)
	}

	return lvolID, nil
}

// patchStatus applies the mutate function to a copy of the status and patches
// only when there is an actual change. This prevents infinite reconcile loops
// caused by no-op status writes.
func (r *BackupPolicyReconciler) patchStatus(
	ctx context.Context,
	policyCR *simplyblockv1alpha1.BackupPolicy,
	mutate func(s *simplyblockv1alpha1.BackupPolicyStatus),
) error {
	desired := policyCR.Status.DeepCopy()
	mutate(desired)
	if reflect.DeepEqual(policyCR.Status, *desired) {
		return nil
	}

	patch := client.MergeFrom(policyCR.DeepCopy())
	policyCR.Status = *desired
	return r.Status().Patch(ctx, policyCR, patch)
}

func (r *BackupPolicyReconciler) apiClient() *webapi.Client {
	if r.APIClient != nil {
		return r.APIClient
	}
	return webapi.NewClient()
}

// policyBackendName returns the name used for the policy in the Simplyblock
// backend. It is derived from the CR name alone, since the Kubernetes CR is
// already namespace-scoped and users typically intend the policy name to be
// human-readable.
func policyBackendName(policyCR *simplyblockv1alpha1.BackupPolicy) string {
	return policyCR.Name
}

// diffAttachments returns entries that are in 'a' but not in 'b', matched by
// PVCNamespace + PVCName + LvolID. Including LvolID ensures that a rebound PVC
// (same namespace/name, new lvol) is treated as a distinct attachment, so the
// old lvol is detached and the new one is attached rather than silently skipped.
func diffAttachments(a, b []simplyblockv1alpha1.AttachedLvol) []simplyblockv1alpha1.AttachedLvol {
	bSet := make(map[string]struct{}, len(b))
	for _, v := range b {
		bSet[attachmentKey(v)] = struct{}{}
	}
	var diff []simplyblockv1alpha1.AttachedLvol
	for _, v := range a {
		if _, found := bSet[attachmentKey(v)]; !found {
			diff = append(diff, v)
		}
	}
	return diff
}

// removeAttachment returns the slice with the given entry removed (matched by
// PVCNamespace + PVCName + LvolID).
func removeAttachment(slice []simplyblockv1alpha1.AttachedLvol, remove simplyblockv1alpha1.AttachedLvol) []simplyblockv1alpha1.AttachedLvol {
	key := attachmentKey(remove)
	out := slice[:0:0]
	for _, v := range slice {
		if attachmentKey(v) != key {
			out = append(out, v)
		}
	}
	return out
}

func attachmentKey(a simplyblockv1alpha1.AttachedLvol) string {
	return a.PVCNamespace + "/" + a.PVCName + "/" + a.LvolID
}

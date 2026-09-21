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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"
	atlaslvol "github.com/simplyblock/atlas/lvol"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	groupReplSyncInterval = 60 * time.Second
	groupReplRequeueError = 30 * time.Second

	// consistencyGroupLabel names a PVC's consistency group (design-consistency-groups.md
	// §4.1). Duplicated, not shared: the webhook package and
	// internal/controllers/consistencygroup each declare their own copy of this same
	// unexported constant, and this package follows that established precedent
	// rather than introducing a shared import across packages for one string.
	consistencyGroupLabel = "storage.simplyblock.io/consistency-group"

	reasonGroupReplicationVerified = "GroupReplicationVerified"
	reasonGroupMembershipMismatch  = "GroupMembershipMismatch"
	reasonGroupReplicationDegraded = "GroupReplicationDegraded"
)

var (
	volumeGroupReplicationGVK = schema.GroupVersionKind{
		Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeGroupReplication",
	}
	volumeGroupReplicationClassGVK = schema.GroupVersionKind{
		Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeGroupReplicationClass",
	}
	volumeReplicationGVK = schema.GroupVersionKind{
		Group: "replication.storage.openshift.io", Version: "v1alpha1", Kind: "VolumeReplication",
	}
)

// VolumeGroupReplicationReconciler fans a VolumeGroupReplication's group-level
// replication intent out to one per-volume VolumeReplication per consistency-group
// member, and fans the members' status back into the group's own
// (design-ramen-integration.md §4.3). It owns no gRPC call of its own: promoting,
// demoting, or resyncing a member is the already-shipped per-volume adapter's job,
// driven by the kubernetes-csi-addons controller-manager reconciling the member
// VolumeReplication objects this reconciler creates (§4.2).
type VolumeGroupReplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumegroupreplications,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumegroupreplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumegroupreplicationclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumereplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

func (r *VolumeGroupReplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	vgr := &unstructured.Unstructured{}
	vgr.SetGroupVersionKind(volumeGroupReplicationGVK)
	if err := r.Get(ctx, req.NamespacedName, vgr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A non-external VolumeGroupReplication is the generic kubernetes-csi-addons
	// controller-manager's to reconcile, not this operator's (§4.2).
	external, _, _ := unstructured.NestedBool(vgr.Object, "spec", "external")
	if !external {
		return ctrl.Result{}, nil
	}
	className, _, _ := unstructured.NestedString(vgr.Object, "spec", "volumeGroupReplicationClassName")
	owned, err := r.ownsClass(ctx, className)
	if err != nil {
		log.Error(err, "failed to resolve VolumeGroupReplicationClass", "class", className)
		return ctrl.Result{RequeueAfter: groupReplRequeueError}, nil
	}
	if !owned {
		return ctrl.Result{}, nil
	}

	members, determinable, mismatch, err := r.resolveMembers(ctx, vgr)
	if err != nil {
		log.Error(err, "failed to resolve group membership")
		return ctrl.Result{RequeueAfter: groupReplRequeueError}, nil
	}
	if !determinable {
		// A selected PVC is not yet bound, or its volume is not yet resolvable.
		// Transient: requeue quietly, matching the webhook's fail-open disposition
		// for the identical case (design §4.4).
		return ctrl.Result{RequeueAfter: groupReplSyncInterval}, nil
	}
	if mismatch != "" {
		r.Recorder.Eventf(vgr, nil, corev1.EventTypeWarning, reasonGroupMembershipMismatch, reasonGroupMembershipMismatch, mismatch)
		return ctrl.Result{RequeueAfter: groupReplSyncInterval}, nil
	}
	r.Recorder.Eventf(vgr, nil, corev1.EventTypeNormal, reasonGroupReplicationVerified, reasonGroupReplicationVerified,
		"selector resolves to the consistency group's current membership (%d member(s))", len(members))

	replicationState, _, _ := unstructured.NestedString(vgr.Object, "spec", "replicationState")
	volumeReplicationClass, _, _ := unstructured.NestedString(vgr.Object, "spec", "volumeReplicationClassName")

	memberVRs, err := r.fanOut(ctx, vgr, members, replicationState, volumeReplicationClass)
	if err != nil {
		log.Error(err, "failed to fan out member VolumeReplications")
		return ctrl.Result{RequeueAfter: groupReplRequeueError}, nil
	}

	if err := r.fanIn(ctx, vgr, members, memberVRs); err != nil {
		log.Error(err, "failed to update VolumeGroupReplication status")
		return ctrl.Result{RequeueAfter: groupReplRequeueError}, nil
	}

	return ctrl.Result{RequeueAfter: groupReplSyncInterval}, nil
}

// ownsClass reports whether className names a VolumeGroupReplicationClass for
// this driver. A class this operator cannot read is treated as not-owned:
// the generic controller-manager or another vendor's controller may still be
// the right owner, and refusing to reconcile is safer than guessing.
func (r *VolumeGroupReplicationReconciler) ownsClass(ctx context.Context, className string) (bool, error) {
	if className == "" {
		return false, nil
	}
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(volumeGroupReplicationClassGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: className}, class); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	provisioner, _, _ := unstructured.NestedString(class.Object, "spec", "provisioner")
	return provisioner == utils.CSIProvisioner, nil
}

// resolveMembers reads spec.source.selector, resolves it to this cluster's own
// PVCs, and verifies the selected set equals a consistency group's current
// membership exactly (design §4.3 step 1, the same invariant
// design-consistency-groups.md §9.2 established for VolumeGroupSnapshot).
//
// determinable is false when a selected PVC is not yet bound, matching the
// webhook's fail-open case for the identical situation. mismatch is non-empty
// when membership is determinable but does not match.
func (r *VolumeGroupReplicationReconciler) resolveMembers(
	ctx context.Context, vgr *unstructured.Unstructured,
) (members []corev1.PersistentVolumeClaim, determinable bool, mismatch string, err error) {
	selMap, found, err := unstructured.NestedMap(vgr.Object, "spec", "source", "selector")
	if err != nil {
		return nil, true, "", fmt.Errorf("read spec.source.selector: %w", err)
	}
	if !found {
		return nil, true, "VolumeGroupReplication has no spec.source.selector", nil
	}
	var labelSelector metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selMap, &labelSelector); err != nil {
		return nil, true, "", fmt.Errorf("convert spec.source.selector: %w", err)
	}
	sel, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, true, fmt.Sprintf("invalid label selector: %v", err), nil
	}

	var pvcList corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList,
		client.InNamespace(vgr.GetNamespace()), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, true, "", fmt.Errorf("list PVCs: %w", err)
	}
	if len(pvcList.Items) == 0 {
		return nil, true, "selector matches no PersistentVolumeClaim", nil
	}

	groupName := ""
	for i := range pvcList.Items {
		val := pvcList.Items[i].Labels[consistencyGroupLabel]
		if val == "" {
			return nil, true, fmt.Sprintf("PVC %q is not labeled %s", pvcList.Items[i].Name, consistencyGroupLabel), nil
		}
		if groupName == "" {
			groupName = val
		} else if val != groupName {
			return nil, true, fmt.Sprintf("selector spans two consistency groups (%q and %q)", groupName, val), nil
		}
	}

	clusterUUID, selectedLvols, ok, err := r.selectedLvols(ctx, pvcList.Items)
	if err != nil {
		return nil, true, "", err
	}
	if !ok {
		return nil, false, "", nil
	}

	apiClient := webapi.NewClient()
	group, err := apiClient.GetConsistencyGroupByName(ctx, clusterUUID, groupName)
	if err != nil {
		return nil, true, "", err
	}
	if group == nil {
		return nil, true, fmt.Sprintf("consistency group %q not found", groupName), nil
	}
	backendMembers, err := apiClient.GetConsistencyGroupMembers(ctx, clusterUUID, group.UUID)
	if err != nil {
		return nil, true, "", err
	}
	if !sameLvolSet(selectedLvols, backendMembers) {
		return nil, true, fmt.Sprintf(
			"selector resolves to %d volume(s) but consistency group %q has %d member(s); "+
				"the selector must equal the group's current membership",
			len(selectedLvols), groupName, len(backendMembers)), nil
	}
	return pvcList.Items, true, "", nil
}

// selectedLvols maps each selected PVC to its backing lvol UUID and returns the
// shared cluster UUID. ok is false when a PVC is not yet bound, which makes
// membership undeterminable rather than mismatched.
func (r *VolumeGroupReplicationReconciler) selectedLvols(
	ctx context.Context, pvcs []corev1.PersistentVolumeClaim,
) (clusterUUID string, lvols []string, ok bool, err error) {
	for i := range pvcs {
		pvName := pvcs[i].Spec.VolumeName
		if pvName == "" {
			return "", nil, false, nil
		}
		pv := &corev1.PersistentVolume{}
		if err := r.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
			return "", nil, false, err
		}
		raw, err := kube.VolumeHandleFromPV(pv)
		if err != nil {
			return "", nil, false, nil
		}
		h, parsed := atlaslvol.ParseHandle(raw)
		if !parsed {
			return "", nil, false, nil
		}
		clusterUUID = h.ClusterID
		lvols = append(lvols, h.VolumeID)
	}
	return clusterUUID, lvols, true, nil
}

// sameLvolSet reports whether a and b hold the same set of lvol ids.
func sameLvolSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

// fanOut ensures one per-volume VolumeReplication exists for each member PVC,
// owned by the group, with spec.replicationState mirroring the group's
// (design §4.3 step 2). It never calls the driver's Replication gRPC: the
// already-shipped kubernetes-csi-addons controller-manager reconciles each
// member exactly as it does any Ramen-created per-volume VolumeReplication.
func (r *VolumeGroupReplicationReconciler) fanOut(
	ctx context.Context,
	vgr *unstructured.Unstructured,
	members []corev1.PersistentVolumeClaim,
	replicationState, volumeReplicationClass string,
) ([]unstructured.Unstructured, error) {
	owner := metav1.OwnerReference{
		APIVersion: volumeGroupReplicationGVK.GroupVersion().String(),
		Kind:       volumeGroupReplicationGVK.Kind,
		Name:       vgr.GetName(),
		UID:        vgr.GetUID(),
	}

	result := make([]unstructured.Unstructured, 0, len(members))
	for i := range members {
		pvc := &members[i]
		name := memberVolumeReplicationName(vgr.GetName(), pvc.Name)

		vr := &unstructured.Unstructured{}
		vr.SetGroupVersionKind(volumeReplicationGVK)
		err := r.Get(ctx, types.NamespacedName{Namespace: vgr.GetNamespace(), Name: name}, vr)
		switch {
		case apierrors.IsNotFound(err):
			vr.SetName(name)
			vr.SetNamespace(vgr.GetNamespace())
			vr.SetOwnerReferences([]metav1.OwnerReference{owner})
			spec := map[string]interface{}{
				"autoResync":             false,
				"replicationState":       replicationState,
				"volumeReplicationClass": volumeReplicationClass,
				"dataSource": map[string]interface{}{
					"kind": "PersistentVolumeClaim",
					"name": pvc.Name,
				},
			}
			if err := unstructured.SetNestedMap(vr.Object, spec, "spec"); err != nil {
				return nil, fmt.Errorf("build VolumeReplication %q spec: %w", name, err)
			}
			if err := r.Create(ctx, vr); err != nil {
				return nil, fmt.Errorf("create VolumeReplication %q: %w", name, err)
			}
		case err != nil:
			return nil, fmt.Errorf("get VolumeReplication %q: %w", name, err)
		default:
			if current, _, _ := unstructured.NestedString(vr.Object, "spec", "replicationState"); current != replicationState {
				if err := unstructured.SetNestedField(vr.Object, replicationState, "spec", "replicationState"); err != nil {
					return nil, fmt.Errorf("set VolumeReplication %q replicationState: %w", name, err)
				}
				if err := r.Update(ctx, vr); err != nil {
					return nil, fmt.Errorf("update VolumeReplication %q: %w", name, err)
				}
			}
		}
		result = append(result, *vr)
	}
	return result, nil
}

// memberVolumeReplicationName deterministically names a group member's
// per-volume VolumeReplication from the group and the member PVC.
func memberVolumeReplicationName(groupName, pvcName string) string {
	return groupName + "-" + pvcName
}

// fanIn aggregates every member's VolumeReplication.status.conditions into the
// group's own status (design §4.3 step 3): Completed is the conjunction across
// members, Degraded and Resyncing are the disjunction, and status.lastSyncTime
// is the oldest of the members' lastSyncTime, since a group's recovery point is
// only as fresh as its slowest member. It also emits GroupReplicationDegraded
// the first time it observes Degraded after the group previously reported it
// false, and writes status.persistentVolumeClaimsRefList to the resolved
// membership.
func (r *VolumeGroupReplicationReconciler) fanIn(
	ctx context.Context,
	vgr *unstructured.Unstructured,
	members []corev1.PersistentVolumeClaim,
	memberVRs []unstructured.Unstructured,
) error {
	wasDegraded := conditionStatus(vgr, "Degraded") == "True"

	completed := len(memberVRs) > 0
	degraded := false
	resyncing := false
	var oldestSync *time.Time
	for i := range memberVRs {
		vr := &memberVRs[i]
		if conditionStatus(vr, "Completed") != "True" {
			completed = false
		}
		if conditionStatus(vr, "Degraded") == "True" {
			degraded = true
		}
		if conditionStatus(vr, "Resyncing") == "True" {
			resyncing = true
		}
		if ts, found, _ := unstructured.NestedString(vr.Object, "status", "lastSyncTime"); found && ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				if oldestSync == nil || parsed.Before(*oldestSync) {
					oldestSync = &parsed
				}
			}
		}
	}

	now := metav1.Now()
	conditions := []interface{}{
		groupCondition("Completed", completed, now),
		groupCondition("Degraded", degraded, now),
		groupCondition("Resyncing", resyncing, now),
	}
	if err := unstructured.SetNestedSlice(vgr.Object, conditions, "status", "conditions"); err != nil {
		return fmt.Errorf("set status.conditions: %w", err)
	}
	if oldestSync != nil {
		if err := unstructured.SetNestedField(vgr.Object, oldestSync.UTC().Format(time.RFC3339), "status", "lastSyncTime"); err != nil {
			return fmt.Errorf("set status.lastSyncTime: %w", err)
		}
	}
	if completed {
		replicationState, _, _ := unstructured.NestedString(vgr.Object, "spec", "replicationState")
		if err := unstructured.SetNestedField(vgr.Object, replicationState, "status", "state"); err != nil {
			return fmt.Errorf("set status.state: %w", err)
		}
	}
	refs := make([]interface{}, 0, len(members))
	for i := range members {
		refs = append(refs, map[string]interface{}{"name": members[i].Name})
	}
	if err := unstructured.SetNestedSlice(vgr.Object, refs, "status", "persistentVolumeClaimsRefList"); err != nil {
		return fmt.Errorf("set status.persistentVolumeClaimsRefList: %w", err)
	}

	if err := r.Status().Update(ctx, vgr); err != nil {
		return err
	}
	if degraded && !wasDegraded {
		r.Recorder.Eventf(vgr, nil, corev1.EventTypeWarning, reasonGroupReplicationDegraded, reasonGroupReplicationDegraded,
			"at least one group member reports Degraded")
	}
	return nil
}

// conditionStatus returns a condition's status ("True"/"False"/"Unknown") on
// an unstructured VolumeReplication or VolumeGroupReplication, or "" if the
// object carries no condition of that type.
func conditionStatus(obj *unstructured.Unstructured, condType string) string {
	raw, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return ""
	}
	for _, c := range raw {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] == condType {
			if s, ok := cm["status"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// groupCondition builds one status.conditions entry.
func groupCondition(condType string, status bool, now metav1.Time) map[string]interface{} {
	s := "False"
	if status {
		s = "True"
	}
	return map[string]interface{}{
		"type":               condType,
		"status":             s,
		"reason":             "GroupMemberAggregation",
		"message":            "",
		"lastTransitionTime": now.UTC().Format(time.RFC3339),
	}
}

func (r *VolumeGroupReplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(volumeGroupReplicationGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(target).
		Named("volumegroupreplication").
		Complete(r)
}

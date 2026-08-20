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
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// pvcReplRequeueUnbound is how long to wait before rechecking a PVC that is not
	// yet Bound (no PV handle available yet, so we cannot resolve the volume UUID).
	pvcReplRequeueUnbound = 15 * time.Second

	// pvcReplRequeueDetaching is how long to wait while the old ReplicationSlot is
	// finishing its detach before we create the replacement slot.
	pvcReplRequeueDetaching = 5 * time.Second
)

// PVCAnnotationWatcher watches PersistentVolumeClaims for changes to the
// storage.simplyblock.io/replication-policy annotation and creates, replaces, or
// deletes ReplicationSlot CRs accordingly. It also watches StorageClasses so that
// an annotation added to a StorageClass propagates to all PVCs using it.
//
// The watcher does NOT call the simplyblock backend directly; all backend calls are
// handled by the ReplicationSlot reconciler.
type PVCAnnotationWatcher struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch

func (r *PVCAnnotationWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// PVC is being deleted: OwnerReference GC will trigger the slot's deletion, which
	// its finalizer will catch. Nothing for the watcher to do here.
	if !pvc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	desiredPolicy, err := r.resolveDesiredPolicy(ctx, &pvc)
	if err != nil {
		return ctrl.Result{}, err
	}

	currentSlot, err := r.findSlotForPVC(ctx, pvc.Namespace, pvc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	if currentSlot == nil && desiredPolicy == "" {
		return ctrl.Result{}, nil
	}

	if currentSlot != nil {
		if !currentSlot.DeletionTimestamp.IsZero() {
			log.Info("Waiting for existing ReplicationSlot to finish detaching",
				"slot", currentSlot.Name, "pvc", pvc.Name)
			return ctrl.Result{RequeueAfter: pvcReplRequeueDetaching}, nil
		}

		if currentSlot.Spec.PolicyRef == desiredPolicy {
			return ctrl.Result{}, nil
		}

		log.Info("Replication policy changed or removed; deleting existing ReplicationSlot",
			"slot", currentSlot.Name, "oldPolicy", currentSlot.Spec.PolicyRef, "newPolicy", desiredPolicy)
		if err := r.Delete(ctx, currentSlot); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete ReplicationSlot %q: %w", currentSlot.Name, err)
		}

		if desiredPolicy == "" {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{RequeueAfter: pvcReplRequeueDetaching}, nil
	}

	if desiredPolicy == "" {
		return ctrl.Result{}, nil
	}

	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		log.Info("PVC not yet Bound; waiting before creating ReplicationSlot",
			"pvc", pvc.Name, "phase", pvc.Status.Phase)
		return ctrl.Result{RequeueAfter: pvcReplRequeueUnbound}, nil
	}

	volumeID, err := r.resolveVolumeHandle(ctx, pvc.Spec.VolumeName)
	if err != nil {
		log.Error(err, "failed to resolve volume handle from PV; retrying", "pv", pvc.Spec.VolumeName)
		return ctrl.Result{RequeueAfter: pvcReplRequeueUnbound}, nil
	}

	var policy simplyblockv1alpha1.ReplicationPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: desiredPolicy, Namespace: pvc.Namespace}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("ReplicationPolicy not found; waiting", "policy", desiredPolicy, "pvc", pvc.Name)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get ReplicationPolicy %q: %w", desiredPolicy, err)
	}
	if !policy.Status.Ready {
		log.Info("ReplicationPolicy not ready yet; waiting", "policy", desiredPolicy)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.createSlot(ctx, &pvc, desiredPolicy, volumeID); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Created ReplicationSlot", "pvc", pvc.Name, "policy", desiredPolicy)
	return ctrl.Result{}, nil
}

// resolveDesiredPolicy returns the policy name for this PVC: the PVC annotation
// takes precedence over the StorageClass annotation. Returns "" when no policy is set.
func (r *PVCAnnotationWatcher) resolveDesiredPolicy(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
) (string, error) {
	if p, ok := pvc.Annotations[utils.AnnotationReplicationPolicy]; ok && p != "" {
		return p, nil
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "", nil
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get StorageClass %q: %w", *pvc.Spec.StorageClassName, err)
	}
	return sc.Annotations[utils.AnnotationReplicationPolicy], nil
}

// findSlotForPVC returns the ReplicationSlot for the given PVC, or nil if none exists.
func (r *PVCAnnotationWatcher) findSlotForPVC(
	ctx context.Context,
	namespace, pvcName string,
) (*simplyblockv1alpha1.ReplicationSlot, error) {
	var list simplyblockv1alpha1.ReplicationSlotList
	if err := r.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.pvcRef": pvcName},
	); err != nil {
		return nil, fmt.Errorf("list ReplicationSlots for PVC %q: %w", pvcName, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &list.Items[0], nil
}

// resolveVolumeHandle returns the full CSI volume handle for a PV.
func (r *PVCAnnotationWatcher) resolveVolumeHandle(ctx context.Context, pvName string) (string, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvName}, &pv); err != nil {
		return "", fmt.Errorf("get PV %q: %w", pvName, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return "", fmt.Errorf("PV %q has no CSI volume handle", pvName)
	}
	return pv.Spec.CSI.VolumeHandle, nil
}

// createSlot creates a ReplicationSlot CR for the given PVC and policy.
// The slot is named "<policy>-<pvc>" and owned by the PVC.
func (r *PVCAnnotationWatcher) createSlot(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	policyName, volumeHandle string,
) error {
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      replicationSlotName(policyName, pvc.Name),
			Namespace: pvc.Namespace,
		},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: policyName,
			PVCRef:    pvc.Name,
			VolumeID:  volumeHandle,
		},
	}
	if err := controllerutil.SetOwnerReference(pvc, slot, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on ReplicationSlot: %w", err)
	}
	if err := r.Create(ctx, slot); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ReplicationSlot %q: %w", slot.Name, err)
	}
	return nil
}

// replicationSlotName returns the deterministic name for a ReplicationSlot.
func replicationSlotName(policyName, pvcName string) string {
	return fmt.Sprintf("%s-%s", policyName, pvcName)
}

// replicationAnnotationChanged is a predicate for PVC events.
var replicationAnnotationChanged = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetAnnotations()[utils.AnnotationReplicationPolicy] !=
			e.ObjectNew.GetAnnotations()[utils.AnnotationReplicationPolicy]
	},
}

// storageClassAnnotationChanged is a predicate for StorageClass events.
var storageClassAnnotationChanged = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		_, ok := e.Object.GetAnnotations()[utils.AnnotationReplicationPolicy]
		return ok
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetAnnotations()[utils.AnnotationReplicationPolicy] !=
			e.ObjectNew.GetAnnotations()[utils.AnnotationReplicationPolicy]
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// storageClassToPVCRequests maps a StorageClass event to all PVCs that use it.
func (r *PVCAnnotationWatcher) storageClassToPVCRequests(
	ctx context.Context,
	obj client.Object,
) []ctrl.Request {
	var pvcList corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList,
		client.MatchingFields{"spec.storageClassName": obj.GetName()},
	); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(pvcList.Items))
	for _, pvc := range pvcList.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
			Name:      pvc.Name,
			Namespace: pvc.Namespace,
		}})
	}
	return reqs
}

// slotToPVCRequest maps a ReplicationSlot event back to its owning PVC so that
// slot deletions (after detach) re-trigger the watcher and allow a replacement slot
// to be created when the policy changed.
func (r *PVCAnnotationWatcher) slotToPVCRequest(
	_ context.Context,
	obj client.Object,
) []ctrl.Request {
	slot, ok := obj.(*simplyblockv1alpha1.ReplicationSlot)
	if !ok {
		return nil
	}
	if slot.Spec.PVCRef == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Name:      slot.Spec.PVCRef,
		Namespace: slot.Namespace,
	}}}
}

func (r *PVCAnnotationWatcher) SetupWithManager(mgr ctrl.Manager) error {
	// Index ReplicationSlots by spec.pvcRef for fast per-PVC lookup.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.ReplicationSlot{},
		"spec.pvcRef",
		func(obj client.Object) []string {
			slot := obj.(*simplyblockv1alpha1.ReplicationSlot)
			return []string{slot.Spec.PVCRef}
		},
	); err != nil {
		return fmt.Errorf("index ReplicationSlot.spec.pvcRef: %w", err)
	}

	// Index PVCs by spec.storageClassName so the SC → PVC mapper is O(matching PVCs).
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.PersistentVolumeClaim{},
		"spec.storageClassName",
		func(obj client.Object) []string {
			pvc := obj.(*corev1.PersistentVolumeClaim)
			if pvc.Spec.StorageClassName == nil {
				return nil
			}
			return []string{*pvc.Spec.StorageClassName}
		},
	); err != nil {
		return fmt.Errorf("index PVC.spec.storageClassName: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(replicationAnnotationChanged)).
		Named("pvcreplication").
		Watches(
			&storagev1.StorageClass{},
			handler.EnqueueRequestsFromMapFunc(r.storageClassToPVCRequests),
			builder.WithPredicates(storageClassAnnotationChanged),
		).
		Watches(
			&simplyblockv1alpha1.ReplicationSlot{},
			handler.EnqueueRequestsFromMapFunc(r.slotToPVCRequest),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Complete(r)
}

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

	// pvcReplRequeueDetaching is how long to wait while the old ReplicationPair is
	// finishing its detach before we create the replacement pair.
	pvcReplRequeueDetaching = 5 * time.Second
)

// PVCAnnotationWatcher watches PersistentVolumeClaims for changes to the
// replication.simplyblock.io/policy annotation and creates, replaces, or deletes
// ReplicationPair CRs accordingly.  It also watches StorageClasses so that an
// annotation added to a StorageClass propagates to all PVCs using it.
//
// The watcher does NOT call the simplyblock backend directly; all backend calls are
// handled by the ReplicationPair reconciler.
type PVCAnnotationWatcher struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch

func (r *PVCAnnotationWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// PVC is being deleted: OwnerReference GC will trigger the pair's deletion, which
	// its finalizer will catch. Nothing for the watcher to do here.
	if !pvc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Determine the desired policy name: PVC annotation wins over StorageClass annotation.
	desiredPolicy, err := r.resolveDesiredPolicy(ctx, &pvc)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Find the current ReplicationPair for this PVC (at most one).
	currentPair, err := r.findPairForPVC(ctx, pvc.Namespace, pvc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// No pair and no desired policy — nothing to do.
	if currentPair == nil && desiredPolicy == "" {
		return ctrl.Result{}, nil
	}

	// A pair exists: evaluate whether it needs to be replaced or removed.
	if currentPair != nil {
		// Deletion in progress — wait for the pair reconciler to finish cleanup.
		if !currentPair.DeletionTimestamp.IsZero() {
			log.Info("Waiting for existing ReplicationPair to finish detaching",
				"pair", currentPair.Name, "pvc", pvc.Name)
			return ctrl.Result{RequeueAfter: pvcReplRequeueDetaching}, nil
		}

		// Steady state: policy unchanged.
		if currentPair.Spec.PolicyRef == desiredPolicy {
			return ctrl.Result{}, nil
		}

		// Policy removed or changed: trigger detach by deleting the pair.
		// The pair's finalizer (FinalizerReplicationPair) ensures the backend
		// DELETE /vol/replication-policy runs before the CR is removed.
		log.Info("Replication policy changed or removed; deleting existing ReplicationPair",
			"pair", currentPair.Name, "oldPolicy", currentPair.Spec.PolicyRef, "newPolicy", desiredPolicy)
		if err := r.Delete(ctx, currentPair); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete ReplicationPair %q: %w", currentPair.Name, err)
		}

		// If no new policy, we're done (backend cleanup continues in the background).
		if desiredPolicy == "" {
			return ctrl.Result{}, nil
		}

		// New policy required — wait for the old pair to be GC'd before creating.
		return ctrl.Result{RequeueAfter: pvcReplRequeueDetaching}, nil
	}

	// No current pair — create one if the policy is set and the PVC is bound.
	if desiredPolicy == "" {
		return ctrl.Result{}, nil
	}

	// PVC must be Bound so we can resolve the backend volume UUID from the PV.
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		log.Info("PVC not yet Bound; waiting before creating ReplicationPair",
			"pvc", pvc.Name, "phase", pvc.Status.Phase)
		return ctrl.Result{RequeueAfter: pvcReplRequeueUnbound}, nil
	}

	volumeID, err := r.resolveVolumeHandle(ctx, pvc.Spec.VolumeName)
	if err != nil {
		log.Error(err, "failed to resolve volume handle from PV; retrying", "pv", pvc.Spec.VolumeName)
		return ctrl.Result{RequeueAfter: pvcReplRequeueUnbound}, nil
	}

	// Confirm the named ReplicationPolicy exists and is ready.
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

	// Create the ReplicationPair.
	if err := r.createPair(ctx, &pvc, desiredPolicy, volumeID); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Created ReplicationPair", "pvc", pvc.Name, "policy", desiredPolicy)
	return ctrl.Result{}, nil
}

// resolveDesiredPolicy returns the policy name for this PVC: the PVC annotation
// takes precedence over the StorageClass annotation.  Returns "" when no policy is set.
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

// findPairForPVC returns the ReplicationPair for the given PVC, or nil if none exists.
func (r *PVCAnnotationWatcher) findPairForPVC(
	ctx context.Context,
	namespace, pvcName string,
) (*simplyblockv1alpha1.ReplicationPair, error) {
	var list simplyblockv1alpha1.ReplicationPairList
	if err := r.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.pvcRef": pvcName},
	); err != nil {
		return nil, fmt.Errorf("list ReplicationPairs for PVC %q: %w", pvcName, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &list.Items[0], nil
}

// resolveVolumeHandle returns the full CSI volume handle for a PV.
// The handle format is "<clusterUUID>:<poolUUID>:<volumeUUID>".
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

// createPair creates a ReplicationPair CR for the given PVC and policy.
// The pair is named "<policy>-<pvc>" and owned by the PVC (cross-namespace GC via
// OwnerReference works because pair and PVC share the same namespace).
func (r *PVCAnnotationWatcher) createPair(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	policyName, volumeHandle string,
) error {
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name:      replicationPairName(policyName, pvc.Name),
			Namespace: pvc.Namespace,
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: policyName,
			PVCRef:    pvc.Name,
			VolumeID:  volumeHandle,
		},
	}
	// OwnedBy PVC: deleting the PVC cascades deletion to the pair, which the
	// pair's finalizer converts into a backend detach before the CR is removed.
	if err := controllerutil.SetOwnerReference(pvc, pair, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on ReplicationPair: %w", err)
	}
	if err := r.Create(ctx, pair); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ReplicationPair %q: %w", pair.Name, err)
	}
	return nil
}

// replicationPairName returns the deterministic name for a ReplicationPair.
func replicationPairName(policyName, pvcName string) string {
	return fmt.Sprintf("%s-%s", policyName, pvcName)
}

// replicationAnnotationChanged is a predicate that wakes the controller only when
// the replication policy annotation is added, changed, or removed on a PVC.
var replicationAnnotationChanged = predicate.Funcs{
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

// storageClassAnnotationChanged is a predicate for StorageClass events that filters
// to only annotation changes on the replication policy annotation.
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

// storageClassToPVCRequests maps a StorageClass event to all PVCs that use it,
// so that a SC annotation change propagates to those PVCs immediately.
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

// pairToPVCRequest maps a ReplicationPair event back to its owning PVC so that
// pair deletions (e.g. pair fully GC'd after detach) re-trigger the watcher
// and allow a replacement pair to be created when the policy changed.
func (r *PVCAnnotationWatcher) pairToPVCRequest(
	_ context.Context,
	obj client.Object,
) []ctrl.Request {
	pair, ok := obj.(*simplyblockv1alpha1.ReplicationPair)
	if !ok {
		return nil
	}
	if pair.Spec.PVCRef == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Name:      pair.Spec.PVCRef,
		Namespace: pair.Namespace,
	}}}
}

func (r *PVCAnnotationWatcher) SetupWithManager(mgr ctrl.Manager) error {
	// Index ReplicationPairs by spec.pvcRef for fast per-PVC lookup.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.ReplicationPair{},
		"spec.pvcRef",
		func(obj client.Object) []string {
			pair := obj.(*simplyblockv1alpha1.ReplicationPair)
			return []string{pair.Spec.PVCRef}
		},
	); err != nil {
		return fmt.Errorf("index ReplicationPair.spec.pvcRef: %w", err)
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
		// Watch StorageClasses: annotation change on a SC propagates to all PVCs using it.
		Watches(
			&storagev1.StorageClass{},
			handler.EnqueueRequestsFromMapFunc(r.storageClassToPVCRequests),
			builder.WithPredicates(storageClassAnnotationChanged),
		).
		// Watch ReplicationPairs: pair deletion (after detach) re-triggers the watcher
		// so a replacement pair is created if the annotation changed to a new policy.
		Watches(
			&simplyblockv1alpha1.ReplicationPair{},
			handler.EnqueueRequestsFromMapFunc(r.pairToPVCRequest),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Complete(r)
}

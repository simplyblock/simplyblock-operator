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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// VolumeCache is the read surface the reconciler needs from the volume
// subscription: a trigger stream (each event naming a LogicalVolume CR), a
// by-id lookup of desired state, and a per-scope synced check. It keeps the
// reconciler independent of how volumes are retrieved and cached.
type VolumeCache interface {
	// Triggers is the reconcile-trigger stream; each event names a LogicalVolume CR.
	Triggers() <-chan event.GenericEvent
	// Lookup returns the desired volume and its scope, or ok=false if the
	// control plane no longer has it.
	Lookup(volumeID string) (cpinformer.Scope, subscriptions.VolumeDTO, bool)
	// Synced reports whether a scope's initial snapshot has been applied.
	Synced(scope cpinformer.Scope) bool
}

// LogicalVolumeReconciler mirrors control-plane volumes into LogicalVolume CRs.
// It is triggered per-CR — by the subscription (a volume changed) and by the CR
// itself (drift/startup) — reads desired state from the cache (never the
// control-plane API), and writes through a workqueue, so the SSE stream is
// unaffected by API latency or write failures.
type LogicalVolumeReconciler struct {
	client.Client
	Volumes VolumeCache
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=logicalvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=logicalvolumes/status,verbs=get;update;patch

// SetupWithManager watches LogicalVolume CRs (for drift and to enumerate stale
// CRs at startup) and the subscription's trigger stream (for control-plane
// changes). Both enqueue a LogicalVolume CR to reconcile.
func (r *LogicalVolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.LogicalVolume{}).
		Named("logicalvolume").
		WatchesRawSource(source.Channel(r.Volumes.Triggers(), &handler.EnqueueRequestForObject{})).
		Complete(r)
}

// Reconcile converges one LogicalVolume CR toward the cache's view of its
// control-plane volume: create/update while the volume exists, delete once it is
// gone. A CR whose volume is absent from a not-yet-synced scope is left alone
// (the cache is not authoritative until its snapshot arrives).
func (r *LogicalVolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	volumeID := strings.TrimPrefix(req.Name, simplyblockv1alpha1.LogicalVolumeNamePrefix)
	scope, dto, inCache := r.Volumes.Lookup(volumeID)

	var lv simplyblockv1alpha1.LogicalVolume
	err := r.Get(ctx, req.NamespacedName, &lv)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	switch {
	case inCache && len(scope) == 2:
		return ctrl.Result{}, r.upsert(ctx, req.NamespacedName, scope, dto, exists, &lv)

	case exists:
		// Volume gone from the control plane → remove the mirror, but only once
		// its scope is synced (else the cache is merely cold, not authoritative).
		crScope := cpinformer.Scope{lv.Spec.ClusterID, lv.Spec.PoolID}
		if !r.Volumes.Synced(crScope) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if err := r.Delete(ctx, &lv); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil

	default:
		return ctrl.Result{}, nil // nothing cached and no CR — nothing to do
	}
}

// upsert creates or updates the mirror CR at key to reflect dto.
func (r *LogicalVolumeReconciler) upsert(ctx context.Context, key client.ObjectKey, scope cpinformer.Scope, dto subscriptions.VolumeDTO, exists bool, lv *simplyblockv1alpha1.LogicalVolume) error {
	spec := simplyblockv1alpha1.LogicalVolumeSpec{ClusterID: scope[0], PoolID: scope[1], VolumeID: dto.ID}
	status := simplyblockv1alpha1.LogicalVolumeStatus{
		Name:      dto.Name,
		PoolName:  dto.PoolName,
		SizeBytes: dto.Size,
		NQN:       dto.NQN,
		State:     dto.Status,
	}

	if !exists {
		*lv = simplyblockv1alpha1.LogicalVolume{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Spec:       spec,
		}
		if err := r.Create(ctx, lv); err != nil {
			return err
		}
	} else if lv.Spec != spec {
		lv.Spec = spec
		if err := r.Update(ctx, lv); err != nil {
			return err
		}
	}

	status.ObservedGeneration = lv.Generation
	if lv.Status != status {
		lv.Status = status
		return r.Status().Update(ctx, lv)
	}
	return nil
}

// the volume subscription satisfies the read surface this reconciler needs.
var _ VolumeCache = (*subscriptions.VolumeSubscription)(nil)

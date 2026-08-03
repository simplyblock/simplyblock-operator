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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	lvCluster = "11111111-1111-1111-1111-111111111111"
	lvPool    = "22222222-2222-2222-2222-222222222222"
	lvVolume  = "33333333-3333-3333-3333-333333333333"
)

func lvScope() cpinformer.Scope { return cpinformer.Scope{lvCluster, lvPool} }

// fakeCache is a static VolumeCache for reconciler tests.
type fakeCache struct {
	synced  bool
	volumes map[string]subscriptions.VolumeDTO
}

func (f *fakeCache) Triggers() <-chan event.GenericEvent { return nil }
func (f *fakeCache) Synced(cpinformer.Scope) bool        { return f.synced }
func (f *fakeCache) Lookup(id string) (cpinformer.Scope, subscriptions.VolumeDTO, bool) {
	dto, ok := f.volumes[id]
	if !ok {
		return nil, subscriptions.VolumeDTO{}, false
	}
	return lvScope(), dto, true
}

func lvReconciler(t *testing.T, cache VolumeCache, objs ...client.Object) *LogicalVolumeReconciler {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme)
	c := newTestClient(t, scheme, []client.Object{&simplyblockv1alpha1.LogicalVolume{}}, objs...)
	return &LogicalVolumeReconciler{Client: c, Volumes: cache}
}

func lvReq(id string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "sb", Name: simplyblockv1alpha1.LogicalVolumeName(id)}}
}

func getLV(t *testing.T, c client.Client, id string) (*simplyblockv1alpha1.LogicalVolume, error) {
	t.Helper()
	var lv simplyblockv1alpha1.LogicalVolume
	err := c.Get(context.Background(), lvReq(id).NamespacedName, &lv)
	return &lv, err
}

func TestLogicalVolumeReconcileCreatesAndUpdates(t *testing.T) {
	cache := &fakeCache{synced: true, volumes: map[string]subscriptions.VolumeDTO{
		lvVolume: {ID: lvVolume, Name: "vol1", Size: 4096, Status: "online"},
	}}
	r := lvReconciler(t, cache)

	if _, err := r.Reconcile(context.Background(), lvReq(lvVolume)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	lv, err := getLV(t, r.Client, lvVolume)
	if err != nil {
		t.Fatalf("expected mirror CR: %v", err)
	}
	if lv.Spec.ClusterID != lvCluster || lv.Spec.PoolID != lvPool || lv.Spec.VolumeID != lvVolume {
		t.Errorf("spec = %+v", lv.Spec)
	}
	if lv.Status.SizeBytes != 4096 || lv.Status.State != "online" {
		t.Errorf("status = %+v", lv.Status)
	}

	cache.volumes[lvVolume] = subscriptions.VolumeDTO{ID: lvVolume, Name: "vol1", Size: 8192, Status: "resizing"}
	if _, err := r.Reconcile(context.Background(), lvReq(lvVolume)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	lv, _ = getLV(t, r.Client, lvVolume)
	if lv.Status.SizeBytes != 8192 || lv.Status.State != "resizing" {
		t.Errorf("status not updated: %+v", lv.Status)
	}
}

func TestLogicalVolumeReconcileDeletesWhenGoneAndSynced(t *testing.T) {
	existing := &simplyblockv1alpha1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: simplyblockv1alpha1.LogicalVolumeName(lvVolume)},
		Spec:       simplyblockv1alpha1.LogicalVolumeSpec{ClusterID: lvCluster, PoolID: lvPool, VolumeID: lvVolume},
	}
	// Volume absent from the cache, scope synced → the CR is deleted.
	r := lvReconciler(t, &fakeCache{synced: true, volumes: map[string]subscriptions.VolumeDTO{}}, existing)

	if _, err := r.Reconcile(context.Background(), lvReq(lvVolume)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getLV(t, r.Client, lvVolume); !apierrors.IsNotFound(err) {
		t.Errorf("expected CR deleted, got %v", err)
	}
}

func TestLogicalVolumeReconcileWaitsForSyncBeforeDeleting(t *testing.T) {
	existing := &simplyblockv1alpha1.LogicalVolume{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: simplyblockv1alpha1.LogicalVolumeName(lvVolume)},
		Spec:       simplyblockv1alpha1.LogicalVolumeSpec{ClusterID: lvCluster, PoolID: lvPool, VolumeID: lvVolume},
	}
	// Volume absent from a NOT-yet-synced cache → the CR must be left alone.
	r := lvReconciler(t, &fakeCache{synced: false, volumes: map[string]subscriptions.VolumeDTO{}}, existing)

	res, err := r.Reconcile(context.Background(), lvReq(lvVolume))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while the scope is not synced")
	}
	if _, err := getLV(t, r.Client, lvVolume); err != nil {
		t.Errorf("CR must not be deleted before sync: %v", err)
	}
}

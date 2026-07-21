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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// A StorageNode carrying the adopt-uuid annotation must adopt the relocated backend
// UUID into status WITHOUT issuing a fresh node-add (PostedAt stays nil).
func TestReconcile_AdoptsMigratedUUID_NoProvision(t *testing.T) {
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	cluster := testCluster(snTestNS, snTestCluster, "cluster-uuid")

	sn := newStorageNode("sn-target", snTestNS, "sns", migrateTargetWorker)
	sn.Finalizers = []string{storageNodeFinalizer}
	sn.Annotations = map[string]string{
		simplyblockv1alpha1.AnnotationAdoptUUID:        opsTestNodeUUID,
		simplyblockv1alpha1.AnnotationMigrationPending: migrationPendingValue,
	}
	// status.UUID empty — the adoption path must fill it.

	r := newSNReconciler(t, sn, sns, cluster)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "sn-target", Namespace: snTestNS},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-target", Namespace: snTestNS}, &updated)
	if updated.Status.UUID != opsTestNodeUUID {
		t.Errorf("status.UUID = %q want %q (adoption failed)", updated.Status.UUID, opsTestNodeUUID)
	}
	if updated.Status.PostedAt != nil {
		t.Error("PostedAt must be nil: adoption must not trigger a node-add POST")
	}
}

// A StorageNode annotated migrated-away must NOT create a drain/remove ops on
// deletion — the backend node was relocated, not removed — and its finalizer must
// be dropped immediately.
func TestHandleDeletion_SkipsDrainWhenMigratedAway(t *testing.T) {
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	sn := newStorageNode("sn-origin", snTestNS, "sns", snTestWorker)
	sn.Finalizers = []string{storageNodeFinalizer}
	sn.Annotations = map[string]string{simplyblockv1alpha1.AnnotationMigratedAway: migratedAwayValue}
	sn.Status.UUID = opsTestNodeUUID
	sn.Status.Status = "online"
	now := metav1.Now()
	sn.DeletionTimestamp = &now

	r := newSNReconciler(t, sn, sns)

	if _, err := r.handleDeletion(context.Background(), sn, sns); err != nil {
		t.Fatalf("handleDeletion returned error: %v", err)
	}

	// No StorageNodeOps(remove) should have been created.
	var opsList simplyblockv1alpha1.StorageNodeOpsList
	_ = r.List(context.Background(), &opsList)
	if len(opsList.Items) != 0 {
		t.Errorf("expected no drain/remove ops for a migrated-away node, got %d", len(opsList.Items))
	}

	// Finalizer must have been removed.
	var updated simplyblockv1alpha1.StorageNode
	if err := r.Get(context.Background(), types.NamespacedName{Name: "sn-origin", Namespace: snTestNS}, &updated); err == nil {
		for _, f := range updated.Finalizers {
			if f == storageNodeFinalizer {
				t.Error("finalizer should have been removed for a migrated-away node")
			}
		}
	}
}

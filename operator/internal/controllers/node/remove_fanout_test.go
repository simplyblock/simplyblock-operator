// A drain's fan-out, and what carries it.
//
// The drain decides which volumes move where. Which kind carries a move is the
// deployment's, and the difference between the two is not cosmetic: a namespaced
// VolumeMigration can be owned by the operation that raised it, and a
// cluster-scoped PersistentVolumeOps cannot — Kubernetes treats a namespaced
// owner of a cluster-scoped object as unresolvable and garbage-collects the
// dependent, which here would delete the migration mid-copy. The creator moves
// into the spec instead, and the cascade becomes this controller's own.
//
// design-storagenode.md §8.4 and design-persistentvolumeops.md §11.1.

package node

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

const (
	// aDrainedNodeID is the backend node the drain is emptying, which is also
	// what its fan-out is labeled by.
	aDrainedNodeID = "node-uuid"
	// aPeerNodeID is where the volumes go.
	aPeerNodeID = "peer-uuid"
	// aDrainUID is what separates this drain from a later one of the same name.
	aDrainUID = "ops-uid"
)

// aFanOut builds a reconciler whose world holds the drained node, a peer to
// move to, and the volume being moved.
func aFanOut(t *testing.T, mover func(client.Client, *runtime.Scheme) vmigration.Mover) (
	*StorageNodeOpsReconciler, client.Client,
) {
	t.Helper()
	scheme := testsupport.NewScheme(t)

	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "a-node", Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "a-cluster"},
	}
	node.Status.UUID = aDrainedNodeID

	peer := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "a-peer", Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: "a-cluster"},
	}
	peer.Status.UUID = aPeerNodeID

	ops := aRemoveOps()
	ops.UID = aDrainUID

	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, peer, ops).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNodeOps{}).
		Build()

	return &StorageNodeOpsReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      goneControlPlane{},
		Mover:    mover(apiClient, scheme),
	}, apiClient
}

// TestTheFanOutRecordsItsCreatorWithoutOwningTheOperation. The cluster-scoped
// kind cannot be owned, so the drain that raised a move has to be findable from
// the move itself: by the label, which is what a List selects on, and by the
// UID, which is what separates one drain from a later drain of the same name.
func TestTheFanOutRecordsItsCreatorWithoutOwningTheOperation(t *testing.T) {
	r, apiClient := aFanOut(t, func(c client.Client, s *runtime.Scheme) vmigration.Mover {
		return vmigration.NewMover(c, s, false)
	})

	err := r.createMigration(context.Background(), aRemoveOpsWithUID(), aDrainedNodeID,
		managedVolume{PVName: "pv-1", VolumeUUID: "volume-uuid"}, aPeerNodeID)
	if err != nil {
		t.Fatalf("raising the move: %v", err)
	}

	var operations simplyblockv1alpha2.PersistentVolumeOpsList
	if err := apiClient.List(context.Background(), &operations); err != nil {
		t.Fatal(err)
	}
	if len(operations.Items) != 1 {
		t.Fatalf("raised %d operations, want 1", len(operations.Items))
	}
	ops := operations.Items[0]

	if len(ops.OwnerReferences) != 0 {
		t.Errorf("the operation carries owner references %v, and a namespaced owner of a "+
			"cluster-scoped object is garbage-collected", ops.OwnerReferences)
	}
	if ops.Spec.CreatorRef == nil {
		t.Fatal("nothing records which drain raised the move")
	}
	if ops.Spec.CreatorRef.UID != aDrainUID {
		t.Errorf("the creator's UID is %q, so a drain recreated under the same name would "+
			"inherit a fan-out it did not issue", ops.Spec.CreatorRef.UID)
	}
	if ops.Spec.Migrate == nil || ops.Spec.Migrate.TargetNodeRef.Name != "a-peer" {
		t.Errorf("the move's target is %+v, want the StorageNode reporting peer-uuid",
			ops.Spec.Migrate)
	}
	if ops.Labels[drainNodeLabel] != aDrainedNodeID {
		t.Errorf("labels = %v, want the one a List selects the fan-out by", ops.Labels)
	}
}

// TestTheLegacyFanOutIsStillOwnedByItsDrain. With the registered kind turned
// back on, the move is namespaced and the owner reference is what cascades, as
// it always did.
func TestTheLegacyFanOutIsStillOwnedByItsDrain(t *testing.T) {
	r, apiClient := aFanOut(t, func(c client.Client, s *runtime.Scheme) vmigration.Mover {
		return vmigration.NewMover(c, s, true)
	})

	err := r.createMigration(context.Background(), aRemoveOpsWithUID(), aDrainedNodeID,
		managedVolume{PVName: "pv-1", VolumeUUID: "volume-uuid"}, aPeerNodeID)
	if err != nil {
		t.Fatalf("raising the move: %v", err)
	}

	var migrations simplyblockv1alpha1.VolumeMigrationList
	if err := apiClient.List(context.Background(), &migrations); err != nil {
		t.Fatal(err)
	}
	if len(migrations.Items) != 1 {
		t.Fatalf("raised %d migrations, want 1", len(migrations.Items))
	}
	migration := migrations.Items[0]

	if len(migration.OwnerReferences) != 1 || migration.OwnerReferences[0].UID != aDrainUID {
		t.Errorf("owner references = %v, want the drain that raised it",
			migration.OwnerReferences)
	}
	if migration.Spec.TargetNodeUUID != "peer-uuid" {
		t.Errorf("target = %q, want the backend UUID the registered kind takes",
			migration.Spec.TargetNodeUUID)
	}
}

// TestTheFanOutIsFoundAgainByItsLabel, which is what makes the drain's progress
// count and its cascade possible at all.
func TestTheFanOutIsFoundAgainByItsLabel(t *testing.T) {
	for name, legacy := range map[string]bool{"PersistentVolumeOps": false, "VolumeMigration": true} {
		t.Run(name, func(t *testing.T) {
			r, _ := aFanOut(t, func(c client.Client, s *runtime.Scheme) vmigration.Mover {
				return vmigration.NewMover(c, s, legacy)
			})
			ops := aRemoveOpsWithUID()

			if err := r.createMigration(context.Background(), ops, aDrainedNodeID,
				managedVolume{PVName: "pv-1", VolumeUUID: "volume-uuid"}, aPeerNodeID); err != nil {
				t.Fatal(err)
			}

			moves, err := r.migrationsOf(context.Background(), ops, aDrainedNodeID)
			if err != nil {
				t.Fatal(err)
			}
			if len(moves) != 1 {
				t.Fatalf("found %d moves, want the one that was raised", len(moves))
			}
			if moves[0].PVName != "pv-1" {
				t.Errorf("the move names volume %q", moves[0].PVName)
			}
		})
	}
}

// aRemoveOpsWithUID is the drain the fan-out is attributed to.
func aRemoveOpsWithUID() *simplyblockv1alpha2.StorageNodeOps {
	ops := aRemoveOps()
	ops.UID = aDrainUID
	return ops
}

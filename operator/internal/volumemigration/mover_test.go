// The two kinds a volume move can be raised as, behind one interface, and the
// properties that have to hold for either of them.
//
// The tests are written against the interface rather than against each
// implementation, because what the three callers depend on is that a move can
// be started, found again, read for an outcome, and removed. Which kind carries
// it is the deployment's choice.

package volumemigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	moveClusterID = "11111111-1111-1111-1111-111111111111"
	movePoolID    = "22222222-2222-2222-2222-222222222222"
	moveVolumeID  = "33333333-3333-3333-3333-333333333333"
	moveTargetID  = "55555555-5555-5555-5555-555555555555"
	movePVName    = "pvc-" + moveVolumeID
	moveNodeName  = "worker-5"
)

func moverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		k8sscheme.AddToScheme,
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the scheme: %v", err)
		}
	}
	return s
}

// moveWorld is what either kind needs to exist: the volume, and the node the
// move is aimed at.
func moveWorld() []client.Object {
	return []client.Object{
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: movePVName},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver: "csi.simplyblock.io",
						VolumeHandle: string(lvol.NewVolumeHandle(
							moveClusterID, movePoolID, moveVolumeID)),
					},
				},
			},
		},
		&simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: moveNodeName, Namespace: testNamespace},
			Status:     simplyblockv1alpha2.StorageNodeStatus{UUID: moveTargetID},
		},
	}
}

func bothMovers(t *testing.T, objs ...client.Object) map[string]Mover {
	t.Helper()
	s := moverScheme(t)
	build := func() client.Client {
		return fake.NewClientBuilder().WithScheme(s).
			WithStatusSubresource(
				&simplyblockv1alpha1.VolumeMigration{},
				&simplyblockv1alpha2.PersistentVolumeOps{},
			).
			WithObjects(append(moveWorld(), objs...)...).Build()
	}
	return map[string]Mover{
		"PersistentVolumeOps": &OperationMover{Client: build()},
		"VolumeMigration":     &MigrationMover{Client: build()},
	}
}

func moveRequest() MoveRequest {
	return MoveRequest{
		Name:           "move-1",
		Namespace:      testNamespace,
		PVName:         movePVName,
		TargetNodeUUID: moveTargetID,
		Labels:         map[string]string{"drain-node": "node-a"},
	}
}

// TestAStartedMoveIsFoundAgainByItsLabel. Every caller raises a move and then
// looks for it later, by the label it tagged it with: the rebalancer to track
// completion, the drain to count its fan-out.
func TestAStartedMoveIsFoundAgainByItsLabel(t *testing.T) {
	for kind, mover := range bothMovers(t) {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			if err := mover.Start(ctx, moveRequest()); err != nil {
				t.Fatalf("starting the move: %v", err)
			}

			moves, err := mover.List(ctx, testNamespace, map[string]string{"drain-node": "node-a"})
			if err != nil {
				t.Fatal(err)
			}
			if len(moves) != 1 {
				t.Fatalf("found %d moves, want the one that was started: %+v", len(moves), moves)
			}
			if moves[0].PVName != movePVName {
				t.Errorf("the move names volume %q, want %q", moves[0].PVName, movePVName)
			}
			if moves[0].Phase != MovePending {
				t.Errorf("a move nothing has reconciled is %q, want Pending", moves[0].Phase)
			}
		})
	}
}

// TestStartingTheSameMoveTwiceIsNotTwoMoves. A caller that crashed between
// creating the move and recording it retries, and two moves of one volume would
// be two backend migrations copying it to two places.
func TestStartingTheSameMoveTwiceIsNotTwoMoves(t *testing.T) {
	for kind, mover := range bothMovers(t) {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			if err := mover.Start(ctx, moveRequest()); err != nil {
				t.Fatal(err)
			}
			if err := mover.Start(ctx, moveRequest()); err != nil {
				t.Fatalf("starting an existing move reported an error: %v", err)
			}

			moves, err := mover.List(ctx, testNamespace, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(moves) != 1 {
				t.Errorf("found %d moves, want 1", len(moves))
			}
		})
	}
}

// TestAMovesOutcomeReadsTheSameForEitherKind. The registered kind reaches
// Completed and the redesigned one reaches Succeeded, and a caller that had to
// know which would be a caller the gate is not hiding anything from.
func TestAMovesOutcomeReadsTheSameForEitherKind(t *testing.T) {
	ctx := context.Background()

	t.Run("VolumeMigration", func(t *testing.T) {
		for _, tc := range []struct {
			phase simplyblockv1alpha1.VolumeMigrationPhase
			want  MovePhase
		}{
			{simplyblockv1alpha1.VolumeMigrationPhaseCompleted, MoveSucceeded},
			{simplyblockv1alpha1.VolumeMigrationPhaseFailed, MoveFailed},
			{simplyblockv1alpha1.VolumeMigrationPhaseAborted, MoveAborted},
			{simplyblockv1alpha1.VolumeMigrationPhaseRunning, MoveRunning},
			{simplyblockv1alpha1.VolumeMigrationPhaseValidating, MoveRunning},
		} {
			t.Run(string(tc.phase), func(t *testing.T) {
				existing := &simplyblockv1alpha1.VolumeMigration{
					ObjectMeta: metav1.ObjectMeta{Name: "move-1", Namespace: testNamespace},
					Spec:       simplyblockv1alpha1.VolumeMigrationSpec{PVName: movePVName},
					Status:     simplyblockv1alpha1.VolumeMigrationStatus{Phase: tc.phase},
				}
				mover := bothMovers(t, existing)["VolumeMigration"]

				got, err := mover.Get(ctx, "move-1", testNamespace)
				if err != nil {
					t.Fatal(err)
				}
				if got.Phase != tc.want {
					t.Errorf("phase %q reads as %q, want %q", tc.phase, got.Phase, tc.want)
				}
			})
		}
	})

	t.Run("PersistentVolumeOps", func(t *testing.T) {
		for _, tc := range []struct {
			phase simplyblockv1alpha2.PersistentVolumeOpsPhase
			want  MovePhase
		}{
			{simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded, MoveSucceeded},
			{simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, MoveFailed},
			{simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted, MoveAborted},
			{simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning, MoveRunning},
			{simplyblockv1alpha2.PersistentVolumeOpsPhasePending, MovePending},
		} {
			t.Run(string(tc.phase), func(t *testing.T) {
				existing := &simplyblockv1alpha2.PersistentVolumeOps{
					ObjectMeta: metav1.ObjectMeta{Name: "move-1"},
					Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
						PersistentVolumeName: movePVName,
						Action:               simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
					},
					Status: simplyblockv1alpha2.PersistentVolumeOpsStatus{Phase: tc.phase},
				}
				mover := bothMovers(t, existing)["PersistentVolumeOps"]

				got, err := mover.Get(ctx, "move-1", testNamespace)
				if err != nil {
					t.Fatal(err)
				}
				if got.Phase != tc.want {
					t.Errorf("phase %q reads as %q, want %q", tc.phase, got.Phase, tc.want)
				}
			})
		}
	})
}

// TestTheOperationNamesTheTargetAsAnObject. The redesigned kind takes a
// StorageNode name rather than a backend UUID, so that a migration can be
// written by hand without looking one up. The callers hold a UUID, so this is
// where the two are joined.
func TestTheOperationNamesTheTargetAsAnObject(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(moverScheme(t)).WithObjects(moveWorld()...).Build()
	mover := &OperationMover{Client: c}

	if err := mover.Start(ctx, moveRequest()); err != nil {
		t.Fatal(err)
	}

	moves, err := mover.List(ctx, testNamespace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 {
		t.Fatalf("found %d moves, want 1", len(moves))
	}
	if moves[0].Namespace != "" {
		t.Errorf("the move is in namespace %q, and the kind is cluster-scoped", moves[0].Namespace)
	}

	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := c.Get(ctx, client.ObjectKey{Name: "move-1"}, &ops); err != nil {
		t.Fatal(err)
	}
	if ops.Spec.Migrate == nil || ops.Spec.Migrate.TargetNodeRef.Name != moveNodeName {
		t.Errorf("the operation's target is %+v, want the StorageNode object", ops.Spec.Migrate)
	}
	if ops.Spec.Migrate.TargetNodeRef.Namespace != testNamespace {
		t.Errorf("the target carries namespace %q", ops.Spec.Migrate.TargetNodeRef.Namespace)
	}
}

// TestAnOperationForANodeNobodyReportsIsRefused. A backend node UUID with no
// StorageNode reporting it cannot be named as an object, and guessing would
// produce an operation the webhook then refuses with a worse message.
func TestAnOperationForANodeNobodyReportsIsRefused(t *testing.T) {
	mover := bothMovers(t)["PersistentVolumeOps"]

	request := moveRequest()
	request.TargetNodeUUID = "99999999-9999-9999-9999-999999999999"

	if err := mover.Start(context.Background(), request); err == nil {
		t.Fatal("a move to a node nothing reports was accepted")
	}
}

// TestADeletedMoveIsGone, which is how every caller reaps a finished one.
func TestADeletedMoveIsGone(t *testing.T) {
	for kind, mover := range bothMovers(t) {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			if err := mover.Start(ctx, moveRequest()); err != nil {
				t.Fatal(err)
			}
			moves, err := mover.List(ctx, testNamespace, nil)
			if err != nil {
				t.Fatal(err)
			}

			if err := mover.Delete(ctx, moves[0]); err != nil {
				t.Fatal(err)
			}
			// Deleting one that is already gone is the state being asked for.
			if err := mover.Delete(ctx, moves[0]); err != nil {
				t.Errorf("deleting an absent move reported an error: %v", err)
			}
		})
	}
}

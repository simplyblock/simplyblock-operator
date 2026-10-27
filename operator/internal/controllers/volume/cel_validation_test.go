// Validation of the CEL rules compiled into the PersistentVolumeOps CRD schema,
// run against a real apiserver.
//
// It lives here rather than under internal/webhook because there is no webhook
// involved: the rules are enforced by the apiserver itself, and envtest is the
// only place in the tree that starts one. The suite installs CRDs and nothing
// else, so a rejection here can only have come from the schema.
//
// What the schema owns and the webhook does not is every rule that is a
// statement about this object's own fields: the agreement between spec.action
// and its parameter block, and the immutability of everything but spec.abort.
// The webhook is left with the rows that are facts about a different object —
// the volume, the target node — which CEL cannot reach
// (design-persistentvolumeops.md §4.3).

package volume

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// migrateOperation is a well-formed operation, which every case below starts
// from and breaks in exactly one way.
func migrateOperation() *simplyblockv1alpha2.PersistentVolumeOps {
	return &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-"},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: "pvc-0001",
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
			Migrate: &simplyblockv1alpha2.MigrateVolumeSpec{
				TargetNodeRef: simplyblockv1alpha2.StorageNodeReference{
					Namespace: "simplyblock",
					Name:      "worker-3",
				},
			},
		},
	}
}

// TestPersistentVolumeOpsCELRequiresTheActionsParameterBlock. An action with no
// parameters is an operation with no target node, which would reach the
// controller and fail there, one reconcile later and with a worse message.
// Every Ops kind in the group states the pairing on the type for that reason
// (design-storagebackup.md uses the same rule).
func TestPersistentVolumeOpsCELRequiresTheActionsParameterBlock(t *testing.T) {
	apiClient := apiServer(t)

	t.Run("Migrate with its block is accepted", func(t *testing.T) {
		ops := migrateOperation()
		if err := apiClient.Create(context.Background(), ops); err != nil {
			t.Fatalf("a well-formed operation was rejected: %v", err)
		}
		t.Cleanup(func() { _ = apiClient.Delete(context.Background(), ops) })
	})

	t.Run("Migrate without its block is rejected", func(t *testing.T) {
		ops := migrateOperation()
		ops.Spec.Migrate = nil

		err := apiClient.Create(context.Background(), ops)
		if err == nil {
			t.Fatal("the apiserver accepted a Migrate with no target node")
		}
		if !strings.Contains(err.Error(), "migrate is required for action Migrate") {
			t.Fatalf("rejected for the wrong reason: %v", err)
		}
	})
}

// TestPersistentVolumeOpsCELFreezesEverythingButAbort. An operation that
// changed what it was doing halfway through would have a status describing
// neither, and the one field a user is meant to change after the fact is the
// request to stop.
func TestPersistentVolumeOpsCELFreezesEverythingButAbort(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		edit       func(*simplyblockv1alpha2.PersistentVolumeOps)
		wantDenied bool
	}{
		{
			name: "abort is the one mutable field",
			edit: func(ops *simplyblockv1alpha2.PersistentVolumeOps) { ops.Spec.Abort = true },
		},
		{
			name:       "the volume cannot be repointed",
			edit:       func(ops *simplyblockv1alpha2.PersistentVolumeOps) { ops.Spec.PersistentVolumeName = "pvc-0002" },
			wantDenied: true,
		},
		{
			name: "the target node cannot be repointed",
			edit: func(ops *simplyblockv1alpha2.PersistentVolumeOps) {
				ops.Spec.Migrate.TargetNodeRef.Name = "worker-4"
			},
			wantDenied: true,
		},
		{
			// The marker sits on the reference rather than on its fields, so
			// the pair moves together or not at all: a target half-changed
			// would name a node in one cluster and a namespace in another.
			name: "the target node's namespace cannot be repointed either",
			edit: func(ops *simplyblockv1alpha2.PersistentVolumeOps) {
				ops.Spec.Migrate.TargetNodeRef.Namespace = "elsewhere"
			},
			wantDenied: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ops := migrateOperation()
			if err := apiClient.Create(ctx, ops); err != nil {
				t.Fatalf("creating the operation: %v", err)
			}
			t.Cleanup(func() { _ = apiClient.Delete(ctx, ops) })

			tc.edit(ops)
			err := apiClient.Update(ctx, ops)
			if tc.wantDenied {
				if err == nil {
					t.Fatal("the apiserver accepted an edit to a frozen field")
				}
				if !strings.Contains(err.Error(), "immutable") {
					t.Fatalf("rejected for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the apiserver rejected the one field a user may change: %v", err)
			}
		})
	}
}

// TestPersistentVolumeOpsCELRejectsAnUndeclaredStep. status.step carries the
// shared statemachine snapshot, whose state field no Enum marker can reach, so
// the rule on the status is what an Enum would have done. Without it a
// hand-edited or downgraded object could name a step this kind has no graph
// for, and the controller would fail to resume it one reconcile later.
func TestPersistentVolumeOpsCELRejectsAnUndeclaredStep(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	ops := migrateOperation()
	if err := apiClient.Create(ctx, ops); err != nil {
		t.Fatalf("creating the operation: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, ops) })

	for _, tc := range []struct {
		step       string
		wantDenied bool
	}{
		{step: "Validating"},
		{step: "Migrating"},
		{step: "Verifying"},
		{step: "Suspending", wantDenied: true},
	} {
		t.Run(tc.step, func(t *testing.T) {
			fresh := &simplyblockv1alpha2.PersistentVolumeOps{}
			if err := apiClient.Get(ctx, types.NamespacedName{Name: ops.Name}, fresh); err != nil {
				t.Fatalf("reading the operation back: %v", err)
			}
			base := fresh.DeepCopy()
			fresh.Status.Step.State = tc.step

			err := apiClient.Status().Patch(ctx, fresh, client.MergeFrom(base))
			if tc.wantDenied {
				if err == nil {
					t.Fatalf("the apiserver accepted step %q, which this kind has no graph for", tc.step)
				}
				if !strings.Contains(err.Error(), "unknown step") {
					t.Fatalf("rejected for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the apiserver rejected declared step %q: %v", tc.step, err)
			}
		})
	}
}

// relocateOperation is a well-formed Relocate.
func relocateOperation() *simplyblockv1alpha2.PersistentVolumeOps {
	return &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-"},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: "pvc-0001",
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionRelocate,
			Relocate:             &simplyblockv1alpha2.RelocateVolumeSpec{TargetClusterRef: "prod-w2-nrh"},
		},
	}
}

// U-73 and its pair: Relocate requires spec.relocate, and the blocks of the
// two actions are not interchangeable.
func TestPersistentVolumeOpsCELPairsRelocateWithItsBlock(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		edit func(*simplyblockv1alpha2.PersistentVolumeOps)
		want string
	}{
		{name: "Relocate with its block is accepted"},
		{
			name: "Relocate without its block is rejected",
			edit: func(ops *simplyblockv1alpha2.PersistentVolumeOps) { ops.Spec.Relocate = nil },
			want: "relocate is required for action Relocate",
		},
		{
			name: "Relocate carrying a migrate block is rejected",
			edit: func(ops *simplyblockv1alpha2.PersistentVolumeOps) {
				ops.Spec.Migrate = migrateOperation().Spec.Migrate
			},
			want: "migrate is required for action Migrate and must be absent otherwise",
		},
		{
			name: "Migrate carrying a relocate block is rejected",
			edit: func(ops *simplyblockv1alpha2.PersistentVolumeOps) {
				ops.Spec.Action = simplyblockv1alpha2.PersistentVolumeOpsActionMigrate
				ops.Spec.Migrate = migrateOperation().Spec.Migrate
			},
			want: "relocate is required for action Relocate and must be absent otherwise",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := relocateOperation()
			if tc.edit != nil {
				tc.edit(ops)
			}
			err := apiClient.Create(ctx, ops)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a well-formed Relocate was rejected: %v", err)
				}
				t.Cleanup(func() { _ = apiClient.Delete(ctx, ops) })
				return
			}
			if err == nil {
				_ = apiClient.Delete(ctx, ops)
				t.Fatal("the apiserver accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rejected for the wrong reason: %v", err)
			}
		})
	}
}

// U-72: the target member is frozen with the rest of the request.
func TestPersistentVolumeOpsCELFreezesTheTargetMember(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()
	ops := relocateOperation()
	if err := apiClient.Create(ctx, ops); err != nil {
		t.Fatalf("creating the operation: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, ops) })

	ops.Spec.Relocate.TargetClusterRef = "prod-w3-ols"
	err := apiClient.Update(ctx, ops)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("Update = %v, want the target member refused as immutable", err)
	}
}

// The Relocate steps are declared on the status, as Migrate's are.
func TestPersistentVolumeOpsCELAcceptsTheRelocateSteps(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()
	ops := relocateOperation()
	if err := apiClient.Create(ctx, ops); err != nil {
		t.Fatalf("creating the operation: %v", err)
	}
	t.Cleanup(func() { _ = apiClient.Delete(ctx, ops) })

	for _, step := range []string{"Replicating", "Committing", "AttachingPaths", "Proceeding"} {
		fresh := &simplyblockv1alpha2.PersistentVolumeOps{}
		if err := apiClient.Get(ctx, types.NamespacedName{Name: ops.Name}, fresh); err != nil {
			t.Fatalf("reading the operation back: %v", err)
		}
		base := fresh.DeepCopy()
		fresh.Status.Step.State = step
		if err := apiClient.Status().Patch(ctx, fresh, client.MergeFrom(base)); err != nil {
			t.Errorf("the apiserver rejected declared step %q: %v", step, err)
		}
	}
}

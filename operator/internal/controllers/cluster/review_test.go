// The findings of the review on #536, each as the test that would have caught
// it.
//
// Four of them share a shape: a value that is absent, empty, or refused is
// read as an answer. An empty secret is written as though it were a
// credential, a conflicted patch is reported as a release, and an abort is
// honored from a step that has left the node down. None of them fails loudly;
// each leaves something in a state nothing afterward corrects.

package cluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// The two steps between a node's shutdown and its restart are not abortable.
// Stopping there leaves the node offline with nothing driving it back up,
// which is the one outcome the abort table exists to prevent.
func TestAnAbortIsRefusedWhileTheNodeIsDown(t *testing.T) {
	for _, current := range []step{stepShuttingDownNode, stepRefreshingPod, stepAwaitingPod,
		stepRestartingNode} {
		t.Run(string(current), func(t *testing.T) {
			if abortable(current) {
				t.Errorf("step %s is abortable, but the node is offline there and the "+
					"walk is what brings it back", current)
			}
		})
	}
}

// The abort is honored where the walk has not taken anything down: before the
// shutdown, and after the node is back online and the cluster is settling.
func TestAnAbortIsHonoredWhereNothingIsDown(t *testing.T) {
	for _, current := range []step{stepCheckingPeers, stepRebalancing} {
		t.Run(string(current), func(t *testing.T) {
			if !abortable(current) {
				t.Errorf("step %s is not abortable, though it has taken nothing down", current)
			}
		})
	}
}

// A conflicted release is not a release. A concurrent write to the cluster's
// status can make the optimistic patch conflict while activeOpsRef still names
// this operation, and reporting success then lets the caller finish and the
// finalizer go, leaving the cluster locked by an object that no longer exists.
func TestAConflictedReleaseIsReportedRatherThanSwallowed(t *testing.T) {
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionShutdown)
	held := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.ActiveOpsRef = testOpsName
	})

	conflicting := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&simplyblockv1alpha2.StorageCluster{},
			&simplyblockv1alpha2.StorageClusterOps{},
		).
		WithObjects(held, ops).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				_ context.Context, _ client.Client, _ string,
				obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*simplyblockv1alpha2.StorageCluster); ok {
					return apierrors.NewConflict(
						schema.GroupResource{Resource: "storageclusters"},
						obj.GetName(), context.Canceled)
				}
				return nil
			},
		}).
		Build()

	r := &StorageClusterOpsReconciler{
		Client:   conflicting,
		Scheme:   testScheme(t),
		Recorder: &recorder{},
		API:      &fakeControlPlane{t: t},
	}

	if err := r.releaseLock(context.Background(), ops); err == nil {
		t.Error("a conflicted release reported success, so the caller will drop the " +
			"finalizer and leave the cluster locked forever")
	}
}

// Adoption by name reads the cluster list, whose secret is write-only, so it
// carries no credential. Persisting that empty value overwrites the entry the
// CSI driver reaches the cluster through, and marks the cluster configured
// while nothing can provision from it.
func TestAdoptionByNameDoesNotPersistAnEmptyCredential(t *testing.T) {
	api := &fakeControlPlane{
		create: func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
			return webapi.ClusterResponse{}, &ControlPlaneError{
				Status: 409, Body: "a cluster of that name exists",
			}
		},
		byName: func(string) (utils.ClusterListEntry, bool, error) {
			// What the list actually returns: no secret, because the field is
			// write-only in the control plane's own schema.
			return utils.ClusterListEntry{
				UUID: testClusterUUID, Name: testClusterName,
				Status: utils.ClusterStatusActive, NDCS: 2, NPCS: 1,
			}, true, nil
		},
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 8)

	// Either the adoption held, or it completed with a real credential. What
	// it must not do is record an empty one and call the cluster configured.
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: testNamespace, Name: csiCredentialsSecret}
	err := r.Get(context.Background(), key, &secret)
	if err == nil && len(secret.Data["secret.json"]) > 0 {
		if !containsSecretValue(string(secret.Data["secret.json"])) {
			t.Error("the CSI credentials entry was written with an empty cluster secret, " +
				"so the driver cannot reach the adopted cluster")
		}
	}
	if cluster.Status.Configured && cluster.Status.UUID != "" {
		var perCluster corev1.Secret
		perKey := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "simplyblock-cluster-" + testClusterName,
		}
		if err := r.Get(context.Background(), perKey, &perCluster); err == nil {
			if len(perCluster.Data["secret"]) == 0 {
				t.Error("the cluster was marked configured with an empty per-cluster " +
					"secret, so nothing can authenticate against it")
			}
		}
	}
}

// containsSecretValue reports whether the aggregate entry carries a non-empty
// cluster secret.
func containsSecretValue(payload string) bool {
	return !contains(payload, `"cluster_secret": ""`)
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// A backup store with no bucket is not a location. The registered v1alpha1
// type had no bucket at all, so every upgraded cluster converts to one, and
// sending it onward asks the control plane to write copies nowhere in
// particular.
func TestABackupStoreWithNoBucketIsRefused(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: objectMeta("backup-credentials"),
		Data: map[string][]byte{
			"access_key_id":     []byte("the-key"),
			"secret_access_key": []byte("the-secret"),
		},
	}
	cluster := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{
			Endpoint:             "https://203.0.113.20:9000",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "backup-credentials"},
		}
	})
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{}, cluster, secret)

	if _, err := r.backupConfig(context.Background(), cluster); err == nil {
		t.Error("a backup store with no bucket was accepted, so the control plane is " +
			"asked to write copies to an unnamed location")
	}
}

// The step a machine is restored into has to be one its graph declares. A
// creation machine restored from a legacy lowercase step would fail to
// resume, which is what the conversion's normalization exists to prevent.
func TestTheCreationMachineRestoresFromEveryDeclaredStep(t *testing.T) {
	ctx := context.Background()
	for _, s := range []simplyblockv1alpha2.StorageClusterStep{
		simplyblockv1alpha2.StorageClusterStepClaiming,
		simplyblockv1alpha2.StorageClusterStepCheckingControlPlane,
		simplyblockv1alpha2.StorageClusterStepResolvingConfig,
		simplyblockv1alpha2.StorageClusterStepCreating,
		simplyblockv1alpha2.StorageClusterStepAdopting,
		simplyblockv1alpha2.StorageClusterStepPersisting,
	} {
		t.Run(string(s), func(t *testing.T) {
			machine, err := statemachine.NewFromSnapshot(ctx, creationGraph(),
				statemachine.Snapshot[simplyblockv1alpha2.StorageClusterStep]{State: s})
			if err != nil {
				t.Fatalf("the creation machine cannot resume at %s: %v", s, err)
			}
			defer machine.Close()
		})
	}

	// The legacy spelling is not one of them, which is why the conversion has
	// to map it rather than copy it.
	machine, err := statemachine.NewFromSnapshot(ctx, creationGraph(),
		statemachine.Snapshot[simplyblockv1alpha2.StorageClusterStep]{State: "creating"})
	if err == nil {
		machine.Close()
		t.Error("the creation machine accepted the legacy lowercase step, so the " +
			"conversion's normalization is untested")
	}
}

// A step is persisted together with the deadline that bounds it.
//
// Writing the state and then the deadline is two patches with a window
// between them, and a process that dies in that window restores a step with
// no deadline at all. TimeoutReached is false for such a step forever, so the
// operation neither advances on its own nor ever reports the deadline failure
// that is the only thing standing between a wedged control plane and an
// operation that runs for good. The hook that sets the deadline is pure:
// every OnEnter in the graphs returns a duration and does nothing else, so
// there is nothing here for a first patch to run ahead of, and the
// write-ahead record the next pass needs is the step itself.
//
// Seeing the window takes looking at every write rather than at the last one,
// which is what the interceptor is for.
func TestAStepIsNeverPersistedWithoutItsDeadline(t *testing.T) {
	var unbounded []string
	watching := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&simplyblockv1alpha2.StorageCluster{},
			&simplyblockv1alpha2.StorageClusterOps{},
		).
		WithObjects(newTestCluster(), newTestOps(
			simplyblockv1alpha2.StorageClusterOpsActionActivate)).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, c client.Client, subResource string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if ops, ok := obj.(*simplyblockv1alpha2.StorageClusterOps); ok {
					if s := ops.Status.Step; s.State != "" && s.Deadline == nil {
						unbounded = append(unbounded, s.State)
					}
				}
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &StorageClusterOpsReconciler{
		Client:   watching,
		Scheme:   testScheme(t),
		Recorder: &recorder{},
		API: &fakeControlPlane{
			t:       t,
			cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		},
	}

	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	for i := 0; i < 6; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile pass %d: %v", i+1, err)
		}
	}

	if len(unbounded) > 0 {
		t.Errorf("a step was persisted with no deadline (%v); a crash there restores an "+
			"operation nothing can ever time out", unbounded)
	}
}

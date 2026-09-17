// Tests for the StorageCluster reconciler: the creation machine, the three
// routes into adoption, the steady-state reading, and deletion.
//
// Creating a backend cluster is not idempotent, so the property worth most
// here is that one object produces one cluster whatever order the passes
// arrive in. The claim that makes that true is an optimistic-lock patch, and a
// fake client honors resourceVersion, so the 409 path is provable without a
// real apiserver.
//
// The other property with teeth is deletion: a control plane that refuses the
// delete must leave the finalizer in place, because removing it orphans a
// cluster with data on it and nothing in Kubernetes will ever name it again.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

func newClusterReconciler(
	t *testing.T, api *fakeControlPlane, rec *recorder, objects ...client.Object,
) *StorageClusterReconciler {
	t.Helper()
	if api != nil {
		api.t = t
	}
	return &StorageClusterReconciler{
		Client:    newClient(t, objects...),
		Scheme:    testScheme(t),
		Recorder:  rec,
		API:       api,
		Namespace: testNamespace,
	}
}

// reconcileCluster runs n passes and returns the cluster as it stands
// afterward. The creation machine advances one step per pass, so a test that
// wants a created cluster says how many that took.
func reconcileCluster(
	t *testing.T, r *StorageClusterReconciler, n int,
) *simplyblockv1alpha2.StorageCluster {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile pass %d: %v", i+1, err)
		}
	}
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, key, &cluster); err != nil {
		t.Fatalf("read the cluster back: %v", err)
	}
	return &cluster
}

// newUncreatedCluster is a cluster object with no backend cluster behind it,
// which is where the creation machine starts.
func newUncreatedCluster(
	mutate ...func(*simplyblockv1alpha2.StorageCluster),
) *simplyblockv1alpha2.StorageCluster {
	c := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status = simplyblockv1alpha2.StorageClusterStatus{}
	})
	for _, m := range mutate {
		m(c)
	}
	return c
}

// ---------------------------------------------------------------------------
// Creation
// ---------------------------------------------------------------------------

func TestACreatedClusterReachesSteadyState(t *testing.T) {
	api := &fakeControlPlane{
		create: func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
			reading := activeCluster()
			reading.Secret = testClusterSecret
			return reading, nil
		},
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != testClusterUUID {
		t.Fatalf("status.uuid = %q, want the cluster the control plane made", cluster.Status.UUID)
	}
	if cluster.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseOnline {
		t.Errorf("phase = %q, want Online", cluster.Status.Phase)
	}
	if cluster.Status.ErasureCodingScheme != "2x1" {
		t.Errorf("erasureCodingScheme = %q, want 2x1", cluster.Status.ErasureCodingScheme)
	}
	if !cluster.Status.Configured {
		t.Error("status.configured is false on a created cluster")
	}
	if api.createCalls != 1 {
		t.Errorf("the control plane was asked to create %d clusters, want 1", api.createCalls)
	}

	// The CSI driver's aggregate entry is what lets it reach the cluster at
	// all, so a created cluster that is not in it is a cluster no volume can
	// be provisioned from.
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: testNamespace, Name: csiCredentialsSecret}
	if err := r.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("the CSI credentials secret was not written: %v", err)
	}
	if len(secret.Data["secret.json"]) == 0 {
		t.Error("the CSI credentials secret carries no entry")
	}
}

// spec.deviceClass is the CRD's spelling of what sbcli's cluster-create wire
// format calls `device_mode`, so the two must map onto each other rather than
// the field simply passing through unmapped.
func TestCreationParamsMapDeviceClassToTheWireDeviceMode(t *testing.T) {
	tests := []struct {
		name    string
		class   simplyblockv1alpha2.StorageClusterDeviceClass
		wantAPI string
	}{
		{"defaulted NVMe", "", "nvme"},
		{"explicit NVMe", simplyblockv1alpha2.StorageClusterDeviceClassNVMe, "nvme"},
		{"LogicalBlock", simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock, "lblk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotParams utils.ClusterAddParams
			api := &fakeControlPlane{
				create: func(params utils.ClusterAddParams) (webapi.ClusterResponse, error) {
					gotParams = params
					reading := activeCluster()
					reading.Secret = testClusterSecret
					return reading, nil
				},
				cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
			}
			cluster := newUncreatedCluster(func(c *simplyblockv1alpha2.StorageCluster) {
				c.Spec.DeviceClass = tt.class
			})
			r := newClusterReconciler(t, api, &recorder{}, cluster)

			reconcileCluster(t, r, 6)

			if gotParams.DeviceMode != tt.wantAPI {
				t.Errorf("device_mode = %q, want %q", gotParams.DeviceMode, tt.wantAPI)
			}
		})
	}
}

// The claim is the mutex. It is an optimistic-lock patch, so a second
// reconciler holding a stale copy of the object is refused and backs off
// rather than posting a second cluster.
func TestAStaleReconcilerCannotClaimTheCreationTwice(t *testing.T) {
	api := &fakeControlPlane{
		create: func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
			// A creation response carries the secret the control plane just
			// minted for the cluster. Only the list response omits it.
			reading := activeCluster()
			reading.Secret = testClusterSecret
			return reading, nil
		},
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newUncreatedCluster())
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}

	// A reconciler that read the object before anybody claimed it.
	var stale simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, key, &stale); err != nil {
		t.Fatalf("read the cluster: %v", err)
	}

	// Somebody else claims it.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The stale reconciler's claim is refused as a back-off rather than as an
	// error, which is why claim returns only a result: a 409 means somebody
	// else owns the creation, and that is an outcome rather than a failure.
	if result := r.claim(ctx, &stale); result.RequeueAfter == 0 {
		t.Error("a refused claim did not back off")
	}
	if api.createCalls != 0 {
		t.Errorf("a refused claim posted %d clusters", api.createCalls)
	}
}

// A control plane that is not ready holds the machine at CheckingControlPlane
// and says so, rather than posting into a backend that cannot accept it.
func TestAControlPlaneThatIsNotReadyHoldsTheCreation(t *testing.T) {
	api := &fakeControlPlane{ready: func() error { return errors.New("fdb is not up") }}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 4)
	if got := cluster.Status.Step.State; got != string(
		simplyblockv1alpha2.StorageClusterStepCheckingControlPlane) {
		t.Errorf("step = %q, want the machine holding at CheckingControlPlane", got)
	}
	if api.createCalls != 0 {
		t.Errorf("a cluster was posted into a control plane that is not ready")
	}
	if !rec.has(FDBNotReady) {
		t.Error("a control plane that is not ready emitted no FDBNotReady")
	}
}

// The first route into adoption: the Secret that migrates a Helm deployment.
// Nothing is posted, and the cluster the Secret names is taken over.
func TestAnUpgradeSecretAdoptsRatherThanCreating(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(id string) (webapi.ClusterResponse, error) {
			if id != testClusterUUID {
				t.Errorf("the upgrade secret's cluster was not read, %q was", id)
			}
			return activeCluster(), nil
		},
	}
	upgrade := &corev1.Secret{
		ObjectMeta: objectMeta("simplyblock-" + testClusterName + "-upgrade"),
		Data: map[string][]byte{
			"uuid":   []byte(testClusterUUID),
			"secret": []byte(testClusterSecret),
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newUncreatedCluster(), upgrade)

	cluster := reconcileCluster(t, r, 6)
	if cluster.Status.UUID != testClusterUUID {
		t.Fatalf("status.uuid = %q, want the adopted cluster (step %q, message %q)",
			cluster.Status.UUID, cluster.Status.Step.State, cluster.Status.Message)
	}
	if api.createCalls != 0 {
		t.Errorf("an adoption posted %d clusters", api.createCalls)
	}
	if !rec.has(ClusterAdopted) {
		t.Error("an adoption emitted no ClusterAdopted, so it is indistinguishable from a creation")
	}
}

// The second route: a POST that failed against a cluster which already exists.
// That covers two reconciles that both passed the claim on different
// resourceVersions, and a response lost after the backend committed.
func TestAFailedPostAdoptsAClusterThatAlreadyExists(t *testing.T) {
	api := &fakeControlPlane{
		create: func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
			return webapi.ClusterResponse{}, &ControlPlaneError{
				Status: http.StatusConflict, Body: "a cluster of that name exists",
			}
		},
		byName: func(string) (utils.ClusterListEntry, bool, error) {
			return utils.ClusterListEntry{
				UUID: testClusterUUID, Secret: testClusterSecret,
				Name: testClusterName, Status: utils.ClusterStatusActive,
				NDCS: 2, NPCS: 1,
			}, true, nil
		},
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 8)
	if cluster.Status.UUID != testClusterUUID {
		t.Fatalf("status.uuid = %q, want the existing cluster adopted (step %q, message %q)",
			cluster.Status.UUID, cluster.Status.Step.State, cluster.Status.Message)
	}
	if !rec.has(ClusterAdopted) {
		t.Error("an adoption emitted no ClusterAdopted")
	}
}

// A refusal with nothing behind it is reported with the control plane's own
// status and body, so the cause is in `kubectl describe` rather than in the
// operator's log.
func TestARefusedCreationCarriesTheControlPlanesAnswer(t *testing.T) {
	api := &fakeControlPlane{
		create: func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
			return webapi.ClusterResponse{}, &ControlPlaneError{
				Status: http.StatusBadRequest, Body: "distr_ndcs must be positive",
			}
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 5)
	if cluster.Status.UUID != "" {
		t.Errorf("status.uuid = %q, want nothing recorded for a refused creation",
			cluster.Status.UUID)
	}
	if !rec.has(ClusterCreationFailed) {
		t.Error("a refused creation emitted no ClusterCreationFailed")
	}
}

// A backup store whose Secret is missing is a configuration error the user can
// fix, and the creation waits at ResolvingConfig rather than posting a cluster
// with no credentials.
func TestAMissingBackupSecretHoldsTheCreation(t *testing.T) {
	api := &fakeControlPlane{}
	withBackup := newUncreatedCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.Backup = &simplyblockv1alpha2.BackupStoreSpec{
			Endpoint:             "https://s3.example.com",
			Bucket:               "simplyblock-backups",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "missing"},
		}
	})
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, withBackup)

	cluster := reconcileCluster(t, r, 5)
	if api.createCalls != 0 {
		t.Errorf("a cluster was posted with unresolved backup credentials")
	}
	if got := cluster.Status.Step.State; got != string(
		simplyblockv1alpha2.StorageClusterStepResolvingConfig) {
		t.Errorf("step = %q, want the machine holding at ResolvingConfig", got)
	}
	if !rec.has(BackupCredentialsError) {
		t.Error("an unresolvable backup secret emitted no BackupCredentialsError")
	}
}

// ---------------------------------------------------------------------------
// Steady state
// ---------------------------------------------------------------------------

// The mapping this file used to assert here lives in phase_test.go, which covers
// every status rather than six of them. Two tables for one function drift.

// status.tasks is a window on the present: only running and pending tasks are
// in it, newest first, and never more than twenty.
func TestTheTaskWindowHoldsOnlyWhatIsRunning(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		tasks: func(string) ([]subscriptions.TaskDTO, error) {
			tasks := []subscriptions.TaskDTO{
				{ID: "running-1", Type: "balancing_on_restart", Status: "running"},
				{ID: "done-1", Type: "lvol_migration", Status: "done"},
				{ID: "canceled-1", Type: "device_restart", Status: "running", Canceled: true},
			}
			// More than the cap, so the window is exercised rather than just
			// the filter. Suspended is not finished: it is a task waiting.
			for i := 0; i < 25; i++ {
				tasks = append(tasks, subscriptions.TaskDTO{
					ID: fmt.Sprintf("pending-%02d", i), Type: "lvol_migration", Status: "suspended",
				})
			}
			return tasks, nil
		},
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())

	cluster := reconcileCluster(t, r, 1)
	if len(cluster.Status.Tasks) != maxTasks {
		t.Fatalf("status.tasks holds %d entries, want the cap of %d",
			len(cluster.Status.Tasks), maxTasks)
	}
	for _, task := range cluster.Status.Tasks {
		if task.ID == "done-1" || task.ID == "canceled-1" {
			t.Errorf("a finished task is still in the window: %s", task.ID)
		}
	}
	// The order is the control plane's own, because its TaskDTO carries no
	// date. What the cap keeps is therefore the head of what it reported.
	if cluster.Status.Tasks[0].ID != "running-1" {
		t.Errorf("the window reordered the control plane's list: %q is at the head",
			cluster.Status.Tasks[0].ID)
	}
}

// A task that leaves the list finished between two readings, and the event is
// what remains of it. The list is a window and the events are the history.
func TestATaskLeavingTheWindowEmitsAnEvent(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		tasks:   func(string) ([]subscriptions.TaskDTO, error) { return nil, nil },
	}
	rec := &recorder{}
	withTask := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.Tasks = []simplyblockv1alpha2.ClusterTask{
			{ID: "task-1", Type: "rebalance", Status: "running"},
		}
	})
	r := newClusterReconciler(t, api, rec, withTask)

	cluster := reconcileCluster(t, r, 1)
	if len(cluster.Status.Tasks) != 0 {
		t.Errorf("status.tasks still holds %d entries", len(cluster.Status.Tasks))
	}
	if !rec.has(TaskCompleted) {
		t.Error("a task that left the window emitted no TaskCompleted")
	}
}

// A control plane that cannot be asked leaves the recorded list alone, because
// an empty list means "nothing is running" and a failed read does not say
// that.
func TestAFailedTaskReadLeavesTheWindowAlone(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		tasks: func(string) ([]subscriptions.TaskDTO, error) {
			return nil, errors.New("connection refused")
		},
	}
	withTask := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.Tasks = []simplyblockv1alpha2.ClusterTask{
			{ID: "task-1", Type: "rebalance", Status: "running"},
		}
	})
	r := newClusterReconciler(t, api, &recorder{}, withTask)

	cluster := reconcileCluster(t, r, 1)
	if len(cluster.Status.Tasks) != 1 {
		t.Errorf("status.tasks holds %d entries, want the last reading kept",
			len(cluster.Status.Tasks))
	}
}

// The effective restart concurrency is the smaller of what the spec asks for
// and what the cluster's fault tolerance allows, published so tooling reads
// one authoritative number rather than recomputing it.
func TestTheEffectiveRestartLimitIsClampedToTheFaultTolerance(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	asked := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.MaxConcurrentWorkerRestarts = ptr.To(int32(4))
	})
	r := newClusterReconciler(t, api, &recorder{}, asked)

	cluster := reconcileCluster(t, r, 1)
	if got := cluster.Status.MaxConcurrentWorkerRestarts; got == nil || *got != 1 {
		t.Errorf("status.maxConcurrentWorkerRestarts = %v, want it clamped to the "+
			"fault tolerance of 1", got)
	}
}

// ---------------------------------------------------------------------------
// Deletion
// ---------------------------------------------------------------------------

// A control plane that refuses the delete blocks the object rather than
// orphaning the cluster behind it.
func TestARefusedDeleteKeepsTheFinalizer(t *testing.T) {
	api := &fakeControlPlane{
		deleteCall: func(string) error { return errors.New("the control plane is unreachable") },
	}
	now := metav1.Now()
	deleting := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.DeletionTimestamp = &now
	})
	r := newClusterReconciler(t, api, &recorder{}, deleting)

	cluster := reconcileCluster(t, r, 1)
	if len(cluster.Finalizers) == 0 {
		t.Fatal("the finalizer was removed while the backend cluster still exists")
	}
	if api.deleteCalls != 1 {
		t.Errorf("the control plane was asked to delete %d times, want 1", api.deleteCalls)
	}
}

// Both finalizer spellings are removed. The rename is the one change in this
// migration that wedges rather than degrades: removing only the new key would
// leave every object an older operator created in Terminating forever.
func TestDeletionRemovesBothFinalizerSpellings(t *testing.T) {
	api := &fakeControlPlane{}
	now := metav1.Now()
	deleting := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.DeletionTimestamp = &now
		c.Finalizers = []string{LegacyFinalizer, Finalizer}
	})
	r := newClusterReconciler(t, api, &recorder{}, deleting)

	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// A fake client deletes the object once the last finalizer goes, so its
	// absence is what "both were removed" looks like from here.
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, key, &cluster); err == nil && len(cluster.Finalizers) > 0 {
		t.Errorf("finalizers = %v, want both spellings removed", cluster.Finalizers)
	}
	if api.deleteCalls != 1 {
		t.Errorf("the control plane was asked to delete %d times, want 1", api.deleteCalls)
	}
}

// A cluster that was never created has nothing to delete, and the finalizer is
// removed without the control plane being asked anything.
func TestDeletingAnUncreatedClusterAsksTheControlPlaneNothing(t *testing.T) {
	api := &fakeControlPlane{}
	now := metav1.Now()
	deleting := newUncreatedCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.DeletionTimestamp = &now
	})
	r := newClusterReconciler(t, api, &recorder{}, deleting)

	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testClusterName}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if api.deleteCalls != 0 {
		t.Errorf("a cluster that was never created was deleted %d times", api.deleteCalls)
	}
}

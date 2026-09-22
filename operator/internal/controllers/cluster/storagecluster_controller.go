// The StorageCluster reconciler: four paths, which are creation, adoption,
// steady-state synchronization, and deletion.
//
// The object is fetched with a direct read rather than from the informer
// cache. A cached read can still return an empty status.uuid immediately after
// a status patch has persisted one, and acting on that stale value is a second
// POST and a second backend cluster.
//
// Creating a backend cluster is not idempotent, so the claim is made in
// Kubernetes before the control plane is touched. The mutex is the
// optimistic-lock patch rather than the value it writes: the patch succeeds for
// exactly one reconciler at a given resourceVersion and returns 409 to the
// rest, so persisting the transition into Claiming is what makes creation
// single-shot. Any persisted field would serve as the token; making it the step
// means the field also says something.
//
// design-storagecluster.md §4 is the specification.

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	atlasnet "github.com/simplyblock/atlas/net"
	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// Finalizer is the group's `<lowercased kind>-finalizer` spelling. The
	// older `cluster-finalizer` is read as well for a release, because an
	// operator that reads only this key would leave every object an older one
	// created in Terminating forever (§4.5).
	Finalizer = "storage.simplyblock.io/storagecluster-finalizer"

	// LegacyFinalizer is the spelling that shipped. It is removed alongside
	// this one on deletion and is never added.
	LegacyFinalizer = "storage.simplyblock.io/cluster-finalizer"

	// csiCredentialsSecret is the aggregate entry the CSI driver reads: one
	// Secret in the operator's namespace carrying every cluster's endpoint and
	// secret.
	csiCredentialsSecret = "simplyblock-csi-secret-v2"

	// clusterRetry is how long the controller waits before looking again at
	// something it cannot hurry: a control plane that is not ready, a Secret
	// that does not resolve, a delete the backend refused.
	clusterRetry = 20 * time.Second

	// clusterResync is the slow backstop for a reading the control plane does
	// not push. It is never how a change is discovered once the cluster stream
	// exists (§4.4); until then it is how every steady-state reading arrives.
	clusterResync = 30 * time.Second

	// maxTasks is what status.tasks holds. Twenty is a window onto a cluster's
	// work rather than a record of it: the list's length tracks concurrency,
	// so a cluster that has run ten thousand tasks has as short a list as one
	// that has run ten (§3.4).
	maxTasks = 20

	// creationStepDeadline bounds each step of the creation machine, so a
	// control plane that never becomes ready is a step that expires rather
	// than a reconcile that repeats forever.
	creationStepDeadline = 10 * time.Minute

	// annotationBackupBucket is where a bucket lives on an object still
	// authored as v1alpha1, which cannot express the field. It is the
	// conversion key of design-api-upgrade.md §6.2, named here only so the
	// event that reports an empty bucket can tell an administrator where to
	// put one; nothing in this package reads it.
	annotationBackupBucket = "storage.simplyblock.io/conversion-spec.backup.bucket"
)

// StorageClusterReconciler reconciles a StorageCluster.
type StorageClusterReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	API       ControlPlane
	Namespace string

	// Clusters is the cluster stream's cache, which is where every
	// steady-state reading comes from (§4.4). It is root-scoped, so no
	// reconciler opens it; it is optional, and a deployment without the
	// control-plane informer reads the control plane directly.
	Clusters ClusterCache

	// Tasks is the task stream's cache, which status.tasks is a window on
	// (§3.4).
	Tasks TaskCache

	// NodeScopes, if set, receives this cluster's scope so the control-plane
	// SSE manager streams its storage nodes. The stream is per cluster and
	// carries every node of it, so the cluster is what opens and closes it
	// rather than any one node.
	NodeScopes *cpinformer.ScopeSet

	// TaskScopes is the task stream's, opened and closed with the node
	// stream's and for the same reason: a task belongs to a cluster and the
	// cluster is the only object that knows when one exists.
	TaskScopes *cpinformer.ScopeSet

	// BackupScopes are the data-protection band's streams, which are per
	// cluster for the same reason the node stream is. They are a slice because
	// backups and backup policies are two streams opened and closed together:
	// a cluster with one and not the other would report copies nobody
	// scheduled or a schedule nothing reports on.
	BackupScopes []*cpinformer.ScopeSet

	// BackupRegistrars learn which object a backend cluster id belongs to.
	//
	// The band needs it and the node stream does not, and the difference is
	// where the object goes: a device object is created beside the StorageNode
	// that owns it, whose namespace that node's own registration carries,
	// while a backup object has no owner and belongs beside this cluster.
	// Nothing else knows both ids at once, so this reconciler is where the
	// mapping is made.
	BackupRegistrars []ClusterRegistrar
}

// ClusterRegistrar records which StorageCluster object a backend cluster id
// belongs to. The cluster, task, and backup subscriptions all implement it;
// the interface is declared here so that this package does not depend on which
// of them does.
//
// Every one of them needs it for the same reason: a stream event carries a
// control-plane id, and the object it has to be turned into lives in a
// namespace only the operator knows. This reconciler is where the two ids meet.
type ClusterRegistrar interface {
	RegisterCluster(clusterID string, cluster types.NamespacedName)
	UnregisterCluster(clusterID string)
}

// ClusterCache is the part of the cluster subscription this package reads.
//
// It is declared here rather than imported as a concrete type because an
// interface belongs to its consumer, and because a test needs to answer it
// without a stream.
type ClusterCache interface {
	// Lookup returns the cached cluster with the given backend id, or
	// ok=false when the control plane no longer reports it.
	Lookup(clusterID string) (subscriptions.ClusterDTO, bool)

	// SyncedRoot reports whether the one stream has delivered its snapshot.
	// Until it has, a miss is an absence of information rather than evidence
	// that the cluster is gone.
	SyncedRoot() bool

	// Triggers names the StorageCluster to reconcile when the stream moves.
	Triggers() <-chan event.GenericEvent
}

// TaskCache is the part of the task subscription this package reads.
type TaskCache interface {
	// List returns every task of a cluster, which is what the window is built
	// from.
	List(scope cpinformer.Scope) []subscriptions.TaskDTO

	// Lookup returns one task by id, which is what a CancelTask operation
	// waits on.
	Lookup(taskID string) (subscriptions.TaskDTO, bool)

	// Synced reports whether the cluster's tasks have been snapshotted. An
	// empty unsynced cache and a cluster with nothing running look identical.
	Synced(scope cpinformer.Scope) bool

	// Triggers names the StorageCluster whose window moved.
	Triggers() <-chan event.GenericEvent
}

// CSICredentials is the aggregate Secret the CSI driver reads.
type CSICredentials struct {
	Clusters []CSIClusterEntry `json:"clusters"`
}

// CSIClusterEntry is one cluster's entry in it.
type CSIClusterEntry struct {
	ClusterID       string `json:"cluster_id"`
	ClusterEndpoint string `json:"cluster_endpoint"`
	ClusterSecret   string `json:"cluster_secret"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager registers the controller and attaches the two streams that
// name a StorageCluster.
//
// Both channels carry the same kind of event — the object to reconcile — so
// both are ordinary watch sources. What they replace is the resync: a cluster
// whose status moved, or whose task window did, is reconciled when it happens
// rather than within the next interval.
func (r *StorageClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageCluster{}).
		Named("storagecluster")
	if r.Clusters != nil {
		builder = builder.WatchesRawSource(
			source.Channel(r.Clusters.Triggers(), &handler.EnqueueRequestForObject{}))
	}
	if r.Tasks != nil {
		builder = builder.WatchesRawSource(
			source.Channel(r.Tasks.Triggers(), &handler.EnqueueRequestForObject{}))
	}
	return builder.Complete(r)
}

func (r *StorageClusterReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &cluster)
	}

	if !controllerutil.ContainsFinalizer(&cluster, Finalizer) {
		controllerutil.AddFinalizer(&cluster, Finalizer)
		return ctrl.Result{}, r.Update(ctx, &cluster)
	}

	if cluster.Status.UUID == "" {
		return r.create(ctx, &cluster)
	}

	// The cluster exists in the control plane, so its streams can be opened.
	// Adding a scope already present is a no-op, which is what makes this safe
	// on every reconcile rather than only on the first.
	r.openStreams(&cluster)
	// A cluster with no pool can hold no volumes, so it is given one
	// (defaultpool.go). It runs here rather than on the creation path because
	// the pool's own controller needs the cluster's UUID, and a pool written
	// before there is one would only wait.
	r.ensureDefaultPool(ctx, &cluster)
	return r.sync(ctx, &cluster)
}

// create walks the creation machine by at most one step per reconcile.
func (r *StorageClusterReconciler) create(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	machine, err := statemachine.NewFromSnapshot(ctx, creationGraph(),
		statemachine.FromKube[simplyblockv1alpha2.StorageClusterStep](cluster.Status.Step))
	if err != nil {
		// An unrecognized step is a downgrade or a hand-edited object, and
		// neither resolves by reconciling again.
		return ctrl.Result{}, r.reportPhase(ctx, cluster,
			simplyblockv1alpha2.StorageClusterPhaseUnavailable,
			fmt.Sprintf("the creation cannot be resumed: %v", err))
	}
	defer machine.Close()

	// The first transition is the mutex. Persisting Claiming with an
	// optimistic-lock patch succeeds for one reconciler and returns 409 to the
	// rest, so exactly one of them goes on to POST.
	if cluster.Status.Step.State == "" {
		return r.claim(ctx, cluster), nil
	}

	if machine.TimeoutReached() {
		return ctrl.Result{RequeueAfter: clusterRetry}, r.reportPhase(ctx, cluster,
			simplyblockv1alpha2.StorageClusterPhaseUnavailable,
			fmt.Sprintf("creation step %s outlived its deadline", machine.CurrentState()))
	}

	switch machine.CurrentState() {
	case simplyblockv1alpha2.StorageClusterStepClaiming:
		return r.enterCreationStep(ctx, cluster, machine,
			simplyblockv1alpha2.StorageClusterStepCheckingControlPlane)

	case simplyblockv1alpha2.StorageClusterStepCheckingControlPlane:
		if err := r.API.Ready(ctx); err != nil {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
				FDBNotReady, FDBNotReady,
				"The control plane is not ready to accept a cluster: %v", err)
			return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster,
				"waiting for the control plane to become ready")
		}
		// The upgrade Secret diverts before any POST, which is the migration
		// route off a Helm deployment (§4.3).
		next := simplyblockv1alpha2.StorageClusterStepResolvingConfig
		if _, found, err := r.upgradeClaim(ctx, cluster); err != nil {
			return ctrl.Result{RequeueAfter: clusterRetry}, err
		} else if found {
			next = simplyblockv1alpha2.StorageClusterStepAdopting
		}
		return r.enterCreationStep(ctx, cluster, machine, next)

	case simplyblockv1alpha2.StorageClusterStepResolvingConfig:
		if _, err := r.creationParams(ctx, cluster); err != nil {
			return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster, err.Error())
		}
		return r.enterCreationStep(ctx, cluster, machine,
			simplyblockv1alpha2.StorageClusterStepCreating)

	case simplyblockv1alpha2.StorageClusterStepCreating:
		return r.postCluster(ctx, cluster, machine)

	case simplyblockv1alpha2.StorageClusterStepAdopting:
		return r.adopt(ctx, cluster, machine)

	case simplyblockv1alpha2.StorageClusterStepPersisting:
		// Reached only when a previous pass crashed between recording the step
		// and writing the UUID, which leaves nothing to persist. Restarting
		// the machine re-reads the control plane and finds the cluster by
		// name, which is the third route into adoption (§4.3).
		return r.adopt(ctx, cluster, machine)

	default:
		return ctrl.Result{}, r.reportPhase(ctx, cluster,
			simplyblockv1alpha2.StorageClusterPhaseUnavailable,
			fmt.Sprintf("creation step %s belongs to no path", machine.CurrentState()))
	}
}

// claim is the machine's first transition and the mutex that makes creation
// single-shot.
//
// It returns no error, which is the point: a failed patch here is a 409 saying
// another reconciler owns the claim, and that is an ordinary outcome to back
// off from rather than a failure to report. Returning an error would requeue
// with backoff and log a line per race.
func (r *StorageClusterReconciler) claim(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) ctrl.Result {
	deadline := metav1.NewTime(time.Now().Add(creationStepDeadline))
	patch := client.MergeFromWithOptions(cluster.DeepCopy(),
		client.MergeFromWithOptimisticLock{})
	cluster.Status.Phase = simplyblockv1alpha2.StorageClusterPhaseCreating
	cluster.Status.Step = statemachine.KubeSnapshot{
		State:    string(simplyblockv1alpha2.StorageClusterStepClaiming),
		Deadline: &deadline,
	}
	cluster.Status.Message = "claiming the creation of this cluster"
	cluster.Status.ObservedGeneration = cluster.Generation
	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		logf.FromContext(ctx).Info(
			"another reconciler holds the creation claim; backing off",
			"cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}
	}
	r.observePhase(cluster)
	return ctrl.Result{RequeueAfter: time.Second}
}

// enterCreationStep records the next step with its deadline and moves the
// machine into it. The record precedes the step's own work on the next pass,
// which is the write-ahead this path needs.
func (r *StorageClusterReconciler) enterCreationStep(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	machine *statemachine.Machine[simplyblockv1alpha2.StorageClusterStep],
	next simplyblockv1alpha2.StorageClusterStep,
) (ctrl.Result, error) {
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter creation step %s: %w", next, err)
	}
	deadline := metav1.NewTime(time.Now().Add(creationStepDeadline))
	err := r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		status.Phase = simplyblockv1alpha2.StorageClusterPhaseCreating
		status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: &deadline}
	})
	return ctrl.Result{RequeueAfter: time.Second}, err
}

// postCluster creates the backend cluster.
//
// A failed POST is treated as a possible race rather than as a failure: the
// controller looks the cluster up by name and adopts it if it is there, which
// covers two reconciles that both passed the claim on different
// resourceVersions, and a response lost after the backend committed.
func (r *StorageClusterReconciler) postCluster(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	machine *statemachine.Machine[simplyblockv1alpha2.StorageClusterStep],
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	params, err := r.creationParams(ctx, cluster)
	if err != nil {
		return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster, err.Error())
	}

	created, err := r.API.CreateCluster(ctx, params)
	if err != nil {
		existing, found, lookupErr := r.API.ClusterByName(ctx, cluster.Name)
		if lookupErr != nil || !found {
			var refusal *ControlPlaneError
			if errors.As(err, &refusal) {
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
					ClusterCreationFailed, ClusterCreationFailed,
					"The control plane refused the cluster (status=%d): %s",
					refusal.Status, refusal.Body)
			} else {
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
					ClusterCreationFailed, ClusterCreationFailed,
					"The cluster could not be created: %v", err)
			}
			return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster, err.Error())
		}
		log.Info("the cluster already exists in the control plane; adopting it",
			"cluster", cluster.Name, "uuid", existing.UUID)
		if _, err := r.enterCreationStep(ctx, cluster, machine,
			simplyblockv1alpha2.StorageClusterStepAdopting); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if _, err := r.enterCreationStep(ctx, cluster, machine,
		simplyblockv1alpha2.StorageClusterStepPersisting); err != nil {
		return ctrl.Result{}, err
	}
	return r.persist(ctx, cluster, adoption{
		UUID:   created.UUID,
		Secret: created.Secret,
		NQN:    created.NQN,
		Status: created.Status,
		NDCS:   created.NDCS,
		NPCS:   created.NPCS,
		FTT:    created.MaxFaultTolerance,
	}, false)
}

// adoption is what either route into Persisting knows about the cluster it is
// taking over. It is the two response shapes' common ground: a creation
// response and a list entry carry the same facts under different types.
type adoption struct {
	UUID   string
	Secret string
	NQN    string
	Status string
	NDCS   int
	NPCS   int
	FTT    int
}

// adopt takes over a backend cluster the operator did not create.
//
// Three paths reach it: an explicit upgrade Secret, a POST that failed against
// a cluster which already exists, and a name lookup that finds one. All three
// converge here, which is what makes the divert a graph a reader can check
// rather than two calls in unrelated branches.
func (r *StorageClusterReconciler) adopt(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	machine *statemachine.Machine[simplyblockv1alpha2.StorageClusterStep],
) (ctrl.Result, error) {
	found, ok, err := r.upgradeClaim(ctx, cluster)
	if err != nil {
		return ctrl.Result{RequeueAfter: clusterRetry}, err
	}
	if !ok {
		entry, listed, err := r.API.ClusterByName(ctx, cluster.Name)
		if err != nil {
			return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster, err.Error())
		}
		if !listed {
			// Nothing to adopt. The machine goes back to Creating, which is
			// the only way a Persisting step reached after a crash with no
			// cluster behind it recovers.
			return ctrl.Result{RequeueAfter: clusterRetry}, r.restartCreation(ctx, cluster)
		}
		found = adoption{
			UUID: entry.UUID, Secret: entry.Secret, NQN: entry.NQN,
			Status: entry.Status, NDCS: entry.NDCS, NPCS: entry.NPCS,
		}
	}

	if machine.CurrentState() != simplyblockv1alpha2.StorageClusterStepPersisting {
		if _, err := r.enterCreationStep(ctx, cluster, machine,
			simplyblockv1alpha2.StorageClusterStepPersisting); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.persist(ctx, cluster, found, true)
}

// upgradeClaim reads the Secret that migrates a Helm deployment into a managed
// cluster, and reports whether there is one.
//
// It is named `simplyblock-{cluster}-upgrade` and carries the UUID and the
// cluster secret. The cluster is fetched with them and the status populated
// from the response, without anything being posted.
func (r *StorageClusterReconciler) upgradeClaim(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (adoption, bool, error) {
	var secret corev1.Secret
	key := types.NamespacedName{
		Name:      fmt.Sprintf("simplyblock-%s-upgrade", cluster.Name),
		Namespace: cluster.Namespace,
	}
	if err := r.Get(ctx, key, &secret); err != nil {
		return adoption{}, false, client.IgnoreNotFound(err)
	}
	uuid, clusterSecret := string(secret.Data["uuid"]), string(secret.Data["secret"])
	if uuid == "" || clusterSecret == "" {
		return adoption{}, false, nil
	}

	found, err := r.API.Cluster(ctx, uuid)
	if err != nil {
		// The Secret names a cluster the control plane does not have. That is
		// worth retrying rather than failing: the control plane may be
		// starting, and the alternative is creating a second cluster beside
		// one somebody meant to adopt.
		return adoption{}, false, fmt.Errorf("read the cluster named by the upgrade secret: %w", err)
	}
	if found.Secret != "" {
		clusterSecret = found.Secret
	}
	return adoption{
		UUID: found.UUID, Secret: clusterSecret, NQN: found.NQN,
		Status: found.Status, NDCS: found.NDCS, NPCS: found.NPCS,
		FTT: found.MaxFaultTolerance,
	}, true, nil
}

// persist writes the per-cluster Secret, the CSI credentials entry, and the
// status that ends the creation path. The status write is last, because
// status.uuid is what every later reconcile branches on and writing it before
// the Secrets exist would advertise a cluster the CSI driver cannot reach.
func (r *StorageClusterReconciler) persist(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	found adoption,
	adopted bool,
) (ctrl.Result, error) {
	// A credential is the one thing this step cannot invent, and the one the
	// name lookup cannot supply: ClusterDTO.secret is write-only in the
	// control plane's own schema, so the cluster list carries none. Writing
	// the empty value onward would overwrite the entry the CSI driver reaches
	// the cluster through and then mark the cluster configured, so a cluster
	// nothing can provision from would look finished.
	secret, err := r.credentialFor(ctx, cluster, found)
	if err != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
			BackupCredentialsError, InvalidConfig,
			"The cluster was adopted but its secret is not known: %v", err)
		return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster, err.Error())
	}
	found.Secret = secret

	if err := r.writeClusterSecret(ctx, cluster, found); err != nil {
		return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster, err.Error())
	}
	if err := r.upsertCSICredentials(ctx, found.UUID, found.Secret); err != nil {
		return ctrl.Result{RequeueAfter: clusterRetry}, r.note(ctx, cluster, err.Error())
	}

	ftt := int32(found.FTT) //nolint:gosec // a fault tolerance is a small count
	err = r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		status.UUID = found.UUID
		status.ClusterName = cluster.Name
		status.NQN = found.NQN
		status.Status = found.Status
		status.ErasureCodingScheme = fmt.Sprintf("%dx%d", found.NDCS, found.NPCS)
		status.Configured = true
		status.MaxFaultTolerance = &ftt
		status.MaxConcurrentWorkerRestarts = effectiveConcurrentRestarts(
			cluster.Spec.MaxConcurrentWorkerRestarts, &ftt)
		status.Phase = phaseFor(found.Status)
		status.Step = statemachine.KubeSnapshot{}
		status.Message = ""
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	r.observePhase(cluster)

	if adopted {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal,
			ClusterAdopted, ClusterAdopted,
			"An existing backend cluster was adopted rather than created: %s", found.UUID)
	}
	logf.FromContext(ctx).Info("the cluster is in steady state",
		"cluster", cluster.Name, "uuid", found.UUID, "adopted", adopted)
	return ctrl.Result{RequeueAfter: clusterResync}, nil
}

// credentialFor is the cluster's secret, and it reports rather than guesses
// when there is none.
//
// Three sources, in the order they can be trusted. A creation response carries
// the secret the control plane just minted. An adoption through the upgrade
// Secret carries the one an administrator supplied. An adoption by name
// carries nothing at all, because the list endpoint's secret is write-only, so
// the only remaining source is a per-cluster Secret an earlier pass already
// wrote — which is exactly the case a retried creation is in.
//
// Failing here holds the cluster at Persisting rather than finishing it. That
// is the right side to err on: a cluster recorded as configured with an empty
// credential is one whose CSI driver cannot reach it, and nothing afterward
// revisits the decision.
func (r *StorageClusterReconciler) credentialFor(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, found adoption,
) (string, error) {
	if found.Secret != "" {
		return found.Secret, nil
	}
	recorded, err := r.clusterSecret(ctx, cluster)
	if err == nil && recorded != "" {
		return recorded, nil
	}
	return "", fmt.Errorf(
		"the control plane reports cluster %s but returns no secret for it, and none is "+
			"recorded: supply it in a Secret named simplyblock-%s-upgrade to adopt the cluster",
		found.UUID, cluster.Name)
}

// restartCreation puts the machine back at Claiming, which is the only
// recovery from a Persisting or Adopting step with nothing behind it.
func (r *StorageClusterReconciler) restartCreation(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	return r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		status.Step = statemachine.KubeSnapshot{}
		status.Phase = simplyblockv1alpha2.StorageClusterPhaseCreating
		status.Message = "no backend cluster was found to adopt; the creation restarts"
	})
}

// sync writes status from what the control plane reports.
//
// A reconcile whose reading matches what is already recorded returns without
// patching, so a cluster whose state has not moved produces no writes. The
// same pass re-upserts the CSI credentials entry, which is how one deleted or
// corrupted out of band is restored without intervention.
func (r *StorageClusterReconciler) sync(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if secret, err := r.clusterSecret(ctx, cluster); err == nil && secret != "" {
		if err := r.upsertCSICredentials(ctx, cluster.Status.UUID, secret); err != nil {
			log.Error(err, "the CSI credentials entry could not be restored",
				"cluster", cluster.Name)
			return ctrl.Result{RequeueAfter: clusterResync}, nil
		}
	}

	reading, err := r.reading(ctx, cluster.Status.UUID)
	if err != nil {
		log.Error(err, "the cluster could not be read", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: clusterResync}, nil
	}

	tasks := r.readTasks(ctx, cluster)

	ftt := int32(reading.MaxFaultTolerance) //nolint:gosec // a fault tolerance is a small count
	err = r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		status.Status = reading.Status
		status.NQN = reading.NQN
		status.Rebalancing = ptr.To(reading.Rebalancing)
		status.ErasureCodingScheme = fmt.Sprintf("%dx%d", reading.NDCS, reading.NPCS)
		status.MaxFaultTolerance = &ftt
		status.MaxConcurrentWorkerRestarts = effectiveConcurrentRestarts(
			cluster.Spec.MaxConcurrentWorkerRestarts, &ftt)
		status.Phase = phaseFor(reading.Status)
		status.Tasks = tasks
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	r.observePhase(cluster)
	return ctrl.Result{RequeueAfter: clusterResync}, nil
}

// reading is what the control plane currently says about one cluster, from the
// stream's cache once it has delivered its snapshot and from the control plane
// until then.
//
// The stream is root-scoped, so one snapshot covers every cluster and the gate
// is asked once rather than per cluster. It is a gate rather than a preference
// because an unsynced cache and a cluster the control plane has forgotten look
// identical, and reading the first as the second would report a live cluster
// as gone.
func (r *StorageClusterReconciler) reading(
	ctx context.Context, clusterID string,
) (subscriptions.ClusterDTO, error) {
	if r.Clusters != nil && r.Clusters.SyncedRoot() {
		if dto, ok := r.Clusters.Lookup(clusterID); ok {
			return dto, nil
		}
		// Synced and absent is the control plane saying the cluster is gone.
		// That is a real answer rather than a cold cache, and it is not this
		// pass's to act on: deletion is the finalizer's path, and a cluster
		// that vanished underneath the operator is reported by the read that
		// follows rather than inferred here.
		return subscriptions.ClusterDTO{},
			fmt.Errorf("the control plane no longer reports cluster %s", clusterID)
	}

	response, err := r.API.Cluster(ctx, clusterID)
	if err != nil {
		return subscriptions.ClusterDTO{}, err
	}
	return subscriptions.ClusterDTO{
		ID:                response.UUID,
		NQN:               response.NQN,
		Status:            response.Status,
		Rebalancing:       response.Rebalancing,
		NDCS:              response.NDCS,
		NPCS:              response.NPCS,
		MaxFaultTolerance: response.MaxFaultTolerance,
	}, nil
}

// readTasks is the window status.tasks publishes: the running and pending
// tasks, capped. A task that reaches a terminal outcome leaves the list and
// emits an event, which is where the history lives.
//
// A control plane that cannot be asked leaves the recorded list alone rather
// than emptying it, because an empty list means nothing is running and a
// failed read does not say that.
func (r *StorageClusterReconciler) readTasks(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) []simplyblockv1alpha2.ClusterTask {
	reported, err := r.reportedTasks(ctx, cluster.Status.UUID)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("the cluster's tasks could not be read",
			"cluster", cluster.Name, "err", err.Error())
		return cluster.Status.Tasks
	}

	current := make(map[string]bool, len(reported))
	running := make([]simplyblockv1alpha2.ClusterTask, 0, len(reported))
	for _, task := range reported {
		if task.Finished() {
			continue
		}
		current[task.ID] = true
		running = append(running, simplyblockv1alpha2.ClusterTask{
			ID:     task.ID,
			Type:   task.Type,
			Status: task.Status,
			Retry:  task.Retry,
		})
	}

	// The order is the control plane's own, because its TaskDTO carries no
	// date: there is nothing to sort newest-first by, and inventing an order
	// would make the cap look like a choice about recency when it is not.
	if len(running) > maxTasks {
		running = running[:maxTasks]
	}

	// A task that was in the list and is no longer running finished between
	// two readings. The event is what remains of it.
	for _, previous := range cluster.Status.Tasks {
		if current[previous.ID] {
			continue
		}
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal,
			TaskCompleted, TaskCompleted,
			"Task %s (%s) is no longer running", previous.ID, previous.Type)
	}
	return running
}

// reportedTasks is every task of one cluster, from the stream's cache once it
// has delivered the cluster's snapshot and from the control plane until then.
func (r *StorageClusterReconciler) reportedTasks(
	ctx context.Context, clusterID string,
) ([]subscriptions.TaskDTO, error) {
	scope := cpinformer.Scope{clusterID}
	if r.Tasks != nil && r.Tasks.Synced(scope) {
		return r.Tasks.List(scope), nil
	}
	return r.API.Tasks(ctx, clusterID)
}

// teardown deletes the backend cluster and then lets the object go.
//
// On any refusal it requeues and retries rather than removing the finalizer,
// so a control plane that is unreachable blocks the object instead of orphaning
// the cluster behind it. A cluster with no UUID has nothing to delete.
func (r *StorageClusterReconciler) teardown(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(cluster, Finalizer) &&
		!controllerutil.ContainsFinalizer(cluster, LegacyFinalizer) {
		return ctrl.Result{}, nil
	}

	if cluster.Status.UUID != "" {
		r.closeStreams(cluster.Status.UUID)
		if err := r.API.DeleteCluster(ctx, cluster.Status.UUID); err != nil {
			log.Error(err, "the cluster could not be deleted; retrying",
				"cluster", cluster.Name, "uuid", cluster.Status.UUID)
			return ctrl.Result{RequeueAfter: clusterRetry}, nil
		}
		if err := r.removeCSICredentials(ctx, cluster.Status.UUID); err != nil {
			log.Error(err, "the CSI credentials entry could not be removed; retrying",
				"cluster", cluster.Name)
			return ctrl.Result{RequeueAfter: clusterRetry}, nil
		}
	}

	clusterPhaseState.DeletePartialMatch(map[string]string{"cluster": cluster.Name})
	operationActiveState.DeleteLabelValues(cluster.Name)

	// Both spellings are removed. The rename is the one change in this
	// migration that wedges rather than degrades: an operator that removed
	// only the new key would leave every object an older one created in
	// Terminating forever (§4.5).
	controllerutil.RemoveFinalizer(cluster, Finalizer)
	controllerutil.RemoveFinalizer(cluster, LegacyFinalizer)
	return ctrl.Result{}, r.Update(ctx, cluster)
}

// creationParams assembles the request the control plane is asked to create a
// cluster with, and reports the one thing a user can get wrong here: a Secret
// or a URL that does not resolve.
//
// It is called twice on the creation path, once by ResolvingConfig to find out
// whether it can be built and once by Creating to build it. That is deliberate:
// resolving is where a bad configuration is reported, and posting is where the
// values are used, and the alternative is carrying a half-built request across
// a persisted step.
func (r *StorageClusterReconciler) creationParams(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (utils.ClusterAddParams, error) {
	backup, err := r.backupConfig(ctx, cluster)
	if err != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
			BackupCredentialsError, BackupCredentialsError,
			"The backup credentials could not be resolved: %v", err)
		return utils.ClusterAddParams{}, err
	}

	vault, err := vaultConfig(cluster.Spec.KMS)
	if err != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
			InvalidConfig, InvalidConfig, "The key store is not usable: %v", err)
		return utils.ClusterAddParams{}, err
	}

	// minHugePagesSize is optional: left unset, the control plane computes the
	// minimum itself and the omitempty tag drops the key. Only a value the user
	// supplied has to parse, because parsing the empty string yields nil and
	// would otherwise fail every cluster that omits the field.
	var hugePages *int64
	if raw := cluster.Spec.MinHugePagesSize; raw != "" {
		hugePages = utils.ParseSize(raw, "si/iec", "", false)
	}

	return utils.ClusterAddParams{
		Name:                   cluster.Name,
		CapWarn:                capacityThreshold(cluster.Spec.WarningThreshold),
		CapCrit:                capacityThreshold(cluster.Spec.CriticalThreshold),
		ProvCapWarn:            provisionedCapacityThreshold(cluster.Spec.WarningThreshold),
		ProvCapCrit:            provisionedCapacityThreshold(cluster.Spec.CriticalThreshold),
		DistrNdcs:              stripeDataChunks(cluster.Spec.Stripe),
		DistrNpcs:              StripeParityChunks(cluster.Spec.Stripe),
		SpdkVcpuCount:          ptr.IntFrom(cluster.Spec.VCPUCount, 8),
		HugepagesMem:           ptr.Int64From(hugePages, 0),
		MaxSubsys:              uint(ptr.IntFrom(cluster.Spec.MaxSubsystemCount, 10)), //nolint:gosec // bounded by the CRD to 10..75
		EnableNodeAffinity:     ptr.BoolFromOrFalse(cluster.Spec.EnableNodeAffinity),
		Fabric:                 cluster.Spec.FabricType,
		CRName:                 cluster.Name,
		CRNameSpace:            cluster.Namespace,
		CRPlural:               "storageclusters",
		ClientDataIfname:       cluster.Spec.ClientDataIfname,
		NvmfBasePort:           ptr.IntFrom(cluster.Spec.NvmfBasePort, 4420),
		RpcBasePort:            ptr.IntFrom(cluster.Spec.RpcBasePort, 8080),
		SnodeApiPort:           ptr.IntFrom(cluster.Spec.SnodeApiPort, 50001),
		BackupConfig:           backup,
		HashicorpVaultSettings: vault,
		EnableFailureDomain:    ptr.BoolFromOrFalse(cluster.Spec.EnableFailureDomains),
		InlineChecksum:         ptr.BoolFromOrFalse(cluster.Spec.EnableChecksumValidation),
		Atomic4k:               ptr.BoolFromOrFalse(cluster.Spec.EnableAtomic4kWrites),
		DeviceMode:             deviceMode(cluster.Spec.DeviceClass),
	}, nil
}

// deviceMode is spec.deviceClass in sbcli's cluster-create spelling. The CRD
// default is applied by the API server's OpenAPI defaulting, which a value
// read here has already gone through in production; the empty case below only
// covers a caller (a test, an old cached object) that bypassed it.
func deviceMode(class simplyblockv1alpha2.StorageClusterDeviceClass) string {
	if class == simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock {
		return "lblk"
	}
	return "nvme"
}

// backupConfig resolves the credentials the backup store is reached with.
//
// What it sends is the location and nothing else. How a copy is taken belongs
// to the control plane, which keeps accepting the fields the redesign removed
// and applies its own defaults now that the operator stops sending them (§12).
func (r *StorageClusterReconciler) backupConfig(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (*utils.BackupConfig, error) {
	store := cluster.Spec.Backup
	if store == nil {
		return nil, nil
	}
	if store.CredentialsSecretRef.Name == "" {
		return nil, errors.New("spec.backup.credentialsSecretRef.name is required")
	}
	// A store with no bucket is not a location. The registered v1alpha1 type
	// had no bucket at all, so a cluster converted from one arrives with an
	// empty bucket unless somebody supplied it, and sending that onward asks
	// the control plane to write copies nowhere in particular (§12).
	//
	// The message names the remedy rather than only the problem. A field this
	// version cannot express is carried in the conversion annotation
	// design-api-upgrade.md §6.2 defines, so an administrator still on
	// v1alpha1 sets it there and the conversion restores it into the field.
	if store.Bucket == "" {
		return nil, fmt.Errorf(
			"spec.backup.bucket is required and is empty: a store authored against "+
				"v1alpha1 has none, because that version cannot express one. Set it on "+
				"the v1alpha2 object, or on a v1alpha1 one through the conversion "+
				"annotation %s", annotationBackupBucket)
	}

	var secret corev1.Secret
	key := types.NamespacedName{Name: store.CredentialsSecretRef.Name, Namespace: cluster.Namespace}
	if err := r.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("read the backup credentials secret %q: %w",
			store.CredentialsSecretRef.Name, err)
	}
	accessKeyID, ok := secret.Data["access_key_id"]
	if !ok {
		return nil, fmt.Errorf("secret %q has no access_key_id", store.CredentialsSecretRef.Name)
	}
	secretAccessKey, ok := secret.Data["secret_access_key"]
	if !ok {
		return nil, fmt.Errorf("secret %q has no secret_access_key", store.CredentialsSecretRef.Name)
	}
	if err := atlasnet.ValidateExternalURL(store.Endpoint); err != nil {
		return nil, fmt.Errorf("spec.backup.endpoint: %w", err)
	}

	return &utils.BackupConfig{
		AccessKeyID:     string(accessKeyID),
		SecretAccessKey: string(secretAccessKey),
		LocalEndpoint:   store.Endpoint,
		Bucket:          store.Bucket,
		Prefix:          store.Prefix,
		Region:          store.Region,
	}, nil
}

// vaultConfig maps the key store onto the control plane's own name for it. The
// regrouping under spec.kms is Kubernetes-side only: the control plane keeps
// hashicorp_vault_settings.base_url, and this is where the two meet.
func vaultConfig(kms *simplyblockv1alpha2.KMSSpec) (*utils.HashicorpVaultConfig, error) {
	if kms == nil || kms.Vault == nil || kms.Vault.BaseURL == "" {
		return nil, nil
	}
	if err := atlasnet.ValidateExternalURL(kms.Vault.BaseURL); err != nil {
		return nil, fmt.Errorf("spec.kms.vault.baseURL: %w", err)
	}
	return &utils.HashicorpVaultConfig{BaseURL: kms.Vault.BaseURL}, nil
}

// writeClusterSecret records the cluster's UUID and secret beside the object
// that owns it, so the pair survives an operator that forgets everything else.
func (r *StorageClusterReconciler) writeClusterSecret(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, found adoption,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      fmt.Sprintf("simplyblock-cluster-%s", cluster.Name),
		Namespace: cluster.Namespace,
	}}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return fmt.Errorf("own the cluster secret: %w", err)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["uuid"] = []byte(found.UUID)
		secret.Data["secret"] = []byte(found.Secret)
		return nil
	})
	if err != nil {
		return fmt.Errorf("write the cluster secret: %w", err)
	}
	return nil
}

// clusterSecret reads the secret back, and reports the empty string when there
// is none.
func (r *StorageClusterReconciler) clusterSecret(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (string, error) {
	var secret corev1.Secret
	key := types.NamespacedName{
		Name:      fmt.Sprintf("simplyblock-cluster-%s", cluster.Name),
		Namespace: cluster.Namespace,
	}
	if err := r.Get(ctx, key, &secret); err != nil {
		return "", err
	}
	return string(secret.Data["secret"]), nil
}

// upsertCSICredentials adds or replaces this cluster's entry in the aggregate
// Secret the CSI driver reads.
func (r *StorageClusterReconciler) upsertCSICredentials(
	ctx context.Context, clusterID, clusterSecret string,
) error {
	return r.editCSICredentials(ctx, func(creds *CSICredentials) {
		entry := CSIClusterEntry{
			ClusterID:       clusterID,
			ClusterEndpoint: utils.ENDPOINT,
			ClusterSecret:   clusterSecret,
		}
		for i := range creds.Clusters {
			if creds.Clusters[i].ClusterID == clusterID {
				creds.Clusters[i] = entry
				return
			}
		}
		creds.Clusters = append(creds.Clusters, entry)
	})
}

// removeCSICredentials drops this cluster's entry from it.
func (r *StorageClusterReconciler) removeCSICredentials(
	ctx context.Context, clusterID string,
) error {
	return r.editCSICredentials(ctx, func(creds *CSICredentials) {
		kept := creds.Clusters[:0]
		for _, entry := range creds.Clusters {
			if entry.ClusterID != clusterID {
				kept = append(kept, entry)
			}
		}
		creds.Clusters = kept
	})
}

// editCSICredentials applies one change to the aggregate Secret. It retries on
// conflict because every cluster in the deployment writes the same object.
func (r *StorageClusterReconciler) editCSICredentials(
	ctx context.Context, edit func(*CSICredentials),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      csiCredentialsSecret,
			Namespace: r.Namespace,
		}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			var creds CSICredentials
			if data, ok := secret.Data["secret.json"]; ok {
				_ = json.Unmarshal(data, &creds)
			}
			edit(&creds)
			payload, err := json.MarshalIndent(creds, "", "  ")
			if err != nil {
				return err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data["secret.json"] = payload
			return nil
		})
		return err
	})
}

// openStreams tells the control-plane informer that this cluster exists and
// where its objects belong. Adding a scope already present is a no-op, which is
// what makes this safe on every reconcile rather than only on the first.
func (r *StorageClusterReconciler) openStreams(cluster *simplyblockv1alpha2.StorageCluster) {
	if r.NodeScopes != nil {
		r.NodeScopes.Add(cpinformer.Scope{cluster.Status.UUID})
	}
	if r.TaskScopes != nil {
		r.TaskScopes.Add(cpinformer.Scope{cluster.Status.UUID})
	}
	for _, registrar := range r.BackupRegistrars {
		registrar.RegisterCluster(cluster.Status.UUID, client.ObjectKeyFromObject(cluster))
	}
	for _, scopes := range r.BackupScopes {
		scopes.Add(cpinformer.Scope{cluster.Status.UUID})
	}
}

// closeStreams stops streaming a cluster that is going away, and forgets where
// its objects belonged. A stream left open would reconnect against a cluster
// that no longer exists, and a mapping left behind would name a namespace for
// objects nothing will create.
func (r *StorageClusterReconciler) closeStreams(clusterUUID string) {
	if r.NodeScopes != nil {
		r.NodeScopes.Remove(cpinformer.Scope{clusterUUID})
	}
	if r.TaskScopes != nil {
		r.TaskScopes.Remove(cpinformer.Scope{clusterUUID})
	}
	for _, scopes := range r.BackupScopes {
		scopes.Remove(cpinformer.Scope{clusterUUID})
	}
	for _, registrar := range r.BackupRegistrars {
		registrar.UnregisterCluster(clusterUUID)
	}
}

// note replaces status.message without moving anything else.
func (r *StorageClusterReconciler) note(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, message string,
) error {
	return r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		status.Message = message
	})
}

// reportPhase writes a phase and the sentence explaining it.
func (r *StorageClusterReconciler) reportPhase(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	phase simplyblockv1alpha2.StorageClusterPhase,
	message string,
) error {
	err := r.writeStatus(ctx, cluster, func(status *simplyblockv1alpha2.StorageClusterStatus) {
		status.Phase = phase
		status.Message = message
	})
	r.observePhase(cluster)
	return err
}

// observePhase publishes the cluster's phase as a gauge, one series per phase
// with exactly one of them set, so a cluster stuck in Creating is alertable.
func (r *StorageClusterReconciler) observePhase(cluster *simplyblockv1alpha2.StorageCluster) {
	for _, phase := range allPhases {
		value := 0.0
		if cluster.Status.Phase == phase {
			value = 1
		}
		clusterPhaseState.WithLabelValues(cluster.Name, string(phase)).Set(value)
	}
}

// allPhases is every value the gauge publishes a series for.
var allPhases = []simplyblockv1alpha2.StorageClusterPhase{
	simplyblockv1alpha2.StorageClusterPhasePending,
	simplyblockv1alpha2.StorageClusterPhaseCreating,
	simplyblockv1alpha2.StorageClusterPhaseOnline,
	simplyblockv1alpha2.StorageClusterPhaseDegraded,
	simplyblockv1alpha2.StorageClusterPhaseUnavailable,
	simplyblockv1alpha2.StorageClusterPhaseSuspended,
}

// writeStatus applies the mutation and patches only when something changed, so
// a cluster whose state has not moved produces no writes.
func (r *StorageClusterReconciler) writeStatus(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	mutate func(*simplyblockv1alpha2.StorageClusterStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageCluster
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if equality.Semantic.DeepEqual(fresh.Status, desired) {
			cluster.Status = desired
			cluster.ResourceVersion = fresh.ResourceVersion
			return nil
		}

		patch := client.MergeFromWithOptions(fresh.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		fresh.Status = desired
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		cluster.Status = fresh.Status
		cluster.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// creationGraph is the entity's own machine. There is one graph rather than a
// MultiConfig, because an entity has no spec.action to key one on.
//
// Adopting is reached from two states rather than one: the upgrade Secret
// diverts before any POST, and a POST that failed against an existing cluster
// diverts after. Both converge on Persisting, and declaring both edges makes
// that a graph a reader can check rather than two calls in unrelated branches.
func creationGraph() statemachine.Config[simplyblockv1alpha2.StorageClusterStep] {
	type clusterStep = simplyblockv1alpha2.StorageClusterStep
	bounded := func(context.Context, clusterStep, clusterStep) (time.Duration, error) {
		return creationStepDeadline, nil
	}
	return statemachine.Config[clusterStep]{
		Initial: simplyblockv1alpha2.StorageClusterStepClaiming,
		States: map[clusterStep]statemachine.StateDef[clusterStep]{
			simplyblockv1alpha2.StorageClusterStepClaiming: {
				To: []clusterStep{simplyblockv1alpha2.StorageClusterStepCheckingControlPlane},
			},
			simplyblockv1alpha2.StorageClusterStepCheckingControlPlane: {
				To: []clusterStep{
					simplyblockv1alpha2.StorageClusterStepResolvingConfig,
					simplyblockv1alpha2.StorageClusterStepAdopting,
				},
				OnEnter: bounded,
			},
			simplyblockv1alpha2.StorageClusterStepResolvingConfig: {
				To:      []clusterStep{simplyblockv1alpha2.StorageClusterStepCreating},
				OnEnter: bounded,
			},
			simplyblockv1alpha2.StorageClusterStepCreating: {
				To: []clusterStep{
					simplyblockv1alpha2.StorageClusterStepPersisting,
					simplyblockv1alpha2.StorageClusterStepAdopting,
				},
				OnEnter: bounded,
			},
			simplyblockv1alpha2.StorageClusterStepAdopting: {
				To:      []clusterStep{simplyblockv1alpha2.StorageClusterStepPersisting},
				OnEnter: bounded,
			},
			simplyblockv1alpha2.StorageClusterStepPersisting: {OnEnter: bounded},
		},
	}
}

// phaseFor reads the control plane's lifecycle string as this group's phase.
// The values on the left are the control plane's own vocabulary, which is why
// they are lowercase; only a value this API defines is PascalCase.
func phaseFor(status string) simplyblockv1alpha2.StorageClusterPhase {
	switch lower(status) {
	case utils.ClusterStatusActive:
		return simplyblockv1alpha2.StorageClusterPhaseOnline
	case "degraded", "read_only":
		return simplyblockv1alpha2.StorageClusterPhaseDegraded
	case utils.ClusterStatusSuspended:
		return simplyblockv1alpha2.StorageClusterPhaseSuspended
	case "":
		return simplyblockv1alpha2.StorageClusterPhasePending
	default:
		// Everything else the control plane reports — unready, in_expansion,
		// in_activation — is the cluster not serving for a reason nobody asked
		// for, which is what Unavailable means.
		return simplyblockv1alpha2.StorageClusterPhaseUnavailable
	}
}

// capacityThreshold and provisionedCapacityThreshold read one alarm level's
// two halves, and zero for a level nobody set. Zero is what the control plane
// reads as "use the default," which is the same statement.
func capacityThreshold(t *simplyblockv1alpha2.CapacityThresholdSpec) int {
	if t == nil || t.Capacity == nil {
		return 0
	}
	return int(*t.Capacity)
}

func provisionedCapacityThreshold(t *simplyblockv1alpha2.CapacityThresholdSpec) int {
	if t == nil || t.ProvisionedCapacity == nil {
		return 0
	}
	return int(*t.ProvisionedCapacity)
}

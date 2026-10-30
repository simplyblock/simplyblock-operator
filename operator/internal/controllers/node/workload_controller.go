// The reconciler that puts a cluster's storage-node workload on the cluster.
//
// It watches StorageCluster and owns everything a storage node needs to run: the
// DaemonSet, the headless Service and its EndpointSlice, the spdk-proxy Service,
// the serving certificates, the ServiceAccount and its role, and the per-node
// ConfigMap. Every object is established as a child of the cluster by controller
// reference, which is what keeps the property that made them owned in the first
// place: deleting the thing they exist for tears them down (§5.1).
//
// It is a second controller on StorageCluster rather than a branch of the
// cluster's own reconciler, and the two write disjoint things. The cluster's
// reconciler owns StorageCluster.status and never touches a DaemonSet; this one
// owns the workload and never writes the cluster. Splitting them is what keeps the
// workload in the package design-crd-model.md §7.10 assigns it to, beside the
// nodes it exists to run.
//
// The retired StorageNodeSet owned all of this, and allowed several sets per
// cluster, each with its own DaemonSet selected by a per-set node label. That
// collapses to one workload per cluster, because growth is nodes rather than sets
// and what differs between hardware generations — the two images, the SPDK memory,
// and the sizing — is per node already (§5.1).

package node

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	atlaskube "github.com/simplyblock/atlas/kube"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// memberServicesPoll is how often a member looks again for its supercluster's
// Services, which are not its own objects and so send it no event.
const memberServicesPoll = 10 * time.Second

// SingletonControlPlaneName is the one ControlPlane a deployment has, which is
// where a cluster that states no storage-node image takes one from.
const SingletonControlPlaneName = "simplyblock"

// StorageNodeWorkloadReconciler reconciles the workload of one StorageCluster.
type StorageNodeWorkloadReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Namespace is where the operator runs, which is where the ControlPlane
	// singleton the default image comes from lives.
	Namespace string

	// TLS answers what the installation's ControlPlane states, per reconcile.
	// Nil is plaintext. It is read each time rather than once at startup, so a
	// ControlPlane created after the operator started takes effect on the next
	// pass.
	TLS controlplane.TLSResolver

	// Workload writes the per-node ConfigMap, which is the one object here whose
	// contents come from the nodes rather than from the cluster.
	Workload *Workload
}

// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager registers the controller.
//
// It watches the nodes as well as the cluster, because two of the objects here are
// built from them: the per-node ConfigMap holds one entry per worker, and the
// EndpointSlice publishes one DNS name per worker. A node that arrives has to
// reach both before its pod can start.
func (r *StorageNodeWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageCluster{},
			builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
				cluster, ok := object.(*simplyblockv1alpha2.StorageCluster)
				return ok && cluster.Spec.Supercluster == nil
			}))).
		Named("storagenode-workload").
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Watches(&simplyblockv1alpha2.StorageNode{},
			handler.EnqueueRequestsFromMapFunc(r.clusterOf)).
		// A migration's target is published before any node runs there.
		Watches(&simplyblockv1alpha2.StorageNodeOps{},
			handler.EnqueueRequestsFromMapFunc(r.clusterOfOperation),
			builder.WithPredicates(migrationOpsChanged())).
		// The spdk-proxy EndpointSlices are built from the pod list, so a pod
		// appearing -- or reappearing on a different RPC port -- is the event that
		// makes them wrong. Without this watch the pass only reruns when something
		// else happens to touch the cluster, and the last node of a sequential
		// rollout is added into the quietest moment there is: its slice is never
		// published, its per-pod DNS name never resolves, and its node add fails
		// on a name lookup (found live 2026-09-23: worker zb6g4's pod was
		// recreated on port 4432 after a retry and no slice followed it).
		//
		// Predicated on the role label rather than filtered in the map function
		// alone: every pod in the namespace would otherwise wake the pass, and a
		// namespace running anything besides storage nodes pays for it.
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.clusterOfProxyPod),
			builder.WithPredicates(isSpdkProxyPod())).
		// The agent's endpoint carries the pod's own address once the cluster
		// declares a network, and a restarted pod comes back on a new one.
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.clusterOfAgentPod),
			builder.WithPredicates(isAgentPod())).
		Complete(r)
}

// clusterOf maps a node back to the cluster whose workload runs it.
func (r *StorageNodeWorkloadReconciler) clusterOf(
	_ context.Context, object client.Object,
) []reconcile.Request {
	node, ok := object.(*simplyblockv1alpha2.StorageNode)
	if !ok || node.Spec.ClusterRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Name:      node.Spec.ClusterRef,
		Namespace: node.Namespace,
	}}}
}

// isSpdkProxyPod is the watch's filter: only an spdk-proxy pod can change what
// reconcileSpdkProxyEndpoints publishes, so only one should wake the pass.
//
// It reads the same label reconcileSpdkProxyEndpoints selects on, so the watch
// and the work it triggers cannot drift apart.
func isSpdkProxyPod() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()["role"] == utils.LabelSpdkProxyRole
	})
}

// isAgentPod admits the storage-node agent pods, which carry the app label the
// DaemonSet sets.
func isAgentPod() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[atlaskube.LabelApp] == atlaskube.AppStorageNode
	})
}

// clusterOfAgentPod maps an agent pod to the cluster its DaemonSet belongs to,
// which the pod's own label names.
func (r *StorageNodeWorkloadReconciler) clusterOfAgentPod(
	_ context.Context, object client.Object,
) []reconcile.Request {
	labels := object.GetLabels()
	cluster := labels[atlaskube.LabelSimplyblockCluster]
	if labels[atlaskube.LabelApp] != atlaskube.AppStorageNode || cluster == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Name:      cluster,
		Namespace: object.GetNamespace(),
	}}}
}

// clusterOfProxyPod maps an spdk-proxy pod to the clusters whose workload pass
// publishes its DNS.
//
// The pod carries no owner reference and no cluster label, so the namespace is
// the only link back and every StorageCluster there is enqueued. That is the
// same set reconcileSpdkProxyEndpoints already considers -- it lists the proxy
// pods of the cluster's own namespace -- so this maps no more widely than the
// work it triggers.
func (r *StorageNodeWorkloadReconciler) clusterOfProxyPod(
	ctx context.Context, object client.Object,
) []reconcile.Request {
	pod, ok := object.(*corev1.Pod)
	if !ok || pod.Labels["role"] != utils.LabelSpdkProxyRole {
		return nil
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(clusters.Items))
	for i := range clusters.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      clusters.Items[i].Name,
			Namespace: clusters.Items[i].Namespace,
		}})
	}
	return requests
}

func (r *StorageNodeWorkloadReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Kubernetes garbage collection tears the workload down with the cluster, so
	// a cluster on its way out needs nothing done to it here.
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// A supercluster has no storage nodes, and the watch predicate keeps it
	// from arriving; this keeps a stale request from building it a workload
	// under names a standalone cluster is reached by.
	if cluster.Spec.Supercluster != nil {
		return ctrl.Result{}, nil
	}

	// Every object here is named from the cluster's workload names, which its
	// own reconciler records before anything else. Until then there is nothing
	// to build, and the status write that records them brings the cluster back.
	if _, recorded := utils.WorkloadNamesOf(&cluster); !recorded {
		logf.FromContext(ctx).V(1).Info("the cluster has not recorded its workload names yet",
			"cluster", cluster.Name)
		return ctrl.Result{}, nil
	}

	// A member is reached through its supercluster's Services, so nothing of
	// its workload starts before they and their certificates exist: a node
	// that came up first would wait on a name that resolves to nothing. Those
	// objects are the supercluster's and wake nothing here, so the wait polls.
	if cluster.Spec.SuperclusterRef != "" {
		ready, waiting, err := utils.WorkloadServicesReady(ctx, r.Client, &cluster, r.TLS.Settings(ctx).Enabled)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			logf.FromContext(ctx).V(1).Info("the member waits for its supercluster's Services",
				"cluster", cluster.Name, "waiting", waiting)
			return ctrl.Result{RequeueAfter: memberServicesPoll}, nil
		}
	}

	// One reading of the node set feeds both halves that depend on it. The
	// ConfigMap is written before the workers are enrolled, and a worker is only
	// schedulable once enrollment has labeled it, so a pod cannot start against
	// an entry that is not there -- while the two halves are reading the same
	// set. Taken separately, a node that appeared between the readings had its
	// worker enrolled by a pass that never wrote its entry, and the pod then
	// reached the node configuration script with no sizing and failed there,
	// which is a long way from the cause (§5.3).
	nodes, err := r.clusterNodes(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Workload.ReconcileConfig(ctx, &cluster, nodes); err != nil {
		return ctrl.Result{}, err
	}

	for _, step := range r.workloadSteps(nodes) {
		if err := step.run(ctx, &cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile %s: %w", step.what, err)
		}
	}
	return ctrl.Result{}, nil
}

// workloadStep is one thing the pass applies, with the name its failure is
// reported under.
type workloadStep struct {
	what string
	run  func(context.Context, *simplyblockv1alpha2.StorageCluster) error
}

// workloadSteps is everything the pass applies, in order.
//
// It is a method rather than a literal inside Reconcile so that what the pass
// does is readable from a test. The spdk-proxy endpoints are in this list
// because they were once in another one: the builder outlived the reconcile that
// called it, kept passing its own unit tests, and published nothing.
func (r *StorageNodeWorkloadReconciler) workloadSteps(
	nodes []simplyblockv1alpha2.StorageNode,
) []workloadStep {
	return []workloadStep{
		{"the service account and its role", r.reconcileRBAC},
		{"the serving certificates", r.reconcileCertificates},
		{"the headless service", r.reconcileService},
		{"the endpoint slice", func(
			ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
		) error {
			return r.reconcileEndpointSlice(ctx, cluster, nodes)
		}},
		{"the spdk-proxy endpoints", r.reconcileSpdkProxyEndpoints},
		{"the worker enrollment", func(
			ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
		) error {
			return r.enrollWorkers(ctx, cluster, nodes)
		}},
		{"the daemon set", r.reconcileDaemonSet},
	}
}

// clusterNodes is the one reading of this cluster's storage nodes that the pass
// uses wherever the answer has to be the same answer.
//
// The filtering is here rather than in each caller so that "this cluster's
// nodes" means one thing: a node on its way out is not a reason to keep its
// worker enrolled, and it is not a reason to write its entry either.
func (r *StorageNodeWorkloadReconciler) clusterNodes(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) ([]simplyblockv1alpha2.StorageNode, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("list the storage nodes: %w", err)
	}

	kept := make([]simplyblockv1alpha2.StorageNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster.Name || node.Spec.WorkerNode == "" {
			continue
		}
		if !node.DeletionTimestamp.IsZero() {
			continue
		}
		kept = append(kept, *node)
	}
	return kept, nil
}

// enrollWorkers puts every worker this cluster has a node on into its storage
// plane, which is what gives the DaemonSet somewhere to schedule.
//
// It runs before the DaemonSet and not after, because the order is the one a
// reader wants it in: the selector is written, then the thing that selects on it.
//
// The retired StorageNodeSet enrolled its own workers, and when provisioning was
// rebuilt around StorageNode the migration path kept doing it and nothing on the
// provisioning path did. What that produced was not a failure but a deadlock: a
// node holds at CheckingHost waiting for the worker's storage-node API, and the
// process that would answer cannot be scheduled until this label exists. Neither
// side reports anything wrong, because neither side is.
//
// Enrollment is per worker rather than per node. Two nodes on one worker are two
// slots of one machine, and LabelWorker rewrites that machine's whole slot label
// set from the nodes that want it, so calling it once per worker is both
// sufficient and what keeps the set consistent.
func (r *StorageNodeWorkloadReconciler) enrollWorkers(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	nodes []simplyblockv1alpha2.StorageNode,
) error {
	seen := map[string]struct{}{}
	for i := range nodes {
		node := &nodes[i]
		if _, already := seen[node.Spec.WorkerNode]; already {
			continue
		}
		seen[node.Spec.WorkerNode] = struct{}{}

		if err := r.Workload.LabelWorker(
			ctx, cluster.Namespace, cluster.Name, node.Spec.WorkerNode); err != nil {
			return fmt.Errorf("enroll worker %s: %w", node.Spec.WorkerNode, err)
		}
	}
	return nil
}

// reconcileDaemonSet applies the pod template every storage node runs under.
//
// The TLS Secret's resourceVersion is stamped onto the template so that a
// certificate rotation rolls the pods. The Secret's name does not change when it
// rotates, so nothing else would notice (§5.4).
func (r *StorageNodeWorkloadReconciler) reconcileDaemonSet(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	image, err := r.image(ctx, cluster)
	if err != nil {
		return err
	}
	secretVersion, err := r.tlsSecretVersion(ctx, cluster)
	if err != nil {
		return err
	}

	tls := r.TLS.Settings(ctx)
	desired := utils.BuildStorageNodeDaemonSet(cluster,
		tls.Enabled, tls.Mutual, tls.Provider, secretVersion, image)
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	// The read that seeds the update is served from the informer cache, and the
	// DaemonSet controller rewrites status on every pod transition — so while
	// pods are rolling, which is exactly when this reconcile runs, the cached
	// resourceVersion is stale and the update loses. Retrying on a fresh read is
	// the answer rather than failing the whole workload pass over a conflict that
	// means no more than that somebody counted a ready pod.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var existing appsv1.DaemonSet
		err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		if err != nil {
			return err
		}

		// The desired object is rebuilt from the template on every attempt, so a
		// retry does not carry the resourceVersion the previous one was refused
		// for.
		update := desired.DeepCopy()
		update.ResourceVersion = existing.ResourceVersion
		return r.Update(ctx, update)
	})
}

// image is the storage-node container image, defaulting to the ControlPlane
// singleton's so that a deployment states the version once (§5.1).
func (r *StorageNodeWorkloadReconciler) image(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (string, error) {
	if wl := cluster.Spec.StorageNodes; wl != nil && wl.Image != "" {
		return string(wl.Image), nil
	}

	// Read at v1alpha2, the stored version, rather than at the retired v1alpha1. A
	// read of the retired version is answered only by the conversion webhook,
	// which a fresh install does not deploy, and the cache it would be served from
	// lists empty instead of failing: the fallback would report the singleton
	// missing on a cluster that has it.
	var controlPlane simplyblockv1alpha2.ControlPlane
	key := types.NamespacedName{Namespace: r.Namespace, Name: SingletonControlPlaneName}
	if err := r.Get(ctx, key, &controlPlane); err != nil {
		return "", fmt.Errorf(
			"spec.storageNodes.image is unset and ControlPlane %s cannot be read: %w",
			SingletonControlPlaneName, err)
	}
	if local := controlPlane.Spec.Source.Local; local != nil && local.Image != "" {
		return string(local.Image), nil
	}
	// A managed control plane is a different Kubernetes cluster's install and
	// carries no image of its own here to fall back to -- spec.source.managed
	// says where the control plane is, not what this cluster's storage nodes
	// should run. StorageNodeImage is the only source of a default left.
	if managed := controlPlane.Spec.Source.Managed; managed != nil && managed.StorageNodeImage != "" {
		return string(managed.StorageNodeImage), nil
	}
	return "", fmt.Errorf(
		"spec.storageNodes.image is unset and ControlPlane %s states no managed image",
		SingletonControlPlaneName)
}

// tlsSecretVersion is what a certificate rotation is noticed by.
func (r *StorageNodeWorkloadReconciler) tlsSecretVersion(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (string, error) {
	if !r.TLS.Settings(ctx).Enabled {
		return "", nil
	}
	names, _ := utils.WorkloadNamesOf(cluster)
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: names.NodeAPITLSSecret}
	err := r.Get(ctx, key, &secret)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return secret.ResourceVersion, nil
}

// reconcileService applies the headless Services the per-pod DNS names hang off.
// A member's are its supercluster's, which the supercluster applies (§6.2).
func (r *StorageNodeWorkloadReconciler) reconcileService(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	if cluster.Spec.SuperclusterRef != "" {
		return nil
	}
	tls := r.TLS.Settings(ctx)
	return utils.ApplyWorkloadServices(ctx, r.Client, r.Scheme, cluster, tls.Enabled, tls.Provider)
}

// reconcileEndpointSlice publishes one per-pod DNS name per worker the cluster's
// nodes run on, plus the target of each migration that is running.
//
// The slice is built from the StorageNode objects rather than from a list on the
// cluster, because the nodes are what say which workers are in the storage plane
// now: a relocation adds the target before the node's own spec.workerNode moves.
// A remote node is the peer site's, runs on no worker here, and is not published.
func (r *StorageNodeWorkloadReconciler) reconcileEndpointSlice(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	nodes []simplyblockv1alpha2.StorageNode,
) error {
	log := logf.FromContext(ctx)

	targets, err := r.migrationTargets(ctx, cluster.Namespace, nodes)
	if err != nil {
		return err
	}

	addresses := map[string]string{}
	for i := range nodes {
		node := &nodes[i]
		for _, worker := range []string{node.Spec.WorkerNode, targets[node.Name]} {
			if _, known := addresses[worker]; known || worker == "" {
				continue
			}
			address, err := r.agentAddress(ctx, cluster, worker)
			if err != nil || address == "" {
				log.V(1).Info("a worker has no internal address yet and is not published",
					"worker", worker)
				continue
			}
			addresses[worker] = address
		}
	}

	desired := utils.BuildStorageNodeEndpointSlice(cluster, addresses)
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	var existing discoveryv1.EndpointSlice
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, desired)
}

// migrationTargets maps each node whose lock a Migrate holds to the worker the
// migration moves it to.
//
// A Migrate queued behind another operation is Pending and holds no lock yet, so
// it publishes nothing: it would otherwise replace the running one's target and
// name a worker no pod is starting on. The lock is written before the phase moves
// to Running, which is why holding it counts as well as Running.
func (r *StorageNodeWorkloadReconciler) migrationTargets(
	ctx context.Context, namespace string, nodes []simplyblockv1alpha2.StorageNode,
) (map[string]string, error) {
	var operations simplyblockv1alpha2.StorageNodeOpsList
	if err := r.List(ctx, &operations, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list the node operations: %w", err)
	}
	holder := make(map[string]string, len(nodes))
	for i := range nodes {
		holder[nodes[i].Name] = nodes[i].Status.ActiveOpsRef
	}

	targets := map[string]string{}
	for i := range operations.Items {
		op := &operations.Items[i]
		if op.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionMigrate {
			continue
		}
		running := op.Status.Phase == simplyblockv1alpha2.StorageNodeOpsPhaseRunning
		if running || holder[op.Spec.NodeRef] == op.Name {
			targets[op.Spec.NodeRef] = op.Spec.MigrateParams().TargetWorkerNode
		}
	}
	return targets, nil
}

// migrationOpsChanged is the StorageNodeOps watch's filter: an operation
// appearing, disappearing, or changing phase.
//
// Those are the events that change what migrationTargets returns. A step writes
// its status about every retry while it waits, and none of that changes the slice.
func migrationOpsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldOp, oldOK := e.ObjectOld.(*simplyblockv1alpha2.StorageNodeOps)
			newOp, newOK := e.ObjectNew.(*simplyblockv1alpha2.StorageNodeOps)
			return !oldOK || !newOK || oldOp.Status.Phase != newOp.Status.Phase
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// clusterOfOperation maps a Migrate to its node's cluster, so a new migration
// reaches the pass that publishes the name its first step waits for.
func (r *StorageNodeWorkloadReconciler) clusterOfOperation(
	ctx context.Context, object client.Object,
) []reconcile.Request {
	op, ok := object.(*simplyblockv1alpha2.StorageNodeOps)
	if !ok || op.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionMigrate {
		return nil
	}
	var node simplyblockv1alpha2.StorageNode
	key := types.NamespacedName{Namespace: op.Namespace, Name: op.Spec.NodeRef}
	if err := r.Get(ctx, key, &node); err != nil {
		return nil
	}
	return r.clusterOf(ctx, &node)
}

// agentAddress is what the storage-node API endpoint of a worker carries: the
// agent pod's own IP, because the agent is in the pod network.
func (r *StorageNodeWorkloadReconciler) agentAddress(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, worker string,
) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			atlaskube.LabelApp:                atlaskube.AppStorageNode,
			atlaskube.LabelSimplyblockCluster: cluster.Name,
		},
	); err != nil {
		return "", fmt.Errorf("list the storage-node agent pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == worker {
			return pods.Items[i].Status.PodIP, nil
		}
	}
	return "", nil
}

// reconcileRBAC applies the ServiceAccount the storage-node pods run as and the
// cluster role they need.
//
// The role and its binding are cluster-scoped, so they carry a managed-by label
// rather than an owner reference: a cluster-scoped object cannot be owned by a
// namespaced one, and Kubernetes garbage-collects one that tries.
func (r *StorageNodeWorkloadReconciler) reconcileRBAC(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	if err := r.shareServiceAccount(ctx, cluster); err != nil {
		return err
	}

	wl := cluster.Spec.StorageNodes
	isOpenShift := wl != nil && wl.OpenShift != nil
	if err := r.apply(ctx, utils.BuildStorageNodeSetClusterRole(isOpenShift)); err != nil {
		return err
	}
	return r.apply(ctx, utils.BuildStorageNodeSetClusterRoleBinding(cluster.Namespace))
}

// shareServiceAccount makes the cluster an owner of the ServiceAccount the
// storage-node pods run as.
//
// The control plane's SPDK pod template names the account, so every cluster in a
// namespace runs as the same one. Each cluster owns it without controlling it,
// which is what keeps it until the last of them is deleted: a controller
// reference would let the second cluster take it from the first, and deleting
// the second would then take it from the first one's pods. An owner reference
// written as a controller by an earlier operator is rewritten as a plain one.
func (r *StorageNodeWorkloadReconciler) shareServiceAccount(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	account := utils.BuildStorageNodeSetServiceAccount(cluster.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var existing corev1.ServiceAccount
		err := r.Get(ctx, client.ObjectKeyFromObject(account), &existing)
		if apierrors.IsNotFound(err) {
			desired := account.DeepCopy()
			if err := controllerutil.SetOwnerReference(cluster, desired, r.Scheme); err != nil {
				return err
			}
			return r.Create(ctx, desired)
		}
		if err != nil {
			return err
		}
		before := len(existing.OwnerReferences)
		controlled := metav1.GetControllerOf(&existing) != nil
		if err := controllerutil.SetOwnerReference(cluster, &existing, r.Scheme); err != nil {
			return err
		}
		for i := range existing.OwnerReferences {
			existing.OwnerReferences[i].Controller = nil
		}
		if len(existing.OwnerReferences) == before && !controlled {
			return nil
		}
		return r.Update(ctx, &existing)
	})
}

// reconcileCertificates applies the serving certificates of the two Services.
// A member's are its supercluster's, as its Services are.
func (r *StorageNodeWorkloadReconciler) reconcileCertificates(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	if cluster.Spec.SuperclusterRef != "" {
		return nil
	}
	tls := r.TLS.Settings(ctx)
	if err := utils.ApplyServingCertificates(ctx, r.Client, r.Scheme, cluster, tls.Enabled, tls.Provider); err != nil {
		return err
	}
	return utils.OwnServingSecrets(ctx, r.Client, r.Scheme, cluster, tls.Enabled)
}

// apply creates an object or updates it in place, which is what every object here
// but the two with server-assigned fields needs.
func (r *StorageNodeWorkloadReconciler) apply(ctx context.Context, desired client.Object) error {
	existing := desired.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, desired)
}

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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

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

	TLSEnabled       bool
	TLSMutualEnabled bool
	TLSProvider      string

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
		For(&simplyblockv1alpha2.StorageCluster{}).
		Named("storagenode-workload").
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Watches(&simplyblockv1alpha2.StorageNode{},
			handler.EnqueueRequestsFromMapFunc(r.clusterOf)).
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
		{"the endpoint slice", r.reconcileEndpointSlice},
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
	secretVersion, err := r.tlsSecretVersion(ctx, cluster.Namespace)
	if err != nil {
		return err
	}

	desired := utils.BuildStorageNodeDaemonSet(cluster,
		r.TLSEnabled, r.TLSMutualEnabled, r.TLSProvider, secretVersion, image)
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
		return wl.Image, nil
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
	if managed := controlPlane.Spec.Source.Local; managed != nil && managed.Image != "" {
		return managed.Image, nil
	}
	return "", fmt.Errorf(
		"spec.storageNodes.image is unset and ControlPlane %s states no managed image",
		SingletonControlPlaneName)
}

// tlsSecretVersion is what a certificate rotation is noticed by.
func (r *StorageNodeWorkloadReconciler) tlsSecretVersion(
	ctx context.Context, namespace string,
) (string, error) {
	if !r.TLSEnabled {
		return "", nil
	}
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: utils.SecretNameStorageNodeSetAPITLS}
	err := r.Get(ctx, key, &secret)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return secret.ResourceVersion, nil
}

// reconcileService applies the headless Service the per-pod DNS names hang off.
func (r *StorageNodeWorkloadReconciler) reconcileService(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	for _, desired := range []*corev1.Service{
		utils.BuildStorageNodeService(cluster, r.TLSEnabled, r.TLSProvider),
		utils.BuildSpdkProxyService(cluster, r.TLSEnabled, r.TLSProvider),
	} {
		if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
			return err
		}
		var existing corev1.Service
		err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, desired); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		// The cluster IP is assigned by Kubernetes and may not be rewritten, so
		// it is carried forward rather than re-stated.
		desired.ResourceVersion = existing.ResourceVersion
		desired.Spec.ClusterIP = existing.Spec.ClusterIP
		if err := r.Update(ctx, desired); err != nil {
			return err
		}
	}
	return nil
}

// reconcileEndpointSlice publishes one per-pod DNS name per worker the cluster's
// nodes run on.
//
// The slice is built from the StorageNode objects rather than from a list on the
// cluster, because the nodes are what say which workers are in the storage plane
// now: a relocation adds the target before the node's own spec.workerNode moves.
func (r *StorageNodeWorkloadReconciler) reconcileEndpointSlice(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	log := logf.FromContext(ctx)

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(cluster.Namespace)); err != nil {
		return fmt.Errorf("list the cluster's nodes: %w", err)
	}

	addresses := map[string]string{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster.Name {
			continue
		}
		if _, known := addresses[node.Spec.WorkerNode]; known {
			continue
		}
		address, err := r.workerAddress(ctx, node.Spec.WorkerNode)
		if err != nil || address == "" {
			log.V(1).Info("a worker has no internal address yet and is not published",
				"worker", node.Spec.WorkerNode)
			continue
		}
		addresses[node.Spec.WorkerNode] = address
	}

	desired := utils.BuildStorageNodeEndpointSlice(cluster, addresses)
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	var existing discoveryv1.EndpointSlice
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, desired)
}

// workerAddress is the worker's internal IP, which is what an endpoint carries.
func (r *StorageNodeWorkloadReconciler) workerAddress(
	ctx context.Context, worker string,
) (string, error) {
	var object corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: worker}, &object); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	for _, address := range object.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			return address.Address, nil
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
	account := utils.BuildStorageNodeSetServiceAccount(cluster.Namespace)
	if err := controllerutil.SetControllerReference(cluster, account, r.Scheme); err != nil {
		return err
	}
	if err := r.apply(ctx, account); err != nil {
		return err
	}

	isOpenShift := false
	if wl := cluster.Spec.StorageNodes; wl != nil && wl.OpenShiftCluster != nil {
		isOpenShift = *wl.OpenShiftCluster
	}
	if err := r.apply(ctx, utils.BuildStorageNodeSetClusterRole(isOpenShift)); err != nil {
		return err
	}
	return r.apply(ctx, utils.BuildStorageNodeSetClusterRoleBinding(cluster.Namespace))
}

// reconcileCertificates applies the serving certificates cert-manager issues for
// the two Services, where the deployment uses cert-manager at all. OpenShift's
// service-ca issues its own from an annotation on the Service, so there is nothing
// to apply there.
func (r *StorageNodeWorkloadReconciler) reconcileCertificates(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) error {
	if !r.TLSEnabled || !utils.IsCertManagerTLSProvider(r.TLSProvider) {
		return nil
	}

	for _, certificate := range []struct{ service, secret string }{
		{"simplyblock-storage-node-api", utils.SecretNameStorageNodeSetAPITLS},
		{"simplyblock-spdk-proxy", utils.SecretNameSpdkProxyTLS},
	} {
		object := utils.BuildServiceServingCertificate(
			cluster.Namespace, certificate.service, certificate.secret)
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, object, func() error {
			desired := utils.BuildServiceServingCertificate(
				cluster.Namespace, certificate.service, certificate.secret)
			object.Object["spec"] = desired.Object["spec"]
			return controllerutil.SetControllerReference(cluster, object, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("apply the serving certificate for %s: %w", certificate.service, err)
		}
	}
	return nil
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

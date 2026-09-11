// The StoragePool reconciler: one tenancy unit within a StorageCluster, carried
// from a spec somebody applied to a pool the control plane has created.
//
// The reconcile is short because most of what changes about a pool is desired
// state rather than an operation. What it does, in order:
//
//  1. Deletion, held while a class is assigned or a volume is bound, then the
//     backend DELETE, then the finalizer.
//  2. The finalizer and the owner reference the cluster holds it by.
//  3. Creation, claimed in Kubernetes before the control plane is touched,
//     because the POST is not idempotent.
//  4. The default pool's one class, written once and never repaired.
//  5. spec.allowedNodes resolved into status, and pushed to the node labels and
//     the control plane's host list.
//  6. The classes assigned to this pool, indexed into status.
//  7. The rest of status, read back from the control plane.
//
// The controller writes one StorageClass and repairs none. Classes are authored,
// with the single exception of the one written for a cluster's default pool, and
// everything else it does with a class is read. See assignment.go for why the
// join is labels rather than a reference, and design-storagepool.md §4 and §6
// for the specification.

package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// FinalizerStoragePool is what holds a pool in Terminating while anything
// Kubernetes knows about still refers to it.
const FinalizerStoragePool = utils.FinalizerStoragePool

// How long to wait before looking again. A held deletion and a cluster that is
// not finished are both states somebody else has to leave, so they are checked
// on a slow timer; a failed control-plane call is retried sooner because it is
// as likely to be transient as not.
const (
	requeueHeld      = 30 * time.Second
	requeueBackend   = 20 * time.Second
	requeueNotReady  = 10 * time.Second
	requeueContended = 5 * time.Second
)

// StoragePoolReconciler reconciles a StoragePool.
type StoragePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// VolumeScopes, if set, receives this pool's (cluster, pool) scope so the
	// control-plane stream manager keeps its volumes in the cache the aggregated
	// metrics API reads. Optional, and nil in tests.
	VolumeScopes *cpinformer.ScopeSet

	// NewAPIClient builds the control-plane client. It is a field so a test can
	// point the reconciler at a mock server; nil selects the real one.
	NewAPIClient func() *webapi.Client

	// reportedMissingNodes remembers which unresolved spec.allowedNodes entries
	// have already been announced, so a name left behind by a removed node is
	// one event rather than one per reconcile forever. The authored list is
	// deliberately not pruned, so without this the event would repeat for the
	// life of the pool.
	reportedMissingNodes sync.Map
}

// poolDTO is the control plane's storage-pool response.
type poolDTO struct {
	ID           string   `json:"id"`
	ClusterID    string   `json:"cluster_id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	MaxRwIOPS    int64    `json:"max_rw_iops"`
	MaxRwMbytes  int64    `json:"max_rw_mbytes"`
	MaxRMbytes   int64    `json:"max_r_mbytes"`
	MaxWMbytes   int64    `json:"max_w_mbytes"`
	QoSHost      string   `json:"qos_host,omitempty"`
	AllowedHosts []string `json:"allowed_hosts"`
}

// legacyPoolDTO is the pre-DTO response shape, which names the same values
// differently. It is still read because a deployment that has not taken the v2
// API answers in it.
type legacyPoolDTO struct {
	UUID         string   `json:"uuid"`
	QoSIOPSLimit int64    `json:"max_rw_ios_per_sec"`
	RWLimit      int64    `json:"max_rw_mbytes_per_sec"`
	RLimit       int64    `json:"max_r_mbytes_per_sec"`
	WLimit       int64    `json:"max_w_mbytes_per_sec"`
	QoSHost      string   `json:"qos_host,omitempty"`
	Status       string   `json:"status"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

func (r *legacyPoolDTO) toDTO() poolDTO {
	return poolDTO{
		ID:           r.UUID,
		Status:       r.Status,
		MaxRwIOPS:    r.QoSIOPSLimit,
		MaxRwMbytes:  r.RWLimit,
		MaxRMbytes:   r.RLimit,
		MaxWMbytes:   r.WLimit,
		QoSHost:      r.QoSHost,
		AllowedHosts: r.AllowedHosts,
	}
}

// parsePoolResponse reads either shape, told apart by which identity field is
// present rather than by a version negotiation, because the response carries no
// version and the two keys cannot both appear.
func parsePoolResponse(data []byte) (poolDTO, error) {
	var probe struct {
		ID   string `json:"id"`
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return poolDTO{}, fmt.Errorf("parse the pool response: %w", err)
	}
	switch {
	case probe.ID != "":
		var dto poolDTO
		if err := json.Unmarshal(data, &dto); err != nil {
			return poolDTO{}, fmt.Errorf("parse the pool response: %w", err)
		}
		return dto, nil
	case probe.UUID != "":
		var legacy legacyPoolDTO
		if err := json.Unmarshal(data, &legacy); err != nil {
			return poolDTO{}, fmt.Errorf("parse the legacy pool response: %w", err)
		}
		return legacy.toDTO(), nil
	default:
		return poolDTO{}, fmt.Errorf(
			"the pool response carries neither an id nor a uuid: %s", string(data))
	}
}

type poolHostParams struct {
	HostNQN string `json:"host_nqn"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one StoragePool toward its spec. It never blocks: every wait
// is a requeue, and every control-plane call that fails is retried on a timer
// rather than held open.
func (r *StoragePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	p := &simplyblockv1alpha2.StoragePool{}
	if err := r.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cluster, clusterUUID, err := r.resolveCluster(ctx, p)
	switch {
	case errors.Is(err, utils.ErrClusterNotFound), errors.Is(err, utils.ErrClusterUUIDNotReady):
		// Neither is a mistake the pool can be blamed for. A cluster that does
		// not exist is refused at admission, so an object in this state was
		// written while the webhook was not serving; a cluster without a UUID is
		// simply not finished, which is the ordinary state of a manifest that
		// declares a cluster and its pools in one apply. Both hold at Pending.
		if !p.DeletionTimestamp.IsZero() {
			// A pool whose cluster went away still deletes through the same
			// path, holds and all. That path is reached with an empty cluster
			// UUID and skips only the backend call: this is the cascade, so it
			// is where the holds matter most rather than least.
			return r.reconcileDeletion(ctx, p, nil, "")
		}
		r.event(p, corev1.EventTypeNormal, ClusterNotReady,
			"waiting for StorageCluster %q in namespace %s: %v",
			p.Spec.ClusterRef, p.Namespace, err)
		return r.hold(ctx, p, simplyblockv1alpha2.StoragePoolPhasePending, err.Error(), requeueNotReady)
	case err != nil:
		return ctrl.Result{}, err
	}

	api := r.apiClient()

	if !p.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, p, api, clusterUUID)
	}

	if changed, err := r.ensureFinalizerAndOwner(ctx, p, cluster); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		// The object was rewritten, so this pass is working from a stale copy.
		return ctrl.Result{}, nil
	}

	if p.Status.UUID == "" {
		return r.reconcileCreate(ctx, p, api, clusterUUID)
	}

	if r.VolumeScopes != nil {
		r.VolumeScopes.Add(cpinformer.Scope{clusterUUID, p.Status.UUID})
	}

	if err := r.ensureDefaultClass(ctx, p, clusterUUID); err != nil {
		log.Error(err, "writing the default pool's storage class")
		return ctrl.Result{RequeueAfter: requeueNotReady}, nil
	}

	// spec.limits is mutable, so raising a pool's capacity has to reach the
	// control plane. It runs before the status sync because status.observedGeneration
	// is written there: reporting a generation as observed while the control
	// plane still enforces the previous ceilings is the one outcome worse than
	// not applying them at all, since it is indistinguishable from success.
	if err := r.applyLimits(ctx, api, clusterUUID, p); err != nil {
		log.Error(err, "applying the pool's limits to the control plane")
		return ctrl.Result{RequeueAfter: requeueBackend}, nil
	}

	resolved := r.resolveAllowedNodes(ctx, p)
	if err := r.syncNodeLabels(ctx, p, resolved); err != nil {
		log.Error(err, "syncing the pool's node labels")
		return ctrl.Result{RequeueAfter: requeueNotReady}, nil
	}
	if err := r.syncAllowedHosts(ctx, api, clusterUUID, p, resolved); err != nil {
		log.Error(err, "syncing the pool's allowed hosts")
		return ctrl.Result{RequeueAfter: requeueBackend}, nil
	}

	classNames, err := r.indexAssignedClasses(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}

	return r.syncStatus(ctx, p, api, clusterUUID, resolved, classNames)
}

// resolveCluster reads the StorageCluster the pool names, which is both the
// owner the pool is held by and the source of the UUID every backend call needs.
func (r *StoragePoolReconciler) resolveCluster(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) (*simplyblockv1alpha1.StorageCluster, string, error) {
	var cluster simplyblockv1alpha1.StorageCluster
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.ClusterRef}, &cluster)
	switch {
	case apierrors.IsNotFound(err):
		return nil, "", fmt.Errorf("%w: cluster %q in namespace %q",
			utils.ErrClusterNotFound, p.Spec.ClusterRef, p.Namespace)
	case err != nil:
		return nil, "", err
	case cluster.Status.UUID == "":
		return &cluster, "", fmt.Errorf("%w: cluster %q in namespace %q",
			utils.ErrClusterUUIDNotReady, p.Spec.ClusterRef, p.Namespace)
	}
	return &cluster, cluster.Status.UUID, nil
}

// ensureFinalizerAndOwner adds both in one write. They belong together because
// they are two halves of one decision: the cluster owns its pools so that
// deleting a cluster reaches them, and the finalizer is what stops that cascade
// destroying tenant data. Establishing one without the other is the dangerous
// half of either.
func (r *StoragePoolReconciler) ensureFinalizerAndOwner(
	ctx context.Context,
	p *simplyblockv1alpha2.StoragePool,
	cluster *simplyblockv1alpha1.StorageCluster,
) (bool, error) {
	base := p.DeepCopy()

	changed := controllerutil.AddFinalizer(p, FinalizerStoragePool)
	if cluster != nil && !metav1.IsControlledBy(p, cluster) {
		if err := controllerutil.SetControllerReference(cluster, p, r.Scheme); err != nil {
			// Another controller already owns the object. That is not something
			// this reconcile can resolve, and taking the reference would be
			// wrong, so the pool works without the cascade rather than failing.
			logf.FromContext(ctx).Error(err, "leaving the pool without a controller reference")
		} else {
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, r.Patch(ctx, p, client.MergeFrom(base))
}

// reconcileCreate claims the creation in Kubernetes, then makes it.
//
// The claim is a status patch under an optimistic lock, and it is what makes a
// non-idempotent POST safe: a second reconciler that also saw an empty
// status.uuid patches the same resourceVersion, gets a 409, and backs off. The
// adoption below is the other half — the case where the POST succeeded and the
// status write that recorded it did not.
func (r *StoragePoolReconciler) reconcileCreate(
	ctx context.Context,
	p *simplyblockv1alpha2.StoragePool,
	api *webapi.Client,
	clusterUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if p.Status.Phase != simplyblockv1alpha2.StoragePoolPhasePending {
		base := p.DeepCopy()
		p.Status.Phase = simplyblockv1alpha2.StoragePoolPhasePending
		p.Status.Message = "creating the pool in the control plane"
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, p, patch); err != nil {
			log.Info("another reconciler claimed this pool's creation, backing off", "pool", p.Name)
			return ctrl.Result{RequeueAfter: requeueContended}, nil
		}
	}

	// A pool the control plane already has under this name is adopted rather
	// than created again. It is how a POST whose status write was lost recovers,
	// and it is why the create path is reached at most once per pool in practice.
	if existing, err := utils.GetPoolByName(ctx, api, clusterUUID, p.Name); err == nil && existing != nil {
		log.Info("adopting the pool the control plane already has", "pool", p.Name, "uuid", existing.UUID)
		return r.recordCreated(ctx, p, poolDTO{
			ID:          existing.UUID,
			Status:      existing.Status,
			QoSHost:     existing.QoSHost,
			MaxRwIOPS:   existing.MaxRwIOPS,
			MaxRwMbytes: existing.RWLimit,
			MaxRMbytes:  existing.RLimit,
			MaxWMbytes:  existing.WLimit,
		})
	}

	params := utils.PoolAddParams{
		Name:          p.Name,
		PoolMax:       parseSize(limitCapacity(p)),
		VolumeMaxSize: parseSize(limitMaxVolumeSize(p)),
		MaxRwIOPS:     int(int32Value(limitIOPS(p))),
		MaxRwMB:       int(int32Value(limitThroughput(p, readWrite))),
		MaxRMB:        int(int32Value(limitThroughput(p, read))),
		MaxWMB:        int(int32Value(limitThroughput(p, write))),
		DHCHAP:        volumeDefaultsDHCHAP(p),
		CRName:        p.Name,
		CRNameSpace:   p.Namespace,
		CRPlural:      "storagepools",
	}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, status, err := api.Do(ctx, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "the control plane refused to create the pool",
			"status", status, "response", string(body))
		r.event(p, corev1.EventTypeWarning, PoolCreationFailed,
			"the control plane refused to create pool %q (status %d): %s",
			p.Name, status, string(body))
		return ctrl.Result{RequeueAfter: requeueBackend}, nil
	}

	dto, err := parsePoolResponse(body)
	if err != nil {
		log.Error(err, "parsing the pool creation response", "raw", string(body))
		return ctrl.Result{RequeueAfter: requeueNotReady}, nil
	}
	r.event(p, corev1.EventTypeNormal, PoolCreated,
		"created pool %q in the control plane as %s", p.Name, dto.ID)
	return r.recordCreated(ctx, p, dto)
}

// recordCreated writes the UUID the pool is now known by. Everything else about
// the status is left to the sync pass, which reads the control plane rather than
// the create response and is therefore the one place status is assembled.
func (r *StoragePoolReconciler) recordCreated(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool, dto poolDTO,
) (ctrl.Result, error) {
	base := p.DeepCopy()
	p.Status.UUID = dto.ID
	p.Status.Status = dto.Status
	p.Status.Phase = simplyblockv1alpha2.StoragePoolPhaseReady
	p.Status.Message = ""
	p.Status.Limits = limitsStatusFromDTO(dto)
	p.Status.ObservedGeneration = p.Generation
	if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
		return ctrl.Result{RequeueAfter: requeueNotReady}, nil //nolint:nilerr // the UUID is recoverable by adoption
	}
	return ctrl.Result{Requeue: true}, nil
}

// ensureDefaultClass writes the one class the operator creates, and only for the
// pool a cluster's own creation path made.
//
// It is written once. status.defaultStorageClassName records that it was, so a
// class that is absent while the field is set was deleted deliberately and is
// not written again: recreating it would be the operator arguing with an
// administrator about a class the pool does not need in order to work.
func (r *StoragePoolReconciler) ensureDefaultClass(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool, clusterUUID string,
) error {
	if p.Name != DefaultPoolName(p.Spec.ClusterRef) || p.Status.DefaultStorageClassName != "" {
		return nil
	}

	name := DefaultStorageClassName(p.Namespace, p.Spec.ClusterRef)
	labels := AssignmentLabels(p)
	labels[LabelManagedBy] = ManagedByStorageCluster

	class := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Labels: labels},
		Provisioner: utils.CSIProvisioner,
		Parameters:  ClassParameters(p, clusterUUID),
		// WaitForFirstConsumer, because a volume is placed where its workload
		// runs and binding it before the scheduler has chosen would place it
		// somewhere else. It is deliberately not marked as Kubernetes' default
		// class: that annotation makes every claim naming no class bind through
		// this driver cluster-wide, which is a decision an administrator can see
		// the consequences of and this operator cannot.
		VolumeBindingMode:    ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
		ReclaimPolicy:        ptr.To(corev1.PersistentVolumeReclaimDelete),
		AllowVolumeExpansion: ptr.To(true),
	}
	err := r.Create(ctx, class)
	switch {
	case apierrors.IsAlreadyExists(err):
		// The name is taken. A StorageClass is cluster-scoped, so the occupant
		// may be nothing to do with this pool, and recording it as the pool's
		// default would leave the pool claiming a class that provisions
		// somewhere else — and never writing the one it needs, since this runs
		// once. The name is only adopted when the object is recognizably the
		// one this operator would have written.
		existing := &storagev1.StorageClass{}
		if getErr := r.Get(ctx, client.ObjectKey{Name: name}, existing); getErr != nil {
			return fmt.Errorf("read the storage class %q that already exists: %w", name, getErr)
		}
		if !r.isOurDefaultClass(existing, p) {
			r.event(p, corev1.EventTypeWarning, StorageClassNameTaken,
				"storage class %q already exists and is not this pool's, so the default pool has "+
					"none; assign a class to it by label, or delete the class occupying the name",
				name)
			return nil
		}
	case err != nil:
		return fmt.Errorf("create the default storage class %q: %w", name, err)
	}

	base := p.DeepCopy()
	p.Status.DefaultStorageClassName = name
	if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record the default storage class on pool %s/%s: %w",
			p.Namespace, p.Name, err)
	}
	r.event(p, corev1.EventTypeNormal, StorageClassCreated,
		"wrote storage class %q for the default pool", name)
	return nil
}

// isOurDefaultClass reports whether an existing class is the one this operator
// would have written for this pool.
//
// The test is the assignment labels plus the managed marker, and not the
// parameters: a class the operator wrote under an earlier release carries the
// older QoS spelling, and parameters are immutable, so demanding they match
// would refuse the operator's own class forever.
func (r *StoragePoolReconciler) isOurDefaultClass(
	class *storagev1.StorageClass, p *simplyblockv1alpha2.StoragePool,
) bool {
	if !IsOperatorManaged(class) || class.Provisioner != utils.CSIProvisioner {
		return false
	}
	for key, want := range AssignmentLabels(p) {
		if class.Labels[key] != want {
			return false
		}
	}
	return true
}

// resolveAllowedNodes turns the authored list into the one the world can honor.
//
// A name that resolves to no Node is dropped and announced once. The authored
// list is left exactly as written: rewriting a user's spec to match the world
// makes the object stop recording what was asked for, and it destroys the case
// the field exists for, which is a node removed for maintenance and added back
// under the same name returning to the pools that named it.
func (r *StoragePoolReconciler) resolveAllowedNodes(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) []corev1.Node {
	resolved := make([]corev1.Node, 0, len(p.Spec.AllowedNodes))
	for _, name := range p.Spec.AllowedNodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			key := p.Namespace + "/" + p.Name + "/" + name
			if _, announced := r.reportedMissingNodes.LoadOrStore(key, struct{}{}); !announced {
				r.event(p, corev1.EventTypeWarning, AllowedNodeMissing,
					"spec.allowedNodes names %q, which resolves to no node; it is ignored "+
						"and left in the spec, so it takes effect again if the node returns", name)
			}
			continue
		}
		r.reportedMissingNodes.Delete(p.Namespace + "/" + p.Name + "/" + name)
		resolved = append(resolved, node)
	}
	return resolved
}

// syncNodeLabels puts the pool's allowed-node label on every resolved node and
// takes it off every node that is no longer in the set. The key carries the
// pool's UUID, so this is only reachable once the pool exists in the control
// plane.
func (r *StoragePoolReconciler) syncNodeLabels(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool, resolved []corev1.Node,
) error {
	log := logf.FromContext(ctx)
	labelKey := kube.PoolNodeLabelKey(p.Status.UUID)

	var labeled corev1.NodeList
	if err := r.List(ctx, &labeled, client.MatchingLabels{labelKey: kube.LabelPoolAllowed}); err != nil {
		return fmt.Errorf("list the nodes carrying %s: %w", labelKey, err)
	}

	want := make(map[string]struct{}, len(resolved))
	for i := range resolved {
		want[resolved[i].Name] = struct{}{}
	}

	have := make(map[string]struct{}, len(labeled.Items))
	for i := range labeled.Items {
		node := &labeled.Items[i]
		have[node.Name] = struct{}{}
		if _, ok := want[node.Name]; ok {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		delete(node.Labels, labelKey)
		if err := r.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("remove %s from node %s: %w", labelKey, node.Name, err)
		}
		log.Info("removed the pool label from a node", "node", node.Name, "label", labelKey)
	}

	for i := range resolved {
		node := resolved[i].DeepCopy()
		if _, ok := have[node.Name]; ok {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[labelKey] = kube.LabelPoolAllowed
		if err := r.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("label node %s with %s: %w", node.Name, labelKey, err)
		}
		log.Info("added the pool label to a node", "node", node.Name, "label", labelKey)
	}
	return nil
}

// syncAllowedHosts reconciles the pool's host list in the control plane against
// the resolved nodes. Each node is named by the host NQN derived from its UID,
// which is the same formula the CSI node plugin uses, so no NQN is managed by
// hand on either side.
func (r *StoragePoolReconciler) syncAllowedHosts(
	ctx context.Context,
	api *webapi.Client,
	clusterUUID string,
	p *simplyblockv1alpha2.StoragePool,
	resolved []corev1.Node,
) error {
	log := logf.FromContext(ctx)

	want := make(map[string]struct{}, len(resolved))
	for i := range resolved {
		want[nqn.Host(string(resolved[i].UID))] = struct{}{}
	}

	dto, err := r.readPool(ctx, api, clusterUUID, p.Status.UUID)
	if err != nil {
		return err
	}
	have := make(map[string]struct{}, len(dto.AllowedHosts))
	for _, h := range dto.AllowedHosts {
		have[h] = struct{}{}
	}
	if len(want) == 0 && len(have) == 0 {
		return nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/host", clusterUUID, p.Status.UUID)
	for host := range want {
		if _, ok := have[host]; ok {
			continue
		}
		if err := r.callHost(ctx, api, http.MethodPost, endpoint, host); err != nil {
			return err
		}
		log.Info("added a host to the pool", "host", host)
	}
	for host := range have {
		if _, ok := want[host]; ok {
			continue
		}
		if err := r.callHost(ctx, api, http.MethodDelete, endpoint, host); err != nil {
			return err
		}
		log.Info("removed a host from the pool", "host", host)
	}
	return nil
}

func (r *StoragePoolReconciler) callHost(
	ctx context.Context, api *webapi.Client, method, endpoint, host string,
) error {
	body, status, err := api.Do(ctx, method, endpoint, poolHostParams{HostNQN: host})
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d: %s", status, string(body))
		}
		return fmt.Errorf("%s host %s: %w", strings.ToLower(method), host, err)
	}
	return nil
}

// indexAssignedClasses publishes the classes assigned to this pool and reports
// what is wrong with them. It creates nothing and repairs nothing: a class is
// authored, and what this pass owns is making the assignment visible from the
// pool rather than only from a cluster-wide list and a mental join.
func (r *StoragePoolReconciler) indexAssignedClasses(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) ([]string, error) {
	classes, names, err := AssignedClasses(ctx, r.Client, p)
	if err != nil {
		return nil, err
	}

	known := make(map[string]struct{}, len(p.Status.StorageClassNames))
	for _, name := range p.Status.StorageClassNames {
		known[name] = struct{}{}
	}

	for i := range classes {
		class := &classes[i]
		if _, seen := known[class.Name]; !seen {
			r.event(p, corev1.EventTypeNormal, StorageClassAssigned,
				"storage class %q is assigned to this pool", class.Name)
		}
		for _, conflict := range kube.QoSParamConflicts(class.Parameters) {
			r.event(p, corev1.EventTypeWarning, QoSParameterConflict,
				"storage class %q states one ceiling under %s; %q wins and the rest are ignored, "+
					"which is a ceiling nobody chose",
				class.Name, strings.Join(conflict, " and "), conflict[0])
		}
	}
	return names, nil
}

// syncStatus reads the pool back from the control plane and writes what changed.
// A pass that finds the status unchanged patches nothing, which is what keeps a
// steady-state pool from writing to etcd on every resync.
func (r *StoragePoolReconciler) syncStatus(
	ctx context.Context,
	p *simplyblockv1alpha2.StoragePool,
	api *webapi.Client,
	clusterUUID string,
	resolved []corev1.Node,
	classNames []string,
) (ctrl.Result, error) {
	dto, err := r.readPool(ctx, api, clusterUUID, p.Status.UUID)
	if err != nil {
		logf.FromContext(ctx).Error(err, "reading the pool back from the control plane")
		return ctrl.Result{RequeueAfter: requeueBackend}, nil
	}

	base := p.DeepCopy()
	p.Status.Status = dto.Status
	p.Status.Limits = limitsStatusFromDTO(dto)
	p.Status.StorageClassNames = classNames
	p.Status.AllowedNodes = nodeNames(resolved)
	p.Status.Phase = simplyblockv1alpha2.StoragePoolPhaseReady
	p.Status.Message = ""
	p.Status.ObservedGeneration = p.Generation

	// An authored list that resolves to nothing is a pool that can place
	// nothing, which is not the same as a pool that named no nodes at all.
	if len(p.Spec.AllowedNodes) > 0 && len(resolved) == 0 {
		p.Status.Phase = simplyblockv1alpha2.StoragePoolPhasePending
		p.Status.Message = "every node in spec.allowedNodes resolves to nothing, so the pool can place no volume"
	}

	if equality(base.Status, p.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// applyLimits sends spec.limits to the control plane when the pool's generation
// has moved past what status last observed.
//
// The generation is the trigger rather than a comparison against what the
// control plane reports, because the two vocabularies do not line up: a capacity
// of "10T" becomes a byte count, and an unset ceiling becomes zero, so a diff
// against the reported values would send an update on every pass for a pool that
// asked for nothing. A generation only moves when somebody edited the spec.
func (r *StoragePoolReconciler) applyLimits(
	ctx context.Context,
	api *webapi.Client,
	clusterUUID string,
	p *simplyblockv1alpha2.StoragePool,
) error {
	if p.Status.ObservedGeneration == p.Generation {
		return nil
	}

	params := utils.PoolUpdateParams{
		Name:          p.Name,
		PoolMax:       parseSize(limitCapacity(p)),
		VolumeMaxSize: parseSize(limitMaxVolumeSize(p)),
		MaxRwIOPS:     int(int32Value(limitIOPS(p))),
		MaxRwMB:       int(int32Value(limitThroughput(p, readWrite))),
		MaxRMB:        int(int32Value(limitThroughput(p, read))),
		MaxWMB:        int(int32Value(limitThroughput(p, write))),
	}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, p.Status.UUID)
	body, status, err := api.Do(ctx, http.MethodPut, endpoint, params)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d: %s", status, string(body))
		}
		return fmt.Errorf("update pool %s: %w", p.Name, err)
	}
	logf.FromContext(ctx).Info("applied the pool's limits",
		"pool", p.Name, "generation", p.Generation)
	return nil
}

// readPool fetches one pool from the control plane.
func (r *StoragePoolReconciler) readPool(
	ctx context.Context, api *webapi.Client, clusterUUID, poolUUID string,
) (poolDTO, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, poolUUID)
	body, status, err := api.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d: %s", status, string(body))
		}
		return poolDTO{}, fmt.Errorf("read pool %s: %w", poolUUID, err)
	}
	return parsePoolResponse(body)
}

// hold parks the pool in a phase and says why, without touching anything else.
func (r *StoragePoolReconciler) hold(
	ctx context.Context,
	p *simplyblockv1alpha2.StoragePool,
	phase simplyblockv1alpha2.StoragePoolPhase,
	message string,
	after time.Duration,
) (ctrl.Result, error) {
	if p.Status.Phase != phase || p.Status.Message != message {
		base := p.DeepCopy()
		p.Status.Phase = phase
		p.Status.Message = message
		p.Status.ObservedGeneration = p.Generation
		if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func (r *StoragePoolReconciler) event(
	object client.Object, eventType, reason, format string, args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(object, nil, eventType, reason, reason, format, args...)
}

func (r *StoragePoolReconciler) apiClient() *webapi.Client {
	if r.NewAPIClient != nil {
		return r.NewAPIClient()
	}
	return webapi.NewClient()
}

// SetupWithManager registers the reconciler and the one watch that is not on the
// pool itself: a StorageClass carries no owner reference to the pool it names, so
// an assignment written or withdrawn would otherwise not be noticed until the
// next resync.
func (r *StoragePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StoragePool{}).
		Watches(&storagev1.StorageClass{}, handler.EnqueueRequestsFromMapFunc(poolForClass)).
		Named("storagepool").
		Complete(r)
}

// poolForClass maps a StorageClass back to the pool its labels assign it to. A
// class carrying an incomplete set of labels maps to nothing, which is the
// honest answer: the assignment is all three labels or none.
func poolForClass(_ context.Context, object client.Object) []reconcile.Request {
	labels := object.GetLabels()
	namespace, pool := labels[LabelNamespace], labels[LabelPool]
	if namespace == "" || pool == "" || labels[LabelCluster] == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{Namespace: namespace, Name: pool},
	}}
}

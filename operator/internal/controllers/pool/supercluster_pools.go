// A pool whose clusterRef names a supercluster (design-superclusters.md §7.1).
//
// The supercluster has no backend cluster, so the pool has no backend pool of
// its own: it is created in every member under its own name, which is what lets
// one StorageClass, naming the pool by name, reach it in whichever member the
// CSI driver is told to place a volume. Each member's backend pool admits the
// whole allowed-node list, because a volume of the pool may land in any member
// and be consumed from any allowed node, and each is held to its share of the
// pool's capacity ceiling (shares.go).
//
// It is a branch of the pool reconciler rather than a controller of its own
// because everything around the backend calls is the same: the finalizer and
// owner, the holds on deletion, the classes assigned by label, and the resolved
// node list. What differs is that every backend call is made once per member,
// authenticated as that member, and that status reports one row per member.

package pool

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// superclusterOf returns the supercluster the pool names, or nil when the pool
// names an ordinary cluster or one that cannot be found, both of which the
// ordinary path handles.
func (r *StoragePoolReconciler) superclusterOf(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool,
) (*simplyblockv1alpha2.StorageCluster, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.ClusterRef}, &cluster)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	case cluster.Spec.Supercluster == nil:
		return nil, nil
	}
	return &cluster, nil
}

// poolLabelID is what the pool's allowed-node label key is derived from: its
// backend UUID, or for a pool of a supercluster, which has one backend pool per
// member and no UUID of its own, its object UID, so that one label and one
// class serve every member. Empty until the pool exists in the control plane.
func poolLabelID(p *simplyblockv1alpha2.StoragePool) string {
	switch {
	case p.Status.UUID != "":
		return p.Status.UUID
	case len(p.Status.Members) > 0:
		return string(p.UID)
	}
	return ""
}

// reconcileSuperclusterPool drives the pool in every member, then writes one
// status for all of them.
func (r *StoragePoolReconciler) reconcileSuperclusterPool(
	ctx context.Context, p *simplyblockv1alpha2.StoragePool, supercluster *simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	members, err := utils.SuperclusterMembers(ctx, r.Client, supercluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !p.DeletionTimestamp.IsZero() {
		return r.reconcileSuperclusterDeletion(ctx, p, members)
	}
	if changed, err := r.ensureFinalizerAndOwner(ctx, p, supercluster); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{}, nil
	}

	resolved := r.resolveAllowedNodes(ctx, p)
	previous := make(map[string]simplyblockv1alpha2.PoolMemberStatus, len(p.Status.Members))
	for _, row := range p.Status.Members {
		previous[row.Cluster] = row
	}
	inputs := make([]memberShare, 0, len(members))
	for i := range members {
		row := previous[members[i].Name]
		inputs = append(inputs, memberShare{
			Name:      members[i].Name,
			Accepting: utils.AcceptsPlacement(&members[i]),
			HasPool:   row.UUID != "",
			Applied:   appliedCapacity(row),
		})
	}
	shares := capacityShares(parseSize(limitCapacity(p)), inputs)

	rows := make([]simplyblockv1alpha2.PoolMemberStatus, 0, len(members))
	var waiting []string
	for i := range members {
		member := &members[i]
		row := r.reconcileMemberPool(ctx, p, member, previous[member.Name], shares[member.Name], resolved)
		rows = append(rows, row)
		if row.Phase != simplyblockv1alpha2.StoragePoolPhaseReady && utils.AcceptsPlacement(member) {
			waiting = append(waiting, member.Name)
		}
	}

	if len(rows) > 0 {
		if err := r.syncNodeLabels(ctx, string(p.UID), resolved); err != nil {
			log.Error(err, "syncing the pool's node labels")
			return ctrl.Result{RequeueAfter: requeueNotReady}, nil
		}
	}
	if supercluster.Status.UUID != "" {
		if err := r.ensureDefaultClass(ctx, p, supercluster.Status.UUID); err != nil {
			log.Error(err, "writing the default pool's storage class")
			return ctrl.Result{RequeueAfter: requeueNotReady}, nil
		}
	}
	classNames, err := r.indexAssignedClasses(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}

	base := p.DeepCopy()
	p.Status.UUID = ""
	p.Status.Status = ""
	p.Status.Limits = nil
	p.Status.Members = rows
	p.Status.StorageClassNames = classNames
	p.Status.AllowedNodes = nodeNames(resolved)
	p.Status.ObservedGeneration = p.Generation
	p.Status.Phase, p.Status.Message = superclusterPoolPhase(p, members, resolved, waiting)
	if !equality(base.Status, p.Status) {
		if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	for _, row := range rows {
		if row.Phase != simplyblockv1alpha2.StoragePoolPhaseReady {
			return ctrl.Result{RequeueAfter: requeueBackend}, nil
		}
	}
	return ctrl.Result{}, nil
}

// superclusterPoolPhase is Ready once every member accepting volumes holds
// the pool. A member that accepts none, because it is being built, is not
// serving, or has placement disabled, holds nothing back: it receives the pool
// all the same, and the supercluster does not place in it until it does.
func superclusterPoolPhase(
	p *simplyblockv1alpha2.StoragePool,
	members []simplyblockv1alpha2.StorageCluster,
	resolved []corev1.Node,
	waiting []string,
) (simplyblockv1alpha2.StoragePoolPhase, string) {
	switch {
	case len(members) == 0:
		return simplyblockv1alpha2.StoragePoolPhasePending,
			fmt.Sprintf("supercluster %q has no member to create the pool in", p.Spec.ClusterRef)
	case len(p.Spec.AllowedNodes) > 0 && len(resolved) == 0:
		return simplyblockv1alpha2.StoragePoolPhasePending,
			"every node in spec.allowedNodes resolves to nothing, so the pool can place no volume"
	case len(waiting) > 0:
		return simplyblockv1alpha2.StoragePoolPhasePending,
			fmt.Sprintf("the pool is not yet in members %s", strings.Join(waiting, ", "))
	}
	return simplyblockv1alpha2.StoragePoolPhaseReady, ""
}

// appliedCapacity is the capacity a member's backend pool last reported.
func appliedCapacity(row simplyblockv1alpha2.PoolMemberStatus) int64 {
	if row.Limits == nil {
		return 0
	}
	return capacityBytes(row.Limits.Capacity)
}

// reconcileMemberPool brings the pool in one member to its spec: created or
// adopted, held to its share, admitting the allowed nodes, and read back. A
// failure is reported in the member's row and retried, and does not stop the
// other members.
func (r *StoragePoolReconciler) reconcileMemberPool(
	ctx context.Context,
	p *simplyblockv1alpha2.StoragePool,
	member *simplyblockv1alpha2.StorageCluster,
	row simplyblockv1alpha2.PoolMemberStatus,
	share int64,
	resolved []corev1.Node,
) simplyblockv1alpha2.PoolMemberStatus {
	log := logf.FromContext(ctx).WithValues("member", member.Name)
	row.Cluster = member.Name
	if member.Status.UUID == "" {
		row.Phase = simplyblockv1alpha2.StoragePoolPhasePending
		row.Message = "the member has no backend cluster yet"
		return row
	}

	// Each call authenticates as the member, as a pool of that member would.
	if secret, err := r.clusterSecret(ctx, member); err == nil && secret != "" {
		ctx = webapi.WithBearerToken(ctx, secret)
	}
	api := r.apiClient(ctx)
	fail := func(err error) simplyblockv1alpha2.PoolMemberStatus {
		log.Error(err, "reconciling the pool in a member")
		if row.UUID == "" {
			row.Phase = simplyblockv1alpha2.StoragePoolPhasePending
		}
		row.Message = err.Error()
		return row
	}

	if row.UUID == "" {
		uuid, err := r.createMemberPool(ctx, api, p, member, share)
		if err != nil {
			r.event(p, corev1.EventTypeWarning, PoolCreationFailed,
				"the control plane refused to create pool %q in member %s: %v", p.Name, member.Name, err)
			return fail(err)
		}
		row.UUID = uuid
		r.event(p, corev1.EventTypeNormal, PoolCreated,
			"created pool %q in member %s as %s", p.Name, member.Name, uuid)
	}
	if r.VolumeScopes != nil {
		r.VolumeScopes.Add(cpinformer.Scope{member.Status.UUID, row.UUID})
	}

	if share != appliedCapacity(row) || p.Status.ObservedGeneration != p.Generation {
		if err := r.updateMemberPool(ctx, api, p, member.Status.UUID, row.UUID, share); err != nil {
			return fail(err)
		}
	}
	if err := r.syncAllowedHosts(ctx, api, member.Status.UUID, row.UUID, resolved); err != nil {
		return fail(err)
	}
	dto, err := r.readPool(ctx, api, member.Status.UUID, row.UUID)
	if err != nil {
		return fail(err)
	}
	row.Phase = simplyblockv1alpha2.StoragePoolPhaseReady
	row.Limits = limitsStatusFromDTO(dto)
	row.Message = ""
	return row
}

// createMemberPool creates the pool in one member, or adopts the one the member
// already has under the pool's name. The lookup comes first and a 409 is read
// as the pool existing, so a create whose answer was lost is adopted rather
// than sent again.
func (r *StoragePoolReconciler) createMemberPool(
	ctx context.Context,
	api *webapi.Client,
	p *simplyblockv1alpha2.StoragePool,
	member *simplyblockv1alpha2.StorageCluster,
	share int64,
) (string, error) {
	adopt := func() (string, error) {
		existing, err := utils.GetPoolByName(ctx, api, member.Status.UUID, p.Name)
		switch {
		case err != nil:
			return "", err
		case existing == nil:
			return "", nil
		}
		return existing.UUID, nil
	}
	if uuid, err := adopt(); err != nil || uuid != "" {
		return uuid, err
	}

	params := utils.PoolAddParams{
		Name:          p.Name,
		PoolMax:       share,
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
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", member.Status.UUID)
	body, status, err := api.Do(ctx, http.MethodPost, endpoint, params)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusConflict:
		uuid, err := adopt()
		if err == nil && uuid == "" {
			err = fmt.Errorf("the control plane reports pool %q exists and does not list it", p.Name)
		}
		return uuid, err
	case status >= 300:
		return "", fmt.Errorf("status %d: %s", status, string(body))
	}
	dto, err := parsePoolResponse(body)
	if err != nil {
		return "", err
	}
	return dto.ID, nil
}

// updateMemberPool sends the pool's limits to one member, its capacity
// replaced by the member's share.
func (r *StoragePoolReconciler) updateMemberPool(
	ctx context.Context,
	api *webapi.Client,
	p *simplyblockv1alpha2.StoragePool,
	clusterUUID, poolUUID string,
	share int64,
) error {
	params := utils.PoolUpdateParams{
		Name:          p.Name,
		PoolMax:       share,
		VolumeMaxSize: parseSize(limitMaxVolumeSize(p)),
		MaxRwIOPS:     int(int32Value(limitIOPS(p))),
		MaxRwMB:       int(int32Value(limitThroughput(p, readWrite))),
		MaxRMB:        int(int32Value(limitThroughput(p, read))),
		MaxWMB:        int(int32Value(limitThroughput(p, write))),
	}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, poolUUID)
	body, status, err := api.Do(ctx, http.MethodPut, endpoint, params)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		return fmt.Errorf("update pool %s: %w", p.Name, err)
	}
	return nil
}

// reconcileSuperclusterDeletion holds the deletion on the same two things an
// ordinary pool's is held on, then deletes the backend pool in every member.
func (r *StoragePoolReconciler) reconcileSuperclusterDeletion(
	ctx context.Context,
	p *simplyblockv1alpha2.StoragePool,
	members []simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(p, FinalizerStoragePool) {
		return ctrl.Result{}, nil
	}
	if held, result, err := r.holdDeletion(ctx, p); held || err != nil {
		return result, err
	}

	byName := make(map[string]*simplyblockv1alpha2.StorageCluster, len(members))
	for i := range members {
		byName[members[i].Name] = &members[i]
	}
	var failed []string
	remaining := make([]simplyblockv1alpha2.PoolMemberStatus, 0, len(p.Status.Members))
	for _, row := range p.Status.Members {
		member := byName[row.Cluster]
		if row.UUID == "" || member == nil || member.Status.UUID == "" {
			// A member that is gone took its backend pool with it.
			continue
		}
		mctx := ctx
		if secret, err := r.clusterSecret(ctx, member); err == nil && secret != "" {
			mctx = webapi.WithBearerToken(ctx, secret)
		}
		if r.VolumeScopes != nil {
			r.VolumeScopes.Remove(cpinformer.Scope{member.Status.UUID, row.UUID})
		}
		gone := p.DeepCopy()
		gone.Status.UUID = row.UUID
		if err := r.deleteBackendPool(mctx, r.apiClient(mctx), member.Status.UUID, gone); err != nil {
			r.event(p, corev1.EventTypeWarning, PoolDeletionFailed,
				"the control plane refused to delete pool %q in member %s: %v", p.Name, member.Name, err)
			failed = append(failed, member.Name)
			remaining = append(remaining, row)
		}
	}
	if len(failed) > 0 {
		base := p.DeepCopy()
		p.Status.Members = remaining
		p.Status.Phase = simplyblockv1alpha2.StoragePoolPhaseDeleting
		p.Status.Message = fmt.Sprintf("deleting the pool in members %s", strings.Join(failed, ", "))
		if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueBackend}, nil
	}

	if err := r.syncNodeLabels(ctx, string(p.UID), nil); err != nil {
		logf.FromContext(ctx).Error(err, "clearing the pool's node labels")
		return ctrl.Result{RequeueAfter: requeueNotReady}, nil
	}
	return r.releaseFinalizer(ctx, p)
}

// poolsOfMember maps a member StorageCluster to the pools of its supercluster,
// so a member that joins, starts serving, or leaves placement is given the
// pool, or has its share rewritten, without waiting for a resync.
func (r *StoragePoolReconciler) poolsOfMember(ctx context.Context, obj client.Object) []reconcile.Request {
	member, ok := obj.(*simplyblockv1alpha2.StorageCluster)
	if !ok {
		return nil
	}
	pools := poolsForMember(ctx, r.Client, member)
	requests := make([]reconcile.Request, 0, len(pools))
	for i := range pools {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools[i])})
	}
	return requests
}

// poolsForMember lists the pools naming the member's supercluster, in name
// order. A cluster that is no member has none.
func poolsForMember(
	ctx context.Context, reader client.Reader, member *simplyblockv1alpha2.StorageCluster,
) []simplyblockv1alpha2.StoragePool {
	if member.Spec.SuperclusterRef == "" {
		return nil
	}
	var pools simplyblockv1alpha2.StoragePoolList
	if err := reader.List(ctx, &pools, client.InNamespace(member.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "listing the pools of a member's supercluster", "member", member.Name)
		return nil
	}
	out := make([]simplyblockv1alpha2.StoragePool, 0, len(pools.Items))
	for i := range pools.Items {
		if pools.Items[i].Spec.ClusterRef == member.Spec.SuperclusterRef {
			out = append(out, pools.Items[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

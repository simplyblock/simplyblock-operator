// The supercluster reconciler: it reconciles a StorageCluster carrying
// spec.supercluster, which has no backend cluster of its own, by finding the
// member clusters whose spec.superclusterRef names it and rolling their status
// up into its own (design-superclusters.md §10.1), and by building the two
// Services its members are reached through, with their serving certificates
// (§6.2).
//
// It lives beside StorageClusterReconciler because both reconcile the same
// kind and split it by one predicate: each object has exactly one reconciler,
// and the two share the control-plane surface and the status-write discipline
// of this package. It writes nothing to the control plane, so it needs no
// finalizer and takes no lock.

package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/simplyblock/atlas/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// superclusterResync is how often the supercluster polls its members'
// capacity. Member phases arrive by watch; capacity has no stream.
const superclusterResync = 60 * time.Second

// superclusterRefIndex indexes StorageClusters by the supercluster they name,
// so a supercluster lists its members without reading every cluster.
const superclusterRefIndex = "spec.superclusterRef"

// superclusterRefOf is the index function for superclusterRefIndex.
func superclusterRefOf(obj client.Object) []string {
	cluster, ok := obj.(*simplyblockv1alpha2.StorageCluster)
	if !ok || cluster.Spec.SuperclusterRef == "" {
		return nil
	}
	return []string{cluster.Spec.SuperclusterRef}
}

// isSupercluster reports whether a StorageCluster is a supercluster.
func isSupercluster(cluster *simplyblockv1alpha2.StorageCluster) bool {
	return cluster.Spec.Supercluster != nil
}

// roleOf is what a cluster is within a supercluster, read from its spec.
func roleOf(cluster *simplyblockv1alpha2.StorageCluster) simplyblockv1alpha2.StorageClusterRole {
	switch {
	case isSupercluster(cluster):
		return simplyblockv1alpha2.StorageClusterRoleSupercluster
	case cluster.Spec.SuperclusterRef != "":
		return simplyblockv1alpha2.StorageClusterRolePlacementGroup
	default:
		return simplyblockv1alpha2.StorageClusterRoleStandalone
	}
}

// SuperclusterReconciler reconciles StorageClusters carrying spec.supercluster.
type SuperclusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Capacity reads each member's capacity from the control plane's metrics
	// exporter. Nil leaves the supercluster's capacity unreported.
	Capacity MemberCapacitySource

	// TLSEnabled and TLSProvider decide the serving certificate of the
	// Services the members share (§6.2), as they do a standalone cluster's.
	TLSEnabled  bool
	TLSProvider string
}

// MemberCapacitySource supplies a member's latest capacity sample, and whether
// there is one. It is satisfied by atlas-lib's prometheus.Provider.
type MemberCapacitySource interface {
	ClusterCapacity(ctx context.Context, clusterUUID string) (prometheus.Capacity, bool, error)
}

// SetupWithManager registers the index the members are listed through, the
// supercluster as the reconciled object, and every member as a trigger for the
// supercluster it names.
func (r *SuperclusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&simplyblockv1alpha2.StorageCluster{}, superclusterRefIndex, superclusterRefOf); err != nil {
		return fmt.Errorf("index StorageClusters by superclusterRef: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageCluster{},
			ctrlbuilder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				cluster, ok := obj.(*simplyblockv1alpha2.StorageCluster)
				return ok && isSupercluster(cluster)
			}))).
		Watches(&simplyblockv1alpha2.StorageCluster{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				refs := superclusterRefOf(obj)
				if len(refs) == 0 {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: obj.GetNamespace(), Name: refs[0],
				}}}
			})).
		Watches(&simplyblockv1alpha2.StoragePool{}, handler.EnqueueRequestsFromMapFunc(superclusterOfPool)).
		Owns(&corev1.Service{}).
		Named("supercluster").
		Complete(r)
}

func (r *SuperclusterReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var supercluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, req.NamespacedName, &supercluster); err != nil {
		// A supercluster that is gone reports nothing; one that could not be
		// read this pass keeps what it last reported.
		if apierrors.IsNotFound(err) {
			forgetSupercluster(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !isSupercluster(&supercluster) || !supercluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	var members simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &members, client.InNamespace(supercluster.Namespace),
		client.MatchingFields{superclusterRefIndex: supercluster.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list the members of %s: %w", supercluster.Name, err)
	}

	pools, err := r.superclusterPools(ctx, &supercluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	readings := make([]memberReading, 0, len(members.Items))
	missing := make(map[string][]string, len(members.Items))
	for i := range members.Items {
		member := &members.Items[i]
		reading := memberReading{
			Cluster:      member,
			Capacity:     r.memberCapacity(ctx, &supercluster, member),
			MissingPools: missingPools(pools, member.Name),
		}
		missing[member.Name] = reading.MissingPools
		readings = append(readings, reading)
	}
	rollup := rollUp(readings)

	previous := supercluster.Status.DeepCopy()
	err = writeClusterStatus(ctx, r.Client, &supercluster,
		func(fresh *simplyblockv1alpha2.StorageCluster, status *simplyblockv1alpha2.StorageClusterStatus) {
			status.UUID = string(fresh.UID)
			status.Role = simplyblockv1alpha2.StorageClusterRoleSupercluster
			// The supercluster's Services are its own, and its members are
			// reached through them (§6.2), so its names never are the
			// constant ones a standalone cluster may hold.
			if status.WorkloadNames == nil {
				status.WorkloadNames = utils.WorkloadNamesStatus(atlaskube.DerivedWorkloadNames(fresh.Name))
			}
			status.Phase = rollup.Phase
			status.Message = rollup.Message
			status.Members = rollup.Members
			status.MemberCount = &rollup.MemberCount
			status.Capacity = rollup.Capacity
			status.MaxFaultTolerance = rollup.MaxFaultTolerance
		})
	if err != nil {
		return ctrl.Result{}, err
	}

	// The Services the members are reached through are the supercluster's,
	// built under the names the write above recorded (§6.2).
	if err := utils.ApplyWorkloadServices(ctx, r.Client, r.Scheme, &supercluster,
		r.TLSEnabled, r.TLSProvider); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply the shared Services: %w", err)
	}
	if err := utils.ApplyServingCertificates(ctx, r.Client, r.Scheme, &supercluster,
		r.TLSEnabled, r.TLSProvider); err != nil {
		return ctrl.Result{}, err
	}
	if err := utils.OwnServingSecrets(ctx, r.Client, r.Scheme, &supercluster, r.TLSEnabled); err != nil {
		return ctrl.Result{}, err
	}

	// The supercluster's own pool, which its generated class is written for
	// (§7.1). Its members are clusters with default pools of their own.
	ensureDefaultPool(ctx, r.Client, r.Scheme, &supercluster)

	r.announceMembership(&supercluster, previous.Members, rollup.Members, missing)
	r.announceThreshold(&supercluster, previous.Capacity, rollup.Capacity)
	observeSupercluster(&supercluster, rollup)
	return ctrl.Result{RequeueAfter: superclusterResync}, nil
}

// memberCapacity is one member's latest sample from the metrics exporter
// (§16 Q13), or nil when there is no exporter, no backend cluster, or no
// sample. A read that fails keeps the member's last reading, so an exporter
// blip does not take the member's capacity out of the supercluster's sum until
// the next pass.
func (r *SuperclusterReconciler) memberCapacity(
	ctx context.Context,
	supercluster, member *simplyblockv1alpha2.StorageCluster,
) *simplyblockv1alpha2.AggregateCapacity {
	if r.Capacity == nil || member.Status.UUID == "" {
		return nil
	}
	sample, ok, err := r.Capacity.ClusterCapacity(ctx, member.Status.UUID)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("a member's capacity could not be read; keeping its last reading",
			"supercluster", supercluster.Name, "member", member.Name, "error", err.Error())
		for _, row := range supercluster.Status.Members {
			if row.Name == member.Name {
				return row.Capacity
			}
		}
		return nil
	}
	if !ok || sample.SampledAt.IsZero() {
		return nil
	}
	return &simplyblockv1alpha2.AggregateCapacity{
		TotalBytes:       &sample.Total,
		UsedBytes:        &sample.Used,
		ProvisionedBytes: &sample.Provisioned,
	}
}

// announceMembership emits MemberAdded for a member the previous status did not
// list, and MemberPlacementChanged for one whose eligibility flipped.
func (r *SuperclusterReconciler) announceMembership(
	supercluster *simplyblockv1alpha2.StorageCluster,
	before, after []simplyblockv1alpha2.SuperclusterMemberStatus,
	missing map[string][]string,
) {
	known := make(map[string]simplyblockv1alpha2.SuperclusterMemberStatus, len(before))
	for _, row := range before {
		known[row.Name] = row
	}
	for _, row := range after {
		previous, ok := known[row.Name]
		if !ok {
			r.Recorder.Eventf(supercluster, nil, corev1.EventTypeNormal, MemberAdded, MemberAdded,
				"Member %s joined the supercluster", row.Name)
			continue
		}
		if previous.PlacementEligible == row.PlacementEligible {
			continue
		}
		if row.PlacementEligible {
			r.Recorder.Eventf(supercluster, nil, corev1.EventTypeNormal,
				MemberPlacementChanged, MemberPlacementChanged,
				"Member %s returned to placement", row.Name)
			continue
		}
		r.Recorder.Eventf(supercluster, nil, corev1.EventTypeNormal,
			MemberPlacementChanged, MemberPlacementChanged,
			"Member %s left placement: %s", row.Name, ineligibleReason(row, missing[row.Name]))
	}
}

// ineligibleReason says why a member's row is not placement-eligible.
func ineligibleReason(row simplyblockv1alpha2.SuperclusterMemberStatus, missingPools []string) string {
	switch {
	case row.Phase == "":
		return "it has not reported a phase"
	case !isServing(row.Phase):
		return fmt.Sprintf("it is %s", row.Phase)
	case len(missingPools) > 0:
		return fmt.Sprintf("it does not hold pools %s yet", strings.Join(missingPools, ", "))
	}
	return "placement is disabled on it"
}

// superclusterPools lists the pools naming the supercluster.
func (r *SuperclusterReconciler) superclusterPools(
	ctx context.Context, supercluster *simplyblockv1alpha2.StorageCluster,
) ([]simplyblockv1alpha2.StoragePool, error) {
	var list simplyblockv1alpha2.StoragePoolList
	if err := r.List(ctx, &list, client.InNamespace(supercluster.Namespace)); err != nil {
		return nil, fmt.Errorf("list the pools of %s: %w", supercluster.Name, err)
	}
	pools := make([]simplyblockv1alpha2.StoragePool, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef == supercluster.Name {
			pools = append(pools, list.Items[i])
		}
	}
	return pools, nil
}

// missingPools are the pools whose row for the member does not report the pool
// Ready, in name order. A pool being deleted is not required of anyone, and
// neither is one the pool controller has not reached yet, so that creating a
// pool does not take every member out of placement for the moment before its
// first pass.
func missingPools(pools []simplyblockv1alpha2.StoragePool, member string) []string {
	var missing []string
	for i := range pools {
		p := &pools[i]
		if !p.DeletionTimestamp.IsZero() || len(p.Status.Members) == 0 {
			continue
		}
		held := false
		for _, row := range p.Status.Members {
			if row.Cluster == member && row.Phase == simplyblockv1alpha2.StoragePoolPhaseReady {
				held = true
				break
			}
		}
		if !held {
			missing = append(missing, p.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

// superclusterOfPool maps a pool to the cluster it names, so a pool reaching a
// member, or created, re-decides that member's placement. A pool of an
// ordinary cluster maps to that cluster, which the predicate-free reconcile
// returns from at once.
func superclusterOfPool(_ context.Context, obj client.Object) []reconcile.Request {
	p, ok := obj.(*simplyblockv1alpha2.StoragePool)
	if !ok || p.Spec.ClusterRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: p.Namespace, Name: p.Spec.ClusterRef,
	}}}
}

// capacityLevel is how far the combined capacity is into the supercluster's
// thresholds: zero below warning, one at or above warning, two at or above
// critical.
func capacityLevel(
	spec simplyblockv1alpha2.StorageClusterSpec, capacity *simplyblockv1alpha2.AggregateCapacity,
) (level int, percent int64) {
	if capacity == nil || capacity.TotalBytes == nil || capacity.UsedBytes == nil || *capacity.TotalBytes <= 0 {
		return 0, 0
	}
	percent = *capacity.UsedBytes * 100 / *capacity.TotalBytes
	crossed := func(threshold *simplyblockv1alpha2.CapacityThresholdSpec) bool {
		return threshold != nil && threshold.Capacity != nil && percent >= *threshold.Capacity
	}
	switch {
	case crossed(spec.CriticalThreshold):
		return 2, percent
	case crossed(spec.WarningThreshold):
		return 1, percent
	}
	return 0, percent
}

// announceThreshold emits AggregateCapacityThreshold when the combined
// capacity rises into a threshold it was not in before. The previous level is
// read from the previous status, so the event fires once per crossing rather
// than once per poll, and an operator restart does not repeat it.
func (r *SuperclusterReconciler) announceThreshold(
	supercluster *simplyblockv1alpha2.StorageCluster,
	before, after *simplyblockv1alpha2.AggregateCapacity,
) {
	was, _ := capacityLevel(supercluster.Spec, before)
	now, percent := capacityLevel(supercluster.Spec, after)
	if now <= was {
		return
	}
	threshold := "warning"
	if now == 2 {
		threshold = "critical"
	}
	r.Recorder.Eventf(supercluster, nil, corev1.EventTypeWarning,
		AggregateCapacityThreshold, AggregateCapacityThreshold,
		"The members' combined capacity is %d%% used, past the supercluster's %s threshold",
		percent, threshold)
}

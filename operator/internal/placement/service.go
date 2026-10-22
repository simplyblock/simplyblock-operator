// Package placement answers which member of a supercluster a new volume is
// created in (design-superclusters.md §7.2, §7.3, §9.3).
//
// The CSI controller asks over the link when a class names a supercluster in
// cluster_id. The operator answers because it holds what the answer is made
// of: the supercluster's view of its members, each member's capacity reading,
// the topology of the workers the members run on, and the placements made
// since the last reading, which no single CreateVolume can see.
//
// A placement is recorded on the claim before the answer, so a retried
// CreateVolume is answered with the member the first attempt was sent to,
// whether or not the volume is visible there yet. That record, the lookup of
// the volume's name in every member, and the serialization of placements per
// supercluster are what keep a retry or a burst from creating a volume twice.
package placement

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/placement/placementrpc"
	"github.com/simplyblock/atlas/prometheus"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// AnnotationPlacementMember records on a claim the backend UUID of the member
// its volume was placed in.
const AnnotationPlacementMember = "storage.simplyblock.io/placement-member"

// AggregateCapacityExhausted is the event on a supercluster when no member has
// room for a requested volume.
const AggregateCapacityExhausted = "AggregateCapacityExhausted"

// readingTTL is how old a member's capacity reading may be before a placement
// refreshes it: two of the supercluster's polling intervals.
const readingTTL = 2 * time.Minute

// refreshTimeout bounds a refresh of a stale reading, because it sits on the
// CreateVolume path.
const refreshTimeout = 2 * time.Second

// exhaustedEventInterval is the least time between two
// AggregateCapacityExhausted events on one supercluster.
const exhaustedEventInterval = time.Minute

// MemberLookup asks a member's control plane what it already holds.
type MemberLookup interface {
	// VolumeExists reports whether the member's backend pool holds a volume
	// of that name.
	VolumeExists(ctx context.Context, member *simplyblockv1alpha2.StorageCluster, poolUUID, name string) (bool, error)
	// GroupMembers is the number of volumes in the member's consistency group
	// of that name, zero when it has none.
	GroupMembers(ctx context.Context, member *simplyblockv1alpha2.StorageCluster, group string) (int, error)
}

// MemberCapacitySource supplies a member's latest capacity sample. It is
// satisfied by atlas-lib's prometheus.Provider.
type MemberCapacitySource interface {
	ClusterCapacity(ctx context.Context, clusterUUID string) (prometheus.Capacity, bool, error)
}

// Service places volumes among a supercluster's members.
type Service struct {
	Client   client.Client
	Lookup   MemberLookup
	Capacity MemberCapacitySource
	Recorder events.EventRecorder

	// The clock is Now and the draw among equal members is Draw. Nil means
	// the wall clock and a random draw.
	Now  func() time.Time
	Draw func(n int) int

	mu       sync.Mutex
	locks    map[types.UID]*sync.Mutex
	ledgers  map[string]*ledger // by member UUID
	groups   map[string]string  // supercluster UID + "/" + group: member UUID
	lastFull map[types.UID]time.Time
}

// ledger is what the service knows of one member's capacity: its latest
// reading, and what it placed since.
type ledger struct {
	reading    prometheus.Capacity
	hasReading bool
	placed     []placed
}

// placed is one placement the member's next reading may not yet include.
type placed struct {
	volume string
	size   int64
	at     time.Time
}

// Observe records a member's capacity reading, which the supercluster
// reconciler reads every polling interval. It clears the placements the
// reading was taken after, because the reading accounts for them.
func (s *Service) Observe(memberUUID string, reading prometheus.Capacity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ledgerOf(memberUUID)
	if l.hasReading && reading.SampledAt.Before(l.reading.SampledAt) {
		return
	}
	l.reading, l.hasReading = reading, true
	kept := l.placed[:0]
	for _, p := range l.placed {
		if p.at.After(reading.SampledAt) {
			kept = append(kept, p)
		}
	}
	l.placed = kept
}

func (s *Service) ledgerOf(memberUUID string) *ledger {
	if s.ledgers == nil {
		s.ledgers = map[string]*ledger{}
	}
	l, ok := s.ledgers[memberUUID]
	if !ok {
		l = &ledger{}
		s.ledgers[memberUUID] = l
	}
	return l
}

// lockFor serializes the placements of one supercluster, so two concurrent
// requests each see the size the other placed.
func (s *Service) lockFor(uid types.UID) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[types.UID]*sync.Mutex{}
	}
	l, ok := s.locks[uid]
	if !ok {
		l = &sync.Mutex{}
		s.locks[uid] = l
	}
	return l
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Place answers the member for one volume.
func (s *Service) Place(ctx context.Context, req placementrpc.Request) (string, error) {
	start := s.now()
	if req.ClaimName == "" || req.ClaimNamespace == "" {
		return "", status.Error(codes.FailedPrecondition,
			"placement needs the claim's name and namespace, which the external-provisioner passes "+
				"with --extra-create-metadata")
	}
	supercluster, err := s.supercluster(ctx, req.AggregateID)
	if err != nil {
		return "", err
	}
	lock := s.lockFor(supercluster.UID)
	lock.Lock()
	defer lock.Unlock()

	member, result, err := s.place(ctx, supercluster, req)
	observeDecision(supercluster, member, result, s.now().Sub(start))
	return memberUUID(member), err
}

// memberUUID is the answer for a chosen member, or nothing.
func memberUUID(member *simplyblockv1alpha2.StorageCluster) string {
	if member == nil {
		return ""
	}
	return member.Status.UUID
}

// place is Place under the supercluster's lock. The result labels the
// decision's metric.
func (s *Service) place(
	ctx context.Context, supercluster *simplyblockv1alpha2.StorageCluster, req placementrpc.Request,
) (*simplyblockv1alpha2.StorageCluster, string, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: req.ClaimNamespace, Name: req.ClaimName}, &pvc); err != nil {
		return nil, resultFailed, status.Errorf(codes.Unavailable, "read claim %s/%s: %v",
			req.ClaimNamespace, req.ClaimName, err)
	}
	members, err := s.members(ctx, supercluster)
	if err != nil {
		return nil, resultFailed, err
	}

	// A recorded placement is the answer, whatever has changed since.
	if recorded := pvc.Annotations[AnnotationPlacementMember]; recorded != "" {
		for i := range members {
			if members[i].Status.UUID == recorded {
				return &members[i], resultRecorded, nil
			}
		}
		return nil, resultFailed, status.Errorf(codes.FailedPrecondition,
			"claim %s/%s was placed in cluster %s, which is no longer a member of supercluster %s",
			req.ClaimNamespace, req.ClaimName, recorded, supercluster.Name)
	}

	// A clone or restore lands in its source's cluster, which the control
	// plane clones within, so the source decides and the ranking does not.
	if req.SourceClusterID != "" {
		for i := range members {
			if members[i].Status.UUID == req.SourceClusterID {
				return s.record(ctx, &pvc, &members[i], req, resultClone)
			}
		}
		return nil, resultFailed, status.Errorf(codes.InvalidArgument,
			"the clone's source is in cluster %s, which is not a member of supercluster %s, "+
				"and a clone stays in its source's cluster", req.SourceClusterID, supercluster.Name)
	}

	holder, err := s.holderOf(ctx, supercluster, members, req)
	if err != nil {
		return nil, resultFailed, err
	}
	if holder != nil {
		return s.record(ctx, &pvc, holder, req, resultExisting)
	}

	if req.ConsistencyGroup != "" {
		groupHolder, err := s.groupHolder(ctx, supercluster, members, req.ConsistencyGroup)
		if err != nil {
			return nil, resultFailed, err
		}
		if groupHolder != nil {
			return s.record(ctx, &pvc, groupHolder, req, resultGroup)
		}
	}

	chosen, err := s.rankMembers(ctx, supercluster, members, req)
	if err != nil {
		return nil, resultExhausted, err
	}
	if req.ConsistencyGroup != "" {
		s.mu.Lock()
		if s.groups == nil {
			s.groups = map[string]string{}
		}
		s.groups[string(supercluster.UID)+"/"+req.ConsistencyGroup] = chosen.Status.UUID
		s.mu.Unlock()
	}
	return s.record(ctx, &pvc, chosen, req, resultPlaced)
}

// supercluster finds the supercluster an aggregate identifier names.
func (s *Service) supercluster(ctx context.Context, aggregateID string) (*simplyblockv1alpha2.StorageCluster, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := s.Client.List(ctx, &clusters); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list the storage clusters: %v", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if c.Spec.Supercluster != nil && c.Status.UUID == aggregateID {
			return c, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no supercluster has the identifier %s", aggregateID)
}

// members lists the supercluster's members in name order.
func (s *Service) members(
	ctx context.Context, supercluster *simplyblockv1alpha2.StorageCluster,
) ([]simplyblockv1alpha2.StorageCluster, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := s.Client.List(ctx, &clusters, client.InNamespace(supercluster.Namespace)); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list the members of %s: %v", supercluster.Name, err)
	}
	var members []simplyblockv1alpha2.StorageCluster
	for i := range clusters.Items {
		if clusters.Items[i].Spec.SuperclusterRef == supercluster.Name && clusters.Items[i].Status.UUID != "" {
			members = append(members, clusters.Items[i])
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, nil
}

// holderOf finds the member that already holds a volume of the request's name,
// in every member, eligible or not. A member that cannot be asked fails the
// call, because placing without knowing is how a volume is created twice.
func (s *Service) holderOf(
	ctx context.Context,
	supercluster *simplyblockv1alpha2.StorageCluster,
	members []simplyblockv1alpha2.StorageCluster,
	req placementrpc.Request,
) (*simplyblockv1alpha2.StorageCluster, error) {
	var p simplyblockv1alpha2.StoragePool
	key := types.NamespacedName{Namespace: supercluster.Namespace, Name: req.PoolName}
	if err := s.Client.Get(ctx, key, &p); err != nil || p.Spec.ClusterRef != supercluster.Name {
		return nil, status.Errorf(codes.FailedPrecondition, "supercluster %s has no pool %q",
			supercluster.Name, req.PoolName)
	}
	pools := make(map[string]string, len(p.Status.Members))
	for _, row := range p.Status.Members {
		pools[row.Cluster] = row.UUID
	}
	for i := range members {
		poolUUID := pools[members[i].Name]
		if poolUUID == "" {
			continue
		}
		found, err := s.Lookup.VolumeExists(ctx, &members[i], poolUUID, req.VolumeName)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable,
				"could not ask member %s whether it holds volume %s: %v", members[i].Name, req.VolumeName, err)
		}
		if found {
			return &members[i], nil
		}
	}
	return nil, nil
}

// groupHolder finds the member holding the request's consistency group: the
// one the service placed the group in, or the one whose control plane has the
// group with live members. A group in two members, or in a member out of
// placement, fails the call naming them.
func (s *Service) groupHolder(
	ctx context.Context,
	supercluster *simplyblockv1alpha2.StorageCluster,
	members []simplyblockv1alpha2.StorageCluster,
	group string,
) (*simplyblockv1alpha2.StorageCluster, error) {
	var holders []*simplyblockv1alpha2.StorageCluster
	for i := range members {
		count, err := s.Lookup.GroupMembers(ctx, &members[i], group)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable,
				"could not ask member %s for consistency group %q: %v", members[i].Name, group, err)
		}
		if count > 0 {
			holders = append(holders, &members[i])
		}
	}
	if len(holders) == 0 {
		s.mu.Lock()
		placedIn := s.groups[string(supercluster.UID)+"/"+group]
		s.mu.Unlock()
		for i := range members {
			if members[i].Status.UUID == placedIn {
				holders = append(holders, &members[i])
			}
		}
	}
	switch len(holders) {
	case 0:
		return nil, nil
	case 1:
	default:
		names := make([]string, 0, len(holders))
		for _, h := range holders {
			names = append(names, h.Name)
		}
		return nil, status.Errorf(codes.FailedPrecondition,
			"consistency group %q has volumes in members %s, and a group lives in one member",
			group, strings.Join(names, " and "))
	}
	if !eligible(supercluster, holders[0].Name) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"consistency group %q lives in member %s, which is not accepting new volumes",
			group, holders[0].Name)
	}
	return holders[0], nil
}

// eligible reads the supercluster's verdict on a member.
func eligible(supercluster *simplyblockv1alpha2.StorageCluster, member string) bool {
	for _, row := range supercluster.Status.Members {
		if row.Name == member {
			return row.PlacementEligible
		}
	}
	return false
}

// rankMembers ranks the members by the supercluster's strategy and answers the
// first, or ResourceExhausted naming why each member was dropped.
func (s *Service) rankMembers(
	ctx context.Context,
	supercluster *simplyblockv1alpha2.StorageCluster,
	members []simplyblockv1alpha2.StorageCluster,
	req placementrpc.Request,
) (*simplyblockv1alpha2.StorageCluster, error) {
	volumes, err := s.volumeCounts(ctx, members)
	if err != nil {
		return nil, err
	}
	regions := s.regions(ctx, members)

	candidates := make([]candidate, 0, len(members))
	for i := range members {
		m := &members[i]
		c := candidate{
			Name:     m.Name,
			UUID:     m.Status.UUID,
			Eligible: eligible(supercluster, m.Name),
			Volumes:  volumes[m.Status.UUID],
			Region:   regions[m.Name],
		}
		if t := m.Spec.CriticalThreshold; t != nil && t.Capacity != nil {
			c.CriticalPercent = *t.Capacity
		}
		if c.Eligible {
			s.fillReading(ctx, &c)
		}
		candidates = append(candidates, c)
	}

	strategy := simplyblockv1alpha2.PlacementMostAvailableCapacity
	if p := supercluster.Spec.Supercluster.Placement; p != nil && p.Strategy != "" {
		strategy = p.Strategy
	}
	draw := s.Draw
	if draw == nil {
		draw = randomDraw
	}
	chosen, reasons := rank(strategy, candidates, req.SizeBytes, requestTopology{
		Preferred: regionsOf(req.Preferred), Requisite: regionsOf(req.Requisite),
	}, draw)
	if chosen == "" {
		s.announceExhausted(supercluster, req, reasons)
		return nil, status.Errorf(codes.ResourceExhausted,
			"no member of supercluster %s can take %d bytes in pool %q: %s",
			supercluster.Name, req.SizeBytes, req.PoolName, describe(reasons))
	}
	for i := range members {
		if members[i].Name == chosen {
			return &members[i], nil
		}
	}
	return nil, status.Errorf(codes.Internal, "the ranking chose %s, which is no member", chosen)
}

// fillReading puts the member's capacity reading on the candidate, refreshing
// one older than readingTTL, and what was placed in it since.
func (s *Service) fillReading(ctx context.Context, c *candidate) {
	s.mu.Lock()
	l := s.ledgerOf(c.UUID)
	reading, has := l.reading, l.hasReading
	s.mu.Unlock()

	if (!has || s.now().Sub(reading.SampledAt) > readingTTL) && s.Capacity != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
		fresh, ok, err := s.Capacity.ClusterCapacity(refreshCtx, c.UUID)
		cancel()
		switch {
		case err != nil:
			logf.FromContext(ctx).Info("a member's capacity reading could not be refreshed; leaving it out",
				"member", c.Name, "error", err.Error())
			return
		case ok && !fresh.SampledAt.IsZero():
			s.Observe(c.UUID, fresh)
			reading, has = fresh, true
		}
	}
	if !has || s.now().Sub(reading.SampledAt) > readingTTL {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c.HasReading = true
	c.Total, c.Used, c.Provisioned = reading.Total, reading.Used, reading.Provisioned
	for _, p := range s.ledgerOf(c.UUID).placed {
		c.Placed += p.size
	}
}

// volumeCounts is each member's volume count for LeastVolumes: the
// PersistentVolumes whose handle names it, and the volumes placed in it whose
// PersistentVolume does not exist yet.
func (s *Service) volumeCounts(
	ctx context.Context, members []simplyblockv1alpha2.StorageCluster,
) (map[string]int, error) {
	var pvs corev1.PersistentVolumeList
	if err := s.Client.List(ctx, &pvs); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list the persistent volumes: %v", err)
	}
	counts := make(map[string]int, len(members))
	names := make(map[string]bool, len(pvs.Items))
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		names[pv.Name] = true
		if !atlaskube.IsManaged(pv) {
			continue
		}
		if cluster, _, ok := strings.Cut(pv.Spec.CSI.VolumeHandle, ":"); ok {
			counts[cluster]++
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range members {
		for _, p := range s.ledgerOf(members[i].Status.UUID).placed {
			if !names[p.volume] {
				counts[members[i].Status.UUID]++
			}
		}
	}
	return counts, nil
}

// regions is each member's region, read from the labels of the workers its
// storage nodes run on. A member whose workers carry none, or disagree, has
// none, which matches any region.
func (s *Service) regions(ctx context.Context, members []simplyblockv1alpha2.StorageCluster) map[string]string {
	out := make(map[string]string, len(members))
	if len(members) == 0 {
		return out
	}
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := s.Client.List(ctx, &nodes, client.InNamespace(members[0].Namespace)); err != nil {
		return out
	}
	seen := map[string]map[string]bool{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.WorkerNode == "" {
			continue
		}
		var worker corev1.Node
		if err := s.Client.Get(ctx, types.NamespacedName{Name: n.Spec.WorkerNode}, &worker); err != nil {
			continue
		}
		region := worker.Labels[corev1.LabelTopologyRegion]
		if region == "" {
			continue
		}
		if seen[n.Spec.ClusterRef] == nil {
			seen[n.Spec.ClusterRef] = map[string]bool{}
		}
		seen[n.Spec.ClusterRef][region] = true
	}
	for name, set := range seen {
		if len(set) == 1 {
			out[name] = slices.Collect(maps.Keys(set))[0]
		}
	}
	return out
}

// regionsOf is the regions a request's topology names, in order, once each.
func regionsOf(topologies []map[string]string) []string {
	var regions []string
	for _, segments := range topologies {
		region := segments[corev1.LabelTopologyRegion]
		if region != "" && !slices.Contains(regions, region) {
			regions = append(regions, region)
		}
	}
	return regions
}

// record writes the member to the claim and counts the placement against the
// member, then answers it. A record that cannot be written fails the call, so
// a volume is never created in a member nothing says it was sent to.
func (s *Service) record(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	member *simplyblockv1alpha2.StorageCluster,
	req placementrpc.Request,
	result string,
) (*simplyblockv1alpha2.StorageCluster, string, error) {
	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[AnnotationPlacementMember] = member.Status.UUID
	if err := s.Client.Patch(ctx, pvc, patch); err != nil {
		return nil, resultFailed, status.Errorf(codes.Unavailable,
			"record the placement in %s on claim %s/%s: %v", member.Name, pvc.Namespace, pvc.Name, err)
	}
	if result == resultPlaced || result == resultGroup {
		s.mu.Lock()
		l := s.ledgerOf(member.Status.UUID)
		l.placed = append(l.placed, placed{volume: req.VolumeName, size: req.SizeBytes, at: s.now()})
		s.mu.Unlock()
	}
	return member, result, nil
}

// announceExhausted emits AggregateCapacityExhausted on the supercluster, at
// most once per exhaustedEventInterval.
func (s *Service) announceExhausted(
	supercluster *simplyblockv1alpha2.StorageCluster, req placementrpc.Request, reasons map[string]string,
) {
	s.mu.Lock()
	if s.lastFull == nil {
		s.lastFull = map[types.UID]time.Time{}
	}
	last, seen := s.lastFull[supercluster.UID]
	if seen && s.now().Sub(last) < exhaustedEventInterval {
		s.mu.Unlock()
		return
	}
	s.lastFull[supercluster.UID] = s.now()
	s.mu.Unlock()
	if s.Recorder == nil {
		return
	}
	s.Recorder.Eventf(supercluster, nil, corev1.EventTypeWarning,
		AggregateCapacityExhausted, AggregateCapacityExhausted,
		"No member has room for a volume of %d bytes in pool %q: %s", req.SizeBytes, req.PoolName, describe(reasons))
}

// describe writes each member's reason, in name order.
func describe(reasons map[string]string) string {
	names := slices.Sorted(maps.Keys(reasons))
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s: %s", name, reasons[name]))
	}
	if len(parts) == 0 {
		return "the supercluster has no members"
	}
	return strings.Join(parts, "; ")
}

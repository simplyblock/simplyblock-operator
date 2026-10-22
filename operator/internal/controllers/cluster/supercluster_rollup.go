// The supercluster's status rollup: one phase, one capacity, and one fault
// tolerance computed from its members (design-superclusters.md §4.3).
//
// It is a pure function of the members and their capacity readings, kept
// apart from the reconciler that gathers them, because the phase table is the
// part of the supercluster an administrator reads and every row of it is
// tested without a client.

package cluster

import (
	"fmt"
	"sort"
	"strings"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// memberReading is one member as the supercluster reconciler found it.
type memberReading struct {
	Cluster *simplyblockv1alpha2.StorageCluster

	// Capacity is the member's latest capacity sample, nil when the control
	// plane had none to give.
	Capacity *simplyblockv1alpha2.AggregateCapacity

	// MissingPools are the supercluster's pools the member does not hold yet.
	MissingPools []string
}

// superclusterRollup is everything the supercluster's status reports about its
// members.
type superclusterRollup struct {
	Phase             simplyblockv1alpha2.StorageClusterPhase
	Message           string
	Members           []simplyblockv1alpha2.SuperclusterMemberStatus
	MemberCount       int32
	Capacity          *simplyblockv1alpha2.AggregateCapacity
	MaxFaultTolerance *int32
}

// rollUp computes the supercluster's status from its members. The member rows
// are in name order, so that a status written twice from the same members is
// the same status and produces no write.
func rollUp(members []memberReading) superclusterRollup {
	sorted := make([]memberReading, len(members))
	copy(sorted, members)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Cluster.Name < sorted[j].Cluster.Name })

	out := superclusterRollup{
		Members:     make([]simplyblockv1alpha2.SuperclusterMemberStatus, 0, len(sorted)),
		MemberCount: int32(len(sorted)), //nolint:gosec // a member count is small
	}
	phases := make([]simplyblockv1alpha2.StorageClusterPhase, 0, len(sorted))
	for _, m := range sorted {
		phase := m.Cluster.Status.Phase
		phases = append(phases, phase)
		out.Members = append(out.Members, simplyblockv1alpha2.SuperclusterMemberStatus{
			Name:              m.Cluster.Name,
			UUID:              m.Cluster.Status.UUID,
			Phase:             phase,
			PlacementEligible: placementEligible(m),
			Capacity:          m.Capacity,
		})
		out.Capacity = addCapacity(out.Capacity, m.Capacity)
	}
	out.Phase, out.Message = rollUpPhase(sorted, phases)
	out.MaxFaultTolerance = lowestFaultTolerance(sorted)
	return out
}

// isServing reports whether a member's phase is one in which it serves I/O.
func isServing(phase simplyblockv1alpha2.StorageClusterPhase) bool {
	return utils.IsServingPhase(phase)
}

// placementEligible reports whether new volumes may be placed in a member:
// it is serving, has not been taken out of placement, and holds every pool of
// the supercluster. The fourth condition of design-superclusters.md §10.1, no
// removal running, arrives with removal.
func placementEligible(m memberReading) bool {
	return utils.AcceptsPlacement(m.Cluster) && len(m.MissingPools) == 0
}

// rollUpPhase is the phase table, and the sentence explaining the row it
// picked.
// isBuilding reports whether a member is still being built, which it is until
// it has been activated: its backend cluster may exist while its nodes are
// added (Provisioning) and while it activates.
func isBuilding(phase simplyblockv1alpha2.StorageClusterPhase) bool {
	switch phase {
	case "", simplyblockv1alpha2.StorageClusterPhasePending, simplyblockv1alpha2.StorageClusterPhaseCreating,
		simplyblockv1alpha2.StorageClusterPhaseProvisioning, simplyblockv1alpha2.StorageClusterPhaseActivating:
		return true
	}
	return false
}

func rollUpPhase(
	members []memberReading, phases []simplyblockv1alpha2.StorageClusterPhase,
) (simplyblockv1alpha2.StorageClusterPhase, string) {
	if len(members) == 0 {
		return simplyblockv1alpha2.StorageClusterPhaseProvisioning,
			"no member cluster names this supercluster yet"
	}

	var building, notOnline, suspended, unavailable []string
	serving := 0
	for i, phase := range phases {
		name := members[i].Cluster.Name
		switch {
		case isBuilding(phase):
			building = append(building, name)
		case phase == simplyblockv1alpha2.StorageClusterPhaseSuspended:
			suspended = append(suspended, name)
		case phase == simplyblockv1alpha2.StorageClusterPhaseUnavailable:
			unavailable = append(unavailable, name)
		}
		if isServing(phase) {
			serving++
		}
		if phase != simplyblockv1alpha2.StorageClusterPhaseOnline {
			notOnline = append(notOnline, name)
		}
	}

	switch {
	case len(building) > 0:
		return simplyblockv1alpha2.StorageClusterPhaseProvisioning,
			fmt.Sprintf("members still being created: %s", strings.Join(building, ", "))
	case len(notOnline) == 0:
		return simplyblockv1alpha2.StorageClusterPhaseOnline,
			fmt.Sprintf("all %d members are online", len(members))
	case serving > 0:
		return simplyblockv1alpha2.StorageClusterPhaseDegraded,
			fmt.Sprintf("members not online: %s", strings.Join(notOnline, ", "))
	case len(suspended) > 0 && len(unavailable) == 0:
		return simplyblockv1alpha2.StorageClusterPhaseSuspended,
			fmt.Sprintf("no member is serving, and %s are suspended", strings.Join(suspended, ", "))
	default:
		return simplyblockv1alpha2.StorageClusterPhaseUnavailable,
			fmt.Sprintf("no member is serving: %s", strings.Join(notOnline, ", "))
	}
}

// lowestFaultTolerance is the weakest member's tolerance. A member that
// reports none makes the aggregate's unknown, because reading the others'
// would claim a tolerance nobody measured.
func lowestFaultTolerance(members []memberReading) *int32 {
	var lowest *int32
	for _, m := range members {
		ftt := m.Cluster.Status.MaxFaultTolerance
		if ftt == nil {
			return nil
		}
		if lowest == nil || *ftt < *lowest {
			lowest = ptr.To(*ftt)
		}
	}
	return lowest
}

// addCapacity adds one member's reading to the running sum. A member with no
// reading adds nothing, and a sum no member contributed to stays nil.
func addCapacity(
	sum, reading *simplyblockv1alpha2.AggregateCapacity,
) *simplyblockv1alpha2.AggregateCapacity {
	if reading == nil {
		return sum
	}
	if sum == nil {
		sum = &simplyblockv1alpha2.AggregateCapacity{}
	}
	add := func(total **int64, v *int64) {
		if v == nil {
			return
		}
		if *total == nil {
			*total = ptr.To(int64(0))
		}
		**total += *v
	}
	add(&sum.TotalBytes, reading.TotalBytes)
	add(&sum.UsedBytes, reading.UsedBytes)
	add(&sum.ProvisionedBytes, reading.ProvisionedBytes)
	return sum
}

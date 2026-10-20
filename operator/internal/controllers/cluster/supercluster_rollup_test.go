// Tests for the supercluster's status rollup (design-superclusters.md §4.3):
// the phase table, the lowest fault tolerance, the capacity sums, and which
// members placement may choose. The rollup is a pure function of the members
// and their readings, so these tests need no client.

package cluster

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func rollupMember(
	name string, phase simplyblockv1alpha2.StorageClusterPhase,
	mutate ...func(*simplyblockv1alpha2.StorageCluster),
) memberReading {
	c := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(name),
		Spec:       simplyblockv1alpha2.StorageClusterSpec{SuperclusterRef: "prod"},
		Status:     simplyblockv1alpha2.StorageClusterStatus{Phase: phase},
	}
	for _, m := range mutate {
		m(c)
	}
	return memberReading{Cluster: c}
}

func withFTT(ftt int32) func(*simplyblockv1alpha2.StorageCluster) {
	return func(c *simplyblockv1alpha2.StorageCluster) { c.Status.MaxFaultTolerance = ptr.To(ftt) }
}

func reading(total, used, provisioned int64) *simplyblockv1alpha2.AggregateCapacity {
	return &simplyblockv1alpha2.AggregateCapacity{
		TotalBytes: ptr.To(total), UsedBytes: ptr.To(used), ProvisionedBytes: ptr.To(provisioned),
	}
}

func TestTheSuperclusterPhaseRollsUpFromItsMembers(t *testing.T) {
	const (
		online      = simplyblockv1alpha2.StorageClusterPhaseOnline
		degraded    = simplyblockv1alpha2.StorageClusterPhaseDegraded
		rebalancing = simplyblockv1alpha2.StorageClusterPhaseRebalancing
		unavailable = simplyblockv1alpha2.StorageClusterPhaseUnavailable
		suspended   = simplyblockv1alpha2.StorageClusterPhaseSuspended
		pending     = simplyblockv1alpha2.StorageClusterPhasePending
		creating    = simplyblockv1alpha2.StorageClusterPhaseCreating
		provision   = simplyblockv1alpha2.StorageClusterPhaseProvisioning
	)
	for _, tc := range []struct {
		id     string
		name   string
		phases []simplyblockv1alpha2.StorageClusterPhase
		want   simplyblockv1alpha2.StorageClusterPhase
	}{
		{"U-01", "every member online", []simplyblockv1alpha2.StorageClusterPhase{online, online, online}, online},
		{"U-02", "one member degraded", []simplyblockv1alpha2.StorageClusterPhase{online, degraded}, degraded},
		{"U-03", "one member rebalancing", []simplyblockv1alpha2.StorageClusterPhase{online, rebalancing}, degraded},
		{"U-04", "one member unavailable while another serves",
			[]simplyblockv1alpha2.StorageClusterPhase{online, unavailable}, degraded},
		{"U-05", "none serving, one suspended, none unavailable",
			[]simplyblockv1alpha2.StorageClusterPhase{suspended, provision}, suspended},
		{"U-06", "none serving, one unavailable",
			[]simplyblockv1alpha2.StorageClusterPhase{suspended, unavailable}, unavailable},
		{"U-07", "one member still pending", []simplyblockv1alpha2.StorageClusterPhase{online, pending}, provision},
		{"U-07", "one member still creating", []simplyblockv1alpha2.StorageClusterPhase{online, creating}, provision},
		{"U-07", "one member never reconciled", []simplyblockv1alpha2.StorageClusterPhase{online, ""}, provision},
		{"U-142", "no member yet", nil, provision},
	} {
		t.Run(tc.id+" "+tc.name, func(t *testing.T) {
			members := make([]memberReading, 0, len(tc.phases))
			for i, phase := range tc.phases {
				members = append(members, rollupMember(string(rune('a'+i)), phase))
			}
			if got := rollUp(members).Phase; got != tc.want {
				t.Errorf("phase = %q, want %q", got, tc.want)
			}
		})
	}
}

// U-08: the aggregate tolerates no more failures than its weakest member, and
// a member that reports none leaves the aggregate's tolerance unknown rather
// than read as the others'.
func TestTheSuperclusterFaultToleranceIsItsWeakestMembers(t *testing.T) {
	online := simplyblockv1alpha2.StorageClusterPhaseOnline

	got := rollUp([]memberReading{
		rollupMember("a", online, withFTT(2)),
		rollupMember("b", online, withFTT(1)),
		rollupMember("c", online, withFTT(2)),
	}).MaxFaultTolerance
	if got == nil || *got != 1 {
		t.Errorf("maxFaultTolerance = %v, want 1", got)
	}

	got = rollUp([]memberReading{
		rollupMember("a", online, withFTT(2)),
		rollupMember("b", online),
	}).MaxFaultTolerance
	if got != nil {
		t.Errorf("maxFaultTolerance = %d with a member reporting none, want unset", *got)
	}
}

// U-09: a member with no reading contributes nothing, rather than zeroing
// the sum or making the whole reading disappear.
func TestTheSuperclusterCapacitySumsTheMembersReadings(t *testing.T) {
	online := simplyblockv1alpha2.StorageClusterPhaseOnline
	a, b, c := rollupMember("a", online), rollupMember("b", online), rollupMember("c", online)
	a.Capacity = reading(1000, 100, 500)
	b.Capacity = reading(3000, 300, 1500)

	got := rollUp([]memberReading{a, b, c}).Capacity
	if diff := cmp.Diff(reading(4000, 400, 2000), got); diff != "" {
		t.Errorf("capacity (-want +got):\n%s", diff)
	}

	if got := rollUp([]memberReading{c}).Capacity; got != nil {
		t.Errorf("capacity = %+v with no member reading, want unset", got)
	}
}

// Placement may choose a member that is serving and has not been taken out of
// placement. The member's row keeps the member's own phase, so a rebalancing
// member reads as Rebalancing there while the supercluster reads Degraded.
func TestTheMemberRowsSayWhichMembersPlacementMayChoose(t *testing.T) {
	members := []memberReading{
		rollupMember("c-cordoned", simplyblockv1alpha2.StorageClusterPhaseOnline,
			func(c *simplyblockv1alpha2.StorageCluster) { c.Spec.DisablePlacement = ptr.To(true) }),
		rollupMember("a-rebalancing", simplyblockv1alpha2.StorageClusterPhaseRebalancing,
			func(c *simplyblockv1alpha2.StorageCluster) { c.Status.UUID = "uuid-a" }),
		rollupMember("b-down", simplyblockv1alpha2.StorageClusterPhaseUnavailable),
	}
	members[1].Capacity = reading(10, 1, 5)

	got := rollUp(members)

	want := []simplyblockv1alpha2.SuperclusterMemberStatus{
		{Name: "a-rebalancing", UUID: "uuid-a", Phase: simplyblockv1alpha2.StorageClusterPhaseRebalancing,
			PlacementEligible: true, Capacity: reading(10, 1, 5)},
		{Name: "b-down", Phase: simplyblockv1alpha2.StorageClusterPhaseUnavailable},
		{Name: "c-cordoned", Phase: simplyblockv1alpha2.StorageClusterPhaseOnline},
	}
	if diff := cmp.Diff(want, got.Members); diff != "" {
		t.Errorf("members (-want +got):\n%s", diff)
	}
	if got.MemberCount != 3 {
		t.Errorf("memberCount = %d, want 3", got.MemberCount)
	}
}

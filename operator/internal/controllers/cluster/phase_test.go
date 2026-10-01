// What the operator calls a cluster it is still building.
//
// The phases after the creation path are a reading of the status the control
// plane publishes, and everything the operator did not recognize was read as
// Unavailable, which the field documents as not serving and not because anybody
// asked. Three of those
// statuses are the opposite of that. A cluster being expanded and a cluster
// being activated are not serving precisely because somebody asked, and one
// that has never been activated is not serving because it is not finished.
//
// So a deployment reported a fault for its whole length, and an activation —
// which is a step of every deployment and of every recovery from a suspension —
// reported one for as long as it ran.

package cluster

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestThePhaseReadsTheControlPlanesLifecycle(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   simplyblockv1alpha2.StorageClusterPhase
	}{
		{"active", simplyblockv1alpha2.StorageClusterPhaseOnline},
		{"degraded", simplyblockv1alpha2.StorageClusterPhaseDegraded},
		{"read_only", simplyblockv1alpha2.StorageClusterPhaseDegraded},
		{"suspended", simplyblockv1alpha2.StorageClusterPhaseSuspended},
		{"", simplyblockv1alpha2.StorageClusterPhasePending},

		// The cluster exists and is being built up: not serving, and nothing
		// wrong with it.
		{"unready", simplyblockv1alpha2.StorageClusterPhaseProvisioning},
		{"in_creation", simplyblockv1alpha2.StorageClusterPhaseProvisioning},
		{"in_expansion", simplyblockv1alpha2.StorageClusterPhaseProvisioning},

		// Activation is its own phase because it is not only a step of a
		// deployment: an expansion and a recovery from a suspension both end in
		// one, long after anything was being provisioned.
		{"in_activation", simplyblockv1alpha2.StorageClusterPhaseActivating},

		// Unavailable keeps its meaning by being what is left: a status this
		// operator has no reading for.
		{"something_new", simplyblockv1alpha2.StorageClusterPhaseUnavailable},
	} {
		t.Run(tc.status, func(t *testing.T) {
			if got := phaseFor(tc.status, phaseFlags{}); got != tc.want {
				t.Errorf("phaseFor(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// A rebalance is read over the two serving statuses and over nothing else.
func TestARebalanceIsReadOverTheServingStatusesOnly(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   simplyblockv1alpha2.StorageClusterPhase
	}{
		{"active", simplyblockv1alpha2.StorageClusterPhaseRebalancing},
		{"degraded", simplyblockv1alpha2.StorageClusterPhaseRebalancing},
		{"read_only", simplyblockv1alpha2.StorageClusterPhaseRebalancing},
		{"suspended", simplyblockv1alpha2.StorageClusterPhaseSuspended},
		{"in_activation", simplyblockv1alpha2.StorageClusterPhaseActivating},
		{"unready", simplyblockv1alpha2.StorageClusterPhaseProvisioning},
		{"", simplyblockv1alpha2.StorageClusterPhasePending},
		{"something_new", simplyblockv1alpha2.StorageClusterPhaseUnavailable},
	} {
		t.Run(tc.status, func(t *testing.T) {
			if got := phaseFor(tc.status, phaseFlags{Rebalancing: true}); got != tc.want {
				t.Errorf("phaseFor(%q, rebalancing) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// A node removal is read over the serving statuses the way a rebalance is, and
// ahead of it: the removal's own volume moves set the rebalancing flag for its
// whole length. A degraded cluster reads Shrinking only where the control plane
// says the removal alone is the reason, so a cluster missing redundancy for any
// other reason during a removal still reads Degraded.
func TestARemovalIsReadAsShrinkingOverTheServingStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		flags  phaseFlags
		want   simplyblockv1alpha2.StorageClusterPhase
	}{
		{"active", "active", phaseFlags{Shrinking: true},
			simplyblockv1alpha2.StorageClusterPhaseShrinking},
		{"active and rebalancing", "active", phaseFlags{Shrinking: true, Rebalancing: true},
			simplyblockv1alpha2.StorageClusterPhaseShrinking},
		{"degraded by the removal", "degraded",
			phaseFlags{Shrinking: true, DegradedByRemoval: true, Rebalancing: true},
			simplyblockv1alpha2.StorageClusterPhaseShrinking},
		{"degraded by something else", "degraded",
			phaseFlags{Shrinking: true, Rebalancing: true},
			simplyblockv1alpha2.StorageClusterPhaseDegraded},
		{"suspended", "suspended", phaseFlags{Shrinking: true, DegradedByRemoval: true},
			simplyblockv1alpha2.StorageClusterPhaseSuspended},
		{"unready", "unready", phaseFlags{Shrinking: true},
			simplyblockv1alpha2.StorageClusterPhaseProvisioning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := phaseFor(tc.status, tc.flags); got != tc.want {
				t.Errorf("phaseFor(%q, %+v) = %q, want %q", tc.status, tc.flags, got, tc.want)
			}
		})
	}
}

// Every phase the mapping can produce has a gauge series, or a dashboard asking
// which phase a cluster is in gets no answer for the phases added last.
func TestEveryPhaseIsPublished(t *testing.T) {
	published := map[simplyblockv1alpha2.StorageClusterPhase]bool{}
	for _, phase := range allPhases {
		published[phase] = true
	}

	for _, phase := range []simplyblockv1alpha2.StorageClusterPhase{
		simplyblockv1alpha2.StorageClusterPhaseProvisioning,
		simplyblockv1alpha2.StorageClusterPhaseActivating,
		simplyblockv1alpha2.StorageClusterPhaseShrinking,
	} {
		if !published[phase] {
			t.Errorf("%s has no gauge series", phase)
		}
	}
}

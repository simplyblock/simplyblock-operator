// The storage network metrics: the attached and host-reachable gauges follow
// status.network, and the failures counter increments by reason (design §11).

package node

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func aBool(v bool) *bool { return &v }

func TestTheAttachedGaugeFollowsThePhase(t *testing.T) {
	status := []simplyblockv1alpha2.NodeNetworkStatus{
		{Network: "fabric", Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached, HostReachable: aBool(true)},
		{Network: "storage", Phase: simplyblockv1alpha2.NodeNetworkPhaseMismatched, HostReachable: aBool(false)},
	}

	recordNetworkMetrics("c-metrics", "n-metrics", status)

	for network, want := range map[string]float64{"fabric": 1, "storage": 0} {
		if got := testutil.ToFloat64(networkAttached.WithLabelValues("c-metrics", "n-metrics", network)); got != want {
			t.Errorf("attached{%s} = %v, want %v", network, got, want)
		}
	}
}

func TestTheHostReachableGaugeIsAbsentUntilThePreflightHasChecked(t *testing.T) {
	networkHostReachable.Reset()
	status := []simplyblockv1alpha2.NodeNetworkStatus{
		{Network: "fabric", Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached},
		{Network: "storage", Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached, HostReachable: aBool(true)},
		{Network: "mgmt", Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached, HostReachable: aBool(false)},
	}

	recordNetworkMetrics("c-reach", "n-reach", status)

	if got := testutil.CollectAndCount(networkHostReachable); got != 2 {
		t.Fatalf("host_reachable series = %d, want 2 (an unchecked network has none)", got)
	}
	for network, want := range map[string]float64{"storage": 1, "mgmt": 0} {
		if got := testutil.ToFloat64(networkHostReachable.WithLabelValues("c-reach", "n-reach", network)); got != want {
			t.Errorf("host_reachable{%s} = %v, want %v", network, got, want)
		}
	}
}

func TestTheFailuresCounterIncrementsByReason(t *testing.T) {
	before := testutil.ToFloat64(networkAttachFailuresTotal.WithLabelValues("c-fail", NetworkAttachmentNotFound))

	recordAttachFailure("c-fail", NetworkAttachmentNotFound)
	recordAttachFailure("c-fail", NetworkAttachmentNotFound)
	recordAttachFailure("c-fail", NetworkAttachmentFailed)

	if got := testutil.ToFloat64(networkAttachFailuresTotal.WithLabelValues("c-fail", NetworkAttachmentNotFound)); got != before+2 {
		t.Errorf("NotFound = %v, want %v", got, before+2)
	}
	if got := testutil.ToFloat64(networkAttachFailuresTotal.WithLabelValues("c-fail", NetworkAttachmentFailed)); got != 1 {
		t.Errorf("Failed = %v, want 1", got)
	}
}

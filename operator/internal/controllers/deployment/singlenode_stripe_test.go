package deployment

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A document that declares a single-node cluster runs 1+0 on one worker:
// parity on one node is refused, it may not name more than one worker, the
// worker hands over a data device, and the same worker without the
// declaration is still too few for anything redundant.
func TestASingleNodeDraftRunsOnePlusZeroOnOneWorker(t *testing.T) {
	stripe := func(d, p int32) *simplyblockv1alpha2.StripeSpec {
		return &simplyblockv1alpha2.StripeSpec{DataChunks: ptr.To(d), ParityChunks: ptr.To(p)}
	}
	nvme := func(n int) []string {
		all := []string{"0000:5e:00.0", "0000:5e:00.1", "0000:5e:00.2", "0000:5e:00.3",
			"0000:5e:00.4", "0000:5e:00.5", "0000:5e:00.6"}
		return all[:n]
	}
	for _, tc := range []struct {
		name       string
		stripe     *simplyblockv1alpha2.StripeSpec
		workers    []string
		singleNode bool
		devices    int  // 0: the group names no devices
		journal    bool // enableJournalDevice
		reason     string
	}{
		{"1+0 single on one worker, one device", stripe(1, 0), []string{"worker-1"}, true, 1, false, ""},
		{"1+0 single, devices unnamed", stripe(1, 0), []string{"worker-1"}, true, 0, false, ""},
		{"1+1 single on one worker, two devices", stripe(1, 1), []string{"worker-1"}, true, 2, false, StripeSingleNodeParity},
		{"2+1 single on one worker, three devices", stripe(2, 1), []string{"worker-1"}, true, 3, false, StripeSingleNodeParity},
		{"4+2 single, devices unnamed", stripe(4, 2), []string{"worker-1"}, true, 0, false, StripeSingleNodeParity},
		{"1+0 single, one device less a journal", stripe(1, 0), []string{"worker-1"}, true, 1, true, StripeBelowMinimumDevices},
		{"1+0 single, two devices less a journal", stripe(1, 0), []string{"worker-1"}, true, 2, true, ""},
		{"1+0 single on two workers", stripe(1, 0), []string{"worker-1", "worker-2"}, true, 1, false, StripeSingleNodeCount},
		{"2+1 undeclared on one worker", stripe(2, 1), []string{"worker-1"}, false, 3, false, StripeBelowMinimumNodes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Spec.Approved = false
				c.Spec.Cluster.Stripe = tc.stripe
				c.Spec.NodeSets[0].Groups[0].Workers = tc.workers
				if tc.devices > 0 {
					c.Spec.NodeSets[0].Groups[0].Devices = &simplyblockv1alpha2.DeviceSelection{NVMe: nvme(tc.devices)}
				} else {
					c.Spec.NodeSets[0].Groups[0].Devices = nil
				}
				if tc.singleNode {
					c.Spec.Cluster.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
				}
				if tc.journal {
					c.Spec.Cluster.EnableJournalDevice = ptr.To(true)
				}
			})
			found := findingsOf(t, config, workers(tc.workers...)...)
			stripeFinding := func(reason string) bool {
				switch reason {
				case StripeBelowMinimumNodes, StripeBelowMinimumWorkers, StripeSingleNodeCount,
					StripeBelowMinimumDevices, StripeSingleNodeParity:
					return true
				}
				return false
			}
			if tc.reason == "" {
				for _, f := range found {
					if stripeFinding(f.reason) {
						t.Fatalf("unexpected finding %+v", f)
					}
				}
				return
			}
			var got []string
			for _, f := range found {
				if stripeFinding(f.reason) {
					got = append(got, f.reason)
				}
			}
			if len(got) != 1 || got[0] != tc.reason {
				t.Fatalf("stripe findings = %v, want [%s] (all: %+v)", got, tc.reason, found)
			}
		})
	}
}

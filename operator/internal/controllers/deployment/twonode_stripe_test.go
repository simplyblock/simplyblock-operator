package deployment

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// A document that declares a two-node cluster may run 1+1 on exactly two
// workers; the same two workers without the declaration are still too few.
func TestATwoNodeDraftRuns1Plus1OnTwoWorkers(t *testing.T) {
	oneOne := &simplyblockv1alpha2.StripeSpec{DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1))}
	for _, tc := range []struct {
		name    string
		workers []string
		twoNode bool
		reason  string // empty: no finding
	}{
		{"two-node on two workers", []string{"worker-1", "worker-2"}, true, ""},
		{"two-node on one worker", []string{"worker-1"}, true, StripeBelowMinimumNodes},
		{"undeclared on two workers", []string{"worker-1", "worker-2"}, false, StripeBelowMinimumNodes},
		{"undeclared on three workers", []string{"worker-1", "worker-2", "worker-3"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Spec.Approved = false
				c.Spec.Cluster.Stripe = oneOne
				c.Spec.NodeSets[0].Groups[0].Workers = tc.workers
				if tc.twoNode {
					c.Spec.Cluster.TwoNode = &simplyblockv1alpha2.TwoNodeSpec{Arbitration: true}
				}
			})
			found := findingsOf(t, config, workers(tc.workers...)...)
			if tc.reason == "" {
				for _, f := range found {
					if f.reason == StripeBelowMinimumNodes || f.reason == StripeBelowMinimumWorkers {
						t.Fatalf("unexpected finding %+v", f)
					}
				}
				return
			}
			only(t, found, tc.reason)
		})
	}
}

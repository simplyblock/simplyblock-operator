// What the draft proposes for enableNodeAffinity, against the stripe it ends
// up with.
//
// A 1+1 stripe is a mirror, so a read can be served from the local node's copy,
// and the draft turns the setting on. Any other scheme gets no proposal. A value
// the installation stated is kept under any scheme, because the field is
// immutable on the cluster.

package discovery

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func stripeOf(data, parity int32) *simplyblockv1alpha2.StripeSpec {
	return &simplyblockv1alpha2.StripeSpec{DataChunks: ptr.To(data), ParityChunks: ptr.To(parity)}
}

func TestNodeAffinityIsOnByDefaultForOnePlusOneOnly(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		workers int
		seed    *simplyblockv1alpha2.ClusterTemplate
		want    *bool
	}{
		{name: "proposed 1+1", workers: 3, want: ptr.To(true)},
		{name: "proposed 1+1 as a two-node cluster", workers: 2, want: ptr.To(true)},
		{name: "proposed 2+1", workers: 4, want: nil},
		{name: "proposed 1+0", workers: 1, want: nil},
		{
			name: "seeded 1+1", workers: 4,
			seed: &simplyblockv1alpha2.ClusterTemplate{Stripe: stripeOf(1, 1)},
			want: ptr.To(true),
		},
		{
			name: "seeded 2+1", workers: 4,
			seed: &simplyblockv1alpha2.ClusterTemplate{Stripe: stripeOf(2, 1)},
			want: nil,
		},
		{
			name: "1+1 with node affinity stated off", workers: 3,
			seed: &simplyblockv1alpha2.ClusterTemplate{EnableNodeAffinity: ptr.To(false)},
			want: ptr.To(false),
		},
		{
			name: "2+1 with node affinity stated on", workers: 4,
			seed: &simplyblockv1alpha2.ClusterTemplate{EnableNodeAffinity: ptr.To(true)},
			want: ptr.To(true),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			template, err := ClusterTemplateFor("a-cluster", aFleetOf(testCase.workers),
				TemplateOptions{Seed: testCase.seed})
			if err != nil {
				t.Fatalf("the fleet was refused: %v", err)
			}
			got := template.Template.EnableNodeAffinity
			switch {
			case testCase.want == nil && got != nil:
				t.Errorf("enableNodeAffinity is %t, want it left unset", *got)
			case testCase.want != nil && got == nil:
				t.Errorf("enableNodeAffinity is unset, want %t", *testCase.want)
			case testCase.want != nil && *got != *testCase.want:
				t.Errorf("enableNodeAffinity is %t, want %t", *got, *testCase.want)
			}
		})
	}
}

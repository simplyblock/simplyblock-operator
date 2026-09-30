// Which runs ask their probes to reclaim, read off the run's own filter.
//
// The decision is one function because it is the whole of the policy: every run
// reclaims, and a block run that said not to does not. Everything else about
// the reclaim is the probe's.

package deployment

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestWhichRunsReclaimTheirUserspaceControllers(t *testing.T) {
	block := func(f *simplyblockv1alpha2.DeviceFilter) *simplyblockv1alpha2.DeviceFilter {
		f.EnableLogicalBlockDevices = ptr.To(true)
		return f
	}

	for _, tc := range []struct {
		name   string
		filter *simplyblockv1alpha2.DeviceFilter
		want   bool
		why    string
	}{{
		name:   "a block run",
		filter: block(&simplyblockv1alpha2.DeviceFilter{}),
		want:   true,
		why: "asking for a block-mode discovery is the statement that this fleet's " +
			"disks belong to the kernel, and a controller SPDK holds presents none",
	}, {
		name:   "a block run that declined",
		filter: block(&simplyblockv1alpha2.DeviceFilter{DisableReclaimUserspaceDevices: ptr.To(true)}),
		want:   false,
		why:    "the run said not to, which is the whole point of the field",
	}, {
		name:   "a block run that declined and changed its mind",
		filter: block(&simplyblockv1alpha2.DeviceFilter{DisableReclaimUserspaceDevices: ptr.To(false)}),
		want:   true,
		why:    "false is not the same as set, and the default is to reclaim",
	}, {
		name:   "an NVMe run",
		filter: &simplyblockv1alpha2.DeviceFilter{},
		want:   true,
		why: "a controller with no kernel driver has no namespace to size, which is " +
			"why such a fleet drafts groups named for a capacity nothing could read",
	}, {
		name:   "an NVMe run stated the long way",
		filter: &simplyblockv1alpha2.DeviceFilter{EnableLogicalBlockDevices: ptr.To(false)},
		want:   true,
		why:    "scanning NVMe is scanning NVMe however it was said",
	}, {
		name:   "a run with no filter at all",
		filter: nil,
		want:   true,
		why:    "a run that says nothing is a run that did not decline",
	}, {
		name: "an NVMe run that tried to decline",
		filter: &simplyblockv1alpha2.DeviceFilter{
			DisableReclaimUserspaceDevices: ptr.To(true),
		},
		want: true,
		why: "declining is the block class's to do, and admission refuses the field " +
			"on an NVMe run, so a value that reached here anyway decides nothing",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reclaimsUserspaceDevices(tc.filter); got != tc.want {
				t.Errorf("reclaims = %v, want %v: %s", got, tc.want, tc.why)
			}
		})
	}
}

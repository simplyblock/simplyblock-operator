// The partitions-per-device count the control plane is asked for.
//
// It is not a free parameter. The backend compares the partitions a device
// already carries against 1 + this number, repartitions when they differ, and
// cannot repartition a device whose table SPDK's gpt module has claimed. So a
// count that disagrees with the one a fleet was built on does not produce a
// different layout — it produces a node that cannot be added at all, on exactly
// the machines that have run this product before.

package node

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The count is what the retired spec.partitions field meant, which is what every
// fleet deployed before that field was replaced carries on its disks.
func TestThePartitionCountMatchesWhatFleetsWereBuiltWith(t *testing.T) {
	for _, tc := range []struct {
		name    string
		journal *bool
		want    int
	}{
		{
			// A journal carved out of every storage device, which is the default
			// and what spec.partitions defaulted to.
			name:    "a journal partition per device",
			journal: nil,
			want:    1,
		},
		{
			name:    "explicitly per device",
			journal: ptr.To(false),
			want:    1,
		},
		{
			// A whole device given to the journal manager, so nothing is carved
			// out of the storage devices.
			name:    "a dedicated journal device",
			journal: ptr.To(true),
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workload := &simplyblockv1alpha2.StorageNodesSpec{EnableJournalDevice: tc.journal}
			if got := partitionsPerDevice(workload); got != tc.want {
				t.Errorf("partitionsPerDevice = %d, want %d", got, tc.want)
			}
		})
	}
}

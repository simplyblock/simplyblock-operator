// Which discovery runs ask their probes to take a controller back.
//
// Every run does, and one kind of run may decline.
//
// A controller a userspace driver holds and nothing is driving is a controller
// some earlier deployment prepared and left, and it is in the way of both
// classes rather than one. A block run cannot see it at all: SPDK takes an NVMe
// controller by rebinding it away from the kernel, and from that moment the
// kernel presents no block device for it, so a fleet prepared for NVMe reports
// no storage to a block run and the reason is a binding rather than an absence.
// An NVMe run does see it — the bus reports the controller whatever drives it —
// but sees nothing else about it: with no kernel block device there is no
// namespace to size, which is why such a fleet drafts groups named for the
// capacity they could not read.
//
// So the reclaim is not a block-mode feature, and the opt-out is not the other
// half of a symmetric choice. It exists for the block run that knows the
// bindings are meant to stay: a fleet mid-migration, or a controller passed
// through to a guest whose holder sits outside the probe's view. An NVMe run
// has no such case, because reclaiming is what lets it read the disks it is
// about to propose, and SPDK binds them back when the cluster is deployed.
//
// What no run does is take a controller something is driving. That is the
// probe's guard rather than this one's, and it is read from the host's process
// table and re-read at the write.

package deployment

import (
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// reclaimsUserspaceDevices reports whether this run's probes hand back the
// NVMe controllers a userspace driver holds and nothing is driving.
//
// A nil filter reclaims, like every other run that did not say otherwise.
func reclaimsUserspaceDevices(filter *simplyblockv1alpha2.DeviceFilter) bool {
	if filter == nil {
		return true
	}
	// Declining is the block class's to do. On an NVMe run the field means
	// nothing and admission refuses it, so reading it here would be reading a
	// value that cannot be set.
	if !ptr.BoolFromOrFalse(filter.EnableLogicalBlockDevices) {
		return true
	}
	return !ptr.BoolFromOrFalse(filter.DisableReclaimUserspaceDevices)
}

// What a run reports about workers whose kernel cannot mount a volume over more
// than one path.
//
// It is a finding and not a refusal. A worker without native NVMe multipath hosts a
// storage node like any other, because the storage engine runs in user space and
// does not use the kernel's NVMe initiator. What it cannot do is mount a volume:
// every path of the volume arrives as a disk of its own and the kernel cannot
// fail over between them. The reviewer is the one who knows whether workloads
// with volumes will be scheduled there.

package discovery

import (
	"fmt"

	"github.com/simplyblock/atlas/inventory"
)

// noteMultipath records a finding for each worker whose kernel is known to lack
// native NVMe multipath, in the order of the plan's workers.
//
// A worker whose state was not read is not named. An unread state is not
// evidence of a problem, and the probe already reports what it could not read.
func (p *Plan) noteMultipath() {
	for _, worker := range p.Workers {
		kernel := worker.Report.HostOS.Kernel
		var message string
		switch inventory.MultipathState(kernel.NVMeMultipath.State) {
		case inventory.MultipathNotBuilt:
			message = fmt.Sprintf(
				"%s boots kernel %s, which is built without CONFIG_NVME_MULTIPATH (%s): it hosts a "+
					"storage node, and it cannot mount a volume over more than one path, so a "+
					"workload with volumes needs a node on another kernel",
				worker.Name, kernel.Release, kernel.NVMeMultipath.Detail)
		case inventory.MultipathDisabled:
			message = fmt.Sprintf(
				"%s boots kernel %s with native NVMe multipath switched off (%s): it hosts a "+
					"storage node, and it mounts a volume over more than one path only after a "+
					"reboot without nvme_core.multipath=N",
				worker.Name, kernel.Release, kernel.NVMeMultipath.Detail)
		default:
			continue
		}
		p.Findings = append(p.Findings, Finding{Reason: NVMeMultipathUnavailable, Message: message})
	}
}

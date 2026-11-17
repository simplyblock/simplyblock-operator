// What a draft tells its reviewer about the software-RAID arrays the deployment
// will stop to take their members.
//
// It is a finding and not a refusal. The probe established that the array is
// not mounted, holds nothing, and carries no content, so stopping it loses
// nothing, and the members are drafted. What the reviewer is owed is the fact
// that approving the draft stops an array and erases its members' RAID
// superblocks, because an approval is the only point at which anybody decides
// that.

package discovery

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/blockdev"
)

// RAIDArrayStopped names a worker whose drafted devices are members of a
// software-RAID array the deployment stops.
const RAIDArrayStopped = "RAIDArrayStopped"

// noteStoppedArrays records a finding for each worker whose draft takes a
// member of an idle array, in the order of the plan's workers. The probe's
// detail names the array, and it is quoted per device rather than parsed: the
// sentence is the probe's, and so is the knowledge of which array it was.
func (p *Plan) noteStoppedArrays() {
	for _, worker := range p.Workers {
		var members []string
		for _, device := range worker.Devices {
			for _, rejection := range device.Rejections {
				if rejection.Reason == string(blockdev.ReasonIdleRAIDMember) {
					members = append(members, fmt.Sprintf("%s (%s)", device.Name, rejection.Detail))
				}
			}
		}
		if len(members) == 0 {
			continue
		}
		p.Findings = append(p.Findings, Finding{Reason: RAIDArrayStopped, Message: fmt.Sprintf(
			"%s: the deployment will stop the array these devices belong to and erase their RAID "+
				"superblocks before a storage node takes them: %s",
			worker.Name, strings.Join(members, "; "))})
	}
}

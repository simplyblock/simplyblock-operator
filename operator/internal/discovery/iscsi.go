// The rule that makes an iSCSI LUN an instruction rather than a default.
//
// Every other bus a run scans is a cable inside the chassis. iSCSI is not: a
// LUN is a disk on the other side of a network, and a storage cluster built on
// one runs every write of its data path over that network, on top of whatever
// the target is doing with the bytes at the far end.
//
// Whether that is wanted is a question about the deployment and not about the
// hardware, and it is exactly the kind of question the approval gate exists for
// — except that a draft proposing a LUN is a draft a reviewer has to notice and
// strike, and a fifty-worker document is not one anybody reads closely enough.
// So the default is the other way around: a LUN reaches a draft only where
// somebody named it, and naming it is the decision.

package discovery

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// ISCSIRule admits an iSCSI LUN only when the run's allow list names it.
type ISCSIRule struct {
	// Class is how a device of this run is named, which is what the allow list
	// is written in.
	Class DeviceClass

	// Allow is the run's allow list for that class. Empty names nothing, so
	// every LUN is refused.
	Allow []string
}

func (ISCSIRule) Name() string { return "iSCSI" }

// Admit refuses an iSCSI device the allow list does not name.
//
// It reads the allow list itself rather than leaving it to the allow-and-deny
// rule, because that rule is only built when a list was given: with no filter
// at all there is nothing to refuse a LUN, and with a list given for another
// reason a LUN would be admitted by being named alongside everything else. What
// this rule states is that a LUN is never taken by default, and that holds
// whether or not a run carries a filter.
func (r ISCSIRule) Admit(_ nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	if device.Transport != string(blockdev.TransportISCSI) {
		return true, ""
	}

	address := r.Class.Address(device)
	for _, allowed := range r.Allow {
		if strings.EqualFold(address, allowed) {
			return true, ""
		}
	}
	return false, fmt.Sprintf(
		"it is an iSCSI LUN, which is storage on the other side of a network, so a run "+
			"takes one only where the allow list names it and this one names %s",
		describeAllowList(r.Allow))
}

// describeAllowList renders what the run was told to take, for the refusal.
func describeAllowList(allow []string) string {
	if len(allow) == 0 {
		return "nothing"
	}
	return strings.Join(allow, ", ")
}

// What a draft does with the members of a software-RAID array nothing uses: it
// takes the array whole or not at all, and tells its reviewer which arrays the
// deployment will stop.
//
// The probe established that the array is not mounted, holds nothing, and
// carries no content, so stopping it loses nothing, and its members are drafted.
// The configure on the worker stops the array only when every member is a
// device the node takes, so a draft holding some members of an array would hold
// devices the array keeps held. And the reviewer is owed the fact that approving
// the draft stops an array, because an approval is the only point at which
// anybody decides that.
//
// The members of one array are recognized by the probe's detail, which names
// the array and is the same sentence for every one of its members. It is
// compared whole rather than parsed: the sentence is the probe's, and so is the
// knowledge of which array it was.

package discovery

import (
	"fmt"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/blockdev"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// RAIDArrayStopped names a software-RAID array the deployment stops on a worker.
const RAIDArrayStopped = "RAIDArrayStopped"

// WholeArrayRule is the rule a member of an idle array is declined under when
// the run drafted only some of that array's members.
const WholeArrayRule = "whole array"

// idleArrayOf returns the probe's detail naming the idle array device belongs
// to, or empty when it is not a member of one.
func idleArrayOf(device nodeprobe.Device) string {
	for _, rejection := range device.Rejections {
		if rejection.Reason == string(blockdev.ReasonIdleRAIDMember) {
			return rejection.Detail
		}
	}
	return ""
}

// keepWholeArrays drops from chosen every member of an idle array not all of
// whose members were chosen, and returns the refusal for each.
//
// A member counts as chosen when its address is: an NVMe run hands over a
// controller with every namespace on it, so a member that shares a chosen
// member's controller is taken whether or not the selection listed it. For the
// same reason a member is dropped with everything sharing its address.
func keepWholeArrays(report nodeprobe.Report, class DeviceClass, chosen []nodeprobe.Device) (
	[]nodeprobe.Device, []Refusal,
) {
	addresses := make(map[string]bool, len(chosen))
	for _, device := range chosen {
		addresses[class.Address(device)] = true
	}

	members := make(map[string][]nodeprobe.Device)
	var arrays []string
	for _, device := range report.Devices {
		if array := idleArrayOf(device); array != "" {
			if _, seen := members[array]; !seen {
				arrays = append(arrays, array)
			}
			members[array] = append(members[array], device)
		}
	}

	dropped := make(map[string]string)
	for _, array := range arrays {
		var taken, left []string
		for _, member := range members[array] {
			if addresses[class.Address(member)] {
				taken = append(taken, member.Name)
			} else {
				left = append(left, member.Name)
			}
		}
		if len(taken) == 0 || len(left) == 0 {
			continue
		}
		why := fmt.Sprintf("the run drafted only some members of an array the deployment would have to stop "+
			"(%s), and leaves out %s; the array is stopped only for a node that takes all of them",
			array, strings.Join(left, ", "))
		for _, member := range members[array] {
			if addresses[class.Address(member)] {
				dropped[class.Address(member)] = why
			}
		}
	}
	if len(dropped) == 0 {
		return chosen, nil
	}

	var refusals []Refusal
	kept := slices.DeleteFunc(slices.Clone(chosen), func(device nodeprobe.Device) bool {
		why, drop := dropped[class.Address(device)]
		if drop {
			refusals = append(refusals, Refusal{
				Worker: report.Node, Device: device.Name, Rule: WholeArrayRule, Reason: why,
			})
		}
		return drop
	})
	return kept, refusals
}

// noteStoppedArrays records one finding for each array a worker's draft takes
// the members of, in the order of the plan's workers and then of the devices.
// One per array rather than one per worker, so that no array's name depends on
// how many members the arrays before it had: an event is bounded, and a
// warning cut short could leave out an array the reviewer is approving the stop
// of.
func (p *Plan) noteStoppedArrays() {
	for _, worker := range p.Workers {
		members := make(map[string][]string)
		var arrays []string
		for _, device := range worker.Devices {
			array := idleArrayOf(device)
			if array == "" {
				continue
			}
			if _, seen := members[array]; !seen {
				arrays = append(arrays, array)
			}
			members[array] = append(members[array], device.Name)
		}
		for _, array := range arrays {
			p.Findings = append(p.Findings, Finding{Reason: RAIDArrayStopped, Message: fmt.Sprintf(
				"%s: the deployment will stop the array %s belong to and erase their RAID superblocks "+
					"before a storage node takes them: %s",
				worker.Name, strings.Join(members[array], ", "), array)})
		}
	}
}

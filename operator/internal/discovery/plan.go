// The pipeline: reports in, a draft out, and a record of everything declined.
//
// Planner is the composition of the five seams and holds no policy of its own.
// What it does own is the order — devices are filtered before workers, because
// whether a worker is worth including is mostly a question about what is left
// of it — and the accounting, because a run that produced a draft without
// saying which disks it declined would leave an administrator comparing the
// draft against the hardware by hand.

package discovery

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// Planner turns a fleet's probe reports into a draft.
//
// Every field is a seam with a default: the zero Planner is the basic pipeline
// this package documents, so a caller replaces the one decision it wants to
// change and leaves the rest.
type Planner struct {
	// Class is which of the two classes of backend storage the run scans.
	// Empty is ClassNVMe.
	Class DeviceClass

	// DeviceRules are applied to every reported device, in order. Nil is the
	// default set, which is what BasicDeviceRules returns.
	DeviceRules []DeviceRule

	// WorkerRules are applied to every worker whose devices survived. Nil is
	// WorkerHasDevices alone.
	WorkerRules []WorkerRule

	// Placement chooses which of a worker's admitted devices to use. Nil is
	// MostAvailableNUMANode.
	Placement Placement

	// Grouper puts the workers into groups. Nil is GroupByHardware.
	Grouper Grouper

	// NodeSetBuilder organizes the groups. Nil is SingleNodeSet.
	NodeSetBuilder NodeSetBuilder
}

// Plan is what a run concluded: the draft to write, and why it looks like that.
type Plan struct {
	// NodeSets are the draft's node sets, ready to go into a spec.
	NodeSets []simplyblockv1alpha2.NodeSet

	// Class is the class of backend storage the draft's devices are named in.
	Class DeviceClass

	// Workers are the machines that made it in, with their placements.
	Workers []Worker

	// Refusals is every device and every worker declined, with the rule and
	// the reason. It is the run's account of itself.
	Refusals []Refusal
}

// Summary is the sentence a run's status carries.
func (p Plan) Summary() string {
	devices := 0
	for _, worker := range p.Workers {
		devices += len(worker.Addresses())
	}

	groups := 0
	for _, set := range p.NodeSets {
		groups += len(set.Groups)
	}

	return fmt.Sprintf(
		"%d workers with %d %s devices, in %d group(s) across %d node set(s); %d refusal(s)",
		len(p.Workers), devices, p.Class, groups, len(p.NodeSets), len(p.Refusals))
}

// RefusalLines renders the refusals for a log or an event, one per line.
func (p Plan) RefusalLines() []string {
	lines := make([]string, 0, len(p.Refusals))
	for _, refusal := range p.Refusals {
		lines = append(lines, refusal.String())
	}
	return lines
}

// BasicDeviceRules is the default device pipeline for a class and a filter.
//
// The order is deliberate and is the order a reader wants the refusal in: what
// the device is, then whether it is free, then whether this run wants it. A
// partition refused as "not in the allow list" would be a true statement and
// the wrong one.
func BasicDeviceRules(class DeviceClass, filter *simplyblockv1alpha2.DeviceFilter) []DeviceRule {
	rules := []DeviceRule{
		WholeDiskRule{},
		ClassRule{Class: class},
	}

	allowPartitioned := filter != nil &&
		filter.EnablePartitionedDevices != nil && *filter.EnablePartitionedDevices
	rules = append(rules, AvailableRule{AllowPartitioned: allowPartitioned})

	if filter == nil {
		return rules
	}

	if class == ClassNVMe {
		if len(filter.PcieAllowList) > 0 || len(filter.PcieDenyList) > 0 {
			rules = append(rules, AllowDenyRule{
				Class: class, Allow: filter.PcieAllowList, Deny: filter.PcieDenyList,
			})
		}
		if filter.PcieModel != "" {
			rules = append(rules, ModelRule{Model: filter.PcieModel})
		}
	} else if len(filter.BlockAllowList) > 0 || len(filter.BlockDenyList) > 0 {
		rules = append(rules, AllowDenyRule{
			Class: class, Allow: filter.BlockAllowList, Deny: filter.BlockDenyList,
		})
	}

	if filter.DriveSizeRange != "" {
		// A range that cannot be parsed is not a range that admits everything.
		// ParseSizeRange is called by the caller that validates the spec, and
		// this one skips what it cannot read rather than silently widening the
		// filter; the run reports the parse failure separately.
		if min, max, err := ParseSizeRange(filter.DriveSizeRange); err == nil {
			rules = append(rules, SizeRule{Min: min, Max: max})
		}
	}
	return rules
}

// Plan runs the pipeline over a fleet's reports.
//
// The reports are taken in whatever order they arrived and the output does not
// depend on it: workers are sorted by name before grouping, so two runs over
// one fleet produce the same draft.
func (p Planner) Plan(reports []nodeprobe.Report, filter *simplyblockv1alpha2.DeviceFilter) Plan {
	class := p.Class
	if class == "" {
		class = ClassNVMe
	}

	deviceRules := p.DeviceRules
	if deviceRules == nil {
		deviceRules = BasicDeviceRules(class, filter)
	}
	workerRules := p.WorkerRules
	if workerRules == nil {
		workerRules = []WorkerRule{WorkerHasDevices{}}
	}
	placement := p.Placement
	if placement == nil {
		placement = MostAvailableNUMANode{}
	}
	grouper := p.Grouper
	if grouper == nil {
		grouper = GroupByHardware{}
	}
	builder := p.NodeSetBuilder
	if builder == nil {
		builder = SingleNodeSet{}
	}

	plan := Plan{Class: class}

	ordered := slices.Clone(reports)
	slices.SortFunc(ordered, func(a, b nodeprobe.Report) int { return cmp.Compare(a.Node, b.Node) })

	for _, report := range ordered {
		admitted, refusals := admitDevices(report, deviceRules)
		plan.Refusals = append(plan.Refusals, refusals...)

		if refused, why := refuseWorker(report, admitted, workerRules); refused != "" {
			plan.Refusals = append(plan.Refusals, Refusal{
				Worker: report.Node, Rule: refused, Reason: why,
			})
			continue
		}

		chosen, why := placement.Choose(report, admitted)
		if len(chosen) == 0 {
			plan.Refusals = append(plan.Refusals, Refusal{
				Worker: report.Node, Rule: placement.Name(), Reason: why,
			})
			continue
		}

		// A device the placement did not take is not a refusal by a rule, and
		// it is still something the draft leaves behind, so it is recorded with
		// the placement as the reason.
		for _, device := range admitted {
			if !slices.ContainsFunc(chosen, func(c nodeprobe.Device) bool { return c.Name == device.Name }) {
				plan.Refusals = append(plan.Refusals, Refusal{
					Worker: report.Node, Device: device.Name,
					Rule: placement.Name(), Reason: why,
				})
			}
		}

		plan.Workers = append(plan.Workers, Worker{
			Name:            report.Node,
			Report:          report,
			Devices:         chosen,
			Class:           class,
			PlacementReason: why,
		})
	}

	if len(plan.Workers) > 0 {
		plan.NodeSets = builder.Build(grouper.Group(plan.Workers))
	}
	return plan
}

// admitDevices applies the device rules to one report, stopping at the first
// rule that declines a device: a device has one reason for not being in the
// draft, and it is the first thing wrong with it.
func admitDevices(report nodeprobe.Report, rules []DeviceRule) ([]nodeprobe.Device, []Refusal) {
	admitted := make([]nodeprobe.Device, 0, len(report.Devices))
	var refusals []Refusal

	for _, device := range report.Devices {
		ok, rule, reason := true, "", ""
		for _, r := range rules {
			if admit, why := r.Admit(report, device); !admit {
				ok, rule, reason = false, r.Name(), why
				break
			}
		}
		if ok {
			admitted = append(admitted, device)
			continue
		}
		refusals = append(refusals, Refusal{
			Worker: report.Node, Device: device.Name, Rule: rule, Reason: reason,
		})
	}
	return admitted, refusals
}

// refuseWorker returns the first rule that declines the worker and its reason,
// or an empty rule name when none does.
func refuseWorker(
	report nodeprobe.Report,
	admitted []nodeprobe.Device,
	rules []WorkerRule,
) (string, string) {
	for _, rule := range rules {
		if ok, why := rule.Admit(report, admitted); !ok {
			return rule.Name(), why
		}
	}
	return "", ""
}

// ParseSizeRange reads a filter's drive size range: 100G-2T, 500G- for no upper
// bound, or -2T for no lower one.
//
// A bare size is both bounds, so 1T means exactly a terabyte rather than at
// least one: a filter naming one size is naming the disks it expects, and
// reading it as a floor would admit the 8 TB disk beside them.
func ParseSizeRange(spec string) (min, max uint64, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, fmt.Errorf("a size range needs a value")
	}

	low, high, isRange := strings.Cut(spec, "-")
	if !isRange {
		size, err := ParseSize(low)
		if err != nil {
			return 0, 0, err
		}
		return size, size, nil
	}

	if strings.TrimSpace(low) != "" {
		if min, err = ParseSize(low); err != nil {
			return 0, 0, err
		}
	}
	if strings.TrimSpace(high) != "" {
		if max, err = ParseSize(high); err != nil {
			return 0, 0, err
		}
	}
	if max > 0 && min > max {
		return 0, 0, fmt.Errorf("the size range %q counts backward", spec)
	}
	return min, max, nil
}

// ParseSize reads one size with an optional binary unit. A bare number is
// gigabytes, matching how the rest of this API reads a size.
func ParseSize(text string) (uint64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("a size needs a value")
	}

	unit := uint64(1) << 30
	digits := text
	for suffix, scale := range map[string]uint64{
		"T": 1 << 40, "TI": 1 << 40, "TB": 1 << 40, "TIB": 1 << 40,
		"G": 1 << 30, "GI": 1 << 30, "GB": 1 << 30, "GIB": 1 << 30,
		"M": 1 << 20, "MI": 1 << 20, "MB": 1 << 20, "MIB": 1 << 20,
	} {
		if rest, cut := strings.CutSuffix(strings.ToUpper(text), suffix); cut && rest != "" {
			// The longest matching suffix wins, so "TIB" is not read as "T"
			// with "IB" left over.
			if len(digits) > len(rest) {
				digits, unit = rest, scale
			}
		}
	}

	var value uint64
	if _, err := fmt.Sscanf(digits, "%d", &value); err != nil {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	if fmt.Sprint(value) != strings.TrimSpace(digits) {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	return value * unit, nil
}

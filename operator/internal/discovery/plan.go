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

	"github.com/simplyblock/atlas/blockdev"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// Planner turns a fleet's probe reports into a draft.
//
// Every field is a seam with a default: the zero Planner is the basic pipeline
// this package documents, so a caller replaces the one decision it wants to
// change and leaves the rest.
type Planner struct {
	// DeviceRules are applied to every reported device, in order. Nil is the
	// default set, which is what BasicDeviceRules returns.
	//
	// The class a run scans is not a field here. It is the filter's to state,
	// through EnableLogicalBlockDevices, and a second statement of it could
	// only ever agree or be wrong: a planner scanning NVMe with a filter
	// carrying block lists read those lists on a branch that never ran and
	// dropped them without a word.
	DeviceRules []DeviceRule

	// WorkerRules are applied to every worker whose devices survived. Nil is
	// WorkerHasDevices alone.
	WorkerRules []WorkerRule

	// Placement chooses which of a worker's admitted devices to use. Nil is
	// MostAvailableNUMANode.
	Placement Placement

	// Grouper puts the workers into groups. Nil is GroupByHardware.
	Grouper Grouper

	// NodeSetBuilder organizes the groups. Nil is SplitByRole, which puts the
	// infrastructure tier in a node set of its own and ahead of the workers.
	NodeSetBuilder NodeSetBuilder

	// KubeNodes is what Kubernetes says about each worker, keyed by name, from
	// KubeNodesOf. It is optional: a plan without it is built from the probe
	// reports alone, and the workers carry a zero KubeNode.
	KubeNodes map[string]KubeNode
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
	devices := p.DeviceCount()

	groups := 0
	for _, set := range p.NodeSets {
		groups += len(set.Groups)
	}

	return fmt.Sprintf(
		"%d workers with %d %s devices, in %d group(s) across %d node set(s); %d refusal(s)",
		len(p.Workers), devices, p.Class, groups, len(p.NodeSets), len(p.Refusals))
}

// DeviceCount is how many devices the draft names across every worker in it,
// which is the number the plan is worth: a run that found ten machines and one
// disk between them has produced nothing to deploy.
func (p Plan) DeviceCount() int {
	devices := 0
	for _, worker := range p.Workers {
		devices += len(worker.Addresses())
	}
	return devices
}

// Explain says why the plan holds nothing, one line per worker.
//
// A worker is refused either as a whole, because its controllers are on a
// userspace driver and the kernel presents no disk, or one device at a time, and
// the two need different answers. The first is on the worker's own refusal. The
// second leaves a worker-level reason that is the arithmetic ("no device
// survived the rules") and puts the reason on the devices, so the device
// refusals are folded in behind it and counted.
//
// They are counted by the rule that made them, which is the fact the refusal
// carries. Counting by the sentence instead meant a rule whose sentence names
// the device never counted at all: forty disks declined by one allow list
// produced forty clauses, each a sentence long, where the reader wanted a rule
// and a number. Where a rule's sentences agree the sentence is still quoted,
// because for most rules it is the substance.
//
// Refusals that were pre-filters are left out of both. A machine presents
// sixteen network block devices and four disks, and a line saying the sixteen
// were not whole disks is true, longer than the rest of the message, and not
// the answer to anything.
// ExplainWithin is what Explain says, written to fit a budget.
//
// Explain writes a clause per worker, which is the detail a reviewer wants and a
// length that grows with the fleet. A Kubernetes event carries 1024 characters
// and is refused rather than truncated past it, so the run that fails on a large
// fleet is exactly the run whose reason never reaches anybody.
//
// The two halves of the answer do not compress the same way. A device refusal
// repeats: every worker of a uniform fleet declines its disks for the same
// reason, and the counts are the finding, since a fleet with no disks and a
// fleet whose disks are all held are different answers. A worker refusal does
// not: it names the controllers it found and where they are, and one worker's
// addresses are not another's. So the devices are counted and the workers are
// listed, and it is the list that gives way when the budget runs out.
func (p Plan) ExplainWithin(budget int) string {
	declined := 0
	workers := map[string]bool{}
	var deviceOrder []string
	byDevice := map[string]int{}
	workerLines := p.Explain()

	for _, refusal := range p.Refusals {
		if refusal.Device == "" || refusal.PreFilter {
			continue
		}
		workers[refusal.Worker] = true
		declined++
		key := refusal.Rule + ": " + refusal.Reason
		if _, counted := byDevice[key]; !counted {
			deviceOrder = append(deviceOrder, key)
		}
		byDevice[key]++
	}

	// A fleet whose every refusal is about a device is the case the per-worker
	// list says nothing extra about, so the counts replace it outright.
	if declined > 0 && len(deviceOrder) > 0 {
		counts := make([]string, 0, len(deviceOrder))
		for _, key := range deviceOrder {
			counts = append(counts, fmt.Sprintf("%d declined by %s", byDevice[key], key))
		}
		return fmt.Sprintf("%d device(s) across %d worker(s), none usable: %s",
			declined, len(workers), strings.Join(counts, "; "))
	}

	// Otherwise, the refusals are about the machines, and what they name cannot
	// be counted away. As many as the budget holds are written, and the rest are
	// reported as a number so that the reader knows the list is not the whole of
	// it.
	kept, used := 0, 0
	for _, line := range workerLines {
		cost := len(line) + len("; ")
		if kept > 0 && used+cost > budget-len(fmt.Sprintf("; and %d more worker(s)", len(workerLines))) {
			break
		}
		used += cost
		kept++
	}
	if kept >= len(workerLines) {
		return strings.Join(workerLines, "; ")
	}
	return fmt.Sprintf("%s; and %d more worker(s)",
		strings.Join(workerLines[:kept], "; "), len(workerLines)-kept)
}

func (p Plan) Explain() []string {
	type byRule struct {
		rule    string
		count   int
		order   []string
		reasons map[string]int
	}
	type perWorker struct {
		worker string
		line   string
		order  []string
		rules  map[string]*byRule
	}

	order := make([]string, 0, len(p.Refusals))
	byWorker := map[string]*perWorker{}
	at := func(worker string) *perWorker {
		if found, ok := byWorker[worker]; ok {
			return found
		}
		fresh := &perWorker{worker: worker, rules: map[string]*byRule{}}
		byWorker[worker] = fresh
		order = append(order, worker)
		return fresh
	}

	for _, refusal := range p.Refusals {
		if refusal.Device == "" {
			at(refusal.Worker).line = refusal.Reason
			continue
		}
		if refusal.PreFilter {
			continue
		}

		entry := at(refusal.Worker)
		group, seen := entry.rules[refusal.Rule]
		if !seen {
			group = &byRule{rule: refusal.Rule, reasons: map[string]int{}}
			entry.rules[refusal.Rule] = group
			entry.order = append(entry.order, refusal.Rule)
		}
		group.count++
		if _, counted := group.reasons[refusal.Reason]; !counted {
			group.order = append(group.order, refusal.Reason)
		}
		group.reasons[refusal.Reason]++
	}

	lines := make([]string, 0, len(order))
	for _, worker := range order {
		entry := byWorker[worker]
		if entry.line == "" && len(entry.order) == 0 {
			// Every refusal on it was a pre-filter and the worker itself was
			// admitted, so there is nothing about it to explain.
			continue
		}

		detail := make([]string, 0, len(entry.order))
		for _, rule := range entry.order {
			group := entry.rules[rule]
			counted := fmt.Sprintf("%d devices declined by %s", group.count, group.rule)
			if group.count == 1 {
				counted = "1 device declined by " + group.rule
			}
			if len(group.order) == 1 {
				detail = append(detail, counted+": "+group.order[0])
				continue
			}
			// The rule refused them for different reasons, and the reasons are
			// the substance: a disk refused for being mounted and one refused
			// for carrying a partition table are two findings.
			parts := make([]string, 0, len(group.order))
			for _, reason := range group.order {
				parts = append(parts, fmt.Sprintf("%d %s", group.reasons[reason], reason))
			}
			detail = append(detail, counted+": "+strings.Join(parts, ", "))
		}

		switch {
		case len(detail) == 0:
			lines = append(lines, entry.worker+": "+entry.line)
		case entry.line == "":
			lines = append(lines, entry.worker+": "+strings.Join(detail, ", "))
		default:
			lines = append(lines, fmt.Sprintf("%s: %s (%s)",
				entry.worker, entry.line, strings.Join(detail, ", ")))
		}
	}
	return lines
}

// RefusalLines renders the refusals for a log or an event, one per line.
func (p Plan) RefusalLines() []string {
	lines := make([]string, 0, len(p.Refusals))
	for _, refusal := range p.Refusals {
		lines = append(lines, refusal.String())
	}
	return lines
}

// claimableControllers is a device for every NVMe controller the machine owns,
// nothing is driving, and the kernel presents no disk for.
//
// A controller on a userspace driver has no block device, so it cannot reach a
// draft through the device reading at all: everything known about it is its PCI
// address. That is not a gap, because a PCI address is precisely how a NodeGroup
// names an NVMe device — the draft that would be written from a block device and
// the draft written from the controller name the same string. Leaving them out
// refused a machine's storage on the grounds that the machine was not currently
// presenting it, which on a fleet that has run this product before is every
// machine.
//
// Only what nothing is using is offered. A held controller is a disk in service,
// and whatever is driving it need not be this product: the same binding is how a
// disk is passed through to a guest.
//
// What is lost with the block device is everything the disk would have said
// about itself — its size, its partition table, whether it looks blank. A
// controller therefore reaches the draft unsized and uninspected, and approving
// it is approving a disk nobody read. That is the trade the draft makes visible
// rather than one it hides: the group it lands in is named for the count and not
// a capacity, so a reviewer sees which machines are being taken on trust.
func claimableControllers(report nodeprobe.Report, class DeviceClass) []nodeprobe.Device {
	if class != ClassNVMe {
		// The other class names devices by path, and a controller with no block
		// device has none to name.
		return nil
	}

	presented := map[string]struct{}{}
	for _, device := range report.Devices {
		if device.PCIAddress != "" {
			presented[device.PCIAddress] = struct{}{}
		}
	}

	var out []nodeprobe.Device
	for _, controller := range report.NVMeControllers {
		if !claimable(controller) {
			continue
		}
		if _, already := presented[controller.Address]; already {
			// The kernel is presenting it after all, so the device reading has
			// it and naming it twice would propose one disk under two entries.
			continue
		}
		out = append(out, nodeprobe.Device{
			Name:       controller.Address,
			PCIAddress: controller.Address,
			Kind:       string(blockdev.KindDisk),
			Transport:  string(blockdev.TransportNVMe),
			NUMANode:   controller.NUMANode,
			Available:  true,
		})
	}
	return out
}

// claimable reports whether a controller the kernel presents no block device for
// is one a run may offer anyway.
//
// Two of the four states qualify, for different reasons.
//
// A controller bound to a userspace driver qualifies only when nothing is
// driving it. Free rather than "not held": a controller the probe could not
// check is not one this may offer, and reading the unchecked state as free is
// how a disk something is driving reaches a draft. The holder need not be this
// product — vfio-pci is also how a disk is passed through to a guest.
//
// A controller bound to nothing at all qualifies outright, and asking who holds
// it would be asking about a character device that does not exist: a driver is
// what exposes one. This is the state a failed run leaves behind, because
// claiming an NVMe controller unbinds it from the kernel first, and an add that
// fails after that leaves it owned by nobody — no namespaces, no block device,
// and nothing in the device reading either. Five workers of a six-worker fleet
// were reported as having no disks at all that way, and the cluster came up on
// the one machine whose add had finished (2026-09-20).
func claimable(controller nodeprobe.Controller) bool {
	if !controller.HasDriver() {
		return true
	}
	return controller.BoundToUserspace() && controller.Free()
}

// BasicDeviceRules is the default device pipeline for a filter.
//
// The order is deliberate and is the order a reader wants the refusal in: what
// the device is, then whether it is free, then whether this run wants it. A
// partition refused as "not in the allow list" would be a true statement and
// the wrong one.
//
// The volume rule sits second, ahead of the class rule, for the same reason
// read the other way. A disk that is one of this fleet's own volumes is refused
// by the class rule too, as a device on a bus this run does not scan, and that
// answer is both true and useless to somebody asking why a machine full of
// disks proposed none. Putting the specific answer first is what puts it in
// front of the reviewer, since the class rule's is a pre-filter and never
// reaches the explanation.
func BasicDeviceRules(filter *simplyblockv1alpha2.DeviceFilter) []DeviceRule {
	class := ClassOf(filter)

	rules := []DeviceRule{
		WholeDiskRule{},
		SimplyblockVolumeRule{},
		ClassRule{Class: class},
	}

	allowPartitioned := filter != nil &&
		filter.EnablePartitionedDevices != nil && *filter.EnablePartitionedDevices
	rules = append(rules, AvailableRule{AllowPartitioned: allowPartitioned})

	if filter == nil {
		// A run with no filter names nothing, so every LUN is refused and every
		// other device is taken on its own terms.
		return append(rules, ISCSIRule{Class: class})
	}

	// An iSCSI LUN is refused unless the run's allow list names it, and that
	// holds for a run carrying no filter too, which is why the rule is added
	// before the filter's own lists and not beside them.
	rules = append(rules, ISCSIRule{Class: class, Allow: allowListFor(filter)})

	// Each class reads its own lists, and the class is the filter's own answer,
	// so the branch cannot be the one the filter was not written for. A PCI
	// filter beside EnableLogicalBlockDevices describes devices the run will
	// never look at, which is why the API refuses that combination rather than
	// leaving it to be ignored here.
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
		// A range that cannot be read builds no rule, which widens the filter
		// to every disk rather than narrowing it to none. That is why a run
		// carrying one never reaches here: the admission webhook refuses it at
		// the request, and the Inspecting step refuses it again for a cluster
		// whose webhook is not installed. Skipping is what is left for a caller
		// that built a Planner directly, and it is the same answer as omitting
		// the field.
		if min, max, err := ParseSizeRange(filter.DriveSizeRange); err == nil {
			rules = append(rules, SizeRule{Min: min, Max: max, Spec: filter.DriveSizeRange})
		}
	}
	return rules
}

// allowListFor is the run's allow list in the vocabulary of the class it scans,
// which is the list an iSCSI LUN has to be named in.
func allowListFor(filter *simplyblockv1alpha2.DeviceFilter) []string {
	if filter == nil {
		return nil
	}
	if ClassOf(filter) == ClassBlock {
		return filter.BlockAllowList
	}
	return filter.PcieAllowList
}

// Plan runs the pipeline over a fleet's reports.
//
// The reports are taken in whatever order they arrived and the output does not
// depend on it: workers are sorted by name before grouping, so two runs over
// one fleet produce the same draft.
func (p Planner) Plan(reports []nodeprobe.Report, filter *simplyblockv1alpha2.DeviceFilter) Plan {
	class := ClassOf(filter)

	deviceRules := p.DeviceRules
	if deviceRules == nil {
		deviceRules = BasicDeviceRules(filter)
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
		builder = SplitByRole{}
	}

	plan := Plan{Class: class}

	ordered := slices.Clone(reports)
	slices.SortFunc(ordered, func(a, b nodeprobe.Report) int { return cmp.Compare(a.Node, b.Node) })

	for _, report := range ordered {
		report.Devices = append(report.Devices, claimableControllers(report, class)...)

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

		kube := p.KubeNodes[report.Node]
		plan.Workers = append(plan.Workers, Worker{
			Name:            report.Node,
			Report:          report,
			Devices:         chosen,
			Class:           class,
			PlacementReason: why,
			Kube:            kube,
			Mgmt:            ManagementOf(report, kube.InternalIP),
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
		ok, rule, reason, pre := true, "", "", false
		for _, r := range rules {
			if admit, why := r.Admit(report, device); !admit {
				ok, rule, reason = false, r.Name(), why
				if marker, says := r.(PreFilter); says {
					pre = marker.PreFilter()
				}
				break
			}
		}
		if ok {
			admitted = append(admitted, device)
			continue
		}
		refusals = append(refusals, Refusal{
			Worker: report.Node, Device: device.Name,
			Rule: rule, Reason: reason, PreFilter: pre,
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

// What went wrong in a discovery, read off a whole bundle at once: the run's
// own failure, the workers it could not inspect or use, the readings its probes
// could not take, and the CDC's errors.
//
// Each of these is visible somewhere on the page already. The diagnosis is the
// list a support agent reads first, so that a bundle answers "why" before the
// agent has to know where to look.

package analysis

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

// Diagnosis is one problem with a discovery.
type Diagnosis struct {
	Severity Severity `json:"severity"`
	// Code names the kind of problem, the same for every bundle.
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Worker is the worker it is about, and Tab the view that explains it.
	Worker string `json:"worker,omitempty"`
	Tab    string `json:"tab,omitempty"`
}

// SeverityInfo is a finding that explains rather than warns.
const SeverityInfo Severity = "info"

// diagnose lists a bundle's problems, errors first.
func diagnose(b *supportbundle.Bundle, a Analysis) []Diagnosis {
	var out []Diagnosis
	add := func(d Diagnosis) { out = append(out, d) }

	if b.Run != nil {
		diagnoseRun(b.Run, add)
	}
	for _, r := range b.Reports {
		if r.Err != "" {
			add(Diagnosis{Severity: SeverityError, Code: "ReportUnreadable", Tab: "overview",
				Title: "A probe report cannot be read", Detail: r.File + ": " + r.Err})
		}
	}
	diagnoseSelection(b, a, add)
	for _, w := range a.Workers {
		diagnoseWorker(w, add)
	}
	diagnoseInterfaces(a, add)

	if a.CDC != nil {
		var errs, warnings []string
		for _, f := range a.CDC.Findings {
			if f.Severity == SeverityError {
				errs = append(errs, f.Message)
			} else {
				warnings = append(warnings, f.Message)
			}
		}
		if len(errs) > 0 {
			add(Diagnosis{Severity: SeverityError, Code: "CDCErrors", Tab: "cdc",
				Title:  fmt.Sprintf("ClusterDeploymentConfig %s has %d error(s)", a.CDC.Name, len(errs)),
				Detail: strings.Join(errs, "\n")})
		}
		if len(warnings) > 0 {
			add(Diagnosis{Severity: SeverityWarning, Code: "CDCWarnings", Tab: "cdc",
				Title:  fmt.Sprintf("ClusterDeploymentConfig %s has %d warning(s)", a.CDC.Name, len(warnings)),
				Detail: strings.Join(warnings, "\n")})
		}
	}

	if a.Collection != "" {
		add(Diagnosis{Severity: SeverityWarning, Code: "CollectionErrors", Tab: "overview",
			Title: "Some of the bundle could not be collected", Detail: a.Collection})
	}
	if len(a.WarningEvents) > 0 {
		add(Diagnosis{Severity: SeverityWarning, Code: "WarningEvents", Tab: "overview",
			Title:  fmt.Sprintf("%d Warning event(s) in the namespace", len(a.WarningEvents)),
			Detail: strings.Join(a.WarningEvents, "\n")})
	}
	if len(b.Problems) > 0 {
		add(Diagnosis{Severity: SeverityWarning, Code: "BundleUnreadable", Tab: "overview",
			Title: "Parts of the bundle cannot be read", Detail: strings.Join(b.Problems, "\n")})
	}

	sort.SliceStable(out, func(i, j int) bool { return severityRank(out[i].Severity) < severityRank(out[j].Severity) })
	return out
}

func severityRank(s Severity) int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	}
	return 2
}

func diagnoseRun(run *simplyblockv1alpha2.OperatorOps, add func(Diagnosis)) {
	switch run.Status.Phase {
	case simplyblockv1alpha2.OperatorOpsPhaseSucceeded:
	case simplyblockv1alpha2.OperatorOpsPhaseFailed:
		add(Diagnosis{Severity: SeverityError, Code: "RunFailed", Tab: "overview",
			Title: "The discovery run failed", Detail: run.Status.Message})
	case simplyblockv1alpha2.OperatorOpsPhaseAborted:
		add(Diagnosis{Severity: SeverityWarning, Code: "RunAborted", Tab: "overview",
			Title: "The discovery run was aborted", Detail: run.Status.Message})
	default:
		add(Diagnosis{Severity: SeverityWarning, Code: "RunNotFinished", Tab: "overview",
			Title:  "The discovery run had not finished when the bundle was collected",
			Detail: fmt.Sprintf("phase %q: %s", run.Status.Phase, run.Status.Message)})
	}
}

// diagnoseSelection names the workers the run selected and has no report for,
// and the nodes it did not select with the reason a node is left out.
func diagnoseSelection(b *supportbundle.Bundle, a Analysis, add func(Diagnosis)) {
	if b.Run == nil || len(b.Run.Status.Workers) == 0 {
		return
	}
	selected := b.Run.Status.Workers
	for _, w := range a.Workers {
		switch {
		case slices.Contains(selected, w.Name) && !w.InReport:
			add(Diagnosis{Severity: SeverityError, Code: "ProbeMissing", Worker: w.Name, Tab: "workers",
				Title:  w.Name + " was selected, and the bundle has no readable report for it",
				Detail: "Its probe failed, or its report is one this tool cannot read. The probe pod's logs say which, within the hour the pod is kept."})
		case !slices.Contains(selected, w.Name) && w.InCluster && len(w.StorageNodes) == 0:
			add(Diagnosis{Severity: SeverityInfo, Code: "NotInspected", Worker: w.Name, Tab: "workers",
				Title: w.Name + " was not inspected", Detail: notInspectedBecause(w)})
		}
	}
}

func notInspectedBecause(w Worker) string {
	var reasons []string
	if w.Node.Cordoned {
		reasons = append(reasons, "it is cordoned")
	}
	for _, taint := range w.Node.Taints {
		if strings.HasSuffix(taint, ":NoSchedule") || strings.HasSuffix(taint, ":NoExecute") {
			reasons = append(reasons, "it carries the taint "+taint)
		}
	}
	for _, role := range w.Node.Roles {
		if role == "ControlPlane" {
			reasons = append(reasons, "it is a control-plane node, which spec.discover.enableControlPlaneNodes includes")
		}
	}
	if len(reasons) == 0 {
		return "Nothing on the node excludes it, so spec.discover.workers or spec.discover.nodeSelector left it out."
	}
	return strings.Join(reasons, "; ")
}

func diagnoseWorker(w Worker, add func(Diagnosis)) {
	if w.Report == nil {
		return
	}
	if w.Summary.Available == 0 {
		add(Diagnosis{Severity: SeverityWarning, Code: "NoUsableDevice", Worker: w.Name, Tab: "workers",
			Title: w.Name + " hands over no device", Detail: refusalSummary(w)})
	}

	var held, unchecked []string
	for _, c := range w.Report.NVMeControllers {
		userspace := c.Driver == "vfio-pci" || c.Driver == "uio_pci_generic"
		switch {
		case c.Held():
			held = append(held, c.Address+" on "+c.Driver)
		case userspace && c.InUse == nil:
			unchecked = append(unchecked, c.Address+" on "+c.Driver)
		}
	}
	if len(held) > 0 {
		add(Diagnosis{Severity: SeverityWarning, Code: "ControllersHeld", Worker: w.Name, Tab: "workers",
			Title: fmt.Sprintf("%d NVMe controller(s) of %s are held by a userspace driver", len(held), w.Name),
			Detail: strings.Join(held, ", ") + ": something drives these disks, such as SPDK, an earlier " +
				"installation, or a virtual machine they are passed through to, so they are not offered."})
	}
	if len(unchecked) > 0 {
		add(Diagnosis{Severity: SeverityWarning, Code: "ControllersUnchecked", Worker: w.Name, Tab: "workers",
			Title:  fmt.Sprintf("Whether %d NVMe controller(s) of %s are in use could not be checked", len(unchecked), w.Name),
			Detail: strings.Join(unchecked, ", ") + ": the probe could not read the host's process table, so they are not offered."})
	}
	if len(w.Report.Unreadable) > 0 {
		add(Diagnosis{Severity: SeverityWarning, Code: "ProbeReadings", Worker: w.Name, Tab: "workers",
			Title:  fmt.Sprintf("The probe on %s could not take %d reading(s)", w.Name, len(w.Report.Unreadable)),
			Detail: strings.Join(w.Report.Unreadable, "\n")})
	}
	if w.KernelReported {
		mp := w.Report.HostOS.Kernel.NVMeMultipath
		if mp.State == "NotBuilt" || mp.State == "Disabled" {
			add(Diagnosis{Severity: SeverityWarning, Code: "MultipathUnavailable", Worker: w.Name, Tab: "workers",
				Title:  w.Name + " cannot mount a volume over several paths",
				Detail: fmt.Sprintf("native NVMe multipath is %s: %s", mp.State, mp.Detail)})
		}
	}
}

// refusalSummary counts why a worker's disks were refused.
func refusalSummary(w Worker) string {
	counts := map[string]int{}
	disks := 0
	for _, d := range w.Devices {
		if d.Kind != "Disk" {
			continue
		}
		disks++
		for _, r := range d.Rejections {
			counts[r.Reason]++
		}
	}
	if disks == 0 {
		return "The report lists no disk at all."
	}
	reasons := make([]string, 0, len(counts))
	for reason, n := range counts {
		reasons = append(reasons, fmt.Sprintf("%s (%d)", reason, n))
	}
	sort.Strings(reasons)
	return fmt.Sprintf("Its %d disk(s) were refused for %s.", disks, strings.Join(reasons, ", "))
}

// diagnoseInterfaces reports workers whose management interfaces have
// different names: one cluster names one interface for all its workers.
func diagnoseInterfaces(a Analysis, add func(Diagnosis)) {
	byName := map[string][]string{}
	for _, w := range a.Workers {
		if w.InReport && w.Suggested.Management != "" {
			byName[w.Suggested.Management] = append(byName[w.Suggested.Management], w.Name)
		}
	}
	if len(byName) < 2 {
		return
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+" on "+strings.Join(byName[name], ", "))
	}
	add(Diagnosis{Severity: SeverityWarning, Code: "InterfaceNamesDiffer", Tab: "workers",
		Title: "The workers' management interfaces have different names",
		Detail: strings.Join(parts, "; ") + ". One cluster names one management interface for all its workers, " +
			"so these workers need separate clusters, or the interfaces renamed."})
}

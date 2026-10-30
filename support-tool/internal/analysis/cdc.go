// The ClusterDeploymentConfig of a bundle, or one the editor built, checked two
// ways.
//
// The operator's own draft checks run against a fake cluster holding what the
// bundle captured: the nodes and the simplyblock objects. They answer what the
// operator would put in the document's status. They do not look at disks or
// interfaces, because the controller has no reports to look at, and the bundle
// does. So the second set of checks reads every group against the probe reports
// of its workers: a device the worker does not have, one the probe refused, and
// an interface it does not hold.

package analysis

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/deployment"
	"github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/admission"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/cdcedit"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

// cdcOf is the bundle's own CDC with its findings.
func cdcOf(b *supportbundle.Bundle) *CDC {
	view := ViewOf(b.CDC)
	view.Phase = string(b.CDC.Status.Phase)
	view.Message = b.CDC.Status.Message
	view.Findings = CheckCDC(b, b.CDC)
	return &view
}

// ViewOf is a CDC's node sets and groups, as the page shows them, and the
// document as YAML to apply.
func ViewOf(cdc *simplyblockv1alpha2.ClusterDeploymentConfig) CDC {
	view := CDC{Name: cdc.Name, Approved: cdc.Spec.Approved}
	if out, err := cdcedit.Render(cdc); err == nil {
		view.YAML = string(out)
	}
	if cdc.Spec.Cluster != nil {
		if out, err := yaml.Marshal(cdc.Spec.Cluster); err == nil {
			view.Cluster = string(out)
		}
	}
	for _, set := range cdc.Spec.NodeSets {
		ns := NodeSet{Name: set.Name}
		for _, g := range set.Groups {
			class, devices := devicesOfGroup(g)
			ns.Groups = append(ns.Groups, Group{
				Name: g.Name, Workers: g.Workers, Class: class, Devices: devices,
				MgmtInterface: g.MgmtInterface, DataInterfaces: g.DataInterfaces, FailureDomain: g.FailureDomain,
				NetworkAttachment: g.NetworkAttachment,
			})
		}
		view.NodeSets = append(view.NodeSets, ns)
	}
	return view
}

func devicesOfGroup(g simplyblockv1alpha2.NodeGroup) (string, []string) {
	switch {
	case g.Devices == nil:
		return "", []string{}
	case len(g.Devices.NVMe) > 0:
		return "nvme", g.Devices.NVMe
	case len(g.Devices.Block) > 0:
		return "block", g.Devices.Block
	}
	return "", []string{}
}

// CheckCDC returns everything wrong with a CDC against the cluster and the
// workers the bundle captured, errors first.
func CheckCDC(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []Finding {
	findings := admissionFindings(cdc)
	findings = append(findings, operatorFindings(b, cdc)...)
	findings = append(findings, bundleFindings(b, cdc)...)
	slices.SortStableFunc(findings, func(x, y Finding) int {
		if x.Severity == y.Severity {
			return 0
		}
		if x.Severity == SeverityError {
			return -1
		}
		return 1
	})
	return findings
}

// admissionFindings is what the API server's admission refuses about the
// document, from the schema and CEL rules of the CRD this build carries. A
// document from an older operator may break a rule its own release did not
// have, which is still what applying it today meets.
func admissionFindings(cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []Finding {
	refused, err := admission.Validate(cdc)
	if err != nil {
		return []Finding{{Source: "admission", Severity: SeverityWarning, Reason: "AdmissionNotChecked",
			Message: fmt.Sprintf("admission's rules could not be checked: %v", err)}}
	}
	out := make([]Finding, 0, len(refused))
	for _, message := range refused {
		out = append(out, Finding{Source: "admission", Severity: SeverityError, Reason: "AdmissionRefused", Message: message})
	}
	return out
}

// operatorFindings runs the operator's draft checks against a fake cluster
// holding the bundle's nodes and simplyblock objects. Every one of them holds a
// draft, so each is an error.
//
// The checks are the operator's code running on what a customer's cluster
// produced, so a panic in them is reported as checks that could not run rather
// than taking the tool down.
func operatorFindings(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) (out []Finding) {
	defer func() {
		if r := recover(); r != nil {
			out = []Finding{{Source: "operator", Severity: SeverityWarning, Reason: "ChecksNotRun",
				Message: fmt.Sprintf("the operator's checks stopped on this bundle: %v", r)}}
		}
	}()
	c, err := fakeClusterOf(b)
	if err == nil {
		var found []deployment.Finding
		found, err = deployment.ValidateDocument(context.Background(), c, cdc.DeepCopy())
		if err == nil {
			out := make([]Finding, 0, len(found))
			for _, f := range found {
				out = append(out, Finding{Source: "operator", Severity: SeverityError, Reason: f.Reason, Message: f.Message})
			}
			return out
		}
	}
	return []Finding{{
		Source: "operator", Severity: SeverityWarning, Reason: "ChecksNotRun",
		Message: fmt.Sprintf("the operator's checks could not run against the bundle: %v", err),
	}}
}

// fakeClusterOf is a fake API server holding what the bundle captured.
func fakeClusterOf(b *supportbundle.Bundle) (client.Client, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, simplyblockv1alpha1.AddToScheme, simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	var objects []client.Object
	for i := range b.Nodes {
		objects = append(objects, b.Nodes[i].DeepCopy())
	}
	for i := range b.ControlPlanes {
		objects = append(objects, b.ControlPlanes[i].DeepCopy())
	}
	for i := range b.StorageClusters {
		objects = append(objects, b.StorageClusters[i].DeepCopy())
	}
	for i := range b.StorageNodes {
		objects = append(objects, b.StorageNodes[i].DeepCopy())
	}
	// An object listed twice is one object to an API server, and the fake one
	// refuses the second copy.
	seen := map[string]bool{}
	unique := objects[:0]
	for _, o := range objects {
		key := fmt.Sprintf("%T/%s/%s", o, o.GetNamespace(), o.GetName())
		if seen[key] {
			continue
		}
		seen[key] = true
		o.SetResourceVersion("")
		unique = append(unique, o)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(unique...).Build(), nil
}

// bundleFindings reads every group against its workers' probe reports.
func bundleFindings(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []Finding {
	reports := map[string]nodeprobe.Report{}
	for _, r := range b.Reports {
		if r.Err == "" && r.Report.Node != "" {
			reports[r.Report.Node] = r.Report
		}
	}
	running := map[string][]simplyblockv1alpha2.StorageNode{}
	for _, sn := range b.StorageNodes {
		running[sn.Spec.WorkerNode] = append(running[sn.Spec.WorkerNode], sn)
	}
	// The clusters the document adds nodes to: the one it grows and, when that
	// is a supercluster, its members. A document creating a cluster adds to none
	// that exist, so every storage node already on a worker is another cluster's.
	target := map[string]bool{}
	if ref := cdc.Spec.ClusterRef; ref != "" {
		target[ref] = true
		for _, c := range b.StorageClusters {
			if c.Spec.SuperclusterRef == ref {
				target[c.Name] = true
			}
		}
	}

	var out []Finding
	add := func(severity Severity, reason, format string, args ...any) {
		out = append(out, Finding{Source: "bundle", Severity: severity, Reason: reason, Message: fmt.Sprintf(format, args...)})
	}
	for _, set := range cdc.Spec.NodeSets {
		for _, g := range set.Groups {
			class, devices := devicesOfGroup(g)
			for _, worker := range g.Workers {
				for _, sn := range running[worker] {
					if !target[sn.Spec.ClusterRef] {
						add(SeverityWarning, "WorkerInUse",
							"%s/%s: %s already runs StorageNode %s of cluster %s, and this document adds a storage node of another cluster to it",
							set.Name, g.Name, worker, sn.Name, sn.Spec.ClusterRef)
					}
				}
				report, ok := reports[worker]
				if !ok {
					add(SeverityWarning, "NotInspected",
						"%s/%s: %s has no probe report in this bundle, so its devices and interfaces cannot be checked",
						set.Name, g.Name, worker)
					continue
				}
				for _, id := range devices {
					checkDevice(add, set.Name, g.Name, worker, class, id, report)
				}
				checkAttachment(add, set.Name, g.Name, worker, g.NetworkAttachment, report)
				checkInterface(add, set.Name, g.Name, worker, "management", g.MgmtInterface, report)
				for _, iface := range g.DataInterfaces {
					checkInterface(add, set.Name, g.Name, worker, "data", iface, report)
				}
			}
		}
	}
	return out
}

func checkDevice(
	add func(Severity, string, string, ...any), set, group, worker, class, id string, report nodeprobe.Report,
) {
	for _, d := range report.Devices {
		match := (class == "nvme" && d.PCIAddress == id && d.Kind == "Disk") ||
			(class == "block" && (d.StablePath == id || d.Path == id))
		if !match {
			continue
		}
		if !d.Available {
			reasons := make([]string, 0, len(d.Rejections))
			for _, r := range d.Rejections {
				reasons = append(reasons, r.Reason)
			}
			add(SeverityWarning, "DeviceRefused",
				"%s/%s: %s on %s was refused by the probe (%s), and enableDriveFormat formats it if it is approved",
				set, group, id, worker, strings.Join(reasons, ", "))
		}
		return
	}
	if class == "nvme" {
		for _, c := range report.NVMeControllers {
			if c.Address != id {
				continue
			}
			if c.Held() {
				add(SeverityError, "DeviceHeld",
					"%s/%s: the NVMe controller %s on %s is bound to %s and in use",
					set, group, id, worker, c.Driver)
			} else {
				add(SeverityWarning, "DeviceWithoutBlockDevice",
					"%s/%s: the NVMe controller %s on %s presents no block device (driver %q)",
					set, group, id, worker, c.Driver)
			}
			return
		}
	}
	add(SeverityError, "DeviceNotOnWorker", "%s/%s: %s has no device %s", set, group, worker, id)
}

// checkAttachment checks a group's network attachment against a worker's report
// by the rule a discovery run applies. A report from before version 11 carries
// no reading of the definitions, so the attachment cannot be checked against it.
func checkAttachment(
	add func(Severity, string, string, ...any), set, group, worker string,
	attachment *simplyblockv1alpha2.NetworkAttachmentSelection, report nodeprobe.Report,
) {
	if attachment == nil {
		return
	}
	if report.Version < 11 {
		add(SeverityWarning, "AttachmentNotInspected",
			"%s/%s: %s's probe report is version %d, which carries no NetworkAttachmentDefinitions, so the attachment cannot be checked",
			set, group, worker, report.Version)
		return
	}
	if ok, why := discovery.AdmitAttachment(report, attachment); !ok {
		add(SeverityError, "AttachmentNotOnWorker", "%s/%s: %s: %s", set, group, worker, why)
	}
}

func checkInterface(
	add func(Severity, string, string, ...any), set, group, worker, role, name string, report nodeprobe.Report,
) {
	if name == "" {
		return
	}
	for _, iface := range report.Interfaces {
		if iface.Name == name {
			return
		}
	}
	add(SeverityError, "InterfaceNotOnWorker", "%s/%s: %s has no %s interface %s", set, group, worker, role, name)
}

// StripeOption is one erasure-coding scheme and whether the document's workers
// and devices carry it.
type StripeOption struct {
	Name         string   `json:"name"`
	DataChunks   int      `json:"dataChunks"`
	ParityChunks int      `json:"parityChunks"`
	Valid        bool     `json:"valid"`
	Reasons      []string `json:"reasons,omitempty"`
}

// StripeOptions is every supported scheme, checked against the document by the
// operator's own stripe checks against the fleet the bundle captured. A
// document that grows a cluster states no stripe, and is offered none.
func StripeOptions(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []StripeOption {
	if cdc.Spec.Cluster == nil {
		return nil
	}
	c, err := fakeClusterOf(b)
	if err != nil {
		return nil
	}
	var out []StripeOption
	for _, scheme := range erasurecoding.Supported() {
		option := StripeOption{
			Name: scheme.String(), DataChunks: scheme.DataChunks, ParityChunks: scheme.ParityChunks, Valid: true,
		}
		doc := cdc.DeepCopy()
		doc.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(scheme.DataChunks)), ParityChunks: ptr.To(int32(scheme.ParityChunks)),
		}
		checks, err := stripeChecks(c, doc)
		if err != nil {
			option.Valid, option.Reasons = false, []string{err.Error()}
		}
		for _, check := range checks {
			option.Valid = false
			option.Reasons = append(option.Reasons, check.Message)
		}
		out = append(out, option)
	}
	return out
}

// stripeChecks runs the operator's stripe checks, reporting a panic in them as
// an error for the reason operatorFindings does.
func stripeChecks(c client.Client, doc *simplyblockv1alpha2.ClusterDeploymentConfig) (checks []deployment.StripeCheck, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the operator's stripe checks stopped on this bundle: %v", r)
		}
	}()
	return deployment.StripeChecks(context.Background(), c, doc.Namespace, doc)
}

// JournalTies reports the workers of a logical block device document with a
// journal device whose smallest chosen devices tie, so that which one carries
// the journal is the control plane's choice.
func JournalTies(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []Finding {
	c := cdc.Spec.Cluster
	if c == nil || c.EnableJournalDevice == nil || !*c.EnableJournalDevice {
		return nil
	}
	reports := map[string]nodeprobe.Report{}
	for _, r := range b.Reports {
		if r.Err == "" {
			reports[r.Report.Node] = r.Report
		}
	}
	var out []Finding
	for _, set := range cdc.Spec.NodeSets {
		for _, g := range set.Groups {
			if g.Devices == nil || len(g.Devices.Block) == 0 {
				continue
			}
			for _, worker := range g.Workers {
				report, ok := reports[worker]
				if !ok {
					continue
				}
				smallest, tied := uint64(0), []string(nil)
				for _, id := range g.Devices.Block {
					for _, d := range report.Devices {
						if d.Path != id && d.StablePath != id {
							continue
						}
						switch {
						case smallest == 0 || d.SizeBytes < smallest:
							smallest, tied = d.SizeBytes, []string{id}
						case d.SizeBytes == smallest:
							tied = append(tied, id)
						}
					}
				}
				if len(tied) > 1 {
					out = append(out, Finding{Source: "bundle", Severity: SeverityWarning, Reason: "JournalDeviceTied",
						Message: fmt.Sprintf("%s/%s: %s's smallest devices tie at %d bytes (%s), so which one carries "+
							"the journal is the control plane's choice: choose force to accept that",
							set.Name, g.Name, worker, smallest, strings.Join(tied, ", "))})
				}
			}
		}
	}
	return out
}

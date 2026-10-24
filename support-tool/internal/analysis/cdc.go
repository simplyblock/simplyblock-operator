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

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/deployment"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
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
			})
		}
		view.NodeSets = append(view.NodeSets, ns)
	}
	return view
}

func devicesOfGroup(g simplyblockv1alpha2.NodeGroup) (string, []string) {
	switch {
	case g.Devices == nil:
		return "", nil
	case len(g.Devices.NVMe) > 0:
		return "nvme", g.Devices.NVMe
	case len(g.Devices.Block) > 0:
		return "block", g.Devices.Block
	}
	return "", nil
}

// CheckCDC returns everything wrong with a CDC against the cluster and the
// workers the bundle captured, errors first.
func CheckCDC(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []Finding {
	findings := operatorFindings(b, cdc)
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

// operatorFindings runs the operator's draft checks against a fake cluster
// holding the bundle's nodes and simplyblock objects. Every one of them holds a
// draft, so each is an error.
func operatorFindings(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []Finding {
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
	for _, o := range objects {
		o.SetResourceVersion("")
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), nil
}

// bundleFindings reads every group against its workers' probe reports.
func bundleFindings(b *supportbundle.Bundle, cdc *simplyblockv1alpha2.ClusterDeploymentConfig) []Finding {
	reports := map[string]nodeprobe.Report{}
	for _, r := range b.Reports {
		if r.Err == "" && r.Report.Node != "" {
			reports[r.Report.Node] = r.Report
		}
	}
	running := map[string][]string{}
	for _, sn := range b.StorageNodes {
		running[sn.Spec.WorkerNode] = append(running[sn.Spec.WorkerNode], sn.Name)
	}

	var out []Finding
	add := func(severity Severity, reason, format string, args ...any) {
		out = append(out, Finding{Source: "bundle", Severity: severity, Reason: reason, Message: fmt.Sprintf(format, args...)})
	}
	for _, set := range cdc.Spec.NodeSets {
		for _, g := range set.Groups {
			class, devices := devicesOfGroup(g)
			for _, worker := range g.Workers {
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
				checkInterface(add, set.Name, g.Name, worker, "management", g.MgmtInterface, report)
				for _, iface := range g.DataInterfaces {
					checkInterface(add, set.Name, g.Name, worker, "data", iface, report)
				}
				if names := running[worker]; len(names) > 0 && cdc.Spec.ClusterRef == "" {
					add(SeverityWarning, "WorkerInUse",
						"%s/%s: %s already runs StorageNode %s, and this document creates another cluster on it",
						set.Name, g.Name, worker, strings.Join(names, ", "))
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

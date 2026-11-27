// The diagnosis of each problem bundle under testdata, and of a healthy one.

package analysis

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

func diagnosis(t *testing.T, a Analysis, code, worker string) Diagnosis {
	t.Helper()
	for _, d := range a.Diagnoses {
		if d.Code == code && (worker == "" || d.Worker == worker) {
			return d
		}
	}
	t.Fatalf("no %s diagnosis for %q among %+v", code, worker, a.Diagnoses)
	return Diagnosis{}
}

func TestAFailedRunIsDiagnosedFirst(t *testing.T) {
	a := analyze(t, "bundle-run-failed", nil)

	if len(a.Diagnoses) == 0 || a.Diagnoses[0].Code != "RunFailed" || a.Diagnoses[0].Severity != SeverityError {
		t.Fatalf("the first diagnosis is not the failed run: %+v", a.Diagnoses)
	}
	if !strings.Contains(a.Diagnoses[0].Detail, "no worker has a device") {
		t.Errorf("the failure does not carry the run's message: %q", a.Diagnoses[0].Detail)
	}
}

func TestAWorkerWithNoUsableDeviceIsDiagnosed(t *testing.T) {
	a := analyze(t, "bundle-run-failed", nil)

	for _, w := range []string{"worker-01", "worker-02", "worker-03"} {
		d := diagnosis(t, a, "NoUsableDevice", w)
		if !strings.Contains(d.Detail, "Mounted") {
			t.Errorf("%s: the diagnosis does not name the reason the disks were refused: %q", w, d.Detail)
		}
	}
}

func TestControllersAUserspaceDriverHoldsAreDiagnosed(t *testing.T) {
	a := analyze(t, "bundle-held-controllers", nil)

	d := diagnosis(t, a, "ControllersHeld", "worker-01")
	if !strings.Contains(d.Detail, "uio_pci_generic") || !strings.Contains(d.Detail, "0000:5e:00.0") {
		t.Errorf("the diagnosis does not name the controllers and their driver: %q", d.Detail)
	}
}

func TestProbeTroubleIsDiagnosed(t *testing.T) {
	a := analyze(t, "bundle-probe-trouble", nil)

	if d := diagnosis(t, a, "ProbeMissing", "worker-03"); d.Severity != SeverityError {
		t.Errorf("a selected worker without a report is %s", d.Severity)
	}
	if d := diagnosis(t, a, "ReportUnreadable", ""); !strings.Contains(d.Detail, "version 8") {
		t.Errorf("the unreadable report does not say why: %q", d.Detail)
	}
	if d := diagnosis(t, a, "NotInspected", "worker-04"); !strings.Contains(d.Detail, "cordoned") {
		t.Errorf("worker-04's diagnosis does not say it is cordoned: %q", d.Detail)
	}
	if d := diagnosis(t, a, "NotInspected", "worker-05"); !strings.Contains(d.Detail, "nvidia.com/gpu") {
		t.Errorf("worker-05's diagnosis does not name its taint: %q", d.Detail)
	}
	if d := diagnosis(t, a, "ProbeReadings", "worker-02"); !strings.Contains(d.Detail, "os-release") {
		t.Errorf("worker-02's unreadable readings are not carried: %q", d.Detail)
	}
	if d := diagnosis(t, a, "MultipathUnavailable", "worker-01"); !strings.Contains(d.Detail, "NotBuilt") {
		t.Errorf("worker-01's multipath state is not carried: %q", d.Detail)
	}
	if d := diagnosis(t, a, "CollectionErrors", ""); !strings.Contains(d.Detail, "Forbidden") {
		t.Errorf("the collection errors are not carried: %q", d.Detail)
	}
	if d := diagnosis(t, a, "WarningEvents", ""); !strings.Contains(d.Detail, "DeviceInspectionFailed") {
		t.Errorf("the Warning events are not carried: %q", d.Detail)
	}
	for i := 1; i < len(a.Diagnoses); i++ {
		if rank(a.Diagnoses[i].Severity) < rank(a.Diagnoses[i-1].Severity) {
			t.Fatalf("the diagnoses are not ordered errors first: %+v", a.Diagnoses)
		}
	}
}

func rank(s Severity) int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	}
	return 2
}

func TestABrokenCDCIsDiagnosed(t *testing.T) {
	a := analyze(t, "bundle-cdc-broken", nil)

	if d := diagnosis(t, a, "CDCErrors", ""); d.Severity != SeverityError || d.Tab != "cdc" {
		t.Errorf("the CDC's errors are %+v", d)
	}
	d := diagnosis(t, a, "InterfaceNamesDiffer", "")
	if !strings.Contains(d.Detail, "eth0") || !strings.Contains(d.Detail, "ens3") {
		t.Errorf("the diagnosis does not name the differing interfaces: %q", d.Detail)
	}
}

func TestAHealthyBundleHasNoErrors(t *testing.T) {
	a := analyze(t, "bundle-v10", nil)
	for _, d := range a.Diagnoses {
		if d.Severity != SeverityInfo {
			t.Errorf("a healthy bundle is diagnosed with %+v", d)
		}
	}
}

func TestStorageClustersShowTheirRole(t *testing.T) {
	a := analyze(t, "bundle-cdc-broken", nil)

	roles := map[string]Object{}
	for _, c := range a.Cluster.StorageClusters {
		roles[c.Name] = c
	}
	if roles["fleet"].Role != "Supercluster" {
		t.Errorf("fleet is %+v", roles["fleet"])
	}
	if roles["fleet-rack-a"].Role != "PlacementGroup" || roles["fleet-rack-a"].Supercluster != "fleet" {
		t.Errorf("fleet-rack-a is %+v", roles["fleet-rack-a"])
	}
	if roles["lab"].Role != "Standalone" {
		t.Errorf("lab, which states no role, is %+v", roles["lab"])
	}
}

func TestASelectedWorkerStaysWhenItsNodeAndReportAreMissing(t *testing.T) {
	// nodes.yaml could not be collected and worker-03 never reported: the run
	// still names it, so the analysis does.
	a := analyze(t, "bundle-probe-trouble", func(name string, data []byte) ([]byte, bool) {
		return data, name != "nodes.yaml"
	})
	w := worker(t, a, "worker-03")
	if w.InReport || w.InCluster {
		t.Errorf("worker-03 is %+v", w)
	}
	diagnosis(t, a, "ProbeMissing", "worker-03")
}

// selectionDiagnoses is what diagnoseSelection says about a run that selected
// master-0 alone, in a cluster of the given nodes.
func selectionDiagnoses(nodes []corev1.Node, enable *bool) map[string]Diagnosis {
	b := &supportbundle.Bundle{
		Run: &simplyblockv1alpha2.OperatorOps{
			Spec: simplyblockv1alpha2.OperatorOpsSpec{
				Discover: &simplyblockv1alpha2.DiscoverSpec{EnableControlPlaneNodes: enable},
			},
			Status: simplyblockv1alpha2.OperatorOpsStatus{Workers: []string{"master-0"}},
		},
		Nodes: nodes,
	}
	a := Analysis{}
	for _, node := range nodes {
		a.Workers = append(a.Workers, Worker{Name: node.Name, InCluster: true, InReport: true, Node: nodeOf(node)})
	}
	out := map[string]Diagnosis{}
	diagnoseSelection(b, a, func(d Diagnosis) { out[d.Worker] = d })
	return out
}

func roleNode(name string, roles ...string) corev1.Node {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	for _, role := range roles {
		node.Labels["node-role.kubernetes.io/"+role] = ""
	}
	return node
}

// Regression: 2026-11-19-compact-cluster-masters-blamed-on-their-role. On a
// compact cluster the control-plane role does not exclude a node when the field
// is unset, so a master the run did not select was left out by the selection,
// and naming its role sends a reader after a setting that changes nothing.
func TestAMasterOfACompactClusterIsNotBlamedOnItsRole(t *testing.T) {
	got := selectionDiagnoses([]corev1.Node{
		roleNode("master-0", "master", "worker"),
		roleNode("master-1", "master", "worker"),
		roleNode("master-2", "master", "worker"),
	}, nil)

	d, found := got["master-1"]
	if !found {
		t.Fatalf("master-1 has no diagnosis among %+v", got)
	}
	if strings.Contains(d.Detail, "control-plane") {
		t.Errorf("master-1 is blamed on its role on a compact cluster: %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "nodeSelector") {
		t.Errorf("master-1's diagnosis does not point at the selection: %q", d.Detail)
	}
}

// Beside a dedicated worker, the role is what keeps a master out.
func TestAMasterBesideADedicatedWorkerIsBlamedOnItsRole(t *testing.T) {
	got := selectionDiagnoses([]corev1.Node{
		roleNode("master-0", "worker"),
		roleNode("master-1", "master", "worker"),
	}, nil)

	if d := got["master-1"]; !strings.Contains(d.Detail, "enableControlPlaneNodes") {
		t.Errorf("master-1's diagnosis does not name the setting that keeps it out: %q", d.Detail)
	}
}

// An explicit false keeps a compact cluster's masters out, and the diagnosis
// says so.
func TestAMasterOfACompactClusterTheRunExcludedIsBlamedOnItsRole(t *testing.T) {
	excluded := false
	got := selectionDiagnoses([]corev1.Node{
		roleNode("master-0", "master", "worker"),
		roleNode("master-1", "master", "worker"),
	}, &excluded)

	if d := got["master-1"]; !strings.Contains(d.Detail, "enableControlPlaneNodes") {
		t.Errorf("master-1's diagnosis does not name the setting that keeps it out: %q", d.Detail)
	}
}

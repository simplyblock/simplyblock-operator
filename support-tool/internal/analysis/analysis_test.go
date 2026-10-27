// Analyzing the fixture bundles: the workers and their facts for both report
// versions, and the CDC with what is wrong with it.

package analysis

import (
	"bytes"
	"strings"
	"testing"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

func analyze(t *testing.T, fixture string, edit func(string, []byte) ([]byte, bool)) Analysis {
	t.Helper()
	b, err := supportbundle.Read(bytes.NewReader(bundletest.Archive(t, bundletest.Fixture(t, fixture), edit)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return Analyze(b)
}

func worker(t *testing.T, a Analysis, name string) Worker {
	t.Helper()
	for _, w := range a.Workers {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no worker %s among %d", name, len(a.Workers))
	return Worker{}
}

func TestEveryWorkerIsShownWithItsFacts(t *testing.T) {
	a := analyze(t, "bundle-v10", nil)

	if a.Run == nil || a.Run.Name != "discover-fleet-02" || a.Run.Environment == "" {
		t.Errorf("the run is %+v", a.Run)
	}
	if len(a.Workers) != 3 {
		t.Fatalf("%d workers, want 3", len(a.Workers))
	}
	w := worker(t, a, "worker-01")
	if !w.InCluster || !w.InReport || w.ReportVersion != 10 || !w.KernelReported {
		t.Errorf("worker-01 is %+v", w)
	}
	if w.Node.InternalIP == "" {
		t.Error("worker-01 has no internal IP")
	}
	if w.Report == nil || w.Report.HostOS.Distro != "ubuntu" {
		t.Errorf("worker-01's report is %+v", w.Report)
	}
	if w.Summary.Available == 0 || w.Summary.Available > w.Summary.Devices || w.Summary.AvailableBytes == 0 {
		t.Errorf("worker-01's summary is %+v", w.Summary)
	}
	if w.Suggested.Management == "" {
		t.Error("no management interface is suggested for worker-01")
	}
	var nvme int
	for _, d := range w.Devices {
		if d.NVMeID != "" {
			nvme++
		}
	}
	if nvme == 0 {
		t.Error("no device of worker-01 carries the PCI address a CDC names it by")
	}
}

func TestAVersion11WorkerShowsItsNetworkAttachments(t *testing.T) {
	a := analyze(t, "bundle-v11", nil)

	w := worker(t, a, "worker-01")
	if !w.InReport || w.ReportVersion != 11 || !w.NetworkAttachmentsReported {
		t.Fatalf("worker-01 is %+v", w)
	}
	if w.Report.NetworkAttachments == nil || len(w.Report.NetworkAttachments.Definitions) != 2 {
		t.Errorf("worker-01's network attachments are %+v", w.Report.NetworkAttachments)
	}
}

func TestAVersion10WorkerShowsNoNetworkAttachmentReading(t *testing.T) {
	w := worker(t, analyze(t, "bundle-v10", nil), "worker-01")
	if w.NetworkAttachmentsReported {
		t.Error("a version-10 report is shown with a network attachment reading")
	}
}

func TestAVersion9WorkerShowsNoKernelFacts(t *testing.T) {
	a := analyze(t, "bundle-v9", nil)

	w := worker(t, a, "worker-02")
	if !w.InReport || w.ReportVersion != 9 {
		t.Fatalf("worker-02 is %+v", w)
	}
	if w.KernelReported {
		t.Error("a version-9 report is shown with kernel facts")
	}
	if w.Summary.Devices == 0 {
		t.Error("the version-9 report's devices are missing")
	}
}

func TestAWorkerWithoutAReportIsStillShown(t *testing.T) {
	a := analyze(t, "bundle-v10", func(name string, data []byte) ([]byte, bool) {
		return data, !strings.HasSuffix(name, "worker-03.json")
	})

	w := worker(t, a, "worker-03")
	if !w.InCluster || w.InReport {
		t.Errorf("worker-03 is %+v, want in the cluster and without a report", w)
	}
}

func TestTheDraftFromTheBundleHasNoErrors(t *testing.T) {
	a := analyze(t, "bundle-v10", nil)

	if a.CDC == nil {
		t.Fatal("the bundle's CDC is not shown")
	}
	if len(a.CDC.NodeSets) == 0 || len(a.CDC.NodeSets[0].Groups) == 0 {
		t.Fatalf("the CDC shows no groups: %+v", a.CDC)
	}
	for _, f := range a.CDC.Findings {
		if f.Severity == SeverityError {
			t.Errorf("the draft the run wrote has an error: %+v", f)
		}
	}
}

func TestADeviceTheWorkerDoesNotHaveIsAnError(t *testing.T) {
	a := analyze(t, "bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if name == "cdc.yaml" {
			return bytes.Replace(data, []byte("nvme:\n"), []byte("nvme:\n        - 0000:99:00.0\n"), 1), true
		}
		return data, true
	})

	if a.CDC == nil {
		t.Fatal("the bundle's CDC is not shown")
	}
	var found *Finding
	for i, f := range a.CDC.Findings {
		if f.Source == "bundle" && strings.Contains(f.Message, "0000:99:00.0") {
			found = &a.CDC.Findings[i]
		}
	}
	if found == nil || found.Severity != SeverityError {
		t.Fatalf("no error names the device the workers do not have: %+v", a.CDC.Findings)
	}
}

func TestTheOperatorsOwnChecksAreShown(t *testing.T) {
	a := analyze(t, "bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if name == "cdc.yaml" {
			return bytes.Replace(data, []byte("- worker-03"), []byte("- worker-09"), 1), true
		}
		return data, true
	})

	if a.CDC == nil {
		t.Fatal("the bundle's CDC is not shown")
	}
	for _, f := range a.CDC.Findings {
		if f.Source == "operator" && f.Reason == "WorkerNotFound" && strings.Contains(f.Message, "worker-09") {
			return
		}
	}
	t.Errorf("the operator's WorkerNotFound is not among %+v", a.CDC.Findings)
}

func TestTheCDCsYAMLIsADocumentToApply(t *testing.T) {
	a := analyze(t, "bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if name == "cdc.yaml" {
			return bytes.Replace(data, []byte("metadata:\n"),
				[]byte("metadata:\n  uid: 139c25fe-db58-4eff-bacf-e1ad8c5b7bdf\n  resourceVersion: \"37855727\"\n"), 1), true
		}
		return data, true
	})
	if a.CDC == nil {
		t.Fatal("the bundle's CDC is not shown")
	}
	for _, field := range []string{"uid:", "resourceVersion:", "status:"} {
		if strings.Contains(a.CDC.YAML, field) {
			t.Errorf("the YAML to download carries %s", field)
		}
	}
}

func TestTheSameObjectTwiceDoesNotStopTheChecks(t *testing.T) {
	a := analyze(t, "bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if name == "nodes.yaml" {
			doc := string(data)
			first := doc[strings.Index(doc, "  - apiVersion: v1"):strings.Index(doc, "  - apiVersion: v1\n    kind: Node\n    metadata:\n      name: worker-02")]
			return []byte(doc + first), true
		}
		return data, true
	})
	if a.CDC == nil {
		t.Fatal("the bundle's CDC is not shown")
	}
	for _, f := range a.CDC.Findings {
		if f.Reason == "ChecksNotRun" {
			t.Errorf("a node listed twice kept the operator's checks from running: %s", f.Message)
		}
	}
}

func inUse(a Analysis, worker string) *Finding {
	for i, f := range a.CDC.Findings {
		if f.Reason == "WorkerInUse" && strings.Contains(f.Message, worker+" ") {
			return &a.CDC.Findings[i]
		}
	}
	return nil
}

// withClusterRef makes the cdc-broken bundle's CDC grow the cluster named.
func withClusterRef(ref string) func(string, []byte) ([]byte, bool) {
	return func(name string, data []byte) ([]byte, bool) {
		if name == "cdc.yaml" && ref != "" {
			return bytes.Replace(data, []byte("spec:\n"), []byte("spec:\n  clusterRef: "+ref+"\n"), 1), true
		}
		return data, true
	}
}

func TestAWorkerInUseIsReportedForTheClusterItBelongsTo(t *testing.T) {
	// worker-01 runs a storage node of fleet-rack-a, a member of fleet.
	for _, c := range []struct {
		ref  string
		warn bool
	}{
		{"", true},              // a new cluster on it
		{"lab", true},           // another cluster's growth
		{"fleet-rack-a", false}, // its own cluster's growth
		{"fleet", false},        // its supercluster's growth
	} {
		a := analyze(t, "bundle-cdc-broken", withClusterRef(c.ref))
		if a.CDC == nil {
			t.Fatal("the bundle's CDC is not shown")
		}
		if got := inUse(a, "worker-01") != nil; got != c.warn {
			t.Errorf("clusterRef %q: WorkerInUse on worker-01 is %v, want %v", c.ref, got, c.warn)
		}
	}
}

func TestAWorkerInUseIsReportedWithoutItsReport(t *testing.T) {
	a := analyze(t, "bundle-cdc-broken", func(name string, data []byte) ([]byte, bool) {
		return data, !strings.HasSuffix(name, "worker-01.json")
	})
	if inUse(a, "worker-01") == nil {
		t.Error("worker-01's storage node went unreported because its probe report is missing")
	}
}

func TestAGroupWithoutDevicesListsNone(t *testing.T) {
	a := analyze(t, "bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if name == "cdc.yaml" {
			doc := string(data)
			start := strings.Index(doc, "      devices:")
			end := strings.Index(doc, "      mgmtInterface:")
			return []byte(doc[:start] + doc[end:]), true
		}
		return data, true
	})
	g := a.CDC.NodeSets[0].Groups[0]
	if g.Devices == nil {
		t.Error("a group without a device selection lists nil devices, which the page cannot read")
	}
}

func TestAdmissionsVerdictIsAmongTheFindings(t *testing.T) {
	a := analyze(t, "bundle-v10", func(name string, data []byte) ([]byte, bool) {
		if name == "cdc.yaml" {
			return bytes.Replace(data, []byte("environment: K3s"), []byte("environment: Nomad"), 1), true
		}
		return data, true
	})
	for _, f := range a.CDC.Findings {
		if f.Source == "admission" && f.Severity == SeverityError && strings.Contains(f.Message, "Nomad") {
			return
		}
	}
	t.Errorf("admission's refusal is not among %+v", a.CDC.Findings)
}

func TestTheFixtureDraftIsAdmitted(t *testing.T) {
	a := analyze(t, "bundle-v10", nil)
	for _, f := range a.CDC.Findings {
		if f.Source == "admission" {
			t.Errorf("the draft the run wrote is refused by admission: %s", f.Message)
		}
	}
}

// selectionOf is a group's attachment, for an assertion.
func selectionOf(g Group) string {
	if g.NetworkAttachment == nil {
		return "none"
	}
	return g.NetworkAttachment.Management + "/" + g.NetworkAttachment.Data
}

func TestAnAttachedDraftShowsItsAttachmentsAndHasNoErrors(t *testing.T) {
	a := analyze(t, "bundle-v11", nil)

	if a.CDC == nil || len(a.CDC.NodeSets) == 0 || len(a.CDC.NodeSets[0].Groups) == 0 {
		t.Fatalf("the CDC shows no groups: %+v", a.CDC)
	}
	if got := selectionOf(a.CDC.NodeSets[0].Groups[0]); got != "storage-mgmt/storage-data" {
		t.Errorf("the group's attachment is %s", got)
	}
	// The operator holds an attached document until it builds storage nodes on
	// attachments, and the tool shows that hold. Nothing else is wrong with it.
	held := false
	for _, f := range a.CDC.Findings {
		if f.Reason == "NetworkAttachmentNotDeployable" {
			held = true
			continue
		}
		if f.Severity == SeverityError {
			t.Errorf("the attached draft has an error: %+v", f)
		}
	}
	if !held {
		t.Errorf("the operator's hold on an attached document is not shown: %+v", a.CDC.Findings)
	}
}

func TestAnAttachmentTheNamespaceDoesNotHoldIsAnError(t *testing.T) {
	a := analyze(t, "bundle-v11", func(name string, data []byte) ([]byte, bool) {
		if name == "cdc.yaml" {
			return bytes.Replace(data, []byte("data: storage-data"), []byte("data: storage-nope"), 1), true
		}
		return data, true
	})

	var found int
	for _, f := range a.CDC.Findings {
		if f.Severity == SeverityError && f.Reason == "AttachmentNotOnWorker" && strings.Contains(f.Message, "storage-nope") {
			found++
		}
	}
	if found != 3 {
		t.Errorf("%d errors about the missing attachment, want one per worker: %+v", found, a.CDC.Findings)
	}
}

// A report from before version 11 carries no reading of the definitions, so
// an attachment cannot be checked against it, which is not the same as one the
// worker cannot take.
func TestAnAttachmentCannotBeCheckedAgainstAnOlderReport(t *testing.T) {
	a := analyze(t, "bundle-v11", func(name string, data []byte) ([]byte, bool) {
		if strings.HasSuffix(name, ".json") {
			return bytes.Replace(data, []byte(`"version": 11`), []byte(`"version": 10`), 1), true
		}
		return data, true
	})

	var warned int
	for _, f := range a.CDC.Findings {
		if f.Severity == SeverityError && f.Reason != "NetworkAttachmentNotDeployable" {
			t.Errorf("an attachment against a version-10 report is an error: %+v", f)
		}
		if f.Reason == "AttachmentNotInspected" {
			warned++
		}
	}
	if warned != 3 {
		t.Errorf("%d warnings that the attachment cannot be checked, want one per worker: %+v", warned, a.CDC.Findings)
	}
}

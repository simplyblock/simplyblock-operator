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

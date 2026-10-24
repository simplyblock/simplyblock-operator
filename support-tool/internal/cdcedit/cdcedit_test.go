// Building documents from the editor's choices against the fixture bundle: the
// prefill from the bundle's own CDC and from its reports, the grouping, and what
// an edit keeps.

package cdcedit

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

func fixture(t *testing.T, edit func(string, []byte) ([]byte, bool)) *supportbundle.Bundle {
	t.Helper()
	b, err := supportbundle.Read(bytes.NewReader(bundletest.Archive(t, bundletest.Fixture(t, "bundle-v10"), edit)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func withoutCDC(name string, data []byte) ([]byte, bool) { return data, name != "cdc.yaml" }

func TestPrefillRebuildsTheBundlesCDC(t *testing.T) {
	b := fixture(t, nil)
	req := Prefill(b)

	if req.Name != b.CDC.Name || req.Namespace != b.CDC.Namespace || req.Mode != ModeNVMe {
		t.Errorf("the request is %+v", req)
	}
	group := b.CDC.Spec.NodeSets[0].Groups[0]
	if len(req.Workers) != len(group.Workers) {
		t.Fatalf("%d workers, want %d", len(req.Workers), len(group.Workers))
	}
	for _, w := range req.Workers {
		if !slices.Equal(w.Devices, group.Devices.NVMe) || w.MgmtInterface != group.MgmtInterface ||
			w.NodeSet != b.CDC.Spec.NodeSets[0].Name {
			t.Errorf("worker %s is %+v", w.Name, w)
		}
	}
	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(built.Spec.NodeSets) != 1 || len(built.Spec.NodeSets[0].Groups) != 1 {
		t.Errorf("the rebuilt document has %+v", built.Spec.NodeSets)
	}
}

func TestPrefillWithoutACDCOffersEveryAvailableDevice(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)

	if len(req.Workers) != 3 || req.Mode != ModeNVMe || req.Name == "" {
		t.Fatalf("the request is %+v", req)
	}
	for _, w := range req.Workers {
		if len(w.Devices) == 0 || w.MgmtInterface == "" {
			t.Errorf("worker %s is offered %+v", w.Name, w)
		}
	}
	if req.Cluster.DataChunks == nil || req.Cluster.EnableDriveFormat == nil || !*req.Cluster.EnableDriveFormat {
		t.Errorf("the cluster is %+v", req.Cluster)
	}
}

func TestWorkersWithTheSameChoicesShareAGroup(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if n := len(built.Spec.NodeSets[0].Groups); n != 1 {
		t.Fatalf("three uniform workers make %d groups", n)
	}

	req.Workers[2].Devices = req.Workers[2].Devices[:1]
	built, err = Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	groups := built.Spec.NodeSets[0].Groups
	if len(groups) != 2 {
		t.Fatalf("a worker with other devices makes %d groups, want 2", len(groups))
	}
	if !slices.Contains(groups[1].Workers, req.Workers[2].Name) && !slices.Contains(groups[0].Workers, req.Workers[2].Name) {
		t.Errorf("%s is in no group", req.Workers[2].Name)
	}
	for _, g := range groups {
		if slices.Contains(g.Workers, req.Workers[2].Name) && len(g.Workers) != 1 {
			t.Errorf("the worker with fewer devices shares a group: %+v", g)
		}
	}
}

func TestAnEditKeepsWhatTheEditorDoesNotShow(t *testing.T) {
	b := fixture(t, nil)
	b.CDC.Labels["team"] = "storage"
	b.CDC.Spec.Cluster.Tolerations = []corev1.Toleration{{Key: "io.simplyblock.node-type", Operator: corev1.TolerationOpExists}}

	req := Prefill(b)
	five := int32(5)
	req.Cluster.VCPUCount = &five
	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Labels["team"] != "storage" || len(built.Spec.Cluster.Tolerations) != 1 {
		t.Errorf("the edit dropped what it did not show: labels %v, tolerations %v",
			built.Labels, built.Spec.Cluster.Tolerations)
	}
	if built.Spec.Cluster.VCPUCount == nil || *built.Spec.Cluster.VCPUCount != 5 {
		t.Errorf("the edit did not take: vcpuCount %v", built.Spec.Cluster.VCPUCount)
	}
}

func TestAnApprovedDocumentIsNotRewritten(t *testing.T) {
	b := fixture(t, nil)
	b.CDC.Spec.Approved = true

	req := Prefill(b)
	if req.Name == b.CDC.Name {
		t.Errorf("the prefill for an approved document reuses its name %q", req.Name)
	}
	same := req
	same.Name = b.CDC.Name
	if _, err := Build(b, same); err == nil {
		t.Error("an approved document was rewritten under its own name")
	}
}

func TestACopyOfAnApprovedDocumentKeepsWhatTheEditorDoesNotShow(t *testing.T) {
	b := fixture(t, nil)
	b.CDC.Spec.Approved = true
	b.CDC.Spec.Cluster.Tolerations = []corev1.Toleration{{Key: "io.simplyblock.node-type", Operator: corev1.TolerationOpExists}}

	built, err := Build(b, Prefill(b))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Name == b.CDC.Name || len(built.Spec.Cluster.Tolerations) != 1 || built.Spec.Approved {
		t.Errorf("the copy is %s, approved %v, tolerations %v",
			built.Name, built.Spec.Approved, built.Spec.Cluster.Tolerations)
	}
}

func TestADocumentIsNeverApproved(t *testing.T) {
	b := fixture(t, withoutCDC)
	built, err := Build(b, Prefill(b))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Spec.Approved {
		t.Error("the editor approved a document")
	}
}

func TestGrowingAClusterStatesNoCluster(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)
	req.ClusterRef = "existing-cluster"

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Spec.ClusterRef != "existing-cluster" || built.Spec.Cluster != nil {
		t.Errorf("clusterRef %q, cluster %+v", built.Spec.ClusterRef, built.Spec.Cluster)
	}
}

func TestAWorkerWithoutDevicesIsRefused(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)
	req.Workers[0].Devices = nil

	if _, err := Build(b, req); err == nil || !strings.Contains(err.Error(), req.Workers[0].Name) {
		t.Errorf("a worker without devices was accepted, or not named: %v", err)
	}
}

func TestRenderIsADocumentToApply(t *testing.T) {
	b := fixture(t, nil)
	built, err := Build(b, Prefill(b))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := Render(built)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the rendering does not parse: %v", err)
	}
	if doc["kind"] != "ClusterDeploymentConfig" || doc["apiVersion"] != "storage.simplyblock.io/v1alpha2" {
		t.Errorf("kind %v, apiVersion %v", doc["kind"], doc["apiVersion"])
	}
	if _, ok := doc["status"]; ok {
		t.Error("the rendering carries a status")
	}
	meta, _ := doc["metadata"].(map[string]any)
	for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields", "generation"} {
		if _, ok := meta[k]; ok {
			t.Errorf("the rendering carries metadata.%s", k)
		}
	}
}

func TestThePrefillTakesTheRunsDeviceMode(t *testing.T) {
	// Every disk of this fleet is NVMe and refused. The run scanned NVMe, so a
	// document for the same fleet names its disks by PCI address.
	b, err := supportbundle.Read(bytes.NewReader(bundletest.Archive(t, bundletest.Fixture(t, "bundle-run-failed"), nil)))
	if err != nil {
		t.Fatal(err)
	}
	if req := Prefill(b); req.Mode != ModeNVMe {
		t.Errorf("the prefill for an NVMe run is in %s mode", req.Mode)
	}
}

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/bundletest"
	"github.com/simplyblock/simplyblock-operator/support-tool/internal/supportbundle"
)

func fixture(t *testing.T, edit func(string, []byte) ([]byte, bool)) *supportbundle.Bundle {
	t.Helper()
	return fixtureOf(t, "bundle-v10", edit)
}

func fixtureOf(t *testing.T, name string, edit func(string, []byte) ([]byte, bool)) *supportbundle.Bundle {
	t.Helper()
	b, err := supportbundle.Read(bytes.NewReader(bundletest.Archive(t, bundletest.Fixture(t, name), edit)))
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

func TestAVanillaDocumentCarriesItsNodeFlags(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)
	req.Environment = "Vanilla"
	on, off := true, false
	req.Cluster.EnableCpuTopology, req.Cluster.EnableKubeletConfiguration = &on, &off

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := built.Spec.Cluster
	if c.EnableCpuTopology == nil || !*c.EnableCpuTopology || c.EnableKubeletConfiguration == nil || *c.EnableKubeletConfiguration {
		t.Errorf("the flags are %v and %v", c.EnableCpuTopology, c.EnableKubeletConfiguration)
	}
}

func TestTheNodeFlagsAreDroppedOutsideVanilla(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)
	req.Environment = "Talos"
	on := true
	req.Cluster.EnableCpuTopology, req.Cluster.EnableKubeletConfiguration = &on, &on

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if c := built.Spec.Cluster; c.EnableCpuTopology != nil || c.EnableKubeletConfiguration != nil {
		t.Errorf("a Talos document carries the flags the schema refuses on it: %v, %v",
			c.EnableCpuTopology, c.EnableKubeletConfiguration)
	}
}

func TestAForcedJournalIsAnEnabledOne(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)
	req.Mode = ModeBlock
	req.Cluster.ForceJournalDevice = true
	req.Cluster.EnableJournalDevice = nil
	for i := range req.Workers {
		req.Workers[i].Devices = []string{"/dev/nvme0n1", "/dev/nvme1n1"}
	}

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if j := built.Spec.Cluster.EnableJournalDevice; j == nil || !*j {
		t.Errorf("a forced journal wrote enableJournalDevice %v", j)
	}
}

func TestTheEnvironmentsAreTheAPIs(t *testing.T) {
	want := []string{"Vanilla", "OpenShift", "Rancher", "K3s", "Talos", "EKS", "GKE", "AKS", "OKE"}
	if len(Environments) != len(want) {
		t.Fatalf("%d environments, want %d", len(Environments), len(want))
	}
	for i, e := range Environments {
		if string(e) != want[i] {
			t.Errorf("environment %d is %s, want %s", i, e, want[i])
		}
	}
}

func TestThePrefillProposesAStripeTheWorkersCarry(t *testing.T) {
	// Three workers carry 1+1 and not 2+1, which needs a fourth to rebuild
	// onto, so a proposal of 2+1 is one the operator refuses.
	req := Prefill(fixture(t, withoutCDC))
	if len(req.Workers) != 3 {
		t.Fatalf("%d workers", len(req.Workers))
	}
	if d, p := req.Cluster.DataChunks, req.Cluster.ParityChunks; d == nil || p == nil || *d != 1 || *p != 1 {
		t.Errorf("three workers are proposed %v+%v, want 1+1", deref(d), deref(p))
	}
}

func deref(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestNodeSetsOfDifferentDeviceClassesKeepTheirClass(t *testing.T) {
	b := fixture(t, nil)
	set := b.CDC.Spec.NodeSets[0]
	nvme := set.Groups[0]
	nvme.Workers = []string{"worker-01", "worker-02"}
	block := *nvme.DeepCopy()
	block.Name = "group-1-block"
	block.Workers = []string{"worker-03"}
	block.Devices = &simplyblockv1alpha2.DeviceSelection{Block: []string{"/dev/nvme0n1", "/dev/nvme1n1"}}
	b.CDC.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
		{Name: "rack-a", Groups: []simplyblockv1alpha2.NodeGroup{nvme}},
		{Name: "rack-b", Groups: []simplyblockv1alpha2.NodeGroup{block}},
	}

	built, err := Build(b, Prefill(b))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, s := range built.Spec.NodeSets {
		d := s.Groups[0].Devices
		switch s.Name {
		case "rack-a":
			if len(d.NVMe) == 0 || len(d.Block) != 0 {
				t.Errorf("the NVMe node set came back as %+v", d)
			}
		case "rack-b":
			if len(d.Block) != 2 || len(d.NVMe) != 0 {
				t.Errorf("the block node set came back as %+v", d)
			}
		}
	}
}

func TestGroupsMergedByAnEditKeepTheirHiddenSettings(t *testing.T) {
	b := fixture(t, nil)
	g := b.CDC.Spec.NodeSets[0].Groups[0]
	first, second := *g.DeepCopy(), *g.DeepCopy()
	first.Name, first.Workers, first.SpdkSystemMemory = "group-a", []string{"worker-01"}, "8G"
	second.Name, second.Workers, second.SpdkSystemMemory = "group-b", []string{"worker-02", "worker-03"}, "8G"
	b.CDC.Spec.NodeSets[0].Groups = []simplyblockv1alpha2.NodeGroup{first, second}

	built, err := Build(b, Prefill(b))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	groups := built.Spec.NodeSets[0].Groups
	if len(groups) != 1 || groups[0].SpdkSystemMemory != "8G" {
		t.Errorf("two groups with the same choices and settings came back as %+v", groups)
	}

	b.CDC.Spec.NodeSets[0].Groups[1].SpdkSystemMemory = "16G"
	built, err = Build(b, Prefill(b))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	memory := map[string]bool{}
	for _, g := range built.Spec.NodeSets[0].Groups {
		memory[g.SpdkSystemMemory] = true
	}
	if !memory["8G"] || !memory["16G"] {
		t.Errorf("groups with different settings were merged: %+v", built.Spec.NodeSets[0].Groups)
	}
}

func TestADeviceNamedTwiceIsListedOnce(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)
	req.Workers[0].Devices = append(req.Workers[0].Devices, req.Workers[0].Devices[0])

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, g := range built.Spec.NodeSets[0].Groups {
		seen := map[string]bool{}
		for _, d := range g.Devices.NVMe {
			if seen[d] {
				t.Errorf("group %s lists %s twice", g.Name, d)
			}
			seen[d] = true
		}
	}
}

func TestOneWorkerWithOneDiskIsProposedOnePlusZero(t *testing.T) {
	b := fixture(t, func(name string, data []byte) ([]byte, bool) {
		return data, name != "cdc.yaml" && !strings.HasSuffix(name, "worker-02.json") && !strings.HasSuffix(name, "worker-03.json")
	})
	report := &b.Reports[0].Report
	report.Devices = report.Devices[:1]

	req := Prefill(b)
	if d, p := req.Cluster.DataChunks, req.Cluster.ParityChunks; deref(d) != int32(1) || deref(p) != int32(0) {
		t.Errorf("one worker with one disk is proposed %v+%v, want 1+0", deref(d), deref(p))
	}
}

func TestThePrefillStatesAVCPUCountAndBuildRequiresOne(t *testing.T) {
	b := fixture(t, withoutCDC)
	req := Prefill(b)
	if req.Cluster.VCPUCount == nil || *req.Cluster.VCPUCount < 4 {
		t.Errorf("the prefill states vcpuCount %v", deref(req.Cluster.VCPUCount))
	}

	req.Cluster.VCPUCount = nil
	if _, err := Build(b, req); err == nil || !strings.Contains(err.Error(), "vCPU") {
		t.Errorf("a cluster without a vCPU count was built: %v", err)
	}
}

func TestGrowingASuperclusterKeepsTheMemberSizing(t *testing.T) {
	b := fixture(t, nil)
	b.StorageClusters = append(b.StorageClusters, simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
	})
	req := Prefill(b)
	req.ClusterRef = "fleet"

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Spec.Cluster == nil {
		t.Error("growing a supercluster dropped the sizing its new members are created with")
	}
}

func TestAnEditKeepsTheGroupsNetworkAttachment(t *testing.T) {
	b := fixtureOf(t, "bundle-v11", nil)
	req := Prefill(b)

	want := simplyblockv1alpha2.NetworkAttachmentSelection{Management: "storage-mgmt", Data: "storage-data"}
	for _, w := range req.Workers {
		if w.NetworkAttachment == nil || *w.NetworkAttachment != want || w.MgmtInterface != "" {
			t.Errorf("worker %s is %+v, want the group's attachment and no interface", w.Name, w)
		}
	}
	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	group := built.Spec.NodeSets[0].Groups[0]
	if group.NetworkAttachment == nil || *group.NetworkAttachment != want || group.MgmtInterface != "" {
		t.Errorf("the rebuilt group is %+v", group)
	}
}

// Without a draft, the editor proposes what the run would: the attachments the
// run named, in place of interfaces.
func TestThePrefillTakesTheRunsNetworkAttachment(t *testing.T) {
	req := Prefill(fixtureOf(t, "bundle-v11", withoutCDC))

	if len(req.Workers) == 0 {
		t.Fatal("no worker is offered")
	}
	for _, w := range req.Workers {
		if w.NetworkAttachment == nil || w.NetworkAttachment.Management != "storage-mgmt" ||
			w.MgmtInterface != "" || len(w.DataInterfaces) > 0 {
			t.Errorf("worker %s is %+v, want the run's attachment and no interfaces", w.Name, w)
		}
	}
}

// Workers attached through different definitions are different groups, as
// workers binding different interfaces are.
func TestWorkersWithDifferentAttachmentsDoNotShareAGroup(t *testing.T) {
	b := fixtureOf(t, "bundle-v11", nil)
	req := Prefill(b)
	req.Workers[0].NetworkAttachment = &simplyblockv1alpha2.NetworkAttachmentSelection{Management: "storage-mgmt"}

	built, err := Build(b, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if groups := len(built.Spec.NodeSets[0].Groups); groups != 2 {
		t.Errorf("%d groups, want the changed worker in a group of its own", groups)
	}
}

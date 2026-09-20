// Cloning one worker's per-node configuration onto another.
//
// The entry is a shell-sourceable env file the storage-node pod's init container
// reads, and a relocation needs the target's to exist before the pod starts
// there: a pod that reaches the node configuration script with no entry fails
// with --max-subsys-count=0, a long way from the cause.
//
// The clone edits the rendered text rather than re-rendering from the node,
// because the node's own spec.config.pcieAllowList has not been rewritten yet —
// that happens after the promote, and this runs before the relocation.
//
// design-storagenode.md §5.3 and §9.

package node

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aRenderedEntry is one worker's entry as the ordinary pass writes it.
const aRenderedEntry = "MAX_SUBSYS_COUNT=10\n" +
	"MAX_HUGE_PAGES_SIZE=''\n" +
	"VCPU_COUNT=8\n" +
	"PCI_ALLOWED='0000:02:00.0,0000:03:00.0'\n" +
	"PCI_BLOCKED=''\n"

// entryFor reads one worker's entry out of the cluster's per-node configuration.
func entryFor(t *testing.T, apiClient client.Client, worker string) (string, bool) {
	t.Helper()
	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: opsNamespace, Name: PerNodeConfigMapName(opsCluster)}
	if err := apiClient.Get(context.Background(), key, &configMap); err != nil {
		t.Fatalf("reading the per-node configuration: %v", err)
	}
	entry, ok := configMap.Data[worker]
	return entry, ok
}

// The target's entry is the source's, with the drives the migration is binding
// merged into the allow list so the host binds them on start and they survive a
// later rebuild.
func TestTheTargetInheritsTheSourcesEntryWithTheNewDrives(t *testing.T) {
	r, apiClient := anOpsWorld(t, aControlPlane(), aPerNodeConfig(aRenderedEntry))

	err := r.Workload.CloneWorkerConfig(context.Background(), opsNamespace, opsCluster,
		opsWorker, opsTarget, []string{"0000:03:00.0", "0000:04:00.0"})
	if err != nil {
		t.Fatalf("cloning the configuration: %v", err)
	}

	entry, cloned := entryFor(t, apiClient, opsTarget)
	if !cloned {
		t.Fatal("the target has no entry, so its init container would find none")
	}
	if !strings.Contains(entry, "PCI_ALLOWED='0000:02:00.0,0000:03:00.0,0000:04:00.0'") {
		t.Errorf("the target's allow list is not the merge of both:\n%s", entry)
	}
	for _, line := range strings.Split(aRenderedEntry, "\n") {
		if line == "" || strings.HasPrefix(line, "PCI_ALLOWED=") {
			continue
		}
		if !strings.Contains(entry, line) {
			t.Errorf("%q did not survive the clone:\n%s", line, entry)
		}
	}

	if source, _ := entryFor(t, apiClient, opsWorker); source != aRenderedEntry {
		t.Errorf("the source's own entry was rewritten by the clone:\n%s", source)
	}
}

// A worker with no entry to clone from is an error rather than an empty entry
// written out: an empty one fails inside the pod, where the cause is hard to
// see.
func TestCloningFromAWorkerWithNoEntryIsRefused(t *testing.T) {
	r, _ := anOpsWorld(t, aControlPlane(), aPerNodeConfig(aRenderedEntry))

	err := r.Workload.CloneWorkerConfig(context.Background(), opsNamespace, opsCluster,
		"worker-9", opsTarget, nil)

	if err == nil {
		t.Error("cloning from a worker that has no configuration was accepted")
	}
}

// A cluster whose configuration has not been written yet has nothing to clone,
// which is not a failure: the ordinary pass writes the map, and the relocation
// holds until the target's pod is ready, which cannot happen before it exists.
func TestNothingToCloneFromIsNotAFailure(t *testing.T) {
	r, _ := anOpsWorld(t, aControlPlane())

	err := r.Workload.CloneWorkerConfig(context.Background(), opsNamespace, opsCluster,
		opsWorker, opsTarget, nil)

	if err != nil {
		t.Errorf("cloning before the configuration exists: %v", err)
	}
}

// An entry that predates the allow-list field gets one appended, which is what a
// node whose list was empty needs; the init container reads the last assignment.
func TestAnEntryWithNoAllowListGetsOne(t *testing.T) {
	merged := mergeAllowedIntoEntry("MAX_SUBSYS_COUNT=10\n", []string{"0000:05:00.0"})

	if !strings.Contains(merged, "MAX_SUBSYS_COUNT=10") {
		t.Errorf("the entry lost what it already said:\n%s", merged)
	}
	if !strings.Contains(merged, "PCI_ALLOWED='0000:05:00.0'") {
		t.Errorf("the drives the migration bound are not in the entry:\n%s", merged)
	}
}

// Nothing to add leaves the entry byte for byte as it was, so a clone of a
// migration binding no drive is the source's own text.
func TestAnEntryIsUntouchedWhenThereIsNothingToAdd(t *testing.T) {
	if merged := mergeAllowedIntoEntry(aRenderedEntry, nil); merged != aRenderedEntry {
		t.Errorf("the entry changed although nothing was added:\n%s", merged)
	}
}

// Reading a list back undoes the quoting the render applied, because the merge
// is a round trip through the text this package wrote.
func TestAListIsReadBackTheWayItWasWritten(t *testing.T) {
	cases := map[string][]string{
		"'0000:02:00.0,0000:03:00.0'": {"0000:02:00.0", "0000:03:00.0"},
		"0000:02:00.0":                {"0000:02:00.0"},
		"'a, b ,c'":                   {"a", "b", "c"},
		"''":                          nil,
		"":                            nil,
	}
	for written, want := range cases {
		got := parseShellList(written)
		if len(got) != len(want) {
			t.Errorf("parseShellList(%q) = %v, want %v", written, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseShellList(%q) = %v, want %v", written, got, want)
				break
			}
		}
	}
}

// renderedFor is the entry the ordinary pass writes for one node's device list.
func renderedFor(t *testing.T, class simplyblockv1alpha2.StorageClusterDeviceClass, names ...string) map[string]string {
	t.Helper()
	cluster := &simplyblockv1alpha2.StorageCluster{
		Spec: simplyblockv1alpha2.StorageClusterSpec{DeviceClass: class},
	}
	node := &simplyblockv1alpha2.StorageNode{
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			Config: simplyblockv1alpha2.StorageNodeConfig{DeviceNames: names},
		},
	}
	out := map[string]string{}
	for _, line := range strings.Split(renderNodeConfig(cluster, node), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			out[key] = strings.Trim(value, "'")
		}
	}
	return out
}

// TestAPCIAddressReachesTheAllowList covers the device list of an NVMe cluster.
//
// Regression: 2026-09-20-pci-addresses-written-to-the-namespace-name-variable —
// every entry of spec.config.deviceNames went to NVME_DEVICES, which the storage
// node's init container passes as --nvme-devices, whose argparse destination is
// nvme_names and which the backend resolves with
// query_nvme_ssd_by_namespace_names against `nvme list` namespace names such as
// nvme0n1. A PCI address never matches one, so the configure selected no device
// and failed with `There are no enough SSD devices on system`. Its return value
// is discarded by node_configure.py, so the init container still exited 0, the
// pod started on whatever configuration the host already had, and the node_add
// that read it was refused for carrying the wrong device class. PCI addresses
// are what --pci-allowed takes.
func TestAPCIAddressReachesTheAllowList(t *testing.T) {
	got := renderedFor(t, simplyblockv1alpha2.StorageClusterDeviceClassNVMe, "0000:01:00.0", "0000:0b:00.0")

	if allowed := got["PCI_ALLOWED"]; allowed != "0000:01:00.0,0000:0b:00.0" {
		t.Errorf("PCI_ALLOWED = %q, and a PCI address is what the allow list takes", allowed)
	}
	if devices := got["NVME_DEVICES"]; devices != "" {
		t.Errorf("NVME_DEVICES = %q, which the backend matches against namespace names", devices)
	}
}

// A bare name is a namespace name, which is exactly what NVME_DEVICES is
// resolved against, so it stays there.
func TestABareNameStaysANamespaceName(t *testing.T) {
	got := renderedFor(t, simplyblockv1alpha2.StorageClusterDeviceClassNVMe, "nvme0n1", "nvme2n1")

	if devices := got["NVME_DEVICES"]; devices != "nvme0n1,nvme2n1" {
		t.Errorf("NVME_DEVICES = %q, and a bare name is a namespace name", devices)
	}
	if allowed := got["PCI_ALLOWED"]; allowed != "" {
		t.Errorf("PCI_ALLOWED = %q, and a namespace name is not a PCI address", allowed)
	}
}

// One list carries both spellings, and each goes where the backend reads it.
func TestBothSpellingsGoWhereTheyAreRead(t *testing.T) {
	got := renderedFor(t, simplyblockv1alpha2.StorageClusterDeviceClassNVMe, "0000:01:00.0", "nvme2n1")

	if allowed := got["PCI_ALLOWED"]; allowed != "0000:01:00.0" {
		t.Errorf("PCI_ALLOWED = %q", allowed)
	}
	if devices := got["NVME_DEVICES"]; devices != "nvme2n1" {
		t.Errorf("NVME_DEVICES = %q", devices)
	}
}

// An explicit allow list and PCI addresses in the device list are one allow
// list, not two, and neither silently drops the other.
func TestTheAllowListAndTheDeviceListAreMerged(t *testing.T) {
	cluster := &simplyblockv1alpha2.StorageCluster{
		Spec: simplyblockv1alpha2.StorageClusterSpec{DeviceClass: simplyblockv1alpha2.StorageClusterDeviceClassNVMe},
	}
	node := &simplyblockv1alpha2.StorageNode{
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			Config: simplyblockv1alpha2.StorageNodeConfig{
				PcieAllowList: []string{"0000:02:00.0"},
				DeviceNames:   []string{"0000:01:00.0"},
			},
		},
	}
	entry := renderNodeConfig(cluster, node)
	if !strings.Contains(entry, "PCI_ALLOWED='0000:01:00.0,0000:02:00.0'") &&
		!strings.Contains(entry, "PCI_ALLOWED='0000:02:00.0,0000:01:00.0'") {
		t.Errorf("the allow list and the device list did not merge:\n%s", entry)
	}
}

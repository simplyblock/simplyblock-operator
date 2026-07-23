package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

const migrateSrcEnvFile = `MAX_LVOL=10
MAX_SIZE=''
CORES_PERCENTAGE=50
RESERVED_SYSTEM_CPUS=''
CPU_TOPOLOGY_ENABLED=true
PCI_ALLOWED='0000:02:00.0,0000:03:00.0'
PCI_BLOCKED=''
NVME_DEVICES=''
DEVICE_MODEL=''
SIZE_RANGE=''
JM_PERCENT=
HA_JM_COUNT=
`

func TestMergePcieList(t *testing.T) {
	got := mergePcieList([]string{"a", "b", ""}, []string{"b", "c", "c"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergePcieList = %v, want %v", got, want)
	}
}

func TestParseShellCSV(t *testing.T) {
	cases := map[string][]string{
		`'0000:02:00.0,0000:03:00.0'`: {"0000:02:00.0", "0000:03:00.0"},
		`''`:                          nil,
		``:                            nil,
		`0000:02:00.0`:                {"0000:02:00.0"},
		`'a, b ,c'`:                   {"a", "b", "c"},
	}
	for in, want := range cases {
		if got := parseShellCSV(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseShellCSV(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMergePcieAllowedIntoEnvFile(t *testing.T) {
	// Empty extra: unchanged.
	if got := mergePcieAllowedIntoEnvFile(migrateSrcEnvFile, nil); got != migrateSrcEnvFile {
		t.Fatalf("empty extra changed the env file:\n%s", got)
	}

	// Merge new address, dedupe an already-present one, leave other lines intact.
	got := mergePcieAllowedIntoEnvFile(migrateSrcEnvFile,
		[]string{"0000:03:00.0", "0000:04:00.0"})
	if !strings.Contains(got, `PCI_ALLOWED='0000:02:00.0,0000:03:00.0,0000:04:00.0'`) {
		t.Fatalf("PCI_ALLOWED not merged as expected:\n%s", got)
	}
	// Only the PCI_ALLOWED line should differ.
	for _, line := range strings.Split(migrateSrcEnvFile, "\n") {
		if strings.HasPrefix(line, "PCI_ALLOWED=") || line == "" {
			continue
		}
		if !strings.Contains(got, line) {
			t.Errorf("line unexpectedly changed/removed: %q", line)
		}
	}

	// No PCI_ALLOWED line present: one is appended.
	appended := mergePcieAllowedIntoEnvFile("MAX_LVOL=10\n", []string{"0000:05:00.0"})
	if !strings.Contains(appended, "MAX_LVOL=10") ||
		!strings.Contains(appended, `PCI_ALLOWED='0000:05:00.0'`) {
		t.Fatalf("PCI_ALLOWED not appended:\n%s", appended)
	}
}

func TestEnsureMigratedWorkerConfig(t *testing.T) {
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-node", Namespace: opsTestNS},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PerNodeConfigMapName(sns.Name),
			Namespace: opsTestNS,
		},
		Data: map[string]string{"worker-1": migrateSrcEnvFile},
	}
	r := newOpsReconciler(t, sns, cm)
	ctx := context.Background()

	if err := r.ensureMigratedWorkerConfig(ctx, sns, "worker-1", "worker-4",
		[]string{"0000:04:00.0"}); err != nil {
		t.Fatalf("ensureMigratedWorkerConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: opsTestNS}, &got); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	entry, ok := got.Data["worker-4"]
	if !ok {
		t.Fatal("worker-4 entry not created")
	}
	if !strings.Contains(entry, `PCI_ALLOWED='0000:02:00.0,0000:03:00.0,0000:04:00.0'`) {
		t.Fatalf("worker-4 PCI_ALLOWED not merged:\n%s", entry)
	}

	// Idempotent: a pre-existing target entry is left untouched.
	got.Data["worker-4"] = "SENTINEL=1\n"
	if err := r.Update(ctx, &got); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	if err := r.ensureMigratedWorkerConfig(ctx, sns, "worker-1", "worker-4", nil); err != nil {
		t.Fatalf("ensureMigratedWorkerConfig (idempotent): %v", err)
	}
	var again corev1.ConfigMap
	_ = r.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: opsTestNS}, &again)
	if again.Data["worker-4"] != "SENTINEL=1\n" {
		t.Fatalf("existing target entry was overwritten: %q", again.Data["worker-4"])
	}

	// Missing source entry is an error.
	if err := r.ensureMigratedWorkerConfig(ctx, sns, "worker-nope", "worker-5", nil); err == nil {
		t.Fatal("expected error for missing source entry")
	}
}

func TestReconcileMigratedTopologyMigratesNodeConfig(t *testing.T) {
	source, target := "worker-1", "worker-4"
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-node", Namespace: opsTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			WorkerNodes:   []string{source, "worker-2"},
			PcieAllowList: []string{"0000:02:00.0"},
			NodeConfigs: map[string]simplyblockv1alpha1.StorageNodeOverrides{
				source: {PcieAllowList: []string{"0000:02:00.0", "0000:03:00.0"}},
			},
		},
	}
	sn := newTestStorageNode("simplyblock-node-x", opsTestNS, sns.Name, source, opsTestNodeUUID)
	r := newOpsReconciler(t, sns, sn)
	ctx := context.Background()

	if err := r.reconcileMigratedTopology(ctx, sn, sns, target, []string{"0000:04:00.0"}); err != nil {
		t.Fatalf("reconcileMigratedTopology: %v", err)
	}

	var fresh simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{Name: sns.Name, Namespace: opsTestNS}, &fresh); err != nil {
		t.Fatalf("get sns: %v", err)
	}

	// Worker list: source dropped, target added.
	if contains(fresh.Spec.WorkerNodes, source) || !contains(fresh.Spec.WorkerNodes, target) {
		t.Fatalf("worker list not swapped: %v", fresh.Spec.WorkerNodes)
	}
	// nodeConfigs: source removed, target holds source's list + newSsdPcie.
	if _, ok := fresh.Spec.NodeConfigs[source]; ok {
		t.Error("source nodeConfig not removed")
	}
	tc, ok := fresh.Spec.NodeConfigs[target]
	if !ok {
		t.Fatal("target nodeConfig not set")
	}
	want := []string{"0000:02:00.0", "0000:03:00.0", "0000:04:00.0"}
	if !reflect.DeepEqual(tc.PcieAllowList, want) {
		t.Fatalf("target PcieAllowList = %v, want %v", tc.PcieAllowList, want)
	}

	// StorageNode re-pointed at the target worker.
	var freshSN simplyblockv1alpha1.StorageNode
	_ = r.Get(ctx, types.NamespacedName{Name: sn.Name, Namespace: opsTestNS}, &freshSN)
	if freshSN.Spec.WorkerNode != target {
		t.Fatalf("StorageNode workerNode = %q, want %q", freshSN.Spec.WorkerNode, target)
	}
}

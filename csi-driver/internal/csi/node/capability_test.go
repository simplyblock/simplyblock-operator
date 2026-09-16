package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

func writeMarker(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return path
}

func TestAdvertiseVDOCapability_WritesTrue(t *testing.T) {
	marker := writeMarker(t, "true")
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.Labels[kube.LabelVDOCapable] != "true" {
		t.Errorf("label = %q, want true", node.Labels[kube.LabelVDOCapable])
	}
	if node.Annotations[kube.AnnoVDOCapableManagedBy] != kube.AnnoVDOCapableManagedByAutoDetect {
		t.Errorf("managed-by annotation = %q, want %q",
			node.Annotations[kube.AnnoVDOCapableManagedBy], kube.AnnoVDOCapableManagedByAutoDetect)
	}
}

func TestAdvertiseVDOCapability_WritesFalse(t *testing.T) {
	marker := writeMarker(t, "false")
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != "false" {
		t.Errorf("label = %q, want false", node.Labels[kube.LabelVDOCapable])
	}
}

// An operator's hand-set label — present without the managed-by annotation —
// is the escape hatch a golden-image node depends on, so the probe leaves it
// alone (design-issue-277 §4.3).
func TestAdvertiseVDOCapability_LeavesAnOperatorSetLabelAlone(t *testing.T) {
	marker := writeMarker(t, "false")
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{kube.LabelVDOCapable: "true"},
	}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != "true" {
		t.Errorf("label = %q, want the operator's true left untouched", node.Labels[kube.LabelVDOCapable])
	}
	if _, ok := node.Annotations[kube.AnnoVDOCapableManagedBy]; ok {
		t.Error("the probe claimed an operator-set label as its own")
	}
}

// A label the probe itself wrote before (carrying the managed-by annotation)
// is exactly the label a later run of the same probe may overwrite — this is
// what lets capability lost (a kernel downgrade) flip the label back to false.
func TestAdvertiseVDOCapability_OverwritesItsOwnPriorLabel(t *testing.T) {
	marker := writeMarker(t, "false")
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "node-a",
		Labels:      map[string]string{kube.LabelVDOCapable: "true"},
		Annotations: map[string]string{kube.AnnoVDOCapableManagedBy: kube.AnnoVDOCapableManagedByAutoDetect},
	}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != "false" {
		t.Errorf("label = %q, want the probe's own label overwritten to false", node.Labels[kube.LabelVDOCapable])
	}
}

func TestAdvertiseVDOCapability_MissingMarkerIsAnError(t *testing.T) {
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	if err := AdvertiseVDOCapability(
		context.Background(), client, "node-a", filepath.Join(t.TempDir(), "missing"),
	); err == nil {
		t.Error("AdvertiseVDOCapability succeeded with no marker file to read")
	}
}

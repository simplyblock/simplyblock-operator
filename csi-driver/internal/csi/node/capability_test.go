package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	marker := writeMarker(t, vdoCapableTrue)
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.Labels[kube.LabelVDOCapable] != vdoCapableTrue {
		t.Errorf("label = %q, want true", node.Labels[kube.LabelVDOCapable])
	}
	if node.Annotations[kube.AnnoVDOCapableManagedBy] != kube.AnnoVDOCapableManagedByAutoDetect {
		t.Errorf("managed-by annotation = %q, want %q",
			node.Annotations[kube.AnnoVDOCapableManagedBy], kube.AnnoVDOCapableManagedByAutoDetect)
	}
}

func TestAdvertiseVDOCapability_WritesFalse(t *testing.T) {
	marker := writeMarker(t, vdoCapableFalse)
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableFalse {
		t.Errorf("label = %q, want false", node.Labels[kube.LabelVDOCapable])
	}
}

// A label with no managed-by annotation is an operator's, left alone.
func TestAdvertiseVDOCapability_LeavesAnOperatorSetLabelAlone(t *testing.T) {
	marker := writeMarker(t, vdoCapableFalse)
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{kube.LabelVDOCapable: vdoCapableTrue},
	}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableTrue {
		t.Errorf("label = %q, want the operator's true left untouched", node.Labels[kube.LabelVDOCapable])
	}
	if _, ok := node.Annotations[kube.AnnoVDOCapableManagedBy]; ok {
		t.Error("the probe claimed an operator-set label as its own")
	}
}

// A label the probe wrote before (has the managed-by annotation) is fair
// game to overwrite, which is how a lost capability flips back to false.
func TestAdvertiseVDOCapability_OverwritesItsOwnPriorLabel(t *testing.T) {
	marker := writeMarker(t, vdoCapableFalse)
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "node-a",
		Labels:      map[string]string{kube.LabelVDOCapable: vdoCapableTrue},
		Annotations: map[string]string{kube.AnnoVDOCapableManagedBy: kube.AnnoVDOCapableManagedByAutoDetect},
	}})

	if err := AdvertiseVDOCapability(context.Background(), client, "node-a", marker); err != nil {
		t.Fatalf("AdvertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableFalse {
		t.Errorf("label = %q, want the probe's own label overwritten to false", node.Labels[kube.LabelVDOCapable])
	}
}

// Regression: the marker does not exist yet when the node plugin starts.
//
// The postStart hook that writes it and the container's own entrypoint run
// concurrently, so reading the marker once, at process start, found nothing on
// every node of a seven-node cluster. The node then carried no vdo-capable
// label at all, and a volume asking for client-side compression could never be
// scheduled anywhere: the feature was silently off across the whole deployment.
func TestAdvertiseVDOCapabilityWaitsForAMarkerTheHookHasNotWrittenYet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.WriteFile(path, []byte(vdoCapableTrue), 0o600); err != nil {
			panic(err)
		}
	}()

	err := advertiseVDOCapability(context.Background(), client, "node-a", path, time.Minute, time.Millisecond)
	if err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableTrue {
		t.Errorf("label = %q, want the capability the hook reported once it had run",
			node.Labels[kube.LabelVDOCapable])
	}
}

// The wait is bounded. A hook that never writes the marker at all is a node
// whose capability is unknown, and waiting on it forever would leave a
// goroutine holding a question nothing will answer.
func TestAdvertiseVDOCapabilityGivesUpOnAMarkerThatNeverArrives(t *testing.T) {
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	err := advertiseVDOCapability(
		context.Background(), client, "node-a", filepath.Join(t.TempDir(), "missing"),
		20*time.Millisecond, time.Millisecond)

	if err == nil {
		t.Fatal("the advertiser reported success with no marker ever written")
	}
	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if _, ok := node.Labels[kube.LabelVDOCapable]; ok {
		t.Error("a node whose capability was never answered was labeled anyway")
	}
}

// A shutdown ends the wait rather than outliving it.
func TestAdvertiseVDOCapabilityStopsWaitingWhenTheProcessIsShuttingDown(t *testing.T) {
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := advertiseVDOCapability(
		ctx, client, "node-a", filepath.Join(t.TempDir(), "missing"), time.Hour, time.Millisecond,
	); err == nil {
		t.Fatal("the advertiser kept waiting after its context was canceled")
	}
}

// A marker that exists and cannot be read is not a marker that has not been
// written yet, and waiting out the budget on it would turn a permission problem
// into a timeout that says nothing about the cause.
func TestAdvertiseVDOCapabilityFailsFastOnAMarkerItCannotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("create the unreadable marker: %v", err)
	}
	client := kfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	start := time.Now()
	err := advertiseVDOCapability(context.Background(), client, "node-a", path, time.Hour, time.Second)
	if err == nil {
		t.Fatal("a marker that cannot be read was accepted")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the advertiser waited %s on a marker it could not read", elapsed)
	}
}

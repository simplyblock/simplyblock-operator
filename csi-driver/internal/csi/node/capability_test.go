package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

// The two values the label carries, as the tests spell them.
const (
	vdoCapableTrue  = "true"
	vdoCapableFalse = "false"
)

// What modprobe says about a module the kernel does not have, which is the
// whole reason the probe reports its output rather than only its exit status.
const (
	noDmVDO = "modprobe: FATAL: Module dm-vdo not found"
	noKvdo  = "modprobe: FATAL: Module kvdo not found"
)

// absent is a kernel carrying neither module.
func absent(k *fakeKernel) *fakeKernel {
	k.says["dm-vdo"], k.says["kvdo"] = noDmVDO, noKvdo
	return k
}

// fakeKernel answers the probe's commands and records what it was asked, so a
// test can stand in for a kernel that does or does not carry VDO.
type fakeKernel struct {
	asked   [][]string
	loads   map[string]bool   // module name to whether modprobe succeeds
	says    map[string]string // module name to what a failed modprobe printed
	release string
	segtype string
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{
		loads:   map[string]bool{},
		says:    map[string]string{},
		release: "6.11.0-19-generic",
		segtype: "striped\nvdo\nvdo-pool\n",
	}
}

func (k *fakeKernel) run(_ context.Context, name string, args ...string) (string, error) {
	k.asked = append(k.asked, append([]string{name}, args...))
	switch name {
	case "uname":
		return k.release, nil
	case "lvm":
		return k.segtype, nil
	case "modprobe":
		module := args[0]
		if k.loads[module] {
			return "", nil
		}
		return k.says[module], errors.New("exit status 1")
	}
	return "", errors.New("unexpected command " + name)
}

func (k *fakeKernel) askedFor(name string, args ...string) bool {
	want := strings.Join(append([]string{name}, args...), " ")
	for _, call := range k.asked {
		if strings.Join(call, " ") == want {
			return true
		}
	}
	return false
}

func testNode(labels, annotations map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a", Labels: labels, Annotations: annotations,
	}}
}

func TestAdvertiseVDOCapabilityLabelsACapableNode(t *testing.T) {
	kernel := newFakeKernel()
	kernel.loads["dm-vdo"] = true
	client := kfake.NewSimpleClientset(testNode(nil, nil))

	if err := advertiseVDOCapability(context.Background(), client, "node-a", kernel.run); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
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

// kvdo is tried when dm-vdo is not there, because "VDO in the kernel" is two
// different modules depending on the node's operating system.
func TestAdvertiseVDOCapabilityFallsBackToKvdo(t *testing.T) {
	kernel := newFakeKernel()
	kernel.says["dm-vdo"] = noDmVDO
	kernel.loads["kvdo"] = true
	client := kfake.NewSimpleClientset(testNode(nil, nil))

	if err := advertiseVDOCapability(context.Background(), client, "node-a", kernel.run); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	if !kernel.askedFor("modprobe", "kvdo") {
		t.Errorf("kvdo was never tried: %v", kernel.asked)
	}
	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableTrue {
		t.Errorf("label = %q, want true: kvdo loaded", node.Labels[kube.LabelVDOCapable])
	}
}

// A module that loads ends the search, so a node carrying dm-vdo is not asked
// for kvdo as well.
func TestAdvertiseVDOCapabilityStopsAtTheFirstModuleThatLoads(t *testing.T) {
	kernel := newFakeKernel()
	kernel.loads["dm-vdo"] = true
	client := kfake.NewSimpleClientset(testNode(nil, nil))

	if err := advertiseVDOCapability(context.Background(), client, "node-a", kernel.run); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	if kernel.askedFor("modprobe", "kvdo") {
		t.Errorf("kvdo was tried after dm-vdo had already loaded: %v", kernel.asked)
	}
}

func TestAdvertiseVDOCapabilityLabelsAnIncapableNodeFalse(t *testing.T) {
	kernel := absent(newFakeKernel())
	client := kfake.NewSimpleClientset(testNode(nil, nil))

	if err := advertiseVDOCapability(context.Background(), client, "node-a", kernel.run); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableFalse {
		t.Errorf("label = %q, want false", node.Labels[kube.LabelVDOCapable])
	}
}

// The probe asks the three questions a reader needs in order to tell a node
// that cannot run VDO from one whose probe never ran: which kernel, what each
// module said, and whether LVM offers the segment types at all.
func TestTheProbeAsksWhatAReaderNeedsToDiagnoseIt(t *testing.T) {
	kernel := absent(newFakeKernel())
	client := kfake.NewSimpleClientset(testNode(nil, nil))

	if err := advertiseVDOCapability(context.Background(), client, "node-a", kernel.run); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	for _, want := range [][]string{
		{"uname", "-r"},
		{"modprobe", "dm-vdo"},
		{"modprobe", "kvdo"},
		{"lvm", "segtypes"},
	} {
		if !kernel.askedFor(want[0], want[1:]...) {
			t.Errorf("the probe never ran %v; its answer cannot be diagnosed from a log", want)
		}
	}
}

// A kernel that answers nothing at all is a node that cannot run VDO, not a
// node plugin that fails to start: every volume not needing the capability is
// still served here.
func TestTheProbeSurvivesACommandThatIsNotThere(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	missing := func(context.Context, string, ...string) (string, error) {
		return "", errors.New(`exec: "modprobe": executable file not found in $PATH`)
	}

	if err := advertiseVDOCapability(context.Background(), client, "node-a", missing); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableFalse {
		t.Errorf("label = %q, want false", node.Labels[kube.LabelVDOCapable])
	}
}

// A label with no managed-by annotation is an operator's, left alone.
func TestAdvertiseVDOCapabilityLeavesAnOperatorSetLabelAlone(t *testing.T) {
	kernel := newFakeKernel()
	client := kfake.NewSimpleClientset(
		testNode(map[string]string{kube.LabelVDOCapable: vdoCapableTrue}, nil))

	if err := advertiseVDOCapability(context.Background(), client, "node-a", kernel.run); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableTrue {
		t.Errorf("label = %q, want the operator's true left untouched", node.Labels[kube.LabelVDOCapable])
	}
	if _, ok := node.Annotations[kube.AnnoVDOCapableManagedBy]; ok {
		t.Error("the probe claimed an operator-set label as its own")
	}
	// It still probed: an override that disagrees with the kernel under it is
	// worth being able to see.
	if !kernel.askedFor("modprobe", "dm-vdo") {
		t.Error("the probe skipped the kernel entirely on an overridden node")
	}
}

// A label the probe wrote before is fair game to overwrite, which is how a lost
// capability flips back to false.
func TestAdvertiseVDOCapabilityOverwritesItsOwnPriorLabel(t *testing.T) {
	kernel := absent(newFakeKernel())
	client := kfake.NewSimpleClientset(testNode(
		map[string]string{kube.LabelVDOCapable: vdoCapableTrue},
		map[string]string{kube.AnnoVDOCapableManagedBy: kube.AnnoVDOCapableManagedByAutoDetect},
	))

	if err := advertiseVDOCapability(context.Background(), client, "node-a", kernel.run); err != nil {
		t.Fatalf("advertiseVDOCapability: %v", err)
	}

	node, _ := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if node.Labels[kube.LabelVDOCapable] != vdoCapableFalse {
		t.Errorf("label = %q, want the probe's own label overwritten to false",
			node.Labels[kube.LabelVDOCapable])
	}
}

func TestVDOSegtypes(t *testing.T) {
	cases := map[string]string{
		"striped\nvdo\nvdo-pool\n": "vdo, vdo-pool",
		"striped\nlinear\n":        "none",
		"":                         "none",
	}
	for out, want := range cases {
		if got := vdoSegtypes(out); got != want {
			t.Errorf("vdoSegtypes(%q) = %q, want %q", out, got, want)
		}
	}
}

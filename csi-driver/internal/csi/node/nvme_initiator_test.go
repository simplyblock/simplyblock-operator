// Tests that a stage on a node whose kernel has no NVMe-oF initiator says so,
// naming what to install, instead of handing kubelet what nvme-cli printed.
package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

const awsKernel = "6.8.0-1044-aws"

// fabricsHost is a host whose fabrics device is there or not, and whose modprobe
// either loads the initiator or says why it cannot.
type fabricsHost struct {
	fabrics    bool
	modprobe   error
	modprobed  int
	loadsOnAsk bool
}

func (k *fabricsHost) run(_ context.Context, name string, args ...string) (string, error) {
	switch name {
	case "uname":
		return awsKernel, nil
	case "modprobe":
		k.modprobed++
		if k.modprobe != nil {
			return "modprobe: FATAL: Module " + args[0] + " not found in directory /lib/modules/" + awsKernel, k.modprobe
		}
		k.fabrics = k.fabrics || k.loadsOnAsk
		return "", nil
	}
	return "", errors.New("unexpected command " + name)
}

func (k *fabricsHost) present(string) bool { return k.fabrics }

func (k *fabricsHost) check() func(context.Context) error {
	return initiatorCheck{run: k.run, present: k.present}.ensure
}

func stageRequest(t *testing.T) *csi.NodeStageVolumeRequest {
	t.Helper()
	return &csi.NodeStageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCapability(),
		VolumeContext:     stagedContext(),
	}
}

// Regression: 2026-11-11-csi-node-missing-nvme-tcp-module — on an Ubuntu worker
// without linux-modules-extra the stage failed with nvme-cli's own output,
// "Failed to open /dev/nvme-fabrics: No such file or directory" once for every
// path, and nothing said which package the node was missing. The pod stayed in
// ContainerCreating behind a mount error nobody could act on.
func TestStageNamesTheMissingNVMeInitiator(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	host := &fabricsHost{modprobe: errors.New("exit status 1")}
	ns.ensureInitiator = host.check()

	_, err := ns.NodeStageVolume(context.Background(), stageRequest(t))

	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeStageVolume returned %v, want FailedPrecondition: the node cannot connect any volume", err)
	}
	wantInError := []string{
		"nvme-tcp", "linux-modules-extra-" + awsKernel, "kernel-modules-extra", "not found in directory",
	}
	for _, want := range wantInError {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error reads %q, want it to name %q", err.Error(), want)
		}
	}
	if runner.called("up") {
		t.Error("the stack was brought up on a node that cannot connect, so the reason is buried under a connect error")
	}
}

// A module that is installed and not yet loaded is loaded, and the stage goes
// on: the node needs nothing done to it.
func TestStageLoadsAnInitiatorThatIsInstalled(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	host := &fabricsHost{loadsOnAsk: true}
	ns.ensureInitiator = host.check()

	if _, err := ns.NodeStageVolume(context.Background(), stageRequest(t)); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if host.modprobed != 1 {
		t.Errorf("modprobe ran %d times, want once", host.modprobed)
	}
	if !runner.called("up") {
		t.Error("the stack was not brought up after the initiator loaded")
	}
}

// A node whose fabrics device is there is asked nothing: the check runs on
// every stage and must cost a stat.
func TestStageAsksNothingOfANodeWithAnInitiator(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	host := &fabricsHost{fabrics: true, modprobe: errors.New("exit status 1")}
	ns.ensureInitiator = host.check()

	if _, err := ns.NodeStageVolume(context.Background(), stageRequest(t)); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if host.modprobed != 0 {
		t.Errorf("modprobe ran %d times on a node whose initiator is loaded, want none", host.modprobed)
	}
}

func nvmeTCPLabel(t *testing.T, client *kfake.Clientset) (string, bool) {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, managed := node.Annotations[kube.AnnoNVMeTCPCapableManagedBy]
	return node.Labels[kube.LabelNVMeTCPCapable], managed
}

// A reader sees every node's initiator state at once: a node whose module is
// loaded says so on the label, and marks the label as the probe's own.
func TestAdvertiseNVMeTCPCapabilityLabelsANodeWithTheInitiator(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	host := &fabricsHost{fabrics: true}
	if err := advertiseNVMeTCPCapability(context.Background(), client, "node-a", host.check()); err != nil {
		t.Fatalf("advertiseNVMeTCPCapability: %v", err)
	}
	if label, managed := nvmeTCPLabel(t, client); label != vdoCapableTrue || !managed {
		t.Errorf("label = %q, managed = %t, want true and managed", label, managed)
	}
}

// The whole point for this customer: a node missing linux-modules-extra is
// labeled false, so it stands out in kubectl get nodes without anyone reading a
// DaemonSet log or waiting for a mount to fail.
func TestAdvertiseNVMeTCPCapabilityLabelsANodeWithoutTheInitiatorFalse(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	host := &fabricsHost{modprobe: errors.New("exit status 1")}
	if err := advertiseNVMeTCPCapability(context.Background(), client, "node-a", host.check()); err != nil {
		t.Fatalf("advertiseNVMeTCPCapability: %v", err)
	}
	if label, _ := nvmeTCPLabel(t, client); label != vdoCapableFalse {
		t.Errorf("label = %q, want false", label)
	}
}

// A hand-set label is an operator's override, left alone.
func TestAdvertiseNVMeTCPCapabilityLeavesAnOperatorSetLabelAlone(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(map[string]string{kube.LabelNVMeTCPCapable: vdoCapableTrue}, nil))
	host := &fabricsHost{modprobe: errors.New("exit status 1")}
	if err := advertiseNVMeTCPCapability(context.Background(), client, "node-a", host.check()); err != nil {
		t.Fatalf("advertiseNVMeTCPCapability: %v", err)
	}
	if label, managed := nvmeTCPLabel(t, client); label != vdoCapableTrue || managed {
		t.Errorf("label = %q, managed = %t, want the operator's true left untouched", label, managed)
	}
}

// A label the probe wrote is its own to overwrite, which is how a node that lost
// the module stops claiming it has it.
func TestAdvertiseNVMeTCPCapabilityOverwritesItsOwnPriorLabel(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(
		map[string]string{kube.LabelNVMeTCPCapable: vdoCapableTrue},
		map[string]string{kube.AnnoNVMeTCPCapableManagedBy: kube.AnnoNVMeTCPCapableManagedByAutoDetect},
	))
	host := &fabricsHost{modprobe: errors.New("exit status 1")}
	if err := advertiseNVMeTCPCapability(context.Background(), client, "node-a", host.check()); err != nil {
		t.Fatalf("advertiseNVMeTCPCapability: %v", err)
	}
	if label, _ := nvmeTCPLabel(t, client); label != vdoCapableFalse {
		t.Errorf("label = %q, want the probe's own label overwritten to false", label)
	}
}

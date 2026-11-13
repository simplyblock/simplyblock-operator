// Tests that a stage on a node whose kernel has no NVMe-oF initiator says so,
// naming what to install, instead of handing kubelet what nvme-cli printed.
package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

const awsKernel = "6.8.0-1044-aws"

// fabricsDevice is what nvme_fabrics exposes once it is loaded, with or without
// a transport beside it.
const fabricsDevice = "/dev/nvme-fabrics"

// fabricsHost is a host whose nvme_tcp module is loaded or not, whose fabrics
// device is there or not, and whose modprobe either loads the initiator or says
// why it cannot. The two are separate facts: nvme_fabrics ships in the base
// kernel package and brings the device with it, without the transport.
type fabricsHost struct {
	loaded     bool
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
		k.loaded = k.loaded || k.loadsOnAsk
		return "", nil
	}
	return "", errors.New("unexpected command " + name)
}

func (k *fabricsHost) present(path string) bool {
	if path == fabricsDevice {
		return k.fabrics
	}
	return k.loaded
}

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
		"nvme-tcp", "linux-modules-extra-" + awsKernel, "not found in directory",
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
	host := &fabricsHost{loaded: true, modprobe: errors.New("exit status 1")}
	ns.ensureInitiator = host.check()

	if _, err := ns.NodeStageVolume(context.Background(), stageRequest(t)); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if host.modprobed != 0 {
		t.Errorf("modprobe ran %d times on a node whose initiator is loaded, want none", host.modprobed)
	}
}

// A node whose fabrics device is there and whose nvme_tcp module is not cannot
// connect a volume either: nvme_fabrics ships in the base kernel package and
// brings /dev/nvme-fabrics with it. Seen on the EKS test cluster, where the
// stage went on and failed with what nvme-cli prints for a transport the
// kernel does not have, which names nothing to install.
func TestFabricsDeviceAloneIsNotAnInitiator(t *testing.T) {
	host := &fabricsHost{fabrics: true, modprobe: errors.New("exit status 1")}

	err := host.check()(context.Background())

	if err == nil {
		t.Fatal("the check passed on a node with /dev/nvme-fabrics and no nvme_tcp module")
	}
	if !strings.Contains(err.Error(), "linux-modules-extra-"+awsKernel) {
		t.Errorf("the error reads %q, want it to name the package", err.Error())
	}
}

// A modprobe that succeeds is believed even when the module shows no sysfs
// entry: a kernel with the transport built in may have none, and refusing such
// a node would stop a plugin that works.
func TestInitiatorCheckBelievesAModprobeThatSucceeds(t *testing.T) {
	host := &fabricsHost{}

	if err := host.check()(context.Background()); err != nil {
		t.Fatalf("the check failed after modprobe succeeded: %v", err)
	}
}

// A pNFS volume is refused the same way. Its client connects the backing
// namespace over NVMe/TCP and reads and writes it directly, so it needs the
// initiator as much as a block volume does, and its own stage path would
// otherwise fail with what nvme-cli printed.
func TestPNFSStageNamesTheMissingNVMeInitiator(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	host := &fabricsHost{modprobe: errors.New("exit status 1")}
	ns.ensureInitiator = host.check()
	req := stageRequest(t)
	req.VolumeContext[csicommon.CtxAccessProtocol] = csicommon.AccessProtocolNFS

	_, err := ns.NodeStageVolume(context.Background(), req)

	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("NodeStageVolume returned %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "linux-modules-extra-"+awsKernel) {
		t.Errorf("the error reads %q, want it to name the package", err.Error())
	}
}

// The node plugin refuses to start on a node that cannot connect a volume, and
// leaves the reason where the pod shows it: the container's termination
// message, which `kubectl describe pod` prints under the last state of a
// container in a restart loop.
func TestStartRefusesANodeWithoutTheNVMeInitiator(t *testing.T) {
	host := &fabricsHost{modprobe: errors.New("exit status 1")}
	message := filepath.Join(t.TempDir(), "termination-log")

	err := requireInitiator(context.Background(), host.check(), message)

	if err == nil {
		t.Fatal("the plugin starts on a node that cannot connect a volume")
	}
	written, readErr := os.ReadFile(message)
	if readErr != nil {
		t.Fatalf("no termination message was written: %v", readErr)
	}
	for _, want := range []string{"nvme-tcp", "linux-modules-extra-" + awsKernel} {
		if !strings.Contains(string(written), want) {
			t.Errorf("the termination message reads %q, want it to name %q", written, want)
		}
	}
}

// A node with the initiator starts, and no termination message is written.
func TestStartAcceptsANodeWithTheNVMeInitiator(t *testing.T) {
	host := &fabricsHost{loadsOnAsk: true}
	message := filepath.Join(t.TempDir(), "termination-log")

	if err := requireInitiator(context.Background(), host.check(), message); err != nil {
		t.Fatalf("the plugin refused a node whose initiator loads: %v", err)
	}
	if _, err := os.Stat(message); err == nil {
		t.Error("a termination message was written for a node that starts")
	}
}

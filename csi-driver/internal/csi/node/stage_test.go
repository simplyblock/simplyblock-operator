// Tests for the format decision, run through the stack the node service
// actually stages with: a real fabric layer over a fake kernel, and a real
// filesystem layer over a fake mounter and a scripted content reading.
//
// The contract is asserted by what was executed and what was mounted, not by
// how the code got there. Nothing here scripts blkid, because the decision no
// longer rests on it: what a device carries is a positive reading of the
// device's own bytes, and the reading is the thing these tests supply.

package node

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	k8smount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/plans"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/mount"
)

// fakeDevice stands in for the NVMe-oF block device staging operates on, and
// extFS and xfsFS name the two filesystems the tests move between.
const (
	fakeDevice = "/dev/fake-lvol"
	extFS      = "ext4"
	xfsFS      = "xfs"
)

// scriptedResult is one external command's scripted outcome: its combined
// output and its error, in the order staging runs commands.
type scriptedResult struct {
	out string
	err error
}

// scriptedExec builds a FakeExec that answers successive commands from script
// and records every invocation's argv, so a test can assert which commands
// staging chose to run. Scripting more results than the code under test
// consumes is fine. Running more commands than scripted panics, so each test
// scripts the longest path it wants to observe.
func scriptedExec(script []scriptedResult) (*testingexec.FakeExec, *[][]string) {
	fe := &testingexec.FakeExec{}
	calls := &[][]string{}
	for _, r := range script {
		fe.CommandScript = append(fe.CommandScript, func(cmd string, args ...string) utilexec.Cmd {
			*calls = append(*calls, append([]string{cmd}, args...))
			fc := &testingexec.FakeCmd{
				CombinedOutputScript: []testingexec.FakeAction{
					func() ([]byte, []byte, error) { return []byte(r.out), nil, r.err },
				},
			}
			return testingexec.InitFakeCmd(fc, cmd, args...)
		})
	}
	return fe, calls
}

// fakeFabric is the node's fabric as these tests need it: a connector that
// attaches nothing and a resolver that answers with one reachable namespace.
// What is under test is the decision above it, so the device is a given.
type fakeFabric struct{ device nvme.Device }

func newFakeFabric() *fakeFabric {
	return &fakeFabric{device: nvme.Device{
		Namespace: nvme.Namespace{
			ID:         1,
			Name:       filepath.Base(fakeDevice),
			DevicePath: fakeDevice,
			UUID:       pvcTestLvol,
			Paths:      []nvme.Path{{Controller: "nvme0", ANAState: nvme.ANAOptimized, NSID: 1}},
		},
		Subsystem: nvme.Subsystem{
			ID:          "nvme-subsys0",
			NQN:         "nqn.2023-05.io.simplyblock:lvol:" + pvcTestLvol,
			Controllers: []nvme.Controller{{ID: "nvme0", State: "live"}},
		},
	}}
}

func (f *fakeFabric) List(context.Context) ([]nvme.Device, error) {
	return []nvme.Device{f.device}, nil
}

func (f *fakeFabric) ListWithSelector(ctx context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	all, err := f.List(ctx)
	if err != nil {
		return nil, err
	}
	return sel.Filter(all), nil
}

func (f *fakeFabric) ByUUID(context.Context, string) (nvme.Device, error) { return f.device, nil }
func (f *fakeFabric) ByDevicePath(context.Context, string) (nvme.Device, error) {
	return f.device, nil
}

func (f *fakeFabric) ByNamespace(context.Context, string, nvme.NamespaceID) (nvme.Device, error) {
	return f.device, nil
}

func (f *fakeFabric) Connect(context.Context, nvmeof.Target) error { return nil }

func (f *fakeFabric) ConnectPaths(_ context.Context, targets []nvmeof.Target) ([]nvmeof.PathResult, error) {
	results := make([]nvmeof.PathResult, 0, len(targets))
	for _, target := range targets {
		results = append(results, nvmeof.PathResult{Target: target, Live: true})
	}
	return results, nil
}

func (f *fakeFabric) Disconnect(context.Context, string) error                    { return nil }
func (f *fakeFabric) DisconnectController(context.Context, nvme.Controller) error { return nil }
func (f *fakeFabric) IsConnected(context.Context, string) (bool, error)           { return true, nil }

// fixedReading is a content reader that answers the same reading for every
// device, which is how these tests say what is on the one device there is.
type fixedReading struct {
	reading blockdev.Reading
	reads   int
}

func (r *fixedReading) Read(context.Context, blockdev.Device) (blockdev.Reading, error) {
	r.reads++
	return r.reading, nil
}

// stagingDir returns a fresh staging parent for one test, resolved through
// symlinks because FakeMounter records the resolved target and the comparison
// has to match it (macOS puts temporary directories behind /var → /private/var).
func stagingDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return base
}

// stagingServer is a node server whose stack runs the real layers over the
// fakes above, which is what makes these assertions about the layers the
// driver stages with rather than about a stand-in.
func stagingServer(t *testing.T, reading blockdev.Reading, script []scriptedResult) (
	*Server, *[][]string, *k8smount.FakeMounter,
) {
	t.Helper()

	fe, calls := scriptedExec(script)
	fm := k8smount.NewFakeMounter(nil)
	mounter := mount.NewWith(fm, fe)
	fabric := newFakeFabric()

	store := volstack.NewStore(t.TempDir())
	s := &stack{
		seams: plans.NodeConfig{
			Connector:  fabric,
			Devices:    fabric,
			Content:    &fixedReading{reading: reading},
			Filesystem: mounter.FilesystemOps(),
		},
		store: store,
	}
	s.runner = volstack.NewRunner(store)

	return &Server{mounter: mounter, stack: s, volumeLocks: csicommon.NewVolumeLocks()}, calls, fm
}

// stageThroughTheStack runs the stage the way NodeStageVolume does, against the
// volume context a staged simplyblock volume carries.
func stageThroughTheStack(t *testing.T, ns *Server, parent string, vc map[string]string) error {
	t.Helper()

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: parent,
		VolumeCapability:  mountCapability(),
		VolumeContext:     vc,
	}
	plan, err := ns.attachPlan(
		context.Background(), req.GetVolumeId(), getStagingTargetPath(req), vc, req.GetVolumeCapability())
	if err != nil {
		t.Fatalf("build the stack plan: %v", err)
	}
	_, err = ns.stack.runner.Up(context.Background(), req.GetVolumeId(), plan)
	return err
}

// ranFormat reports whether any mkfs reached the node.
func ranFormat(calls *[][]string) []string {
	for _, call := range *calls {
		if strings.HasPrefix(call[0], "mkfs") {
			return call
		}
	}
	return nil
}

// Regression: 2026-09-04-format-decided-by-upstream-reprobe. Staging probed the
// device, found ext4, and then handed the device to a helper that probes it
// again itself and formats whenever that second probe reads blank. On a fabric
// that degraded between the two probes this reformatted a filesystem staging
// had just positively identified, and the annotation guard never ran because it
// is only consulted when the first reading is blank. This is the remaining door
// of the mkfs data-loss incident of 2026-09-03 after the annotation guard.
//
// The stack closes it structurally: the device is read once, and the layer that
// formats is the only thing that decides, from that one reading, whether it may.
func TestStageNeverFormatsWhenTheReadingFoundAFilesystem(t *testing.T) {
	ns, calls, fm := stagingServer(t,
		blockdev.Reading{Content: blockdev.ContentFilesystem, Type: extFS},
		[]scriptedResult{{out: ""}, {out: ""}})

	parent := stagingDir(t)
	if err := stageThroughTheStack(t, ns, parent, stagedContext()); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if call := ranFormat(calls); call != nil {
		t.Fatalf(
			"staging ran %v on a device whose reading found ext4; "+
				"a volume that was formatted once must never be formatted again", call)
	}

	stagingPath := filepath.Join(parent, pvcTestHandle)
	mounted := false
	for _, mp := range fm.MountPoints {
		if mp.Path == stagingPath && mp.Type == extFS {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("staging did not mount the ext4 filesystem at %s; mounts: %+v", stagingPath, fm.MountPoints)
	}
}

// Regression: 2026-09-06-staged-a-filesystem-the-class-did-not-ask-for. Staging
// mounted whatever the device carried, warning about the disagreement and
// carrying on. Nothing was destroyed by that, and it is still the wrong answer:
// a volume serving XFS to a class that says ext4 is a misconfiguration nobody
// is told about, and the next thing to notice it is whatever decides to make
// the device match the class again. That decision reformats.
//
// So staging refuses, while the volume is intact and while the disagreement is
// the thing in front of whoever is looking. It must refuse without reformatting,
// which is the half of this that would be catastrophic to get wrong.
func TestStageRefusesAFilesystemTheClassDidNotAskFor(t *testing.T) {
	ns, calls, fm := stagingServer(t,
		blockdev.Reading{Content: blockdev.ContentFilesystem, Type: xfsFS},
		[]scriptedResult{{out: ""}, {out: ""}})

	err := stageThroughTheStack(t, ns, stagingDir(t), stagedContext())
	if err == nil {
		t.Fatal("staging mounted a device carrying xfs for a volume that asks for ext4")
	}
	for _, want := range []string{xfsFS, extFS} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so a reader cannot tell what disagreed: %v", want, err)
		}
	}

	if call := ranFormat(calls); call != nil {
		t.Fatalf("staging formatted a device carrying a filesystem: %v", call)
	}
	if len(fm.MountPoints) != 0 {
		t.Errorf("staging mounted something anyway: %v", fm.MountPoints)
	}
}

// The same rule on the other branch: a blank reading settled by the claim's
// record. The record says what the volume was formatted as, so a class asking
// for something else is the same disagreement, reached by a different road.
//
// Regression: 2026-09-06-staged-a-filesystem-the-class-did-not-ask-for.
func TestStageRefusesWhenTheRecordedFilesystemIsNotTheClassOne(t *testing.T) {
	ns, calls, fm := stagingServer(t,
		blockdev.Reading{Content: blockdev.ContentBlank},
		[]scriptedResult{{out: ""}, {out: ""}})

	guard, _ := newPVCTestNodeServer(t, annotatedPVC(xfsFS))
	ns.kubeClient, ns.manager = guard.kubeClient, guard.manager

	vc := stagedContext()
	vc[csicommon.CSIStorageNamespaceKey] = pvcTestNamespace
	vc[csicommon.CSIStorageNameKey] = pvcTestName

	err := stageThroughTheStack(t, ns, stagingDir(t), vc)
	if err == nil {
		t.Fatal("staging mounted a volume recorded as xfs for a class that asks for ext4")
	}
	for _, want := range []string{xfsFS, extFS} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if call := ranFormat(calls); call != nil {
		t.Fatalf("staging formatted a volume the claim says holds a filesystem: %v", call)
	}
	if len(fm.MountPoints) != 0 {
		t.Errorf("staging mounted something anyway: %v", fm.MountPoints)
	}
}

// A device that reads blank on a volume nothing has a record for is the one
// case a format is permitted, and it is formatted as the class asks.
func TestStageFormatsOnlyABlankDeviceWithNoRecord(t *testing.T) {
	ns, calls, fm := stagingServer(t,
		blockdev.Reading{Content: blockdev.ContentBlank},
		[]scriptedResult{{out: ""}, {out: ""}})

	parent := stagingDir(t)
	if err := stageThroughTheStack(t, ns, parent, stagedContext()); err != nil {
		t.Fatalf("stage: %v", err)
	}

	call := ranFormat(calls)
	if call == nil {
		t.Fatalf("a blank device was never formatted; the commands run were %v", *calls)
	}
	if call[0] != "mkfs.ext4" || call[len(call)-1] != fakeDevice {
		t.Errorf("the format ran as %v, want mkfs.ext4 against %s", call, fakeDevice)
	}
	if len(fm.MountPoints) != 1 {
		t.Errorf("the formatted device was not mounted: %v", fm.MountPoints)
	}
}

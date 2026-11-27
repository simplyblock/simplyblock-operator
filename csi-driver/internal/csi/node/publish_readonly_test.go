// Tests for publishing a volume read-only when the pod asks for it.

package node

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8smount "k8s.io/mount-utils"

	"github.com/simplyblock/atlas/devmapper"

	"github.com/simplyblock/csi-driver/internal/mount"
)

// recordingPublishMapper keeps the read-only mappings a publish makes.
type recordingPublishMapper struct {
	created map[string]string // name → device
	removed []string
}

func (m *recordingPublishMapper) Sectors(context.Context, string) (uint64, error) { return 2048, nil }

func (m *recordingPublishMapper) CreateReadOnly(_ context.Context, name, device string, _ uint64) error {
	m.created[name] = device
	return nil
}

func (m *recordingPublishMapper) ReadOnly(_ context.Context, name string) (bool, error) {
	if _, ok := m.created[name]; !ok {
		return false, devmapper.ErrNotFound
	}
	return true, nil
}

func (m *recordingPublishMapper) Remove(_ context.Context, name string) error {
	m.removed = append(m.removed, name)
	delete(m.created, name)
	return nil
}

// readOnlyPublishServer is a stacked node server whose mounter and publish
// mapper the test can read back.
func readOnlyPublishServer(t *testing.T) (*Server, *k8smount.FakeMounter, *recordingPublishMapper) {
	t.Helper()
	ns, _ := newStackedServer(t, newRecordingRunner())
	fake := k8smount.NewFakeMounter(nil)
	ns.mounter = mount.NewWith(fake, nil)
	mapper := &recordingPublishMapper{created: map[string]string{}}
	ns.publishMapper = mapper
	return ns, fake, mapper
}

func publishRequest(t *testing.T, volCap *csi.VolumeCapability, readOnly bool) *csi.NodePublishVolumeRequest {
	t.Helper()
	return &csi.NodePublishVolumeRequest{
		VolumeId:          pvcTestHandle,
		StagingTargetPath: stashedAt(t, stagedContext()),
		TargetPath:        filepath.Join(t.TempDir(), "published"),
		VolumeCapability:  volCap,
		Readonly:          readOnly,
	}
}

// mountOf is the fake kernel's mount at target.
func mountOf(t *testing.T, fake *k8smount.FakeMounter, target string) k8smount.MountPoint {
	t.Helper()
	// The fake records the resolved path, and a temporary directory can sit
	// behind a symlink (/var on macOS).
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		resolved = target
	}
	for _, mp := range fake.MountPoints {
		if mp.Path == target || mp.Path == resolved {
			return mp
		}
	}
	t.Fatalf("nothing mounted at %s; mounts %v", target, fake.MountPoints)
	return k8smount.MountPoint{}
}

func blockPublishCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

// Regression: 2026-11-25-publish-ignores-readonly. NodePublishVolume never read
// the request's readonly field, so a pod that asked for its volume read-only
// was published a writable filesystem; only kubelet's own read-only container
// mount stood between the pod and its writes.
func TestAReadOnlyPublishMountsTheFilesystemReadOnly(t *testing.T) {
	ns, fake, _ := readOnlyPublishServer(t)
	req := publishRequest(t, mountCapability(), true)
	if _, err := ns.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if opts := mountOf(t, fake, req.GetTargetPath()).Opts; !slices.Contains(opts, "ro") {
		t.Errorf("mount options %v, want ro", opts)
	}
}

func TestAWritablePublishMountsTheFilesystemWritable(t *testing.T) {
	ns, fake, _ := readOnlyPublishServer(t)
	req := publishRequest(t, mountCapability(), false)
	if _, err := ns.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if opts := mountOf(t, fake, req.GetTargetPath()).Opts; slices.Contains(opts, "ro") {
		t.Errorf("mount options %v, want no ro", opts)
	}
}

// Regression: 2026-11-25-publish-ignores-readonly, raw block. A read-only raw
// block publish gets a read-only mapping of its own over the staged device.
// Making the staged device read-only instead would take writes away from every
// other pod on the node using the same ReadWriteOnce volume.
func TestAReadOnlyBlockPublishGetsAReadOnlyDeviceOfItsOwn(t *testing.T) {
	ns, fake, mapper := readOnlyPublishServer(t)
	req := publishRequest(t, blockPublishCapability(), true)
	if _, err := ns.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if len(mapper.created) != 1 {
		t.Fatalf("read-only mappings %v, want one", mapper.created)
	}
	for name, device := range mapper.created {
		if device != fakeDevice {
			t.Errorf("mapping %s over %s, want over the staged device %s", name, device, fakeDevice)
		}
		if got := mountOf(t, fake, req.GetTargetPath()).Device; got != "/dev/mapper/"+name {
			t.Errorf("published %s, want the read-only mapping /dev/mapper/%s", got, name)
		}
		if strings.HasPrefix(name, "sb-") {
			t.Errorf("mapping %s takes the sb- prefix, which the swap watchdog claims as a volume's own", name)
		}
	}

	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: req.GetVolumeId(), TargetPath: req.GetTargetPath(),
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if len(mapper.created) != 0 {
		t.Errorf("mappings %v left after unpublish, want none", mapper.created)
	}
}

func TestAWritableBlockPublishMakesNoMapping(t *testing.T) {
	ns, _, mapper := readOnlyPublishServer(t)
	req := publishRequest(t, blockPublishCapability(), false)
	if _, err := ns.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if len(mapper.created) != 0 {
		t.Errorf("read-only mappings %v for a writable publish, want none", mapper.created)
	}
}

// Without device-mapper there is no read-only device to hand up, and a
// writable one is not what the pod asked for.
func TestAReadOnlyBlockPublishWithoutDeviceMapperIsRefused(t *testing.T) {
	ns, _, _ := readOnlyPublishServer(t)
	ns.deviceMapper = false
	_, err := ns.NodePublishVolume(context.Background(), publishRequest(t, blockPublishCapability(), true))
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("NodePublishVolume = %v, want FailedPrecondition", err)
	}
}

// Regression: #989 review (pNFS readonly). A pNFS volume is bound from its
// staging mount by publishPNFSVolume, which returned before the read-only
// handling, so a pod that asked for a pNFS volume read-only was bound it
// writable.
func TestAReadOnlyPNFSPublishBindsTheExportReadOnly(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		ns, fake, _ := readOnlyPublishServer(t)
		vc := stagedContext()
		vc[csicommon.CtxAccessProtocol] = csicommon.AccessProtocolNFS
		req := publishRequest(t, mountCapability(), readOnly)
		req.StagingTargetPath = stashedAt(t, vc)
		if _, err := ns.NodePublishVolume(context.Background(), req); err != nil {
			t.Fatalf("readonly=%v: NodePublishVolume: %v", readOnly, err)
		}
		if opts := mountOf(t, fake, req.GetTargetPath()).Opts; slices.Contains(opts, "ro") != readOnly {
			t.Errorf("readonly=%v: bind options %v", readOnly, opts)
		}
	}
}

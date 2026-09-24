// What the filesystem layer has to guarantee.
//
// This is the layer that formats, so most of what follows is about when it must
// not. StateAbsent is the only state that permits a mkfs, and establishing it
// is the content reading's job rather than a tool's silence: a device that could
// not be read, or that carries something this driver did not put there, is not
// an empty device.

package layers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// fakeFS records what the layer did to the device and to the mount point.
type fakeFS struct {
	formatted      []formatCall
	mounted        []mountCall
	unmounted      []string
	forceUnmounted []string

	mountPoints map[string]bool

	grown [][]string

	formatErr     error
	mountErr      error
	unmountErr    error
	growErr       error
	mountPointErr error
}

type formatCall struct {
	device  string
	fsType  string
	options []string
}

type mountCall struct {
	source, target, fsType string
	options                []string
}

func newFakeFS() *fakeFS { return &fakeFS{mountPoints: map[string]bool{}} }

func (f *fakeFS) Format(_ context.Context, device, fsType string, options []string) error {
	f.formatted = append(f.formatted, formatCall{device, fsType, options})
	return f.formatErr
}

func (f *fakeFS) Mount(_ context.Context, source, target, fsType string, options []string) error {
	f.mounted = append(f.mounted, mountCall{source, target, fsType, options})
	if f.mountErr != nil {
		return f.mountErr
	}
	f.mountPoints[target] = true
	return nil
}

func (f *fakeFS) Unmount(_ context.Context, target string) error {
	f.unmounted = append(f.unmounted, target)
	if f.unmountErr != nil {
		return f.unmountErr
	}
	delete(f.mountPoints, target)
	return nil
}

func (f *fakeFS) ForceUnmount(_ context.Context, target string) error {
	f.forceUnmounted = append(f.forceUnmounted, target)
	delete(f.mountPoints, target)
	return nil
}

func (f *fakeFS) Grow(_ context.Context, command []string) error {
	f.grown = append(f.grown, command)
	return f.growErr
}

func (f *fakeFS) IsMountPoint(_ context.Context, path string) (bool, error) {
	if f.mountPointErr != nil {
		// What a dead mount answers: the real implementation reports the mount
		// it cannot interrogate as an error rather than as a false.
		return false, f.mountPointErr
	}
	return f.mountPoints[path], nil
}

// fakeReader answers the content reading for the device under test.
type fakeReader struct {
	reading blockdev.Reading
	err     error
}

func (f fakeReader) Read(context.Context, blockdev.Device) (blockdev.Reading, error) {
	return f.reading, f.err
}

const stagingPath = "/var/lib/kubelet/plugins/x/staging/vol"

func belowArtifact() volstack.Artifact {
	return volstack.Artifact{
		Devices: []blockdev.Device{{
			Path: "/dev/nvme0n1", Name: "nvme0n1", LogicalBlockSize: 512, SizeBytes: 1 << 30,
		}},
	}
}

func newFS(t *testing.T, fs *fakeFS, reading blockdev.Reading, readErr error) *Filesystem {
	t.Helper()
	return newFSAsking(t, fs, "ext4", reading, readErr)
}

// newFSAsking is newFS for a case that has to name the filesystem the plan asks
// for, because the layer refuses a device carrying any other.
func newFSAsking(t *testing.T, fs *fakeFS, fsType string, reading blockdev.Reading, readErr error) *Filesystem {
	t.Helper()
	return NewFilesystem(FilesystemConfig{
		FsType:      fsType,
		StagingPath: stagingPath,
		Ops:         fs,
		Content:     fakeReader{reading: reading, err: readErr},
	})
}

// Regression: a teardown reaching this layer after the device below it is gone
// has to get a state rather than an error.
//
// NodeUnstageVolume releases the stack and then, for a volume that is being
// deleted, destroys it, and the release is what detaches the fabric. So the
// destroy that follows surveys a plan whose bottom layer now exposes nothing,
// and this layer answering with an error failed the whole RPC. Kubelet retried
// it forever, the volume was never released, and the pool it came from could
// not be deleted while a bound volume remained.
//
// Absent is the honest answer and not merely the convenient one: nothing is
// mounted and there is no device, so nothing of this layer is present on this
// host. It cannot be read as permission to format, because Ensure refuses an
// empty artifact before it observes anything.
func TestObserveWithNoDeviceBelowReportsAbsentWithoutError(t *testing.T) {
	l := newFS(t, newFakeFS(), blockdev.Reading{Content: blockdev.ContentBlank}, nil)

	state, own, err := l.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe errored on a device that is already gone: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Errorf("state = %s, want Absent", state)
	}
	if len(own.Devices) != 0 || own.Path != "" {
		t.Errorf("an absent layer exposed %+v", own)
	}
}

// The mount is still the first question, because total path loss leaves one
// behind after the device is gone. Answering Absent on the strength of the
// missing device alone would have a teardown skip the release that clears it,
// stranding a mount that answers EIO.
func TestObserveWithNoDeviceBelowStillReportsALiveMount(t *testing.T) {
	fs := newFakeFS()
	fs.mountPoints[stagingPath] = true
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentBlank}, nil)

	state, own, err := l.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Errorf("state = %s, want Ready: the mount is still there to be released", state)
	}
	if own.Path != stagingPath {
		t.Errorf("the layer exposed %q, want the staging path it is still mounted at", own.Path)
	}
}

// Ensure is what keeps Absent from meaning "format this": it refuses an empty
// artifact before it observes anything, so the state above can never reach a
// mkfs.
func TestEnsureRefusesAnEmptyArtifactBeforeObserving(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentBlank}, nil)

	if _, err := l.Ensure(context.Background(), volstack.Artifact{}); err == nil {
		t.Fatal("Ensure accepted a plan with no device to put a filesystem on")
	}
	if len(fs.formatted) != 0 {
		t.Errorf("a format ran against no device: %+v", fs.formatted)
	}
}

// An encrypted volume reads as random bytes when it is empty, because the
// control plane stacks an AES-XTS crypto bdev under the namespace it exports
// and decrypting never-written blocks yields pseudo-random plaintext. The
// content probe therefore reports Foreign with no known signature on a volume
// that has nothing on it, and refusing that is refusing every encrypted volume
// there will ever be.
func TestAnEncryptedVolumeIsStagedThoughItsContentCannotBeRead(t *testing.T) {
	fs := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "ext4",
		StagingPath: stagingPath,
		Ops:         fs,
		Content: fakeReader{reading: blockdev.Reading{
			Content: blockdev.ContentForeign,
			Detail:  "no known signature, and the probed regions are not empty: first non-zero byte at 0",
		}},
		Encrypted: true,
	})

	state, _, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe refused an encrypted volume: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Errorf("state = %s, want Absent: the volume has no filesystem and may be formatted", state)
	}
}

// What protects an encrypted volume is the record rather than the read. A
// volume the control plane says already carries a filesystem is not formatted
// again, whatever the bytes on it look like.
func TestAnEncryptedVolumeRecordedAsFormattedIsNotFormattedAgain(t *testing.T) {
	fs := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "ext4",
		StagingPath: stagingPath,
		Ops:         fs,
		Content:     fakeReader{reading: blockdev.Reading{Content: blockdev.ContentForeign}},
		Encrypted:   true,
		PriorFormat: func(context.Context) (string, error) { return "xfs", nil },
	})

	if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
		t.Fatal("an encrypted volume recorded as carrying xfs was accepted for an ext4 plan")
	}
	if len(fs.formatted) > 0 {
		t.Errorf("the volume was formatted over its record: %+v", fs.formatted)
	}
}

// The guard keeps its teeth where the read means something. A plaintext volume
// carrying content this driver did not write is still refused, which is the
// case the reading was introduced for.
func TestAPlaintextVolumeCarryingForeignContentIsStillRefused(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{
		Content: blockdev.ContentForeign, Detail: "first non-zero byte at 0",
	}, nil)

	if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
		t.Fatal("a device carrying foreign content was accepted")
	}
}

// A device positively read as blank is the one case a format is permitted.
func TestFilesystemFormatsOnlyABlankDevice(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentBlank}, nil)

	state, _, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Fatalf("state = %s, want Absent: a blank device is the only thing that may be formatted", state)
	}

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(fs.formatted) != 1 || fs.formatted[0].fsType != "ext4" {
		t.Fatalf("formatted %+v, want one ext4 format", fs.formatted)
	}
	if len(fs.mounted) != 1 || fs.mounted[0].target != stagingPath {
		t.Fatalf("mounted %+v, want the staging path", fs.mounted)
	}
}

// A device that already carries a filesystem is mounted and never formatted,
// and it is mounted as what is on it rather than as what the volume asked for:
// those disagree exactly when it matters, and mounting ext4 as XFS fails.
func TestFilesystemMountsAnExistingFilesystemAndNeverFormats(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	state, _, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateInactive {
		t.Fatalf("state = %s, want Inactive: the filesystem exists and is not mounted", state)
	}

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(fs.formatted) != 0 {
		t.Fatalf("a device carrying a filesystem was formatted: %+v", fs.formatted)
	}
	if len(fs.mounted) != 1 || fs.mounted[0].fsType != "ext4" {
		t.Fatalf("mounted %+v, want the filesystem already on the device mounted as it is", fs.mounted)
	}
}

// A filesystem already mounted at the staging path is ready, and Ensure does
// nothing to it. NodeStageVolume is retried and every verb is convergent.
func TestFilesystemAlreadyMountedIsReady(t *testing.T) {
	fs := newFakeFS()
	fs.mountPoints[stagingPath] = true
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	state, _, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Fatalf("state = %s, want Ready", state)
	}

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(fs.formatted) != 0 || len(fs.mounted) != 0 {
		t.Fatalf("a mounted filesystem was touched: formatted %+v mounted %+v", fs.formatted, fs.mounted)
	}
}

// Every reading that is not blank and not a filesystem refuses, because none of
// them establishes that the device is empty and formatting is irreversible.
func TestFilesystemRefusesEverythingItCannotAccountFor(t *testing.T) {
	cases := []struct {
		name    string
		reading blockdev.Reading
		readErr error
	}{
		{"a device that could not be read", blockdev.Reading{}, errors.New("input/output error")},
		{"an LVM physical volume where a filesystem was expected",
			blockdev.Reading{Content: blockdev.ContentStackLayer, Type: "LVM2_member"}, nil},
		{"a partition table", blockdev.Reading{Content: blockdev.ContentForeign, Type: "gpt"}, nil},
		{"somebody else's filesystem", blockdev.Reading{Content: blockdev.ContentForeign, Type: "vfat"}, nil},
		{"bytes matching nothing known", blockdev.Reading{Content: blockdev.ContentForeign}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeFS()
			l := newFS(t, fs, tc.reading, tc.readErr)

			if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
				t.Error("Observe accepted a device it cannot account for")
			}
			if _, err := l.Ensure(context.Background(), belowArtifact()); err == nil {
				t.Fatal("Ensure proceeded on a device it cannot account for")
			}
			if len(fs.formatted) != 0 {
				t.Fatalf("it formatted anyway: %+v", fs.formatted)
			}
		})
	}
}

// The one that matters most, stated on its own: a read failure is never a
// format. This is the 2026-09-03 incident expressed as a layer.
func TestFilesystemNeverFormatsADeviceItCouldNotRead(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{}, errors.New("no path to the device"))

	_, err := l.Ensure(context.Background(), belowArtifact())
	if err == nil {
		t.Fatal("Ensure succeeded on an unreadable device")
	}
	if len(fs.formatted) != 0 {
		t.Fatalf("an unreadable device was formatted: %+v", fs.formatted)
	}
}

// Release unmounts and keeps the data. It is the only verb an unstage calls.
func TestFilesystemReleaseUnmountsAndKeepsTheData(t *testing.T) {
	fs := newFakeFS()
	fs.mountPoints[stagingPath] = true
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	if err := l.Release(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(fs.unmounted) != 1 || fs.unmounted[0] != stagingPath {
		t.Fatalf("unmounted %v, want the staging path once", fs.unmounted)
	}
	if len(fs.formatted) != 0 {
		t.Fatal("Release wrote to the device")
	}
}

// Release on a path that is not mounted is a no-op rather than an error, because
// a teardown may resume after a crash and arrives at a stack that is partly down.
func TestFilesystemReleaseIsIdempotent(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem}, nil)

	if err := l.Release(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Release on an unmounted path: %v", err)
	}
	if len(fs.unmounted) != 0 {
		t.Errorf("it unmounted something that was not mounted: %v", fs.unmounted)
	}
}

// Destroy does nothing. Removing a filesystem means removing the volume, which
// is the control plane's, and a node that reached for it on a teardown would be
// the defect the four verbs exist to prevent.
func TestFilesystemDestroyDoesNothing(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentBlank}, nil)

	if err := l.Destroy(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fs.formatted) != 0 || len(fs.unmounted) != 0 {
		t.Fatal("Destroy touched the device")
	}
}

// A heal remounts and never reformats: the data exists, which is why a restage
// runs this and not a bring-up.
func TestFilesystemHealRemountsWithoutFormatting(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	if err := l.Heal(context.Background(), belowArtifact(), volstack.Artifact{Path: stagingPath}); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if len(fs.formatted) != 0 {
		t.Fatalf("a heal formatted the device: %+v", fs.formatted)
	}
	if len(fs.mounted) != 1 {
		t.Fatalf("a heal did not remount: %+v", fs.mounted)
	}
}

// XFS refuses to mount two filesystems with the same UUID, which a volume and
// its clone have, so it is mounted with nouuid and ext4 is not.
func TestFilesystemMountFlagsFollowTheFilesystem(t *testing.T) {
	for _, tc := range []struct {
		fsType     string
		wantNoUUID bool
	}{
		{"xfs", true},
		{"ext4", false},
	} {
		t.Run(tc.fsType, func(t *testing.T) {
			fs := newFakeFS()
			l := newFSAsking(t, fs, tc.fsType,
				blockdev.Reading{Content: blockdev.ContentFilesystem, Type: tc.fsType}, nil)

			if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			got := strings.Join(fs.mounted[0].options, ",")
			if has := strings.Contains(got, "nouuid"); has != tc.wantNoUUID {
				t.Errorf("mount options %q, nouuid=%v want %v", got, has, tc.wantNoUUID)
			}
		})
	}
}

// Stripe alignment is passed only when the layer below reports real geometry. A
// virtualized device reports none, and hints computed for the backend underneath
// it describe nothing once its blocks are relocated.
func TestFilesystemAlignsOnlyToKnownGeometry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		geometry  volstack.Geometry
		wantAlign bool
	}{
		{"a striped device below", volstack.Geometry{ChunkBytes: 65536, Stripes: 2}, true},
		{"a virtualized device below", volstack.Geometry{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeFS()
			l := NewFilesystem(FilesystemConfig{
				FsType: "xfs", StagingPath: stagingPath, Ops: fs,
				Content: fakeReader{reading: blockdev.Reading{Content: blockdev.ContentBlank}},
			})

			below := belowArtifact()
			below.Geometry = tc.geometry
			if _, err := l.Ensure(context.Background(), below); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			opts := strings.Join(fs.formatted[0].options, " ")
			if has := strings.Contains(opts, "su="); has != tc.wantAlign {
				t.Errorf("format options %q, stripe alignment=%v want %v", opts, has, tc.wantAlign)
			}
		})
	}
}

// The artifact a mounted filesystem exposes is its path, which is what
// NodeStageVolume acts on.
func TestFilesystemExposesItsPath(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentBlank}, nil)

	art, err := l.Ensure(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if art.Path != stagingPath {
		t.Errorf("Path = %q, want the staging path", art.Path)
	}
}

// Regression: 2026-09-05-heal-stacked-a-second-mount — Heal mounted over a mount
// that was already at the staging path instead of clearing it first, which
// leaves two mounts stacked on one path.
//
// That is the arming condition for a defect the node service already carries:
// its teardown unmounts once and then removes the path recursively, so with two
// mounts stacked the unmount peels one and the removal walks into the
// filesystem still mounted underneath. The path to it is ordinary rather than
// exotic: total path loss leaves a dead mount, Healthy reports it unhealthy, and
// the runner calls Heal.
func TestHealClearsTheMountBeforeRemounting(t *testing.T) {
	fs := newFakeFS()
	fs.mountPoints[stagingPath] = true
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	if err := l.Heal(context.Background(), belowArtifact(), volstack.Artifact{Path: stagingPath}); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if len(fs.unmounted)+len(fs.forceUnmounted) == 0 {
		t.Fatal("Heal mounted over the existing mount without clearing it, stacking two mounts on one path")
	}
	if len(fs.mounted) != 1 {
		t.Fatalf("Heal mounted %d times, want once after clearing", len(fs.mounted))
	}
}

// A dead mount does not come down the ordinary way: the backing device is gone
// and a plain unmount refuses. The layer escalates rather than giving up, which
// is what keeps a heal from stranding the stack.
func TestHealForcesWhenAPlainUnmountRefuses(t *testing.T) {
	fs := newFakeFS()
	fs.mountPoints[stagingPath] = true
	fs.unmountErr = errors.New("device is busy")
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	if err := l.Heal(context.Background(), belowArtifact(), volstack.Artifact{Path: stagingPath}); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if len(fs.forceUnmounted) == 0 {
		t.Fatal("a plain unmount refused and the heal did not fall back to its force path")
	}
}

// A mount point that cannot be interrogated is how a dead mount presents, and
// the release detaches it rather than reading the failure as an empty path.
// Reading it the other way is how total path loss leaves a staging path mounted
// over a device that is gone, with nothing that will ever take it down.
func TestReleaseDetachesAMountItCannotInterrogate(t *testing.T) {
	fs := newFakeFS()
	fs.mountPointErr = errors.New("the mount is dead, because the device behind it is gone")
	fs.unmountErr = errors.New("transport endpoint is not connected")
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	if err := l.Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(fs.forceUnmounted) == 0 {
		t.Fatal("the dead mount was read as absent and left in place")
	}
}

// Release owes the same escalation. Bring-down proceeds through layers whose
// foundation may already be gone, which is the normal case after total path loss
// rather than an edge case, and a layer with no force path strands the stack.
func TestReleaseForcesWhenAPlainUnmountRefuses(t *testing.T) {
	fs := newFakeFS()
	fs.mountPoints[stagingPath] = true
	fs.unmountErr = errors.New("transport endpoint is not connected")
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	if err := l.Release(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(fs.forceUnmounted) == 0 {
		t.Fatal("a plain unmount refused and the release did not fall back to its force path")
	}
}

// A stack layer is a signature the probe positively recognized, so finding one
// means the plan is wrong rather than that the bytes were undecipherable. The
// encryption relaxation does not reach it: an LVM physical-volume or RAID label
// under a plan that names no such layer is somebody else's volume.
func TestAnEncryptedVolumeCarryingAStackLayerIsStillRefused(t *testing.T) {
	fs := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "xfs",
		StagingPath: stagingPath,
		Ops:         fs,
		Content: fakeReader{reading: blockdev.Reading{
			Content: blockdev.ContentStackLayer,
			Detail:  "LVM2_member",
		}},
		Encrypted: true,
	})

	if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
		t.Fatal("Observe accepted an encrypted device carrying an LVM label")
	}
	if len(fs.formatted) > 0 {
		t.Errorf("the device was formatted over a stack layer: %+v", fs.formatted)
	}
}

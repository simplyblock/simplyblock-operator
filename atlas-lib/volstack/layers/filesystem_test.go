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

	formatErr  error
	mountErr   error
	unmountErr error
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

func (f *fakeFS) IsMountPoint(_ context.Context, path string) (bool, error) {
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
	return NewFilesystem(FilesystemConfig{
		FsType:      "ext4",
		StagingPath: stagingPath,
		Ops:         fs,
		Content:     fakeReader{reading: reading, err: readErr},
	})
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
func TestFilesystemMountsWhatIsOnTheDeviceAndNeverFormats(t *testing.T) {
	fs := newFakeFS()
	l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "xfs"}, nil)

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
	if len(fs.mounted) != 1 || fs.mounted[0].fsType != "xfs" {
		t.Fatalf("mounted %+v, want it mounted as the xfs that is on it", fs.mounted)
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
			l := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: tc.fsType}, nil)

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

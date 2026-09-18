// Tests for the export assembler, weighted toward the two things that break
// it: a step re-run after a partial assembly, and a device that already carries
// a filesystem.
//
// The second is the dangerous one. The caller is a reconciler, so Create runs
// again on every pass, and a Create that formats unconditionally destroys the
// data it was asked to serve. TestCreateNeverFormatsANonBlankDevice is the test
// that exists for that.

package export

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

const (
	testNGUID = "71714b79784f4b54756f65624e495374"
	testUUID  = "cb2f293c-6d6f-4687-ad13-eb81fbec7314"
	testFSID  = "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
	testDev   = "/dev/nvme0n1"
)

// fakeDevices resolves one namespace by NGUID.
type fakeDevices struct {
	nvme.DeviceResolver
	device nvme.Device
	err    error
}

func (f *fakeDevices) ByUUID(context.Context, string) (nvme.Device, error) {
	return f.device, f.err
}

// fakeFS records what it was asked to do and can pretend a mount already exists.
type fakeFS struct {
	formatted []string
	mounted   []string
	unmounted []string
	isMounted bool
	formatErr error
}

func (f *fakeFS) Format(_ context.Context, device, _ string, _ []string) error {
	if f.formatErr != nil {
		return f.formatErr
	}
	f.formatted = append(f.formatted, device)
	return nil
}

func (f *fakeFS) Mount(_ context.Context, source, target, _ string, _ []string) error {
	f.mounted = append(f.mounted, source+"->"+target)
	f.isMounted = true
	return nil
}

func (f *fakeFS) Unmount(_ context.Context, target string) error {
	f.unmounted = append(f.unmounted, target)
	f.isMounted = false
	return nil
}

func (f *fakeFS) IsMountPoint(context.Context, string) (bool, error) { return f.isMounted, nil }

type harness struct {
	asm      *Assembler
	fs       *fakeFS
	spec     Spec
	exportsD string
	commands []string
}

func newHarness(t *testing.T, blank bool) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		fs:       &fakeFS{},
		exportsD: filepath.Join(root, "exports.d"),
	}
	h.spec = Spec{
		VolumeUUID: testUUID,
		Path:       filepath.Join(root, "mnt", "team-a-shared-3c81"),
		FSID:       testFSID,
		Clients:    []string{"192.168.10.0/24"},
	}
	devices := &fakeDevices{device: nvme.Device{
		Namespace: nvme.Namespace{DevicePath: testDev, NGUID: testNGUID},
	}}
	asm, err := New(Config{
		Devices:    devices,
		Filesystem: h.fs,
		Blank: func(context.Context, string) (bool, error) {
			return blank, nil
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, int, error) {
			h.commands = append(h.commands, name+" "+strings.Join(args, " "))
			return nil, 0, nil
		},
		ExportsDir: h.exportsD,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.asm = asm
	return h
}

func (h *harness) dropIn() string {
	return filepath.Join(h.exportsD, h.spec.dropInName())
}

// The happy path: format, mount, publish, in that order.
func TestCreateAssemblesInOrder(t *testing.T) {
	h := newHarness(t, true)

	if err := h.asm.Create(context.Background(), h.spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(h.fs.formatted) != 1 || h.fs.formatted[0] != testDev {
		t.Errorf("formatted = %v, want one mkfs on %s", h.fs.formatted, testDev)
	}
	if len(h.fs.mounted) != 1 {
		t.Errorf("mounted = %v, want one mount", h.fs.mounted)
	}
	body, err := os.ReadFile(h.dropIn())
	if err != nil {
		t.Fatalf("reading the drop-in: %v", err)
	}
	for _, want := range []string{"pnfs", "fsid=" + testFSID, "192.168.10.0/24", h.spec.Path} {
		if !strings.Contains(string(body), want) {
			t.Errorf("drop-in is missing %q:\n%s", want, body)
		}
	}
	if len(h.commands) != 1 || !strings.HasPrefix(h.commands[0], "exportfs -ra") {
		t.Errorf("commands = %v, want one exportfs -ra", h.commands)
	}
}

// The one that matters. A reconciler calls Create on every pass, so a device
// that already carries a filesystem must never be formatted again: doing so
// destroys exactly the data the export exists to serve.
func TestCreateNeverFormatsANonBlankDevice(t *testing.T) {
	h := newHarness(t, false)

	if err := h.asm.Create(context.Background(), h.spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(h.fs.formatted) != 0 {
		t.Fatalf("formatted a device that was not blank: %v", h.fs.formatted)
	}
	if len(h.fs.mounted) != 1 {
		t.Errorf("mounted = %v, want it still mounted", h.fs.mounted)
	}
}

// Create is re-entrant: a second pass over a finished export changes nothing
// on the host and still republishes, because the drop-in is the record and
// rewriting it is how a changed client set takes effect.
func TestCreateIsIdempotent(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	if err := h.asm.Create(ctx, h.spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// The device now carries a filesystem, which is what a second pass sees.
	h.asm.cfg.Blank = func(context.Context, string) (bool, error) { return false, nil }
	if err := h.asm.Create(ctx, h.spec); err != nil {
		t.Fatalf("second Create: %v", err)
	}

	if len(h.fs.formatted) != 1 {
		t.Errorf("formatted %d times, want exactly 1", len(h.fs.formatted))
	}
	if len(h.fs.mounted) != 1 {
		t.Errorf("mounted %d times, want exactly 1", len(h.fs.mounted))
	}
}

// A partial assembly resumes. The mount landed but the drop-in never got
// written, which is what a reconcile dying between the two leaves behind.
func TestCreateResumesAfterAPartialAssembly(t *testing.T) {
	h := newHarness(t, false)
	h.fs.isMounted = true

	if err := h.asm.Create(context.Background(), h.spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(h.fs.mounted) != 0 {
		t.Errorf("remounted an existing mount: %v", h.fs.mounted)
	}
	if _, err := os.Stat(h.dropIn()); err != nil {
		t.Errorf("the drop-in was not written on the resuming pass: %v", err)
	}
}

// Teardown unpublishes before it unmounts. The other order takes the filesystem
// away from clients that still hold it.
func TestDeleteUnpublishesBeforeUnmounting(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	if err := h.asm.Create(ctx, h.spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	h.commands = nil

	if err := h.asm.Delete(ctx, h.spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(h.dropIn()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drop-in survived teardown: %v", err)
	}
	if len(h.commands) != 1 || !strings.HasPrefix(h.commands[0], "exportfs -ra") {
		t.Errorf("commands = %v, want one exportfs -ra", h.commands)
	}
	if len(h.fs.unmounted) != 1 {
		t.Errorf("unmounted = %v, want one", h.fs.unmounted)
	}
}

// Delete converges on a host that has already lost the export. The caller is a
// finalizer, so failing here would hold the record forever.
func TestDeleteToleratesAnAlreadyGoneExport(t *testing.T) {
	h := newHarness(t, true)

	if err := h.asm.Delete(context.Background(), h.spec); err != nil {
		t.Fatalf("Delete on a host with nothing to remove: %v", err)
	}
	if len(h.fs.unmounted) != 0 {
		t.Errorf("unmounted something that was not mounted: %v", h.fs.unmounted)
	}
}

// An empty client set is refused rather than widened to everyone, which is what
// defaulting it would mean.
func TestCreateRefusesAnEmptyClientSet(t *testing.T) {
	h := newHarness(t, true)
	h.spec.Clients = nil

	err := h.asm.Create(context.Background(), h.spec)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("err = %v, want ErrInvalidSpec", err)
	}
	if len(h.fs.formatted) != 0 || len(h.fs.mounted) != 0 {
		t.Error("touched the host despite refusing the spec")
	}
}

// A failed mkfs stops the assembly rather than publishing an export over a
// filesystem that was never made.
func TestCreateStopsWhenFormatFails(t *testing.T) {
	h := newHarness(t, true)
	h.fs.formatErr = errors.New("mkfs.xfs: device busy")

	if err := h.asm.Create(context.Background(), h.spec); err == nil {
		t.Fatal("a failed mkfs returned no error")
	}
	if len(h.fs.mounted) != 0 {
		t.Errorf("mounted after a failed mkfs: %v", h.fs.mounted)
	}
	if _, err := os.Stat(h.dropIn()); !errors.Is(err, os.ErrNotExist) {
		t.Error("published an export after a failed mkfs")
	}
}

// Every export carries the option that makes the whole feature work. Losing it
// would leave a working NFS export serving every byte through the metadata
// server, correct and silently slow.
func TestExportLineCarriesPNFS(t *testing.T) {
	line := Spec{Path: "/mnt/x", FSID: testFSID, Clients: []string{"*"}}.line()
	if !strings.Contains(line, ",pnfs,") && !strings.Contains(line, "(pnfs,") {
		t.Errorf("the export line does not carry pnfs: %s", line)
	}
	if !strings.Contains(line, "fsid="+testFSID) {
		t.Errorf("the export line does not carry the fsid: %s", line)
	}
}

// attachRecorder stands in for the consumer that makes the namespace present.
type attachRecorder struct {
	attached []Spec
	detached []Spec
	err      error
	// present is what the device resolver reports: false until Attach runs,
	// which is the whole ordering this exercises.
	present *bool
}

func (a *attachRecorder) attach(_ context.Context, spec Spec) error {
	if a.err != nil {
		return a.err
	}
	a.attached = append(a.attached, spec)
	if a.present != nil {
		*a.present = true
	}
	return nil
}

func (a *attachRecorder) detach(_ context.Context, spec Spec) error {
	a.detached = append(a.detached, spec)
	return nil
}

// newAttachHarness is newHarness with a device that is not there until Attach
// puts it there.
func newAttachHarness(t *testing.T) (*harness, *attachRecorder) {
	t.Helper()
	root := t.TempDir()
	h := &harness{fs: &fakeFS{}, exportsD: filepath.Join(root, "exports.d")}
	h.spec = Spec{
		VolumeUUID: testUUID,
		ClusterID:  "f0bb9077-78c4-4482-9ccf-a5693ce2df78",
		PoolID:     "9d016dd4-34d7-42f0-b549-52a5af2f1399",
		Path:       filepath.Join(root, "mnt", "team-a-shared-3c81"),
		FSID:       testFSID,
		Clients:    []string{"192.168.10.0/24"},
	}
	present := false
	rec := &attachRecorder{present: &present}
	asm, err := New(Config{
		Devices:    &presenceDevices{present: &present},
		Filesystem: h.fs,
		Blank:      func(context.Context, string) (bool, error) { return true, nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, int, error) {
			h.commands = append(h.commands, name+" "+strings.Join(args, " "))
			return nil, 0, nil
		},
		ExportsDir: h.exportsD,
		Attach:     rec.attach,
		Detach:     rec.detach,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.asm = asm
	return h, rec
}

// presenceDevices reports the namespace only once something has attached it.
type presenceDevices struct {
	nvme.DeviceResolver
	present *bool
}

func (d *presenceDevices) ByUUID(_ context.Context, _ string) (nvme.Device, error) {
	if !*d.present {
		return nvme.Device{}, errs.ErrNotFound
	}
	return nvme.Device{Namespace: nvme.Namespace{DevicePath: testDev, NGUID: testNGUID}}, nil
}

// The namespace has to be attached before anything looks for it. The MDS host
// is an NVMe-oF initiator for the volume exactly like a client is, and nothing
// else on that host has a reason to connect it: no CSI call targets the metadata
// server, so if assembly does not attach it, the device is never there.
func TestCreateAttachesBeforeLookingForTheDevice(t *testing.T) {
	h, rec := newAttachHarness(t)

	if err := h.asm.Create(context.Background(), h.spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(rec.attached) != 1 {
		t.Fatalf("Attach ran %d times, want once", len(rec.attached))
	}
	if got := rec.attached[0]; got.VolumeUUID != testUUID || got.ClusterID == "" || got.PoolID == "" {
		t.Errorf("Attach got %+v, want the volume identified to the control plane", got)
	}
	if len(h.fs.mounted) != 1 {
		t.Errorf("mounted = %v, want the export assembled on the attached device", h.fs.mounted)
	}
}

// An attach that fails stops the assembly rather than falling through to a
// device lookup that can only report not-found and blame the wrong thing.
func TestCreateStopsWhenAttachFails(t *testing.T) {
	h, rec := newAttachHarness(t)
	rec.err = errors.New("the control plane refused the host")

	err := h.asm.Create(context.Background(), h.spec)
	if err == nil {
		t.Fatal("Create succeeded with no namespace attached")
	}
	if !strings.Contains(err.Error(), "refused the host") {
		t.Errorf("error = %v, want it to carry the attach failure", err)
	}
	if len(h.fs.formatted) != 0 || len(h.fs.mounted) != 0 {
		t.Error("the assembly continued past a failed attach")
	}
}

// Teardown detaches, and only after the filesystem is unmounted: detaching a
// mounted device leaves the host with a mount over a namespace that is gone.
func TestDeleteDetachesAfterUnmounting(t *testing.T) {
	h, rec := newAttachHarness(t)
	if err := h.asm.Create(context.Background(), h.spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := h.asm.Delete(context.Background(), h.spec); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(rec.detached) != 1 {
		t.Fatalf("Detach ran %d times, want once", len(rec.detached))
	}
	if len(h.fs.unmounted) != 1 {
		t.Fatalf("unmounted = %v, want one unmount", h.fs.unmounted)
	}
}

// A configuration with no Attach is still valid, and assembles against a device
// something else put there. That is how the package behaved before attaching
// existed, and a host that manages its own fabric should not have to grow one.
func TestAttachIsOptional(t *testing.T) {
	h := newHarness(t, true)
	if err := h.asm.Create(context.Background(), h.spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(h.fs.mounted) != 1 {
		t.Errorf("mounted = %v, want one mount", h.fs.mounted)
	}
}

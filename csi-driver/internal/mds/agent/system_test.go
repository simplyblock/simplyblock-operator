// Tests for the guest's system adapters: the state volume's format-or-mount
// decision (U-22), nfsd's key parameter, systemd, the host identifier, and the
// connector's use of the host NQN.

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
)

const subsysA = "nqn:lvol:a"

type fakeProber struct {
	reading blockdev.Reading
	err     error
}

func (f fakeProber) Read(context.Context, blockdev.Device) (blockdev.Reading, error) {
	return f.reading, f.err
}

type fakeFS struct {
	calls   []string
	mounted bool
}

func (f *fakeFS) Format(_ context.Context, device, fsType string, _ []string) error {
	f.calls = append(f.calls, "format "+fsType+" "+device)
	return nil
}
func (f *fakeFS) Mount(_ context.Context, source, target, fsType string, _ []string) error {
	f.calls = append(f.calls, "mount "+fsType+" "+source+" "+target)
	return nil
}
func (f *fakeFS) Unmount(_ context.Context, target string) error {
	f.calls = append(f.calls, "unmount "+target)
	return nil
}
func (f *fakeFS) IsMountPoint(context.Context, string) (bool, error) { return f.mounted, nil }

func mountState(reading blockdev.Reading, mounted bool) (*fakeFS, error) {
	fs := &fakeFS{mounted: mounted}
	m := FSMounter{Prober: fakeProber{reading: reading}, FS: fs}
	return fs, m.Mount(context.Background(), blockdev.Device{Path: devOne}, "/var/lib/nfs")
}

func TestStateMounterU22FormatsExt4OnceWhenTheDeviceIsBlank(t *testing.T) {
	fs, err := mountState(blockdev.Reading{Content: blockdev.ContentBlank}, false)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	want := []string{"format ext4 /dev/nvme1n1", "mount ext4 /dev/nvme1n1 /var/lib/nfs"}
	if !slices.Equal(fs.calls, want) {
		t.Errorf("calls = %q, want %q", fs.calls, want)
	}
}

func TestStateMounterU22MountsAnExt4DeviceWithoutFormattingIt(t *testing.T) {
	fs, err := mountState(blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, false)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if want := []string{"mount ext4 /dev/nvme1n1 /var/lib/nfs"}; !slices.Equal(fs.calls, want) {
		t.Errorf("calls = %q, want %q", fs.calls, want)
	}
}

func TestStateMounterNeverFormatsADeviceThatCarriesSomethingElse(t *testing.T) {
	for _, r := range []blockdev.Reading{
		{Content: blockdev.ContentFilesystem, Type: "xfs"},
		{Content: blockdev.ContentForeign},
		{Content: blockdev.ContentUnknown},
	} {
		fs, err := mountState(r, false)
		if err == nil {
			t.Errorf("%v: Mount passed", r)
		}
		if len(fs.calls) != 0 {
			t.Errorf("%v: calls = %q, want none", r, fs.calls)
		}
	}
	m := FSMounter{Prober: fakeProber{err: errors.New("read failed")}, FS: &fakeFS{}}
	if err := m.Mount(context.Background(), blockdev.Device{}, "/var/lib/nfs"); err == nil {
		t.Error("an unreadable device was mounted or formatted")
	}
}

func TestStateMounterAMountedDirectoryIsLeftAlone(t *testing.T) {
	fs, err := mountState(blockdev.Reading{Content: blockdev.ContentBlank}, true)
	if err != nil || len(fs.calls) != 0 {
		t.Errorf("Mount = %v with calls %q, want a no-op", err, fs.calls)
	}
}

func TestStateMounterUnmounts(t *testing.T) {
	fs := &fakeFS{}
	if err := (FSMounter{FS: fs}).Unmount(context.Background(), "/var/lib/nfs"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"unmount /var/lib/nfs"}; !slices.Equal(fs.calls, want) {
		t.Errorf("calls = %q, want %q", fs.calls, want)
	}
}

func TestParamKeyWriterLoadsNFSDThenWritesTheKeyInDecimal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pnfs_mds_pr_key")
	var order []string
	w := ParamKeyWriter{Path: path, LoadModule: func(context.Context) error {
		order = append(order, "load")
		return os.WriteFile(path, nil, 0o600)
	}}
	if err := w.Write(context.Background(), 0x0100000000000007); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := os.ReadFile(path)
	if strings.TrimSpace(string(got)) != "72057594037927943" {
		t.Errorf("parameter = %q, want the key in decimal", got)
	}
	if !slices.Equal(order, []string{"load"}) {
		t.Errorf("order = %v, want the module loaded first", order)
	}
}

func TestParamKeyWriterFailsWhenTheModuleDoesNotLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pnfs_mds_pr_key")
	w := ParamKeyWriter{Path: path, LoadModule: func(context.Context) error { return errors.New("modprobe: not found") }}
	if err := w.Write(context.Background(), 1); err == nil {
		t.Error("Write passed with the module missing")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the parameter was written anyway")
	}
}

func TestSystemctlRunsTheUnitCommands(t *testing.T) {
	var ran []string
	s := Systemctl{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	ctx := context.Background()
	for _, err := range []error{
		s.Start(ctx, "nfs-server.service"), s.Stop(ctx, "nfs-server.service"),
		s.CreateTmpfiles(ctx, "/usr/lib/tmpfiles.d/pnfs.conf"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"systemctl start nfs-server.service",
		"systemctl stop nfs-server.service",
		"systemd-tmpfiles --create /usr/lib/tmpfiles.d/pnfs.conf",
	}
	if !slices.Equal(ran, want) {
		t.Errorf("ran %q, want %q", ran, want)
	}
	failing := Systemctl{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("unit not found"), errors.New("exit status 5")
	}}
	if err := failing.Start(ctx, "x.service"); err == nil || !strings.Contains(err.Error(), "unit not found") {
		t.Errorf("Start = %v, want the command's output in the error", err)
	}
}

type recordingNVMeConnector struct {
	nvmeof.Connector
	targets []nvmeof.Target
}

func (c *recordingNVMeConnector) ConnectPaths(_ context.Context, ts []nvmeof.Target) ([]nvmeof.PathResult, error) {
	c.targets = ts
	return nil, nil
}

type oneDevice struct {
	nvme.DeviceResolver
	dev nvme.Device
}

func (o oneDevice) ListWithSelector(context.Context, nvme.DeviceSelector) ([]nvme.Device, error) {
	return []nvme.Device{o.dev}, nil
}

func TestFabricConnectorConnectsAsTheHostNQNAndReturnsTheDevice(t *testing.T) {
	node := filepath.Join(t.TempDir(), "nvme1n1")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recordingNVMeConnector{}
	c := FabricConnector{Connector: rec, Devices: oneDevice{dev: nvme.Device{
		Namespace: nvme.Namespace{
			ID: 1, Name: "nvme1n1", DevicePath: node, Dev: "259:1", LogicalBlockSize: 512, Capacity: 1 << 21,
			Paths: []nvme.Path{{Controller: "nvme1", NSID: 1, ANAState: nvme.ANAOptimized}},
		},
		Subsystem: nvme.Subsystem{NQN: subsysA},
	}}}
	conn := lvol.Connection{NQN: subsysA, NSID: 1, Endpoints: []lvol.Endpoint{
		{Address: "10.0.0.1", Port: 4420, Transport: "tcp"},
	}}

	dev, err := c.Connect(context.Background(), conn, podHostNQN)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if dev.Path != node {
		t.Errorf("device = %q, want %q", dev.Path, node)
	}
	if len(rec.targets) != 1 || rec.targets[0].HostNQN != podHostNQN {
		t.Errorf("targets = %+v, want one presenting the pod's host NQN", rec.targets)
	}
}

// The image gates nfs-server, nfs-mountd, and nfsdcld on a file so that nothing
// starts nfsd at boot, and a standby that is promoted lets them start by
// creating it. Revoking it again must work when the file is already gone.
func TestSystemctlAllowsAndRevokesTheNFSDUnitsThroughTheGateFile(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "run", "mds", "active")
	s := Systemctl{GateFile: gate}
	ctx := context.Background()

	if err := s.AllowNFSD(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gate); err != nil {
		t.Fatalf("the gate file does not exist after AllowNFSD: %v", err)
	}
	if err := s.RevokeNFSD(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gate); err == nil {
		t.Error("the gate file still exists after RevokeNFSD")
	}
	if err := s.RevokeNFSD(ctx); err != nil {
		t.Errorf("a second RevokeNFSD: %v", err)
	}
}

package storagerpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"
)

// fakeSubsystems is a nvme.SubsystemResolver over a fixed snapshot.
type fakeSubsystems struct {
	subsystems []nvme.Subsystem
	err        error
}

func (f *fakeSubsystems) List(context.Context) ([]nvme.Subsystem, error) {
	return f.subsystems, f.err
}

func (f *fakeSubsystems) ByNQN(_ context.Context, nqn string) (nvme.Subsystem, error) {
	if f.err != nil {
		return nvme.Subsystem{}, f.err
	}
	for _, s := range f.subsystems {
		if s.NQN == nqn {
			return s, nil
		}
	}
	return nvme.Subsystem{}, fmt.Errorf("subsystem %s: %w", nqn, errs.ErrNotFound)
}

// fakeDevices is a nvme.DeviceResolver over a fixed snapshot. It answers the
// way the sysfs resolver does, so what the tests pin is the wire layer rather
// than a reimplementation of the lookup rules.
type fakeDevices struct {
	devices []nvme.Device
	err     error
}

func (f *fakeDevices) List(context.Context) ([]nvme.Device, error) { return f.devices, f.err }

func (f *fakeDevices) ListWithSelector(ctx context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	all, err := f.List(ctx)
	if err != nil {
		return nil, err
	}
	return sel.Filter(all), nil
}

func (f *fakeDevices) ByUUID(ctx context.Context, uuid string) (nvme.Device, error) {
	return f.first(ctx, nvme.DeviceSelector{UUID: uuid}, "uuid "+uuid)
}

func (f *fakeDevices) ByDevicePath(ctx context.Context, devicePath string) (nvme.Device, error) {
	return f.first(ctx, nvme.DeviceSelector{DevicePath: devicePath}, devicePath)
}

func (f *fakeDevices) ByNamespace(ctx context.Context, nqn string, nsid nvme.NamespaceID) (nvme.Device, error) {
	return f.first(ctx, nvme.DeviceSelector{NQN: nqn, NSID: nsid}, fmt.Sprintf("%s nsid %d", nqn, nsid))
}

func (f *fakeDevices) first(ctx context.Context, sel nvme.DeviceSelector, what string) (nvme.Device, error) {
	matched, err := f.ListWithSelector(ctx, sel)
	if err != nil {
		return nvme.Device{}, err
	}
	if len(matched) == 0 {
		return nvme.Device{}, fmt.Errorf("device %s: %w", what, errs.ErrNotFound)
	}
	return matched[0], nil
}

// serve starts the node services on loopback and returns a connection to them.
// It is the plain-gRPC path: the link is exercised separately, in
// TestResolvesOverALink, so a failure here is the wire layer and not the
// transport under it.
func serve(t *testing.T, subs nvme.SubsystemResolver, devs nvme.DeviceResolver) *grpc.ClientConn {
	t.Helper()

	server, err := NewServer(storage.Accessor{SubsystemResolver: subs, DeviceResolver: devs})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	server.Register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// assertSameDevice compares a decoded device against the fixture it came from.
// Now that a device carries no resolver handle it is a plain value, so this can
// be an equality check rather than a field-by-field one.
func assertSameDevice(t *testing.T, got, want nvme.Device) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("device did not survive the round trip:\n got %+v\nwant %+v", got, want)
	}
}

func TestDeviceResolverList(t *testing.T) {
	want := fullDevice()
	devs := NewDeviceResolver(serve(t, &fakeSubsystems{}, &fakeDevices{devices: []nvme.Device{want}}))

	got, err := devs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d devices, want 1", len(got))
	}
	assertSameDevice(t, got[0], want)

	// The whole snapshot crosses, not a summary: the controller tunables and the
	// per-path ANA view are what later decisions are made from.
	if n := len(got[0].Subsystem.Controllers); n != 1 {
		t.Fatalf("controllers = %d, want 1", n)
	}
	if state := got[0].Namespace.Paths[0].ANAState; state != nvme.ANAOptimized {
		t.Errorf("ana state = %q, want optimized", state)
	}
	if tmo := got[0].Subsystem.Controllers[0].CtrlLossTMOSec; tmo != 600 {
		t.Errorf("ctrl_loss_tmo = %d, want 600", tmo)
	}
}

func TestDeviceResolverLookups(t *testing.T) {
	want := fullDevice()
	devs := NewDeviceResolver(serve(t, &fakeSubsystems{}, &fakeDevices{devices: []nvme.Device{want}}))
	ctx := context.Background()

	t.Run("ByUUID", func(t *testing.T) {
		got, err := devs.ByUUID(ctx, want.Namespace.UUID)
		if err != nil {
			t.Fatalf("ByUUID: %v", err)
		}
		assertSameDevice(t, got, want)
	})

	t.Run("ByDevicePath", func(t *testing.T) {
		got, err := devs.ByDevicePath(ctx, want.Namespace.DevicePath)
		if err != nil {
			t.Fatalf("ByDevicePath: %v", err)
		}
		assertSameDevice(t, got, want)
	})

	t.Run("ByNamespace", func(t *testing.T) {
		got, err := devs.ByNamespace(ctx, want.Subsystem.NQN, want.Namespace.ID)
		if err != nil {
			t.Fatalf("ByNamespace: %v", err)
		}
		assertSameDevice(t, got, want)
	})

	t.Run("ListWithSelector", func(t *testing.T) {
		got, err := devs.ListWithSelector(ctx, nvme.DeviceSelector{UUID: want.Namespace.UUID})
		if err != nil {
			t.Fatalf("ListWithSelector: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("returned %d devices, want 1", len(got))
		}
		assertSameDevice(t, got[0], want)
	})
}

// A selector nothing matches is an empty result, not an error — the contract
// nvme.DeviceResolver states, and the one nvmeof.WaitForDevice polls against.
func TestSelectorWithNoMatchIsNotAnError(t *testing.T) {
	devs := NewDeviceResolver(serve(t, &fakeSubsystems{}, &fakeDevices{devices: []nvme.Device{fullDevice()}}))

	got, err := devs.ListWithSelector(context.Background(), nvme.DeviceSelector{UUID: "no-such-uuid"})
	if err != nil {
		t.Fatalf("ListWithSelector: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("returned %d devices, want none", len(got))
	}
}

// The sentinel has to survive the link, or every caller's errors.Is check
// quietly stops working the moment the resolver is remote.
func TestNotFoundCrossesTheWire(t *testing.T) {
	server := serve(t, &fakeSubsystems{}, &fakeDevices{})
	ctx := context.Background()

	t.Run("device", func(t *testing.T) {
		_, err := NewDeviceResolver(server).ByUUID(ctx, "absent")
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("err = %v, want errs.ErrNotFound", err)
		}
		if c := class.Of(err); c.Code != codes.NotFound {
			t.Errorf("code = %v, want NotFound", c.Code)
		}
	})

	t.Run("subsystem", func(t *testing.T) {
		_, err := NewSubsystemResolver(server).ByNQN(ctx, "nqn.absent")
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("err = %v, want errs.ErrNotFound", err)
		}
	})
}

// An unrecognised failure must not come back as retryable: a caller that
// requeues forever on a permanent fault is worse than one that reports it.
func TestUnknownErrorStaysPermanent(t *testing.T) {
	devs := NewDeviceResolver(serve(t, &fakeSubsystems{}, &fakeDevices{err: errors.New("sysfs exploded")}))

	_, err := devs.List(context.Background())
	if err == nil {
		t.Fatal("List succeeded against a broken resolver")
	}
	if c := class.Of(err); c.Retryable {
		t.Errorf("err classified as retryable: %v", err)
	}
}

// The re-scanning questions must work against a remote node: storage.Accessor does
// the scan, the remote resolver answers it, and the pure filters in nvme do the
// rest — the same three steps as locally.
func TestRescanningQuestionsWorkRemotely(t *testing.T) {
	primary := fullDevice()

	// Same volume seen over a second path: same namespace UUID, its own sysfs
	// entry. This is what a node looks like with native multipath disabled.
	sibling := fullDevice()
	sibling.Namespace.SysfsPath = "/sys/class/nvme/nvme2/nvme2n3"
	sibling.Namespace.DevicePath = "/dev/nvme2n3"

	// A different volume on the same subsystem — a namespaced lvol co-tenant.
	coTenant := fullDevice()
	coTenant.Namespace.ID = 4
	coTenant.Namespace.UUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	coTenant.Namespace.SysfsPath = "/sys/class/nvme-subsystem/nvme-subsys0/nvme0n4"

	storage := Remote(serve(t, &fakeSubsystems{}, &fakeDevices{
		devices: []nvme.Device{primary, sibling, coTenant},
	}))
	ctx := context.Background()

	device, err := storage.DeviceResolver.ByDevicePath(ctx, primary.Namespace.DevicePath)
	if err != nil {
		t.Fatalf("ByDevicePath: %v", err)
	}

	siblings, err := storage.Siblings(ctx, device)
	if err != nil {
		t.Fatalf("Siblings against a remote node: %v", err)
	}
	if len(siblings) != 1 {
		t.Fatalf("Siblings returned %d devices, want 1", len(siblings))
	}
	if siblings[0].Namespace.DevicePath != sibling.Namespace.DevicePath {
		t.Errorf("sibling = %s, want %s", siblings[0].Namespace.DevicePath, sibling.Namespace.DevicePath)
	}

	coTenants, err := storage.CoTenants(ctx, device)
	if err != nil {
		t.Fatalf("CoTenants against a remote node: %v", err)
	}
	if len(coTenants) != 1 {
		t.Fatalf("CoTenants returned %d devices, want 1", len(coTenants))
	}
	if coTenants[0].Namespace.ID != coTenant.Namespace.ID {
		t.Errorf("co-tenant nsid = %d, want %d", coTenants[0].Namespace.ID, coTenant.Namespace.ID)
	}
}

// Accessible is derived from the fields that crossed, so it must answer the
// same on the operator as it would on the node.
func TestAccessibilitySurvivesTheWire(t *testing.T) {
	usable := fullDevice()

	// A stale head the kernel has not reaped: present in sysfs, no path that can
	// carry I/O, no live controller behind it.
	stale := fullDevice()
	stale.Namespace.DevicePath = "/dev/nvme9n1"
	stale.Namespace.SysfsPath = "/sys/class/nvme-subsystem/nvme-subsys9/nvme9n1"
	stale.Namespace.Paths = []nvme.Path{{Controller: "nvme9", ANAState: nvme.ANAInaccessible}}
	stale.Subsystem.Controllers = []nvme.Controller{{ID: "nvme9", State: "connecting"}}

	devs := NewDeviceResolver(serve(t, &fakeSubsystems{}, &fakeDevices{
		devices: []nvme.Device{usable, stale},
	}))

	got, err := devs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !got[0].Accessible() {
		t.Error("a device with an optimized path decoded as inaccessible")
	}
	if got[1].Accessible() {
		t.Error("a stale device with no usable path decoded as accessible")
	}
}

func TestSubsystemResolver(t *testing.T) {
	want := fullDevice().Subsystem
	subs := NewSubsystemResolver(serve(t, &fakeSubsystems{subsystems: []nvme.Subsystem{want}}, &fakeDevices{}))
	ctx := context.Background()

	listed, err := subs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].NQN != want.NQN {
		t.Fatalf("List = %+v, want one subsystem with nqn %s", listed, want.NQN)
	}

	got, err := subs.ByNQN(ctx, want.NQN)
	if err != nil {
		t.Fatalf("ByNQN: %v", err)
	}
	if len(got.Controllers) != 1 || got.Controllers[0].Address.TrAddr != want.Controllers[0].Address.TrAddr {
		t.Errorf("controllers = %+v, want the full path list", got.Controllers)
	}
	if len(got.Namespaces) != 1 {
		t.Errorf("namespaces = %d, want 1", len(got.Namespaces))
	}
}

// TestResolvesOverALink is the whole stack: a node peer dials the operator,
// registers these services on its side of the session, and the operator
// resolves a device over the connection the node opened.
func TestResolvesOverALink(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hub, err := link.NewHub(link.HubConfig{
		Listener: lis,
		Auth:     link.InsecureStaticAuthenticator{Token: "test-token"},
		Logger:   quiet,
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Serve(ctx) }()

	want := fullDevice()
	server, err := NewServer(storage.Accessor{SubsystemResolver: &fakeSubsystems{}, DeviceResolver: &fakeDevices{devices: []nvme.Device{want}}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	agent, err := link.NewAgent(link.AgentConfig{
		Dial:         link.InsecureDialer(hub.Addr().String()),
		ID:           link.NodePeer("worker-1"),
		Token:        link.StaticToken("test-token"),
		Register:     server.Register,
		Capabilities: Capabilities(),
		Logger:       quiet,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	go func() { _ = agent.Run(ctx) }()

	var peer *link.Peer
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p, err := hub.Registry().Peer(link.NodePeer("worker-1")); err == nil {
			peer = p
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if peer == nil {
		t.Fatal("timed out waiting for the node to link")
	}
	if !peer.HasCapability(CapabilityDevices) {
		t.Errorf("peer capabilities = %v, want to contain %s", peer.Capabilities, CapabilityDevices)
	}

	storage := Remote(peer.Conn())
	got, err := storage.DeviceResolver.ByUUID(ctx, want.Namespace.UUID)
	if err != nil {
		t.Fatalf("ByUUID over the link: %v", err)
	}
	assertSameDevice(t, got, want)

	// And the re-scan works too, which means it went back down the same link.
	if _, err := storage.Siblings(ctx, got); err != nil {
		t.Errorf("Siblings over the link: %v", err)
	}
}

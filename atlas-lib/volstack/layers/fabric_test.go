// What the fabric layer has to guarantee.
//
// Two of these are the reason the layer exists rather than the connect being
// inline. Release is a detach and not a disconnect, because disconnecting a
// subsystem tears down every namespace on it and a simplyblock subsystem can
// hold several volumes. And Observe distinguishes a device that is present from
// one that can serve, because the second is what a stack above it depends on.

package layers

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
)

const (
	testNQN    = "nqn.2023-04.io.simplyblock:cluster:lvol:aaaaaaaa"
	testHandle = "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333"
)

// fakeConnector records what the layer asked the fabric to do.
type fakeConnector struct {
	connected     [][]nvmeof.Target
	disconnected  []string
	connectErr    error
	disconnectErr error
}

func (f *fakeConnector) Connect(context.Context, nvmeof.Target) error { return f.connectErr }

func (f *fakeConnector) ConnectPaths(_ context.Context, ts []nvmeof.Target) ([]nvmeof.PathResult, error) {
	f.connected = append(f.connected, ts)
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	out := make([]nvmeof.PathResult, 0, len(ts))
	for _, t := range ts {
		out = append(out, nvmeof.PathResult{Target: t})
	}
	return out, nil
}

func (f *fakeConnector) Disconnect(_ context.Context, nqn string) error {
	f.disconnected = append(f.disconnected, nqn)
	return f.disconnectErr
}

func (f *fakeConnector) DisconnectController(context.Context, nvme.Controller) error { return nil }

func (f *fakeConnector) IsConnected(context.Context, string) (bool, error) { return false, nil }

// fakeDevices answers device lookups from a fixed list.
type fakeDevices struct {
	devices []nvme.Device
	err     error
}

func (f *fakeDevices) List(context.Context) ([]nvme.Device, error) { return f.devices, f.err }

func (f *fakeDevices) ListWithSelector(_ context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []nvme.Device
	for _, d := range f.devices {
		if sel.Matches(d) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeDevices) ByUUID(context.Context, string) (nvme.Device, error) {
	return nvme.Device{}, errors.New("not used")
}

func (f *fakeDevices) ByDevicePath(context.Context, string) (nvme.Device, error) {
	return nvme.Device{}, errors.New("not used")
}

func (f *fakeDevices) ByNamespace(context.Context, string, nvme.NamespaceID) (nvme.Device, error) {
	return nvme.Device{}, errors.New("not used")
}

// device builds an attached namespace as a resolver would report it.
//
// Serviceability is expressed through the ANA state of the namespace's paths,
// which is what nvme.Device.Accessible judges a namespace with an ANA view on:
// an inaccessible path is a device that is present in sysfs and cannot take I/O.
func device(name string, serving bool) nvme.Device {
	ana := nvme.ANAInaccessible
	if serving {
		ana = nvme.ANAOptimized
	}
	return nvme.Device{
		Namespace: nvme.Namespace{
			ID: 1, Name: name, DevicePath: "/dev/" + name, Dev: "259:1",
			LogicalBlockSize: 512, Capacity: 1 << 21,
			Paths: []nvme.Path{{Controller: "nvme0", NSID: 1, ANAState: ana}},
		},
		Subsystem: nvme.Subsystem{NQN: testNQN},
	}
}

func testConnection() lvol.Connection {
	return lvol.Connection{
		NQN:  testNQN,
		NSID: 1,
		Endpoints: []lvol.Endpoint{
			{Address: "10.0.0.1", Port: 4420, Transport: "tcp"},
			{Address: "10.0.0.2", Port: 4420, Transport: "tcp"},
		},
	}
}

func newFabric(t *testing.T, conn *fakeConnector, devs *fakeDevices) *Fabric {
	t.Helper()
	return NewFabric(FabricConfig{
		Connection: testConnection(),
		Connector:  conn,
		Devices:    devs,
	})
}

// A namespace that is attached and serving is ready, and its device is what the
// layer hands upward.
func TestFabricObserveReady(t *testing.T) {
	f := newFabric(t, &fakeConnector{}, &fakeDevices{devices: []nvme.Device{device("nvme0n1", true)}})

	state, _, err := f.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Fatalf("state = %s, want Ready", state)
	}
}

// No namespace at all is absent, which is the one state that permits Ensure to
// connect.
func TestFabricObserveAbsent(t *testing.T) {
	f := newFabric(t, &fakeConnector{}, &fakeDevices{})

	state, _, err := f.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Fatalf("state = %s, want Absent", state)
	}
}

// A device that is present but cannot serve is partial, not ready. A stack above
// a device in that state would format or mount something that answers no I/O,
// which is the distinction this layer owes the layers above it.
func TestFabricObservePresentButNotServingIsPartial(t *testing.T) {
	f := newFabric(t, &fakeConnector{}, &fakeDevices{devices: []nvme.Device{device("nvme0n1", false)}})

	state, _, err := f.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state == volstack.StateReady {
		t.Fatal("a device with no live controller was reported Ready")
	}
	if state != volstack.StatePartial {
		t.Fatalf("state = %s, want Partial", state)
	}
}

// Ensure connects every endpoint the control plane published, in the order it
// published them, because connecting out of order hands I/O to the wrong node
// until the kernel has the full ANA picture.
func TestFabricEnsureConnectsEveryEndpointInOrder(t *testing.T) {
	conn := &fakeConnector{}
	f := newFabric(t, conn, &fakeDevices{devices: []nvme.Device{device("nvme0n1", true)}})

	art, err := f.Ensure(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(conn.connected) != 1 {
		t.Fatalf("ConnectPaths was called %d times, want once", len(conn.connected))
	}
	got := conn.connected[0]
	if len(got) != 2 || got[0].Address != "10.0.0.1" || got[1].Address != "10.0.0.2" {
		t.Fatalf("connected %v, want the published endpoints in their published order", got)
	}
	dev, ok := art.Device()
	if !ok || dev.Path != "/dev/nvme0n1" {
		t.Fatalf("artifact = %+v, want the namespace device", art.Devices)
	}
}

// The namespace belongs to the control plane, so this layer has nothing to
// destroy. A verb with nothing to do returns without error rather than refusing.
func TestFabricDestroyDoesNothing(t *testing.T) {
	conn := &fakeConnector{}
	f := newFabric(t, conn, &fakeDevices{devices: []nvme.Device{device("nvme0n1", true)}})

	if err := f.Destroy(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(conn.disconnected) != 0 {
		t.Fatal("Destroy tore the fabric down; the namespace is the control plane's to remove")
	}
}

// A subsystem that can hold other volumes is never torn down on one volume's
// unstage. This is the property the layer exists for: disconnecting takes every
// namespace on the subsystem with it, and on a namespaced pool those are other
// tenants' volumes on the same node.
func TestFabricReleaseLeavesASharedSubsystemConnected(t *testing.T) {
	conn := &fakeConnector{}
	// A namespace at an NSID above one is conclusive from sysfs alone: the
	// subsystem hosts more than a single namespace.
	dev := device("nvme0n1", true)
	dev.Namespace.ID = 2
	dev.Subsystem.Namespaces = []nvme.Namespace{dev.Namespace}

	f := NewFabric(FabricConfig{
		Connection: lvol.Connection{NQN: testNQN, NSID: 2, Endpoints: testConnection().Endpoints},
		Connector:  conn,
		Devices:    &fakeDevices{devices: []nvme.Device{dev}},
	})

	if err := f.Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(conn.disconnected) != 0 {
		t.Fatalf("a shared subsystem was disconnected: %v", conn.disconnected)
	}
}

// When whether the subsystem is shared cannot be determined, Release fails and
// disconnects nothing.
//
// A single namespace at NSID 1 is byte-for-byte identical in sysfs to a private
// subsystem, so the answer comes from an Identify the kernel can only give for a
// live controller. Failing there is the safe direction: an unstage that errors is
// retried, and one that guessed wrong takes a co-tenant's device away.
func TestFabricReleaseFailsClosedWhenSharingIsUndecidable(t *testing.T) {
	conn := &fakeConnector{}
	f := newFabric(t, conn, &fakeDevices{devices: []nvme.Device{device("nvme0n1", true)}})

	err := f.Release(context.Background(), volstack.Artifact{})
	if err == nil {
		t.Fatal("Release succeeded although it could not tell whether the subsystem is shared")
	}
	if len(conn.disconnected) != 0 {
		t.Fatalf("it disconnected anyway: %v", conn.disconnected)
	}
}

// Release has to succeed when the device is already gone, which is the normal
// case after total path loss rather than an edge case.
func TestFabricReleaseToleratesAnAbsentDevice(t *testing.T) {
	conn := &fakeConnector{}
	f := newFabric(t, conn, &fakeDevices{})

	if err := f.Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatalf("Release with no device: %v", err)
	}
	if len(conn.disconnected) != 0 {
		t.Error("Release disconnected a subsystem whose device was already gone")
	}
}

// Healthy is the read a heal dispatches on: a device that cannot serve is not
// healthy, and one that can is.
func TestFabricHealthy(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
		want bool
	}{
		{"a serving namespace", true, true},
		{"a namespace with no live controller", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFabric(t, &fakeConnector{}, &fakeDevices{devices: []nvme.Device{device("nvme0n1", tc.live)}})
			got, err := f.Healthy(context.Background(), volstack.Artifact{})
			if err != nil {
				t.Fatalf("Healthy: %v", err)
			}
			if got != tc.want {
				t.Errorf("Healthy = %v, want %v", got, tc.want)
			}
		})
	}
}

// A heal reconnects rather than recreating, because the namespace is the control
// plane's and the data behind it already exists.
func TestFabricHealReconnects(t *testing.T) {
	conn := &fakeConnector{}
	f := newFabric(t, conn, &fakeDevices{devices: []nvme.Device{device("nvme0n1", true)}})

	if err := f.Heal(context.Background(), volstack.Artifact{}, volstack.Artifact{}); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if len(conn.connected) != 1 {
		t.Fatalf("Heal issued %d connects, want one", len(conn.connected))
	}
}

// The layer records where to read its secret rather than the secret itself: the
// record outlives the pod and a credential in it would sit in cleartext on every
// node that ever staged the volume.
func TestFabricParamsCarryNoSecret(t *testing.T) {
	f := NewFabric(FabricConfig{
		Connection:   testConnection(),
		Connector:    &fakeConnector{},
		Devices:      &fakeDevices{},
		DHCHAPSecret: "s3cret-key-material",
	})

	params := f.Params()
	p, ok := params.(FabricParams)
	if !ok {
		t.Fatalf("Params returned %T", params)
	}
	if p.NQN != testNQN {
		t.Errorf("NQN = %q, want the subsystem", p.NQN)
	}
	for _, field := range []string{p.NQN, p.HostNQN} {
		if field == "s3cret-key-material" {
			t.Fatal("the recorded parameters carry the DHCHAP secret itself")
		}
	}
}

// The layer's name is in the record and a teardown after an upgrade replays a
// record an earlier version wrote, so it is stable rather than derived.
func TestFabricNameIsStable(t *testing.T) {
	f := newFabric(t, &fakeConnector{}, &fakeDevices{})
	if got := f.Name(); got != "fabric" {
		t.Errorf("Name = %q, want fabric", got)
	}
}

package nvmeof

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// recordingConnector records the NQNs it was asked to disconnect. Only
// Disconnect is exercised here.
type recordingConnector struct {
	disconnected []string
	err          error
}

func (c *recordingConnector) Connect(context.Context, Target) error { return nil }
func (c *recordingConnector) ConnectPaths(context.Context, []Target) ([]PathResult, error) {
	return nil, nil
}
func (c *recordingConnector) Disconnect(_ context.Context, nqn string) error {
	if c.err != nil {
		return c.err
	}
	c.disconnected = append(c.disconnected, nqn)
	return nil
}
func (c *recordingConnector) IsConnected(context.Context, string) (bool, error) { return true, nil }

// stubMultiNamespace substitutes the subsystem capability answer, standing in for
// the Identify Controller command the real one may issue.
func stubMultiNamespace(t *testing.T, shared bool, err error) {
	t.Helper()
	prev := isMultiNamespace
	isMultiNamespace = func(nvme.Device) (bool, error) { return shared, err }
	t.Cleanup(func() { isMultiNamespace = prev })
}

// device is one namespace of a subsystem holding the given nsids, with a live
// controller — what a scan reports for an attached volume.
func device(nsid nvme.NamespaceID, nsids ...nvme.NamespaceID) nvme.Device {
	sub := nvme.Subsystem{
		ID:          "nvme-subsys0",
		NQN:         testNQN,
		Controllers: []nvme.Controller{{ID: "nvme0", State: "live", DevicePath: "/dev/nvme0"}},
	}
	for _, id := range nsids {
		sub.Namespaces = append(sub.Namespaces, nvme.Namespace{ID: id, Name: "nvme0n1"})
	}
	var own nvme.Namespace
	for _, ns := range sub.Namespaces {
		if ns.ID == nsid {
			own = ns
		}
	}
	return nvme.Device{Namespace: own, Subsystem: sub}
}

func TestDetachDevice_DisconnectsAnExclusiveSubsystem(t *testing.T) {
	stubMultiNamespace(t, false, nil)
	c := &recordingConnector{}

	out, err := DetachDevice(context.Background(), c, device(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Disconnected || out.SharedSubsystem {
		t.Errorf("outcome = %+v, want disconnected", out)
	}
	if len(c.disconnected) != 1 || c.disconnected[0] != testNQN {
		t.Errorf("disconnected = %v, want [%s]", c.disconnected, testNQN)
	}
}

// The case this function exists for: tearing the subsystem down here would take
// the co-tenant volumes' block devices with it.
func TestDetachDevice_KeepsASharedSubsystem(t *testing.T) {
	c := &recordingConnector{}

	// Several namespaces attached — conclusive from sysfs, no Identify needed.
	out, err := DetachDevice(context.Background(), c, device(1, 1, 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if out.Disconnected || !out.SharedSubsystem {
		t.Errorf("outcome = %+v, want kept as shared", out)
	}
	if len(c.disconnected) != 0 {
		t.Errorf("Disconnect was called for %v", c.disconnected)
	}
}

// A namespace above 1 proves the subsystem is shared even when this scan sees
// only that one.
func TestDetachDevice_KeepsSubsystemForANamespaceAboveOne(t *testing.T) {
	c := &recordingConnector{}

	out, err := DetachDevice(context.Background(), c, device(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if out.Disconnected || !out.SharedSubsystem {
		t.Errorf("outcome = %+v, want kept as shared", out)
	}
}

// The race the capability question closes: a subsystem provisioned to be shared
// currently holds only this volume. Enumerating neighbours would answer "none"
// and allow a disconnect that a namespace joining a moment later — or during the
// teardown — turns destructive.
func TestDetachDevice_KeepsAShareableSubsystemWithNoCurrentCoTenants(t *testing.T) {
	stubMultiNamespace(t, true, nil) // max_namespaces > 1, one namespace attached
	c := &recordingConnector{}

	dev := device(1, 1)
	if tenants, err := dev.CoTenants(context.Background()); err == nil && len(tenants) != 0 {
		t.Fatalf("fixture must have no current co-tenants, got %d", len(tenants))
	}

	out, err := DetachDevice(context.Background(), c, dev)
	if err != nil {
		t.Fatal(err)
	}
	if out.Disconnected {
		t.Error("disconnected a subsystem that other volumes may join at any time")
	}
	if !out.SharedSubsystem {
		t.Errorf("outcome = %+v, want SharedSubsystem", out)
	}
}

func TestDetachDevice_RefusesWhenTheQuestionCannotBeAnswered(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		// No live controller to issue Identify against.
		{"not connected", errs.ErrNotConnected},
		// Not Linux: no admin passthrough ioctl.
		{"unsupported platform", errs.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubMultiNamespace(t, false, tc.err)
			c := &recordingConnector{}

			_, err := DetachDevice(context.Background(), c, device(1, 1))
			if !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want %v", err, tc.err)
			}
			if len(c.disconnected) != 0 {
				t.Error("disconnected without knowing whether the subsystem is shared")
			}
		})
	}

	t.Run("no subsystem nqn", func(t *testing.T) {
		c := &recordingConnector{}
		_, err := DetachDevice(context.Background(), c, nvme.Device{})
		if !errors.Is(err, errs.ErrUnsupported) {
			t.Errorf("err = %v, want errs.ErrUnsupported", err)
		}
		if len(c.disconnected) != 0 {
			t.Error("disconnected a device with no subsystem NQN")
		}
	})
}

func TestDetachDevice_PropagatesDisconnectFailure(t *testing.T) {
	stubMultiNamespace(t, false, nil)
	boom := errors.New("delete_controller: permission denied")
	c := &recordingConnector{err: boom}

	out, err := DetachDevice(context.Background(), c, device(1, 1))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the disconnect error", err)
	}
	if out.Disconnected {
		t.Error("reported a disconnect that failed")
	}
}

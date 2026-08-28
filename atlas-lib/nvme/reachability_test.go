package nvme

import (
	"context"
	"path/filepath"
	"testing"
)

const dupNQN = "nqn.2023-02.io.simplyblock:c30a691a-015f-40c1-a7b6-26897264d489:lvol:" +
	"792e184c-43d5-40ba-b497-3b645347cf1d"

const dupUUID = "792e184c-43d5-40ba-b497-3b645347cf1d"

// staleFixture is the node-migration shape: the volume has been reconnected to
// a new storage node, giving a second subsystem for the same NQN, while the old
// subsystem lingers with a connecting controller and an inaccessible path. The
// stale one is nvme-subsys0, so scan order favors exactly the wrong device.
func staleFixture(t *testing.T) string {
	t.Helper()
	stale, fresh := "class/nvme-subsystem/nvme-subsys0", "class/nvme-subsystem/nvme-subsys1"
	return writeFixture(t, map[string]string{
		// stale subsystem: head nvme0n1 over controller nvme0
		stale + "/subsysnqn":                   dupNQN,
		stale + "/nvme0/uevent":                "",
		stale + "/nvme0n1/nsid":                "1",
		stale + "/nvme0n1/uuid":                dupUUID,
		stale + "/nvme0n1/dev":                 "259:1",
		"class/nvme/nvme0/subsysnqn":           dupNQN,
		"class/nvme/nvme0/state":               "connecting",
		"class/nvme/nvme0/address":             "traddr=192.168.10.3,trsvcid=4420",
		"class/nvme/nvme0/nvme0c0n1/nsid":      "1",
		"class/nvme/nvme0/nvme0c0n1/ana_state": "inaccessible",

		// fresh subsystem: head nvme2n1 over controller nvme2
		fresh + "/subsysnqn":                   dupNQN,
		fresh + "/nvme2/uevent":                "",
		fresh + "/nvme2n1/nsid":                "1",
		fresh + "/nvme2n1/uuid":                dupUUID,
		fresh + "/nvme2n1/dev":                 "259:7",
		"class/nvme/nvme2/subsysnqn":           dupNQN,
		"class/nvme/nvme2/state":               "live",
		"class/nvme/nvme2/address":             "traddr=192.168.10.5,trsvcid=4420",
		"class/nvme/nvme2/nvme2c2n1/nsid":      "1",
		"class/nvme/nvme2/nvme2c2n1/ana_state": "optimized",
	})
}

// Controllers and ANA legs must follow the subsystem that owns them. Matching
// on the NQN alone would give each of two same-NQN subsystems both controllers
// and both legs, making the stale one look as healthy as the fresh one.
func TestScanKeepsDuplicateSubsystemsApart(t *testing.T) {
	subs, err := scanSubsystems(staleFixture(t), "/dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("got %d subsystems, want 2 for one NQN", len(subs))
	}

	for _, want := range []struct {
		id       SubsystemID
		ctrl     ControllerID
		nsName   string
		anaState ANAState
	}{
		{"nvme-subsys0", "nvme0", "nvme0n1", ANAInaccessible},
		{"nvme-subsys1", "nvme2", "nvme2n1", ANAOptimized},
	} {
		var s Subsystem
		for _, c := range subs {
			if c.ID == want.id {
				s = c
			}
		}
		if len(s.Controllers) != 1 || s.Controllers[0].ID != want.ctrl {
			t.Errorf("%s controllers = %v, want just %s", want.id, ctrlIDsOf(s), want.ctrl)
		}
		if len(s.Namespaces) != 1 || s.Namespaces[0].Name != want.nsName {
			t.Fatalf("%s namespaces = %d, want just %s", want.id, len(s.Namespaces), want.nsName)
		}
		paths := s.Namespaces[0].Paths
		if len(paths) != 1 || paths[0].ANAState != want.anaState {
			t.Errorf("%s/%s paths = %v, want one %s", want.id, want.nsName, paths, want.anaState)
		}
	}
}

func TestPickPrefersLiveSubsystem(t *testing.T) {
	r := NewSysfsDeviceResolver(SysfsConfig{SysRoot: staleFixture(t), DevRoot: "/dev"})
	ctx := context.Background()

	all, err := r.ListWithSelector(ctx, DeviceSelector{NQN: dupNQN})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListWithSelector = %d devices, want both the stale and the fresh one", len(all))
	}

	// Both By* keys are satisfied by either device, and the reachable one wins,
	// even though the stale head comes first in scan order.
	byNS, err := r.ByNamespace(ctx, dupNQN, 1)
	if err != nil {
		t.Fatal(err)
	}
	if byNS.Namespace.Dev != "259:7" {
		t.Errorf("ByNamespace = %s (%s), want the fresh 259:7",
			byNS.Namespace.DevicePath, byNS.Namespace.Dev)
	}
	byUUID, err := r.ByUUID(ctx, dupUUID)
	if err != nil {
		t.Fatal(err)
	}
	if byUUID.Namespace.Dev != "259:7" {
		t.Errorf("ByUUID = %s (%s), want the fresh 259:7",
			byUUID.Namespace.DevicePath, byUUID.Namespace.Dev)
	}

	// ByDevicePath names one device outright, so it must still return the
	// stale one when asked for it: reachability ranks, it does not filter.
	stale, err := r.ByDevicePath(ctx, "/dev/nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	if stale.Accessible() {
		t.Error("stale device reports accessible")
	}
}

// nonMultipathFixture is the nvme_core.multipath=0 layout: no subsystem-level
// head, each controller exposing its own block device for the same namespace.
// The first in scan order is the one whose controller is down.
func nonMultipathFixture(t *testing.T) string {
	t.Helper()
	sub := "class/nvme-subsystem/nvme-subsys0"
	return writeFixture(t, map[string]string{
		sub + "/subsysnqn":    dupNQN,
		sub + "/nvme0/uevent": "",
		sub + "/nvme1/uevent": "",

		"class/nvme/nvme0/subsysnqn":    dupNQN,
		"class/nvme/nvme0/state":        "connecting",
		"class/nvme/nvme0/nvme0n1/nsid": "1",
		"class/nvme/nvme0/nvme0n1/uuid": dupUUID,
		"class/nvme/nvme0/nvme0n1/dev":  "259:1",

		"class/nvme/nvme1/subsysnqn":    dupNQN,
		"class/nvme/nvme1/state":        "live",
		"class/nvme/nvme1/nvme1n1/nsid": "1",
		"class/nvme/nvme1/nvme1n1/uuid": dupUUID,
		"class/nvme/nvme1/nvme1n1/dev":  "259:2",
	})
}

func TestNonMultipathNamespaces(t *testing.T) {
	root := nonMultipathFixture(t)
	r := NewSysfsDeviceResolver(SysfsConfig{SysRoot: root, DevRoot: "/dev"})
	ctx := context.Background()

	all, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List = %d devices, want one per controller", len(all))
	}
	for _, d := range all {
		if d.Namespace.Controller == "" {
			t.Errorf("%s has no owning controller recorded", d.Namespace.Name)
		}
		if len(d.Namespace.Paths) != 0 {
			t.Errorf("%s has ANA paths, but no head means no ANA view", d.Namespace.Name)
		}
	}

	// Same volume over two paths: siblings, not co-tenants, and not a
	// multi-namespace subsystem.
	sibs := Siblings(all[0], all)
	if len(sibs) != 1 || sibs[0].Namespace.Name != all[1].Namespace.Name {
		t.Errorf("Siblings = %v, want the other path's device", devNames(sibs))
	}
	if ct, _ := all[0].CoTenants(context.Background()); len(ct) != 0 {
		t.Errorf("CoTenants = %v, want none", devNames(ct))
	}
	stubIdentifyMNAN(t, 1, nil)
	if multi, err := all[0].IsMultiNamespace(); err != nil || multi {
		t.Errorf("IsMultiNamespace = %v, %v; want false, nil (one volume, two paths)", multi, err)
	}

	// Reachability follows the owning controller, not just any live one in
	// the subsystem, so the device behind the connecting controller loses.
	got, err := r.ByUUID(ctx, dupUUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.Dev != "259:2" {
		t.Errorf("ByUUID = %s (%s), want 259:2, the one on the live controller",
			got.Namespace.DevicePath, got.Namespace.Dev)
	}
	if filepath.Base(got.Namespace.SysfsPath) != "nvme1n1" {
		t.Errorf("sysfs path = %q, want the controller-owned nvme1n1", got.Namespace.SysfsPath)
	}
}

func TestDeviceRank(t *testing.T) {
	live := Controller{ID: "nvme0", State: "live"}
	dead := Controller{ID: "nvme0", State: "connecting"}
	sub := func(c Controller) Subsystem { return Subsystem{Controllers: []Controller{c}} }

	cases := []struct {
		name string
		dev  Device
		want int
	}{
		{"optimized path", Device{
			Namespace: Namespace{Paths: []Path{{ANAState: ANAOptimized}}},
			Subsystem: sub(live),
		}, rankOptimized},
		{"non-optimized path", Device{
			Namespace: Namespace{Paths: []Path{{ANAState: ANANonOptimized}}},
			Subsystem: sub(live),
		}, rankAccessible},
		{"optimized wins over inaccessible", Device{
			Namespace: Namespace{Paths: []Path{{ANAState: ANAInaccessible}, {ANAState: ANAOptimized}}},
			Subsystem: sub(live),
		}, rankOptimized},
		{"live controller cannot rescue lost paths", Device{
			Namespace: Namespace{Paths: []Path{{ANAState: ANAPersistentLoss}}},
			Subsystem: sub(live),
		}, rankUnreachable},
		{"no ANA view, live controller", Device{
			Namespace: Namespace{Name: "nvme0n1", Controller: "nvme0"},
			Subsystem: sub(live),
		}, rankLive},
		{"no ANA view, dead controller", Device{
			Namespace: Namespace{Name: "nvme0n1", Controller: "nvme0"},
			Subsystem: sub(dead),
		}, rankUnreachable},
		{"no ANA view, live controller is not the owner", Device{
			Namespace: Namespace{Name: "nvme1n1", Controller: "nvme1"},
			Subsystem: sub(live),
		}, rankUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.dev.rank(); got != tc.want {
				t.Errorf("rank = %d, want %d", got, tc.want)
			}
			if got := tc.dev.Accessible(); got != (tc.want > rankUnreachable) {
				t.Errorf("Accessible = %v at rank %d", got, tc.want)
			}
		})
	}
}

func TestLegHead(t *testing.T) {
	for leg, want := range map[string]string{
		"nvme0c0n1":  "nvme0n1",
		"nvme0c1n1":  "nvme0n1",
		"nvme2c2n1":  "nvme2n1",
		"nvme0c1n12": "nvme0n12",
		"nvme0n1":    "", // a head, not a leg
		"ng0n1":      "",
	} {
		if got := legHead(leg); got != want {
			t.Errorf("legHead(%q) = %q, want %q", leg, got, want)
		}
	}
}

func ctrlIDsOf(s Subsystem) []ControllerID {
	out := make([]ControllerID, len(s.Controllers))
	for i, c := range s.Controllers {
		out[i] = c.ID
	}
	return out
}

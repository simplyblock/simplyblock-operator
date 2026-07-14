package nvme

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

// writeFixture builds a sysfs tree under root from a map of relative path
// to file contents, mirroring the multipath NVMe-oF layout observed on a
// live node (one subsystem, two controllers, one namespace).
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func vm17Fixture(t *testing.T) string {
	const nqn = "nqn.2023-02.io.simplyblock:c30a691a-015f-40c1-a7b6-26897264d489:lvol:792e184c-43d5-40ba-b497-3b645347cf1d"
	sub := "class/nvme-subsystem/nvme-subsys0"
	ns := sub + "/nvme0n1"
	return writeFixture(t, map[string]string{
		// subsystem
		sub + "/subsysnqn":    nqn,
		sub + "/model":        "792e184c-43d5-40ba-b497-3b645347cf1d    ",
		sub + "/serial":       "ha                  ",
		sub + "/firmware_rev": "25.01   ",
		sub + "/subsystype":   "nvm",
		sub + "/iopolicy":     "numa",
		// namespace (block head)
		ns + "/nsid":                     "1",
		ns + "/uuid":                     "792e184c-43d5-40ba-b497-3b645347cf1d",
		ns + "/nguid":                    "51673754-7362-6165-6138-5074624c4e6e",
		ns + "/wwid":                     "uuid.792e184c-43d5-40ba-b497-3b645347cf1d",
		ns + "/csi":                      "0",
		ns + "/size":                     "20971520",
		ns + "/nuse":                     "2621440",
		ns + "/metadata_bytes":           "0",
		ns + "/ro":                       "0",
		ns + "/hidden":                   "0",
		ns + "/dev":                      "259:1",
		ns + "/queue/logical_block_size": "4096",
		// controller nvme0 (path A -> vm19)
		"class/nvme/nvme0/subsysnqn":   nqn,
		"class/nvme/nvme0/transport":   "tcp",
		"class/nvme/nvme0/state":       "live",
		"class/nvme/nvme0/cntrltype":   "io",
		"class/nvme/nvme0/cntlid":      "1",
		"class/nvme/nvme0/address":     "traddr=192.168.10.69,trsvcid=4426,src_addr=192.168.10.67",
		"class/nvme/nvme0/numa_node":   "-1",
		"class/nvme/nvme0/queue_count": "15",
		"class/nvme/nvme0/dev":         "238:0",
		// controller nvme1 (path B -> vm17)
		"class/nvme/nvme1/subsysnqn": nqn,
		"class/nvme/nvme1/transport": "tcp",
		"class/nvme/nvme1/state":     "live",
		"class/nvme/nvme1/cntlid":    "1000",
		"class/nvme/nvme1/address":   "traddr=192.168.10.67,trsvcid=4426,src_addr=192.168.10.67",
		// per-controller ANA legs (active/passive)
		"class/nvme/nvme0/nvme0c0n1/nsid":      "1",
		"class/nvme/nvme0/nvme0c0n1/ana_state": "optimized",
		"class/nvme/nvme0/nvme0c0n1/ana_grpid": "1",
		"class/nvme/nvme1/nvme0c1n1/nsid":      "1",
		"class/nvme/nvme1/nvme0c1n1/ana_state": "non-optimized",
		"class/nvme/nvme1/nvme0c1n1/ana_grpid": "1",
	})
}

func TestSysfsSubsystemResolver(t *testing.T) {
	root := vm17Fixture(t)
	r := NewSysfsSubsystemResolver(SysfsConfig{SysRoot: root})
	ctx := context.Background()

	subs, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d subsystems, want 1", len(subs))
	}
	s := subs[0]
	if s.IOPolicy != "numa" || s.Type != "nvm" {
		t.Errorf("subsystem attrs: iopolicy=%q type=%q", s.IOPolicy, s.Type)
	}
	if s.Model != "792e184c-43d5-40ba-b497-3b645347cf1d" {
		t.Errorf("model not trimmed: %q", s.Model)
	}
	if len(s.Controllers) != 2 {
		t.Fatalf("got %d controllers, want 2 (multipath)", len(s.Controllers))
	}
	if len(s.Namespaces) != 1 {
		t.Fatalf("got %d namespaces, want 1", len(s.Namespaces))
	}

	byNQN, err := r.ByNQN(ctx, s.NQN)
	if err != nil {
		t.Fatal(err)
	}
	if byNQN.ID != s.ID {
		t.Errorf("ByNQN returned %q, want %q", byNQN.ID, s.ID)
	}
	if _, err := r.ByNQN(ctx, "nqn.does.not:exist"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("ByNQN(missing) err = %v, want ErrNotFound", err)
	}
}

func TestSysfsDeviceResolver(t *testing.T) {
	root := vm17Fixture(t)
	r := NewSysfsDeviceResolver(SysfsConfig{SysRoot: root, DevRoot: "/dev"})
	ctx := context.Background()

	d, err := r.ByUUID(ctx, "792e184c-43d5-40ba-b497-3b645347cf1d")
	if err != nil {
		t.Fatal(err)
	}
	if d.Namespace.ID != 1 {
		t.Errorf("nsid = %d, want 1", d.Namespace.ID)
	}
	if d.Namespace.DevicePath != "/dev/nvme0n1" {
		t.Errorf("device path = %q, want /dev/nvme0n1", d.Namespace.DevicePath)
	}
	if d.Namespace.Capacity != 20971520 || d.Namespace.LogicalBlockSize != 4096 {
		t.Errorf("size attrs: cap=%d lbs=%d", d.Namespace.Capacity, d.Namespace.LogicalBlockSize)
	}
	if len(d.Subsystem.Controllers) != 2 {
		t.Errorf("device subsystem controllers = %d, want 2", len(d.Subsystem.Controllers))
	}

	// controller address parsing
	var a0 Address
	for _, c := range d.Subsystem.Controllers {
		if c.ID == "nvme0" {
			a0 = c.Address
		}
	}
	if a0.TrAddr != "192.168.10.69" || a0.TrSvcID != "4426" || a0.SrcAddr != "192.168.10.67" {
		t.Errorf("nvme0 address parsed wrong: %+v", a0)
	}

	// ANA: one optimized + one non-optimized path, both in group 1
	if len(d.Namespace.Paths) != 2 {
		t.Fatalf("namespace paths = %d, want 2 (multipath ANA)", len(d.Namespace.Paths))
	}
	states := map[ControllerID]ANAState{}
	for _, p := range d.Namespace.Paths {
		states[p.Controller] = p.ANAState
		if p.ANAGroupID != 1 || p.NSID != 1 {
			t.Errorf("path %s: grpid=%d nsid=%d", p.Name, p.ANAGroupID, p.NSID)
		}
		if !p.ANAState.Accessible() {
			t.Errorf("path %s state %q should be accessible", p.Name, p.ANAState)
		}
	}
	if states["nvme0"] != ANAOptimized || states["nvme1"] != ANANonOptimized {
		t.Errorf("ANA states = %v, want nvme0=optimized nvme1=non-optimized", states)
	}

	if _, err := r.ByDevicePath(ctx, "/dev/nvme0n1"); err != nil {
		t.Errorf("ByDevicePath: %v", err)
	}
	if _, err := r.ByNamespace(ctx, d.Subsystem.NQN, 1); err != nil {
		t.Errorf("ByNamespace: %v", err)
	}
	if _, err := r.ByUUID(ctx, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("ByUUID(missing) err = %v, want ErrNotFound", err)
	}
}

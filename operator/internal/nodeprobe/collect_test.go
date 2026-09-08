// What survives the translation from a collected inventory into a report.
//
// The interesting cases are the ones that lose something if nobody is looking.
// A device whose usage could not be established carries its reason in an error,
// and an error is the one thing JSON cannot carry, so a report that marshaled
// the inventory directly would hand the operator a device refused for a reason
// that had become an empty object on the way. That is the defect the whole wire
// type exists to avoid, and it is the first test below.

package nodeprobe

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/pci"
)

// probedAt is a fixed instant, so a report is compared against a value rather
// than against whatever the clock said.
var probedAt = time.Date(2026, 9, 8, 11, 30, 0, 0, time.UTC)

// oneFreeDisk is the candidate a worker with an unclaimed NVMe SSD produces.
func oneFreeDisk() blockdev.Candidate {
	return blockdev.Candidate{
		Disk: blockdev.Disk{
			Device: blockdev.Device{
				Path:      "/dev/nvme0n1",
				Name:      "nvme0n1",
				SizeBytes: 3200631791616,
			},
			Kind:       blockdev.KindDisk,
			Transport:  blockdev.TransportNVMe,
			Model:      "SAMSUNG MZQL23T8HCLS-00A07",
			Serial:     "S6CVNE0T500123",
			PCIAddress: "0000:5e:00.0",
			NUMANode:   0,
		},
		Reading: blockdev.Reading{Content: blockdev.ContentBlank, Detail: "read and zero"},
	}
}

func TestFromInventoryKeepsTheReasonADeviceCouldNotBeChecked(t *testing.T) {
	// The reason lives in an error on the way in and has to be a string on the
	// way out. json.Marshal of an error is {}, so a report built by marshaling
	// the inventory would say the device was refused and not why.
	unreadable := errors.New("no permission to open /dev/nvme0n1")
	candidate := oneFreeDisk()
	candidate.Usage = blockdev.Usage{ProbeErr: unreadable}
	candidate.Rejections = []blockdev.Rejection{{
		Reason: blockdev.ReasonUnreadable,
		Detail: fmt.Sprintf("the kernel could not be asked: %v", unreadable),
	}}
	candidate.Reading = blockdev.Reading{}

	report := FromInventory(testNode, probedAt, inventory.Inventory{
		Devices: []blockdev.Candidate{candidate},
	}, nil)

	if len(report.Devices) != 1 {
		t.Fatalf("rendered %d devices, want 1", len(report.Devices))
	}
	device := report.Devices[0]
	if device.Available {
		t.Error("a device whose usage could not be established was reported free")
	}
	if !device.RejectedFor(string(blockdev.ReasonUnreadable)) {
		t.Fatalf("rejected for %+v, want %s among them", device.Rejections, blockdev.ReasonUnreadable)
	}

	// Round-tripping is the part that would silently drop it.
	encoded, err := Encode(report)
	if err != nil {
		t.Fatalf("encode the report: %v", err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	detail := decoded.Devices[0].Rejections[0].Detail
	if detail == "" {
		t.Fatal("the reason survived the render and not the round trip")
	}
	if !strings.Contains(detail, unreadable.Error()) {
		t.Errorf("the detail is %q, and it does not name why the device could not be asked about", detail)
	}
}

func TestFromInventoryLeavesTheContentOfAnUnopenedDeviceEmpty(t *testing.T) {
	// A device refused before anything was opened has no reading. Writing
	// "Unknown" into the report would read as a device that was checked and
	// came back inconclusive, which is a different fact.
	mounted := oneFreeDisk()
	mounted.Reading = blockdev.Reading{}
	mounted.Rejections = []blockdev.Rejection{{Reason: blockdev.ReasonMounted, Detail: "mounted at [/]"}}

	report := FromInventory(testNode, probedAt, inventory.Inventory{
		Devices: []blockdev.Candidate{mounted, oneFreeDisk()},
	}, nil)

	if got := report.Devices[0].Content; got != "" {
		t.Errorf("a device that was never opened reports content %q, want none", got)
	}
	if got := report.Devices[1].Content; got != "Blank" {
		t.Errorf("a device that was read reports content %q, want Blank", got)
	}
}

func TestFromInventoryRendersEveryDeviceAndFiltersNone(t *testing.T) {
	// The run's device filter is the operator's to apply, over the whole
	// fleet's reports. A probe that had already narrowed its answer would leave
	// an administrator reading a narrowed report that never said what it left
	// out.
	boot := oneFreeDisk()
	boot.Name, boot.Path = "nvme1n1", "/dev/nvme1n1"
	boot.Rejections = []blockdev.Rejection{{Reason: blockdev.ReasonMounted, Detail: "mounted at [/]"}}

	loop := blockdev.Candidate{
		Disk:       blockdev.Disk{Device: blockdev.Device{Name: "loop0", Path: "/dev/loop0"}, Kind: blockdev.KindLoop},
		Rejections: []blockdev.Rejection{{Reason: blockdev.ReasonNotAWholeDisk, Detail: "the device is a Loop"}},
	}

	report := FromInventory(testNode, probedAt, inventory.Inventory{
		Devices: []blockdev.Candidate{oneFreeDisk(), boot, loop},
	}, nil)

	if len(report.Devices) != 3 {
		t.Fatalf("rendered %d devices, want all 3", len(report.Devices))
	}
	if free := report.AvailableDevices(); len(free) != 1 || free[0].Name != "nvme0n1" {
		t.Errorf("the free devices are %+v, want nvme0n1 alone", free)
	}
}

func TestFromInventoryRendersTheMachinesReadings(t *testing.T) {
	report := FromInventory(testNode, probedAt, inventory.Inventory{
		CPU: inventory.CPU{
			OnlineCount:    16,
			OfflineCount:   2,
			PhysicalCores:  8,
			Sockets:        2,
			ThreadsPerCore: 2,
			HyperThreading: true,
			AffinityCount:  4,
			NUMANodes: []inventory.NUMACPUs{
				{Node: 0, OnlineCPUs: []int{0, 1}, PhysicalCores: 1},
				{Node: 1, OnlineCPUs: []int{2, 3}, PhysicalCores: 1},
			},
		},
		HugePages: inventory.HugePages{Pools: []inventory.HugePagePool{{
			SizeBytes: 1 << 30,
			Total:     16,
			Free:      16,
			NUMANodes: []inventory.NUMAHugePages{{Node: 0, Total: 8, Free: 8}, {Node: 1, Total: 8, Free: 8}},
		}}},
		Interfaces: []inventory.Interface{{
			Name:       "eth0",
			SpeedMbps:  25000,
			OperState:  inventory.LinkUp,
			Driver:     "mlx5_core",
			PCIAddress: "0000:3b:00.0",
			NUMANode:   0,
		}},
	}, nil)

	if report.Node != testNode || !report.ProbedAt.Time.Equal(probedAt) {
		t.Errorf("the report is for %q at %s, want testNode at %s", report.Node, report.ProbedAt, probedAt)
	}
	if report.Version != ReportVersion {
		t.Errorf("the report is version %d, want %d", report.Version, ReportVersion)
	}
	if report.CPU.OnlineCPUs != 16 || report.CPU.PhysicalCores != 8 || !report.CPU.HyperThreading {
		t.Errorf("the CPU reading is %+v", report.CPU)
	}
	if len(report.CPU.NUMANodes) != 2 || report.CPU.NUMANodes[1].Node != 1 {
		t.Errorf("the per-node CPU reading is %+v", report.CPU.NUMANodes)
	}
	if report.HugePageBytes() != 16<<30 {
		t.Errorf("read %d bytes of huge pages, want %d", report.HugePageBytes(), uint64(16)<<30)
	}
	if len(report.HugePages[0].NUMANodes) != 2 {
		t.Errorf("the per-node huge pages are %+v", report.HugePages[0].NUMANodes)
	}
	if len(report.Interfaces) != 1 || report.Interfaces[0].SpeedMbps != 25000 ||
		report.Interfaces[0].State != string(inventory.LinkUp) {
		t.Errorf("the interface reading is %+v", report.Interfaces)
	}
}

func TestFromInventoryCarriesEveryReadingAndNotMostOfThem(t *testing.T) {
	// A translation layer's whole job is carrying fields, and its failure mode
	// is a field nobody wired up: the function still compiles, the tests that
	// name other fields still pass, and the report goes out with a zero where a
	// reading should be. That is what happened to the memory reading, which
	// reached a real worker as nought of nought megabytes available.
	//
	// So this asserts that every top-level reading survives a non-zero
	// inventory, rather than naming the ones somebody remembered.
	inv := inventory.Inventory{
		CPU:    inventory.CPU{OnlineCount: 16, PhysicalCores: 8},
		Memory: inventory.Memory{TotalBytes: 256 << 30, AvailableBytes: 250 << 30},
		HugePages: inventory.HugePages{Pools: []inventory.HugePagePool{
			{SizeBytes: 1 << 30, Total: 16, Free: 16},
		}},
		Interfaces:      []inventory.Interface{{Name: "eth0", SpeedMbps: 25000}},
		Devices:         []blockdev.Candidate{oneFreeDisk()},
		NVMeControllers: []pci.Device{{Address: "0000:5e:00.0", Class: "0x010802", Driver: "nvme"}},
	}

	report := FromInventory("worker-3", probedAt, inv, nil)

	for name, carried := range map[string]bool{
		"cpu":             report.CPU.OnlineCPUs != 0,
		"memory":          report.Memory.TotalBytes != 0,
		"hugePages":       len(report.HugePages) != 0,
		"interfaces":      len(report.Interfaces) != 0,
		"devices":         len(report.Devices) != 0,
		"nvmeControllers": len(report.NVMeControllers) != 0,
	} {
		if !carried {
			t.Errorf("the %s reading was not carried into the report", name)
		}
	}

	// The memory numbers in particular, since they are what a reviewer sizes
	// against.
	if report.Memory.AvailableBytes != 250<<30 {
		t.Errorf("read %d bytes available, want %d",
			report.Memory.AvailableBytes, uint64(250)<<30)
	}
}

func TestFromInventoryDoesNotReportTheProbesOwnAffinityAsTheWorkers(t *testing.T) {
	// AffinityCount is how many CPUs the probe pod may run on, which is its own
	// cgroup's limit and not the machine's capacity. A report carrying it would
	// invite a reviewer to size a storage node against the probe's limits.
	report := FromInventory(testNode, probedAt, inventory.Inventory{
		CPU: inventory.CPU{OnlineCount: 64, AffinityCount: 2},
	}, nil)

	if report.CPU.OnlineCPUs != 64 {
		t.Errorf("the report says %d online CPUs, want the machine's 64", report.CPU.OnlineCPUs)
	}
	encoded, err := Encode(report)
	if err != nil {
		t.Fatalf("encode the report: %v", err)
	}
	if strings.Contains(string(encoded), "affinity") {
		t.Error("the report carries the probe's own CPU affinity")
	}
}

func TestFromInventoryListsEachReaderThatFailedSeparately(t *testing.T) {
	// Collect joins its failures, and the report says what could not be read as
	// a list because that is what it is: one blob with embedded newlines is not
	// something a status message or an event can quote one of.
	joined := errors.Join(
		fmt.Errorf("read the CPU topology: %w", errors.New("no online CPUs")),
		fmt.Errorf("read the network interfaces: %w", errors.New("no class/net")),
	)

	report := FromInventory(testNode, probedAt, inventory.Inventory{}, joined)

	if len(report.Unreadable) != 2 {
		t.Fatalf("listed %d failures, want 2: %v", len(report.Unreadable), report.Unreadable)
	}
	for _, want := range []string{"CPU topology", "network interfaces"} {
		if !slices.ContainsFunc(report.Unreadable, func(s string) bool { return strings.Contains(s, want) }) {
			t.Errorf("the failures %v do not name %q", report.Unreadable, want)
		}
	}
}

func TestFromInventoryReportsNoFailuresWhenThereWereNone(t *testing.T) {
	report := FromInventory(testNode, probedAt, inventory.Inventory{}, nil)
	if len(report.Unreadable) != 0 {
		t.Errorf("listed %v for a collection that succeeded", report.Unreadable)
	}
}

func TestSummaryNamesWhatWasFound(t *testing.T) {
	boot := oneFreeDisk()
	boot.Rejections = []blockdev.Rejection{{Reason: blockdev.ReasonMounted}}

	report := FromInventory(testNode, probedAt, inventory.Inventory{
		CPU:     inventory.CPU{OnlineCount: 16, PhysicalCores: 8, HyperThreading: true},
		Devices: []blockdev.Candidate{oneFreeDisk(), boot},
	}, nil)

	summary := Summary(report)
	for _, want := range []string{testNode, "1 of 2", "16 online CPUs", "8 cores"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary %q does not say %q", summary, want)
		}
	}
}

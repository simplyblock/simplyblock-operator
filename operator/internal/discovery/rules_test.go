// What each rule admits and declines, and the reason it gives.
//
// The reason is asserted alongside the verdict throughout, because a refusal
// nobody can read is the failure mode this whole layer exists to avoid: an
// administrator whose disk is missing from a draft has to be able to find out
// which rule took it and why.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const tb = uint64(1) << 40

// admit runs one rule and returns its verdict and reason.
func admit(rule DeviceRule, device nodeprobe.Device) (bool, string) {
	return rule.Admit(report("worker-1", device), device)
}

func TestAvailableRuleWaivesOnlyAPartitionTable(t *testing.T) {
	free := disk("nvme0n1", "0000:5e:00.0", 0, tb)
	partitioned := refused(free, blockdev.ReasonPartitioned)
	mounted := refused(free, blockdev.ReasonMounted)
	both := refused(free, blockdev.ReasonPartitioned, blockdev.ReasonMounted)

	strict := AvailableRule{}
	waiving := AvailableRule{AllowPartitioned: true}

	if ok, _ := admit(strict, free); !ok {
		t.Error("declined a free disk")
	}
	if ok, why := admit(strict, partitioned); ok {
		t.Error("admitted a partitioned disk without the waiver")
	} else if !strings.Contains(why, string(blockdev.ReasonPartitioned)) {
		t.Errorf("the reason %q does not name the ground", why)
	}
	if ok, _ := admit(waiving, partitioned); !ok {
		t.Error("the waiver did not admit a disk whose only problem is its table")
	}

	// The waiver is narrow on purpose. A disk that is also mounted is one
	// simplyblock would corrupt, and no flag turns that into an intention.
	if ok, _ := admit(waiving, mounted); ok {
		t.Error("the partition waiver admitted a mounted disk")
	}
	if ok, _ := admit(waiving, both); ok {
		t.Error("the partition waiver admitted a disk that is mounted as well as partitioned")
	}
}

func TestClassRuleRefusesADeviceItCannotName(t *testing.T) {
	nvme := disk("nvme0n1", "0000:5e:00.0", 0, tb)
	virtio := blockDisk("vda", 0, tb)

	if ok, _ := admit(ClassRule{Class: ClassNVMe}, nvme); !ok {
		t.Error("an NVMe run declined an NVMe disk")
	}
	if ok, why := admit(ClassRule{Class: ClassNVMe}, virtio); ok {
		t.Error("an NVMe run admitted a virtio disk")
	} else if !strings.Contains(why, "Virtio") {
		t.Errorf("the reason %q does not say what bus it is on", why)
	}

	// The block class names a device by path, which every device has.
	if ok, _ := admit(ClassRule{Class: ClassBlock}, virtio); !ok {
		t.Error("a block run declined a virtio disk")
	}

	// An NVMe device with no PCI address cannot be named in an NVMe draft. A
	// fabric namespace is the real case, and the probe refuses it first — this
	// is the rule that would catch it if it did not.
	noSlot := nvme
	noSlot.PCIAddress = ""
	if ok, why := admit(ClassRule{Class: ClassNVMe}, noSlot); ok {
		t.Error("an NVMe run admitted a device with no PCI address")
	} else if !strings.Contains(why, "PCI address") {
		t.Errorf("the reason %q does not say what is missing", why)
	}
}

func TestWholeDiskRuleRefusesAPartition(t *testing.T) {
	part := disk("nvme0n1p1", "0000:5e:00.0", 0, tb)
	part.Kind = string(blockdev.KindPartition)

	if ok, why := admit(WholeDiskRule{}, part); ok {
		t.Error("admitted a partition as backend storage")
	} else if !strings.Contains(why, "Partition") {
		t.Errorf("the reason %q does not say what it is", why)
	}
}

func TestAllowDenyRuleIsCaseInsensitiveAndDenyWins(t *testing.T) {
	// A PCI address is written lowercase by sysfs and uppercase by plenty of
	// documentation, and an administrator should not be caught by that.
	device := disk("nvme0n1", "0000:5e:00.0", 0, tb)

	if ok, _ := admit(AllowDenyRule{Class: ClassNVMe, Allow: []string{"0000:5E:00.0"}}, device); !ok {
		t.Error("an allow list in uppercase did not match a lowercase address")
	}
	if ok, why := admit(AllowDenyRule{Class: ClassNVMe, Deny: []string{"0000:5e:00.0"}}, device); ok {
		t.Error("a denied address was admitted")
	} else if !strings.Contains(why, "deny list") {
		t.Errorf("the reason %q does not name the list", why)
	}

	// Deny is checked first, so listing an address in both refuses it. A rule
	// that admitted it would make a deny list advisory.
	both := AllowDenyRule{
		Class: ClassNVMe,
		Allow: []string{"0000:5e:00.0"},
		Deny:  []string{"0000:5e:00.0"},
	}
	if ok, _ := admit(both, device); ok {
		t.Error("an address in both lists was admitted; a deny list has to be final")
	}

	// An empty allow list admits everything not denied, rather than nothing.
	if ok, _ := admit(AllowDenyRule{Class: ClassNVMe}, device); !ok {
		t.Error("an empty allow list refused everything")
	}
}

func TestModelRuleMatchesASubstring(t *testing.T) {
	device := disk("nvme0n1", "0000:5e:00.0", 0, tb)

	for _, wanted := range []string{"MZQL2", "mzql2", "SAMSUNG"} {
		if ok, _ := admit(ModelRule{Model: wanted}, device); !ok {
			t.Errorf("%q did not match %q; a model string is padded and vendor-formatted, "+
				"so the filter is a substring", wanted, device.Model)
		}
	}
	if ok, why := admit(ModelRule{Model: "INTEL"}, device); ok {
		t.Error("a model filter admitted another vendor's disk")
	} else if !strings.Contains(why, device.Model) {
		t.Errorf("the reason %q does not quote the model it saw", why)
	}
	if ok, _ := admit(ModelRule{}, device); !ok {
		t.Error("an empty model filter refused a disk")
	}
}

func TestSizeRuleBoundsBothEnds(t *testing.T) {
	small := disk("nvme0n1", "0000:5e:00.0", 0, 100<<30)
	large := disk("nvme1n1", "0000:5f:00.0", 0, 8*tb)

	rule := SizeRule{Min: 1 << 40, Max: 4 * tb}

	if ok, why := admit(rule, small); ok {
		t.Error("admitted a disk below the range")
	} else if !strings.Contains(why, "range starts at") {
		t.Errorf("the reason %q does not say which end it missed", why)
	}
	if ok, why := admit(rule, large); ok {
		t.Error("admitted a disk above the range")
	} else if !strings.Contains(why, "range ends at") {
		t.Errorf("the reason %q does not say which end it missed", why)
	}
	if ok, _ := admit(rule, disk("nvme2n1", "0000:af:00.0", 0, 2*tb)); !ok {
		t.Error("declined a disk inside the range")
	}

	// A zero maximum is no upper bound, not a maximum of zero.
	if ok, _ := admit(SizeRule{Min: 1 << 30}, large); !ok {
		t.Error("a rule with no upper bound refused a large disk")
	}
}

func TestWorkerHasDevicesRefusesAnEmptyWorker(t *testing.T) {
	worker := report("worker-1")

	if ok, why := (WorkerHasDevices{}).Admit(worker, nil); ok {
		t.Error("admitted a worker with no devices; it would expand into a storage " +
			"node with nothing to store on")
	} else if why == "" {
		t.Error("declined a worker without saying why")
	}
	if ok, _ := (WorkerHasDevices{}).Admit(worker, []nodeprobe.Device{
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
	}); !ok {
		t.Error("declined a worker with a device")
	}
}

func TestWorkerWasReadableIsOffByDefaultAndSaysWhatWasMissed(t *testing.T) {
	worker := report("worker-1", disk("nvme0n1", "0000:5e:00.0", 0, tb))
	worker.Unreadable = []string{"read the CPU topology: no online CPUs"}

	// It is not in the default worker rules, so a worker with an unreadable
	// reading still reaches a draft.
	plan := Planner{}.Plan([]nodeprobe.Report{worker}, nil)
	if len(plan.Workers) != 1 {
		t.Errorf("the default pipeline dropped a worker over an unreadable reading")
	}

	if ok, why := (WorkerWasReadable{}).Admit(worker, worker.Devices); ok {
		t.Error("the rule admitted a worker with an unreadable reading")
	} else if !strings.Contains(why, "CPU topology") {
		t.Errorf("the reason %q does not say what could not be read", why)
	}
}

func TestClassOfDefaultsToNVMe(t *testing.T) {
	if got := ClassOf(nil); got != ClassNVMe {
		t.Errorf("a run with no filter scans %q, want %q: NVMe is what every "+
			"deployment before the block class was built out of", got, ClassNVMe)
	}
	if got := ClassOf(&simplyblockv1alpha2.DeviceFilter{}); got != ClassNVMe {
		t.Errorf("an empty filter scans %q, want %q", got, ClassNVMe)
	}
	if got := ClassOf(&simplyblockv1alpha2.DeviceFilter{
		EnableLogicalBlockDevices: ptr.To(true),
	}); got != ClassBlock {
		t.Errorf("a filter asking for block devices scans %q, want %q", got, ClassBlock)
	}
	if got := ClassOf(&simplyblockv1alpha2.DeviceFilter{
		EnableLogicalBlockDevices: ptr.To(false),
	}); got != ClassNVMe {
		t.Errorf("a filter declining block devices scans %q, want %q", got, ClassNVMe)
	}
}

func TestParseSize(t *testing.T) {
	for text, want := range map[string]uint64{
		"1G":   1 << 30,
		"1GiB": 1 << 30,
		"1gb":  1 << 30,
		"2T":   2 * tb,
		"2TiB": 2 * tb,
		"512M": 512 << 20,
		// A bare number is gigabytes, matching how the rest of this API reads
		// a size.
		"100": 100 << 30,
	} {
		got, err := ParseSize(text)
		if err != nil {
			t.Errorf("parse %q: %v", text, err)
			continue
		}
		if got != want {
			t.Errorf("parse %q: read %d, want %d", text, got, want)
		}
	}

	for _, bad := range []string{"", "  ", "G", "1X", "one", "1.5T", "-1G"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("parsed %q as a size", bad)
		}
	}
}

func TestParseSizeRange(t *testing.T) {
	for spec, want := range map[string][2]uint64{
		"100G-2T": {100 << 30, 2 * tb},
		"500G-":   {500 << 30, 0},
		"-2T":     {0, 2 * tb},
		// One size is both bounds: a filter naming a size is naming the disks
		// it expects, and reading it as a floor would admit the 8 TB disk
		// beside them.
		"1T": {tb, tb},
	} {
		min, max, err := ParseSizeRange(spec)
		if err != nil {
			t.Errorf("parse %q: %v", spec, err)
			continue
		}
		if min != want[0] || max != want[1] {
			t.Errorf("parse %q: read %d-%d, want %d-%d", spec, min, max, want[0], want[1])
		}
	}

	for _, bad := range []string{"", "2T-100G", "abc-2T", "100G-xyz"} {
		if _, _, err := ParseSizeRange(bad); err == nil {
			t.Errorf("parsed %q as a size range", bad)
		}
	}
}

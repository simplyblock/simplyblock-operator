// Which controllers a collection asks for their LBA formats, which namespaces
// it asks about, and what it reports when one cannot answer.
//
// The Identify itself is stubbed: no kernel stands behind a fixture tree, and
// the decoding is the `nvme` package's to test.

package inventory

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// formatQuery is one Identify Namespace a collection sent.
type formatQuery struct {
	device string
	nsid   nvme.NamespaceID
}

// recordingFormats answers every query with one 4 KiB format and records it,
// failing for the devices named in fail.
func recordingFormats(queries *[]formatQuery, fail ...string) FormatReader {
	return func(device string, nsid nvme.NamespaceID) (nvme.NamespaceFormats, error) {
		*queries = append(*queries, formatQuery{device, nsid})
		if slices.Contains(fail, filepath.Base(device)) {
			return nvme.NamespaceFormats{}, errors.New("invalid namespace or format")
		}
		return nvme.NamespaceFormats{
			NSID:    nsid,
			Formats: []nvme.LBAFormat{{Index: 0, DataSize: 4096}},
		}, nil
	}
}

func TestCollectReadsTheFormatsOfTheBootController(t *testing.T) {
	// The OKD worker's one kernel-driven controller is nvme2 at 0000:04:00.0,
	// with one namespace, nvme2n1, at NSID 1.
	root := hostFixture(t, "okd-worker")
	cfg := syntheticHost(root)
	cfg.DevRoot = devRootOf(root)
	var queries []formatQuery
	cfg.NVMeFormats = recordingFormats(&queries)

	inv, err := Collect(context.Background(), cfg)
	if err != nil {
		t.Logf("the reading reported: %v", err)
	}

	want := []formatQuery{{filepath.Join(cfg.DevRoot, "nvme2"), 1}}
	if !slices.Equal(queries, want) {
		t.Fatalf("asked %v, want %v", queries, want)
	}
	if len(inv.NVMeFormats) != 1 {
		t.Fatalf("reported formats for %d controllers, want 1: %+v", len(inv.NVMeFormats), inv.NVMeFormats)
	}
	got := inv.NVMeFormats[0]
	if got.Address != "0000:04:00.0" || got.Controller != "nvme2" {
		t.Errorf("reported %s as %s, want 0000:04:00.0 as nvme2", got.Address, got.Controller)
	}
	if len(got.Namespaces) != 1 || got.Namespaces[0].NSID != 1 || len(got.Namespaces[0].Formats) != 1 {
		t.Errorf("namespaces = %+v, want NSID 1 with its format", got.Namespaces)
	}
}

// formatControllers is four NVMe controllers: nvme0 with two namespaces, nvme1
// with none, nvme2 resetting, and one SPDK holds on a userspace driver.
func formatControllers() fixture {
	f := fixture{files: map[string]string{}, links: map[string]string{}}

	kernel := func(address, name, state string, nsids ...string) {
		dir := "devices/pci0000:00/" + address
		f.files[dir+"/class"] = "0x010802"
		f.files[dir+"/numa_node"] = "0"
		f.links[dir+"/driver"] = "../../../bus/pci/drivers/nvme"
		f.files[dir+"/nvme/"+name+"/state"] = state
		for i, nsid := range nsids {
			f.files[dir+"/nvme/"+name+"/"+name+"c0n"+string(rune('1'+i))+"/nsid"] = nsid
		}
		f.links["bus/pci/devices/"+address] = "../../../" + dir
	}
	kernel("0000:01:00.0", "nvme0", "live", "2", "1")
	kernel("0000:02:00.0", "nvme1", "live")
	kernel("0000:03:00.0", "nvme2", "resetting", "1")
	f.dirs = append(f.dirs, "bus/pci/drivers/nvme")

	spdk := "bus/pci/devices/0000:04:00.0"
	f.files[spdk+"/class"] = "0x010802"
	f.links[spdk+"/driver"] = "../../../bus/pci/drivers/vfio-pci"
	f.dirs = append(f.dirs, "bus/pci/drivers/vfio-pci")

	return f
}

func TestCollectReadsEveryNamespaceAndTheCommonFormatsOfAnEmptyController(t *testing.T) {
	root := formatControllers().write(t)
	cfg := syntheticHost(root)
	cfg.DevRoot = filepath.Join(root, "dev")
	var queries []formatQuery
	cfg.NVMeFormats = recordingFormats(&queries)

	inv, err := Collect(context.Background(), cfg)

	// nvme0's namespaces ascending, nvme1's common formats, nothing for the
	// resetting controller or the one on vfio-pci.
	want := []formatQuery{
		{filepath.Join(cfg.DevRoot, "nvme0"), 1},
		{filepath.Join(cfg.DevRoot, "nvme0"), 2},
		{filepath.Join(cfg.DevRoot, "nvme1"), nvme.NSIDAll},
	}
	if !slices.Equal(queries, want) {
		t.Errorf("asked %v, want %v", queries, want)
	}

	addresses := make([]string, 0, len(inv.NVMeFormats))
	for _, c := range inv.NVMeFormats {
		addresses = append(addresses, c.Address)
	}
	if !slices.Equal(addresses, []string{"0000:01:00.0", "0000:02:00.0"}) {
		t.Errorf("reported formats for %v, want the two live kernel-driven controllers", addresses)
	}

	// The resetting controller is a failure to report, not a controller with
	// no formats.
	if !errors.Is(err, errs.ErrNotConnected) || !strings.Contains(err.Error(), "0000:03:00.0") {
		t.Errorf("the collection reported %v, want the resetting controller as not connected", err)
	}
}

func TestAControllerThatCannotAnswerDoesNotHideTheOthers(t *testing.T) {
	root := formatControllers().write(t)
	cfg := syntheticHost(root)
	cfg.DevRoot = filepath.Join(root, "dev")
	var queries []formatQuery
	cfg.NVMeFormats = recordingFormats(&queries, "nvme1")

	inv, err := Collect(context.Background(), cfg)

	if err == nil || !strings.Contains(err.Error(), "0000:02:00.0") {
		t.Errorf("the collection reported %v, want nvme1's failure", err)
	}
	if len(inv.NVMeFormats) != 1 || inv.NVMeFormats[0].Controller != "nvme0" {
		t.Errorf("reported %+v, want nvme0 alone", inv.NVMeFormats)
	}
}

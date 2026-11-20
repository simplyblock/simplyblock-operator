// What a collection does with the NVMe controllers a dead deployment left on a
// userspace driver, and when it does it.
//
// The order is the behavior: a controller handed back to the kernel after the
// block devices were read presents its disk to nobody, and the collection
// reports the absence it was meant to cure. So these tests watch the sysfs
// attribute from inside the device reading rather than afterward.

package inventory

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// takenControllers is one NVMe controller a previous deployment left on a
// userspace driver, with the attributes a rebind writes to.
func takenControllers() fixture {
	f := fixture{files: map[string]string{}, links: map[string]string{}}

	dir := "bus/pci/devices/0000:00:02.0"
	f.files[dir+"/class"] = "0x010802"
	f.files[dir+"/vendor"] = "0x1b36"
	f.files[dir+"/device"] = "0x0010"
	f.files[dir+"/numa_node"] = "-1"
	f.files[dir+"/driver_override"] = "(null)"
	f.links[dir+"/driver"] = "../../../bus/pci/drivers/uio_pci_generic"
	f.dirs = append(f.dirs, dir+"/uio/uio0", "bus/pci/drivers/uio_pci_generic")
	f.files["bus/pci/drivers/uio_pci_generic/unbind"] = ""
	f.files["bus/pci/drivers_probe"] = ""

	return f
}

// hostWithTakenControllers is the whole host, plus the controllers.
func hostWithTakenControllers() fixture {
	whole := wholeHost()
	part := takenControllers()
	maps.Copy(whole.files, part.files)
	maps.Copy(whole.links, part.links)
	whole.dirs = append(whole.dirs, part.dirs...)
	return whole
}

// driverOverride reads the attribute a rebind clears.
func driverOverride(t *testing.T, root string) string {
	t.Helper()
	content, err := os.ReadFile(
		filepath.Join(root, "bus", "pci", "devices", "0000:00:02.0", "driver_override"))
	if err != nil {
		t.Fatalf("read the driver_override: %v", err)
	}
	return strings.TrimSpace(string(content))
}

func TestCollectReclaimsAControllerBeforeItReadsTheDisks(t *testing.T) {
	root := hostWithTakenControllers().write(t)

	// The exclusive opener is called while the disks are being read, which is
	// the only moment from which the order can be observed.
	var whenDisksWereRead string
	read := false
	inv, err := Collect(context.Background(), Config{
		SysfsRoot:          root,
		ProcRoot:           root,
		HostRoot:           root,
		Prober:             blankDisks(),
		Machine:            staticMachine("x86_64"),
		ReclaimControllers: true,
		// No kernel is behind this tree, so no namespace will ever appear and
		// a wait would only be the ceiling, spent.
		ReclaimSettle: -1,
		Exclusive: func(path string) error {
			if !read {
				read = true
				whenDisksWereRead = driverOverride(t, root)
			}
			return handsOverEveryDevice(path)
		},
	})
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	if !read {
		t.Fatal("no disk was read, so the order this test is about never happened")
	}
	if whenDisksWereRead != "" {
		t.Errorf("the controller was still on %q when the disks were read, so its disk was not there to find",
			whenDisksWereRead)
	}

	if len(inv.Reclaimed) != 1 {
		t.Fatalf("the collection reclaimed %d controller(s), want 1", len(inv.Reclaimed))
	}
	if got := inv.Reclaimed[0].Address; got != "0000:00:02.0" {
		t.Errorf("it reclaimed %s, want 0000:00:02.0", got)
	}
}

func TestCollectLeavesTheControllersAloneWhenItWasNotAsked(t *testing.T) {
	// The capture tool and anything else reading a machine it does not own
	// collects without this, and a reading that rebound a host's hardware
	// would be a reading that changed what it came to look at.
	root := hostWithTakenControllers().write(t)

	inv, err := Collect(context.Background(), Config{
		SysfsRoot: root,
		ProcRoot:  root,
		HostRoot:  root,
		Prober:    blankDisks(),
		Exclusive: handsOverEveryDevice,
		Machine:   staticMachine("x86_64"),
	})
	if err != nil {
		t.Fatalf("collect the inventory: %v", err)
	}

	if got := driverOverride(t, root); got != "(null)" {
		t.Errorf("the driver_override is %q, and a collection that was not asked writes nothing", got)
	}
	if len(inv.Reclaimed) != 0 {
		t.Errorf("it reports %d reclaimed controller(s)", len(inv.Reclaimed))
	}
	if len(inv.ControllersBoundToUserspace()) != 1 {
		t.Error("the controller is no longer reported as held by a userspace driver")
	}
}

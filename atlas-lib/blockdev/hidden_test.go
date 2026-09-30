// What the scan does with a gendisk the kernel presents to nobody.
//
// A namespace reached over NVMe multipath is published twice: once as the
// namespace, which has a device node and is what anything opens, and once per
// controller as a hidden path, which has none. The hidden entries sit in
// class/block beside every other device and carry no dev attribute at all,
// because there is no device node for one to name.
//
// That is not a captured tree's omission — it is what the kernel exports, on
// every worker of a fleet that has attached one of this product's own volumes,
// which is to say on every worker a discovery run has to be able to read.

package blockdev

import "testing"

// fabricMultipathHost is a worker that has attached one namespace over two
// NVMe-oF controllers: the namespace itself, and the two hidden paths the
// kernel publishes for it. It is the shape of a lab worker with a simplyblock
// volume mounted, taken from one.
func fabricMultipathHost() tree {
	const (
		namespace = "devices/virtual/nvme-subsystem/nvme-subsys8/nvme8n1"
		pathOne   = "devices/virtual/nvme-fabrics/ctl/nvme3/nvme8c3n1"
		pathTwo   = "devices/virtual/nvme-fabrics/ctl/nvme6/nvme8c6n1"
	)
	t2 := tree{files: map[string]string{}, links: map[string]string{}}

	t2.blockAttrs(namespace, "259:13", 41943040, false, false, false)
	t2.files["devices/virtual/nvme-subsystem/nvme-subsys8/nvme3/transport"] = "tcp"
	t2.links["class/block/nvme8n1"] = "../../" + namespace

	// The hidden paths. blockAttrs writes a dev attribute, so each one is
	// written by hand: the absent dev is the whole point, and a fixture that
	// gave them one would exercise nothing.
	for _, dir := range []string{pathOne, pathTwo} {
		t2.files[dir+"/hidden"] = "1"
		t2.files[dir+"/size"] = "81920"
		t2.files[dir+"/ro"] = "0"
		t2.files[dir+"/removable"] = "0"
		t2.files[dir+"/queue/logical_block_size"] = "512"
	}
	t2.links["class/block/nvme8c3n1"] = "../../" + pathOne
	t2.links["class/block/nvme8c6n1"] = "../../" + pathTwo

	t2.files["self/mountinfo"] = "25 1 0:24 / / rw - overlay overlay rw"
	t2.files["swaps"] = swapOnVirtio
	return t2
}

// The scan reports the host, and the hidden paths are not in it.
//
// Failing instead is what a missing dev attribute meant before: a worker with a
// volume attached reported no disks at all, and said the reason was that one
// entry of class/block could not be read. Nothing downstream could tell that
// apart from a machine with no storage.
func TestAHiddenMultipathPathDoesNotFailTheScan(t *testing.T) {
	root := fabricMultipathHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root, DevRoot: "/dev"})
	if err != nil {
		t.Fatalf("scan a host with an attached multipath namespace: %v", err)
	}

	for _, disk := range disks {
		if disk.Name == "nvme8c3n1" || disk.Name == "nvme8c6n1" {
			t.Errorf("%s is in the scan, and the kernel presents it to nobody: it has "+
				"no device node, so nothing can open it and it names the same bytes "+
				"as the namespace it is a path to", disk.Name)
		}
	}

	// The namespace itself is reported, which is the device those paths lead to
	// and the one a reader is asking about.
	got := scanned(t, disks, "nvme8n1")
	if got.Major != 259 || got.Minor != 13 {
		t.Errorf("read device numbers %d:%d, want 259:13", got.Major, got.Minor)
	}
}

// A device with no dev attribute and no hidden marker still fails the scan. The
// numbers are the identity a mount is matched against, so a device missing them
// for no stated reason cannot be told apart from another, and reporting it
// anyway would put a device with the numbers 0:0 in front of every reader.
func TestADeviceMissingItsNumbersForNoReasonStillFailsTheScan(t *testing.T) {
	const dir = "devices/pci0000:00/0000:00:05.0/virtio2/block/vdb"
	t2 := fabricMultipathHost()
	t2.files[dir+"/size"] = "209715200"
	t2.links["class/block/vdb"] = "../../" + dir
	root := t2.write(t)

	if _, err := Scan(ScanConfig{SysfsRoot: root, DevRoot: "/dev"}); err == nil {
		t.Error("the scan succeeded, and vdb has no device numbers and no reason for it")
	}
}

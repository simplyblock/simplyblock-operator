// The block-device half of the inventory fixtures.
//
// It is separate from the sysfs trees the other three readers use because it is
// the only one whose reader opens a device: the disks are read by blockdev, and
// what has to be supplied for that is a prober, not another directory. What the
// tree here contributes is the two NVMe disks a collection is expected to find,
// one on each memory node, so the NUMA rollup has something to join on.

package inventory

import (
	"context"
	"errors"

	"github.com/simplyblock/atlas/blockdev"
)

// diskHost is two free NVMe SSDs on different memory nodes, plus the boot disk
// whose partitions carry the root filesystem.
func diskHost() fixture {
	const (
		nodeZero = "devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1"
		nodeOne  = "devices/pci0000:80/0000:af:00.0/nvme/nvme1/nvme1n1"
		boot     = "devices/pci0000:00/0000:5f:00.0/nvme/nvme2/nvme2n1"
	)
	f := fixture{files: map[string]string{}, links: map[string]string{}}

	for _, disk := range []struct {
		path, dev, numa string
		sectors         string
	}{
		{nodeZero, "259:0", "0", "6251233968"},
		{nodeOne, "259:2", "1", "6251233968"},
		{boot, "259:4", "0", "976773168"},
	} {
		f.files[disk.path+"/dev"] = disk.dev
		f.files[disk.path+"/size"] = disk.sectors
		f.files[disk.path+"/ro"] = "0"
		f.files[disk.path+"/removable"] = "0"
		f.files[disk.path+"/queue/logical_block_size"] = "512"
		f.files[disk.path+"/queue/physical_block_size"] = "4096"
		f.files[disk.path+"/queue/rotational"] = "0"
	}

	// The controllers are PCIe, and the slot each disk sits in carries the
	// memory node it hangs off.
	f.files["devices/pci0000:00/0000:5e:00.0/numa_node"] = "0"
	f.files["devices/pci0000:80/0000:af:00.0/numa_node"] = "1"
	f.files["devices/pci0000:00/0000:5f:00.0/numa_node"] = "0"

	f.links["class/block/nvme0n1"] = "../../" + nodeZero
	f.links["class/block/nvme1n1"] = "../../" + nodeOne
	f.links["class/block/nvme2n1"] = "../../" + boot
	f.links[nodeZero+"/device"] = ".."
	f.links[nodeOne+"/device"] = ".."
	f.links[boot+"/device"] = ".."

	// The boot disk's root partition, and the mount that makes the whole disk
	// off limits.
	f.files[boot+"/nvme2n1p1/dev"] = "259:5"
	f.files[boot+"/nvme2n1p1/size"] = "976771072"
	f.files[boot+"/nvme2n1p1/partition"] = "1"
	f.files[boot+"/nvme2n1p1/queue/logical_block_size"] = "512"
	f.links["class/block/nvme2n1p1"] = "../../" + boot + "/nvme2n1p1"

	f.files["self/mountinfo"] = "25 1 259:5 / / rw,relatime shared:1 - ext4 /dev/nvme2n1p1 rw"
	f.files["swaps"] = "Filename\t\t\t\tType\t\tSize\tUsed\tPriority"
	return f
}

// blankDisks is a prober that reads every device as holding nothing, which is
// what the two free disks of diskHost hold.
//
// It serves zeros for any device it is asked about rather than for a named set,
// because what these tests are about is the composition: which readings end up
// in an Inventory and how they are joined, not what a signature decodes to.
// The signature catalog has its own tests, against images real tools wrote.
func blankDisks() *blockdev.Prober {
	return blockdev.NewProberWithOpener(
		func(_ context.Context, _ blockdev.Device) (blockdev.Reader, error) {
			return zeroReader{}, nil
		},
		blockdev.WithRegionSize(blockdev.MinRegionSize),
	)
}

// unreadableDisks is a prober whose every read fails, which is the reading a
// device nothing could open produces.
func unreadableDisks() *blockdev.Prober {
	return blockdev.NewProberWithOpener(
		func(_ context.Context, dev blockdev.Device) (blockdev.Reader, error) {
			return nil, errors.New("the test refuses to open " + dev.Path)
		},
		blockdev.WithRegionSize(blockdev.MinRegionSize),
	)
}

// zeroReader serves zeros at every offset.
type zeroReader struct{}

func (zeroReader) Close() error {
	return nil
}

func (zeroReader) ReadAt(ctx context.Context, p []byte, _ int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// handsOverEveryDevice is an exclusive opener whose answer is always that
// nothing holds the device, so the fixture's mounts and holders are the only
// thing deciding a device's usage.
func handsOverEveryDevice(string) error {
	return nil
}

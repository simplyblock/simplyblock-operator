// The two path questions, against the shapes a real sysfs tree has.
//
// The paths below are transcripts rather than constructions. Both functions are
// string walks whose whole risk is that a real tree is shaped differently from
// the one the author pictured: a device behind two PCI bridges crosses three
// addresses, and a virtual device's path has "virtual" in it as a path segment
// and not as a substring of a name.

package sysfs

import "testing"

func TestPCIAddressOfTakesTheInnermostDevice(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{
			name: "an NVMe namespace behind its controller",
			path: "/sys/devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1",
			want: "0000:5e:00.0",
		},
		{
			name: "a SCSI disk behind an ATA port",
			path: "/sys/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda",
			want: "0000:00:1f.2",
		},
		{
			// The device is what identifies it, not the bridge or the root port
			// it hangs off, so the walk runs outward from the leaf and stops at
			// the first address it meets.
			name: "a function behind two bridges",
			path: "/sys/devices/pci0000:00/0000:00:03.1/0000:80:00.0/0000:81:02.0/nvme/nvme4/nvme4n1",
			want: "0000:81:02.0",
		},
		{
			name: "the PCI device itself",
			path: "/sys/devices/pci0000:00/0000:af:00.0",
			want: "0000:af:00.0",
		},
		{
			name: "a virtual device, which sits in no slot",
			path: "/sys/devices/virtual/block/dm-0",
			want: "",
		},
		{
			// A domain is four hex digits and a function is one, so the host
			// bridge's own directory name is not an address and must not be
			// read as one.
			name: "a host bridge is not a device",
			path: "/sys/devices/pci0000:00",
			want: "",
		},
		{
			name: "nothing at all",
			path: "",
			want: "",
		},
	} {
		if got := PCIAddressOf(tc.path); got != tc.want {
			t.Errorf("%s: read %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestIsVirtual(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/sys/devices/virtual/block/dm-0", true},
		{"/sys/devices/virtual/net/cni0", true},
		{"/sys/devices/virtual/nvme-subsystem/nvme-subsys0/nvme0n1", true},
		{"/sys/devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1", false},
		// "virtual" as part of a name is not the virtual subtree, and a
		// substring match would read both of these as virtual devices.
		{"/sys/devices/pci0000:00/0000:00:05.0/virtio2/block/vda", false},
		{"/sys/devices/pci0000:00/0000:00:05.0/virtualbox/block/vdb", false},
		{"", false},
	} {
		if got := IsVirtual(tc.path); got != tc.want {
			t.Errorf("IsVirtual(%q) is %v, want %v", tc.path, got, tc.want)
		}
	}
}

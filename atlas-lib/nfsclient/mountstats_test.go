// Parsing /proc/self/mountstats, against the file as a pNFS client prints it.

package nfsclient

import (
	"slices"
	"testing"
)

// sample is a CSI node's mountstats: one pNFS staging mount, the bind a pod
// sees it through, and the ordinary mounts around them. Trimmed to the rows the
// parser reads, with the per-mount layout kept as the kernel writes it.
const sample = `device rootfs mounted on / with fstype rootfs
device proc mounted on /proc with fstype proc
device 10.101.106.68:/default-shared-0 mounted on /var/lib/kubelet/plugins/kubernetes.io/csi/csi.simplyblock.io/9d88/globalmount/c:p:6e17 with fstype nfs4 statvers=1.1
	opts:	rw,vers=4.1,rsize=262144,wsize=262144,soft,proto=tcp,timeo=100,retrans=2
	age:	3379
	nfsv4:	bm0=0xfdffafff,bm1=0x40fdbe3e,bm2=0x803,acl=0x0,sessions,pnfs=LAYOUT_SCSI,lease_time=90,lease_expired=0
	sec:	flavor=1,pseudoflavor=1
	events:	39 11 0 1 0 2 25 524289 0 0 0 34 0 2 5 3 0 5 0 2 1 0 0 0 0 499616 173895
	bytes:	0 2147487744 2172592128 717307904 2172592128 2864795648 0 524289
	RPC iostats version: 1.1  p/v: 100003/4 (nfs)
	xprt:	tcp 0 0 6 0 29 169588 169584 0 4885578 0 31 7116 4400333
	per-op statistics
	        NULL: 1 1 0 44 24 0 0 0
	        READ: 30802 30802 0 5 6 0 1 2
	       WRITE: 9994 9994 0 7 8 0 1 2
	   LAYOUTGET: 18 18 0 464 336 0 0 3

device 10.101.106.68:/default-shared-0 mounted on /var/lib/kubelet/pods/1e98/volumes/kubernetes.io~csi/pvc-f117/mount with fstype nfs4 statvers=1.1
	opts:	rw,vers=4.1
	nfsv4:	bm0=0xfdffafff,sessions,pnfs=LAYOUT_SCSI,lease_time=90
	xprt:	tcp 0 0 6 0 29 169588 169584 0 4885578 0 31 7116 4400333
	per-op statistics
	   LAYOUTGET: 18 18 0 464 336 0 0 3

device 10.1.2.3:/plain mounted on /mnt/plain with fstype nfs4 statvers=1.1
	opts:	rw,vers=4.2
	nfsv4:	bm0=0xfdffafff,sessions,pnfs=not configured,lease_time=90
	per-op statistics
	        READ: 1 1 0 0 0 0 0 0
device tmpfs mounted on /run with fstype tmpfs
`

func TestParseMountstatsReadsEveryNFSMountAndOnlyThose(t *testing.T) {
	mounts := ParseMountstats(sample)
	got := make([]string, 0, len(mounts))
	for _, m := range mounts {
		got = append(got, m.MountPoint)
	}
	want := []string{
		"/var/lib/kubelet/plugins/kubernetes.io/csi/csi.simplyblock.io/9d88/globalmount/c:p:6e17",
		"/var/lib/kubelet/pods/1e98/volumes/kubernetes.io~csi/pvc-f117/mount",
		"/mnt/plain",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("mount points = %q, want %q", got, want)
	}
}

func TestParseMountstatsReadsAStagingMount(t *testing.T) {
	m := ParseMountstats(sample)[0]

	if m.Device != "10.101.106.68:/default-shared-0" || m.FSType != "nfs4" {
		t.Errorf("device %q fstype %q", m.Device, m.FSType)
	}
	if !slices.Equal(m.LayoutTypes, []string{"LAYOUT_SCSI"}) {
		t.Errorf("layout types = %q, want [LAYOUT_SCSI]", m.LayoutTypes)
	}
	if m.Connects != 6 {
		t.Errorf("connects = %d, want 6", m.Connects)
	}
	for op, want := range map[string]int64{"READ": 30802, "WRITE": 9994, "LAYOUTGET": 18} {
		if m.Ops[op] != want {
			t.Errorf("%s = %d, want %d", op, m.Ops[op], want)
		}
	}
}

// A mount without pNFS says so as text, which is not a layout type.
func TestParseMountstatsReadsNoLayoutTypeFromAPlainMount(t *testing.T) {
	m := ParseMountstats(sample)[2]
	if len(m.LayoutTypes) != 0 {
		t.Errorf("layout types = %q, want none", m.LayoutTypes)
	}
	if m.Connects != 0 {
		t.Errorf("connects = %d, want 0 without an xprt line", m.Connects)
	}
}

func TestMountUsesLayout(t *testing.T) {
	mounts := ParseMountstats(sample)
	if !mounts[0].UsesLayout("LAYOUT_SCSI") {
		t.Error("the staging mount does not report LAYOUT_SCSI")
	}
	if mounts[2].UsesLayout("LAYOUT_SCSI") {
		t.Error("the plain mount reports LAYOUT_SCSI")
	}
}
